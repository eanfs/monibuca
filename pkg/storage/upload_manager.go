package storage

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
)

var (
	uploadSem     chan struct{}
	activeUploads int32
	pendingDir    string
	maxConcurrent int

	// trailerSem 限制并发 trailer 写盘槽位数, 避免多路 record stop 时同时
	// moov-rewrite + bufio flush 把磁盘吃满. 与 uploadSem 解耦:
	//   - trailerSem 控磁盘 IO (mp4/flv 等录制 plugin 共用)
	//   - uploadSem 控网络上传 (s3/cos/oss 后端)
	trailerSem            chan struct{}
	activeTrailerWrites   int32
	maxConcurrentTrailers int

	// OnUploadFailed 上传失败回调，由上层（server）注册。
	// 参数: localPath=本地文件路径, objectKey=远端对象键, storageType=存储类型,
	//       fileSize=文件大小, metadata=用户元数据, err=错误信息
	OnUploadFailed func(localPath, objectKey, storageType string, fileSize int64, metadata map[string]string, err error)
)

// UploadConfig 上传管理配置
type UploadConfig struct {
	MaxConcurrentUploads       int    `desc:"最大并发上传数" default:"4"`
	MaxConcurrentTrailerWrites int    `desc:"最大并发 trailer 写盘槽位数, 控制 record stop 时磁盘 IO burst (目标 ~300 MB/s SSD)" default:"8"`
	PendingDir                 string `desc:"上传失败文件暂存目录" default:"pending_uploads"`
}

// InitUploadManager 初始化上传管理器（并发控制 + 暂存目录）
func InitUploadManager(cfg UploadConfig) {
	if cfg.MaxConcurrentUploads <= 0 {
		cfg.MaxConcurrentUploads = 4
	}
	maxConcurrent = cfg.MaxConcurrentUploads
	uploadSem = make(chan struct{}, cfg.MaxConcurrentUploads)

	if cfg.MaxConcurrentTrailerWrites <= 0 {
		cfg.MaxConcurrentTrailerWrites = 8
	}
	maxConcurrentTrailers = cfg.MaxConcurrentTrailerWrites
	trailerSem = make(chan struct{}, cfg.MaxConcurrentTrailerWrites)

	if cfg.PendingDir == "" {
		cfg.PendingDir = "pending_uploads"
	}
	pendingDir = cfg.PendingDir
	if err := os.MkdirAll(pendingDir, 0755); err != nil {
		log.Printf("[storage] failed to create pending dir %s: %v", pendingDir, err)
	}
	log.Printf("[storage] upload manager initialized: maxConcurrent=%d, maxTrailer=%d, pendingDir=%s",
		maxConcurrent, maxConcurrentTrailers, pendingDir)
}

// AcquireUploadSlot 获取一个上传槽位，阻塞直到有可用槽位或 ctx 取消
func AcquireUploadSlot(ctx context.Context) error {
	if uploadSem == nil {
		return nil
	}
	select {
	case uploadSem <- struct{}{}:
		atomic.AddInt32(&activeUploads, 1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseUploadSlot 释放一个上传槽位
func ReleaseUploadSlot() {
	if uploadSem == nil {
		return
	}
	<-uploadSem
	atomic.AddInt32(&activeUploads, -1)
}

// GetActiveUploads 获取当前活跃上传数
func GetActiveUploads() int32 {
	return atomic.LoadInt32(&activeUploads)
}

// GetMaxConcurrentUploads 获取最大并发上传数
func GetMaxConcurrentUploads() int {
	return maxConcurrent
}

// MoveToPendingDir 将文件移动到暂存目录，返回新路径。
// 优先使用 rename（同文件系统快速），失败时 fallback 到 copy+delete。
func MoveToPendingDir(srcPath string) (string, error) {
	if pendingDir == "" {
		return srcPath, nil // 未配置暂存目录，保留原路径
	}
	if err := os.MkdirAll(pendingDir, 0755); err != nil {
		return "", err
	}
	dstPath := filepath.Join(pendingDir, filepath.Base(srcPath))

	// 避免同名冲突
	if _, err := os.Stat(dstPath); err == nil {
		dstPath = filepath.Join(pendingDir, filepath.Base(srcPath)+"."+filepath.Base(os.TempDir()))
	}

	// 尝试 rename
	if err := os.Rename(srcPath, dstPath); err == nil {
		return dstPath, nil
	}

	// fallback: copy + delete
	src, err := os.Open(srcPath)
	if err != nil {
		return "", err
	}
	defer src.Close()

	dst, err := os.Create(dstPath)
	if err != nil {
		return "", err
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		os.Remove(dstPath)
		return "", err
	}
	src.Close()
	os.Remove(srcPath)
	return dstPath, nil
}

// GetPendingDir 获取暂存目录路径
func GetPendingDir() string {
	return pendingDir
}

// AcquireTrailerSlot 获取一个 trailer 写盘槽位, 阻塞直到有可用槽位或 ctx 取消.
// 配对 ReleaseTrailerSlot. 调用方通常在 record stop 流程进入 trailer flush 前 acquire,
// 在 task Dispose / Run 末尾 defer Release.
// trailerSem 未初始化时为 no-op, 立即返回 nil (保证测试 / 早期初始化场景安全).
func AcquireTrailerSlot(ctx context.Context) error {
	if trailerSem == nil {
		return nil
	}
	select {
	case trailerSem <- struct{}{}:
		atomic.AddInt32(&activeTrailerWrites, 1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseTrailerSlot 释放一个 trailer 写盘槽位.
// 未初始化时 no-op, 保证 defer 安全.
func ReleaseTrailerSlot() {
	if trailerSem == nil {
		return
	}
	<-trailerSem
	atomic.AddInt32(&activeTrailerWrites, -1)
}

// GetActiveTrailerWrites 当前活跃 trailer 写盘数
func GetActiveTrailerWrites() int32 {
	return atomic.LoadInt32(&activeTrailerWrites)
}

// GetMaxConcurrentTrailerWrites 当前 trailer slot 上限
func GetMaxConcurrentTrailerWrites() int {
	return maxConcurrentTrailers
}
