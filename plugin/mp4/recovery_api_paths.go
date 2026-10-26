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

// errRecoveryPathDenied 表示路径解析后落在恢复白名单之外（含符号链接逃逸）。
// 调用方据此返回 403，且绝不触碰目标文件。
var errRecoveryPathDenied = errors.New("recovery path outside recovery allow-list")

// validateRunTarget 校验请求目标是否在白名单内。
// 返回非零 HTTP status 表示拒绝（403 = 越界/符号链接逃逸，400 = 不存在/类型错误）。
// file 为媒体文件时会解析出对应 sidecar；找不到 sidecar 返回 400。
//
// 安全性：请求路径只是入口，恢复过程真正读写的路径来自索引 header 的
// MediaPath。因此在放行前还要读取索引头，校验 MediaPath 与产物路径同样落在
// 白名单（或其所在目录）内，避免“合法 sidecar 指向白名单外媒体”的覆盖/删除。
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
		info, err := os.Lstat(abs)
		if err != nil || info.IsDir() {
			return nil, "", http.StatusBadRequest, "file not found"
		}
		// 符号链接一律拒绝：EvalSymlinks 只解析路径，无法区分“用户直接给了
		// 一个指向白名单外的链接”与链接目标本身。
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, "", http.StatusForbidden, "file outside recovery allow-list: symlink not allowed"
		}
		canon, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, "", http.StatusBadRequest, "resolve file: " + err.Error()
		}
		if !pathWithinAny(allowed, canon) {
			return nil, "", http.StatusForbidden, "file outside recovery allow-list"
		}
		resolved, status, message := resolveRecoverySidecar(canon, allowed)
		if status != 0 {
			return nil, "", status, message
		}
		if status, message := validateRecoveryIndexTargets(allowed, resolved); status != 0 {
			return nil, "", status, message
		}
		return nil, resolved, 0, ""
	}

	// 未指定目标：使用配置/默认目录。
	return nil, "", 0, ""
}

// validateRecoveryIndexTargets 读取索引头并校验其记录的媒体路径/产物路径。
// 索引不可读时不在这里拒绝（保持既有语义：由运行结果报告失败），
// 但真正执行时仍会再做一次路径校验。
func validateRecoveryIndexTargets(allowed []string, sidecar string) (status int, message string) {
	header, err := mp4pkg.ReadIndexHeader(sidecar)
	if err != nil {
		return 0, ""
	}
	recorded := header.MediaPath
	if recorded == "" {
		recorded = mediaPathFromSidecar(sidecar)
	}
	if !recoveryMediaAllowed(allowed, sidecar, recorded) {
		return http.StatusForbidden, "recovery target outside recovery allow-list"
	}
	return 0, ""
}

// recoveryMediaAllowed 判断索引记录的媒体/产物路径是否可接受。
// 白名单为恢复目录 + sidecar 所在目录（sidecar 本身已通过白名单校验）。
func recoveryMediaAllowed(allowed []string, sidecar, recorded string) bool {
	dirs := append(append([]string(nil), allowed...), filepath.Dir(sidecar))
	canonMedia, ok := canonicalRecoveryTarget(recorded)
	if !ok || !pathWithinAny(dirs, canonMedia) {
		return false
	}
	output := recoveryOutputPath(recorded)
	canonOutput, ok := canonicalRecoveryTarget(output)
	if !ok || !pathWithinAny(dirs, canonOutput) {
		return false
	}
	return true
}

// absoluteRecoveryTarget 把历史 sidecar 里可能记录的相对路径转成绝对路径。
//
// 旧版本把 storage.local 的相对 LocalFile.Name() 原样写进 header.MediaPath；
// 白名单校验要求绝对路径，若不先转换，这类 sidecar 会永远被判越界。相对
// 路径按进程当前工作目录解析——这也正是录制时 LocalFile.Name() 的语义。
// 新写入的 sidecar 已是绝对路径，此函数对它们无副作用。
func absoluteRecoveryTarget(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

// canonicalRecoveryTarget 规范化一个可能尚不存在的路径：已存在时拒绝符号链接
// 并解析全部链接；不存在时解析父目录（父目录必须存在）。
func canonicalRecoveryTarget(path string) (string, bool) {
	path = absoluteRecoveryTarget(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return "", false
		}
		canon, err := filepath.EvalSymlinks(path)
		if err != nil {
			return "", false
		}
		return canon, true
	}
	canonDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", false
	}
	return filepath.Join(canonDir, filepath.Base(path)), true
}

// recoveryPathGuard 校验恢复过程中实际读写的路径（sidecar/媒体/产物）。
// 白名单 = 恢复目录 + sidecar 所在目录；符号链接一律拒绝。
type recoveryPathGuard struct {
	allowed []string
}

func newRecoveryPathGuard(dirs []string, sidecar string) (*recoveryPathGuard, error) {
	allowed := canonicalExistingDirs(dirs)
	info, err := os.Lstat(sidecar)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: sidecar %s is a symlink", errRecoveryPathDenied, sidecar)
	}
	canonSidecar, err := filepath.EvalSymlinks(sidecar)
	if err != nil {
		return nil, err
	}
	allowed = append(allowed, filepath.Dir(canonSidecar))
	return &recoveryPathGuard{allowed: allowed}, nil
}

// allowExisting 校验一个已存在路径。相对路径先按 CWD 绝对化（兼容旧 sidecar）；
// 返回**解析符号链接后的真实路径**（canon）：调用方用它去 rename/覆盖/删除，
// 返回原路径会把「检查的路径」与「实际操作的路径」重新分开，留下符号链接
// TOCTOU 窗口。canon 也让 C1 的 source==output 比较在同一坐标系下成立。
func (g *recoveryPathGuard) allowExisting(path, what string) (string, error) {
	path = absoluteRecoveryTarget(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %s must be an absolute path", errRecoveryPathDenied, what)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s %s is a symlink", errRecoveryPathDenied, what, path)
	}
	canon, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !pathWithinAny(g.allowed, canon) {
		return "", fmt.Errorf("%w: %s %s", errRecoveryPathDenied, what, path)
	}
	return canon, nil
}

// allowOutput 校验一个待写入路径：父目录必须存在且在白名单内，
// 目标已存在时不得是符号链接。
//
// 返回父目录解析后的 canon + 原文件名：写入方拿到的路径不含任何待解析的
// 符号链接，避免「校验用 A、写入用 B」。
func (g *recoveryPathGuard) allowOutput(path, what string) (string, error) {
	path = absoluteRecoveryTarget(path)
	if path == "" {
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %s must be an absolute path", errRecoveryPathDenied, what)
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s %s is a symlink", errRecoveryPathDenied, what, path)
	}
	canonDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if !pathWithinAny(g.allowed, canonDir) {
		return "", fmt.Errorf("%w: %s %s", errRecoveryPathDenied, what, path)
	}
	canon := filepath.Join(canonDir, filepath.Base(path))
	if info, err := os.Lstat(canon); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s %s is a symlink", errRecoveryPathDenied, what, path)
	}
	return canon, nil
}

func (t *IndexRecoveryTask) newRecoveryPathGuard(sidecar string) (*recoveryPathGuard, error) {
	dirs, _ := t.resolveDirs(nil)
	return newRecoveryPathGuard(dirs, sidecar)
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
		if mp4pkg.WithinDir(base, target) {
			return true
		}
	}
	return false
}

// resolveRecoverySidecar 把用户给出的文件解析为可处理的 sidecar。
// 接受 sidecar 本身（.m7sidx/.m7sidx.recovering），或媒体文件（自动补后缀）。
//
// 返回非零 status 表示拒绝：403 = 候选是符号链接或解析后越界，
// 400 = 没有待恢复索引（.failed 不重试）。
// 候选路径必须重新规范化：仅做存在性检查无法发现“白名单内的链接指向外部”。
func resolveRecoverySidecar(path string, allowed []string) (sidecar string, status int, message string) {
	canonical := func(candidate string) (string, int, string) {
		canon, ok := canonicalRecoveryFile(candidate, allowed)
		if !ok {
			return "", http.StatusForbidden, "file outside recovery allow-list: symlink not allowed"
		}
		return canon, 0, ""
	}
	switch {
	case strings.HasSuffix(path, mp4pkg.IndexExtension+recoveringSuffix),
		strings.HasSuffix(path, mp4pkg.IndexExtension):
		return canonical(path)
	case strings.HasSuffix(path, mp4pkg.IndexExtension+failedSuffix):
		return "", http.StatusBadRequest, "no pending recovery index for file (failed indexes are not retried)"
	}
	for _, suffix := range []string{mp4pkg.IndexExtension + recoveringSuffix, mp4pkg.IndexExtension} {
		candidate := path + suffix
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		return canonical(candidate)
	}
	return "", http.StatusBadRequest, "no pending recovery index for file (failed indexes are not retried)"
}

// canonicalRecoveryFile 校验并规范化一个已存在的文件路径：
// 符号链接一律拒绝（Lstat），解析祖先符号链接后必须仍在白名单内。
func canonicalRecoveryFile(path string, allowed []string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", false
	}
	canon, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	if !pathWithinAny(allowed, canon) {
		return "", false
	}
	return canon, true
}

// ---------------------------------------------------------------- 目录扫描

// scanRecoveryEntries 有界扫描恢复目录，收集 sidecar 状态。
//
// limit 是**遍历条目总数**预算（含目录与普通文件），不是 sidecar 数量：
// 上限的目的是限制遍历代价。只在命中 sidecar 时计数的话，一个塞满普通文件的
// 目录可以无视上限、每次都全树递归。
//
// 预算耗尽后停止（truncated=true），绝不把无界目录读入内存。
func scanRecoveryEntries(dirs []string, limit int) (entries []recoveryListEntry, truncated bool, errs []error) {
	seen := make(map[string]bool)
	visited := 0
	for _, dir := range dirs {
		if truncated {
			break
		}
		walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
				return nil
			}
			visited++
			if visited > limit {
				truncated = true
				return fs.SkipAll
			}
			if d.IsDir() {
				return nil
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
