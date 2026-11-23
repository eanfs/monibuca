package mp4

import (
	"sync/atomic"
	"time"
)

// RecordConfig 是录制收尾策略的运行时配置。
//
// 配置位于插件的 record 段（yaml: mp4.record），但录制器在 pkg 包内，无法直接
// 读取插件配置。插件 Start 时通过 SetRecordConfig 注入，录制路径只读这里的值。
// 与 IndexConfig 相同的注入方式。
type RecordConfig struct {
	// MinDuration 大于 0 时，收尾阶段实际时长低于该值的片段会被整段丢弃：
	// 不上传对象存储、不写 record_streams/RecordEvent、删除本地媒体与 sidecar。
	// 默认 0 表示不限制（保持既有行为）。
	//
	// 现场背景：磁盘高 IO 时 ErrDiscard 会触发原地重录制，产生大量 <1s 的
	// 碎片段。每段都会上传对象并写库，污染录像列表、放大对象存储，还会挤占
	// 上传队列触发 backlog full → pending_uploads 溢出。
	MinDuration time.Duration
}

var recordConfig atomic.Pointer[RecordConfig]

func init() {
	recordConfig.Store(&RecordConfig{})
}

// SetRecordConfig 注入录制收尾配置。零值即「不丢弃任何片段」。
func SetRecordConfig(cfg RecordConfig) {
	recordConfig.Store(&cfg)
}

// GetRecordConfig 返回当前录制收尾配置（始终返回有效值）。
func GetRecordConfig() RecordConfig {
	if cfg := recordConfig.Load(); cfg != nil {
		return *cfg
	}
	return RecordConfig{}
}
