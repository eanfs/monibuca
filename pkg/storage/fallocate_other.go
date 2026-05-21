//go:build !linux

package storage

import "os"

// InsertRange 在非 Linux 平台恒返回 ErrRangeInsertUnsupported，调用方回退全量重写。
func InsertRange(f *os.File, offset, length int64) error {
	return ErrRangeInsertUnsupported
}

// CollapseRange 在非 Linux 平台恒返回 ErrRangeInsertUnsupported。
func CollapseRange(f *os.File, offset, length int64) error {
	return ErrRangeInsertUnsupported
}
