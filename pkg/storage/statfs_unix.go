//go:build !windows

package storage

import "syscall"

// GetDiskFreeBytes 返回 path 所在文件系统对非特权用户的可用字节数。
func GetDiskFreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
