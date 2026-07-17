package m7s

import (
	"context"
	"encoding/json"
	"os"
	"time"

	task "github.com/eanfs/gotask"
	"m7s.live/v5/pkg/config"
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

	// 回收卡在 Uploading 状态的任务(进程崩溃 / goroutine 超时残留),扫回 Failed
	if n, err := ReclaimStaleUploading(u.s.DB, staleUploadingThreshold); err != nil {
		u.Error("reclaim stale uploading", "err", err)
	} else if n > 0 {
		u.Info("reclaimed stale uploading tasks", "count", n)
	}

	// pending 目录水位巡检:接近上限即告警(RaiseUploadAlarm 自带去重,不会刷屏)
	if warn, detail := storage.PendingWatermarkExceeded(); warn {
		RaiseUploadAlarm(u.s.DB, config.AlarmDiskSpaceFull,
			"pending dir near full", "", storage.GetPendingDir(),
			"pending 暂存目录接近容量上限: "+detail)
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
		// pending 文件消失 = 该录像已实际丢失,必须与补传耗尽路径一样告警,
		// 否则运维永远收不到这条录像丢失的信号。
		RaiseUploadAlarm(u.s.DB, config.AlarmStorageException,
			"pending file missing", ut.StreamPath, ut.LocalPath,
			"补传暂存文件已丢失,该录像无法恢复: "+ut.ObjectKey)
		return
	}

	// 原子抢占:仅当任务仍为 Failed 时占用;抢不到说明已被其他 goroutine 处理
	if !MarkUploading(u.s.DB, ut.ID) {
		return
	}

	// ⚠️ 此处不能抢上传槽位:UploadLocalFile → storage.File.Close → uploadTempFile
	// 内部会抢同一个槽位。若这里先占一个再等内部的第二个,≥槽位数(默认 4)个补传
	// goroutine 同时运行时,全部槽位被外层占住、内层永远等不到 —— 上传系统整体
	// 死锁,连正常录制的上传一起冻结。并发控制统一交给 uploadTempFile 内部的槽位。
	// ctx 只约束本地文件打开与向暂存文件的拷贝阶段(上传阶段自带超时与重试)。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

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
		// 补传次数耗尽:不再被 QueryPendingUploads 命中,告警通知运维介入
		if ut.RetryCount+1 >= ut.MaxRetries {
			RaiseUploadAlarm(u.s.DB, config.AlarmStorageException,
				"upload retry exhausted", ut.StreamPath, ut.LocalPath,
				"补传次数已耗尽,文件待人工处理: "+ut.ObjectKey)
		}
		return
	}

	u.Info("retry upload succeeded",
		"id", ut.ID,
		"objectKey", ut.ObjectKey,
		"retryCount", ut.RetryCount+1)
	MarkUploadSuccess(u.s.DB, ut.ID, ut.LocalPath)
}
