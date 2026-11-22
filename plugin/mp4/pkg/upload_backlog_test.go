package mp4

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// TestUploadBacklogGateSkipsLocalAndDBLess pins the spill decision table: only
// object storage with a DB retry queue may spill; local storage has nothing to
// upload, and without a DB a spill would delete the only copy.
func TestUploadBacklogGateSkipsLocalAndDBLess(t *testing.T) {
	tests := []struct {
		name       string
		storageKey string
		hasDB      bool
		full       bool
		want       bool
	}{
		{"local never spills", "local", true, true, false},
		{"no db never spills", "s3", false, true, false},
		{"object storage with db spills when full", "s3", true, true, true},
		{"object storage with db and room does not spill", "s3", true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uploadBacklogFullFor(tt.storageKey, tt.hasDB, tt.full); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSpillToPendingDoesNotCloseFile pins that a spill hands ownership to the
// pending path: it must reach onFail exactly once and never Close the file
// (Close would trigger the upload and delete the temp file).
func TestSpillToPendingDoesNotCloseFile(t *testing.T) {
	called := 0
	var cause error
	file := &fakeFinalizeFile{}
	ft := &finalizeUploadTask{
		file:     file,
		filePath: "live/ch1/fixed.mp4",
		size:     1,
		onFail:   func(err error) { called++; cause = err },
	}
	ft.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	ft.spillToPending(errUploadBacklogFull)

	if called != 1 || !errors.Is(cause, errUploadBacklogFull) {
		t.Fatalf("onFail called=%d cause=%v, want 1 and errUploadBacklogFull", called, cause)
	}
	if ft.file != nil {
		t.Fatal("file ownership must be dropped so the caller's pending path owns it")
	}
	if file.closed {
		t.Fatal("spill must not Close: Close triggers upload and deletes the temp file")
	}
}

// closeErrorFile fails on Close so the failure path of Go() is exercised.
type closeErrorFile struct{ fakeFinalizeFile }

func (f *closeErrorFile) Close() error {
	f.closed = true
	return errors.New("close failed")
}

// TestFinalizeReleasesUploadQueueOnFailure pins that a failed finalize still
// returns its reserved slot through onDone.
func TestFinalizeReleasesUploadQueueOnFailure(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	if !storage.TryAcquireUploadQueue() {
		t.Fatal("acquire failed")
	}
	failed := 0
	ft := &finalizeUploadTask{
		file:     &closeErrorFile{},
		filePath: "live/ch1/fixed.mp4",
		size:     1,
		onFail:   func(error) { failed++ },
	}
	ft.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ft.onDone = storage.ReleaseUploadQueue

	if err := ft.Go(); err == nil {
		t.Fatal("expected the close error to propagate")
	}
	if failed != 1 {
		t.Fatalf("onFail called %d times, want 1", failed)
	}
	if got := storage.QueuedUploads(); got != 0 {
		t.Fatalf("QueuedUploads = %d, want 0 after failure", got)
	}
}

// TestFinalizeReleasesUploadQueueWhenRunInline pins that the inline fallback
// (queue stopped, task rejected) still runs the finalize and releases the slot.
func TestFinalizeReleasesUploadQueueWhenRunInline(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	if !storage.TryAcquireUploadQueue() {
		t.Fatal("acquire failed")
	}
	ft := &finalizeUploadTask{file: &fakeFinalizeFile{}, filePath: "live/ch1/fixed.mp4", size: 1}
	ft.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ft.onDone = storage.ReleaseUploadQueue

	if !enqueueFinalizeOrRunInline(newStoppedUploadQueue(t), ft, ft.Logger) {
		t.Fatal("a stopped queue must reject the task and force inline execution")
	}
	if got := storage.QueuedUploads(); got != 0 {
		t.Fatalf("QueuedUploads = %d, want 0 after inline completion", got)
	}
}

// TestFinalizeReleasesUploadQueue pins the happy path releases its slot.
func TestFinalizeReleasesUploadQueue(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	if !storage.TryAcquireUploadQueue() {
		t.Fatal("acquire failed")
	}
	file := &fakeFinalizeFile{}
	ft := &finalizeUploadTask{file: file, filePath: "live/ch1/fixed.mp4", size: 1}
	ft.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	ft.onDone = storage.ReleaseUploadQueue

	if err := ft.Go(); err != nil {
		t.Fatalf("Go: %v", err)
	}
	if !file.closed {
		t.Fatal("finalize must close the file")
	}
	if got := storage.QueuedUploads(); got != 0 {
		t.Fatalf("QueuedUploads = %d, want 0 after completion", got)
	}
}
