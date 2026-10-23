package mp4

import (
	"io"
	"log/slog"
	"os"
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
		if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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
		if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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
	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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
		if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

// 仅以「MediaPath 尾部」形态成立（recoveryBindingMediaSuffix）且**没有既有行**时，
// updateRecoveredRecord 必须返回成功（不阻塞上传）但不 Create，只 Warn：
// 可写恢复目录的进程可以用「媒体文件 <dir>/etc/shadow + FilePath=/etc/shadow」
// 自造后缀关系，而下载接口对绝对 file_path 直接 http.ServeFile/os.Open
// （api.go:151-158），新建该行即任意文件读取。
func TestUpdateRecoveredRecordSuffixOnlyWithoutExistingRowSkipsCreate(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	dir := t.TempDir()
	header := IndexHeader{
		StreamPath:  "live/poison",
		StorageType: "local",
		MediaPath:   filepath.Join(dir, "etc", "shadow"),
		FilePath:    "/etc/shadow",
		StartTime:   time.Now().Add(-time.Hour).Truncate(time.Millisecond),
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	// 返回成功：不阻塞上传（产物仍可 upload，只是不留下不可信的元数据行）。
	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

// 正常录制在开录时已用同一个绝对 file_path 写入半成品行：后缀形态必须更新该行。
func TestUpdateRecoveredRecordSuffixFormUpdatesExistingRow(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	dir := t.TempDir()
	filePath := "/data/records/rec.mp4"
	start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	if err := db.Create(&m7s.RecordStream{
		StartTime:   start,
		FilePath:    filePath,
		StreamPath:  "live/rec",
		Type:        "mp4",
		StorageType: "local",
	}).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}
	header := IndexHeader{
		StreamPath:  "live/rec",
		StorageType: "local",
		MediaPath:   filepath.Join(dir, "data", "records", "rec.mp4"),
		FilePath:    filePath,
		StartTime:   start,
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var rows []m7s.RecordStream
	if err := db.Where("file_path = ?", filePath).Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("record_streams rows = %d, want 1", len(rows))
	}
	if rows[0].Duration != 40 || rows[0].EndTime.IsZero() {
		t.Fatalf("row not updated: %+v", rows[0])
	}
}

// FilePath 经符号链接解析后落在白名单外时不得写库（否则下载/删除侧跟随链接）。
func TestUpdateRecoveredRecordRejectsSymlinkEscape(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	logger := recoveryDBTestLogger()
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, "sub")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unsupported on this platform: %v", err)
	}
	header := IndexHeader{
		StreamPath:  "live/symlink",
		StorageType: "local",
		MediaPath:   filepath.Join(dir, "a.tmp"),
		FilePath:    filepath.Join(link, "shadow"),
		StartTime:   time.Now().Add(-time.Hour).Truncate(time.Millisecond),
	}
	records := []IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}

	if err := updateRecoveredRecord(db, logger, header, records, nil); err != nil {
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

// seedStartedRecordingRow 预置"开录时写入"的 record_streams 行。真实路径是
// recoder.go 在录制开始时用 ICodecCtx.String()（RFC 6381，如 avc1.4D401E）
// 保存 codec；崩溃后恢复命中的正是这一行，而不是空行。
func seedStartedRecordingRow(t *testing.T, header IndexHeader, audio, video string) *gorm.DB {
	t.Helper()
	db := newRecoveryDBTestDB(t)
	row := m7s.RecordStream{
		StartTime:    header.StartTime,
		FilePath:     header.FilePath,
		StreamPath:   header.StreamPath,
		Type:         "mp4",
		AudioCodec:   audio,
		VideoCodec:   video,
		StorageLevel: 1,
		StorageType:  header.StorageType,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("seed record_streams: %v", err)
	}
	return db
}

// recoveryDBTestHeader 造 video+audio 双轨的索引头，STSD 是真实可解析的
// avcC / alaw sample entry（恢复路径据此推导 RFC 6381 codec 字符串）。
func recoveryDBTestHeader(t *testing.T, name string) IndexHeader {
	t.Helper()
	return IndexHeader{
		StreamPath:  "live/" + name,
		FilePath:    "record/live/" + name + ".mp4",
		StorageType: "local",
		StartTime:   time.Now().Add(-time.Minute).Truncate(time.Millisecond),
		Tracks: []IndexTrack{
			{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: restoreTestSTSD(t, true)},
			{ID: 2, FourCC: "alaw", SampleRate: 8000, Channels: 1, STSD: restoreTestSTSD(t, false)},
		},
	}
}

// 恢复产物丢掉整条音轨（记录全部越界）时，必须**显式清空**既有行的
// audio_codec：开录时写入的 "alaw" 若只是"不做更新"会被原样保留，API
// (api.go:971) 仍会宣称产物有音轨。视频轨的录制值（RFC 6381）不得被降级。
func TestUpdateRecoveredRecordClearsDroppedAudioCodecOnRecordedRow(t *testing.T) {
	header := recoveryDBTestHeader(t, "drop_audio")
	db := seedStartedRecordingRow(t, header, "alaw", "avc1.4D401E")

	if err := updateRecoveredRecord(db, recoveryDBTestLogger(), header,
		[]IndexRecord{{TrackID: 1, Size: 10, DTS: 0, Duration: 40}}, []uint16{2}); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var row m7s.RecordStream
	if err := db.Where("file_path = ?", header.FilePath).First(&row).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if row.AudioCodec != "" {
		t.Fatalf("audio_codec = %q, want cleared (audio track was dropped)", row.AudioCodec)
	}
	if row.VideoCodec != "avc1.4D401E" {
		t.Fatalf("video_codec = %q, want recorded RFC 6381 value preserved", row.VideoCodec)
	}
}

// 既有行已带录制路径写入的 RFC 6381 codec 时，恢复回写绝不能把它降级成裸
// FourCC（avc1.4D401E → avc1）。
func TestUpdateRecoveredRecordDoesNotDowngradeRecordedCodecs(t *testing.T) {
	header := recoveryDBTestHeader(t, "keep_codecs")
	db := seedStartedRecordingRow(t, header, "alaw", "avc1.4D401E")

	records := []IndexRecord{
		{TrackID: 1, Size: 10, DTS: 0, Duration: 40},
		{TrackID: 2, Size: 320, DTS: 0, Duration: 40},
	}
	if err := updateRecoveredRecord(db, recoveryDBTestLogger(), header, records, nil); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var row m7s.RecordStream
	if err := db.Where("file_path = ?", header.FilePath).First(&row).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if row.AudioCodec != "alaw" || row.VideoCodec != "avc1.4D401E" {
		t.Fatalf("codecs = audio:%q video:%q, want recorded values preserved (no downgrade)",
			row.AudioCodec, row.VideoCodec)
	}
}

// 既有行列值为空时，恢复应补写与录制口径一致的 RFC 6381 字符串
// （从索引头保存的 stsd 推导），而不是裸 FourCC。
func TestUpdateRecoveredRecordFillsEmptyCodecsWithRFC6381(t *testing.T) {
	header := recoveryDBTestHeader(t, "fill_codecs")
	db := seedStartedRecordingRow(t, header, "", "")

	records := []IndexRecord{
		{TrackID: 1, Size: 10, DTS: 0, Duration: 40},
		{TrackID: 2, Size: 320, DTS: 0, Duration: 40},
	}
	if err := updateRecoveredRecord(db, recoveryDBTestLogger(), header, records, nil); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var row m7s.RecordStream
	if err := db.Where("file_path = ?", header.FilePath).First(&row).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if row.AudioCodec != "alaw" || row.VideoCodec != "avc1.64001F" {
		t.Fatalf("codecs = audio:%q video:%q, want alaw/avc1.64001F", row.AudioCodec, row.VideoCodec)
	}
}

// 记录不存在时新建行，codec 同样使用 RFC 6381 表示。
func TestUpdateRecoveredRecordWritesRecoveredCodecs(t *testing.T) {
	db := newRecoveryDBTestDB(t)
	header := recoveryDBTestHeader(t, "with_audio")
	records := []IndexRecord{
		{TrackID: 1, Size: 10, DTS: 0, Duration: 40},
		{TrackID: 2, Size: 320, DTS: 0, Duration: 40},
	}

	if err := updateRecoveredRecord(db, recoveryDBTestLogger(), header, records, nil); err != nil {
		t.Fatalf("updateRecoveredRecord: %v", err)
	}
	var row m7s.RecordStream
	if err := db.Where("file_path = ?", header.FilePath).First(&row).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if row.AudioCodec != "alaw" || row.VideoCodec != "avc1.64001F" {
		t.Fatalf("codecs = audio:%q video:%q, want alaw/avc1.64001F", row.AudioCodec, row.VideoCodec)
	}
}

// stsd 不可解析时回退到 FourCC：宁可给出粗粒度值，也不能写空/乱码。
func TestRecoveredTrackCodecTagFallsBackToFourCC(t *testing.T) {
	if got := recoveredTrackCodecTag(IndexTrack{ID: 1, FourCC: "hvc1"}); got != "hvc1" {
		t.Fatalf("tag = %q, want FourCC fallback hvc1", got)
	}
}
