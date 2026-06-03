//go:build linux

package storage

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// InsertRange 在 f 的 offset 处插入 length 字节空间（offset 与 length 均须为
// 文件系统块大小，通常 4096，的整数倍）。offset 之后的数据逻辑后移、物理 extent
// 不动——这是一个 O(extent 数) 的元数据操作，不重写数据本身。
//
// 文件系统不支持时（tmpfs / NFS / 非 extent ext4 / 旧 kernel）返回
// ErrRangeInsertUnsupported，调用方应回退到全量重写路径。
func InsertRange(f *os.File, offset, length int64) error {
	err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_INSERT_RANGE, offset, length)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return ErrRangeInsertUnsupported
	}
	return err
}

// CollapseRange 删除 f 的 [offset, offset+length) 区间（offset 与 length 均须块对齐），
// 其后数据前移。是 InsertRange 的逆操作，用于 INSERT 成功但后续写入失败时把文件
// 回退到插入前的状态。
func CollapseRange(f *os.File, offset, length int64) error {
	err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_COLLAPSE_RANGE, offset, length)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		return ErrRangeInsertUnsupported
	}
	return err
}
