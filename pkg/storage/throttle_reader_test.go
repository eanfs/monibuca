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
	if got := uploadReadLimiter.Limit(); got != rate.Limit(100<<20) {
		t.Fatalf("Limit() = %v, want %v", got, rate.Limit(100<<20))
	}
	// 100 MB/s / 4 = 250 ms of headroom, inside the [1 MiB, 32 MiB] clamp.
	if got := uploadReadLimiter.Burst(); got != 25<<20 {
		t.Fatalf("Burst() = %d, want %d", got, 25<<20)
	}
	InitUploadManager(UploadConfig{PendingDir: t.TempDir()})
	if uploadReadLimiter != nil {
		t.Fatal("read rate 0 must leave no limiter (zero overhead)")
	}
}

func TestInitUploadManagerReadLimiterBurstBounds(t *testing.T) {
	t.Cleanup(func() { InitUploadManager(UploadConfig{PendingDir: t.TempDir()}) })
	cases := []struct {
		mbps      int
		wantBurst int
	}{
		{1, 1 << 20},     // rate/4 = 256 KiB, floored to one chunk
		{1024, 32 << 20}, // rate/4 = 256 MiB, capped
	}
	for _, c := range cases {
		InitUploadManager(UploadConfig{PendingDir: t.TempDir(), UploadReadRateMBps: c.mbps})
		if uploadReadLimiter == nil {
			t.Fatalf("read rate %d MB/s must install a limiter", c.mbps)
		}
		if got := uploadReadLimiter.Burst(); got != c.wantBurst {
			t.Fatalf("read rate %d MB/s: Burst() = %d, want %d", c.mbps, got, c.wantBurst)
		}
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

func TestUploadThrottledReadSeekerHidesReaderAtWhenSourceLacksIt(t *testing.T) {
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<30), 8<<20)
	defer func() { uploadReadLimiter = nil }()

	// Embedding io.ReadSeeker promotes Read and Seek but not ReadAt.
	src := struct{ io.ReadSeeker }{bytes.NewReader(make([]byte, 16))}
	wrapped := NewUploadThrottledReadSeeker(context.Background(), src)
	if _, ok := wrapped.(io.ReaderAt); ok {
		t.Fatal("wrapper must not advertise io.ReaderAt when the source lacks it")
	}
}

func TestUploadThrottledReadersShareOneBucket(t *testing.T) {
	// 64 KiB/s with an 8 MiB burst: the four 1 MiB reservations below consume
	// 4 MiB and never drive the bucket negative, so the test does not sleep.
	// Refill must stay slow on purpose — at a high rate the time spent copying
	// the 4 MiB would refill the bucket fast enough to mask the consumption.
	uploadReadLimiter = rate.NewLimiter(rate.Limit(64<<10), 8<<20)
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

func TestUploadThrottledReadSeekerMultiChunk(t *testing.T) {
	// 1 GiB/s so the three 1 MiB reservations never sleep; the assertion is that
	// Read/ReadAt refill the caller's whole buffer across chunk boundaries.
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<30), 8<<20)
	defer func() { uploadReadLimiter = nil }()

	const size = 3 << 20 // three maxThrottleChunk pieces
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i%251 + 1) // non-zero so a short read changes the content
	}
	ctx := context.Background()

	buf := make([]byte, size)
	n, err := NewUploadThrottledReadSeeker(ctx, bytes.NewReader(src)).Read(buf)
	if n != size || err != nil {
		t.Fatalf("Read = (%d, %v), want (%d, nil)", n, err, size)
	}
	if !bytes.Equal(buf, src) {
		t.Fatal("Read across chunks returned corrupt content")
	}

	ra, ok := NewUploadThrottledReadSeeker(ctx, bytes.NewReader(src)).(io.ReaderAt)
	if !ok {
		t.Fatal("bytes.Reader source must expose io.ReaderAt through the wrapper")
	}
	atBuf := make([]byte, size)
	n, err = ra.ReadAt(atBuf, 0)
	if n != size || err != nil {
		t.Fatalf("ReadAt = (%d, %v), want (%d, nil)", n, err, size)
	}
	if !bytes.Equal(atBuf, src) {
		t.Fatal("ReadAt across chunks returned corrupt content")
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
