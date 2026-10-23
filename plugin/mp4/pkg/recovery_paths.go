package mp4

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"m7s.live/v5/pkg/storage"
)

// ErrIndexHeaderPathMismatch 表示索引头记录的录像路径与其绑定依据不一致：
// 本地存储时绝对路径不在 MediaPath 的目录子树内，或（任意存储）路径含穿越段，
// 或对象存储时录像路径是绝对路径。
//
// sidecar 内容一律视为不可信输入。FilePath（或 FilePath 为空时的 StorageKey）
// 既会被写进 record_streams.file_path（下载接口随后可能直接 os.Open 它），
// 也会被当作上传对象键，因此必须按存储类型校验。
var ErrIndexHeaderPathMismatch = errors.New("mp4: index header path rejected")

// 规则标签：写进错误信息，现场日志据此定位命中的是哪一条。
const (
	// recoveryRuleLocalDirectory：本地存储的绝对 FilePath 未绑定到 MediaPath 目录。
	recoveryRuleLocalDirectory = "local-directory"
	// recoveryRuleObjectKeyAbsolute：对象存储的录像路径是绝对路径（对象键必须相对）。
	recoveryRuleObjectKeyAbsolute = "object-key-absolute"
	// recoveryRuleTraversal：任何存储类型下路径含 ".." 段。
	recoveryRuleTraversal = "traversal"
	// recoveryRuleObjectKeyInvalid：对象键没有任何有效路径段（如 "."）。
	recoveryRuleObjectKeyInvalid = "object-key-invalid"
)

// ValidateIndexHeaderPaths 按存储类型校验索引头里录像路径的可信性。
//
// 被校验的值与 recoveryObjectKey / updateRecoveredRecord 使用的值一致：
// 优先 FilePath，为空时取 StorageKey。
//
// 规则：
//   - 两者都为空：不参与校验（调用方原有的“无路径”分支负责处理）；
//   - 任意类型：含 ".." 段一律拒绝（"/" 与 "\" 都视为分隔符）；
//   - 本地存储（storageType=local）：绝对路径必须与 MediaPath 同文件或位于
//     其目录子树内；相对路径按 CWD 解析（旧行为），不做目录绑定；
//   - 对象存储（s3/cos/oss/未声明）：录像路径就是对象键，必须是相对路径，
//     绝对路径一律拒绝（对象键不是文件系统路径，同目录绑定对它无意义）。
//
// MediaPath 必须是绝对路径的约束由恢复侧的路径 guard 负责（这里只做绑定）。
func ValidateIndexHeaderPaths(header IndexHeader) error {
	key := strings.TrimSpace(header.FilePath)
	if key == "" {
		key = strings.TrimSpace(header.StorageKey)
	}
	if key == "" {
		return nil
	}
	mediaPath := strings.TrimSpace(header.MediaPath)

	if hasTraversalSegment(key) {
		return recoveryPathRuleError(recoveryRuleTraversal, header, mediaPath)
	}

	if isLocalRecoveryStorage(header.StorageType) {
		if !filepath.IsAbs(key) {
			// 本地存储的相对路径没有可绑定对象：它按进程 CWD 解析，
			// 由下载侧的 storage 解析（GetFullPath）兜住。
			return nil
		}
		if recoveryAbsolutePathBinding(mediaPath, key) != recoveryBindingNone {
			return nil
		}
		return recoveryPathRuleError(recoveryRuleLocalDirectory, header, mediaPath)
	}

	// 对象存储：必须是相对路径且含有效段。
	if filepath.IsAbs(key) || strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) ||
		filepath.VolumeName(key) != "" {
		return recoveryPathRuleError(recoveryRuleObjectKeyAbsolute, header, mediaPath)
	}
	if !hasMeaningfulObjectKeySegment(key) {
		return recoveryPathRuleError(recoveryRuleObjectKeyInvalid, header, mediaPath)
	}
	return nil
}

// recoveryPathRuleError 组装带规则标签的错误，便于现场日志直接定位。
func recoveryPathRuleError(rule string, header IndexHeader, mediaPath string) error {
	return fmt.Errorf("%w: rule=%s filePath=%q storageKey=%q mediaPath=%q storageType=%q",
		ErrIndexHeaderPathMismatch, rule, header.FilePath, header.StorageKey, mediaPath, header.StorageType)
}

// recoveryPathBinding 描述录像路径与 MediaPath 的绑定形态。
type recoveryPathBinding int

const (
	// recoveryBindingNone 表示未建立绑定。
	recoveryBindingNone recoveryPathBinding = iota
	// recoveryBindingSameFile 表示录像路径就是 MediaPath 指向的同一个文件。
	recoveryBindingSameFile
	// recoveryBindingWithinMediaDir 表示录像路径位于 MediaPath 的目录子树内。
	recoveryBindingWithinMediaDir
	// recoveryBindingMediaSuffix 表示 MediaPath = <storage base> + 绝对录像路径：
	// 即 LocalStorage.CreateFile 的 filepath.Join(basePath, path) 形态
	// （local.go:189）。此时二者是同一逻辑文件却不同目录。
	recoveryBindingMediaSuffix
)

// recoveryAbsolutePathBinding 判断绝对录像路径与 MediaPath 的绑定形态。
func recoveryAbsolutePathBinding(mediaPath, key string) recoveryPathBinding {
	if mediaPath == "" || !filepath.IsAbs(mediaPath) || !filepath.IsAbs(key) {
		return recoveryBindingNone
	}
	media := filepath.Clean(mediaPath)
	keyClean := filepath.Clean(key)
	if media == keyClean {
		return recoveryBindingSameFile
	}
	// MediaPath = basePath + FilePath：MediaPath 以分隔符 + FilePath 的相对形式结尾。
	if rel := strings.TrimPrefix(keyClean, string(filepath.Separator)); rel != "" &&
		strings.HasSuffix(media, string(filepath.Separator)+rel) {
		return recoveryBindingMediaSuffix
	}
	if withinRecoveryDir(filepath.Dir(media), keyClean) {
		return recoveryBindingWithinMediaDir
	}
	return recoveryBindingNone
}

// isSuffixBoundAbsoluteFilePath 报告绝对录像路径是否仅以「MediaPath 尾部」形态
// 通过绑定（MediaPath = <storage base> + FilePath），而非真的指向媒体文件。
//
// 该形态足以放行恢复（录制侧确实这样拼接），但不足以证明录像路径本身可信：
// sidecar 由不可信来源提供时，MediaPath 与 FilePath 都是攻击者可控的，
// 后缀关系可以随手构造。因此 DB 回写必须额外收紧（见 updateRecoveredRecord）。
func isSuffixBoundAbsoluteFilePath(header IndexHeader) bool {
	key := strings.TrimSpace(header.FilePath)
	if key == "" {
		key = strings.TrimSpace(header.StorageKey)
	}
	if key == "" || !filepath.IsAbs(key) {
		return false
	}
	return recoveryAbsolutePathBinding(strings.TrimSpace(header.MediaPath), key) == recoveryBindingMediaSuffix
}

func isLocalRecoveryStorage(storageType string) bool {
	return strings.EqualFold(strings.TrimSpace(storageType), string(storage.StorageTypeLocal))
}

// hasTraversalSegment 判断路径是否含 ".." 段（同时处理 / 与 \ 分隔符）。
func hasTraversalSegment(key string) bool {
	for _, segment := range strings.FieldsFunc(key, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == ".." {
			return true
		}
	}
	return false
}

// hasMeaningfulObjectKeySegment 判断对象键是否至少含一段有效名称。
func hasMeaningfulObjectKeySegment(key string) bool {
	for _, segment := range strings.FieldsFunc(key, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment != "." {
			return true
		}
	}
	return false
}

// withinRecoveryDir 判断 target 是否位于 base 目录内（含自身）。
func withinRecoveryDir(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
