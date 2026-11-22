# Record Segment Unique Naming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop an in-process retry or a fragment rotation from writing to the same file name / object key as the previous segment, so a one-hour recording is never reduced to its last few seconds.

**Architecture:** `m7s.DefaultRecorder.CreateStream` is the single funnel every mp4/flv/hls segment passes through (first start, gotask retry, `checkFragment` rotation). Add a per-recorder segment counter and append `-%03d` to the path returned by `customFileName` for segments after the first — only when `RecConf.FileName` is set. The first segment keeps the exact requested name, so the caller's `<bucket>/<streamId>/<recordingId>.mp4` contract is unchanged.

**Tech Stack:** Go (module `m7s.live/v5`), gotask task lifecycle, `pkg/storage` abstraction, existing process-level harness in `test/` (real server child + ffmpeg).

**Spec:** No standalone spec document; the incident evidence and design rationale are captured in "Background" below.

## Background (incident evidence, 2026-11-22)

- 36 cameras recorded 60 minutes each; 15 objects ended up 2.8–136.3 s long.
- The server log shows 59 successful S3 uploads and 15 object keys uploaded 2–4 times.
- Camera `1921681122` wrote four segments into one key:

| Segment | Media range | durationMs | Upload finished |
|---|---|---|---|
| 1 | 19:48:06 → 20:46:56 | 3,528,049 | 20:47:07 |
| 2 | 20:46:57 → 20:47:02 | 2,810 | 20:47:05 |
| 3 | 20:47:02 → 20:47:56 | 51,250 | 20:48:12 |
| 4 | 20:47:56 → 20:48:08 | 10,450 | 20:48:25 |

The final object is segment 4 (10.45 s); S3 upload is last-writer-wins.

Failure chain:

1. 36 recordings start together; their trailers upload ~900 MB each around 20:46 while new uploads keep arriving (4 upload slots, s3manager 5×64 MB range reads, no read throttle).
2. The record subscriber writes synchronously to the S3 temp file, falls behind the publisher ring (~1 GOP), jumps to the latest IDR, and finally receives `ErrDiscard`.
3. `RecordJob` retries in place (`SetRetry(-1, 1s)`); `Recorder.createStream` runs again with `r.muxer == nil`.
4. `CustomFileName` returns `filepath.Join(RecConf.FilePath, RecConf.FileName)` — the same path every time.
5. The next upload overwrites the full-length object.

The caller (`xde-media-spring-boot-starter`) always sends a fixed `fileName`
(`rec-<camera>-<epochMillis>.mp4`, `fragment=0s`) and stores
`<bucket>/<streamId>/<recordingId>.mp4` as the recording URL. The in-process retry
is invisible to it; its own `restartRecording` (which appends `-NNN`) only fires
when the server reports no active recording — never true while the RecordJob
retries.

Recording and pub/sub code is byte-identical between v5.3.13 and v5.3.16
(`git diff --name-status` contains only crash-recovery files), so this is a
latent bug, not a version regression.

**Non-goals:** eliminating ring discard (deployment-side
`global.publish.buffertime` is the mitigation), stitching segments in the
business layer, and changing auto-resume (it already drops a fixed `fileName`;
`TestResumeRecordingDropsFixedFileNameAndUsesRemainingDuration` pins that).

## Global Constraints

- Branch from `main`; PR base is `main` (repo policy since 2026-10-12).
- Go toolchain per `go.mod`; run tests with `-race` for touched packages.
- Segment 0 MUST use the exact path returned by `customFileName` (caller contract).
- Segments ≥ 1 with `RecConf.FileName != ""` get `-%03d` inserted before the file
  extension: `rec-1.mp4` → `rec-1-001.mp4`.
- A `CreateStream` call that returns an error (e.g. storage unavailable) MUST NOT
  consume a segment number; the next attempt reuses the first unused name.
- When `RecConf.FileName == ""`, naming is unchanged (mp4 already uses a
  nanosecond timestamp).
- Do not change API request fields, `RecordJob.GetKey()`, or `RecConf.FileName`.
- Commit messages: Conventional Commits, English.

## Review Focus

- First segment keeps the requested name — otherwise every caller's stored URL
  breaks (Task 2, `TestCreateStreamAssignsUniqueNamesAcrossSegments`).
- Storage failure before the first segment must not silently rename it (Task 2,
  `TestCreateStreamDoesNotConsumeSegmentOnStorageFailure`).
- `fileName`-less recordings must keep timestamp naming (Task 2,
  `TestCreateStreamKeepsTimestampNamingWhenFileNameEmpty`).
- Real fragment rotation must produce distinct files on disk (Task 3,
  `TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite`).
- A counter beyond 999 produces a 4-digit suffix (`-1000`); pinned in Task 1's
  table so the boundary is a decision, not an accident.
- A rejected upload-queue handoff must not silently drop the file: the finalize
  step runs inline instead (Task 4, `TestFinalizeRunsInlineWhenUploadQueueRejected`).

---

### Task 1: `segmentRecordPath` helper

**Files:**
- Create: `recoder_segment.go`
- Test: `recoder_segment_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `func segmentRecordPath(path string, seq int) string` in package `m7s`.

- [ ] **Step 1: Write the failing test**

```go
package m7s

import "testing"

func TestSegmentRecordPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		seq  int
		want string
	}{
		{"first segment unchanged", "live/ch1/rec-1.mp4", 0, "live/ch1/rec-1.mp4"},
		{"second segment", "live/ch1/rec-1.mp4", 1, "live/ch1/rec-1-001.mp4"},
		{"three digits padded", "live/ch1/rec-1.mp4", 42, "live/ch1/rec-1-042.mp4"},
		{"extension only on base name", "live/v1.2/rec.1.mp4", 7, "live/v1.2/rec.1-007.mp4"},
		{"no extension", "live/ch1/rec-1", 3, "live/ch1/rec-1-003"},
		{"beyond three digits", "live/ch1/rec-1.mp4", 1000, "live/ch1/rec-1-1000.mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := segmentRecordPath(tt.path, tt.seq); got != tt.want {
				t.Fatalf("segmentRecordPath(%q, %d) = %q, want %q", tt.path, tt.seq, got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run it and verify it fails**

Run: `go test . -run '^TestSegmentRecordPath$' -count=1`
Expected: FAIL — `undefined: segmentRecordPath`.

- [ ] **Step 3: Implement `segmentRecordPath` in `recoder_segment.go`**

```go
package m7s

import (
	"fmt"
	"path/filepath"
	"strings"
)

// segmentRecordPath appends a zero-padded segment number before the extension
// for every segment after the first: rec-1.mp4 -> rec-1-001.mp4.
// Three digits keep the caller's recording-name regex matching
// (.*rec-\d{4,16}-\d{10,20}(-\d{3}){0,3}$).
func segmentRecordPath(path string, seq int) string {
	if seq <= 0 {
		return path
	}
	ext := filepath.Ext(path)
	return fmt.Sprintf("%s-%03d%s", strings.TrimSuffix(path, ext), seq, ext)
}
```

- [ ] **Step 4: Run it and verify it passes**

Run: `go test . -run '^TestSegmentRecordPath$' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add recoder_segment.go recoder_segment_test.go
git commit -m "feat(record): add segment suffix helper for fixed file names"
```

---

### Task 2: Sequence segments in `DefaultRecorder.CreateStream`

**Files:**
- Modify: `recoder.go:56-60` (`DefaultRecorder` struct)
- Modify: `recoder.go:113-192` (`CreateStream`)
- Modify: `recoder_segment_test.go` (append tests)
- Modify: `CLAUDE.md` (Known Issues and Common Pitfalls entries)

**Interfaces:**
- Consumes: `segmentRecordPath(path string, seq int) string` from Task 1.
- Produces: no new exported API. `DefaultRecorder.segmentSeq` is an unexported
  field; later tasks observe it only through `Event.FilePath` / `Event.FileName`.

- [ ] **Step 1: Write the failing tests**

Append to `recoder_segment_test.go` (imports: `path/filepath`, `testing`, `time`,
`m7s.live/v5/pkg/config`, `m7s.live/v5/pkg/storage`):

```go
func newSegmentTestRecorder(t *testing.T, fileName string, st storage.Storage, status StorageStatus) *DefaultRecorder {
	t.Helper()
	server := &Server{}
	server.activateStorage(st, status)
	rec := &DefaultRecorder{}
	rec.RecordJob.Plugin = &Plugin{Server: server}
	rec.RecordJob.Subscriber = &Subscriber{Publisher: &Publisher{}}
	rec.RecordJob.RecConf = &config.Record{FilePath: "live/ch1", FileName: fileName, Type: "mp4"}
	return rec
}

func fixedName(*RecordJob) string { return "live/ch1/rec-1.mp4" }

func TestCreateStreamAssignsUniqueNamesAcrossSegments(t *testing.T) {
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := newSegmentTestRecorder(t, "rec-1.mp4", local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	want := []string{"rec-1.mp4", "rec-1-001.mp4", "rec-1-002.mp4", "rec-1-003.mp4"}
	for i, w := range want {
		if err := rec.CreateStream(time.Now(), fixedName); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		if got := filepath.Base(rec.Event.FilePath); got != w {
			t.Fatalf("segment %d path=%q, want %q", i, got, w)
		}
		if rec.Event.FileName != w {
			t.Fatalf("segment %d RecordStream.FileName=%q, want %q", i, rec.Event.FileName, w)
		}
	}
}

func TestCreateStreamDoesNotConsumeSegmentOnStorageFailure(t *testing.T) {
	rec := newSegmentTestRecorder(t, "rec-1.mp4", storage.NewUnavailableStorage("s3"),
		StorageStatus{DesiredType: "s3", ActiveType: "s3", Degraded: true})
	if err := rec.CreateStream(time.Now(), fixedName); err == nil {
		t.Fatal("expected storage error")
	}
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordJob.Plugin.Server.activateStorage(local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	if err := rec.CreateStream(time.Now(), fixedName); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(rec.Event.FilePath); got != "rec-1.mp4" {
		t.Fatalf("recovered first segment path=%q, want %q", got, "rec-1.mp4")
	}
}

func TestCreateStreamKeepsTimestampNamingWhenFileNameEmpty(t *testing.T) {
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := newSegmentTestRecorder(t, "", local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	names := []string{
		"live/ch1/2026-11-22-20-00-00_000000001.mp4",
		"live/ch1/2026-11-22-20-00-01_000000002.mp4",
	}
	for i, name := range names {
		if err := rec.CreateStream(time.Now(), func(*RecordJob) string { return name }); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		if rec.Event.FilePath != name {
			t.Fatalf("segment %d path=%q, want %q", i, rec.Event.FilePath, name)
		}
	}
}
```

- [ ] **Step 2: Run them and verify the new behavior test fails**

Run: `go test . -run '^(TestCreateStreamAssignsUniqueNamesAcrossSegments|TestCreateStreamDoesNotConsumeSegmentOnStorageFailure|TestCreateStreamKeepsTimestampNamingWhenFileNameEmpty)$' -count=1`
Expected: `TestCreateStreamAssignsUniqueNamesAcrossSegments` FAILS with
`segment 1 path="rec-1.mp4", want "rec-1-001.mp4"`; the other two PASS.

- [ ] **Step 3: Implement the counter**

Add to `DefaultRecorder` (`recoder.go:56-60`):

```go
	// segmentSeq counts segments already opened by this recorder task (first = 0).
	segmentSeq int
```

In `CreateStream`, replace the path computation (`recoder.go:118`):

```go
	filePath := customFileName(recordJob)
	if recordJob.RecConf.FileName != "" {
		filePath = segmentRecordPath(filePath, r.segmentSeq)
	}
```

At the end of `CreateStream`, just before the final `return`:

```go
	if recordJob.RecConf.FileName != "" {
		r.segmentSeq++
	}
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test . -run '^(TestSegmentRecordPath|TestCreateStreamAssignsUniqueNamesAcrossSegments|TestCreateStreamDoesNotConsumeSegmentOnStorageFailure|TestCreateStreamKeepsTimestampNamingWhenFileNameEmpty)$' -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Update `CLAUDE.md`**

In the "Known Issues" section, replace the entry
`mp4 分片录制 + 指定 fileName 时反复覆盖同一文件(数据丢失)` with:

```markdown
### ~~mp4 分片录制 + 指定 fileName 时反复覆盖同一文件(数据丢失)~~ (fixed 2026-11-22)

- **Status**: fixed in `m7s.DefaultRecorder.CreateStream`.
- The first segment keeps the requested `fileName`; every later segment opened by
  the same recorder task (in-process retry after `ErrDiscard`, or `fragment>0`
  rotation) gets `-001`, `-002`, ... appended before the extension, e.g.
  `rec-1.mp4` -> `rec-1-001.mp4`. This applies to mp4, flv and hls recorders.
- The segment counter advances only after the segment's storage is resolved, so a
  failed first attempt does not consume the requested name.
- `fragment` still defaults to 1 minute on the API; callers that want a single
  file must still send `"fragment":"0"`.
```

In "Common Pitfalls", replace the `指定 fileName + fragment>0 = 静默丢数据` bullet
with:

```markdown
- **指定 fileName + fragment>0 no longer overwrites**: the server names the first
  segment exactly as requested and appends `-001`, `-002`, ... for later segments
  (retry or fragment rotation). `"fragment":"0"` is still required for a
  single-file recording.
```

- [ ] **Step 6: Commit**

```bash
git add recoder.go recoder_segment_test.go CLAUDE.md
git commit -m "fix(record): give retry and fragment segments unique file names"
```

---

### Task 3: Process-level fragment regression and full verification

**Files:**
- Modify: `test/recovery_integration_test.go` (`startRecoveryServer`, `startRecoveryChild`)
- Create: `test/record_segment_name_test.go`

**Interfaces:**
- Consumes: the fix from Task 2; harness helpers `startRecoveryServer`,
  `startFFmpeg`, `waitFor`, `postJSON`, `repoFile`, `recoveryChildEnv`,
  `recoveryConfigEnv` from `test/recovery_integration_test.go`.
- Produces: `startRecoveryServer(t *testing.T, root, storageBase, recordDir, logName, runPattern string) *recoveryServer` (new `runPattern` parameter).

- [ ] **Step 1: Parameterize the child test pattern**

In `test/recovery_integration_test.go`:

- `startRecoveryServer` gains a `runPattern string` parameter and passes it to
  `startRecoveryChild`.
- `startRecoveryChild` gains `runPattern string` and builds the command as
  `exec.Command(bin, "-test.run="+runPattern, "-test.v")` instead of the hardcoded
  `-test.run=^TestRecoveryAfterKill$`; callers pass the bare pattern.
- Update both existing calls to pass `"^TestRecoveryAfterKill$"`.

Verify: `go test ./test -run '^$' -count=1`
Expected: PASS (compiles; no tests run).

- [ ] **Step 2: Write the failing regression test**

Create `test/record_segment_name_test.go`:

```go
package test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"m7s.live/v5"
)

// TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite pins that a fixed
// fileName produces one file for the first segment and -001/-002/... for
// fragment rotations. Before the fix every rotation rewrote fixed.mp4 and
// fixed-001.mp4 never appeared.
func TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite(t *testing.T) {
	if os.Getenv(recoveryChildEnv) == "1" {
		cfg := os.Getenv(recoveryConfigEnv)
		if cfg == "" {
			t.Fatalf("%s is not set in child mode", recoveryConfigEnv)
		}
		if err := m7s.Run(context.Background(), cfg); err != nil {
			t.Fatalf("child server exited: %v", err)
		}
		return
	}
	if testing.Short() {
		t.Skip("skipping process-level recording test in -short mode")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skipf("ffmpeg not found in PATH: %v", err)
	}

	root := t.TempDir()
	storageBase := filepath.Join(root, "storage")
	recordDir := filepath.Join(storageBase, "rec")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatalf("create record dir: %v", err)
	}

	s := startRecoveryServer(t, root, storageBase, recordDir, "segment-child.log",
		"^TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite$")
	startFFmpeg(t, root, repoFile(t, "example/default/test.flv"), s.rtmpPort)

	waitFor(t, 30*time.Second, 250*time.Millisecond, "record api accepted", func() error {
		status, body, err := postJSON(fmt.Sprintf("http://127.0.0.1:%d/mp4/api/start/live/test", s.httpPort), map[string]string{
			"filePath": "rec",
			"fileName": "fixed.mp4",
			"fragment": "1s",
			"duration": "0",
			"type":     "mp4",
		})
		if err != nil {
			return err
		}
		if status == http.StatusOK || strings.Contains(body, "record exists") {
			return nil
		}
		return fmt.Errorf("http %d: %s", status, strings.TrimSpace(body))
	})

	basePath := filepath.Join(recordDir, "fixed.mp4")
	secondPath := filepath.Join(recordDir, "fixed-001.mp4")

	waitFor(t, 30*time.Second, 250*time.Millisecond, "first segment file", func() error {
		return nonEmptyFile(basePath)
	})
	waitFor(t, 30*time.Second, 250*time.Millisecond, "second segment file", func() error {
		return nonEmptyFile(secondPath)
	})

	_, _, _ = postJSON(fmt.Sprintf("http://127.0.0.1:%d/mp4/api/stop/live/test", s.httpPort), nil)

	if err := nonEmptyFile(basePath); err != nil {
		t.Fatalf("first segment after rotation: %v", err)
	}
	if err := nonEmptyFile(secondPath); err != nil {
		t.Fatalf("second segment after stop: %v", err)
	}
}

func nonEmptyFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty", filepath.Base(path))
	}
	return nil
}
```

- [ ] **Step 3: Run it and verify it passes**

Run: `go test ./test -run '^TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite$' -count=1 -v`
Expected: PASS (both `fixed.mp4` and `fixed-001.mp4` exist and are non-empty).

- [ ] **Step 4: Negative control**

Comment out the `r.segmentSeq++` lines added in Task 2 Step 3, then run the
same command. Expected: FAIL — timeout waiting for `fixed-001.mp4`.
Restore the lines and rerun. Expected: PASS.

- [ ] **Step 5: Full verification**

Run, in order:

```bash
gofmt -l recoder.go recoder_segment.go recoder_segment_test.go test/record_segment_name_test.go test/recovery_integration_test.go
go vet . ./plugin/mp4/... ./test
go test -race . ./plugin/mp4/... -count=1
go test ./test -run '^(TestRecordFixedFileNameFragmentSegmentsDoNotOverwrite|TestRecoveryAfterKill)$' -count=1
```

Expected: `gofmt -l` prints nothing; all commands PASS.

- [ ] **Step 6: Commit**

```bash
git add test/recovery_integration_test.go test/record_segment_name_test.go
git commit -m "test(record): pin unique naming across fragment rotations"
```

---

### Task 4: Run the upload finalize step inline when the upload queue rejects the task

**Files:**
- Modify: `plugin/mp4/pkg/record.go:856-872` (`handoffToUploadQueue`)
- Create: `plugin/mp4/pkg/upload_handoff_test.go`

**Interfaces:**
- Consumes: the **existing** helper
  `func enqueueUploadTask(queue task.IJob, u *finalizeUploadTask, logger *slog.Logger) error`
  (`plugin/mp4/pkg/recovery_upload.go:285`, already returns an error on
  `m7s.AddTaskRejected`), `finalizeUploadTask.Go() (err error)`, and the existing
  test helper `newStoppedUploadQueue(t)` (`plugin/mp4/pkg/recovery_upload_test.go:19`).
- Produces: `func enqueueFinalizeOrRunInline(queue task.IJob, ft *finalizeUploadTask, logger *slog.Logger) (inline bool)` in package `mp4`.

**Why:** `handoffToUploadQueue` transfers file ownership to `ft` before calling
`uploadQueueTask.AddTask`, but discards the rejection state (`record.go:869`).
When the queue rejects the task — parent `Work` stopped during shutdown, or the
256-slot event loop channel is full (`gotask.ErrTooManyChildren`) — nobody runs
`ft.Go()`: the file is neither uploaded nor registered for pending retry,
silently lost. The recovery-upload path already solved this with
`enqueueUploadTask`; the recording handoff must use it too.

- [ ] **Step 1: Write the failing test**

Create `plugin/mp4/pkg/upload_handoff_test.go`:

```go
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
```

- [ ] **Step 2: Run it and verify it fails**

Run: `go test ./plugin/mp4/pkg -run '^TestFinalize(RunsInline|StaysQueued)' -count=1`
Expected: FAIL — `undefined: enqueueFinalizeOrRunInline`.

- [ ] **Step 3: Implement `enqueueFinalizeOrRunInline` and call it from `handoffToUploadQueue`**

Add below `handoffToUploadQueue` in `plugin/mp4/pkg/record.go`. It wraps the
existing `enqueueUploadTask` helper; `queue` is a parameter so tests can pass a
stopped queue without touching the global one:

```go
// enqueueFinalizeOrRunInline 把最终持久化任务交给上传队列；队列同步拒绝时就地
// 执行 ft.Go()。拒绝意味着文件所有权已移交却没人执行（shutdown 或事件循环
// 通道满），不就地执行会静默丢失录像；内联路径同时保证 flush/上传/索引/DB
// 副作用只走一份实现，不要复制 Go() 的函数体。
func enqueueFinalizeOrRunInline(queue task.IJob, ft *finalizeUploadTask, logger *slog.Logger) (inline bool) {
	if err := enqueueUploadTask(queue, ft, logger); err != nil {
		ft.Warn("upload queue rejected task; running finalize inline",
			"file", ft.filePath, "size", ft.size, "error", err)
		inline = true
		if goErr := ft.Go(); goErr != nil {
			ft.Error("inline finalize failed", "file", ft.filePath, "err", goErr)
		}
	}
	return
}
```

Replace the last line of `handoffToUploadQueue` (`uploadQueueTask.AddTask(ft, t.Logger)`)
with:

```go
	enqueueFinalizeOrRunInline(&uploadQueueTask, ft, t.Logger)
```

The inline path blocks the trailer queue for the duration of one finalize. That
is deliberate: this branch only fires on shutdown or a saturated event-loop
channel, where preserving the recording outweighs trailer-queue throughput.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test -race ./plugin/mp4/pkg -run '^TestFinalize(RunsInline|StaysQueued)' -count=1`
Expected: PASS.

- [ ] **Step 5: Verify the full package and the call site**

Run:

```bash
gofmt -l plugin/mp4/pkg/record.go plugin/mp4/pkg/upload_handoff_test.go
go vet ./plugin/mp4/...
go test -race ./plugin/mp4/... -count=1
```

Expected: `gofmt -l` prints nothing; all commands PASS.

- [ ] **Step 6: Commit**

```bash
git add plugin/mp4/pkg/record.go plugin/mp4/pkg/upload_handoff_test.go
git commit -m "fix(mp4): never drop a recording when the upload queue rejects the finalize task"
```

## Compatibility notes

- The first segment's object key is unchanged, so existing caller URLs stay valid.
- No caller change is required (checked against `xde-media-spring-boot-starter`):
  the request still carries `fileName`, `StopRecord` matches by `StreamPath` only
  (`plugin/mp4/api.go:893`), and `StartRecord` idempotency matches
  `RecConf.FilePath` + `RecConf.FileName` (`plugin/mp4/api.go:811`) — both fields
  are left untouched by this change. The `-001` suffix also matches the caller's
  `RECORDING_FILE_NAME_PATTERN` (`(-\d{3}){0,3}` suffix groups).
- The canonical key now always holds segment 1 instead of the last writer. In the
  2026-11-22 incident that turns the 10.45 s object into the 58.8-minute one; the
  trailing ~70 s of retries live under `-001`..`-003`, visible in
  `/mp4/api/list` but not behind the caller's stored URL.
- A segment that fails after `CreateStream` returns (e.g. `st.CreateFile` or
  `WriteInitSegment` fails in `plugin/mp4/pkg/record.go:1062-1076`) has already
  consumed its number, so the first successfully written segment may start at
  `-001`. Accepted trade-off: closing that gap needs a per-plugin write-commit
  signal, and the caller already compensates by starting a new recording when its
  expected object is missing.
- Later segments appear as `<requested-stem>-001.mp4`, `-002.mp4`, … in the same
  directory/prefix; `/mp4/api/list` and `/mp4/api/delete` operate per DB row and
  already handle multiple rows for one stream.
- The fix lives in the shared recorder, so flv and hls recordings that specify a
  fixed `fileName` also stop overwriting on retry/rotation. Recordings without a
  fixed `fileName` are untouched.
- Total stored bytes are unchanged; only the object count grows by the number of
  retries/rotations (4 in the 2026-11-22 incident).
- Business-side playback of a full hour needs to aggregate the segments; that is
  a follow-up outside this plan.
