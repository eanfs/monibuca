package m7s

import (
	"fmt"
	"testing"

	task "github.com/eanfs/gotask"
	pkg "m7s.live/v5/pkg"
)

func TestShouldStopPullJobOnPublishStop(t *testing.T) {
	cases := []struct {
		name     string
		reason   error
		maxRetry int
		want     bool
	}{
		{"idle timeout stops pull", pkg.ErrPublishIdleTimeout, -1, true},
		{"wrapped idle timeout stops pull", fmt.Errorf("outer: %w", pkg.ErrPublishIdleTimeout), -1, true},
		{"delay close timeout stops pull", pkg.ErrPublishDelayCloseTimeout, -1, true},
		{"stop by user stops pull", task.ErrStopByUser, -1, true},
		{"zero max retry stops pull", pkg.ErrPublishTimeout, 0, true},
		{"network failure with retry keeps pulling", pkg.ErrPublishTimeout, -1, false},
		{"network failure with remaining retries keeps pulling", pkg.ErrPublishTimeout, 3, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldStopPullJobOnPublishStop(c.reason, c.maxRetry); got != c.want {
				t.Fatalf("shouldStopPullJobOnPublishStop(%v, %d) = %v, want %v", c.reason, c.maxRetry, got, c.want)
			}
		})
	}
}
