package plugin_mp4

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/storage"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// closedRecoveryTestDB 返回连接已关闭的 DB：登记必然失败。
func closedRecoveryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormlite.Open(filepath.Join(t.TempDir(), "closed.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	return db
}

// 登记失败时媒体必须回到原路径：否则它躺在 pending 目录里既没有 DB 行、
// 补传调度找不到，又因为文件名是 os.CreateTemp 的随机名而无法被
// locateRecoveryMedia 按 <pending>/<base(recorded)> 命中——数据虽在，
// 却永远不会被自动补传，sidecar 每轮以 "media file not found" 失败。
func TestRecoveryLocatesMediaAfterFailedPendingRegistration(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_rollback.tmp", []byte{0xDE, 0xAD, 0xBE, 0xEF})
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("stat media: %v", err)
	}

	registeredPath, regErr := m7s.RecoverFailedUpload(nil, closedRecoveryTestDB(t), mediaPath,
		"live/test/rec.mp4", "s3", "live/test", info.Size(), 0, nil, errors.New("upload failed"))
	if regErr == nil {
		t.Fatal("registration against a closed database must fail")
	}
	if registeredPath != "" {
		t.Fatalf("registeredPath = %q, want empty for an unconfirmed registration", registeredPath)
	}

	// 回滚后媒体在原路径，恢复扫描（sidecar → locateRecoveryMedia）仍能找回它。
	guard, err := newRecoveryPathGuard(nil, sidecar)
	if err != nil {
		t.Fatalf("newRecoveryPathGuard: %v", err)
	}
	found, ok, locateErr := locateRecoveryMedia(mediaPath, sidecar, guard)
	if locateErr != nil || !ok || found != canonPath(t, mediaPath) {
		t.Fatalf("locateRecoveryMedia = (%q, %v, %v), want the media restored at %q",
			found, ok, locateErr, mediaPath)
	}
	entries, readErr := os.ReadDir(pendingDir)
	if readErr != nil {
		t.Fatalf("read pending dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("pending dir must not keep unregistered media, found %d entries", len(entries))
	}
	// sidecar 必须保留，它是下次重新登记的唯一线索。
	if _, statErr := os.Stat(sidecar); statErr != nil {
		t.Fatalf("sidecar must be kept for the next recovery run: %v", statErr)
	}
	if _, statErr := os.Stat(mediaPath + mp4pkg.IndexExtension); statErr != nil {
		t.Fatalf("index sidecar missing: %v", statErr)
	}
}
