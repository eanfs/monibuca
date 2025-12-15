package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	task "github.com/langhuihui/gotask"
	"gorm.io/gorm"
)

// 上传状态常量
const (
	UploadStatusPending   = "pending"   // 等待上传
	UploadStatusUploading = "uploading" // 上传中
	UploadStatusCompleted = "completed" // 已完成
	UploadStatusFailed    = "failed"    // 失败
)

// UploadQueueTask 上传队列任务（全局单例）
// 用于管理所有录制文件的异步上传
type UploadQueueTask struct {
	task.Work
	maxConcurrent int           // 最大并发上传数
	semaphore     chan struct{} // 并发控制信号量
	retryInterval time.Duration // 重试间隔
	maxRetries    int           // 最大重试次数
}

var (
	uploadQueue     *UploadQueueTask
	uploadQueueOnce sync.Once
)

// InitUploadQueue 初始化上传队列（在服务器启动时调用）
func InitUploadQueue(maxConcurrent int) *UploadQueueTask {
	uploadQueueOnce.Do(func() {
		if maxConcurrent <= 0 {
			maxConcurrent = 5 // 默认5个并发
		}
		uploadQueue = &UploadQueueTask{
			maxConcurrent: maxConcurrent,
			semaphore:     make(chan struct{}, maxConcurrent),
			retryInterval: time.Minute * 5,
			maxRetries:    3,
		}
	})
	return uploadQueue
}

// GetUploadQueue 获取上传队列实例
func GetUploadQueue() *UploadQueueTask {
	return uploadQueue
}

// DocumentUploader 支持直接上传文件的存储接口
type DocumentUploader interface {
	UploadFile(ctx context.Context, key string, localPath string) error
}

// UploadTask 单个文件上传任务
type UploadTask struct {
	task.Task
	RecordID      uint           // 录像记录ID
	LocalPath     string         // 本地文件路径
	RemotePath    string         // 远程存储路径
	StorageConfig map[string]any // 存储配置
	DB            *gorm.DB       // 数据库连接
	Queue         *UploadQueueTask
	RetryCount    int  // 当前重试次数
	DeleteLocal   bool // 上传成功后是否删除本地文件
}

// NewUploadTask 创建上传任务
func NewUploadTask(recordID uint, localPath, remotePath string, storageConfig map[string]any, db *gorm.DB, deleteLocal bool) *UploadTask {
	return &UploadTask{
		RecordID:      recordID,
		LocalPath:     localPath,
		RemotePath:    remotePath,
		StorageConfig: storageConfig,
		DB:            db,
		Queue:         uploadQueue,
		RetryCount:    0,
		DeleteLocal:   deleteLocal,
	}
}

func (t *UploadTask) Start() error {
	// Start 应该快速返回，不要进行阻塞操作
	// 信号量获取移至 Run 中

	// 更新状态为上传中（异步更新，或者快速更新）
	if t.DB != nil && t.RecordID > 0 {
		t.DB.Table("record_streams").Where("id = ?", t.RecordID).Updates(map[string]interface{}{
			"upload_status": UploadStatusUploading,
		})
	}

	return nil
}

func (t *UploadTask) Run() error {
	// 获取信号量，限制并发（在Run中阻塞是安全的）
	select {
	case t.Queue.semaphore <- struct{}{}:
		// 成功获取信号量
	case <-time.After(time.Minute * 30): // 增加超时时间到30分钟
		return t.handleUploadError(fmt.Errorf("failed to acquire upload semaphore, timeout"))
	case <-t.Context.Done():
		return t.Context.Err()
	}

	// 确保释放信号量
	defer func() {
		<-t.Queue.semaphore
	}()

	t.Info("starting upload", "recordID", t.RecordID, "localPath", t.LocalPath, "remotePath", t.RemotePath)

	// 检查本地文件是否存在
	fileInfo, err := os.Stat(t.LocalPath)
	if err != nil {
		return t.handleUploadError(fmt.Errorf("local file not found: %w", err))
	}

	// 确定存储类型并创建存储实例
	var storageInstance Storage
	var storageType string
	for sType, conf := range t.StorageConfig {
		if sType == "local" {
			continue // 跳过本地存储
		}
		storageInstance, err = CreateStorage(sType, conf)
		if err == nil {
			storageType = sType
			break
		}
		t.Warn("failed to create storage", "type", sType, "err", err)
	}

	if storageInstance == nil {
		return t.handleUploadError(fmt.Errorf("no valid remote storage configuration found"))
	}

	t.Info("using storage", "type", storageType)

	var totalWritten int64 = fileInfo.Size()

	// ⚡️ 优化：如果存储实现了上传接口，直接使用 UploadFile，避免双重拷贝
	if uploader, ok := storageInstance.(DocumentUploader); ok {
		t.Info("using fast direct upload path")
		if err := uploader.UploadFile(t.Context, t.RemotePath, t.LocalPath); err != nil {
			return t.handleUploadError(fmt.Errorf("direct upload failed: %w", err))
		}
	} else {
		// 常规路径：使用 Writer 接口（会产生临时文件）
		// 打开本地文件
		localFile, err := os.Open(t.LocalPath)
		if err != nil {
			return t.handleUploadError(fmt.Errorf("failed to open local file: %w", err))
		}
		defer localFile.Close()

		// 创建远程文件
		remoteFile, err := storageInstance.CreateFile(context.Background(), t.RemotePath)
		if err != nil {
			return t.handleUploadError(fmt.Errorf("failed to create remote file: %w", err))
		}

		// 复制文件内容
		buf := make([]byte, 1024*1024) // 1MB buffer
		totalWritten = 0
		for {
			n, readErr := localFile.Read(buf)
			if n > 0 {
				written, writeErr := remoteFile.Write(buf[:n])
				if writeErr != nil {
					remoteFile.Close()
					return t.handleUploadError(fmt.Errorf("failed to write to remote: %w", writeErr))
				}
				totalWritten += int64(written)
			}
			if readErr != nil {
				if readErr == io.EOF {
					break
				}
				remoteFile.Close()
				return t.handleUploadError(fmt.Errorf("failed to read local file: %w", readErr))
			}
		}

		// 关闭远程文件（触发实际上传）
		if err := remoteFile.Close(); err != nil {
			return t.handleUploadError(fmt.Errorf("failed to close remote file: %w", err))
		}
	}

	t.Info("upload completed", "recordID", t.RecordID, "bytes", totalWritten, "fileSize", fileInfo.Size())

	// 更新数据库状态为已完成
	if t.DB != nil && t.RecordID > 0 {
		t.DB.Table("record_streams").Where("id = ?", t.RecordID).Updates(map[string]interface{}{
			"upload_status": UploadStatusCompleted,
			"upload_error":  "",
			"storage_type":  storageType,
		})
	}

	// 上传成功后删除本地文件（可选）
	if t.DeleteLocal {
		if err := os.Remove(t.LocalPath); err != nil {
			t.Warn("failed to remove local file after upload", "err", err)
		} else {
			t.Info("local file removed after successful upload", "path", t.LocalPath)
		}
	}

	return task.ErrTaskComplete
}

func (t *UploadTask) handleUploadError(err error) error {
	t.Error("upload failed", "err", err, "retryCount", t.RetryCount)

	t.RetryCount++

	// 更新数据库状态
	if t.DB != nil && t.RecordID > 0 {
		status := UploadStatusPending
		if t.RetryCount >= t.Queue.maxRetries {
			status = UploadStatusFailed
			t.Error("max retries reached, marking as failed", "retryCount", t.RetryCount)
		} else {
			t.Info("will retry upload", "retryCount", t.RetryCount, "maxRetries", t.Queue.maxRetries)
		}

		t.DB.Table("record_streams").Where("id = ?", t.RecordID).Updates(map[string]interface{}{
			"upload_status": status,
			"upload_error":  err.Error(),
			"retry_count":   t.RetryCount,
			"last_retry_at": time.Now(),
		})
	}

	return err
}

// QueueUpload 将文件加入上传队列
func (q *UploadQueueTask) QueueUpload(uploadTask *UploadTask, logger *slog.Logger) {
	if q == nil {
		return
	}
	uploadTask.Queue = q
	if logger != nil {
		q.AddTask(uploadTask, logger.With("recordID", uploadTask.RecordID))
	} else {
		q.AddTask(uploadTask, nil)
	}
}

// UploadRetryTask 定时重试上传任务
type UploadRetryTask struct {
	task.TickTask
	DB            *gorm.DB
	Queue         *UploadQueueTask
	StorageConfig map[string]any
}

func (t *UploadRetryTask) GetTickInterval() time.Duration {
	return time.Minute * 5 // 每5分钟检查一次
}

func (t *UploadRetryTask) Tick(now any) {
	if t.DB == nil || t.Queue == nil {
		return
	}

	t.Info("checking for pending uploads")

	// 查询待上传和失败的记录
	var pendingRecords []struct {
		ID       uint
		FilePath string
	}

	// 查询需要重试的记录
	t.DB.Table("record_streams").
		Select("id, file_path").
		Where("upload_status IN ? AND storage_type = ?",
			[]string{UploadStatusPending, UploadStatusFailed},
			"local").
		Where("retry_count < ?", t.Queue.maxRetries).
		Find(&pendingRecords)

	t.Info("found pending uploads", "count", len(pendingRecords))

	for _, record := range pendingRecords {
		t.Info("retrying upload", "recordID", record.ID, "filePath", record.FilePath)

		uploadTask := NewUploadTask(
			record.ID,
			record.FilePath,
			record.FilePath, // 使用相同路径
			t.StorageConfig,
			t.DB,
			true, // 上传成功后删除本地文件
		)

		t.Queue.QueueUpload(uploadTask, nil) // 使用 nil logger
	}
}
