package mp4

import (
	"sync/atomic"
	"time"
)

// IndexConfig 是伴随索引的运行时配置。
//
// 配置位于插件的 record.index 段，但录制器(Recorder)在 pkg 包内，无法直接
// 读取插件配置。插件 Start 时通过 SetIndexConfig 注入，录制路径只读这里的值。
type IndexConfig struct {
	// Enable 为 false 时不再创建/追加 sidecar（恢复能力随之关闭）。
	Enable bool
	// FlushInterval 是索引落盘节流间隔；<=0 时回退到默认 1s。
	FlushInterval time.Duration
}

var (
	defaultIndexConfig = IndexConfig{Enable: true, FlushInterval: indexFlushInterval}
	indexConfig        atomic.Pointer[IndexConfig]
)

func init() {
	indexConfig.Store(&defaultIndexConfig)
}

// SetIndexConfig 注入索引配置。未设置的字段回退到默认值。
// 注意：只影响之后创建的索引写入器；已打开的写句柄保持原间隔。
func SetIndexConfig(cfg IndexConfig) {
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = indexFlushInterval
	}
	indexConfig.Store(&cfg)
}

// GetIndexConfig 返回当前索引配置（始终返回有效值）。
func GetIndexConfig() IndexConfig {
	if cfg := indexConfig.Load(); cfg != nil {
		return *cfg
	}
	return defaultIndexConfig
}
