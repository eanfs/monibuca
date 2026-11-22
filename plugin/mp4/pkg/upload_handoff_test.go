package mp4

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
)

// fakeStorageFile 实现 storage.File,用于验证 finalizeUploadTask 的成功/失败分支。
type fakeStorageFile struct {
	closeErr error
	closed   bool
}

func (f *fakeStorageFile) Write(p []byte) (int, error)               { return len(p), nil }
func (f *fakeStorageFile) WriteAt(p []byte, off int64) (int, error)  { return len(p), nil }
func (f *fakeStorageFile) Read(p []byte) (int, error)                { return 0, io.EOF }
func (f *fakeStorageFile) ReadAt(p []byte, off int64) (int, error)   { return 0, io.EOF }
func (f *fakeStorageFile) Seek(off int64, whence int) (int64, error) { return 0, nil }
func (f *fakeStorageFile) Sync() error                               { return nil }
func (f *fakeStorageFile) Close() error                              { f.closed = true; return f.closeErr }
func (f *fakeStorageFile) Stat() (os.FileInfo, error)                { return nil, os.ErrInvalid }
func (f *fakeStorageFile) Name() string                              { return "fake" }
func (f *fakeStorageFile) SetMetadata(key, value string)             {}

func newTestFinalizeTask(f *fakeStorageFile) *finalizeUploadTask {
	u := &finalizeUploadTask{
		file:       f,
		filePath:   "live/x/x.mp4",
		streamPath: "live/x",
	}
	u.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return u
}

// 上传(Close)失败:必须触发 onFail 兜底,返回错误。
func TestFinalizeUploadTaskCloseFail(t *testing.T) {
	f := &fakeStorageFile{closeErr: errors.New("upload failed")}
	u := newTestFinalizeTask(f)
	var gotCause error
	u.onFail = func(cause error) { gotCause = cause }
	var dbWriteCalled bool
	u.dbWrite = func(task.IJob) error { dbWriteCalled = true; return nil }

	err := u.Go()
	if err == nil {
		t.Fatal("expected error from Go()")
	}
	if !f.closed {
		t.Fatal("file should be closed")
	}
	if gotCause == nil {
		t.Fatal("onFail should be called with cause")
	}
	if dbWriteCalled {
		t.Fatal("dbWrite must NOT run when upload failed (延迟入库语义)")
	}
}

// 上传成功:执行延迟入库,不触发 onFail。
func TestFinalizeUploadTaskSuccess(t *testing.T) {
	f := &fakeStorageFile{}
	u := newTestFinalizeTask(f)
	var onFailCalled bool
	u.onFail = func(error) { onFailCalled = true }
	var dbWriteCalled bool
	u.dbWrite = func(tailJob task.IJob) error {
		if tailJob == nil {
			t.Error("tailJob should not be nil")
		}
		dbWriteCalled = true
		return nil
	}

	if err := u.Go(); err != nil {
		t.Fatalf("Go() = %v", err)
	}
	if !dbWriteCalled {
		t.Fatal("dbWrite should run after successful close")
	}
	if onFailCalled {
		t.Fatal("onFail must not be called on success")
	}
}

type fakeFinalizeFile struct{ closed bool }

func (f *fakeFinalizeFile) Write(p []byte) (int, error)                  { return len(p), nil }
func (f *fakeFinalizeFile) WriteAt(p []byte, off int64) (int, error)     { return len(p), nil }
func (f *fakeFinalizeFile) Read(p []byte) (int, error)                   { return 0, io.EOF }
func (f *fakeFinalizeFile) ReadAt(p []byte, off int64) (int, error)      { return 0, io.EOF }
func (f *fakeFinalizeFile) Seek(offset int64, whence int) (int64, error) { return 0, nil }
func (f *fakeFinalizeFile) Sync() error                                  { return nil }
func (f *fakeFinalizeFile) Close() error                                 { f.closed = true; return nil }
func (f *fakeFinalizeFile) Stat() (os.FileInfo, error)                   { return nil, nil }
func (f *fakeFinalizeFile) Name() string                                 { return "fake" }
func (f *fakeFinalizeFile) SetMetadata(key, value string)                {}

func newInlineFinalizeTask(t *testing.T) (*finalizeUploadTask, *fakeFinalizeFile, <-chan struct{}) {
	t.Helper()
	file := &fakeFinalizeFile{}
	done := make(chan struct{})
	ft := &finalizeUploadTask{file: file, filePath: "live/ch1/fixed.mp4", size: 1}
	ft.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ft.onDone = func() { close(done) }
	return ft, file, done
}

func TestFinalizeRunsInlineWhenUploadQueueRejected(t *testing.T) {
	queue := newStoppedUploadQueue(t)
	ft, file, done := newInlineFinalizeTask(t)
	if !enqueueFinalizeOrRunInline(queue, ft, ft.Logger) {
		t.Fatal("expected inline fallback when the upload queue rejects the task")
	}
	select {
	case <-done:
	default:
		t.Fatal("finalize did not run inline; the recording would be silently dropped")
	}
	if !file.closed {
		t.Fatal("inline finalize must close (flush + upload) the file")
	}
	if ft.file != nil {
		t.Fatal("file ownership must be released after inline finalize")
	}
}

func TestFinalizeStaysQueuedWhenUploadQueueAccepts(t *testing.T) {
	if raceEnabled {
		t.Skip("skipping under -race: starting/stopping a gotask Work triggers the upstream v1.0.5 EventLoop race (CLAUDE.md Known Issues)")
	}
	var root task.RootManager[uint32, task.ManagerItem[uint32]]
	root.Init()
	queue := &UploadQueueTask{}
	if err := root.AddTask(queue).WaitStarted(); err != nil {
		t.Fatalf("start upload queue: %v", err)
	}
	t.Cleanup(func() {
		queue.Stop(errors.New("test done"))
		_ = queue.WaitStopped()
	})

	ft, file, done := newInlineFinalizeTask(t)
	if enqueueFinalizeOrRunInline(queue, ft, ft.Logger) {
		t.Fatal("accepted task must not run inline")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queued finalize did not run")
	}
	if !file.closed {
		t.Fatal("queued finalize must close (flush + upload) the file")
	}
}
