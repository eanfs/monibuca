package m7s

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	. "m7s.live/v5/pkg"
)

// TestPrometheusCollectOneSidedTracks 防止视频-only / 音频-only 流在
// Prometheus 抓取时解引用另一侧为 nil 的 AVTracks.AVTrack 导致进程 panic。
func TestPrometheusCollectOneSidedTracks(t *testing.T) {
	s := &Server{}
	s.prometheusDesc.init()

	videoOnly := &Publisher{}
	videoOnly.StreamPath = "live/video-only"
	videoOnly.Plugin = &Plugin{Meta: &PluginMeta{Name: "test"}}
	videoOnly.VideoTrack.AVTrack = &AVTrack{}
	s.Streams.Collection.Add(videoOnly)

	audioOnly := &Publisher{}
	audioOnly.StreamPath = "live/audio-only"
	audioOnly.Plugin = &Plugin{Meta: &PluginMeta{Name: "test"}}
	audioOnly.AudioTrack.AVTrack = &AVTrack{}
	s.Streams.Collection.Add(audioOnly)

	ch := make(chan prometheus.Metric, 8)
	s.Collect(ch)
	if got := len(ch); got != 4 {
		t.Fatalf("collected %d metrics, want 4 (video-only 2 + audio-only 2)", got)
	}
}
