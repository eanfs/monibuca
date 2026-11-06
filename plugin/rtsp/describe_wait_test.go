package plugin_rtsp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/util"
)

func TestDescribeSubscribeConfigClampsInitialWaitTimeout(t *testing.T) {
	base := config.Subscribe{WaitTimeout: 10 * time.Minute, InitialWaitTimeout: 5 * time.Second}
	cases := []struct {
		name  string
		base  config.Subscribe
		limit time.Duration
		want  time.Duration
	}{
		{"limit tightens global", base, 3 * time.Second, 3 * time.Second},
		{"limit does not loosen global", config.Subscribe{InitialWaitTimeout: time.Second}, 3 * time.Second, time.Second},
		{"zero limit keeps global", base, 0, 5 * time.Second},
		{"zero global uses limit", config.Subscribe{}, 2 * time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describeSubscribeConfig(c.base, c.limit); got.InitialWaitTimeout != c.want {
				t.Fatalf("InitialWaitTimeout = %v, want %v", got.InitialWaitTimeout, c.want)
			}
		})
	}
}

// WaitTimeout 同时是 subscriber 整个生命周期的重等预算（subscriber.go 的
// checkPublishChanged 与 server.go 的卡死兜底），DESCRIBE 只能收紧订阅阶段的
// InitialWaitTimeout，绝不能把它一起压到 3s——否则已在播放的连接在 publisher
// 短暂消失（拉流重试默认 5s）时会提前放弃。
func TestDescribeSubscribeConfigKeepsWaitTimeout(t *testing.T) {
	base := config.Subscribe{WaitTimeout: 10 * time.Minute, WaitTrack: "video"}
	got := describeSubscribeConfig(base, time.Second)
	if got.WaitTimeout != base.WaitTimeout {
		t.Fatalf("WaitTimeout changed to %v, want %v", got.WaitTimeout, base.WaitTimeout)
	}
	if got.WaitTrack != base.WaitTrack {
		t.Fatalf("WaitTrack changed to %q", got.WaitTrack)
	}
	if got.InitialWaitTimeout != time.Second {
		t.Fatalf("InitialWaitTimeout = %v, want 1s", got.InitialWaitTimeout)
	}
}

func TestDescribeErrorFailure(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCode  int
		wantRetry time.Duration
	}{
		{"subscribe timeout -> 503", pkg.ErrSubscribeTimeout, http.StatusServiceUnavailable, time.Second},
		{"wrapped timeout -> 503", fmt.Errorf("subscribe: %w", pkg.ErrSubscribeTimeout), http.StatusServiceUnavailable, time.Second},
		{"context deadline -> 503", context.DeadlineExceeded, http.StatusServiceUnavailable, time.Second},
		{"no track -> 400", pkg.ErrNoTrack, http.StatusBadRequest, 0},
		{"other -> 400", errors.New("internal detail"), http.StatusBadRequest, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := describeErrorFailure(c.err)
			if got.StatusCode != c.wantCode || got.RetryAfter != c.wantRetry {
				t.Fatalf("describeErrorFailure(%v) = (%d,%v), want (%d,%v)",
					c.err, got.StatusCode, got.RetryAfter, c.wantCode, c.wantRetry)
			}
			if got.StatusText != http.StatusText(c.wantCode) {
				t.Fatalf("StatusText = %q, want %q", got.StatusText, http.StatusText(c.wantCode))
			}
			if strings.Contains(got.StatusText, "internal detail") {
				t.Fatalf("StatusText leaks error detail: %q", got.StatusText)
			}
		})
	}
}

func TestDescribeFailureResponseRetryAfter(t *testing.T) {
	cases := []struct {
		name      string
		failure   describeFailure
		wantRetry string
	}{
		{"503 carries retry-after", describeErrorFailure(pkg.ErrSubscribeTimeout), "1"},
		{"400 has no retry-after", describeErrorFailure(pkg.ErrNoTrack), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := c.failure.response(&util.Request{})
			if got := res.Header.Get("Retry-After"); got != c.wantRetry {
				t.Fatalf("Retry-After = %q, want %q", got, c.wantRetry)
			}
			if res.StatusCode != c.failure.StatusCode {
				t.Fatalf("StatusCode = %d, want %d", res.StatusCode, c.failure.StatusCode)
			}
		})
	}
}
