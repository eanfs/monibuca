package mp4

import (
	"context"
	"errors"
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

// truncateRecordTime 把时间归一到毫秒。
//
// record_streams.start_time/end_time 在 MySQL 上是 DATETIME(3)（driver 默认
// DefaultDatetimePrecision=3）：写入时 MySQL 会把纳秒截断/四舍五入到毫秒，
// 而查询谓词绑定的字面量仍带纳秒精度，等值比较永远不成立。于是恢复重跑时
// 精确查询落空 → 走 fallback → fallback 也落空（end_time 参数同样是纳秒）
// → Create 出重复行。写入值与所有查询谓词必须使用同一毫秒精度。
func truncateRecordTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Truncate(time.Millisecond)
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
	if err := ValidateIndexHeaderPaths(header); err != nil {
		// 不阻塞上传：产物已经落盘/可上传，只是这条元数据不可信。
		// 写进 DB 会让下载接口按 file_path 直接打开任意文件。
		logger.Warn("recovered record filePath is not bound to mediaPath; skip record_streams update",
			"err", err,
			"streamPath", header.StreamPath,
			"filePath", header.FilePath,
			"mediaPath", header.MediaPath)
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

	// 入口即归一化：写入值与精确查询谓词共用同一 startTime。
	startTime := truncateRecordTime(header.StartTime)
	duration := recoveredRecordDuration(records)
	endTime := time.Time{}
	if !startTime.IsZero() && duration > 0 {
		endTime = startTime.Add(time.Duration(duration) * time.Millisecond)
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

	// 判存必须用 SELECT，不能用 RowsAffected：MySQL 未开 clientFoundRows 时，
	// 对值完全相同的 UPDATE 返回 0。恢复成功写库后若 sidecar 删除失败、或进程
	// 在两者之间被杀，下次启动会再恢复一次——查询能命中行但 UPDATE 是 no-op，
	// 若据此退化到 Create 就会产生重复的 record_streams 行。
	//
	// 定位顺序：
	//  1. file_path + start_time 精确定位本段录像（分片录制可能复用同一
	//     fileName，只按 file_path 会一次改到多条记录）；start_time 已是毫秒精度，
	//     与列精度一致，新版本写入的行必被命中；
	//  2. 历史行（旧版本按纳秒写入，MySQL DATETIME(3) 可能四舍五入到相邻毫秒）
	//     用 ±1ms 窄区间再兜底一次；
	//  3. 历史 sidecar 的 start_time 与 DB 不一致时，退化为该路径下可认领的
	//     行：尚未收尾（end_time 为空），或与本次恢复结果完全一致
	//     （end_time + duration 相同——上次 fallback 已写入，重跑必须幂等）。
	//     后者避免“第一次 fallback 写入后该行不再未收尾，重跑又 Create”；
	//     已完成的历史分片 end_time 各不相同，不会被误认领；
	//  4. 以上都为空才 Create。
	var existing m7s.RecordStream
	found := false

	exact := db.WithContext(dbCtx).Where("file_path = ?", filePath)
	if !startTime.IsZero() {
		exact = exact.Where("start_time = ?", startTime)
	} else {
		// 无起始时间时只能定位「尚未收尾」的那一行。
		exact = exact.Where("(end_time IS NULL OR end_time = ?)", time.Time{})
	}
	findErr := exact.First(&existing).Error
	switch {
	case findErr == nil:
		found = true
	case !errors.Is(findErr, gorm.ErrRecordNotFound):
		return findErr
	}

	if !found && !startTime.IsZero() {
		// 历史行兜底：旧版本写入时未做精度归一化，MySQL 对 DATETIME(3) 的
		// 舍入可让库内毫秒值与 header 截断值相差 1ms。
		historical := db.WithContext(dbCtx).Where("file_path = ?", filePath).
			Where("start_time BETWEEN ? AND ?",
				startTime.Add(-time.Millisecond), startTime.Add(time.Millisecond))
		historicalErr := historical.First(&existing).Error
		switch {
		case historicalErr == nil:
			found = true
		case !errors.Is(historicalErr, gorm.ErrRecordNotFound):
			return historicalErr
		}
	}

	if !found && !startTime.IsZero() {
		fallback := db.WithContext(dbCtx).Where("file_path = ?", filePath)
		if !endTime.IsZero() && duration > 0 {
			// end_time 的 ±1ms 同样服务于旧版本写入的历史行。
			fallback = fallback.Where(
				"(end_time IS NULL OR end_time = ? OR (end_time BETWEEN ? AND ? AND duration = ?))",
				time.Time{}, endTime.Add(-time.Millisecond), endTime.Add(time.Millisecond), duration)
		} else {
			fallback = fallback.Where("(end_time IS NULL OR end_time = ?)", time.Time{})
		}
		fallbackErr := fallback.First(&existing).Error
		switch {
		case fallbackErr == nil:
			found = true
		case !errors.Is(fallbackErr, gorm.ErrRecordNotFound):
			return fallbackErr
		}
	}

	if found {
		// 只按主键更新命中行；即使值完全相同（MySQL RowsAffected=0）也算成功。
		if err := db.WithContext(dbCtx).Model(&m7s.RecordStream{}).
			Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
			return err
		}
		logger.Info("record_streams updated after recovery",
			"filePath", filePath,
			"recordId", existing.ID,
			"duration", duration,
			"storageType", header.StorageType)
		return nil
	}

	// 走到这里说明按 file_path + start_time 没有找到既有行。若本次的绝对
	// file_path 只是以「MediaPath 尾部」形态通过绑定（recoveryBindingMediaSuffix：
	// MediaPath = <basePath> + FilePath），则不允许新建行，只 Warn。
	//
	// 为什么 DB 写入要额外收窄：
	//   - 后缀关系在 sidecar 不可信时可以**被攻击者自造**——他同时控制
	//     MediaPath 与 FilePath：先投放媒体 <恢复目录>/etc/shadow，再写
	//     FilePath=/etc/shadow，后缀即成立。
	//   - 而绝对 file_path 在下载侧是直接打开的：api.go:151-158 的
	//     http.ServeFile / os.Open，GetFullPath 只用于相对路径分支（api.go:186）。
	//     所以"新建一行 file_path=/etc/shadow"= 任意文件读取。
	//   - 该形态只出现在「local + 绝对 record.filepath + 非空 basePath」的既有
	//     坏配置上（下载本来就打不开），因此只更新既有行是正确取舍：
	//     既有行由录制进程自己写入，其 file_path 必然是真实的逻辑路径。
	if isSuffixBoundAbsoluteFilePath(header) {
		logger.Warn("recovered record filePath only matched as a suffix of mediaPath and no existing row was found; skip creating record_streams row",
			"filePath", filePath,
			"mediaPath", header.MediaPath,
			"streamPath", header.StreamPath,
			"storageType", header.StorageType)
		return nil
	}

	record := m7s.RecordStream{
		StartTime:    startTime,
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
