package plugin_mp4

import (
	"testing"

	"m7s.live/v5/pkg/config"
)

// TestRecoveryConfigKeysBindToFields 钉住"配置键 → 结构体字段"的绑定。
//
// m7s 的配置解析把键归一化（删 -/_ 后小写）再与字段名比较，因此字段
// RequiredDirs 的默认键是 "requireddirs"，而文档/模板/现场写的是
// "requiredirs"——两者不相等时配置被静默忽略（2026-11-09 51 负向验证：
// requiredirs: true 完全不生效，容器照常启动）。必须用 yaml tag 显式绑定。
func TestRecoveryConfigKeysBindToFields(t *testing.T) {
	cases := []struct {
		name string
		conf map[string]any
		get  func(*MP4Plugin) any
		want any
	}{
		{
			name: "requiredirs binds to RequiredDirs",
			conf: map[string]any{"record": map[string]any{"recovery": map[string]any{"requiredirs": true}}},
			get:  func(p *MP4Plugin) any { return p.RecordConfig.Recovery.RequiredDirs },
			want: true,
		},
		{
			name: "dirs binds to Dirs",
			conf: map[string]any{"record": map[string]any{"recovery": map[string]any{"dirs": []any{"/tmp/a"}}}},
			get:  func(p *MP4Plugin) any { return len(p.RecordConfig.Recovery.Dirs) },
			want: 1,
		},
		{
			name: "onstart binds to OnStart",
			conf: map[string]any{"record": map[string]any{"recovery": map[string]any{"onstart": false}}},
			get:  func(p *MP4Plugin) any { return p.RecordConfig.Recovery.OnStart },
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &MP4Plugin{}
			config.Parse(p, c.conf)
			if got := c.get(p); got != c.want {
				t.Fatalf("config key not bound: got %v, want %v (conf=%v)", got, c.want, c.conf)
			}
		})
	}
}
