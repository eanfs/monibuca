package debug

import (
	"bytes"

	"m7s.live/v5/plugin/debug/pkg/internal/graph"
	"m7s.live/v5/plugin/debug/pkg/internal/report"
	"m7s.live/v5/plugin/debug/pkg/profile"
)

func GetDotGraph(profile *profile.Profile) (string, error) {
	// 设置节点数量限制，使图形更简洁（类似官方 pprof）
	options := report.Options{
		NodeCount:    80,    // 限制最大节点数
		NodeFraction: 0.005, // 过滤掉小于 0.5% 的节点
	}
	rpt := report.NewDefault(profile, options)
	g, config := report.GetDOT(rpt)
	dot := &bytes.Buffer{}
	graph.ComposeDot(dot, g, &graph.DotAttributes{}, config)
	return dot.String(), nil
}
