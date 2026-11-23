package m7s

import (
	"context"
	"encoding/json"
	"fmt"
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
	// pendingUsageErr 是上一次已告警的 pending 用量扫描错误。目录不可读会每个
	// tick 复现，同一个错误只告警一次（错误变化时重新告警），避免刷屏；恢复
	// 后清空并记一条 Info，让运维知道水位告警重新生效。
	pendingUsageErr string
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
	// 注意：storage 不可用不能整体跳过——「仅待写库」的 record_streams 定稿
	// 重试任务不依赖任何 storage，必须在存储故障期间也能补上。
	if u.s == nil || u.s.DB == nil {
		return
	}

	// 回收卡在 Uploading 状态的任务(进程崩溃 / goroutine 超时残留),扫回 Failed
	if n, err := ReclaimStaleUploading(u.s.DB, staleUploadingThreshold); err != nil {
		u.Error("reclaim stale uploading", "err", err)
	} else if n > 0 {
		u.Info("reclaimed stale uploading tasks", "count", n)
	}

	// pending 目录水位巡检:接近上限即告警(RaiseUploadAlarm 自带去重,不会刷屏)。
	// 日志字段固定为 count/bytes/limit,供现场按 30 天日志检索。
	// 扫描失败必须显式告警:过去被 `usageErr == nil` 静默吞掉,目录不可读时
	// 水位告警永久失效且没有任何日志。
	//
	// 未配置 size/files 上限时水位判定恒为 false,跳过整目录遍历:默认配置下
	// 每个 tick 的 WalkDir 只会算出一个用不上的数字(积压上万文件时是数万次
	// syscall/5min)。
	if !storage.PendingLimitsConfigured() {
		u.clearPendingUsageScanFailure()
	} else if usage, usageErr := storage.GetPendingUsage(); usageErr != nil {
		u.warnPendingUsageScanFailure(usageErr)
	} else {
		u.clearPendingUsageScanFailure()
		if exceeded, limit := usage.WatermarkExceeded(); exceeded {
			u.Warn("pending dir near full",
				"count", usage.Count,
				"bytes", usage.Bytes,
				"limit", limit,
				"sizeLimit", usage.SizeLimit,
				"fileLimit", usage.FileLimit)
			RaiseUploadAlarm(u.s.DB, config.AlarmDiskSpaceFull,
				"pending dir near full", "", storage.GetPendingDir(),
				fmt.Sprintf("pending 暂存目录接近容量上限: 已用 %d 字节/%d 文件, 上限 %d",
					usage.Bytes, usage.Count, limit))
		}
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
		// 「仅待写库」任务没有文件要上传，storage 缺失时也必须处理；
		// 普通上传任务保持旧行为：storage 不可用则本轮跳过。
		if !isRecordDBRetryTask(ut) && u.s.GetStorage() == nil {
			continue
		}
		go u.retryUpload(ut)
	}
}

// warnPendingUsageScanFailure 记录 pending 用量扫描失败（同一错误只告警一次）。
// 只有 Tick 调用，Tick 由任务事件循环串行执行，字段无需加锁。
func (u *UploadRetryScheduler) warnPendingUsageScanFailure(err error) {
	msg := err.Error()
	if msg == u.pendingUsageErr {
		return
	}
	u.pendingUsageErr = msg
	u.Warn("pending dir usage scan failed",
		"error", err,
		"pendingDir", storage.GetPendingDir(),
		"effect", "watermark alarm disabled")
}

// clearPendingUsageScanFailure 在扫描恢复后清空去重状态并提示一次，
// 否则运维无从判断水位告警是否已重新生效。
func (u *UploadRetryScheduler) clearPendingUsageScanFailure() {
	if u.pendingUsageErr == "" {
		return
	}
	u.pendingUsageErr = ""
	u.Info("pending dir usage scan recovered",
		"pendingDir", storage.GetPendingDir(),
		"effect", "watermark alarm re-enabled")
}

// retryUpload 执行单个上传重试
func (u *UploadRetryScheduler) retryUpload(ut UploadTask) {
	if u.s == nil || u.s.DB == nil {
		return
	}
	// record_streams 延迟入库的补写任务不涉及文件上传，单独走 DB 重试路径。
	if isRecordDBRetryTask(ut) {
		u.retryRecordDB(ut)
		return
	}
	st := u.s.GetStorage()
	if st == nil {
		u.Warn("retry upload skipped: storage unavailable",
			"id", ut.ID,
			"taskStorageType", ut.StorageType)
		return
	}
	activeStorageType := st.GetKey()
	if activeStorageType != ut.StorageType {
		u.Warn("retry upload skipped: storage type mismatch",
			"id", ut.ID,
			"taskStorageType", ut.StorageType,
			"activeStorageType", activeStorageType)
		return
	}

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
		"streamPath", ut.StreamPath,
		"file", ut.ObjectKey,
		"objectKey", ut.ObjectKey,
		"size", ut.FileSize,
		"fileSize", ut.FileSize,
		"retryCount", ut.RetryCount)

	// 执行上传
	err := UploadLocalFile(ctx, st, ut.LocalPath, ut.ObjectKey, metadata)
	if err != nil {
		// 与队列首次失败（record.go 的 upload failed）保持同一检索契约：
		// Error 级 + error + streamPath。现场按 Error 检索不能漏掉补传失败。
		u.Error("retry upload failed",
			"id", ut.ID,
			"streamPath", ut.StreamPath,
			"file", ut.ObjectKey,
			"objectKey", ut.ObjectKey,
			"size", ut.FileSize,
			"retryCount", ut.RetryCount+1,
			"errorClass", storage.UploadErrorClass(err),
			"nextRetryAt", NextUploadRetryAt(ut.RetryCount+1),
			"error", err)
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
		"streamPath", ut.StreamPath,
		"file", ut.ObjectKey,
		"objectKey", ut.ObjectKey,
		"size", ut.FileSize,
		"retryCount", ut.RetryCount+1)
	MarkUploadSuccess(u.s.DB, ut.ID, ut.LocalPath)
}

// retryRecordDB 只补写 record_streams（及可选的 RecordEvent），不触碰任何文件。
// 这是延迟入库失败（磁盘高负载下 DB 写超时）的补偿路径：任务行已持久化，
// 进程重启后仍会被调度。成功 → 该行标记 Success；失败 → 保留行并按指数
// 退避重试，次数耗尽后告警（沿用 "record db save failed" 便于现场检索）。
func (u *UploadRetryScheduler) retryRecordDB(ut UploadTask) {
	payload, err := parseRecordDBRetry(ut)
	if err != nil {
		// payload 损坏已无法补写：按耗尽处理并告警，不能让这条录像永久
		// 无声地缺失索引记录。
		u.Error("record db retry payload invalid; marking permanently failed",
			"id", ut.ID, "streamPath", ut.StreamPath,
			"retryCount", ut.RetryCount, "error", err)
		MarkUploadRetryFailed(u.s.DB, ut.ID, ut.MaxRetries, err)
		RaiseUploadAlarm(u.s.DB, config.AlarmStorageException,
			"record db save failed", ut.StreamPath, ut.StreamPath,
			"record_streams 定稿重试数据损坏, 无法补写: "+err.Error())
		return
	}
	// 与上传补传共用原子抢占，避免重复 tick / 多实例同时写同一条记录。
	if !MarkUploading(u.s.DB, ut.ID) {
		return
	}
	eventErr, streamErr := writeRecordDBRetry(u.s.DB, payload)
	if eventErr != nil {
		// 与 WriteTailDeferred 一致：事件字段写失败不阻塞 record_streams 定稿。
		u.Warn("retry record event db save failed (non-fatal)",
			"id", ut.ID, "streamPath", ut.StreamPath,
			"file", payload.Stream.FilePath, "error", eventErr)
	}
	if streamErr != nil {
		u.Error("retry record db save failed",
			"id", ut.ID,
			"streamPath", ut.StreamPath,
			"file", payload.Stream.FilePath,
			"retryCount", ut.RetryCount+1,
			"nextRetryAt", NextUploadRetryAt(ut.RetryCount+1),
			"error", streamErr)
		MarkUploadRetryFailed(u.s.DB, ut.ID, ut.RetryCount, streamErr)
		if ut.RetryCount+1 >= ut.MaxRetries {
			RaiseUploadAlarm(u.s.DB, config.AlarmStorageException,
				"record db save failed", ut.StreamPath, payload.Stream.FilePath,
				"record_streams 定稿补写次数已耗尽, 该录像将永久缺失索引记录: "+streamErr.Error())
		}
		return
	}
	u.Info("retry record db save succeeded",
		"id", ut.ID,
		"streamPath", ut.StreamPath,
		"file", payload.Stream.FilePath,
		"retryCount", ut.RetryCount+1)
	// 没有本地文件：第三个参数为空，MarkUploadSuccess 只更新状态。
	MarkUploadSuccess(u.s.DB, ut.ID, "")
}
