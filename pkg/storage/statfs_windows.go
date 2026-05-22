//go:build windows

package storage

import "errors"

// GetDiskFreeBytes 在 Windows 暂不支持磁盘剩余空间检查,返回错误使调用方降级
// (checkPendingCapacity 在 err != nil 时跳过磁盘检查,仅按文件数 / 总大小限制)。
func GetDiskFreeBytes(_ string) (uint64, error) {
	return 0, errors.New("GetDiskFreeBytes not supported on windows")
}
