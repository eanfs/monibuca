package mp4

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"m7s.live/v5/pkg/storage"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

// capturedLog 是一条被测试 handler 捕获的结构化日志。
type capturedLog struct {
	Level slog.Level
	Msg   string
	Attrs map[string]any
}

// captureHandler 把 slog.Record 收集到内存，便于按字段断言。
// 忽略 WithAttrs/WithGroup：测试只关心事件本身的字段。
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

func (h *captureHandler) find(msg string) (capturedLog, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.logs {
		if entry.Msg == msg {
			return entry, true
		}
	}
	return capturedLog{}, false
}

func (h *captureHandler) mustFind(t *testing.T, msg string, level slog.Level) capturedLog {
	t.Helper()
	entry, ok := h.find(msg)
	if !ok {
		t.Fatalf("log %q not found; captured=%+v", msg, h.logs)
	}
	if entry.Level != level {
		t.Fatalf("log %q level = %v, want %v", msg, entry.Level, level)
	}
	return entry
}

func requireAttr(t *testing.T, entry capturedLog, key string) any {
	t.Helper()
	v, ok := entry.Attrs[key]
	if !ok {
		t.Fatalf("log %q missing field %q; fields=%v", entry.Msg, key, entry.Attrs)
	}
	return v
}

func requireStringAttr(t *testing.T, entry capturedLog, key, want string) {
	t.Helper()
	if got := fmt.Sprint(requireAttr(t, entry, key)); got != want {
		t.Fatalf("log %q field %q = %q, want %q", entry.Msg, key, got, want)
	}
}

func requireIntAttr(t *testing.T, entry capturedLog, key string) int64 {
	t.Helper()
	switch v := requireAttr(t, entry, key).(type) {
	case int:
		return int64(v)
	case int32:
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

// —— 事件 1：录制开始 ——

func TestRecordStartLogFields(t *testing.T) {
	h := &captureHandler{}
	r := &Recorder{}
	r.Logger = slog.New(h)
	r.Event.StreamPath = "live/cam1"
	r.Event.FilePath = "records/live/cam1/rec.mp4"
	r.Event.Type = "mp4"
	r.Event.StorageType = "s3"
	r.Event.VideoCodec = "avc1.64001f"

	r.logRecordStart(nil)

	entry := h.mustFind(t, "record started", slog.LevelInfo)
	requireStringAttr(t, entry, "streamPath", "live/cam1")
	requireStringAttr(t, entry, "file", "records/live/cam1/rec.mp4")
	requireStringAttr(t, entry, "type", "mp4")
	requireStringAttr(t, entry, "storageType", "s3")
	requireStringAttr(t, entry, "codec", "avc1.64001f")
}

func TestRecordStartAudioOnlyCodec(t *testing.T) {
	h := &captureHandler{}
	r := &Recorder{}
	r.Logger = slog.New(h)
	r.Event.StreamPath = "live/radio"
	r.Event.FilePath = "records/live/radio/rec.mp4"
	r.Event.AudioCodec = "mp4a.40.2"

	r.logRecordStart(nil)

	entry := h.mustFind(t, "record started", slog.LevelInfo)
	requireStringAttr(t, entry, "codec", "mp4a.40.2")
}

// —— 事件 2：录制结束 ——

func TestRecordStopLogFields(t *testing.T) {
	h := &captureHandler{}
	muxer := NewMuxer(0)
	video := muxer.AddTrack(box.MP4_CODEC_H264)
	video.Samplelist = []box.Sample{{}, {}, {}}
	audio := muxer.AddTrack(box.MP4_CODEC_AAC)
	audio.Samplelist = []box.Sample{{}, {}}

	trailer := &writeTrailerTask{
		muxer:      muxer,
		streamPath: "live/cam1",
		filePath:   "records/live/cam1/rec.mp4",
		durationMs: 4500,
		startedAt:  time.Now().Add(-2 * time.Second),
	}
	trailer.Logger = slog.New(h)

	trailer.logRecordStop(987654)

	entry := h.mustFind(t, "record stopped", slog.LevelInfo)
	requireStringAttr(t, entry, "streamPath", "live/cam1")
	requireStringAttr(t, entry, "file", "records/live/cam1/rec.mp4")
	if got := requireIntAttr(t, entry, "durationMs"); got != 4500 {
		t.Fatalf("durationMs = %d, want 4500", got)
	}
	if got := requireIntAttr(t, entry, "size"); got != 987654 {
		t.Fatalf("size = %d, want 987654", got)
	}
	if got := requireIntAttr(t, entry, "samples"); got != 5 {
		t.Fatalf("samples = %d, want 5", got)
	}
	requireStringAttr(t, entry, "samplesSource", "samplelist")
	if got := requireIntAttr(t, entry, "tracks"); got != 2 {
		t.Fatalf("tracks = %d, want 2", got)
	}
	if got := requireIntAttr(t, entry, "elapsedMs"); got < 1500 {
		t.Fatalf("elapsedMs = %d, want >= 1500", got)
	}
}

// fMP4(fragment) 模式下 Samplelist 只保留尚未写出的当前一帧，
// record stopped 必须报告累计样本数，而不是恒等于轨道数的假值。
func TestRecordStopLogFragmentSamplesAccumulated(t *testing.T) {
	h := &captureHandler{}
	muxer := NewMuxer(FLAG_FRAGMENT)
	video := muxer.AddTrack(box.MP4_CODEC_H264)
	audio := muxer.AddTrack(box.MP4_CODEC_AAC)

	const videoFrames = 120
	const audioFrames = 117
	for i := 0; i < videoFrames; i++ {
		muxer.CreateFlagment(video, box.Sample{Timestamp: uint32(i * 40), KeyFrame: i == 0})
	}
	for i := 0; i < audioFrames; i++ {
		muxer.CreateFlagment(audio, box.Sample{Timestamp: uint32(i * 41)})
	}

	// CreateFlagment 每次写出的是上一帧，两个轨道各还有 1 帧缓冲在 Samplelist。
	if len(video.Samplelist) != 1 || len(audio.Samplelist) != 1 {
		t.Fatalf("fragment Samplelist should retain one buffered sample each, got video=%d audio=%d",
			len(video.Samplelist), len(audio.Samplelist))
	}

	trailer := &writeTrailerTask{
		muxer:      muxer,
		streamPath: "live/cam1",
		filePath:   "records/live/cam1/rec.mp4",
		durationMs: 4800,
	}
	trailer.Logger = slog.New(h)

	trailer.logRecordStop(123456)

	entry := h.mustFind(t, "record stopped", slog.LevelInfo)
	if got := requireIntAttr(t, entry, "samples"); got != videoFrames+audioFrames {
		t.Fatalf("samples = %d, want %d (accumulated, not track count)", got, videoFrames+audioFrames)
	}
	requireStringAttr(t, entry, "samplesSource", "fragment")
	if got := requireIntAttr(t, entry, "tracks"); got != 2 {
		t.Fatalf("tracks = %d, want 2", got)
	}
}

// —— 事件 3：索引写入异常 ——

func TestIndexWriteErrorLogFields(t *testing.T) {
	h := &captureHandler{}
	r := &Recorder{}
	r.Logger = slog.New(h)
	r.Event.StreamPath = "live/cam1"
	r.Event.FilePath = "records/live/cam1/rec.mp4"
	r.indexPath = "records/live/cam1/rec.mp4.m7sidx"
	r.indexSamples = 42

	r.warnIndex(errors.New("disk full"))

	entry := h.mustFind(t, "index write failed", slog.LevelError)
	requireStringAttr(t, entry, "streamPath", "live/cam1")
	requireStringAttr(t, entry, "file", "records/live/cam1/rec.mp4")
	if got := requireIntAttr(t, entry, "recordsWritten"); got != 42 {
		t.Fatalf("recordsWritten = %d, want 42", got)
	}
	errAttr, ok := requireAttr(t, entry, "error").(error)
	if !ok || errAttr.Error() != "disk full" {
		t.Fatalf("error = %v, want disk full", requireAttr(t, entry, "error"))
	}
	if _, ok := entry.Attrs["indexPath"]; !ok {
		t.Fatalf("indexPath should be present for locating the sidecar: %v", entry.Attrs)
	}
}

// —— 事件 4：上传成功 / 失败 ——

func newCapturedFinalizeTask(t *testing.T, h *captureHandler, f *fakeStorageFile, dbConfigured bool) *finalizeUploadTask {
	t.Helper()
	u := &finalizeUploadTask{
		file:       f,
		filePath:   "live/cam1/rec.mp4",
		streamPath: "live/cam1",
		size:       12345,
	}
	u.Logger = slog.New(h)
	if dbConfigured {
		u.db = newPendingTestDB(t)
	}
	return u
}

func TestUploadSucceededLogFields(t *testing.T) {
	h := &captureHandler{}
	u := newCapturedFinalizeTask(t, h, &fakeStorageFile{}, false)

	if err := u.Go(); err != nil {
		t.Fatalf("Go() = %v", err)
	}

	entry := h.mustFind(t, "upload succeeded", slog.LevelInfo)
	requireStringAttr(t, entry, "file", "live/cam1/rec.mp4")
	requireStringAttr(t, entry, "streamPath", "live/cam1")
	if got := requireIntAttr(t, entry, "size"); got != 12345 {
		t.Fatalf("size = %d, want 12345", got)
	}
	if got := requireIntAttr(t, entry, "retryCount"); got != 0 {
		t.Fatalf("retryCount = %d, want 0", got)
	}
}

func TestUploadFailedLogFields(t *testing.T) {
	h := &captureHandler{}
	u := newCapturedFinalizeTask(t, h, &fakeStorageFile{closeErr: errors.New("connection reset")}, true)

	if err := u.Go(); err == nil {
		t.Fatal("expected upload failure")
	}

	entry := h.mustFind(t, "upload failed", slog.LevelError)
	requireStringAttr(t, entry, "file", "live/cam1/rec.mp4")
	requireStringAttr(t, entry, "errorClass", "transient")
	if got := requireIntAttr(t, entry, "size"); got != 12345 {
		t.Fatalf("size = %d, want 12345", got)
	}
	if got := requireIntAttr(t, entry, "retryCount"); got != 0 {
		t.Fatalf("retryCount = %d, want 0", got)
	}
	nextRetryAt, ok := requireAttr(t, entry, "nextRetryAt").(time.Time)
	if !ok {
		t.Fatalf("nextRetryAt should be time.Time, got %T", requireAttr(t, entry, "nextRetryAt"))
	}
	if nextRetryAt.IsZero() {
		t.Fatal("nextRetryAt should be set when a pending retry can be registered")
	}
}

func TestUploadFailedPermanentClass(t *testing.T) {
	h := &captureHandler{}
	u := newCapturedFinalizeTask(t, h, &fakeStorageFile{closeErr: os.ErrNotExist}, false)

	if err := u.Go(); err == nil {
		t.Fatal("expected upload failure")
	}

	entry := h.mustFind(t, "upload failed", slog.LevelError)
	requireStringAttr(t, entry, "errorClass", "permanent")
}

func TestUploadErrorClass(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"missing file", os.ErrNotExist, "permanent"},
		{"permission", os.ErrPermission, "permanent"},
		{"pending full", storage.ErrPendingDirFull, "permanent"},
		{"network", errors.New("i/o timeout"), "transient"},
		{"nil", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storage.UploadErrorClass(tc.err); got != tc.want {
				t.Fatalf("UploadErrorClass(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestUploadEnqueuedLogFields(t *testing.T) {
	h := &captureHandler{}
	trailer := &writeTrailerTask{
		filePath:   "live/cam1/rec.mp4",
		streamPath: "live/cam1",
	}
	trailer.Logger = slog.New(h)

	trailer.logUploadEnqueued(12345)

	entry := h.mustFind(t, "upload enqueued", slog.LevelInfo)
	requireStringAttr(t, entry, "file", "live/cam1/rec.mp4")
	if got := requireIntAttr(t, entry, "size"); got != 12345 {
		t.Fatalf("size = %d, want 12345", got)
	}
	if got := requireIntAttr(t, entry, "retryCount"); got != 0 {
		t.Fatalf("retryCount = %d, want 0", got)
	}
}
