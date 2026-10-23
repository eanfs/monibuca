package mp4

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

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
// 媒体是唯一副本，不得被删除（留在原地或已移入 pending 均可）。
func TestIndexKeptWhenPendingRegistrationFails(t *testing.T) {
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	mediaPath, indexPath := newPendingTestSidecar(t, t.TempDir())
	task := newPendingTestTrailerTask(indexPath, closedPendingTestDB(t))
	logs := &captureHandler{}
	task.Logger = slog.New(logs)

	if registered := task.recoverLocalFile(mediaPath, 5, errors.New("upload failed")); registered {
		t.Fatal("recoverLocalFile must not report registration when the upload_tasks write fails")
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("sidecar must be kept when pending registration fails: %v", err)
	}
	inPlace := pathExists(mediaPath)
	inPending := pathExists(filepath.Join(pendingDir, filepath.Base(mediaPath)))
	if !inPlace && !inPending {
		t.Fatal("the only copy of the media was deleted")
	}
	logs.mustFind(t, "failed upload registration failed; media kept for retry", slog.LevelError)
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
	// newPendingTestDB 只迁移 upload_tasks/alarms：record_streams 查询必然失败。
	db := newPendingTestDB(t)
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
