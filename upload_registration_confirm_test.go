package m7s

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gorm.io/gorm"
	"m7s.live/v5/pkg/storage"
)

// closedUploadTestDB 返回一个连接已关闭的 DB：任何读写都会失败。
// 用来模拟真实世界的登记失败（SQLite "database is locked"、MySQL 断连、磁盘满）。
func closedUploadTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newUploadTestDB(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	return db
}

// C4：落库失败必须如实返回。历史上 db.Create 的返回值被完全丢弃，INSERT 失败
// （锁冲突/断连/磁盘满）时调用方仍以为「已登记补传」，随后删掉 sidecar，录像
// 永久丢失。
func TestSaveFailedUploadReturnsErrorWhenDBUnavailable(t *testing.T) {
	db := closedUploadTestDB(t)
	err := SaveFailedUpload(db, "/pending/a.mp4", "live/a/a.mp4", "s3", "live/a",
		4, 0, nil, errors.New("upload failed"))
	if err == nil {
		t.Fatal("SaveFailedUpload must report the DB write failure")
	}
}

// 没有数据库就没有补传队列：必须报错，而不是伪装成登记成功。
func TestSaveFailedUploadRequiresDB(t *testing.T) {
	if err := SaveFailedUpload(nil, "/pending/a.mp4", "live/a/a.mp4", "s3", "live/a",
		4, 0, nil, errors.New("upload failed")); err == nil {
		t.Fatal("SaveFailedUpload without a database must report an error")
	}
}

// 登记失败时 RecoverFailedUpload 必须返回错误（调用方据此保留 sidecar），
// 且媒体是唯一副本——无论它留在原地还是已移入 pending，都不得被删除。
func TestRecoverFailedUploadRegistrationFailureKeepsMedia(t *testing.T) {
	db := closedUploadTestDB(t)
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	src := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	pendingPath, err := RecoverFailedUpload(nil, db, src, "recordings/live/a/a.mp4", "s3",
		"live/a", 4, 0, nil, errors.New("boom"))
	if err == nil {
		t.Fatal("unconfirmed registration must surface an error")
	}
	if pendingPath == "" {
		t.Fatal("media was moved to pending; the pending path must be reported for recovery")
	}
	if _, statErr := os.Stat(pendingPath); statErr != nil {
		t.Fatalf("the only copy of the media must be kept: %v", statErr)
	}
}

// ConfirmPendingRegistration 是「登记真的落库」的唯一权威判据：
// 必须按 local_path + object_key 命中，查询失败必须返回错误而不是 false。
func TestConfirmPendingRegistration(t *testing.T) {
	db := newUploadTestDB(t)

	confirmed, err := ConfirmPendingRegistration(db, "/pending/a.mp4", "live/a/a.mp4")
	if err != nil {
		t.Fatalf("confirm on empty table: %v", err)
	}
	if confirmed {
		t.Fatal("no upload_tasks row yet; must not report confirmed")
	}

	if createErr := db.Create(&UploadTask{
		LocalPath: "/pending/a.mp4", ObjectKey: "live/a/a.mp4", StorageType: "s3",
		Status: UploadStatusFailed, MaxRetries: 10,
	}).Error; createErr != nil {
		t.Fatalf("seed upload task: %v", createErr)
	}

	confirmed, err = ConfirmPendingRegistration(db, "/pending/a.mp4", "live/a/a.mp4")
	if err != nil || !confirmed {
		t.Fatalf("registered row must be confirmed: confirmed=%v err=%v", confirmed, err)
	}
	// 同一本地路径的另一份产物（固定 fileName 的分片）不得被误认为已登记。
	if confirmed, err = ConfirmPendingRegistration(db, "/pending/a.mp4", "live/a/other.mp4"); err != nil || confirmed {
		t.Fatalf("objectKey mismatch must not confirm: confirmed=%v err=%v", confirmed, err)
	}

	closed := closedUploadTestDB(t)
	if _, err = ConfirmPendingRegistration(closed, "/pending/a.mp4", "live/a/a.mp4"); err == nil {
		t.Fatal("query failure must be reported, not folded into \"not confirmed\"")
	}
}
