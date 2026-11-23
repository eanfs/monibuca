package mp4

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	task "github.com/eanfs/gotask"
	"gorm.io/gorm"
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
		{"empty storage type is local", "", true, true, false},
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
		onFail:   func(err error) bool { called++; cause = err; return false },
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

// TestSpillToPendingWritesDeferredRecord pins that a spilled finalize still
// writes the delayed record_streams update. Without it, every spilled recording
// keeps end_time=0/duration=0 forever (the pending retry path never writes the
// record), so time-range queries silently drop it.
func TestSpillToPendingWritesDeferredRecord(t *testing.T) {
	dbWrites := 0
	ft := &finalizeUploadTask{
		file:     &fakeFinalizeFile{},
		filePath: "live/ch1/fixed.mp4",
		size:     1,
		onFail:   func(error) bool { return true },
		dbWrite:  func(task.IJob) error { dbWrites++; return nil },
	}
	ft.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	ft.spillToPending(errUploadBacklogFull)

	if dbWrites != 1 {
		t.Fatalf("dbWrite ran %d times, want 1 (spilled records must still get end_time/duration)", dbWrites)
	}
}

// onFail 未能确认媒体被保留（例如 localPath 为空）时不得写库：否则会留下
// 指向一个永远不会存在的对象的记录。
func TestSpillToPendingSkipsDeferredRecordWhenMediaNotKept(t *testing.T) {
	dbWrites := 0
	ft := &finalizeUploadTask{
		file:     &fakeFinalizeFile{},
		filePath: "live/ch1/fixed.mp4",
		size:     1,
		onFail:   func(error) bool { return false },
		dbWrite:  func(task.IJob) error { dbWrites++; return nil },
	}
	ft.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	ft.spillToPending(errUploadBacklogFull)

	if dbWrites != 0 {
		t.Fatalf("dbWrite ran %d times, want 0 when the media is not retained", dbWrites)
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
		onFail:   func(error) bool { failed++; return false },
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

// newHandoffTrailerTask builds the trailer task the handoff tests drive. The
// per-test logger is required because the spill path logs before the task ever
// starts; a nil logger would drop the Warn.
func newHandoffTrailerTask(storageKey string, db *gorm.DB) (*writeTrailerTask, *fakeFinalizeFile) {
	file := &fakeFinalizeFile{}
	tt := &writeTrailerTask{
		file:       file,
		filePath:   "live/ch1/fixed.mp4",
		streamPath: "live/ch1",
		storageKey: storageKey,
		db:         db,
	}
	tt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return tt, file
}

// TestHandoffSpillsWhenBacklogFull pins the call-site wiring of the spill
// branch: object storage + DB + no free slot must hand the file to onFail with
// errUploadBacklogFull, without closing it and without touching the prefill.
func TestHandoffSpillsWhenBacklogFull(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	if !storage.TryAcquireUploadQueue() {
		t.Fatal("prefill failed")
	}
	tt, file := newHandoffTrailerTask("s3", &gorm.DB{})
	called := 0
	var cause error

	tt.handoffToUploadQueue(1, func(err error) bool { called++; cause = err; return false })

	if called != 1 || !errors.Is(cause, errUploadBacklogFull) {
		t.Fatalf("onFail called=%d cause=%v, want 1 and errUploadBacklogFull", called, cause)
	}
	if file.closed {
		t.Fatal("spill must not close the file: Close triggers the upload and deletes the temp file")
	}
	if got := storage.QueuedUploads(); got != 1 {
		t.Fatalf("QueuedUploads = %d, want 1 (prefill untouched)", got)
	}
}

// TestHandoffReleasesSlotOnInlineFinalize pins that a slot taken by the handoff
// is handed to onDone, so the forced inline finalize returns it.
func TestHandoffReleasesSlotOnInlineFinalize(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	queue := newStoppedUploadQueue(t) // stopped → enqueue rejects → inline path, no async run
	tt, file := newHandoffTrailerTask("s3", &gorm.DB{})
	failed := false

	tt.handoffToUploadQueueWith(queue, 1, func(error) bool { failed = true; return false })

	if !file.closed {
		t.Fatal("inline finalize must close (flush + upload) the file")
	}
	if failed {
		t.Fatal("onFail must not run when the inline finalize succeeds")
	}
	if got := storage.QueuedUploads(); got != 0 {
		t.Fatalf("QueuedUploads = %d, want 0 after inline completion (slot leaked)", got)
	}
}

// TestHandoffLocalNeverSpillsWithFullBacklog pins that a local Close is a
// rename: it must neither spill nor consume/release a slot.
func TestHandoffLocalNeverSpillsWithFullBacklog(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	if !storage.TryAcquireUploadQueue() {
		t.Fatal("prefill failed")
	}
	queue := newStoppedUploadQueue(t)
	tt, file := newHandoffTrailerTask("local", &gorm.DB{})
	failed := false

	tt.handoffToUploadQueueWith(queue, 1, func(error) bool { failed = true; return false })

	if failed {
		t.Fatal("local storage must never spill to the pending retry queue")
	}
	if !file.closed {
		t.Fatal("local inline finalize must close the file")
	}
	if got := storage.QueuedUploads(); got != 1 {
		t.Fatalf("QueuedUploads = %d, want 1 (local must not touch the prefill)", got)
	}
}

// TestHandoffNoDBNeverSpills pins that without a DB retry queue a full backlog
// still enqueues: a spill would delete the only copy of the recording.
func TestHandoffNoDBNeverSpills(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	if !storage.TryAcquireUploadQueue() {
		t.Fatal("prefill failed")
	}
	queue := newStoppedUploadQueue(t)
	tt, file := newHandoffTrailerTask("s3", nil)
	failed := false

	tt.handoffToUploadQueueWith(queue, 1, func(error) bool { failed = true; return false })

	if failed {
		t.Fatal("without a DB there is no retry queue; spilling would delete the only copy")
	}
	if !file.closed {
		t.Fatal("inline finalize must close the file")
	}
	if got := storage.QueuedUploads(); got != 1 {
		t.Fatalf("QueuedUploads = %d, want 1 (no-DB path holds no slot)", got)
	}
}

// slotSamplingFile samples the queued-upload counter from inside Close, i.e.
// while the inline finalize still holds whatever slot the handoff reserved.
type slotSamplingFile struct {
	fakeFinalizeFile
	onClose func()
}

func (f *slotSamplingFile) Close() error {
	if f.onClose != nil {
		f.onClose()
	}
	f.closed = true
	return nil
}

// TestHandoffLocalDoesNotAcquireSlot pins that local finalization never takes a
// backlog slot: if it did, local recordings could push object+DB recordings
// into spill. Go() runs inline and defers the onDone release, so sampling
// QueuedUploads() inside Close sees the slot exactly while it would be held.
func TestHandoffLocalDoesNotAcquireSlot(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 1})
	var during int32
	file := &slotSamplingFile{onClose: func() { during = storage.QueuedUploads() }}
	tt := &writeTrailerTask{file: file, filePath: "live/ch1/fixed.mp4", streamPath: "live/ch1", storageKey: "local", db: &gorm.DB{}}
	tt.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	tt.handoffToUploadQueueWith(newStoppedUploadQueue(t), 1, func(error) bool { t.Error("local handoff must not spill"); return false })

	if during != 0 {
		t.Fatalf("local finalize held %d queue slots at Close; it must not acquire one", during)
	}
}
