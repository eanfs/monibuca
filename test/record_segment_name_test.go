package test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m7s.live/v5"
)

// TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite pins that a fixed
// fileName produces one file for the first segment and -001/-002/... for
// fragment rotations. Before the fix every rotation rewrote fixed.mp4 and
// fixed-001.mp4 never appeared.
func TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite(t *testing.T) {
	if os.Getenv(recoveryChildEnv) == "1" {
		cfg := os.Getenv(recoveryConfigEnv)
		if cfg == "" {
			t.Fatalf("%s is not set in child mode", recoveryConfigEnv)
		}
		if err := m7s.Run(context.Background(), cfg); err != nil {
			t.Fatalf("child server exited: %v", err)
		}
		return
	}
	if testing.Short() {
		t.Skip("skipping process-level recording test in -short mode")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg not found in PATH: %v", err)
	}

	root := t.TempDir()
	storageBase := filepath.Join(root, "storage")
	recordDir := filepath.Join(storageBase, "rec")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatalf("create record dir: %v", err)
	}

	s := startRecoveryServer(t, root, storageBase, recordDir, "segment-child.log",
		"^TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite$")
	startFFmpeg(t, root, repoFile(t, "example/default/test.flv"), s.rtmpPort)

	waitFor(t, 30*time.Second, 250*time.Millisecond, "record api accepted", func() error {
		status, body, err := postJSON(fmt.Sprintf("http://127.0.0.1:%d/mp4/api/start/live/test", s.httpPort), map[string]string{
			"filePath": "rec",
			"fileName": "fixed.mp4",
			"fragment": "1s",
			"duration": "0",
			"type":     "mp4",
		})
		if err != nil {
			return err
		}
		if status == http.StatusOK || strings.Contains(body, "record exists") {
			return nil
		}
		return fmt.Errorf("http %d: %s", status, strings.TrimSpace(body))
	})

	basePath := filepath.Join(recordDir, "fixed.mp4")
	secondPath := filepath.Join(recordDir, "fixed-001.mp4")

	waitFor(t, 30*time.Second, 250*time.Millisecond, "first segment file", func() error {
		return nonEmptyFile(basePath)
	})
	waitFor(t, 30*time.Second, 250*time.Millisecond, "second segment file", func() error {
		return nonEmptyFile(secondPath)
	})

	_, _, _ = postJSON(fmt.Sprintf("http://127.0.0.1:%d/mp4/api/stop/live/test", s.httpPort), nil)

	if err := nonEmptyFile(basePath); err != nil {
		t.Fatalf("first segment after rotation: %v", err)
	}
	if err := nonEmptyFile(secondPath); err != nil {
		t.Fatalf("second segment after stop: %v", err)
	}
}

func nonEmptyFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty", filepath.Base(path))
	}
	return nil
}
