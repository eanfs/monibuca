//go:build cluster

package plugin_cluster

import (
	"runtime"
	"time"

	task "github.com/eanfs/gotask"
	m7s "m7s.live/v5"
)

// LoadReporter 周期把本节点指标写到 m7s/nodes/<self> 的 Metrics 字段。
// task.TickTask 实现,interval 由 ClusterPlugin.Metrics.ReportInterval 控制。
//
// v1 范围指标: streams(本节点 publisher 数), goroutines(runtime.NumGoroutine)。
// CPU / 带宽 / subscribers 留 v2(避免引入 system metrics 依赖)。
type LoadReporter struct {
	task.TickTask
	plugin *ClusterPlugin
}

func (r *LoadReporter) GetTickInterval() time.Duration {
	if r.plugin == nil || r.plugin.Metrics.ReportInterval <= 0 {
		return 5 * time.Second
	}
	return r.plugin.Metrics.ReportInterval
}

func (r *LoadReporter) Tick(_ any) {
	if r.plugin == nil || r.plugin.membership == nil {
		return
	}
	if err := r.plugin.membership.UpdateMetrics(r.collectMetrics()); err != nil {
		r.Warn("metrics report failed", "error", err)
	}
}

// collectMetrics 采集 v1 范围指标。
func (r *LoadReporter) collectMetrics() map[string]any {
	m := map[string]any{
		"goroutines": runtime.NumGoroutine(),
	}
	if r.plugin != nil && r.plugin.Server != nil {
		// Streams.Length 在 Streams 事件循环上无锁自增减,跨 goroutine 直接读是
		// 数据竞争;经 SafeRange(在事件循环内执行)计数。本方法跑在 LoadReporter
		// 自己的 TickTask goroutine 上、不在 Streams 循环内,无重入死锁风险。
		streams := 0
		r.plugin.Server.Streams.SafeRange(func(*m7s.Publisher) bool {
			streams++
			return true
		})
		m["streams"] = streams
	}
	return m
}
