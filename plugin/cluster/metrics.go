//go:build cluster

package plugin_cluster

import (
	"runtime"
	"time"

	task "github.com/langhuihui/gotask"
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
// Server.Streams.Length 是 pkg/util.Collection 的 int 字段(非方法)。
func (r *LoadReporter) collectMetrics() map[string]any {
	m := map[string]any{
		"goroutines": runtime.NumGoroutine(),
	}
	if r.plugin != nil && r.plugin.Server != nil {
		m["streams"] = r.plugin.Server.Streams.Length
	}
	return m
}
