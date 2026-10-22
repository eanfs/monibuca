package plugin_mp4

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/config"
	mp4pb "m7s.live/v5/plugin/mp4/pb"
)

// waitFor 在 deadline 前轮询 ready，用于替代固定 sleep（确定性测试）。
func waitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func acquireAsync(l *RecordLimiter, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- l.Acquire(ctx) }()
	return done
}

// 上限 1、并发 3：第 2 个排队，Release 后放行；第 3 个排队超时返回错误。
func TestRecordLimiterQueuesThenTimesOut(t *testing.T) {
	l := NewRecordLimiter(1)
	l.timeout = 30 * time.Millisecond

	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	second := acquireAsync(l, context.Background())
	waitFor(t, "second waiter to queue", func() bool { return l.Waiting() == 1 })

	third := acquireAsync(l, context.Background())
	waitFor(t, "third waiter to queue", func() bool { return l.Waiting() == 2 })

	l.Release()
	select {
	case err := <-second:
		if err != nil {
			t.Fatalf("second acquire should be admitted after release, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("second acquire did not return after release")
	}
	if got := l.Active(); got != 1 {
		t.Fatalf("active = %d after handover, want 1", got)
	}

	select {
	case err := <-third:
		if !errors.Is(err, ErrRecordLimitTimeout) {
			t.Fatalf("third acquire error = %v, want ErrRecordLimitTimeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("third acquire did not time out")
	}

	// 超时的等待者不得占用名额。
	if got := l.Waiting(); got != 0 {
		t.Fatalf("waiting = %d after timeout, want 0", got)
	}
	l.Release()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after final release, want 0", got)
	}
}

// Release 必须唤醒等待者；槽位在释放者与等待者之间转移而不会凭空减少。
func TestRecordLimiterReleaseLetsWaiterProceed(t *testing.T) {
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	waiter := acquireAsync(l, context.Background())
	waitFor(t, "waiter to queue", func() bool { return l.Waiting() == 1 })

	l.Release()
	select {
	case err := <-waiter:
		if err != nil {
			t.Fatalf("waiter acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter was not woken by release")
	}
	if got, want := l.Active(), 1; got != want {
		t.Fatalf("active = %d, want %d", got, want)
	}
	l.Release()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after release, want 0", got)
	}
}

// 有界等待队列：容量满后立即拒绝，不无限排队。
func TestRecordLimiterQueueIsBounded(t *testing.T) {
	l := NewRecordLimiter(1)
	l.timeout = time.Second
	if want := 1 * recordLimitQueueFactor; l.queueCapacity != want {
		t.Fatalf("queueCapacity = %d, want %d", l.queueCapacity, want)
	}
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	results := make([]<-chan error, 0, l.queueCapacity)
	for i := 0; i < l.queueCapacity; i++ {
		results = append(results, acquireAsync(l, ctx))
	}
	waitFor(t, "queue to fill", func() bool { return l.Waiting() == l.queueCapacity })

	if err := l.Acquire(context.Background()); !errors.Is(err, ErrRecordLimitQueueFull) {
		t.Fatalf("acquire with full queue = %v, want ErrRecordLimitQueueFull", err)
	}

	cancel() // 释放所有排队者（返回 ctx.Canceled）
	for _, result := range results {
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("canceled waiter should return an error")
			}
		case <-time.After(time.Second):
			t.Fatal("canceled waiter did not return")
		}
	}
	waitFor(t, "waiters to drain", func() bool { return l.Waiting() == 0 })
	l.Release()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d, want 0", got)
	}
}

// 上限 0 = 完全不限：Acquire/Release/TryAcquire 均为 no-op。
func TestRecordLimiterDisabledIsNoop(t *testing.T) {
	l := NewRecordLimiter(0)
	if l.Enabled() {
		t.Fatal("limiter with max=0 must be disabled")
	}
	for i := 0; i < 100; i++ {
		if err := l.Acquire(context.Background()); err != nil {
			t.Fatalf("acquire #%d: %v", i, err)
		}
	}
	if waiter, ok := l.Reserve(); !ok || waiter != nil {
		t.Fatalf("Reserve on disabled limiter = (%v, %v), want (nil, true)", waiter, ok)
	}
	l.Release()
	l.Release()
	if l.Active() != 0 || l.Waiting() != 0 {
		t.Fatalf("disabled limiter state changed: active=%d waiting=%d", l.Active(), l.Waiting())
	}
}

// gate：启用时先 TryAcquire，槽位占用且队列满时必须返回 Refused，不能静默停录。
func TestGateRecordRefusesWhenQueueFull(t *testing.T) {
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	l := NewRecordLimiter(1)
	l.timeout = 10 * time.Millisecond
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	l.mu.Lock()
	for i := 0; i < l.queueCapacity; i++ {
		l.waiters = append(l.waiters, make(chan struct{}, 1))
	}
	l.mu.Unlock()
	p.recordLimiter.Store(l)

	release, result := p.GateRecord(&m7s.Publisher{}, config.Record{})
	if release != nil {
		t.Fatal("refused recording must not return a release fn")
	}
	if result != m7s.RecordGateRefused {
		t.Fatalf("result = %v, want RecordGateRefused", result)
	}
}

// gate：上限 0 时必须完全放行（保持现有行为）。
func TestGateRecordDisabledAdmits(t *testing.T) {
	p := &MP4Plugin{}
	p.recordLimiter.Store(NewRecordLimiter(0))
	release, result := p.GateRecord(&m7s.Publisher{}, config.Record{})
	if release != nil {
		t.Fatal("disabled limiter must not return a release fn")
	}
	if result != m7s.RecordGateAdmit {
		t.Fatalf("result = %v, want RecordGateAdmit", result)
	}
}

// StartRecord 在 limiter 拒绝时返回 503 映射（codes.Unavailable）。
func TestStartRecordReturnsUnavailableWhenLimiterRefuses(t *testing.T) {
	p := newLimiterTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = 10 * time.Millisecond
	l.active = 1
	l.mu.Lock()
	for i := 0; i < l.queueCapacity; i++ {
		l.waiters = append(l.waiters, make(chan struct{}, 1))
	}
	l.mu.Unlock()
	p.recordLimiter.Store(l)

	_, err := p.StartRecord(context.Background(), &mp4pb.ReqStartRecord{StreamPath: "live/test"})
	if err == nil {
		t.Fatal("StartRecord should fail when limiter refuses")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("status code = %v, want %v (err=%v)", got, codes.Unavailable, err)
	}
}

// StartRecord 在取得名额后失败（此处为流不存在）必须释放名额，不得泄漏。
func TestStartRecordReleasesSlotOnFailure(t *testing.T) {
	p := newLimiterTestPlugin(t)
	l := NewRecordLimiter(1)
	l.timeout = 20 * time.Millisecond
	p.recordLimiter.Store(l)

	_, err := p.StartRecord(context.Background(), &mp4pb.ReqStartRecord{StreamPath: "live/missing"})
	if err == nil {
		t.Fatal("StartRecord should fail for a missing stream")
	}
	if status.Code(err) == codes.Unavailable {
		t.Fatalf("missing stream must not be reported as limiter refusal: %v", err)
	}
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after failed StartRecord, want 0 (slot leaked)", got)
	}
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("slot was leaked, re-acquire failed: %v", err)
	}
	l.Release()
}

// 上限 0 时 StartRecord 行为与现状一致（不排队、不拒绝）。
func TestStartRecordUnlimitedPreservesBehavior(t *testing.T) {
	p := newLimiterTestPlugin(t)
	l := NewRecordLimiter(0)
	p.recordLimiter.Store(l)

	_, err := p.StartRecord(context.Background(), &mp4pb.ReqStartRecord{StreamPath: "live/missing"})
	if err == nil {
		t.Fatal("StartRecord should fail for a missing stream")
	}
	if status.Code(err) == codes.Unavailable {
		t.Fatalf("unlimited limiter must not refuse: %v", err)
	}
	if l.Active() != 0 || l.Waiting() != 0 {
		t.Fatalf("disabled limiter state changed: active=%d waiting=%d", l.Active(), l.Waiting())
	}
}

func newLimiterTestPlugin(t *testing.T) *MP4Plugin {
	t.Helper()
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	p.Server = &m7s.Server{}
	return p
}

// 并发安全：多 goroutine 同时 Acquire/Release 后名额必须归零（配合 -race）。
func TestRecordLimiterConcurrentAcquireRelease(t *testing.T) {
	l := NewRecordLimiter(4)
	l.timeout = time.Second
	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := l.Acquire(ctx); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			l.Release()
		}()
	}
	wg.Wait()
	if got := l.Active(); got != 0 {
		t.Fatalf("active = %d after all releases, want 0", got)
	}
	if got := l.Waiting(); got != 0 {
		t.Fatalf("waiting = %d after all releases, want 0", got)
	}
}
