package m7s

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// 上传失败兜底:文件应移入 pending 目录且登记 upload_tasks 记录。
func TestRecoverFailedUpload(t *testing.T) {
	db := newUploadTestDB(t)
	pending := filepath.Join(t.TempDir(), "pending")
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pending})

	src := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(src, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}

	pendingPath, err := RecoverFailedUpload(nil, db, src, "recordings/live/a/a.mp4", "s3",
		"live/a", 4, 1234, map[string]string{"k": "v"}, errors.New("boom"))
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if pendingPath == "" {
		t.Fatal("expected pending path")
	}
	if _, statErr := os.Stat(pendingPath); statErr != nil {
		t.Fatalf("pending file missing: %v", statErr)
	}
	if _, statErr := os.Stat(src); !os.IsNotExist(statErr) {
		t.Fatalf("src should be moved away, stat err = %v", statErr)
	}
	var ut UploadTask
	if dbErr := db.First(&ut).Error; dbErr != nil {
		t.Fatalf("upload task not saved: %v", dbErr)
	}
	if ut.LocalPath != pendingPath || ut.ObjectKey != "recordings/live/a/a.mp4" ||
		ut.StorageType != "s3" || ut.Status != UploadStatusFailed || ut.DurationMs != 1234 {
		t.Fatalf("unexpected upload task: %+v", ut)
	}
}

// local 存储:文件已在最终路径,不应移动、不应登记。
func TestRecoverFailedUploadLocalSkip(t *testing.T) {
	db := newUploadTestDB(t)
	storage.InitUploadManager(storage.UploadConfig{PendingDir: filepath.Join(t.TempDir(), "pending")})

	src := filepath.Join(t.TempDir(), "b.mp4")
	if err := os.WriteFile(src, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	pendingPath, err := RecoverFailedUpload(nil, db, src, "b.mp4", "local", "live/b", 4, 0, nil, errors.New("boom"))
	if err != nil || pendingPath != "" {
		t.Fatalf("local should skip, got path=%q err=%v", pendingPath, err)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("local file should stay in place: %v", statErr)
	}
	var cnt int64
	db.Model(&UploadTask{}).Count(&cnt)
	if cnt != 0 {
		t.Fatalf("no upload task should be saved for local, got %d", cnt)
	}
}

// 无 DB:没有补传队列,不移动。
func TestRecoverFailedUploadNilDB(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: filepath.Join(t.TempDir(), "pending")})
	src := filepath.Join(t.TempDir(), "c.mp4")
	if err := os.WriteFile(src, []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	pendingPath, err := RecoverFailedUpload(nil, nil, src, "c.mp4", "s3", "live/c", 4, 0, nil, errors.New("boom"))
	if err != nil || pendingPath != "" {
		t.Fatalf("nil db should skip, got path=%q err=%v", pendingPath, err)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("file should stay in place: %v", statErr)
	}
}
