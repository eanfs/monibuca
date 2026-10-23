package storage

import (
	"errors"
	"os"
)

// UploadErrorClass 把上传错误分成 permanent / transient 两类，供结构化日志
// 与告警分级使用。只有「重试也不可能成功」的错误才标 permanent：
// 源文件确已不存在、权限不足、pending 目录已满（需人工扩容）。
// 网络超时、后端不可用、5xx 等一律 transient，由补传调度继续重试。
func UploadErrorClass(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, os.ErrNotExist), errors.Is(err, os.ErrPermission),
		errors.Is(err, ErrPendingDirFull):
		return "permanent"
	default:
		return "transient"
	}
}
