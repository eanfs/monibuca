package mp4

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// newInsertRangeTestTask 造一个可走 INSERT_RANGE 快路径的现场：
// [ftyp][free][mdat] + 一个 sample + 文件尾 moov（与 Start() 之后一致）。
func newInsertRangeTestTask(t *testing.T, indexPath string) (*writeTrailerTask, *Muxer, string) {
	t.Helper()
	r, m, track, mediaPath := newIndexTestRecorder(t)
	if err := r.writeSample(track, newIndexTestSample(0, true, 1, 2, 3, 4)); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	// Start() 会先 WriteTrailer 把 moov 写到文件尾，Run() 的 mdatOffset 判据
	// 依赖这一布局。
	if err := m.WriteTrailer(r.file); err != nil {
		t.Fatalf("write trailer: %v", err)
	}
	if m.mdatOffset != 48 {
		t.Fatalf("mdatOffset = %d, want 48 (fixture must allow the fast path)", m.mdatOffset)
	}
	task := &writeTrailerTask{
		muxer:      m,
		file:       r.file,
		filePath:   "live/test/rec.mp4",
		durationMs: 20,
		indexPath:  indexPath,
	}
	task.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return task, m, mediaPath
}

// insertRangeUnsupported 探测当前平台/文件系统是否真的不支持 INSERT_RANGE。
// 快路径在支持的平台上会一路走到上传队列（单测里没有 Server），故只在
// 不支持的平台跑“回退后的一致性”断言。
func insertRangeUnsupported(t *testing.T) bool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.bin")
	if err := os.WriteFile(path, make([]byte, 4096), 0o644); err != nil {
		t.Fatalf("write probe: %v", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer f.Close()
	return errors.Is(storage.InsertRange(f, 0, 4096), storage.ErrRangeInsertUnsupported)
}

// 平移意图写不进 sidecar 时绝不做原地插入：否则崩溃后恢复侧无从判断数据
// 是否已经移位，只能产出错位 MP4。此时必须原样回退到全量重写。
func TestInsertRangeAbortsWhenShiftRecordUnwritable(t *testing.T) {
	// 用目录占住 sidecar 路径：openIndexAppend 必然失败。
	blocked := t.TempDir()
	task, m, mediaPath := newInsertRangeTestTask(t, blocked)
	before, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	beforeOffset := m.Tracks[1].Samplelist[0].Offset

	handled, runErr := task.runInsertRangeFastPath()
	if handled || runErr != nil {
		t.Fatalf("handled=%v err=%v, want fallback (false,nil)", handled, runErr)
	}
	after, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media after: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("media size changed %d -> %d: INSERT_RANGE must not run", len(before), len(after))
	}
	if got := m.Tracks[1].Samplelist[0].Offset; got != beforeOffset {
		t.Fatalf("sample offset = %d, want restored %d", got, beforeOffset)
	}
}

// INSERT_RANGE 不受支持而回退时，sidecar 里已落盘的 SHIFT 不能把后续恢复
// 带偏：文件长度仍是 preLen，恢复判定必须得出「不平移」。
func TestInsertRangeFallbackLeavesResolvableShiftRecord(t *testing.T) {
	if !insertRangeUnsupported(t) {
		t.Skip("当前平台/文件系统支持 INSERT_RANGE，快路径会继续执行，另由真机验证")
	}
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "rec.mp4"+IndexExtension)
	task, _, mediaPath := newInsertRangeTestTask(t, indexPath)

	// sidecar 先处于「已提交」状态（生产上 commitIndex 在 writeTailer 里先跑）。
	idx, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: mediaPath})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err = idx.Commit(20, 1); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err = idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	handled, runErr := task.runInsertRangeFastPath()
	if handled || runErr != nil {
		t.Fatalf("handled=%v err=%v, want fallback (false,nil)", handled, runErr)
	}

	got, err := ReadIndex(indexPath)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.OffsetShift != OffsetShiftPending {
		t.Fatalf("OffsetShift = %v, want pending after an attempted insert", got.OffsetShift)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("stat media: %v", err)
	}
	shift, err := got.ResolveOffsetShift(info.Size())
	if err != nil {
		t.Fatalf("ResolveOffsetShift: %v", err)
	}
	if shift != 0 {
		t.Fatalf("shift = %d, want 0 (INSERT_RANGE never applied)", shift)
	}
}
