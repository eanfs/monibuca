package m7s

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// retryLogCapture 收集 slog.Record，供补传日志字段断言使用。
type retryLogCapture struct {
	mu   sync.Mutex
	logs []retryCapturedLog
}

type retryCapturedLog struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

func (h *retryLogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *retryLogCapture) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, retryCapturedLog{Level: r.Level, Msg: r.Message, Attrs: attrs})
	return nil
}

func (h *retryLogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *retryLogCapture) WithGroup(string) slog.Handler      { return h }

func (h *retryLogCapture) find(msg string) (retryCapturedLog, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.logs {
		if entry.Msg == msg {
			return entry, true
		}
	}
	return retryCapturedLog{}, false
}

// failingUploadStorage 让 UploadLocalFile 在 CreateFile 阶段稳定失败。
type failingUploadStorage struct {
	runtimeTestStorage
	err error
}

func (s *failingUploadStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, s.err
}

// 补传失败日志必须与队列首次失败使用同一检索契约：Error 级、error 字段、
// streamPath；否则源站长时间中断后现场按 Error 检索会完全查不到。
func TestRetryUploadExhaustedLogFields(t *testing.T) {
	db := newUploadTestDB(t)
	// 重试耗尽路径会写告警表；先迁移以免测试输出被 sqlite 噪声淹没。
	if err := db.AutoMigrate(&AlarmInfo{}); err != nil {
		t.Fatalf("migrate AlarmInfo: %v", err)
	}
	localPath := filepath.Join(t.TempDir(), "pending.mp4")
	if err := os.WriteFile(localPath, []byte("pending upload"), 0600); err != nil {
		t.Fatalf("write pending file: %v", err)
	}
	upload := UploadTask{
		LocalPath:   localPath,
		ObjectKey:   "records/pending.mp4",
		StreamPath:  "live/cam1",
		StorageType: string(storage.StorageTypeLocal),
		Status:      UploadStatusFailed,
		RetryCount:  9,
		MaxRetries:  10,
	}
	if err := db.Create(&upload).Error; err != nil {
		t.Fatalf("seed upload task: %v", err)
	}

	active := &failingUploadStorage{
		runtimeTestStorage: runtimeTestStorage{key: string(storage.StorageTypeLocal)},
		err:                errors.New("connection reset"),
	}
	server := &Server{}
	server.DB = db
	server.activateStorage(active, StorageStatus{ActiveType: string(storage.StorageTypeLocal)})

	h := &retryLogCapture{}
	scheduler := &UploadRetryScheduler{s: server}
	scheduler.Logger = slog.New(h)

	scheduler.retryUpload(upload)

	// 重试进行中也要带 streamPath，便于把三个阶段串成同一条录像。
	inProgress, ok := h.find("retrying upload")
	if !ok {
		t.Fatalf("retrying upload log missing; captured=%+v", h.logs)
	}
	if got := inProgress.Attrs["streamPath"]; got != "live/cam1" {
		t.Fatalf("retrying upload streamPath = %v, want live/cam1", got)
	}

	entry, ok := h.find("retry upload failed")
	if !ok {
		t.Fatalf("retry upload failed log missing; captured=%+v", h.logs)
	}
	if entry.Level != slog.LevelError {
		t.Fatalf("retry upload failed level = %v, want Error", entry.Level)
	}
	if got := entry.Attrs["streamPath"]; got != "live/cam1" {
		t.Fatalf("streamPath = %v, want live/cam1", got)
	}
	errAttr, ok := entry.Attrs["error"].(error)
	if !ok {
		t.Fatalf("error field must be an error, got %T", entry.Attrs["error"])
	}
	if !errors.Is(errAttr, active.err) {
		t.Fatalf("error = %v, want to wrap %v", errAttr, active.err)
	}
	if _, exists := entry.Attrs["err"]; exists {
		t.Fatalf("legacy err field must not remain beside error: %v", entry.Attrs)
	}
}
