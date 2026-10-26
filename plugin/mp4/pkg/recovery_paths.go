package mp4

import (
	"errors"
	"fmt"
	"os"
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
	key := indexHeaderRecordPath(header)
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
	if rule := objectKeyInvalidReason(key); rule != "" {
		return recoveryPathRuleError(rule, header, mediaPath)
	}
	return nil
}

// indexHeaderRecordPath 返回索引头里的录像逻辑路径：优先 FilePath，为空时
// 回退 StorageKey（两端空白都会被去掉）。校验、上传对象键与 DB 回写必须用
// 同一个取值口径，因此集中在这里。
func indexHeaderRecordPath(header IndexHeader) string {
	if key := strings.TrimSpace(header.FilePath); key != "" {
		return key
	}
	return strings.TrimSpace(header.StorageKey)
}

// objectKeyInvalidReason 按对象键规则校验 key：必须非空、是相对路径（不接受
// 绝对路径与盘符）、不含 ".." 段、至少含一段有效名称。返回命中的规则标签，
// 合法时返回空字符串。
//
// ValidateIndexHeaderPaths 与 recoveryObjectKey 都调用这里，规则只写一份：
// 两条入口的差异只剩「何时调用」，不再是「各自实现一遍规则」。
func objectKeyInvalidReason(key string) string {
	switch {
	case filepath.IsAbs(key) || strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) ||
		filepath.VolumeName(key) != "":
		return recoveryRuleObjectKeyAbsolute
	case hasTraversalSegment(key):
		return recoveryRuleTraversal
	case !hasMeaningfulObjectKeySegment(key):
		return recoveryRuleObjectKeyInvalid
	}
	return ""
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
//
// 三态（任一非 none 即可放行**恢复**，但放行范围不同）：
//  1. SameFile：FilePath 就是 MediaPath 指向的同一个文件——本地录制的常态，
//     MediaPath 是 LocalFile.Name()，与传给 CreateFile 的路径完全一致。
//  2. MediaSuffix：MediaPath = <basePath> + FilePath。LocalStorage.CreateFile
//     用 filepath.Join(basePath, path)（local.go:189），所以配置绝对
//     record.filepath 时实际文件落在 basePath 之下，DB 记录的却是那个绝对路径。
//     这种「同一逻辑文件、不同目录」只能靠尾部关系识别。
//  3. WithinMediaDir：FilePath 位于 MediaPath 的目录子树内（恢复产物/同目录兄弟文件）。
//
// 安全性差异：SameFile 与 WithinMediaDir 证明录像路径确实落在受 guard 保护的
// 媒体路径上；MediaSuffix 不能——sidecar 不可信时 MediaPath 与 FilePath 都由
// 攻击者构造，后缀关系可以随手捏造（建 <恢复目录>/etc/shadow 当媒体，再写
// FilePath=/etc/shadow 即可命中）。因此 MediaSuffix 只放行恢复动作，DB 回写
// 必须额外收窄，见 isSuffixBoundAbsoluteFilePath 与 updateRecoveredRecord。
func recoveryAbsolutePathBinding(mediaPath, key string) recoveryPathBinding {
	if mediaPath == "" || !filepath.IsAbs(mediaPath) || !filepath.IsAbs(key) {
		return recoveryBindingNone
	}
	// 必须解析符号链接后再比较：词法上同在 <dir> 下的 <dir>/sub/x 若 sub 是
	// 指向白名单外的链接，写入 record_streams.file_path 后，下载（api.go:151）、
	// 取流（api_extract.go）与删除（exception.go）都会跟随链接操作任意文件。
	media := resolvePathForBinding(mediaPath)
	keyClean := resolvePathForBinding(key)
	if media == keyClean {
		return recoveryBindingSameFile
	}
	// MediaPath = basePath + FilePath：MediaPath 以分隔符 + FilePath 的相对形式结尾。
	if rel := strings.TrimPrefix(keyClean, string(filepath.Separator)); rel != "" &&
		strings.HasSuffix(media, string(filepath.Separator)+rel) {
		return recoveryBindingMediaSuffix
	}
	if WithinDir(filepath.Dir(media), keyClean) {
		return recoveryBindingWithinMediaDir
	}
	return recoveryBindingNone
}

// resolvePathForBinding 解析绝对路径中的符号链接，供绑定比较使用。
//
// 与 filepath.EvalSymlinks 的区别：目标可以尚不存在（恢复产物还没生成）。
// 此时逐级解析父目录再把叶子拼回，而不是放弃解析——否则攻击者只要让叶子
// 不存在即可绕过链接检查（「<dir>/sub -> /etc」+ FilePath=<dir>/sub/shadow）。
// 相对路径原样返回（绑定逻辑只处理绝对路径）。
func resolvePathForBinding(path string) string {
	clean := filepath.Clean(path)
	if clean == "" || !filepath.IsAbs(clean) {
		return clean
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved)
	}
	parent, base := filepath.Split(clean)
	if base == "" || base == "." {
		return clean
	}
	if parent = filepath.Clean(parent); parent == clean {
		return clean
	}
	return filepath.Join(resolvePathForBinding(parent), base)
}

// isSuffixBoundAbsoluteFilePath 报告绝对录像路径是否仅以「MediaPath 尾部」形态
// 通过绑定（MediaPath = <storage base> + FilePath），而非真的指向媒体文件。
//
// 该形态足以放行恢复（录制侧确实这样拼接），但不足以证明录像路径本身可信：
// sidecar 由不可信来源提供时，MediaPath 与 FilePath 都是攻击者可控的，
// 后缀关系可以随手构造。因此 DB 回写必须额外收窄（见 updateRecoveredRecord）。
//
// 该形态只出现在「local 存储 + 绝对 record.filepath + 非空 basePath」这种既有
// 坏配置上：录制侧写出 <basePath>/<filepath>，DB 存的却是那个绝对路径，而下载
// 侧对绝对 file_path 直接 http.ServeFile/os.Open（api.go:151-158），不拼
// basePath（GetFullPath 只用于相对路径），所以这类录像在客户端本来就打不开。
// 因此「只更新既有行、不新建」是正确取舍：既抢救了 duration/end_time 等元数据，
// 又不会把攻击者指定的绝对路径写成新的 file_path。
func isSuffixBoundAbsoluteFilePath(header IndexHeader) bool {
	key := indexHeaderRecordPath(header)
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

// PathExists 报告 path 是否是已存在的普通文件（目录不算）。
func PathExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// WithinDir 判断 target 是否位于 base 目录内（含自身）。恢复 API 的越界检查
// 与索引路径绑定共用这一份实现——安全敏感的判定不能有第二份拷贝。
func WithinDir(base, target string) bool {
	rel, err := filepath.Rel(filepath.Clean(base), filepath.Clean(target))
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
