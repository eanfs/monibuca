package m7s

import (
	"testing"

	"m7s.live/v5/pkg/config"
)

// TestUploadReadRateConfigKeyBinding pins "global.upload.readratembps" to
// ServerConfig.Upload.UploadReadRateMBps. The parser silently ignores unknown
// keys, so losing the yaml tag would turn the documented knob into a no-op.
func TestUploadReadRateConfigKeyBinding(t *testing.T) {
	s := &Server{}
	var c config.Config
	c.Parse(&s.ServerConfig, "GLOBAL")
	c.ParseUserFile(map[string]any{"upload": map[string]any{"readratembps": 128}})
	if s.Upload.UploadReadRateMBps != 128 {
		t.Fatalf("upload.readratembps = %d, want 128", s.Upload.UploadReadRateMBps)
	}
}

// The field-derived spelling must NOT bind: the documented key is readratembps.
func TestUploadReadRateConfigKeyFieldNameIgnored(t *testing.T) {
	s := &Server{}
	var c config.Config
	c.Parse(&s.ServerConfig, "GLOBAL")
	c.ParseUserFile(map[string]any{"upload": map[string]any{"uploadreadratembps": 32}})
	if s.Upload.UploadReadRateMBps == 32 {
		t.Fatal("uploadreadratembps must not bind; the yaml tag defines readratembps")
	}
}
