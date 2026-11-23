package storage

import (
	"bytes"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"m7s.live/v5/pkg/config"
)

func TestIOWarnings(t *testing.T) {
	tests := []struct {
		name string
		cfg  UploadConfig
		want []string // one substring per expected warning
	}{
		{
			name: "fully tuned",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, UploadReadRateMBps: 128, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
		},
		{
			name: "zero read rate",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
			want: []string{"readratembps"},
		},
		{
			name: "zero trailer rate",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, UploadReadRateMBps: 128, MaxQueuedUploads: 8},
			want: []string{"trailerwriteratembps"},
		},
		{
			name: "unbounded queue",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, UploadReadRateMBps: 128, TrailerWriteRateMBps: 64},
			want: []string{"maxqueueduploads"},
		},
		{
			name: "too many upload slots",
			cfg:  UploadConfig{MaxConcurrentUploads: 6, UploadReadRateMBps: 128, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
			want: []string{"maxconcurrentuploads"},
		},
		{
			name: "five upload slots warns",
			cfg:  UploadConfig{MaxConcurrentUploads: 5, UploadReadRateMBps: 128, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
			want: []string{"maxconcurrentuploads=5"},
		},
		{
			name: "negative read rate reports the value",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, UploadReadRateMBps: -1, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
			want: []string{"readratembps=-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := IOWarnings(tt.cfg)
			if len(warnings) != len(tt.want) {
				t.Fatalf("got %d warnings %q, want %d", len(warnings), warnings, len(tt.want))
			}
			got := strings.Join(warnings, "\n")
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("warnings %q do not contain %q", got, want)
				}
			}
		})
	}
}

func TestUploadConfigMaxQueuedDefault(t *testing.T) {
	var cfg UploadConfig
	config.Parse(&cfg, map[string]any{})
	if cfg.MaxQueuedUploads != 8 {
		t.Fatalf("default MaxQueuedUploads = %d, want 8 from the default tag", cfg.MaxQueuedUploads)
	}
	if cfg.UploadReadRateMBps != 0 {
		t.Fatalf("default UploadReadRateMBps = %d, want 0", cfg.UploadReadRateMBps)
	}
}

// TestUploadConfigIOWarningsAreLogged pins the wiring in InitUploadManager:
// testing IOWarnings alone would stay green if the call site were deleted
// (the same trap as the cluster relay pull-now regression).
func TestUploadConfigIOWarningsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	InitUploadManager(UploadConfig{PendingDir: filepath.Join(t.TempDir(), "pending")})

	logged := buf.String()
	for _, want := range []string{
		"WARNING: upload.readratembps=0",
		"WARNING: upload.trailerwriteratembps=0",
		"WARNING: upload.maxqueueduploads=0",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("InitUploadManager log missing %q; got:\n%s", want, logged)
		}
	}
	// MaxConcurrentUploads=0 falls back to 4 in InitUploadManager, so the
	// effective slot count is fine and must not produce a warning.
	if strings.Contains(logged, "maxconcurrentuploads") {
		t.Fatalf("default slot count should not warn; got:\n%s", logged)
	}
}
