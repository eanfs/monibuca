package plugin_rtsp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
)

func TestDescribeSubscribeConfigClampsWaitTimeout(t *testing.T) {
	base := config.Subscribe{WaitTimeout: 10 * time.Minute, WaitTrack: "video"}
	cases := []struct {
		name  string
		base  config.Subscribe
		limit time.Duration
		want  time.Duration
	}{
		{"limit tightens global", base, 3 * time.Second, 3 * time.Second},
		{"limit does not loosen global", config.Subscribe{WaitTimeout: time.Second}, 3 * time.Second, time.Second},
		{"zero limit keeps global", base, 0, 10 * time.Minute},
		{"zero global uses limit", config.Subscribe{}, 2 * time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describeSubscribeConfig(c.base, c.limit); got.WaitTimeout != c.want {
				t.Fatalf("describeSubscribeConfig() WaitTimeout = %v, want %v", got.WaitTimeout, c.want)
			}
		})
	}
	// 不应修改其它字段
	if got := describeSubscribeConfig(base, time.Second); got.WaitTrack != "video" {
		t.Fatalf("WaitTrack changed to %q", got.WaitTrack)
	}
}

func TestDescribeErrorResponse(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   int
		wantRetry  int
		wantStatus string
	}{
		{"no track -> 503", pkg.ErrNoTrack, http.StatusServiceUnavailable, 1, "Service Unavailable"},
		{"wrapped no track -> 503", fmt.Errorf("subscribe: %w", pkg.ErrNoTrack), http.StatusServiceUnavailable, 1, "Service Unavailable"},
		{"subscribe timeout -> 503", pkg.ErrSubscribeTimeout, http.StatusServiceUnavailable, 1, "Service Unavailable"},
		{"context deadline -> 503", context.DeadlineExceeded, http.StatusServiceUnavailable, 1, "Service Unavailable"},
		{"not found -> 404", pkg.ErrNotFound, http.StatusNotFound, 0, "Not Found"},
		{"other -> 400", errors.New("bad request detail"), http.StatusBadRequest, 0, "bad request detail"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, text, retry := describeErrorResponse(c.err)
			if code != c.wantCode || retry != c.wantRetry || text != c.wantStatus {
				t.Fatalf("describeErrorResponse(%v) = (%d,%q,%d), want (%d,%q,%d)",
					c.err, code, text, retry, c.wantCode, c.wantStatus, c.wantRetry)
			}
		})
	}
}
