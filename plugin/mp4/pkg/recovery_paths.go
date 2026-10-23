package mp4

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrIndexHeaderPathMismatch 表示索引头记录的录像路径与 mediaPath 没有绑定关系：
// 绝对路径落在 mediaPath 所在目录之外，或相对路径含穿越段。
//
// sidecar 内容一律视为不可信输入。FilePath（或 FilePath 为空时的 StorageKey）
// 既会被写进 record_streams.file_path（下载接口随后可能直接 os.Open 它），
// 也会被当作上传对象键，因此必须与 mediaPath 建立绑定，避免「白名单内的
// sidecar 指向白名单外的文件/对象键」。
var ErrIndexHeaderPathMismatch = errors.New("mp4: index header filePath is not bound to mediaPath")

// ValidateIndexHeaderPaths 校验索引头里录像路径与 MediaPath 的绑定关系。
//
// 被校验的值与 recoveryObjectKey / updateRecoveredRecord 使用的值保持一致：
// 优先 FilePath，为空时取 StorageKey（旧 header 与本地存储的形态）。
//
// 规则：
//   - 两者都为空：不参与校验（调用方原有的“无路径”分支负责处理）；
//   - 绝对路径：本地录制的正常形态，必须与 MediaPath 同目录或位于其目录
//     子树内，否则拒绝；
//   - 相对路径：对象存储的逻辑录像路径/对象键。它与本地暂存 MediaPath
//     （如 /tmp/s3writer_xxx.tmp）没有目录关系，因此按对象键规则校验——
//     拒绝绝对路径、任何 ".." 段，且必须至少含一段有效名称。
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

	if filepath.IsAbs(key) {
		if mediaPath != "" && filepath.IsAbs(mediaPath) &&
			withinRecoveryDir(filepath.Dir(mediaPath), key) {
			return nil
		}
		return fmt.Errorf("%w: filePath=%q storageKey=%q mediaPath=%q",
			ErrIndexHeaderPathMismatch, header.FilePath, header.StorageKey, mediaPath)
	}

	if !isSafeRecoveryObjectKey(key) {
		return fmt.Errorf("%w: filePath=%q storageKey=%q mediaPath=%q",
			ErrIndexHeaderPathMismatch, header.FilePath, header.StorageKey, mediaPath)
	}
	return nil
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
