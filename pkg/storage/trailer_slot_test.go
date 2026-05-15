package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// initForTrailerTest 重置 UploadManager 为指定 trailer 槽位
func initForTrailerTest(t *testing.T, maxTrailer int) {
	t.Helper()
	InitUploadManager(UploadConfig{
		MaxConcurrentUploads:       4,
		MaxConcurrentTrailerWrites: maxTrailer,
		PendingDir:                 t.TempDir(),
	})
}

func TestTrailerSlotConcurrencyLimit(t *testing.T) {
	initForTrailerTest(t, 2)

	var active int32
	var maxObserved int32
	var wg sync.WaitGroup
	hold := 100 * time.Millisecond

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := AcquireTrailerSlot(context.Background()); err != nil {
				t.Errorf("AcquireTrailerSlot: %v", err)
				return
			}
			defer ReleaseTrailerSlot()
			cur := atomic.AddInt32(&active, 1)
			for {
				prev := atomic.LoadInt32(&maxObserved)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxObserved, prev, cur) {
					break
				}
			}
			time.Sleep(hold)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()

	if maxObserved > 2 {
		t.Fatalf("max concurrent trailers should be ≤ 2, got %d", maxObserved)
	}
	if got := GetActiveTrailerWrites(); got != 0 {
		t.Fatalf("active should be 0 after all done, got %d", got)
	}
	if got := GetMaxConcurrentTrailerWrites(); got != 2 {
		t.Fatalf("GetMaxConcurrentTrailerWrites should be 2, got %d", got)
	}
}

func TestTrailerSlotContextCancel(t *testing.T) {
	initForTrailerTest(t, 1)
	if err := AcquireTrailerSlot(context.Background()); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	defer ReleaseTrailerSlot()

	// pre-cancelled ctx
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := AcquireTrailerSlot(ctx); err == nil {
		t.Fatal("second acquire should fail with cancelled ctx")
	}
}

func TestTrailerSlotFallsBackTo8WhenZero(t *testing.T) {
	// MaxConcurrentTrailerWrites 留 0 时, 应取 fallback 8
	InitUploadManager(UploadConfig{
		MaxConcurrentUploads: 4,
		PendingDir:           t.TempDir(),
	})
	if got := GetMaxConcurrentTrailerWrites(); got != 8 {
		t.Fatalf("zero/unset should fall back to 8, got %d", got)
	}
}

func TestTrailerSlotIndependentFromUploadSem(t *testing.T) {
	// trailer 槽位 = 2, upload 槽位 = 4. 把 trailer 槽位占满, upload 应仍可正常 acquire.
	InitUploadManager(UploadConfig{
		MaxConcurrentUploads:       4,
		MaxConcurrentTrailerWrites: 2,
		PendingDir:                 t.TempDir(),
	})
	if err := AcquireTrailerSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer ReleaseTrailerSlot()
	if err := AcquireTrailerSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer ReleaseTrailerSlot()
	// trailer 满 (2/2). 第 3 次 trailer acquire 应 block; 用 timeout ctx 验.

	// upload 槽位与 trailer 独立, 应能拿到
	done := make(chan struct{})
	go func() {
		if err := AcquireUploadSlot(context.Background()); err != nil {
			t.Error("upload acquire blocked while trailer full:", err)
			close(done)
			return
		}
		ReleaseUploadSlot()
		close(done)
	}()
	select {
	case <-done:
		// OK
	case <-time.After(200 * time.Millisecond):
		t.Fatal("upload sem 与 trailer sem 不独立, upload 被 trailer 满槽阻塞")
	}
}
