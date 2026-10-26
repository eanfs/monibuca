package mp4

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// startFailFile 让 writeTrailerTask.Start 必然失败：NewMuxer(0) 的
// reWriteMdatSize 会先 Seek 到负偏移，LocalFile.Seek 返回错误。
// seeks 计数用于证明 Start 真的执行到了重写步骤。
type startFailFile struct {
	*storage.LocalFile
	seeks atomic.Int32
}

func (f *startFailFile) Seek(off int64, whence int) (int64, error) {
	f.seeks.Add(1)
	return f.LocalFile.Seek(off, whence)
}

// Important 1（回归）：writeTrailerTask.Start() 失败时 gotask 不会调用 Run()
// （没有 SetRetry 时任务被直接移出队列），而且它的 dispose 在
// state < TASK_STATE_STARTED 时直接返回——Run 的 defer 与 OnDispose 都不会触发。
// 活跃登记必须在 Start 的失败路径上注销，否则 IsActiveRecordingPath 会让恢复扫描
// 永久跳过这个 sidecar（相对 commitIndex 同步注销的基线行为是回归）。
//
// 这里直接调用 Start 而不驱动 gotask 队列：队列 dispose 会命中 gotask v1.0.5
// EventLoop 的上游 -race 竞态（CLAUDE.md 已记录），会让本用例确定性染红 -race
// 门禁；被测语义（Start 失败即注销）与调度无关。
func TestActiveRegistrationReleasedWhenTrailerStartFails(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "rec.mp4")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	indexPath := indexFilePath(mediaPath)
	fd, err := os.OpenFile(mediaPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open media: %v", err)
	}
	file := &startFailFile{LocalFile: &storage.LocalFile{File: fd}}
	t.Cleanup(func() { fd.Close() })

	trailer := &writeTrailerTask{
		muxer:          NewMuxer(0), // 未写入任何数据：Start 的 trailer 重写必然失败
		file:           file,
		filePath:       "live/test/rec.mp4",
		indexPath:      indexPath,
		indexMediaPath: mediaPath,
	}
	trailer.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	RegisterActiveIndex(indexPath, mediaPath)
	t.Cleanup(func() { UnregisterActiveIndex(indexPath, mediaPath) })
	if !IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("fixture: recording must be registered as active before Start")
	}

	if startErr := trailer.Start(); startErr == nil {
		t.Fatal("writeTrailerTask.Start must fail for this fixture")
	}
	if file.seeks.Load() == 0 {
		t.Fatal("writeTrailerTask.Start never reached the trailer rewrite: test fixture is wrong")
	}
	if IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("active registration leaked after writeTrailerTask.Start failed: " +
			"recovery would skip this sidecar for the rest of the process lifetime")
	}
}
