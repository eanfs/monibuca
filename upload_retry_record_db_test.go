package m7s

import (
	"testing"
	"time"

	"m7s.live/v5/pkg/storage"
)

// C2：对象存储故障（GetStorage() 为 nil）但 DB 可用时，调度器必须仍然处理
// record-db 补写任务——这正是 #39 的现场前提（上传通道坏了，但索引必须补上）。
// 变异验证：把 Tick 恢复成「storage 为 nil 就整体 return」，本用例必须红。
func TestUploadRetrySchedulerProcessesRecordDBTaskWithoutStorage(t *testing.T) {
	db := newDeferredRetryDB(t, true)
	end := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	stream := RecordStream{
		ID:         11,
		StartTime:  end.Add(-time.Minute),
		EndTime:    end,
		Duration:   60000,
		FileName:   "rec.mp4",
		Type:       "mp4",
		FilePath:   "live/test/rec.mp4",
		StreamPath: "live/test",
	}
	ut := seedRecordDBRetryTask(t, db, stream, nil, 0, 10)

	// 一条普通上传任务：storage 不可用时本轮应被延后，而不是永久占据
	// QueryPendingUploads 的头部配额（那会把 record-db 任务饿死）。
	normal := UploadTask{
		Status:      UploadStatusFailed,
		LocalPath:   "/tmp/never-touched.mp4",
		ObjectKey:   "live/test/never.mp4",
		StorageType: "s3",
		StreamPath:  "live/test",
		MaxRetries:  10,
		NextRetryAt: time.Now().Add(-2 * time.Minute),
	}
	if err := db.Create(&normal).Error; err != nil {
		t.Fatalf("seed normal upload task: %v", err)
	}

	// storageRuntime 为 nil ⇒ GetStorage() 返回 nil，正是存储不可用的形态。
	s := &Server{}
	s.DB = db
	scheduler := &UploadRetryScheduler{s: s}
	scheduler.Tick(nil)

	deadline := time.Now().Add(5 * time.Second)
	for {
		var after UploadTask
		if err := db.First(&after, ut.ID).Error; err != nil {
			t.Fatalf("reload record-db task: %v", err)
		}
		if after.Status == UploadStatusSuccess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("record-db task was not processed while storage is unavailable: status=%d", after.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var finalized RecordStream
	if err := db.First(&finalized, "id = ?", stream.ID).Error; err != nil {
		t.Fatalf("record_streams row was not finalized: %v", err)
	}
	if !finalized.EndTime.Equal(end) || finalized.Duration != 60000 {
		t.Fatalf("record_streams not finalized: end=%v duration=%d", finalized.EndTime, finalized.Duration)
	}

	var normalAfter UploadTask
	if err := db.First(&normalAfter, normal.ID).Error; err != nil {
		t.Fatalf("reload normal task: %v", err)
	}
	if normalAfter.Status != UploadStatusFailed {
		t.Fatalf("normal upload without storage must stay failed, got status=%d", normalAfter.Status)
	}
	if !normalAfter.NextRetryAt.After(time.Now()) {
		t.Fatal("skipped normal upload must be deferred, otherwise it starves record-db retries")
	}
}

// C3：延迟入库的 DB 写超时必须真的被用上，而不只是 getter 能读到配置。
// 把超时压到 1ns 后 Save 必然超时；变异验证：把 recoder.go 两处改回硬编码
// 10s，本用例会因为 Save 成功而没有重试行 → 红。
func TestDeferredRecordWriteHonorsConfiguredDBTimeout(t *testing.T) {
	prev := storage.RecordDBWriteTimeout()
	t.Cleanup(func() {
		storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), RecordDBWriteTimeout: prev})
	})
	storage.InitUploadManager(storage.UploadConfig{
		PendingDir:           t.TempDir(),
		RecordDBWriteTimeout: time.Nanosecond,
	})

	db := newDeferredRetryDB(t, true)
	rec := newDeferredRetryRecorder(db)
	end := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	rec.Event.RecordStream = RecordStream{
		ID:         5,
		StartTime:  end.Add(-time.Minute),
		Duration:   60000,
		FileName:   "rec.mp4",
		Type:       "mp4",
		FilePath:   "live/test/slow.mp4",
		StreamPath: "live/test",
	}

	write := rec.WriteTailDeferred(end)
	if write == nil {
		t.Fatal("WriteTailDeferred must return a closure when a DB is configured")
	}
	if err := write(nil); err == nil {
		t.Fatal("an already-expired context must fail the record_streams save")
	}

	var tasks []UploadTask
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatalf("load upload tasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("timed-out deferred write must persist exactly one retry row, got %d", len(tasks))
	}
	if !isRecordDBRetryTask(tasks[0]) {
		t.Fatalf("retry row is not marked as record-db: %s", tasks[0].MetadataJSON)
	}
}
