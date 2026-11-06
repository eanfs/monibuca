package m7s

import (
	"context"
	"errors"
	"testing"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/util"
)

// newTrackTestPublisher 构造只用于轨道等待测试的 publisher：
// audioReady/videoReady 是唯一需要的状态，不需要真实的 Server 或 task 生命周期。
func newTrackTestPublisher(audio, video bool) *Publisher {
	p := &Publisher{
		Publish:    config.Publish{PubAudio: audio, PubVideo: video},
		audioReady: util.NewPromise(context.Background()),
		videoReady: util.NewPromise(context.Background()),
	}
	return p
}

func TestResolveSubscribeTimeout(t *testing.T) {
	cases := []struct {
		name string
		conf config.Subscribe
		want time.Duration
	}{
		{"initial unset keeps wait timeout", config.Subscribe{WaitTimeout: 30 * time.Second}, 30 * time.Second},
		{"initial tightens", config.Subscribe{WaitTimeout: 30 * time.Second, InitialWaitTimeout: 3 * time.Second}, 3 * time.Second},
		{"initial does not loosen", config.Subscribe{WaitTimeout: 2 * time.Second, InitialWaitTimeout: 30 * time.Second}, 2 * time.Second},
		{"both unset falls back to default", config.Subscribe{}, defaultSubscribeTimeout},
		{"only initial set", config.Subscribe{InitialWaitTimeout: 3 * time.Second}, 3 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveSubscribeTimeout(c.conf); got != c.want {
				t.Fatalf("resolveSubscribeTimeout() = %v, want %v", got, c.want)
			}
		})
	}
}

// 生命周期预算不得被订阅阶段预算污染：subscriber 会长期持有原始 conf，
// 播放期等流恢复读的就是 conf.WaitTimeout。
func TestResolveSubscribeTimeoutDoesNotMutateWaitTimeout(t *testing.T) {
	conf := config.Subscribe{WaitTimeout: 10 * time.Minute, InitialWaitTimeout: time.Second}
	if got := resolveSubscribeTimeout(conf); got != time.Second {
		t.Fatalf("resolve = %v, want 1s", got)
	}
	if conf.WaitTimeout != 10*time.Minute {
		t.Fatalf("conf.WaitTimeout mutated to %v", conf.WaitTimeout)
	}
}

func TestWaitTrackContext(t *testing.T) {
	t.Run("video ready resolves", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		p.videoReady.Resolve()
		if err := p.WaitTrackContext(context.Background(), false, true); err != nil {
			t.Fatalf("WaitTrackContext() = %v, want nil", err)
		}
	})

	t.Run("video never ready hits deadline", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if err := p.WaitTrackContext(ctx, false, true); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitTrackContext() = %v, want DeadlineExceeded", err)
		}
	})

	t.Run("any track mode resolves on audio", func(t *testing.T) {
		p := newTrackTestPublisher(true, true)
		p.audioReady.Resolve()
		// WaitTrack=="" 走 any-track 分支：audio/video 参数都为 false
		if err := p.WaitTrackContext(context.Background(), false, false); err != nil {
			t.Fatalf("WaitTrackContext() = %v, want nil", err)
		}
	})

	t.Run("pub does not carry the track is no track", func(t *testing.T) {
		// 生产路径：PubVideo=false 时 Start 里直接 Reject(ErrMuted)
		p := newTrackTestPublisher(false, false)
		p.videoReady.Reject(pkg.ErrMuted)
		if err := p.WaitTrackContext(context.Background(), false, true); !errors.Is(err, pkg.ErrNoTrack) {
			t.Fatalf("WaitTrackContext() = %v, want ErrNoTrack", err)
		}
	})

	t.Run("cancel propagates as canceled", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := p.WaitTrackContext(ctx, false, true); !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitTrackContext() = %v, want Canceled", err)
		}
	})

	t.Run("WaitTrack delegates to WaitTrackContext", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		p.videoReady.Resolve()
		if err := p.WaitTrack(false, true); err != nil {
			t.Fatalf("WaitTrack() = %v, want nil", err)
		}
	})
}

func TestWaitPublisherTracks(t *testing.T) {
	t.Run("track ready", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		p.videoReady.Resolve()
		if err := waitPublisherTracks(context.Background(), p, "video", time.Second); err != nil {
			t.Fatalf("waitPublisherTracks() = %v, want nil", err)
		}
	})

	t.Run("budget bounds the wait", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		start := time.Now()
		err := waitPublisherTracks(context.Background(), p, "video", 20*time.Millisecond)
		elapsed := time.Since(start)
		if !errors.Is(err, pkg.ErrSubscribeTimeout) {
			t.Fatalf("waitPublisherTracks() = %v, want pkg.ErrSubscribeTimeout", err)
		}
		if elapsed < 20*time.Millisecond || elapsed > time.Second {
			t.Fatalf("elapsed = %v, want ~20ms", elapsed)
		}
	})

	t.Run("exhausted budget fails fast", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		start := time.Now()
		if err := waitPublisherTracks(context.Background(), p, "video", 0); !errors.Is(err, pkg.ErrSubscribeTimeout) {
			t.Fatalf("waitPublisherTracks() = %v, want pkg.ErrSubscribeTimeout", err)
		}
		if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
			t.Fatalf("elapsed = %v, want near immediate", elapsed)
		}
	})

	t.Run("caller cancel is not reported as timeout", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := waitPublisherTracks(ctx, p, "video", time.Second)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waitPublisherTracks() = %v, want Canceled", err)
		}
		if errors.Is(err, pkg.ErrSubscribeTimeout) {
			t.Fatalf("canceled wait must not look like a subscribe timeout")
		}
	})

	t.Run("publisher stopped mid wait is transient not no track", func(t *testing.T) {
		p := newTrackTestPublisher(false, true)
		// 生产里轨道就绪 promise 以 publisher 为父 ctx：publisher 停止时 promise 会被
		// 取消并带回停止原因，WaitTrack 会把它归一成 ErrNoTrack。这里模拟同一结果，
		// 并让 publisher 处于停止状态。
		p.videoReady.Reject(pkg.ErrLost)
		stopped, cancel := context.WithCancelCause(context.Background())
		cancel(pkg.ErrLost)
		p.Context = stopped
		if err := waitPublisherTracks(context.Background(), p, "video", time.Second); !errors.Is(err, pkg.ErrSubscribeTimeout) {
			t.Fatalf("waitPublisherTracks() = %v, want pkg.ErrSubscribeTimeout", err)
		}
	})
}
