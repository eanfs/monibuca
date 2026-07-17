package mp4

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"

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
