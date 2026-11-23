package plugin_mp4

import (
	"fmt"
	"time"
)

// recordingIOWarnings flags an untuned publisher ring at MP4 plugin start when
// recordings target object storage. The ring is created by the publisher from
// config.Publish (pkg/track.go) and shared by every subscriber, so the recorder
// cannot enlarge it on its own: warn, do not rewrite configuration.
func recordingIOWarnings(publishBuffer time.Duration, storageKey string) []string {
	if storageKey == "" || storageKey == "local" || publishBuffer > 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"record with %s storage and publish.buffertime=%s: the shared publisher ring holds ~1 GOP, so a short disk stall forces reader jumps/discards; consider global.publish.buffertime: 3s-5s",
		storageKey, publishBuffer)}
}

// warnRecordingIOConfig logs recordingIOWarnings through the plugin logger.
// Start calls it once so the warning lands in the plugin's own log route.
func (p *MP4Plugin) warnRecordingIOConfig(storageType string) {
	for _, warning := range recordingIOWarnings(p.GetCommonConf().Publish.BufferTime, storageType) {
		p.Warn("recording IO configuration", "warning", warning)
	}
}
