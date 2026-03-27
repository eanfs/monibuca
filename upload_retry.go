package m7s

import (
	"context"
	"encoding/json"
	"os"
	"time"

	task "github.com/langhuihui/gotask"
	"m7s.live/v5/pkg/storage"
)

// UploadRetryScheduler 定时补传调度器，周期性检查并重试失败的上传任务
type UploadRetryScheduler struct {
	task.TickTask
	s             *Server
	retryInterval time.Duration
}

// GetTickInterval 补传检查间隔（默认 5 分钟）
func (u *UploadRetryScheduler) GetTickInterval() time.Duration {
	if u.retryInterval > 0 {
		return u.retryInterval
	}
	return 5 * time.Minute
}

// Tick 每个周期执行一次补传检查
func (u *UploadRetryScheduler) Tick(any) {
	if u.s == nil || u.s.DB == nil || u.s.Storage == nil {
		return
	}

	// 查询待重试的任务（每次最多处理 20 个，避免单次过多）
	tasks, err := QueryPendingUploads(u.s.DB, 20)
	if err != nil {
		u.Error("query pending uploads", "err", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	u.Info("found pending uploads", "count", len(tasks))

	for i := range tasks {
		ut := tasks[i]
		go u.retryUpload(ut)
	}
}

// retryUpload 执行单个上传重试
func (u *UploadRetryScheduler) retryUpload(ut UploadTask) {
	// 检查本地文件是否存在
	if _, err := os.Stat(ut.LocalPath); os.IsNotExist(err) {
		u.Warn("local file not found, marking permanently failed",
			"id", ut.ID, "path", ut.LocalPath)
		// 标记为超出最大重试（不再重试）
		MarkUploadRetryFailed(u.s.DB, ut.ID, ut.MaxRetries, err)
		return
	}

	// 获取上传槽位（并发控制）
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := storage.AcquireUploadSlot(ctx); err != nil {
		u.Warn("acquire upload slot timeout", "id", ut.ID, "err", err)
		return
	}
	defer storage.ReleaseUploadSlot()

	// 标记为上传中
	MarkUploading(u.s.DB, ut.ID)

	// 解析元数据
	var metadata map[string]string
	if ut.MetadataJSON != "" {
		json.Unmarshal([]byte(ut.MetadataJSON), &metadata)
	}

	u.Info("retrying upload",
		"id", ut.ID,
		"objectKey", ut.ObjectKey,
		"retryCount", ut.RetryCount,
		"fileSize", ut.FileSize)

	// 执行上传
	err := UploadLocalFile(ctx, u.s.Storage, ut.LocalPath, ut.ObjectKey, metadata)
	if err != nil {
		u.Warn("retry upload failed",
			"id", ut.ID,
			"objectKey", ut.ObjectKey,
			"retryCount", ut.RetryCount+1,
			"err", err)
		MarkUploadRetryFailed(u.s.DB, ut.ID, ut.RetryCount, err)
		return
	}

	u.Info("retry upload succeeded",
		"id", ut.ID,
		"objectKey", ut.ObjectKey,
		"retryCount", ut.RetryCount+1)
	MarkUploadSuccess(u.s.DB, ut.ID, ut.LocalPath)
}
