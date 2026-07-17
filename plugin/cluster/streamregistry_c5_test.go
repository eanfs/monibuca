//go:build cluster

package plugin_cluster

import (
	"testing"

	m7s "m7s.live/v5"
)

// C5 回归(2026-07-16):relay 路由常驻,分类必须同时要求 pub.Type == "pull",
// 否则入站推流撞上曾 relay 过的路径会跳过 first-write-wins → 脑裂。
// 完整根因与设计见 streamregistry.go OnPublish 处的注释。
// 真机对照:同一路径 relay 前推流被停(exit=224),relay 后推满(exit=0,漏防)。
func TestStreamRegistry_OnPublish_RelayClassification(t *testing.T) {
	for _, c := range []struct {
		name         string
		activeRelays map[string]struct{}
		pubType      string
		streamPath   string
		wantAcquire  bool
	}{
		{"入站推流撞 relay 路由:仍须 acquire(C5 修复核心)",
			map[string]struct{}{"live/x": {}}, m7s.PublishTypeServer, "live/x", true},
		{"relay 本体(pull+路由命中):跳过,环回防护不回退",
			map[string]struct{}{"live/x": {}}, m7s.PublishTypePull, "live/x", false},
		{"普通本地 pull(路由未命中):照常注册",
			map[string]struct{}{"live/other": {}}, m7s.PublishTypePull, "live/camera1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			sr := &StreamRegistry{
				localStreams: make(map[string]struct{}),
				plugin:       &ClusterPlugin{activeRelays: c.activeRelays},
			}
			var acquired string
			sr.acquireFn = func(p string) error { acquired = p; return nil }
			sr.dispatch = func(fn func()) { fn() } // 同步执行,便于断言

			pub := &m7s.Publisher{}
			pub.Type = c.pubType
			pub.StreamPath = c.streamPath
			sr.OnPublish(pub)

			if c.wantAcquire {
				if acquired != c.streamPath {
					t.Fatalf("必须走 KV acquire(first-write-wins),实际 acquired=%q", acquired)
				}
				return
			}
			if acquired != "" {
				t.Fatalf("relay 派生不得 acquire(环回防护),却对 %q 发起了", acquired)
			}
			if len(sr.localStreams) != 0 {
				t.Fatalf("relay 派生不得进 localStreams,got %v", sr.localStreams)
			}
		})
	}
}
