package m7s

import (
	"sync"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
)

func newUploadTestDB(t *testing.T) *gorm.DB {
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
	if err := db.AutoMigrate(&UploadTask{}); err != nil {
		t.Fatalf("migrate UploadTask: %v", err)
	}
	return db
}

// seedUploading 造一个 Uploading 状态、UploadStartedAt 为 startedAgo 之前的任务。
func seedUploading(t *testing.T, db *gorm.DB, startedAgo time.Duration, retryCount int) UploadTask {
	t.Helper()
	ut := UploadTask{
		LocalPath:       "/tmp/x.mp4",
		ObjectKey:       "x.mp4",
		Status:          UploadStatusUploading,
		RetryCount:      retryCount,
		MaxRetries:      10,
		UploadStartedAt: time.Now().Add(-startedAgo),
	}
	if err := db.Create(&ut).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	return ut
}

// TestReclaimStaleUploading：超时 Uploading 任务被扫回 Failed 且 retry_count 不变；
// 未超时的不动。
func TestReclaimStaleUploading(t *testing.T) {
	db := newUploadTestDB(t)
	stale := seedUploading(t, db, time.Hour, 3)   // 卡死 1h，应回收
	fresh := seedUploading(t, db, time.Minute, 2) // 刚 1min，不回收

	n, err := ReclaimStaleUploading(db, 35*time.Minute)
	if err != nil {
		t.Fatalf("ReclaimStaleUploading: %v", err)
	}
	if n != 1 {
		t.Fatalf("应回收 1 个，实际 %d", n)
	}

	var gotStale UploadTask
	db.First(&gotStale, stale.ID)
	if gotStale.Status != UploadStatusFailed {
		t.Errorf("卡死任务应回收为 Failed，实际 status=%d", gotStale.Status)
	}
	if gotStale.RetryCount != 3 {
		t.Errorf("回收不应改 retry_count，期望 3 实际 %d", gotStale.RetryCount)
	}

	var gotFresh UploadTask
	db.First(&gotFresh, fresh.ID)
	if gotFresh.Status != UploadStatusUploading {
		t.Errorf("未超时任务不应被回收，实际 status=%d", gotFresh.Status)
	}
}

// TestMarkUploadingClaim：对同一 Failed 任务并发抢占，只有一个成功；
// 已是 Uploading 的任务不可再被抢占。
func TestMarkUploadingClaim(t *testing.T) {
	db := newUploadTestDB(t)
	ut := UploadTask{
		LocalPath: "/tmp/y.mp4", ObjectKey: "y.mp4",
		Status: UploadStatusFailed, MaxRetries: 10,
	}
	if err := db.Create(&ut).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx] = MarkUploading(db, ut.ID)
		}(i)
	}
	wg.Wait()

	claimed := 0
	for _, ok := range results {
		if ok {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("并发抢占应只有 1 个成功，实际 %d", claimed)
	}
	if MarkUploading(db, ut.ID) {
		t.Error("已 Uploading 的任务不应再被抢占")
	}
}

// TestReclaimThenQueryable：P1-a 修复链路 —— 卡死任务回收后能被 QueryPendingUploads 命中。
func TestReclaimThenQueryable(t *testing.T) {
	db := newUploadTestDB(t)
	stale := seedUploading(t, db, time.Hour, 1)

	if got, _ := QueryPendingUploads(db, 10); len(got) != 0 {
		t.Fatalf("回收前 Uploading 任务不应被补传查询命中，实际 %d", len(got))
	}
	if _, err := ReclaimStaleUploading(db, 35*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := QueryPendingUploads(db, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != stale.ID {
		t.Fatalf("回收后任务应可被补传命中，实际 %d 条", len(got))
	}
}
