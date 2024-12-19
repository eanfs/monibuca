package debug

import (
	"bytes"

	"m7s.live/v5/plugin/debug/pkg/internal/graph"
	"m7s.live/v5/plugin/debug/pkg/internal/report"
	"m7s.live/v5/plugin/debug/pkg/profile"
)

func GetDotGraph(profile *profile.Profile) (string, error) {
	rpt := report.NewDefault(profile, report.Options{})
	g, config := report.GetDOT(rpt)
	dot := &bytes.Buffer{}
	graph.ComposeDot(dot, g, &graph.DotAttributes{}, config)
	return dot.String(), nil
}
