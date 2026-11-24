package m7s

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gorm.io/gorm"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

// Issue #39：延迟入库失败后的持久化重试。
//
// 媒体已经持久化（上传成功，或上传失败后已保留、移交补传队列），但
// record_streams 的延迟写库可能因磁盘高负载下 SQLite fsync 变慢而超过 context
// 期限。此时仅告警会留下孤儿文件：end_time 为 NULL、duration 为 0，列表与
// 时间范围查询永远查不到它。
//
// 这里复用已有的 upload_tasks 表承载「仅待写库」的任务，避免新表迁移：
//   - MetadataJSON.kind = "record-db" 明确标记该行不涉及上传；
//   - MetadataJSON["record-stream"] 是 RecordStream 的 JSON 快照；
//   - MetadataJSON["record-event"]（可选）是关联 RecordEvent 的快照。
//
// UploadRetryScheduler 的 5 分钟 tick 会命中它：status=Failed、next_retry_at
// 到期、且未耗尽重试次数（新建行的 next_retry_at = now+1min，首个 tick 可能
// 略过；与普通上传任务共用每次 LIMIT 20 的配额）。命中后只执行 DB 写入并重试。
// 行已落库，进程重启后仍能补上。

const (
	// uploadTaskKindKey/uploadTaskKindRecordDB 是 upload_tasks.metadata 的
	// 「任务种类」标记。老行没有该键，按普通上传任务处理。
	uploadTaskKindKey      = "kind"
	uploadTaskKindRecordDB = "record-db"

	// recordDBStreamMetadataKey / recordDBEventMetadataKey 保存定稿 payload。
	recordDBStreamMetadataKey = "record-stream"
	recordDBEventMetadataKey  = "record-event"

	// recordDBRetryMaxRetries 与 SaveFailedUpload 的默认值保持一致。
	recordDBRetryMaxRetries = 10

	// recordDBRetryPersistTimeout 是「把重试任务写进 upload_tasks」这一步的超时。
	//
	// 这里刻意不用 30s 的定稿超时：本函数正是在延迟入库已超时/DB 已卡住时被
	// 调用的，而 spillToPending 路径跑在单线程的 trailer 队列上——无界或过长的
	// 等待会拖住所有录像的收尾。单行 INSERT 正常 <1ms，5s 足够；超时说明 DB
	// 确实不可用，再等也不会更好，调用方会照常记 Error 并告警。
	recordDBRetryPersistTimeout = 5 * time.Second
)

// recordDBRetryPayload 是一条「仅待写库」任务恢复出的定稿数据。
type recordDBRetryPayload struct {
	Stream RecordStream        `json:"stream"`
	Event  *config.RecordEvent `json:"event,omitempty"`
}

// saveRecordDBRetry 把延迟入库的定稿数据持久化进 upload_tasks，供补传调度器
// 重试。返回错误表示重试任务没有落库（DB 不可用 / 磁盘满等），调用方必须
// 记 Error 并告警——绝不能把「没写进去」当成「已安排重试」。
func saveRecordDBRetry(db *gorm.DB, stream RecordStream, event *config.RecordEvent, cause error) error {
	if db == nil {
		return fmt.Errorf("persist record_streams retry for %s: no database", stream.FilePath)
	}
	streamJSON, err := json.Marshal(stream)
	if err != nil {
		return fmt.Errorf("marshal record_streams retry payload for %s: %w", stream.FilePath, err)
	}
	metadata := map[string]string{
		uploadTaskKindKey:         uploadTaskKindRecordDB,
		recordDBStreamMetadataKey: string(streamJSON),
	}
	if event != nil {
		eventJSON, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal record_event retry payload for %s: %w", stream.FilePath, err)
		}
		metadata[recordDBEventMetadataKey] = string(eventJSON)
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("marshal record_streams retry metadata for %s: %w", stream.FilePath, err)
	}
	var errMsg string
	if cause != nil {
		errMsg = cause.Error()
		if len(errMsg) > 1024 {
			errMsg = errMsg[:1024]
		}
	}
	// LocalPath/ObjectKey 刻意留空：该行没有文件要上传。两个「同路径产物」查询
	// （ConfirmPendingRegistration / recoveryUploadRegistered）都以**调用方传入的**
	// localPath 是否为空提前返回，且查询条件要求与行内的 local_path/object_key
	// 相等——空值不可能等于任何非空路径，因此 record-db 行不会被误命中。
	task := UploadTask{
		Status:       UploadStatusFailed,
		MaxRetries:   recordDBRetryMaxRetries,
		ErrorMessage: errMsg,
		StreamPath:   stream.StreamPath,
		MetadataJSON: string(metadataJSON),
		NextRetryAt:  time.Now().Add(nextRetryDelay(0)),
	}
	ctx, cancel := context.WithTimeout(context.Background(), recordDBRetryPersistTimeout)
	defer cancel()
	if err := db.WithContext(ctx).Create(&task).Error; err != nil {
		return fmt.Errorf("persist record_streams retry for %s: %w", stream.FilePath, err)
	}
	return nil
}

// isRecordDBRetryTask 判断一条 upload_tasks 行是否只承载 record_streams 定稿，
// 不涉及任何文件上传。metadata 损坏时按普通上传任务处理（保守），让原有
// 上传路径决定如何处置，而不是把一条未知行当作 DB 重试静默吞掉。
func isRecordDBRetryTask(ut UploadTask) bool {
	if ut.MetadataJSON == "" {
		return false
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(ut.MetadataJSON), &metadata); err != nil {
		return false
	}
	return metadata[uploadTaskKindKey] == uploadTaskKindRecordDB
}

// parseRecordDBRetry 从 upload_tasks 行恢复定稿 payload。
func parseRecordDBRetry(ut UploadTask) (recordDBRetryPayload, error) {
	var metadata map[string]string
	if err := json.Unmarshal([]byte(ut.MetadataJSON), &metadata); err != nil {
		return recordDBRetryPayload{}, fmt.Errorf("decode record_streams retry metadata for task %d: %w", ut.ID, err)
	}
	var payload recordDBRetryPayload
	if err := json.Unmarshal([]byte(metadata[recordDBStreamMetadataKey]), &payload.Stream); err != nil {
		return recordDBRetryPayload{}, fmt.Errorf("decode record_streams retry payload for task %d: %w", ut.ID, err)
	}
	if raw := metadata[recordDBEventMetadataKey]; raw != "" {
		var event config.RecordEvent
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			return recordDBRetryPayload{}, fmt.Errorf("decode record_event retry payload for task %d: %w", ut.ID, err)
		}
		payload.Event = &event
	}
	return payload, nil
}

// writeRecordDBRetry 执行一次定稿补写，语义与 WriteTailDeferred 的闭包一致：
// RecordEvent 写失败只影响事件字段，不阻塞 record_streams 定稿。
// 返回值分开，供调用方对两次写入分别记日志。
func writeRecordDBRetry(db *gorm.DB, payload recordDBRetryPayload) (eventErr, streamErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), storage.RecordDBWriteTimeout())
	defer cancel()
	if payload.Event != nil {
		if err := db.WithContext(ctx).Save(&EventRecordStream{
			RecordEvent:  payload.Event,
			RecordStream: payload.Stream,
		}).Error; err != nil {
			eventErr = err
		}
	}
	if err := db.WithContext(ctx).Save(&payload.Stream).Error; err != nil {
		streamErr = err
	}
	return eventErr, streamErr
}
