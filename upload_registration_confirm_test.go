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

// 登记失败时 RecoverFailedUpload 必须返回错误（调用方据此保留 sidecar 与媒体），
// 并把媒体从 pending **退回原路径**：留在 pending 的文件既不会被补传调度找到
// （没有 DB 行），又会永久占用 pending 容量。
func TestRecoverFailedUploadRegistrationFailureRestoresMedia(t *testing.T) {
	db := closedUploadTestDB(t)
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "a.mp4")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	registeredPath, err := RecoverFailedUpload(nil, db, src, "recordings/live/a/a.mp4", "s3",
		"live/a", 4, 0, nil, errors.New("boom"))
	if err == nil {
		t.Fatal("unconfirmed registration must surface an error")
	}
	if registeredPath != "" {
		t.Fatalf("registeredPath = %q, want empty for an unconfirmed registration", registeredPath)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("the only copy of the media must be restored to its original path: %v", statErr)
	}
	entries, readErr := os.ReadDir(pendingDir)
	if readErr != nil {
		t.Fatalf("read pending dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("pending dir must not keep unregistered media, found %d entries", len(entries))
	}
}

// pending 目录不可用（已满）时不能等于放弃：补传读的是本地文件，原地登记即可。
func TestRecoverFailedUploadPendingFullRegistersInPlace(t *testing.T) {
	db := newUploadTestDB(t)
	pendingDir := t.TempDir()
	// 占满文件数上限 → MoveToPendingDir 返回 ErrPendingDirFull。
	if err := os.WriteFile(filepath.Join(pendingDir, "existing.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed pending dir: %v", err)
	}
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 1})

	src := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	registeredPath, err := RecoverFailedUpload(nil, db, src, "recordings/live/a/a.mp4", "s3",
		"live/a", 4, 0, nil, errors.New("boom"))
	if err != nil {
		t.Fatalf("in-place registration must succeed when only the pending dir is full: %v", err)
	}
	if registeredPath != src {
		t.Fatalf("registeredPath = %q, want the original path %q", registeredPath, src)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("media must stay at its original path: %v", statErr)
	}
	var tasks []UploadTask
	if err := db.Find(&tasks).Error; err != nil {
		t.Fatalf("load upload tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].LocalPath != src || tasks[0].ObjectKey != "recordings/live/a/a.mp4" {
		t.Fatalf("unexpected upload tasks: %+v", tasks)
	}
}

// pending 满 + DB 不可用：既移不走也登记不了，媒体必须原地保留。
func TestRecoverFailedUploadPendingFullAndDBDownKeepsMedia(t *testing.T) {
	db := closedUploadTestDB(t)
	pendingDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(pendingDir, "existing.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed pending dir: %v", err)
	}
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 1})

	src := filepath.Join(t.TempDir(), "a.mp4")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	registeredPath, err := RecoverFailedUpload(nil, db, src, "recordings/live/a/a.mp4", "s3",
		"live/a", 4, 0, nil, errors.New("boom"))
	if err == nil {
		t.Fatal("both move and registration failed; an error is required")
	}
	if registeredPath != "" {
		t.Fatalf("registeredPath = %q, want empty", registeredPath)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Fatalf("media must stay at its original path: %v", statErr)
	}
}

// 回滚不得覆盖原路径上已存在的文件：固定 fileName + 分片时该路径可能已被
// 新录像占用，覆盖会毁掉正在写的数据。此时保留 pending 副本并返回错误。
func TestRecoverFailedUploadRestoreRefusesToOverwrite(t *testing.T) {
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "a.mp4")
	if err := os.WriteFile(src, []byte("new recording"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 模拟「媒体已被移入 pending、登记失败」的现场：MoveToPendingDir 会看到
	// 同名文件而加唯一后缀，这里直接构造 pending 副本并在原路径放一份新数据。
	pendingCopy := filepath.Join(pendingDir, "a.mp4")
	if err := os.WriteFile(pendingCopy, []byte("old recording"), 0o644); err != nil {
		t.Fatal(err)
	}
	restoreErr := restoreMediaFromPending(pendingCopy, src)
	if restoreErr == nil {
		t.Fatal("restore must refuse to overwrite an occupied original path")
	}
	if data, readErr := os.ReadFile(src); readErr != nil || string(data) != "new recording" {
		t.Fatalf("occupied original path must be untouched: data=%q err=%v", data, readErr)
	}
	if _, statErr := os.Stat(pendingCopy); statErr != nil {
		t.Fatalf("pending copy must be kept when restore is refused: %v", statErr)
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
