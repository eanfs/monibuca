package m7s

import (
	"encoding/json"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"

	"m7s.live/v5/pkg/config"
)

// Issue #39 回归：延迟入库失败不能只告警后丢弃。
//
// 磁盘高负载时 SQLite 的 INSERT 可能超过延迟入库的 context 期限；此时录像文件
// 已经上传成功，但 record_streams 永远停在 end_time=NULL/duration=0，列表与
// 时间范围查询永远查不到它（孤儿文件）。修复要求把定稿数据持久化进可重试
// 队列，由补传调度器在下一个 tick 补写，进程重启也能继续。
//
// 这些测试刻意使用 upload_tasks 的 "kind=record-db" 元数据契约：该行不涉及
// 任何上传，只承载 record_streams 的定稿 payload。

// newDeferredRetryDB 造一个只迁移了相关表的 in-memory DB。
// withRecordStream=false 时故意不建 record_streams 表，让延迟入库的 Save
// 必定失败，模拟真实的高 IO / context 超时失败路径。
func newDeferredRetryDB(t *testing.T, withRecordStream bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1) // :memory: 每连接独立库，限单连接以共享同一库
	models := []any{&UploadTask{}, &AlarmInfo{}}
	if withRecordStream {
		models = append(models, &RecordStream{})
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

func newDeferredRetryRecorder(db *gorm.DB) *DefaultRecorder {
	rec := &DefaultRecorder{}
	rec.RecordJob.Plugin = &Plugin{DB: db}
	rec.RecordJob.RecConf = &config.Record{Type: "mp4"}
	return rec
}

// seedRecordDBRetryTask 按持久化契约造一条「仅待写库」的 upload_tasks 行。
// key 使用字面量而非实现里的常量：测试要钉住落库格式本身。
func seedRecordDBRetryTask(t *testing.T, db *gorm.DB, stream RecordStream, event *config.RecordEvent,
	retryCount, maxRetries int) UploadTask {
	t.Helper()
	streamJSON, err := json.Marshal(stream)
	if err != nil {
		t.Fatalf("marshal stream: %v", err)
	}
	meta := map[string]string{
		"kind":          "record-db",
		"record-stream": string(streamJSON),
	}
	if event != nil {
		eventJSON, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		meta["record-event"] = string(eventJSON)
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	ut := UploadTask{
		Status:       UploadStatusFailed,
		RetryCount:   retryCount,
		MaxRetries:   maxRetries,
		StreamPath:   stream.StreamPath,
		ErrorMessage: "context deadline exceeded",
		MetadataJSON: string(metaJSON),
		NextRetryAt:  time.Now().Add(-time.Minute),
	}
	if err := db.Create(&ut).Error; err != nil {
		t.Fatalf("seed upload task: %v", err)
	}
	return ut
}

// 延迟入库失败时，WriteTailDeferred 返回的闭包必须把定稿数据持久化到可重试
// 队列，而不是仅返回错误交给调用方告警后丢弃。
func TestDeferredRecordWriteFailurePersistsRetryRow(t *testing.T) {
	db := newDeferredRetryDB(t, false) // 无 record_streams 表 → Save 必失败
	rec := newDeferredRetryRecorder(db)
	end := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	rec.Event.RecordStream = RecordStream{
		ID:          42,
		StartTime:   end.Add(-time.Minute),
		Duration:    60000,
		FileName:    "rec.mp4",
		Type:        "mp4",
		FilePath:    "live/test/rec.mp4",
		StreamPath:  "live/test",
		StorageType: "s3",
	}

	write := rec.WriteTailDeferred(end)
	if write == nil {
		t.Fatal("WriteTailDeferred must return a closure when a DB is configured")
	}
	if err := write(nil); err == nil {
		t.Fatal("expected the record_streams save to fail without its table")
	}

	var tasks []UploadTask
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatalf("load upload tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("deferred record DB failure must persist exactly one retry row, got %d", len(tasks))
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(tasks[0].MetadataJSON), &meta); err != nil {
		t.Fatalf("decode retry metadata: %v", err)
	}
	if meta["kind"] != "record-db" {
		t.Fatalf("retry row kind = %q, want record-db", meta["kind"])
	}
	var persisted RecordStream
	if err := json.Unmarshal([]byte(meta["record-stream"]), &persisted); err != nil {
		t.Fatalf("decode persisted record_streams: %v", err)
	}
	if persisted.ID != 42 || !persisted.EndTime.Equal(end) || persisted.Duration != 60000 {
		t.Fatalf("persisted record_streams mismatch: %+v", persisted)
	}
	if tasks[0].StreamPath != "live/test" {
		t.Fatalf("retry row streamPath = %q, want live/test", tasks[0].StreamPath)
	}
}

// 补传调度器必须识别「仅待写库」的行并补写 record_streams，成功后把该行
// 标记为 Success。该路径不依赖任何 storage（没有文件要上传）。
func TestUploadRetrySchedulerFinalizesPersistedRecordDBRetry(t *testing.T) {
	db := newDeferredRetryDB(t, true)
	end := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	stream := RecordStream{
		ID:         7,
		StartTime:  end.Add(-time.Minute),
		EndTime:    end,
		Duration:   60000,
		FileName:   "rec.mp4",
		Type:       "mp4",
		FilePath:   "live/test/rec.mp4",
		StreamPath: "live/test",
	}
	ut := seedRecordDBRetryTask(t, db, stream, nil, 0, 10)

	s := &Server{}
	s.DB = db
	scheduler := &UploadRetryScheduler{s: s}
	scheduler.retryUpload(ut)

	var got RecordStream
	if err := db.First(&got, "id = ?", stream.ID).Error; err != nil {
		t.Fatalf("record_streams row was not finalized by the retry scheduler: %v", err)
	}
	if !got.EndTime.Equal(end) || got.Duration != 60000 {
		t.Fatalf("record_streams not finalized: end=%v duration=%d", got.EndTime, got.Duration)
	}
	var after UploadTask
	if err := db.First(&after, ut.ID).Error; err != nil {
		t.Fatalf("reload upload task: %v", err)
	}
	if after.Status != UploadStatusSuccess {
		t.Fatalf("retry row status = %d, want %d (success)", after.Status, UploadStatusSuccess)
	}
}

// 重试耗尽必须留下与既有语义一致的告警（AlarmName=record db save failed），
// 且该行不再被 QueryPendingUploads 命中。
func TestUploadRetrySchedulerAlarmsWhenRecordDBRetryExhausted(t *testing.T) {
	db := newDeferredRetryDB(t, true)
	// 删掉 record_streams，让补写持续失败。
	if err := db.Migrator().DropTable(&RecordStream{}); err != nil {
		t.Fatalf("drop record_streams: %v", err)
	}
	end := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	stream := RecordStream{ID: 9, EndTime: end, Duration: 1000, FilePath: "live/test/rec.mp4", StreamPath: "live/test"}
	ut := seedRecordDBRetryTask(t, db, stream, nil, 9, 10)

	s := &Server{}
	s.DB = db
	scheduler := &UploadRetryScheduler{s: s}
	scheduler.retryUpload(ut)

	var alarms []AlarmInfo
	if err := db.Find(&alarms).Error; err != nil {
		t.Fatalf("load alarms: %v", err)
	}
	if len(alarms) != 1 {
		t.Fatalf("exhausted record DB retry must raise exactly one alarm, got %d", len(alarms))
	}
	if alarms[0].AlarmName != "record db save failed" {
		t.Fatalf("alarm name = %q, want record db save failed", alarms[0].AlarmName)
	}
	if alarms[0].StreamPath != "live/test" {
		t.Fatalf("alarm streamPath = %q, want live/test", alarms[0].StreamPath)
	}
	var pending []UploadTask
	if err := db.Where("status = ? AND retry_count < max_retries", UploadStatusFailed).Find(&pending).Error; err != nil {
		t.Fatalf("query pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("exhausted retry row still pending: %+v", pending)
	}
}
