package plugin_mp4

import (
	"strings"
	"testing"
	"time"
)

func TestRecordingIOWarnings(t *testing.T) {
	if got := recordingIOWarnings(3*time.Second, "s3"); len(got) != 0 {
		t.Fatalf("tuned config returned warnings: %v", got)
	}
	for _, storageKey := range []string{"s3", "oss", "cos"} {
		got := strings.Join(recordingIOWarnings(0, storageKey), "\n")
		if !strings.Contains(got, "publish.buffertime") {
			t.Fatalf("%s with 0 buffer must warn about publish.buffertime, got %q", storageKey, got)
		}
	}
	if got := recordingIOWarnings(0, "local"); len(got) != 0 {
		t.Fatalf("local storage must not warn, got %v", got)
	}
	if got := recordingIOWarnings(0, ""); len(got) != 0 {
		t.Fatalf("unknown storage must not warn, got %v", got)
	}
}
