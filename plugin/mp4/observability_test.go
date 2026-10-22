package plugin_mp4

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// capturedLog 是一条被测试 handler 捕获的结构化日志。
type capturedLog struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

type captureHandler struct {
	mu   sync.Mutex
	logs []capturedLog
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, capturedLog{Level: r.Level, Msg: r.Message, Attrs: attrs})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) mustFind(t *testing.T, msg string, level slog.Level) capturedLog {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var found []capturedLog
	for _, entry := range h.logs {
		if entry.Msg == msg {
			found = append(found, entry)
		}
	}
	if len(found) == 0 {
		t.Fatalf("log %q not found; captured=%+v", msg, h.logs)
	}
	for _, entry := range found {
		if entry.Level == level {
			return entry
		}
	}
	t.Fatalf("log %q has no entry at level %v; captured=%+v", msg, level, found)
	return capturedLog{}
}

func requireRecoveryAttr(t *testing.T, entry capturedLog, key string) any {
	t.Helper()
	v, ok := entry.Attrs[key]
	if !ok {
		t.Fatalf("log %q missing field %q; fields=%v", entry.Msg, key, entry.Attrs)
	}
	return v
}

func requireRecoveryInt(t *testing.T, entry capturedLog, key string) int64 {
	t.Helper()
	switch v := requireRecoveryAttr(t, entry, key).(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case uint:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		return int64(v)
	case float64:
		return int64(v)
	default:
		t.Fatalf("log %q field %q is not numeric: %T", entry.Msg, key, v)
		return 0
	}
}

func newObservedRecoveryTask(h *captureHandler) *IndexRecoveryTask {
	t := &IndexRecoveryTask{
		// 注入 enqueue，避免测试触达真实上传队列。
		EnqueueUpload: func(RecoveryItem) error { return nil },
	}
	t.Logger = slog.New(h)
	return t
}

// —— 事件 6：恢复扫描汇总 ——

func TestRecoveryScanSummaryLogFields(t *testing.T) {
	h := &captureHandler{}
	dir := t.TempDir()
	_, sidecar, _ := buildRecoveryFixture(t, dir, "rec.mp4", []byte("0123456789"))
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("fixture sidecar missing: %v", err)
	}

	task := newObservedRecoveryTask(h)
	stats, err := task.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("recovered = %d, want 1", stats.Recovered)
	}

	entry := h.mustFind(t, "index recovery completed", slog.LevelInfo)
	if got := requireRecoveryInt(t, entry, "scanned"); got != 1 {
		t.Fatalf("scanned = %d, want 1", got)
	}
	if got := requireRecoveryInt(t, entry, "recovered"); got != 1 {
		t.Fatalf("recovered = %d, want 1", got)
	}
	if got := requireRecoveryInt(t, entry, "failed"); got != 0 {
		t.Fatalf("failed = %d, want 0", got)
	}
	if got := requireRecoveryInt(t, entry, "elapsedMs"); got < 0 {
		t.Fatalf("elapsedMs = %d, want >= 0", got)
	}

	// 单文件结果：必须带 file 与 elapsedMs。
	result := h.mustFind(t, "recovery result", slog.LevelInfo)
	if got := fmt.Sprint(requireRecoveryAttr(t, result, "file")); got == "" {
		t.Fatal("recovery result missing non-empty file")
	}
	if got := fmt.Sprint(requireRecoveryAttr(t, result, "outcome")); got != "recovered" {
		t.Fatalf("outcome = %q, want recovered", got)
	}
	if got := requireRecoveryInt(t, result, "elapsedMs"); got < 0 {
		t.Fatalf("elapsedMs = %d, want >= 0", got)
	}
}

// —— 事件 6：单个恢复失败 ——

func TestRecoveryFailureResultLogFields(t *testing.T) {
	h := &captureHandler{}
	dir := t.TempDir()
	sidecar := filepath.Join(dir, "broken.mp4.m7sidx")
	if err := os.WriteFile(sidecar, []byte("not-an-index"), 0o644); err != nil {
		t.Fatalf("write corrupt sidecar: %v", err)
	}
	ageRecoveryFiles(t, sidecar)

	task := newObservedRecoveryTask(h)
	stats, err := task.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("expected recovery error for corrupt sidecar")
	}
	if stats.Failed != 1 {
		t.Fatalf("failed = %d, want 1", stats.Failed)
	}

	summary := h.mustFind(t, "index recovery completed", slog.LevelInfo)
	if got := requireRecoveryInt(t, summary, "failed"); got != 1 {
		t.Fatalf("summary failed = %d, want 1", got)
	}

	result := h.mustFind(t, "recovery result", slog.LevelError)
	if got := fmt.Sprint(requireRecoveryAttr(t, result, "outcome")); got != "failed" {
		t.Fatalf("outcome = %q, want failed", got)
	}
	if got := fmt.Sprint(requireRecoveryAttr(t, result, "file")); got == "" {
		t.Fatal("recovery failure missing non-empty file")
	}
	if _, ok := requireRecoveryAttr(t, result, "error").(error); !ok {
		t.Fatalf("recovery failure error field = %T, want error", requireRecoveryAttr(t, result, "error"))
	}
	if got := requireRecoveryInt(t, result, "elapsedMs"); got < 0 {
		t.Fatalf("elapsedMs = %d, want >= 0", got)
	}
}
