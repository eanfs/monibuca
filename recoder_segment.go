package m7s

import (
	"fmt"
	"path/filepath"
	"strings"
)

// segmentRecordPath appends a zero-padded segment number before the extension
// for every segment after the first: rec-1.mp4 -> rec-1-001.mp4.
// Three digits keep the caller's recording-name regex matching for seq 1..999
// (.*rec-\d{4,16}-\d{10,20}(-\d{3}){0,3}$); seq >= 1000 emits a four-digit
// suffix that falls outside those three-digit groups.
func segmentRecordPath(path string, seq int) string {
	if seq <= 0 {
		return path
	}
	ext := filepath.Ext(path)
	return fmt.Sprintf("%s-%03d%s", strings.TrimSuffix(path, ext), seq, ext)
}
