package mp4

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	"m7s.live/v5"
)

// newRecoveryDBTestDB 建一个文件 sqlite 并迁移 record_streams。
func newRecoveryDBTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormlite.Open(filepath.Join(t.TempDir(), "records.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err = db.AutoMigrate(&m7s.RecordStream{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// simulateMySQLNoopUpdate 让 UPDATE 的 RowsAffected 恒为 0，模拟 MySQL 未开
// clientFoundRows 时“值完全相同”的 no-op 更新。旧实现据此误判行不存在并
// 退化到 Create，从而产生重复行；新实现必须忽略 RowsAffected。
func simulateMySQLNoopUpdate(db *gorm.DB) {
	db.Callback().Update().After("gorm:update").Register("test:zero_rows_affected", func(tx *gorm.DB) {
		tx.RowsAffected = 0
	})
}

// simulateMySQLMillisecondDateTime 模拟 MySQL DATETIME(3) 列语义：写入时把
// 时间截断到毫秒（查询参数保持调用方传入的原始精度）。SQLite 本身保留纳秒，
// 若不模拟，旧实现“写入纳秒 + 查询纳秒”在测试里天然相等，无法暴露问题。
func simulateMySQLMillisecondDateTime(db *gorm.DB) {
	truncate := func(t time.Time) time.Time {
		if t.IsZero() {
			return t
		}
		return t.Truncate(time.Millisecond)
	}
	truncateRecord := func(record *m7s.RecordStream) {
		record.StartTime = truncate(record.StartTime)
		record.EndTime = truncate(record.EndTime)
	}
	db.Callback().Create().Before("gorm:create").Register("test:ms_datetime_create", func(tx *gorm.DB) {
		switch dest := tx.Statement.Dest.(type) {
		case *m7s.RecordStream:
			truncateRecord(dest)
		case *[]m7s.RecordStream:
			for i := range *dest {
				truncateRecord(&(*dest)[i])
			}
		}
	})
	db.Callback().Update().Before("gorm:update").Register("test:ms_datetime_update", func(tx *gorm.DB) {
		updates, ok := tx.Statement.Dest.(map[string]any)
		if !ok {
			return
		}
		if endTime, ok := updates["end_time"].(time.Time); ok {
			updates["end_time"] = truncate(endTime)
		}
	})
}

func recoveryDBTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Finding 2 回归（MySQL DATETIME(3)）：header 的 StartTime 带纳秒，而列只存
// 毫秒。同一产物连续恢复两次，必须只有 1 行——实现需在入口把时间归一到毫秒，
// 让写入值与查询谓词同精度。
func TestUpdateRecoveredRecordMySQLMillisecondPrecisionDoesNotDuplicate(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	simulateMySQLMillisecondDateTime(db)
	logger := recoveryDBTestLogger()

	filePath := "record/live/db_ms_precision.mp4"
	// 故意保留纳秒（JSON sidecar 按纳秒保存，模拟真实 header）。
	start := time.Now().Add(-time.Hour).Truncate(time.Millisecond).Add(456789 * time.Nanosecond)
	header := IndexHeader{
		StreamPath:  "live/db_ms_precision",
		FilePath:    filePath,
		StorageType: "s3",
		StartTime:   start,
	}
	records := []IndexRecord{
		{TrackID: 1, Size: 10, DTS: 0, Duration: 40},
		{TrackID: 1, Size: 10, DTS: 40, Duration: 40},
	}

	for i := 0; i < 2; i++ {
		if err := updateRecoveredRecord(db, logger, header, records); err != nil {
			t.Fatalf("updateRecoveredRecord #%d: %v", i+1, err)
		}
	}

	var rows []m7s.RecordStream
	if err := db.Where("file_path = ?", filePath).Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("record_streams rows = %d, want 1 (nanosecond header must not duplicate on DATETIME(3))", len(rows))
	}
	if rows[0].Duration != 80 || rows[0].EndTime.IsZero() {
		t.Fatalf("recovered record fields = %+v", rows[0])
	}
	if rows[0].StartTime.Nanosecond()%int(time.Millisecond) != 0 {
		t.Fatalf("start_time = %v, want millisecond precision", rows[0].StartTime)
	}
}

// 历史行（旧版本未归一化写入，MySQL 四舍五入后比截断值大 1ms）且已收尾、
// duration 与本次恢复不同——只有历史行 ±1ms 区间兜底能认领它，且重跑幂等。
func TestUpdateRecoveredRecordHistoricalRoundedStartTimeIsClaimed(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	simulateMySQLMillisecondDateTime(db)
	logger := recoveryDBTestLogger()

	filePath := "record/live/db_legacy_rounded.mp4"
	start := time.Now().Add(-3 * time.Hour).Truncate(time.Millisecond).Add(654321 * time.Nanosecond)
	seedStart := start.Truncate(time.Millisecond).Add(time.Millisecond)
	if err := db.Create(&m7s.RecordStream{
		StartTime:   seedStart,
		EndTime:     seedStart.Add(40 * time.Millisecond),
		Duration:    40,
		FilePath:    filePath,
		StreamPath:  "live/db_legacy_rounded",
		Type:        "mp4",
		StorageType: "local",
	}).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}

	header := IndexHeader{
		StreamPath:  "live/db_legacy_rounded",
		FilePath:    filePath,
		StorageType: "s3",
		StartTime:   start,
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 80}}

	for i := 0; i < 2; i++ {
		if err := updateRecoveredRecord(db, logger, header, records); err != nil {
			t.Fatalf("updateRecoveredRecord #%d: %v", i+1, err)
		}
	}

	var rows []m7s.RecordStream
	if err := db.Where("file_path = ?", filePath).Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("record_streams rows = %d, want 1 (historical rounded row must be claimed)", len(rows))
	}
	if rows[0].Duration != 80 {
		t.Fatalf("duration = %d, want 80", rows[0].Duration)
	}
	if rows[0].StorageType != "s3" {
		t.Fatalf("storage_type = %q, want s3", rows[0].StorageType)
	}
}

// Finding 2 回归：同一产物连续回写两次，第二次即便 UPDATE 是 no-op
// （MySQL RowsAffected=0）也不得新建第二行。
func TestUpdateRecoveredRecordNoopUpdateDoesNotDuplicate(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	simulateMySQLNoopUpdate(db)

	filePath := "record/live/db_noop.mp4"
	start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	header := IndexHeader{
		StreamPath:  "live/db_noop",
		FilePath:    filePath,
		StorageType: "s3",
		StartTime:   start,
	}
	records := []IndexRecord{
		{TrackID: 1, Size: 10, DTS: 0, Duration: 40},
		{TrackID: 1, Size: 10, DTS: 40, Duration: 40},
	}
	logger := recoveryDBTestLogger()

	if err := updateRecoveredRecord(db, logger, header, records); err != nil {
		t.Fatalf("first updateRecoveredRecord: %v", err)
	}
	var first m7s.RecordStream
	if err := db.Where("file_path = ?", filePath).First(&first).Error; err != nil {
		t.Fatalf("query after first call: %v", err)
	}
	if first.Duration != 80 || first.EndTime.IsZero() {
		t.Fatalf("first call did not write recovery fields: %+v", first)
	}

	// 第二次：SELECT 命中同一行，UPDATE 为 no-op（RowsAffected 被强制为 0）。
	if err := updateRecoveredRecord(db, logger, header, records); err != nil {
		t.Fatalf("second updateRecoveredRecord: %v", err)
	}
	var count int64
	if err := db.Model(&m7s.RecordStream{}).Where("file_path = ?", filePath).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("record_streams rows = %d, want 1 (no-op UPDATE must not Create)", count)
	}
}

// 历史 sidecar 的 start_time 与 DB 行不一致时走“未收尾行”回退分支；
// 该分支重跑同样不得产生重复行（MySQL no-op UPDATE 场景）。
func TestUpdateRecoveredRecordHistoricalFallbackDoesNotDuplicate(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	simulateMySQLNoopUpdate(db)

	logger := recoveryDBTestLogger()
	filePath := "record/live/db_legacy.mp4"
	seedStart := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	if err := db.Create(&m7s.RecordStream{
		StartTime:   seedStart,
		FilePath:    filePath,
		StreamPath:  "live/db_legacy",
		Type:        "mp4",
		StorageType: "local",
	}).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}

	// header.StartTime 与 DB 行不同：精确查询落空，必须回退到未收尾行。
	header := IndexHeader{
		StreamPath:  "live/db_legacy",
		FilePath:    filePath,
		StorageType: "s3",
		StartTime:   seedStart.Add(30 * time.Second),
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	for i := 0; i < 2; i++ {
		if err := updateRecoveredRecord(db, logger, header, records); err != nil {
			t.Fatalf("updateRecoveredRecord #%d: %v", i+1, err)
		}
	}

	var rows []m7s.RecordStream
	if err := db.Where("file_path = ?", filePath).Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("record_streams rows = %d, want 1 (fallback must not Create)", len(rows))
	}
	if rows[0].Duration != 40 {
		t.Fatalf("duration = %d, want 40", rows[0].Duration)
	}
	if rows[0].StorageType != "s3" {
		t.Fatalf("storage_type = %q, want s3", rows[0].StorageType)
	}
}

// 两次查询都为空时才允许新建。
func TestUpdateRecoveredRecordCreatesWhenMissing(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	header := IndexHeader{
		StreamPath:  "live/db_new",
		FilePath:    "record/live/db_new.mp4",
		StorageType: "local",
		StartTime:   start,
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 25}}

	if err := updateRecoveredRecord(db, logger, header, records); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var got m7s.RecordStream
	if err := db.Where("file_path = ?", header.FilePath).First(&got).Error; err != nil {
		t.Fatalf("query created record: %v", err)
	}
	if got.ID == 0 || got.Duration != 25 {
		t.Fatalf("created record = %+v", got)
	}
}

// sidecar 的 filePath 指向媒体目录之外时不得写进 record_streams：
// DB 回写与对象键一样把 sidecar 内容视为不可信输入。
func TestUpdateRecoveredRecordSkipsFilePathOutsideMediaDir(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	header := IndexHeader{
		StreamPath:  "live/shadow",
		MediaPath:   filepath.Join(t.TempDir(), "s3writer_test.tmp"),
		FilePath:    "/etc/shadow",
		StorageType: "s3",
		StartTime:   time.Now().Add(-time.Hour).Truncate(time.Millisecond),
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	if err := updateRecoveredRecord(db, logger, header, records); err != nil {
		t.Fatalf("updateRecoveredRecord must not fail the upload path: %v", err)
	}
	var count int64
	if err := db.Model(&m7s.RecordStream{}).Count(&count).Error; err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 0 {
		var rows []m7s.RecordStream
		_ = db.Find(&rows).Error
		t.Fatalf("record_streams rows = %d (%+v), want 0", count, rows)
	}
}

// FilePath 与 MediaPath 同目录（本地录制的正常形态）仍然正常回写。
func TestUpdateRecoveredRecordAcceptsFilePathWithinMediaDir(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	dir := t.TempDir()
	header := IndexHeader{
		StreamPath:  "live/ok",
		MediaPath:   filepath.Join(dir, "rec.mp4"),
		FilePath:    filepath.Join(dir, "rec.mp4"),
		StorageType: "local",
		StartTime:   time.Now().Add(-time.Hour).Truncate(time.Millisecond),
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	if err := updateRecoveredRecord(db, logger, header, records); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var got m7s.RecordStream
	if err := db.Where("file_path = ?", header.FilePath).First(&got).Error; err != nil {
		t.Fatalf("query record: %v", err)
	}
	if got.Duration != 40 {
		t.Fatalf("record = %+v, want duration 40", got)
	}
}

// FilePath 为空时 StorageKey 回落同样不可信：绝对路径不得写进 record_streams。
func TestUpdateRecoveredRecordSkipsUnsafeStorageKey(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	header := IndexHeader{
		StreamPath:  "live/unsafe_key",
		MediaPath:   filepath.Join(t.TempDir(), "s3writer_test.tmp"),
		StorageKey:  "/etc/shadow",
		StorageType: "s3",
		StartTime:   time.Now().Add(-time.Hour).Truncate(time.Millisecond),
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	if err := updateRecoveredRecord(db, logger, header, records); err != nil {
		t.Fatalf("updateRecoveredRecord must not fail the upload path: %v", err)
	}
	var count int64
	if err := db.Model(&m7s.RecordStream{}).Count(&count).Error; err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 0 {
		t.Fatalf("record_streams rows = %d, want 0", count)
	}
}

// 本地存储的 StorageKey 就是媒体绝对路径：与 MediaPath 一致时正常回写。
func TestUpdateRecoveredRecordAcceptsLocalStorageKey(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	mediaPath := filepath.Join(t.TempDir(), "rec.mp4")
	header := IndexHeader{
		StreamPath:  "live/local_key",
		MediaPath:   mediaPath,
		StorageKey:  mediaPath,
		StorageType: "local",
		StartTime:   time.Now().Add(-time.Hour).Truncate(time.Millisecond),
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	if err := updateRecoveredRecord(db, logger, header, records); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var got m7s.RecordStream
	if err := db.Where("file_path = ?", mediaPath).First(&got).Error; err != nil {
		t.Fatalf("query record: %v", err)
	}
	if got.Duration != 40 {
		t.Fatalf("record = %+v, want duration 40", got)
	}
}
