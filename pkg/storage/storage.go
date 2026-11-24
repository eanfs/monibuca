package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

var logger *slog.Logger

// SetLogger 设置 storage 包使用的 logger
func SetLogger(l *slog.Logger) {
	logger = l
}

// StorageType 存储类型
type StorageType string

const (
	StorageTypeLocal StorageType = "local"
	StorageTypeS3    StorageType = "s3"
	StorageTypeOSS   StorageType = "oss"
	StorageTypeCOS   StorageType = "cos"
)

// StorageConfig 存储配置接口
type StorageConfig interface {
	GetType() StorageType
	Validate() error
}

// Storage 存储接口
type Storage interface {
	CreateFile(ctx context.Context, path string) (File, error)
	// OpenFile 以只读模式打开文件（不会上传修改）
	OpenFile(ctx context.Context, path string) (File, error)
	// Delete 删除文件
	Delete(ctx context.Context, path string) error

	// Exists 检查文件是否存在
	Exists(ctx context.Context, path string) (bool, error)

	// GetSize 获取文件大小
	GetSize(ctx context.Context, path string) (int64, error)

	// GetURL 获取文件访问URL
	GetURL(ctx context.Context, path string) (string, error)

	// List 列出文件
	List(ctx context.Context, prefix string) ([]FileInfo, error)

	// Close 关闭存储连接
	Close() error

	GetKey() string
}

// Writer 写入器接口
type Writer interface {
	io.Writer
	io.WriterAt
	io.Closer

	// Sync 同步到存储
	Sync() error

	// Seek 设置写入位置
	Seek(offset int64, whence int) (int64, error)
}

// Reader 读取器接口
type Reader interface {
	io.Reader
	io.ReaderAt
	io.Closer
	io.Seeker
}

type File interface {
	Writer
	Reader
	Stat() (os.FileInfo, error)
	Name() string
	// SetMetadata 在上传到对象存储时携带自定义用户元数据（key/value）。
	// 须在 Close 触发上传前调用才生效。本地存储可提供空实现。
	SetMetadata(key, value string)
}

// Discarder 是 storage.File 的可选能力：主动放弃尚未持久化的内容。
//
// 它只关闭本地句柄并删除本地数据（对象存储=本地暂存文件，本地存储=目标文件），
// 绝不触发上传。用于录制片段因低于最小录制时长被整段丢弃时的清理——这类片段
// 的文件句柄还开着，若调用 Close 会先把垃圾对象传上对象存储，正好与丢弃相反。
type Discarder interface {
	// Discard 关闭句柄并删除本地数据，不执行任何上传。可重复调用。
	//
	// 删除或关闭失败必须返回错误：调用方据此告警。把错误吞掉会让被丢弃片段
	// 的临时文件静默残留在扫描目录里，恢复扫描再把它当崩溃残留上传——
	// 丢弃语义被静默破坏，且没有任何痕迹。
	Discard() error
}

// FileInfo 文件信息
type FileInfo struct {
	Name         string    `json:"name"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	ETag         string    `json:"etag,omitempty"`
	ContentType  string    `json:"content_type,omitempty"`
}

// CreateStorage 创建存储实例的便捷函数
func CreateStorage(t string, config any) (Storage, error) {
	factory, exists := Factory[t]
	if !exists {
		return nil, ErrUnsupportedStorageType
	}
	return factory(config)
}

// 错误定义
var (
	ErrUnsupportedStorageType   = fmt.Errorf("unsupported storage type")
	ErrInvalidStorageConfig     = fmt.Errorf("invalid storage config")
	ErrFileNotFound             = fmt.Errorf("file not found")
	ErrStorageNotAvailable      = fmt.Errorf("storage not available")
	ErrStorageTypeNotConfigured = fmt.Errorf("storage type not configured")
	ErrStorageRegistryClosed    = fmt.Errorf("storage registry closed")
)
