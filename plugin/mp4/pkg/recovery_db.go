package mp4

import (
	"context"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
	"m7s.live/v5"
)

// recoveryDBTimeout 是恢复回写 record_streams 的超时。恢复是后台任务，
// 不受录制帧回调的 2s 限制；取与正常延迟入库一致的 10s。
const recoveryDBTimeout = 10 * time.Second

// recoveredRecordDuration 由索引记录算出本段录像时长（毫秒）。
// 索引已按文件实际长度截断，最后一条 sample 的 DTS+Duration 即媒体时长。
func recoveredRecordDuration(records []IndexRecord) uint32 {
	var end uint64
	for _, record := range records {
		finish := uint64(record.DTS) + uint64(record.Duration)
		if finish > end {
			end = finish
		}
	}
	if end > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(end)
}

// updateRecoveredRecord 在恢复成功后回写 record_streams。
//
// 崩溃前开录已插入一行（end_time 空、duration=0），旧的 DB 补录（RunDBBackfill）
// 对已有记录直接跳过，因此恢复流程必须自己补偿：按 header 的 FilePath 匹配，
// 更新 end_time/duration/file_path/storage_type；记录不存在则新建。
//
// end_time = header.StartTime + 索引算出的时长（spec §2.5）。File 保持原样：
// header.FilePath 是逻辑录像路径，也是 record_streams.file_path 的匹配键。
func updateRecoveredRecord(db *gorm.DB, logger *slog.Logger, header IndexHeader, records []IndexRecord) error {
	if db == nil {
		return nil
	}
	filePath := strings.TrimSpace(header.FilePath)
	if filePath == "" {
		filePath = strings.TrimSpace(header.StorageKey)
	}
	if filePath == "" {
		logger.Warn("recovered record has no filePath/storageKey; skip record_streams update",
			"streamPath", header.StreamPath, "mediaPath", header.MediaPath)
		return nil
	}

	duration := recoveredRecordDuration(records)
	endTime := time.Time{}
	if !header.StartTime.IsZero() && duration > 0 {
		endTime = header.StartTime.Add(time.Duration(duration) * time.Millisecond)
	}

	updates := map[string]any{
		"file_path": filePath,
	}
	if duration > 0 {
		updates["duration"] = duration
	}
	if !endTime.IsZero() {
		updates["end_time"] = endTime
	}
	if storageType := strings.TrimSpace(header.StorageType); storageType != "" {
		updates["storage_type"] = storageType
	}

	dbCtx, cancel := context.WithTimeout(context.Background(), recoveryDBTimeout)
	defer cancel()

	// 先用 file_path + start_time 精确定位本段录像：分片录制可能复用同一
	// fileName，只按 file_path 会一次改到多条记录。
	query := db.WithContext(dbCtx).Model(&m7s.RecordStream{}).Where("file_path = ?", filePath)
	if !header.StartTime.IsZero() {
		query = query.Where("start_time = ?", header.StartTime)
	} else {
		// 无起始时间时只能定位「尚未收尾」的那一行。
		query = query.Where("end_time IS NULL OR end_time = ?", time.Time{})
	}
	result := query.Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 && !header.StartTime.IsZero() {
		// 兼容历史 sidecar：起始时间与 DB 行不一致时，退化为更新该路径下
		// 尚未收尾（end_time 为空）的那一行，避免把已完成的分片一起改掉。
		result = db.WithContext(dbCtx).Model(&m7s.RecordStream{}).
			Where("file_path = ? AND (end_time IS NULL OR end_time = ?)", filePath, time.Time{}).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
	}
	if result.RowsAffected > 0 {
		logger.Info("record_streams updated after recovery",
			"filePath", filePath,
			"duration", duration,
			"storageType", header.StorageType)
		return nil
	}

	record := m7s.RecordStream{
		StartTime:    header.StartTime,
		EndTime:      endTime,
		Duration:     duration,
		FileName:     filepath.Base(filePath),
		Type:         "mp4",
		FilePath:     filePath,
		StreamPath:   header.StreamPath,
		StorageLevel: 1,
		StorageType:  strings.TrimSpace(header.StorageType),
	}
	if err := db.WithContext(dbCtx).Create(&record).Error; err != nil {
		return err
	}
	logger.Info("record_streams created after recovery",
		"filePath", filePath,
		"duration", duration,
		"storageType", header.StorageType)
	return nil
}
