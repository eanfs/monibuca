package plugin_mp4

import (
	"bytes"
	"log/slog"
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
	// Negative values are also untuned, and the message must report the
	// configured value instead of a hardcoded 0.
	if got := strings.Join(recordingIOWarnings(-time.Second, "s3"), "\n"); !strings.Contains(got, "publish.buffertime=-1s") {
		t.Fatalf("negative buffer must report publish.buffertime=-1s, got %q", got)
	}
	if got := recordingIOWarnings(0, "local"); len(got) != 0 {
		t.Fatalf("local storage must not warn, got %v", got)
	}
	if got := recordingIOWarnings(0, ""); len(got) != 0 {
		t.Fatalf("unknown storage must not warn, got %v", got)
	}
}

// The guardrail only works if MP4Plugin.Start actually routes the warnings
// through the plugin logger; this pins that call site.
func TestRecordingIOConfigWarningIsLogged(t *testing.T) {
	var buf bytes.Buffer
	p := &MP4Plugin{}
	p.Logger = slog.New(slog.NewTextHandler(&buf, nil))

	p.warnRecordingIOConfig("s3") // publish.buffertime defaults to 0

	out := buf.String()
	if !strings.Contains(out, "recording IO configuration") {
		t.Fatalf("expected the plugin to log the recording IO warning; logs = %q", out)
	}
	if !strings.Contains(out, "publish.buffertime=0s") {
		t.Fatalf("warning must report the configured buffer value; logs = %q", out)
	}

	buf.Reset()
	p.warnRecordingIOConfig("local")
	if buf.Len() != 0 {
		t.Fatalf("local storage must not log anything; logs = %q", buf.String())
	}
}
