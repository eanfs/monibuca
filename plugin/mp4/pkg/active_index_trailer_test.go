package mp4

import (
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// activeProbeFile 在 trailer 真正读文件（Seek）的那一刻探测活跃登记，
// 用来断言「重写窗口内登记仍然有效」。
type activeProbeFile struct {
	*storage.LocalFile
	mu       sync.Mutex
	probing  bool
	seeks    int
	active   bool
	isActive func() bool
}

func (f *activeProbeFile) Seek(off int64, whence int) (int64, error) {
	f.mu.Lock()
	if f.probing {
		f.seeks++
		if f.isActive() {
			f.active = true
		}
	}
	f.mu.Unlock()
	return f.LocalFile.Seek(off, whence)
}

func (f *activeProbeFile) observed() (seeks int, active bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seeks, f.active
}

// Important 4：活跃登记必须在整个 trailer 重写窗口内保持有效（Run 的 defer
// 才注销），否则恢复扫描只能靠 mtime 判据，长重写会被误判为崩溃残留并覆盖
// 正在写的文件。trailer 失败也要注销，避免登记永久泄漏。
func TestActiveRegistrationHeldUntilTrailerFinishes(t *testing.T) {
	rec, m, _, mediaPath := newIndexTestRecorder(t)
	indexPath := indexFilePath(mediaPath)
	w, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: mediaPath})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 截断媒体：trailer 在复制 pre-mdat 区时读到 EOF 返回，不会走到上传队列。
	if err = os.Truncate(mediaPath, 8); err != nil {
		t.Fatalf("truncate media: %v", err)
	}
	probe := &activeProbeFile{
		LocalFile: rec.file.(*storage.LocalFile),
		isActive:  func() bool { return IsActiveRecordingPath(indexPath, mediaPath) },
	}
	task := &writeTrailerTask{
		muxer:          m, // moov 未生成：INSERT_RANGE 快路径直接放弃
		file:           probe,
		filePath:       "live/test/rec.mp4",
		indexPath:      indexPath,
		indexMediaPath: mediaPath,
	}
	task.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	RegisterActiveIndex(indexPath, mediaPath)
	t.Cleanup(func() { UnregisterActiveIndex(indexPath, mediaPath) })

	probe.mu.Lock()
	probe.probing = true
	probe.mu.Unlock()
	// 该现场会在写临时文件阶段失败返回（mdatOffset 与 mdat 头不匹配），
	// 正是「trailer 失败也要注销」的路径。
	_ = task.Run()
	probe.mu.Lock()
	probe.probing = false
	probe.mu.Unlock()

	seeks, active := probe.observed()
	if seeks == 0 {
		t.Fatal("probe never observed the trailer running")
	}
	if !active {
		t.Fatal("active registration must survive while the trailer is rewriting the file")
	}
	if IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("trailer finished: active registration must be released even on failure")
	}
}
