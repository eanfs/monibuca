package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 事件 5：pending 水位日志的字段来源（count/bytes/limit）必须可测试且与容量
// 检查使用同一套阈值。
func TestGetPendingUsageWatermarkSize(t *testing.T) {
	dir := t.TempDir()
	InitUploadManager(UploadConfig{PendingDir: dir, PendingMaxSizeMB: 1})

	if err := os.WriteFile(filepath.Join(dir, "a.mp4"), make([]byte, 900000), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}

	usage, err := GetPendingUsage()
	if err != nil {
		t.Fatalf("GetPendingUsage: %v", err)
	}
	if usage.Bytes != 900000 || usage.Count != 1 {
		t.Fatalf("usage = %+v, want bytes=900000 count=1", usage)
	}
	if usage.SizeLimit != 1024*1024 {
		t.Fatalf("SizeLimit = %d, want %d", usage.SizeLimit, 1024*1024)
	}
	exceeded, limit := usage.WatermarkExceeded()
	if !exceeded || limit != 1024*1024 {
		t.Fatalf("WatermarkExceeded = (%v,%d), want (true,%d)", exceeded, limit, 1024*1024)
	}
}

func TestGetPendingUsageWatermarkFiles(t *testing.T) {
	dir := t.TempDir()
	InitUploadManager(UploadConfig{PendingDir: dir, PendingMaxFiles: 2})

	for _, name := range []string{"a.mp4", "b.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write seed file: %v", err)
		}
	}

	usage, err := GetPendingUsage()
	if err != nil {
		t.Fatalf("GetPendingUsage: %v", err)
	}
	exceeded, limit := usage.WatermarkExceeded()
	if !exceeded || limit != 2 {
		t.Fatalf("WatermarkExceeded = (%v,%d), want (true,2)", exceeded, limit)
	}
}

func TestPendingUsageWatermarkUnconfigured(t *testing.T) {
	dir := t.TempDir()
	InitUploadManager(UploadConfig{PendingDir: dir})
	if err := os.WriteFile(filepath.Join(dir, "a.mp4"), make([]byte, 1024), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}

	usage, err := GetPendingUsage()
	if err != nil {
		t.Fatalf("GetPendingUsage: %v", err)
	}
	if exceeded, limit := usage.WatermarkExceeded(); exceeded || limit != 0 {
		t.Fatalf("WatermarkExceeded = (%v,%d), want (false,0)", exceeded, limit)
	}
}
