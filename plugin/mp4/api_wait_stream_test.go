package plugin_mp4

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg"
	mp4pb "m7s.live/v5/plugin/mp4/pb"
)

func TestWaitForPublisherReturnsImmediatelyWhenStreamExists(t *testing.T) {
	start := time.Now()
	if !waitForPublisher(context.Background(), "live/a", time.Second, time.Millisecond,
		func(string) (*m7s.Publisher, bool) { return &m7s.Publisher{}, true }) {
		t.Fatal("waitForPublisher() = false, want true")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waitForPublisher() took %v, should return immediately", elapsed)
	}
}

// 流比录制请求晚出现时（拉流刚发起/设备刚重连）应等到它上线再返回。
func TestWaitForPublisherWaitsUntilStreamAppears(t *testing.T) {
	var calls int
	ok := waitForPublisher(context.Background(), "live/a", 2*time.Second, time.Millisecond,
		func(string) (*m7s.Publisher, bool) {
			calls++
			return &m7s.Publisher{}, calls >= 3
		})
	if !ok {
		t.Fatal("waitForPublisher() = false, want true")
	}
	if calls < 3 {
		t.Fatalf("waitForPublisher() polled %d times, want >= 3", calls)
	}
}

func TestWaitForPublisherTimesOut(t *testing.T) {
	start := time.Now()
	if waitForPublisher(context.Background(), "live/a", 20*time.Millisecond, 2*time.Millisecond,
		func(string) (*m7s.Publisher, bool) { return nil, false }) {
		t.Fatal("waitForPublisher() = true, want false on timeout")
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("waitForPublisher() returned after %v, want about 20ms", elapsed)
	}
}

func TestWaitForPublisherHonorsContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	start := time.Now()
	if waitForPublisher(ctx, "live/a", 5*time.Second, time.Millisecond,
		func(string) (*m7s.Publisher, bool) { return nil, false }) {
		t.Fatal("waitForPublisher() = true, want false after ctx deadline")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waitForPublisher() ignored ctx deadline, waited %v", elapsed)
	}
}

func TestWaitForPublisherDisabledWhenTimeoutZero(t *testing.T) {
	called := false
	if waitForPublisher(context.Background(), "live/a", 0, time.Millisecond,
		func(string) (*m7s.Publisher, bool) {
			called = true
			return nil, false
		}) {
		t.Fatal("waitForPublisher(timeout=0) = true, want false")
	}
	if called {
		t.Fatal("waitForPublisher(timeout=0) must not poll")
	}
}

// 接线回归：删掉 StartRecord 里的 waitPublisherBeforeRecord 后，本用例会因
// 立即返回而失败；同时钉住"缺失流返回 404 而非 500"。
func TestStartRecordWaitsForMissingStream(t *testing.T) {
	var root task.RootManager[uint32, task.ManagerItem[uint32]]
	root.Init()
	s := &m7s.Server{}
	if err := root.AddTask(&s.Streams).WaitStarted(); err != nil {
		t.Fatalf("start Streams: %v", err)
	}

	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	p.Server = s
	p.RecordConfig.WaitForPublisher = 150 * time.Millisecond

	start := time.Now()
	_, err := p.StartRecord(context.Background(), &mp4pb.ReqStartRecord{StreamPath: "live/missing"})
	elapsed := time.Since(start)

	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("StartRecord() code = %v, want NotFound (err=%v)", code, err)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("StartRecord() returned in %v, wait window not applied", elapsed)
	}
}

// 接线回归：StopRecord 缺失流必须是 404（旧实现是裸 error → HTTP 500）。
func TestStopRecordMissingReturnsNotFound(t *testing.T) {
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.DiscardHandler)
	p.Server = &m7s.Server{}

	_, err := p.StopRecord(context.Background(), &mp4pb.ReqStopRecord{StreamPath: "live/missing"})
	if code := status.Code(err); code != codes.NotFound {
		t.Fatalf("StopRecord() code = %v, want NotFound", code)
	}
}

func TestClassifyRecordAPIError(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "recording concurrency limit reached")
	cases := []struct {
		name string
		in   error
		want codes.Code
	}{
		{"nil", nil, codes.OK},
		{"not found", pkg.ErrNotFound, codes.NotFound},
		{"wrapped not found", fmt.Errorf("start: %w", pkg.ErrNotFound), codes.NotFound},
		{"record exists", pkg.ErrRecordExists, codes.AlreadyExists},
		{"status error passes through", unavailable, codes.Unavailable},
		{"plain error stays unknown", errors.New("boom"), codes.Unknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyRecordAPIError(c.in)
			if c.in == nil {
				if got != nil {
					t.Fatalf("classifyRecordAPIError(nil) = %v, want nil", got)
				}
				return
			}
			if code := status.Code(got); code != c.want {
				t.Fatalf("classifyRecordAPIError(%v) code = %v, want %v", c.in, code, c.want)
			}
		})
	}
}
