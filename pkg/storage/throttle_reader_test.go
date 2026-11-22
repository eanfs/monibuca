package storage

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestInitUploadManagerConfiguresReadLimiter(t *testing.T) {
	t.Cleanup(func() { InitUploadManager(UploadConfig{PendingDir: t.TempDir()}) })
	InitUploadManager(UploadConfig{PendingDir: t.TempDir(), UploadReadRateMBps: 100})
	if uploadReadLimiter == nil {
		t.Fatal("read rate 100 MB/s must install a limiter")
	}
	InitUploadManager(UploadConfig{PendingDir: t.TempDir()})
	if uploadReadLimiter != nil {
		t.Fatal("read rate 0 must leave no limiter (zero overhead)")
	}
}

func TestUploadThrottledReadSeekerZeroRatePassthrough(t *testing.T) {
	InitUploadManager(UploadConfig{PendingDir: t.TempDir()}) // UploadReadRateMBps = 0
	rs := bytes.NewReader(make([]byte, 16))
	if got := NewUploadThrottledReadSeeker(context.Background(), rs); got != io.ReadSeeker(rs) {
		t.Fatalf("zero rate must return the original reader, got %T", got)
	}
}

func TestUploadThrottledReadSeekerPreservesReaderAt(t *testing.T) {
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<30), 8<<20)
	defer func() { uploadReadLimiter = nil }()

	src := bytes.NewReader(make([]byte, 4096))
	wrapped := NewUploadThrottledReadSeeker(context.Background(), src)
	ra, ok := wrapped.(io.ReaderAt)
	if !ok {
		t.Fatal("wrapper must keep io.ReaderAt so the S3 SDK can use concurrent range reads")
	}
	buf := make([]byte, 4096)
	if n, err := ra.ReadAt(buf, 0); n != 4096 || err != nil {
		t.Fatalf("ReadAt = (%d, %v), want (4096, nil)", n, err)
	}
}

func TestUploadThrottledReadersShareOneBucket(t *testing.T) {
	// 1 MiB/s with an 8 MiB burst: the four 1 MiB reservations below consume 4 MiB
	// and never drive the bucket negative, so the test does not sleep. Refill must
	// stay slow on purpose — at a high rate (e.g. 1 GiB/s) the time spent copying
	// the 4 MiB refills the bucket fast enough to mask the consumption.
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<20), 8<<20)
	defer func() { uploadReadLimiter = nil }()

	ctx := context.Background()
	buf := make([]byte, 1<<20) // exactly maxThrottleChunk
	for i := 0; i < 4; i++ {
		rs := bytes.NewReader(make([]byte, len(buf)))
		if _, err := io.ReadFull(NewUploadThrottledReadSeeker(ctx, rs), buf); err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
	}
	// Four 1 MB reads must have consumed ~4 MB from the single shared bucket.
	if tokens := uploadReadLimiter.Tokens(); tokens > 5<<20 {
		t.Fatalf("tokens left = %.0f, want <= %d (readers must share one bucket)", tokens, 5<<20)
	}
}

func TestUploadThrottledReaderDeadline(t *testing.T) {
	// 1 B/s with a 1 MiB burst. Drain the burst first: a fresh bucket would let
	// the read through immediately. The next 1 MiB read cannot finish in 20 ms.
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1), 1<<20)
	defer func() { uploadReadLimiter = nil }()
	if err := uploadReadLimiter.WaitN(context.Background(), 1<<20); err != nil {
		t.Fatalf("drain burst: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rs := bytes.NewReader(make([]byte, maxThrottleChunk))
	start := time.Now()
	_, err := NewUploadThrottledReadSeeker(ctx, rs).Read(make([]byte, maxThrottleChunk))
	if err == nil {
		t.Fatal("read must fail once the context deadline cannot be met")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("read blocked for %s; it must observe the deadline", elapsed)
	}
}

func TestUploadThrottledReadSeekerSeekPassthrough(t *testing.T) {
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<30), 1<<20)
	defer func() { uploadReadLimiter = nil }()

	rs := bytes.NewReader(make([]byte, 1024))
	wrapped := NewUploadThrottledReadSeeker(context.Background(), rs)
	off, err := wrapped.Seek(512, io.SeekStart)
	if err != nil || off != 512 {
		t.Fatalf("Seek = (%d, %v), want (512, nil)", off, err)
	}
}
