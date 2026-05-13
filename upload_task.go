package m7s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"gorm.io/gorm"
	"m7s.live/v5/pkg/storage"
)

// UploadStatus 上传任务状态
type UploadStatus int

const (
	UploadStatusPending   UploadStatus = 0 // 等待上传
	UploadStatusUploading UploadStatus = 1 // 上传中
	UploadStatusSuccess   UploadStatus = 2 // 上传成功
	UploadStatusFailed    UploadStatus = 3 // 上传失败（等待重试）
)

// UploadTask 上传任务持久化模型，用于追踪失败上传和定时补传
type UploadTask struct {
	ID           uint         `gorm:"primarykey"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LocalPath    string       `gorm:"size:512;index" json:"localPath" desc:"本地文件路径"`
	ObjectKey    string       `gorm:"size:512" json:"objectKey" desc:"远端对象键"`
	StorageType  string       `gorm:"size:20" json:"storageType" desc:"存储类型(s3/oss/cos)"`
	Status       UploadStatus `gorm:"default:0;index" json:"status" desc:"状态: 0=待传 1=传输中 2=成功 3=失败"`
	RetryCount   int          `gorm:"default:0" json:"retryCount" desc:"已重试次数"`
	MaxRetries   int          `gorm:"default:10" json:"maxRetries" desc:"最大重试次数"`
	FileSize     int64        `json:"fileSize" desc:"文件大小(字节)"`
	ErrorMessage string       `gorm:"size:1024" json:"errorMessage" desc:"最近一次错误信息"`
	StreamPath   string       `gorm:"size:255;index" json:"streamPath" desc:"关联流路径"`
	DurationMs   uint32       `json:"durationMs" desc:"视频时长(毫秒)"`
	MetadataJSON string       `gorm:"column:metadata;type:text" json:"metadata" desc:"JSON编码的用户元数据"`
	NextRetryAt  time.Time    `gorm:"index" json:"nextRetryAt" desc:"下次重试时间"`
}

// TableName GORM 表名
func (UploadTask) TableName() string {
	return "upload_tasks"
}

// nextRetryDelay 根据重试次数计算下次重试延迟（指数退避）
func nextRetryDelay(retryCount int) time.Duration {
	delays := []time.Duration{
		1 * time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		1 * time.Hour,
		6 * time.Hour,
	}
	if retryCount < len(delays) {
		return delays[retryCount]
	}
	return 24 * time.Hour
}

// SaveFailedUpload 保存失败的上传任务到数据库
func SaveFailedUpload(db *gorm.DB, localPath, objectKey, storageType, streamPath string,
	fileSize int64, durationMs uint32, metadata map[string]string, uploadErr error) {
	if db == nil {
		return
	}
	var metaJSON string
	if len(metadata) > 0 {
		if b, err := json.Marshal(metadata); err == nil {
			metaJSON = string(b)
		}
	}
	var errMsg string
	if uploadErr != nil {
		errMsg = uploadErr.Error()
		if len(errMsg) > 1024 {
			errMsg = errMsg[:1024]
		}
	}
	task := UploadTask{
		LocalPath:    localPath,
		ObjectKey:    objectKey,
		StorageType:  storageType,
		Status:       UploadStatusFailed,
		FileSize:     fileSize,
		ErrorMessage: errMsg,
		StreamPath:   streamPath,
		DurationMs:   durationMs,
		MetadataJSON: metaJSON,
		MaxRetries:   10,
		NextRetryAt:  time.Now().Add(nextRetryDelay(0)),
	}
	db.Create(&task)
}

// QueryPendingUploads 查询待重试的上传任务
func QueryPendingUploads(db *gorm.DB, limit int) ([]UploadTask, error) {
	var tasks []UploadTask
	now := time.Now()
	err := db.Where("status = ? AND next_retry_at <= ? AND retry_count < max_retries",
		UploadStatusFailed, now).
		Order("next_retry_at ASC").
		Limit(limit).
		Find(&tasks).Error
	return tasks, err
}

// MarkUploading 标记任务为上传中
func MarkUploading(db *gorm.DB, taskID uint) {
	db.Model(&UploadTask{}).Where("id = ?", taskID).Update("status", UploadStatusUploading)
}

// MarkUploadSuccess 标记任务上传成功，删除本地文件
func MarkUploadSuccess(db *gorm.DB, taskID uint, localPath string) {
	db.Model(&UploadTask{}).Where("id = ?", taskID).Updates(map[string]any{
		"status":        UploadStatusSuccess,
		"error_message": "",
	})
	if localPath != "" {
		os.Remove(localPath)
	}
}

// MarkUploadRetryFailed 标记本次重试失败，更新重试计数和下次重试时间
func MarkUploadRetryFailed(db *gorm.DB, taskID uint, retryCount int, err error) {
	var errMsg string
	if err != nil {
		errMsg = err.Error()
		if len(errMsg) > 1024 {
			errMsg = errMsg[:1024]
		}
	}
	db.Model(&UploadTask{}).Where("id = ?", taskID).Updates(map[string]any{
		"status":        UploadStatusFailed,
		"retry_count":   retryCount + 1,
		"error_message": errMsg,
		"next_retry_at": time.Now().Add(nextRetryDelay(retryCount + 1)),
	})
}

// UploadLocalFile 将本地文件上传到云存储（通用方法，用于补传）
func UploadLocalFile(ctx context.Context, st storage.Storage, localPath, remotePath string, metadata map[string]string) error {
	local, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open local file %s: %w", localPath, err)
	}
	defer local.Close()

	remote, err := st.CreateFile(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("create remote file %s: %w", remotePath, err)
	}

	if _, err = io.Copy(remote, local); err != nil {
		remote.Close()
		return fmt.Errorf("copy to remote: %w", err)
	}

	for k, v := range metadata {
		remote.SetMetadata(k, v)
	}

	if err = remote.Close(); err != nil {
		return fmt.Errorf("close remote file (upload): %w", err)
	}
	return nil
}
