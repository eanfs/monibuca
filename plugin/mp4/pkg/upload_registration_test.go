package mp4

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	gotask "github.com/eanfs/gotask"
	"gorm.io/gorm"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/storage"
)

// closedPendingTestDB 返回连接已关闭的 DB：任何读写都会失败，用来模拟
// SQLite 锁冲突 / MySQL 断连等「登记写不进去」的真实现场。
func closedPendingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newPendingTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	return db
}

// 登记写不进去时 sidecar 必须保留：它是下次启动重试登记的唯一线索。
// 媒体必须退回原路径（而不是留在 pending 里既没 DB 行又占容量）。
func TestIndexKeptWhenPendingRegistrationFails(t *testing.T) {
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	task := newPendingTestTrailerTask(indexPath, closedPendingTestDB(t))
	logs := &captureHandler{}
	task.Logger = slog.New(logs)

	if moved, _ := task.recoverLocalFile(mediaPath, 5, errors.New("upload failed")); moved {
		t.Fatal("recoverLocalFile must not report a pending move when the upload_tasks write fails")
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("sidecar must be kept when pending registration fails: %v", err)
	}
	if _, err := os.Stat(mediaPath); err != nil {
		t.Fatalf("media must be restored to its original path: %v", err)
	}
	entries, readErr := os.ReadDir(pendingDir)
	if readErr != nil {
		t.Fatalf("read pending dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("unregistered media must not be left in pending, found %d entries", len(entries))
	}
	logs.mustFind(t, "failed upload registration failed; media kept for retry", slog.LevelError)
}

// 登记未确认时必须留下告警：媒体虽然保留，但可能无法自动补传
// （pending 里没有 upload_tasks 行，恢复扫描也未必覆盖原路径）。
func TestHandleFailedUploadRaisesAlarmWhenRegistrationFails(t *testing.T) {
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})
	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	db := newPendingTestDB(t)
	// 只删 upload_tasks，保留 alarm_info：登记失败路径必须可观测。
	if err := db.Migrator().DropTable(&m7s.UploadTask{}); err != nil {
		t.Fatalf("drop upload_tasks: %v", err)
	}
	task := newPendingTestTrailerTask(indexPath, db)

	keepMedia, moved := task.handleFailedUpload(mediaPath, 5, nil, errors.New("upload failed"))
	if !keepMedia || moved {
		t.Fatalf("keepMedia=%v moved=%v, want true/false", keepMedia, moved)
	}
	if _, err := os.Stat(mediaPath); err != nil {
		t.Fatalf("media must be restored to its original path: %v", err)
	}
	var alarms []m7s.AlarmInfo
	if err := db.Find(&alarms).Error; err != nil {
		t.Fatalf("load alarms: %v", err)
	}
	if len(alarms) != 1 || alarms[0].AlarmName != "upload registration unconfirmed" {
		t.Fatalf("unexpected alarms: %+v", alarms)
	}
}

// 正常路径：媒体移入 pending，sidecar 作为孤儿被清理。
func TestHandleFailedUploadMovesMediaToPending(t *testing.T) {
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	db := newPendingTestDB(t)
	task := newPendingTestTrailerTask(indexPath, db)

	keepMedia, moved := task.handleFailedUpload(mediaPath, 5, nil, errors.New("upload failed"))
	if !keepMedia {
		t.Fatal("media must be kept after pending registration")
	}
	if !moved {
		t.Fatal("media should be reported as moved into the pending dir")
	}
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("media should have left its original path: %v", err)
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("sidecar must be removed once the media left its original path: %v", err)
	}
	var tasks []m7s.UploadTask
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatalf("load upload tasks: %v", err)
	}
	if len(tasks) != 1 || filepath.Dir(tasks[0].LocalPath) != pendingDir {
		t.Fatalf("unexpected upload tasks: %+v", tasks)
	}
}

// Critical 回归（决策层）：tempPath 是否可以被 defer 删除。
// 只有「明确没有补传队列」才允许删；pending 满 / DB 写失败时删掉的就是唯一副本。
func TestRecoverTempAfterFinalizeFailureOwnership(t *testing.T) {
	t.Run("no database means no retry queue", func(t *testing.T) {
		storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir()})
		mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
		task := newPendingTestTrailerTask(indexPath, nil)
		writes := 0
		task.dbWrite = func(gotask.IJob) error { writes++; return nil }

		if deleteTemp := task.recoverTempAfterFinalizeFailure(mediaPath, 5, nil, errors.New("upload failed")); !deleteTemp {
			t.Fatal("without a database there is no retry queue: caller may discard the temp copy")
		}
		if writes != 0 {
			t.Fatalf("dbWrite ran %d times, want 0: the media is discarded, a DB row would point at a deleted file", writes)
		}
	})

	t.Run("pending dir full keeps the temp copy", func(t *testing.T) {
		pendingDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(pendingDir, "existing.mp4"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed pending dir: %v", err)
		}
		storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 1})
		mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
		task := newPendingTestTrailerTask(indexPath, newPendingTestDB(t))
		writes := 0
		task.dbWrite = func(gotask.IJob) error { writes++; return nil }

		if deleteTemp := task.recoverTempAfterFinalizeFailure(mediaPath, 5, nil, errors.New("upload failed")); deleteTemp {
			t.Fatal("media registered in place is the only copy: caller must not delete it")
		}
		if _, err := os.Stat(mediaPath); err != nil {
			t.Fatalf("media must stay at its original path: %v", err)
		}
		if writes != 1 {
			t.Fatalf("dbWrite ran %d times, want 1: in-place registration must still finalize the record", writes)
		}
	})

	t.Run("registration failure keeps the temp copy", func(t *testing.T) {
		pendingDir := t.TempDir()
		storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})
		mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
		task := newPendingTestTrailerTask(indexPath, closedPendingTestDB(t))

		if deleteTemp := task.recoverTempAfterFinalizeFailure(mediaPath, 5, nil, errors.New("upload failed")); deleteTemp {
			t.Fatal("unconfirmed registration must never authorize deleting the only copy")
		}
		if _, err := os.Stat(mediaPath); err != nil {
			t.Fatalf("media must be restored to its original path: %v", err)
		}
	})
}

// FinalizeFromTemp 失败但媒体被保留（pending/原地登记）时同样必须补写延迟
// 入库：pending 补传成功只更新 upload_tasks，不会回写 record_streams。
func TestRecoverTempAfterFinalizeFailureWritesDeferredRecord(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir()})
	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	db := newPendingTestDB(t)
	trailer := newPendingTestTrailerTask(indexPath, db)
	writes := 0
	trailer.dbWrite = func(gotask.IJob) error { writes++; return nil }

	if deleteTemp := trailer.recoverTempAfterFinalizeFailure(mediaPath, 5, nil, errors.New("finalize failed")); deleteTemp {
		t.Fatal("registered media must be kept: caller must not delete the only copy")
	}
	if writes != 1 {
		t.Fatalf("dbWrite ran %d times, want 1", writes)
	}
}

// Critical 回归：pending 目录满（MoveToPendingDir 失败）时必须改为原地登记，
// 媒体与 sidecar 都不能被删除——过去这里会让 defer 删掉唯一副本，而日志
// 还写着 "keeping media"。
func TestHandleFailedUploadKeepsMediaWhenPendingUnavailable(t *testing.T) {
	pendingDir := t.TempDir()
	// 占满文件数上限 → MoveToPendingDir 返回 ErrPendingDirFull。
	if err := os.WriteFile(filepath.Join(pendingDir, "existing.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed pending dir: %v", err)
	}
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 1})

	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	db := newPendingTestDB(t)
	task := newPendingTestTrailerTask(indexPath, db)
	logs := &captureHandler{}
	task.Logger = slog.New(logs)

	keepMedia, moved := task.handleFailedUpload(mediaPath, 5, nil, errors.New("upload failed"))
	if !keepMedia {
		t.Fatal("the only copy of the media must be kept when the pending dir is unavailable")
	}
	if moved {
		t.Fatal("media was not moved; movedToPending must be false")
	}
	if _, err := os.Stat(mediaPath); err != nil {
		t.Fatalf("media must stay at its original path: %v", err)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("sidecar must be kept while the media stays in place: %v", err)
	}
	logs.mustFind(t, "move to pending dir failed", slog.LevelError)
	// 原地登记：补传调度按原路径接手，媒体不会成为无人认领的孤儿。
	var tasks []m7s.UploadTask
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatalf("load upload tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].LocalPath != mediaPath {
		t.Fatalf("unexpected upload tasks: %+v", tasks)
	}
}

// pending 满 + DB 不可用：两条路都失败，媒体与 sidecar 仍必须原样保留，
// 且错误必须可见。
func TestHandleFailedUploadKeepsMediaWhenPendingAndDBFail(t *testing.T) {
	pendingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pendingDir, "existing.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed pending dir: %v", err)
	}
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 1})

	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	task := newPendingTestTrailerTask(indexPath, closedPendingTestDB(t))
	logs := &captureHandler{}
	task.Logger = slog.New(logs)

	keepMedia, moved := task.handleFailedUpload(mediaPath, 5, nil, errors.New("upload failed"))
	if !keepMedia {
		t.Fatal("the only copy of the media must be kept when move and registration both fail")
	}
	if moved {
		t.Fatal("media was not moved; movedToPending must be false")
	}
	if _, err := os.Stat(mediaPath); err != nil {
		t.Fatalf("media must stay at its original path: %v", err)
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("sidecar must be kept: %v", err)
	}
	logs.mustFind(t, "failed upload registration not confirmed; keeping media and index sidecar", slog.LevelError)
}

// 恢复上传失败且 pending 目录不可用：改为原地登记，媒体与 sidecar 都保留。
// 下次扫描会因「已登记」跳过上传，只回写 DB 并清理 sidecar。
func TestEnqueueRecoveryUploadPendingFullRegistersInPlace(t *testing.T) {
	db := newPendingTestDB(t)
	pendingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pendingDir, "existing.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed pending dir: %v", err)
	}
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 1})

	fixture := newRecoveryUploadFixture(t, "s3")
	backend := &fakeRecoveryStorage{key: "s3", file: &fakeRecoveryFile{closeErr: errors.New("upload failed")}}
	if err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		DB:       db,
		Resolver: func(string) (storage.Storage, error) { return backend, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			_ = task.Go() // 上传失败只触发 onFail
			return nil
		},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	fixture.assertRetained(t)

	var tasks []m7s.UploadTask
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatalf("load upload tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].LocalPath != fixture.output {
		t.Fatalf("media should be registered in place, got %+v", tasks)
	}
}

// 判重查询失败时结果不可确定：既不重复上传，也不删除 sidecar/产物，
// 必须把错误交给调用方（恢复任务保留现场，下次再试）。
func TestEnqueueRecoveryUploadRegistrationCheckFailureKeepsSidecar(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "s3")
	queued := 0
	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		DB:       closedPendingTestDB(t),
		Resolver: func(string) (storage.Storage, error) { return &fakeRecoveryStorage{key: "s3"}, nil },
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	})
	if err == nil {
		t.Fatal("unconfirmed registration state must surface an error")
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0: dedupe state unknown must not upload", queued)
	}
	fixture.assertRetained(t)
}

// 已登记分支：record_streams 回写失败必须返回错误并保留 sidecar，
// 否则这条录像的 DB 记录再无补偿机会（sidecar 是重试回写的唯一线索）。
func TestEnqueueRecoveryUploadRegisteredBranchKeepsSidecarOnDBWriteFailure(t *testing.T) {
	db := newPendingTestDB(t)
	// 制造 record_streams 写失败：删掉表，upload_tasks 判重仍可正常命中。
	if err := db.Migrator().DropTable(&m7s.RecordStream{}); err != nil {
		t.Fatalf("drop record_streams: %v", err)
	}
	fixture := newRecoveryUploadFixture(t, "s3")
	if err := db.Create(&m7s.UploadTask{
		LocalPath:   fixture.output,
		ObjectKey:   fixture.header.FilePath,
		StorageType: "s3",
		Status:      m7s.UploadStatusFailed,
		MaxRetries:  10,
	}).Error; err != nil {
		t.Fatalf("seed upload task: %v", err)
	}

	queued := 0
	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		DB:       db,
		Resolver: func(string) (storage.Storage, error) { return &fakeRecoveryStorage{key: "s3"}, nil },
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	})
	if err == nil {
		t.Fatal("record_streams write failure must surface an error")
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0 for an already registered upload", queued)
	}
	fixture.assertRetained(t)
}

// pathExists 报告路径是否为已存在的普通文件。
func pathExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// 上传失败并登记 pending 后必须补写延迟入库：补传调度成功后只更新
// upload_tasks，不会回写 record_streams。漏写会让该录像的 end_time/duration
// 永久为零，时间范围查询静默漏掉它（2026-11-23 T7 现场实证：4 次上传失败后
// 媒体进入 pending_uploads，record_streams 仍为 end_time=NULL、duration=0）。
func TestUploadFailureRegistersPendingAndWritesDeferredRecord(t *testing.T) {
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	db := newPendingTestDB(t)
	trailer := newPendingTestTrailerTask(indexPath, db)
	dbWrites := 0
	trailer.dbWrite = func(gotask.IJob) error { dbWrites++; return nil }

	ft := newTestFinalizeTask(&fakeStorageFile{closeErr: errors.New("upload failed")})
	ft.db = db
	ft.dbWrite = trailer.dbWrite
	ft.onFail = func(cause error) bool {
		keepMedia, _ := trailer.handleFailedUpload(mediaPath, 5, nil, cause)
		return keepMedia
	}

	if err := ft.Go(); err == nil {
		t.Fatal("expected the upload error to propagate")
	}
	var tasks []m7s.UploadTask
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatalf("load upload tasks: %v", err)
	}
	if len(tasks) != 1 || filepath.Dir(tasks[0].LocalPath) != pendingDir {
		t.Fatalf("media must be registered in the pending dir, got %+v", tasks)
	}
	if dbWrites != 1 {
		t.Fatalf("dbWrite ran %d times, want 1: the pending retry path never writes record_streams", dbWrites)
	}
}
