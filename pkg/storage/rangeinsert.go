package storage

import (
	"errors"
	"os"
)

// ErrRangeInsertUnsupported 表示当前平台或文件系统不支持 fallocate INSERT_RANGE。
// 调用方收到此错误应回退到全量重写路径。
var ErrRangeInsertUnsupported = errors.New("storage: range insert unsupported")

// RangeInserter 是 storage.File 的可选能力：暴露承载数据的底层本地文件句柄，
// 供 MP4 trailer 用 fallocate(INSERT_RANGE) 在文件头就地插入 moov，避免全量重写。
// 仅本地文件 / 对象存储后端的本地暂存文件可实现。
type RangeInserter interface {
	// LocalFd 返回底层本地文件句柄。句柄所有权仍属 storage.File，
	// 调用方可读写 / fallocate / ftruncate，但不得 Close 它。
	LocalFd() *os.File
}
