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

func recoveryDBTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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
