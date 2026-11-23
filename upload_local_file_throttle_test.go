package m7s

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"m7s.live/v5/pkg/storage"
)

// TestUploadLocalFileCopyIsThrottled pins that the pending-file copy in
// UploadLocalFile reads through the process-wide upload read limiter.
//
// UploadLocalFile only acquires an upload slot later, inside uploadTempFile, so
// this copy would otherwise be ungoverned: during a retry tick the scheduler
// starts up to 20 of these copies at once, each reading a whole file at full
// speed before any storage-side limit applies.
func TestUploadLocalFileCopyIsThrottled(t *testing.T) {
	// 1 MB/s with a 1 MiB burst (rate/4 clamped up to one chunk).
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), UploadReadRateMBps: 1})
	t.Cleanup(func() {
		storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir()})
	})

	src := filepath.Join(t.TempDir(), "pending.mp4")
	if err := os.WriteFile(src, make([]byte, 2<<20), 0o600); err != nil {
		t.Fatalf("write source file: %v", err)
	}
	st := &retryCountingStorage{
		runtimeTestStorage: runtimeTestStorage{key: "s3"},
		remoteDir:          t.TempDir(),
	}

	start := time.Now()
	if err := UploadLocalFile(context.Background(), st, src, "records/x.mp4", nil); err != nil {
		t.Fatalf("UploadLocalFile: %v", err)
	}
	elapsed := time.Since(start)

	// The first MiB comes from the burst; the second must wait ~1 s at 1 MB/s.
	// An untrottled io.Copy finishes in milliseconds, so the lower bound is the
	// assertion that matters (no upper bound: a loaded machine may add slop).
	if elapsed < 900*time.Millisecond {
		t.Fatalf("copy finished in %v; the second MiB must wait ~1s at 1 MB/s (copy is not throttled)", elapsed)
	}
}
