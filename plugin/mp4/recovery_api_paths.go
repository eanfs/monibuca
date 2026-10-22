package plugin_mp4

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// 恢复 API 的路径安全与目录扫描：请求解析、白名单校验、有界遍历。

func parseRecoveryPagination(values url.Values) (page, size int, err error) {
	page, size = 1, recoveryListDefaultPageSize
	if raw := values.Get("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			return 0, 0, errors.New("invalid query param: page")
		}
	}
	if raw := values.Get("size"); raw != "" {
		size, err = strconv.Atoi(raw)
		if err != nil || size < 1 {
			return 0, 0, errors.New("invalid query param: size")
		}
		if size > recoveryListMaxPageSize {
			size = recoveryListMaxPageSize
		}
	}
	return page, size, nil
}

func parseRecoveryRunRequest(r *http.Request) (recoveryRunRequest, error) {
	var req recoveryRunRequest
	if r.Body != nil {
		decoder := json.NewDecoder(io.LimitReader(r.Body, recoveryRunRequestBodyLimit))
		if err := decoder.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			return req, errors.New("invalid JSON body")
		}
	}
	query := r.URL.Query()
	if req.Dir == "" {
		req.Dir = query.Get("dir")
	}
	if req.File == "" {
		req.File = query.Get("file")
	}
	req.Dir = strings.TrimSpace(req.Dir)
	req.File = strings.TrimSpace(req.File)
	if req.Dir != "" && req.File != "" {
		return req, errors.New("dir and file are mutually exclusive")
	}
	return req, nil
}

// validateRunTarget 校验请求目标是否在白名单内。
// 返回非零 HTTP status 表示拒绝（403 = 越界/符号链接逃逸，400 = 不存在/类型错误）。
// file 为媒体文件时会解析出对应 sidecar；找不到 sidecar 返回 400。
func (t *IndexRecoveryTask) validateRunTarget(dir, file string) (dirs []string, sidecar string, status int, message string) {
	allowed, _ := t.resolveDirs(nil)
	allowed = canonicalExistingDirs(allowed)

	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, "", http.StatusBadRequest, "invalid directory"
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, "", http.StatusBadRequest, "directory not found"
		}
		if !info.IsDir() {
			return nil, "", http.StatusBadRequest, "not a directory"
		}
		canon, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, "", http.StatusBadRequest, "resolve directory: " + err.Error()
		}
		if !pathWithinAny(allowed, canon) {
			return nil, "", http.StatusForbidden, "directory outside recovery allow-list"
		}
		return []string{canon}, "", 0, ""
	}

	if file != "" {
		abs, err := filepath.Abs(file)
		if err != nil {
			return nil, "", http.StatusBadRequest, "invalid file"
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			return nil, "", http.StatusBadRequest, "file not found"
		}
		canon, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, "", http.StatusBadRequest, "resolve file: " + err.Error()
		}
		if !pathWithinAny(allowed, canon) {
			return nil, "", http.StatusForbidden, "file outside recovery allow-list"
		}
		resolved, ok := resolveRecoverySidecar(canon)
		if !ok {
			return nil, "", http.StatusBadRequest, "no pending recovery index for file (failed indexes are not retried)"
		}
		return nil, resolved, 0, ""
	}

	// 未指定目标：使用配置/默认目录。
	return nil, "", 0, ""
}

// canonicalExistingDirs 对白名单目录做绝对路径 + 符号链接解析。
func canonicalExistingDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		canon, err := filepath.EvalSymlinks(abs)
		if err != nil {
			continue
		}
		out = append(out, canon)
	}
	return out
}

// pathWithinAny 判断 target 是否位于任一 allowed 目录内（含自身）。
func pathWithinAny(allowed []string, target string) bool {
	for _, base := range allowed {
		if pathWithin(base, target) {
			return true
		}
	}
	return false
}

func pathWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveRecoverySidecar 把用户给出的文件解析为可处理的 sidecar。
// 接受 sidecar 本身（.m7sidx/.m7sidx.recovering），或媒体文件（自动补后缀）。
func resolveRecoverySidecar(path string) (string, bool) {
	switch {
	case strings.HasSuffix(path, mp4pkg.IndexExtension+recoveringSuffix):
		return path, true
	case strings.HasSuffix(path, mp4pkg.IndexExtension):
		return path, true
	case strings.HasSuffix(path, mp4pkg.IndexExtension+failedSuffix):
		return "", false
	}
	for _, suffix := range []string{mp4pkg.IndexExtension + recoveringSuffix, mp4pkg.IndexExtension} {
		if candidate := path + suffix; fileExists(candidate) {
			return candidate, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------- 目录扫描

// scanRecoveryEntries 有界扫描恢复目录，收集 sidecar 状态。
// 达到 limit 后停止（truncated=true），绝不把无界目录读入内存。
func scanRecoveryEntries(dirs []string, limit int) (entries []recoveryListEntry, truncated bool, errs []error) {
	seen := make(map[string]bool)
	for _, dir := range dirs {
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if len(entries) >= limit {
				truncated = true
				return fs.SkipAll
			}
			category, ok := recoveryCategoryForName(d.Name())
			if !ok || seen[path] {
				return nil
			}
			seen[path] = true
			entry := recoveryListEntry{
				Category:  category,
				Path:      path,
				MediaPath: mediaPathFromSidecar(path),
			}
			entry.OutputPath = recoveryOutputPath(entry.MediaPath)
			if info, infoErr := d.Info(); infoErr == nil {
				entry.Size = info.Size()
				entry.ModTime = info.ModTime()
			}
			entries = append(entries, entry)
			return nil
		})
		if walkErr != nil {
			errs = append(errs, fmt.Errorf("walk recovery dir %s: %w", dir, walkErr))
		}
	}

	// 同一媒体同时存在 .m7sidx 与 .m7sidx.recovering 时只保留 recovering
	// （与 discoverSidecars 的优先级一致）。
	recovering := make(map[string]bool)
	for _, entry := range entries {
		if entry.Category == "recovering" {
			recovering[strings.TrimSuffix(entry.Path, recoveringSuffix)] = true
		}
	}
	filtered := entries[:0]
	for _, entry := range entries {
		if entry.Category == "pending" && recovering[entry.Path] {
			continue
		}
		filtered = append(filtered, entry)
	}
	entries = filtered

	categoryOrder := func(category string) int {
		switch category {
		case "pending":
			return 0
		case "recovering":
			return 1
		default:
			return 2
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		left, right := entries[i], entries[j]
		if left.Category != right.Category {
			return categoryOrder(left.Category) < categoryOrder(right.Category)
		}
		return left.Path < right.Path
	})
	return entries, truncated, errs
}

func recoveryCategoryForName(name string) (string, bool) {
	switch {
	case strings.HasSuffix(name, mp4pkg.IndexExtension+recoveringSuffix):
		return "recovering", true
	case strings.HasSuffix(name, mp4pkg.IndexExtension+failedSuffix):
		return "failed", true
	case strings.HasSuffix(name, mp4pkg.IndexExtension):
		return "pending", true
	default:
		// .m7sidx.tmp 重写中间态等一律忽略。
		return "", false
	}
}

func pageSlice[T any](items []T, start, size int) []T {
	if start >= len(items) || start < 0 || size <= 0 {
		return nil
	}
	end := start + size
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}
