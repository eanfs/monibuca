package plugin_mp4

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// buildCorruptRecoverySidecar 造一个确定性损坏的 sidecar：读取即判定损坏，
// 恢复流程会 markFailed 并计入 stats.Errors。
func buildCorruptRecoverySidecar(t *testing.T, dir, name string) string {
	t.Helper()
	mediaPath := filepath.Join(dir, name)
	if err := os.WriteFile(mediaPath, []byte{0xDE, 0xAD}, 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	sidecar := mediaPath + mp4pkg.IndexExtension
	if err := os.WriteFile(sidecar, []byte("not-an-index"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	ageRecoveryFiles(t, mediaPath, sidecar)
	return sidecar
}

// countCaptured 统计给定消息的日志条数。
func countCaptured(h *captureHandler, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, entry := range h.logs {
		if entry.Msg == msg {
			n++
		}
	}
	return n
}

// 启动扫描过去只在完成日志里记 errors=N，明细全部丢失。每个错误必须单独
// 输出（Error 级，带原始错误），否则现场无法定位是哪个 sidecar/目录失败。
func TestRecoveryScanLogsEachError(t *testing.T) {
	dir := t.TempDir()
	buildCorruptRecoverySidecar(t, dir, "s3writer_bad.tmp")

	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{}
	recovery.Logger = slog.New(logs)

	if _, err := recovery.RunOnce([]string{dir}); err == nil {
		t.Fatal("RunOnce must report the scan error")
	}

	entry := logs.mustFind(t, "index recovery error", slog.LevelError)
	if entry.Attrs["error"] == nil {
		t.Fatalf("per-error log must carry the error: %+v", entry.Attrs)
	}
	if got := entry.Attrs["total"]; got != int64(1) {
		t.Fatalf("total = %v, want 1", got)
	}
}

// 明细日志必须有上限：侧目录塞满坏 sidecar 时不能把启动日志刷爆。
func TestRecoveryScanErrorLogCapped(t *testing.T) {
	dir := t.TempDir()
	const files = recoveryErrorLogLimit + 5
	for i := 0; i < files; i++ {
		buildCorruptRecoverySidecar(t, dir, fmt.Sprintf("s3writer_bad%02d.tmp", i))
	}

	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{}
	recovery.Logger = slog.New(logs)

	if _, err := recovery.RunOnce([]string{dir}); err == nil {
		t.Fatal("RunOnce must report the scan errors")
	}

	if n := countCaptured(logs, "index recovery error"); n != recoveryErrorLogLimit {
		t.Fatalf("logged %d per-error entries, want %d", n, recoveryErrorLogLimit)
	}
	truncated := logs.mustFind(t, "index recovery errors truncated", slog.LevelError)
	if got := truncated.Attrs["total"]; got != int64(files) {
		t.Fatalf("truncated total = %v, want %d", got, files)
	}
}
