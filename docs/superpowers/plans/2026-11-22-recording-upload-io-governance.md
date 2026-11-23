# Recording Upload IO Governance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound the disk read/write pressure that recording finalization (trailer rewrite + object upload) puts on the host, so a synchronized finalize storm cannot starve the live recording write path and force ring-buffer discards.

**Architecture:** Add one process-wide token-bucket rate limiter for upload reads (S3), make the S3 SDK's multipart concurrency configurable with a lower default, bound the in-memory finalize backlog and spill the overflow to the durable pending-retry queue, and make the effective IO settings visible at startup with warnings for untuned deployments. Recording-side buffering stays a publisher-level configuration (`global.publish.buffertime`) — this plan warns about it instead of resizing a ring shared with live subscribers.

**Tech Stack:** Go 1.26, `golang.org/x/time/rate` (already in `go.sum` at v0.14.0), AWS SDK Go v1 `s3manager`, gotask task system, existing `pkg/storage` upload manager and `plugin/mp4` trailer/upload queues.

**Spec:** No separate spec. "Background" below is the authoritative incident record. The companion stop-loss fix is `docs/superpowers/plans/2026-11-22-record-segment-unique-naming.md` (unique segment names); this plan is its root-cause follow-up.

## Background (incident evidence, 2026-11-22)

- 36 cameras recorded 60 minutes each and all stopped within the same minute.
- Between 20:46:16 and 20:49:51 the server uploaded 59 files totalling 28.9 GiB (31,081,701,783 bytes); 28 of the 59 uploads started while all 4 upload slots were busy (`active=4/4`).
- `MaxConcurrentUploads` is 4 (`pkg/storage/upload_manager.go:48`). The AWS SDK uploader is created with only `PartSize` set (`pkg/storage/s3.go:212-216`), so its default `Concurrency=5` is in force: up to 4 × 5 concurrent 64 MB range reads against the same disk.
- There is no upload read-rate limit. `TrailerWriteRateMBps` defaults to 0 (unlimited) and only wraps the trailer rewrite's *write* side (`pkg/storage/throttle.go`), not upload reads.
- The record subscriber writes synchronously: frame callback → `PlayBlock` → `muxer.WriteSample` → `S3File.Write` (1 MB bufio, `pkg/storage/s3.go:404`). When disk write latency spikes, the subscriber falls behind the publisher ring (~1 GOP), `pkg/av_reader.go:59-82` jumps to the newest IDR (`reader too slow`), and finally `ErrDiscard` ends the segment; `RecordJob` retries in place.
- 1122 log lines: 150 `reader too slow` between 20:46:19 and 20:48:29, 23 `ErrDiscard`.

The unique-naming fix removes the data-loss consequence of the retry. It does not remove the retries themselves; this plan reduces their probability.

## Non-goals

- **Async recording writes** (copy samples off the ring into a writer task): sample payloads are recycled ring memory, so this needs a full payload-copy pipeline. Separate project.
- **Resizing the publisher ring from the recorder**: the ring is created from `config.Publish.RingSize`/`BufferTime` (`pkg/track.go:89-90`) and shared by every subscriber. Record-only resizing would change live playback semantics. Task 5 warns about untuned values; the deployment sets `global.publish.buffertime`.
- **Record-start disk-free guard**: uploads *free* disk space (the S3 temp file is deleted after a successful upload), so gating uploads on low free space is counterproductive. `upload.pendingdiskminfreemb` already guards the pending fallback; a record-start free-space guard is a separate follow-up.
- **OSS/COS read throttling**: their SDKs upload from a file path internally (`pkg/storage/oss.go:395`, `pkg/storage/cos.go:399`), so a reader wrapper cannot be applied. The global slot count bounds them; only S3 gets the byte-rate limiter.
- **Choosing production values for the new knobs**: this plan makes the knobs exist, safe at 0, and visible at startup. The right MB/s depends on the host's disk and network; values must be validated on the target hardware.

## Global Constraints

- Branch from `main`; PR base is `main`.
- `golang.org/x/time` is an indirect dependency today (`go.mod:157`); after importing `golang.org/x/time/rate`, run `go mod tidy` so it is listed as a direct dependency at v0.14.0.
- New settings live in `storage.UploadConfig` (YAML path `global.upload.*`), lowercase field names, with `default`/`desc` tags.
- `0` means disabled/unchanged for every new knob: rate limiter returns the original reader, queue cap is unlimited, SDK concurrency stays at the SDK default.
- Never throttle or block the recording write path; only finalization (trailer rewrite, upload) may be paced or deferred.
- `go test -race` on touched packages; `gofmt -w` changed files.
- `pkg/storage/s3.go` and `oss.go`/`cos.go` are build-tag gated (`//go:build s3` etc.). Every build, vet, or test that touches an S3 symbol must pass `-tags s3`; the untagged `./pkg/storage` run does not compile those files.
- Commit messages: Conventional Commits, English. Docs and comments in English.
- **Dependency:** this plan runs after `2026-11-22-record-segment-unique-naming.md` is merged. Task 3 reuses two symbols that plan introduces: `finalizeUploadTask` inline execution via `enqueueFinalizeOrRunInline` (rejection must run the task, otherwise a reserved queue slot leaks) and the test fake `fakeFinalizeFile` in `plugin/mp4/pkg/upload_handoff_test.go`.

## Review Focus

- A rate of 0 returns the exact same `io.ReadSeeker` value (no wrapper, no extra syscall, no latency) — Task 1, `TestUploadThrottledReadSeekerZeroRatePassthrough`.
- A paced read observes the upload context deadline instead of blocking past it — Task 1, `TestUploadThrottledReaderDeadline`.
- N concurrent uploads consume from one shared bucket, so aggregate read throughput is at most the configured rate, not N × rate — Task 1, `TestUploadThrottledReadersShareOneBucket`.
- S3 `Concurrency` unset (0) keeps today's SDK default 5; negative values are clamped to ≥ 1 — Task 2, `TestNewS3UploaderAppliesConfig`.
- `MaxQueuedUploads` 0 means unlimited; overflow spills only for object storage with a database retry queue — local storage and DB-less deployments keep the current path — Task 3, `TestUploadBacklogGateSkipsLocalAndDBLess`, `TestUploadQueueUnlimitedWhenZero`.
- The queue slot is released on success, on upload failure, and when the queue rejects the task and it runs inline — Task 3, `TestFinalizeReleasesUploadQueue`, `TestFinalizeReleasesUploadQueueOnFailure`, `TestFinalizeReleasesUploadQueueWhenRunInline`.
- Spilling must not close the file, because `Close` is what triggers the upload and deletes the temp copy — Task 3, `TestSpillToPendingDoesNotCloseFile`.
- A record-heavy node with object storage and `publish.buffertime == 0` is told about it at startup, not silently left untuned — Task 5, `TestRecordingIOWarnings`.

---

### Task 1: Process-wide upload read-rate limiter

**Files:**
- Modify: `pkg/storage/upload_manager.go` (config field, limiter init, chunk constant)
- Create: `pkg/storage/throttle_reader.go`
- Test: `pkg/storage/throttle_reader_test.go`
- Modify: `pkg/storage/s3.go:632` (wrap the upload body)
- Modify: `go.mod` / `go.sum` (`go mod tidy`)

**Interfaces:**
- Consumes: `UploadConfig` (`pkg/storage/upload_manager.go:46-55`), `InitUploadManager` (`:57`), `uploadCtx` in `S3File.uploadTempFile` (`pkg/storage/s3.go:590`).
- Produces:
  - `UploadConfig.UploadReadRateMBps int` (`default:"0"`, 0 = unlimited)
  - `func NewUploadThrottledReadSeeker(ctx context.Context, rs io.ReadSeeker) io.ReadSeeker` — package `storage`; returns `rs` unchanged when no limiter is configured. When `rs` also implements `io.ReaderAt`, the returned value keeps that capability (the AWS SDK switches to concurrent `io.NewSectionReader` range reads only for `io.ReaderAt`+`io.ReadSeeker` bodies; hiding `ReaderAt` would silently serialize multipart reads).
  - unexported: `uploadReadLimiter *rate.Limiter`, `maxThrottleChunk = 1 << 20`.

- [ ] **Step 1: Write the failing tests**

Create `pkg/storage/throttle_reader_test.go`:

```go
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestInitUploadManagerConfiguresReadLimiter(t *testing.T) {
	t.Cleanup(func() { InitUploadManager(UploadConfig{PendingDir: t.TempDir()}) })
	InitUploadManager(UploadConfig{PendingDir: t.TempDir(), UploadReadRateMBps: 100})
	if uploadReadLimiter == nil {
		t.Fatal("read rate 100 MB/s must install a limiter")
	}
	InitUploadManager(UploadConfig{PendingDir: t.TempDir()})
	if uploadReadLimiter != nil {
		t.Fatal("read rate 0 must leave no limiter (zero overhead)")
	}
}

func TestUploadThrottledReadSeekerZeroRatePassthrough(t *testing.T) {
	InitUploadManager(UploadConfig{PendingDir: t.TempDir()}) // UploadReadRateMBps = 0
	rs := bytes.NewReader(make([]byte, 16))
	if got := NewUploadThrottledReadSeeker(context.Background(), rs); got != io.ReadSeeker(rs) {
		t.Fatalf("zero rate must return the original reader, got %T", got)
	}
}

func TestUploadThrottledReadSeekerPreservesReaderAt(t *testing.T) {
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<30), 8<<20)
	defer func() { uploadReadLimiter = nil }()

	src := bytes.NewReader(make([]byte, 4096))
	wrapped := NewUploadThrottledReadSeeker(context.Background(), src)
	ra, ok := wrapped.(io.ReaderAt)
	if !ok {
		t.Fatal("wrapper must keep io.ReaderAt so the S3 SDK can use concurrent range reads")
	}
	buf := make([]byte, 4096)
	if n, err := ra.ReadAt(buf, 0); n != 4096 || err != nil {
		t.Fatalf("ReadAt = (%d, %v), want (4096, nil)", n, err)
	}
}

func TestUploadThrottledReadersShareOneBucket(t *testing.T) {
	// 1 GiB/s so the test never sleeps; the assertion is about token accounting,
	// not wall-clock time.
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<30), 8<<20)
	defer func() { uploadReadLimiter = nil }()

	ctx := context.Background()
	buf := make([]byte, 1<<20) // exactly maxThrottleChunk
	for i := 0; i < 4; i++ {
		rs := bytes.NewReader(make([]byte, len(buf)))
		if _, err := io.ReadFull(NewUploadThrottledReadSeeker(ctx, rs), buf); err != nil {
			t.Fatalf("reader %d: %v", i, err)
		}
	}
	// Four 1 MB reads must have consumed ~4 MB from the single shared bucket.
	if tokens := uploadReadLimiter.Tokens(); tokens > 5<<20 {
		t.Fatalf("tokens left = %.0f, want <= %d (readers must share one bucket)", tokens, 5<<20)
	}
}

func TestUploadThrottledReaderDeadline(t *testing.T) {
	// 1 B/s with a 1 MiB burst. Drain the burst first: a fresh bucket would let
	// the read through immediately. The next 1 MiB read cannot finish in 20 ms.
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1), 1<<20)
	defer func() { uploadReadLimiter = nil }()
	if err := uploadReadLimiter.WaitN(context.Background(), 1<<20); err != nil {
		t.Fatalf("drain burst: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	rs := bytes.NewReader(make([]byte, maxThrottleChunk))
	_, err := NewUploadThrottledReadSeeker(ctx, rs).Read(make([]byte, maxThrottleChunk))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestUploadThrottledReadSeekerSeekPassthrough(t *testing.T) {
	uploadReadLimiter = rate.NewLimiter(rate.Limit(1<<30), 1<<20)
	defer func() { uploadReadLimiter = nil }()

	rs := bytes.NewReader(make([]byte, 1024))
	wrapped := NewUploadThrottledReadSeeker(context.Background(), rs)
	off, err := wrapped.Seek(512, io.SeekStart)
	if err != nil || off != 512 {
		t.Fatalf("Seek = (%d, %v), want (512, nil)", off, err)
	}
}
```

- [ ] **Step 2: Run them and verify they fail**

Run: `go test ./pkg/storage -run '^TestUploadThrottled' -count=1`
Expected: FAIL — `undefined: NewUploadThrottledReadSeeker`, `undefined: uploadReadLimiter`, `undefined: maxThrottleChunk`.

- [ ] **Step 3: Implement `pkg/storage/throttle_reader.go`**

Use `golang.org/x/time/rate` for a shared token bucket. Read in at most
`maxThrottleChunk` pieces so the bucket's burst never has to exceed the largest
single wait; `WaitN` before each read makes the limiter cumulative and shared
across goroutines (unlike the single-threaded `throttledWriter` in
`throttle.go`).

```go
package storage

import (
	"context"
	"errors"
	"io"

	"golang.org/x/time/rate"
)

const maxThrottleChunk = 1 << 20

// uploadReadLimiter is the process-wide upload read token bucket; nil means
// unconfigured (passthrough).
var uploadReadLimiter *rate.Limiter

// NewUploadThrottledReadSeeker wraps rs with the global upload read limiter.
// With no limiter configured it returns rs unchanged (zero overhead).
//
// The static return type is io.ReadSeeker, but the dynamic type keeps ReadAt
// when rs supports it: the AWS SDK only uses concurrent io.NewSectionReader
// range reads for a Body implementing both io.ReaderAt and io.ReadSeeker.
// Returning a plain ReadSeeker would silently serialize multipart reads.
func NewUploadThrottledReadSeeker(ctx context.Context, rs io.ReadSeeker) io.ReadSeeker {
	if uploadReadLimiter == nil {
		return rs
	}
	w := &uploadThrottledReadSeeker{ctx: ctx, rs: rs}
	if ra, ok := rs.(io.ReaderAt); ok {
		w.ra = ra
	}
	return w
}

type uploadThrottledReadSeeker struct {
	ctx context.Context
	rs  io.ReadSeeker
	ra  io.ReaderAt // nil when the source cannot read at offsets
}

func (r *uploadThrottledReadSeeker) Read(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > maxThrottleChunk {
			chunk = maxThrottleChunk
		}
		if err := uploadReadLimiter.WaitN(r.ctx, chunk); err != nil {
			return total, err
		}
		n, err := r.rs.Read(p[total : total+chunk])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

func (r *uploadThrottledReadSeeker) ReadAt(p []byte, off int64) (int, error) {
	if r.ra == nil {
		return 0, errors.New("storage: source does not support ReadAt")
	}
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > maxThrottleChunk {
			chunk = maxThrottleChunk
		}
		if err := uploadReadLimiter.WaitN(r.ctx, chunk); err != nil {
			return total, err
		}
		n, err := r.ra.ReadAt(p[total:total+chunk], off+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

func (r *uploadThrottledReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.rs.Seek(offset, whence)
}
```

- [ ] **Step 4: Wire the config field and limiter init**

In `UploadConfig` (`pkg/storage/upload_manager.go:46-55`) add, after
`MaxConcurrentUploads`:

```go
	UploadReadRateMBps         int    `desc:"upload read rate limit in MB/s; caps concurrent upload disk reads so they cannot starve recording writes; 0 = unlimited (default)" default:"0"`
```

In `InitUploadManager`, next to the trailer rate block (`:70-75`):

```go
	if cfg.UploadReadRateMBps > 0 {
		bytesPerSec := int64(cfg.UploadReadRateMBps) * 1024 * 1024
		burst := bytesPerSec / 4 // 250 ms of headroom
		if burst < 1<<20 {
			burst = 1 << 20
		}
		if burst > 32<<20 {
			burst = 32 << 20
		}
		uploadReadLimiter = rate.NewLimiter(rate.Limit(bytesPerSec), int(burst))
	} else {
		uploadReadLimiter = nil
	}
```

Extend the `[storage] upload manager initialized: ...` log line (`:92-93`) with
`readRate=%dMB/s`.

Import `golang.org/x/time/rate` in `upload_manager.go`.

- [ ] **Step 5: Wrap the S3 upload body**

In `pkg/storage/s3.go`, inside the `uploadFn` retry closure, replace
`Body: w.tempFile,` with:

```go
				Body:        NewUploadThrottledReadSeeker(ctx, w.tempFile),
```

Use the closure's `ctx` (the per-attempt `WithTimeout` context), not the outer
`uploadCtx` — `uploadCtx` is `context.WithoutCancel` and would let a limiter
wait outlive the attempt timeout.

- [ ] **Step 6: Run the tests and tidy the module**

Run:

```bash
go mod tidy
go test -race ./pkg/storage -run '^TestUploadThrottled|^TestInitUploadManagerConfiguresReadLimiter' -count=1
go test -race -tags s3 ./pkg/storage -run '^TestUploadThrottled|^TestInitUploadManagerConfiguresReadLimiter' -count=1
go vet -tags s3 ./pkg/storage
go build -tags s3 ./...
```

Expected: PASS; `go.mod` lists `golang.org/x/time v0.14.0` without `// indirect`.

- [ ] **Step 7: Commit**

```bash
git add pkg/storage/upload_manager.go pkg/storage/throttle_reader.go pkg/storage/throttle_reader_test.go pkg/storage/s3.go go.mod go.sum
git commit -m "feat(storage): add a process-wide upload read rate limiter"
```

---

### Task 2: Configurable S3 multipart concurrency

**Files:**
- Modify: `pkg/storage/s3.go:38-61` (`S3StorageConfig`), `pkg/storage/s3.go:211-216` (uploader creation)
- Test: `pkg/storage/s3_uploader_test.go` (must start with `//go:build s3`)

**Interfaces:**
- Consumes: `S3StorageConfig`, `s3manager.NewUploader`.
- Produces:
  - `S3StorageConfig.Concurrency int` (`default:"2"`)
  - `func newS3Uploader(sess *session.Session, cfg *S3StorageConfig) *s3manager.Uploader` (unexported; used by `NewS3Storage` and the test)

- [ ] **Step 1: Write the failing test**

Create `pkg/storage/s3_uploader_test.go`:

```go
//go:build s3

package storage

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
)

func newTestS3Session(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Credentials: credentials.NewStaticCredentials("test", "test", ""),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func TestNewS3UploaderAppliesConfig(t *testing.T) {
	sess := newTestS3Session(t)

	u := newS3Uploader(sess, &S3StorageConfig{PartSize: 32 << 20, Concurrency: 2})
	if u.PartSize != 32<<20 {
		t.Fatalf("PartSize = %d, want %d", u.PartSize, 32<<20)
	}
	if u.Concurrency != 2 {
		t.Fatalf("Concurrency = %d, want 2", u.Concurrency)
	}

	// Zero keeps the SDK default (5): the runtime default 2 comes from the
	// `default:"2"` tag, applied by the config system, not by a struct literal.
	u = newS3Uploader(sess, &S3StorageConfig{})
	if u.Concurrency != 5 {
		t.Fatalf("default Concurrency = %d, want 5", u.Concurrency)
	}

	// Negative is clamped to 1: a zero/negative SDK concurrency panics or stalls.
	u = newS3Uploader(sess, &S3StorageConfig{Concurrency: -3})
	if u.Concurrency != 1 {
		t.Fatalf("clamped Concurrency = %d, want 1", u.Concurrency)
	}
}
```

- [ ] **Step 2: Run it and verify it fails**

Run: `go test -tags s3 ./pkg/storage -run '^TestNewS3UploaderAppliesConfig$' -count=1`
Expected: FAIL — `undefined: newS3Uploader`.

- [ ] **Step 3: Implement**

In `S3StorageConfig` (after `PartSize`, `pkg/storage/s3.go:52`) add:

```go
	Concurrency     int           `desc:"number of concurrent multipart part uploads; lower values reduce concurrent disk read pressure" default:"2"`
```

Replace the inline uploader construction (`pkg/storage/s3.go:212-216`) with:

```go
	uploader := newS3Uploader(sess, cfg)
```

and add below `NewS3Storage`:

```go
func newS3Uploader(sess *session.Session, cfg *S3StorageConfig) *s3manager.Uploader {
	return s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
		if cfg.PartSize > 0 {
			u.PartSize = cfg.PartSize
		}
		if cfg.Concurrency > 0 {
			u.Concurrency = cfg.Concurrency
		} else if cfg.Concurrency < 0 {
			u.Concurrency = 1
		}
	})
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -race -tags s3 ./pkg/storage -run '^TestNewS3UploaderAppliesConfig$' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/storage/s3.go pkg/storage/s3_uploader_test.go
git commit -m "feat(storage): make S3 multipart concurrency configurable (default 2)"
```

---

### Task 3: Bounded finalize backlog with durable pending spill

**Files:**
- Modify: `pkg/storage/upload_manager.go` (cap, counter, acquire/release)
- Test: `pkg/storage/upload_manager_test.go`
- Modify: `plugin/mp4/pkg/record.go` (`handoffToUploadQueue` tail and the
  `finalizeUploadTask` spill helper)
- Test: `plugin/mp4/pkg/upload_backlog_test.go`

**Interfaces:**
- Consumes: the `writeTrailerTask.storageKey` / `.db` fields, the
  `finalizeUploadTask.onFail` / `.onDone` hooks, the existing `onFail` closures
  (`recoverLocalFile` / `handleFailedUpload`), and the naming plan's
  `enqueueFinalizeOrRunInline(&uploadQueueTask, ft, t.Logger)`.
- Produces:
  - `UploadConfig.MaxQueuedUploads int` (`default:"8"`, 0 = unlimited)
  - `func TryAcquireUploadQueue() bool`, `func ReleaseUploadQueue()`, `func QueuedUploads() int32` in `pkg/storage`
  - `func uploadBacklogFullFor(storageKey string, hasDB, backlogFull bool) bool` in `plugin/mp4/pkg`
  - `func (u *finalizeUploadTask) spillToPending(cause error)` in `plugin/mp4/pkg`

- [ ] **Step 1: Write the failing storage test**

Create `pkg/storage/upload_manager_test.go`:

```go
package storage

import "testing"

func TestUploadQueueCap(t *testing.T) {
	InitUploadManager(UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 2})

	if !TryAcquireUploadQueue() || !TryAcquireUploadQueue() {
		t.Fatal("first two acquisitions must succeed")
	}
	if TryAcquireUploadQueue() {
		t.Fatal("third acquisition must fail at cap 2")
	}
	ReleaseUploadQueue()
	if QueuedUploads() != 1 {
		t.Fatalf("QueuedUploads = %d, want 1", QueuedUploads())
	}
	if !TryAcquireUploadQueue() {
		t.Fatal("released slot must be reusable")
	}
	ReleaseUploadQueue()
	ReleaseUploadQueue()
}

func TestUploadQueueUnlimitedWhenZero(t *testing.T) {
	InitUploadManager(UploadConfig{PendingDir: t.TempDir(), MaxConcurrentUploads: 1, MaxQueuedUploads: 0})
	for i := 0; i < 100; i++ {
		if !TryAcquireUploadQueue() {
			t.Fatalf("acquisition %d failed; 0 must mean unlimited", i)
		}
	}
	for i := 0; i < 100; i++ {
		ReleaseUploadQueue()
	}
}
```

- [ ] **Step 2: Run it and verify it fails**

Run: `go test ./pkg/storage -run '^TestUploadQueue' -count=1`
Expected: FAIL — `undefined: TryAcquireUploadQueue` (and the other new symbols).

- [ ] **Step 3: Implement the queue counter in `upload_manager.go`**

Add to the package vars:

```go
	// maxQueuedUploads bounds final-persistence tasks that are queued but not yet
	// finished; 0 = unlimited. When full, the caller hands the file straight to
	// the durable pending-retry queue (on disk, survives restart) instead of
	// piling hundreds of in-memory tasks up during a finalize storm.
	maxQueuedUploads int
	queuedUploads    atomic.Int32
```

Add to `UploadConfig`:

```go
	MaxQueuedUploads           int    `desc:"maximum queued-but-unfinished final persistence tasks; overflow spills to the pending retry queue; 0 = unlimited" default:"8"`
```

In `InitUploadManager`, set `maxQueuedUploads = cfg.MaxQueuedUploads` and reset
`queuedUploads.Store(0)`.

Add:

```go
// TryAcquireUploadQueue reserves one queued-upload slot; always succeeds when
// the cap is <= 0.
func TryAcquireUploadQueue() bool {
	if maxQueuedUploads <= 0 {
		queuedUploads.Add(1)
		return true
	}
	for {
		cur := queuedUploads.Load()
		if int(cur) >= maxQueuedUploads {
			return false
		}
		if queuedUploads.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// ReleaseUploadQueue returns one slot; every task must call it after success,
// failure, or inline execution.
func ReleaseUploadQueue() {
	queuedUploads.Add(-1)
}

// QueuedUploads reports the number of reserved slots.
func QueuedUploads() int32 {
	return queuedUploads.Load()
}
```

Extend the init log line with `maxQueued=%d`.

- [ ] **Step 4: Write the failing mp4 tests**

Create `plugin/mp4/pkg/upload_backlog_test.go`:

```go
package mp4

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"m7s.live/v5/pkg/storage"
)

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

type closeErrorFile struct{ fakeFinalizeFile }

func (f *closeErrorFile) Close() error {
	f.closed = true
	return errors.New("close failed")
}

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
```

`fakeFinalizeFile` is defined by the companion naming plan in
`plugin/mp4/pkg/upload_handoff_test.go`; do not redefine it. If that file is
absent, stop and report a missing dependency instead of inventing a duplicate.

- [ ] **Step 5: Run the mp4 tests and verify they fail**

Run: `go test ./plugin/mp4/pkg -run '^(TestUploadBacklog|TestSpill|TestFinalizeReleases)' -count=1`
Expected: FAIL — `undefined: uploadBacklogFullFor`, `undefined: errUploadBacklogFull`, `undefined: spillToPending`.

- [ ] **Step 6: Implement the mp4 side**

In `plugin/mp4/pkg/record.go`, add near `handoffToUploadQueue`:

```go
var errUploadBacklogFull = errors.New("mp4: upload backlog full")

// uploadBacklogFullFor reports a spill: object storage, a DB retry queue, and no
// free queue slot. Only then is handing the file to the durable pending queue
// safer than holding it in memory. Local storage already has the file in place,
// and without a DB there is no retry queue (a spill would delete the only copy).
func uploadBacklogFullFor(storageKey string, hasDB, backlogFull bool) bool {
	return storageKey != "local" && hasDB && backlogFull
}

// spillToPending abandons this upload and lets onFail register the pending
// retry. It must not call Close: the upload never started, and Close would
// trigger it and delete the temp file. Registration is a DB insert plus a
// rename, so running it on the trailer queue is bounded work — unlike running
// the upload itself.
func (u *finalizeUploadTask) spillToPending(cause error) {
	if u.onDone != nil {
		defer u.onDone()
	}
	u.Warn("upload backlog full; deferring to pending retry",
		"file", u.filePath, "size", u.size, "cause", cause)
	u.file = nil
	if u.onFail != nil {
		u.onFail(cause)
	}
}
```

The tail of `handoffToUploadQueue` (after the existing struct literal and
`t.file = nil`) becomes exactly:

```go
	t.logUploadEnqueued(size)

	// Only object-storage finalization reads the recording back from disk;
	// a local Close is a rename and must not consume a backlog slot.
	objectStorage := t.storageKey != "local"
	acquired := objectStorage && storage.TryAcquireUploadQueue()
	if acquired {
		ft.onDone = storage.ReleaseUploadQueue
	}
	if uploadBacklogFullFor(t.storageKey, t.db != nil, objectStorage && !acquired) {
		ft.spillToPending(errUploadBacklogFull)
		return
	}
	// local, or object storage with no DB retry queue when full: keep the old
	// path and let the in-memory queue absorb it (no slot to release).
	enqueueFinalizeOrRunInline(&uploadQueueTask, ft, t.Logger)
```

Decision table this implements:

| storage | DB | slot acquired | action |
|---|---|---|---|
| local | any | never | enqueue (unchanged path) |
| object | yes | yes | enqueue, release slot in `onDone` |
| object | yes | no | spill to pending, no slot held |
| object | no | no | enqueue without a slot (old behavior; no retry queue exists) |

- [ ] **Step 7: Run the tests**

Run: `go test -race ./pkg/storage ./plugin/mp4/pkg -run '^(TestUploadQueue|TestUploadBacklog|TestSpill|TestFinalizeReleases)' -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add pkg/storage/upload_manager.go pkg/storage/upload_manager_test.go plugin/mp4/pkg/record.go plugin/mp4/pkg/upload_backlog_test.go
git commit -m "feat(mp4): bound the upload backlog and spill overflow to pending"
```

---

### Task 4: Startup IO diagnostics

**Files:**
- Create: `pkg/storage/io_diagnostics.go`
- Test: `pkg/storage/io_diagnostics_test.go`
- Modify: `pkg/storage/upload_manager.go` (`InitUploadManager` logs the warnings)
- Modify: `CLAUDE.md` (Common Pitfalls entry)

**Interfaces:**
- Consumes: `UploadConfig`.
- Produces: `func IOWarnings(cfg UploadConfig) []string`.

- [ ] **Step 1: Write the failing test**

Create `pkg/storage/io_diagnostics_test.go`:

```go
package storage

import (
	"strings"
	"testing"
)

func TestIOWarnings(t *testing.T) {
	tests := []struct {
		name string
		cfg  UploadConfig
		want []string // one substring per expected warning
	}{
		{
			name: "fully tuned",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, UploadReadRateMBps: 128, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
		},
		{
			name: "zero read rate",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
			want: []string{"readratembps"},
		},
		{
			name: "zero trailer rate",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, UploadReadRateMBps: 128, MaxQueuedUploads: 8},
			want: []string{"trailerwriteratembps"},
		},
		{
			name: "unbounded queue",
			cfg:  UploadConfig{MaxConcurrentUploads: 4, UploadReadRateMBps: 128, TrailerWriteRateMBps: 64},
			want: []string{"maxqueueduploads"},
		},
		{
			name: "too many upload slots",
			cfg:  UploadConfig{MaxConcurrentUploads: 6, UploadReadRateMBps: 128, TrailerWriteRateMBps: 64, MaxQueuedUploads: 8},
			want: []string{"maxconcurrentuploads"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings := IOWarnings(tt.cfg)
			if len(warnings) != len(tt.want) {
				t.Fatalf("got %d warnings %q, want %d", len(warnings), warnings, len(tt.want))
			}
			got := strings.Join(warnings, "\n")
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Fatalf("warnings %q do not contain %q", got, want)
				}
			}
		})
	}
}
```

- [ ] **Step 2: Run it and verify it fails**

Run: `go test ./pkg/storage -run '^TestIOWarnings$' -count=1`
Expected: FAIL — `undefined: IOWarnings`.

- [ ] **Step 3: Implement `pkg/storage/io_diagnostics.go`**

```go
package storage

import "fmt"

// IOWarnings returns warnings for untuned IO settings (English, for log
// searches). It only inspects its argument, never global state, so it is easy
// to unit test.
func IOWarnings(cfg UploadConfig) []string {
	var out []string
	if cfg.UploadReadRateMBps <= 0 {
		out = append(out, "upload.readratembps is 0 (unlimited): concurrent uploads can saturate disk reads and starve recording writes")
	}
	if cfg.TrailerWriteRateMBps <= 0 {
		out = append(out, "upload.trailerwriteratembps is 0 (unlimited): trailer rewrites can burst the disk")
	}
	if cfg.MaxQueuedUploads <= 0 {
		out = append(out, "upload.maxqueueduploads is 0 (unbounded): a finalize storm can pile up hundreds of in-memory tasks")
	}
	if cfg.MaxConcurrentUploads > 4 {
		out = append(out, fmt.Sprintf("upload.maxconcurrentuploads=%d exceeds the recommended maximum 4", cfg.MaxConcurrentUploads))
	}
	return out
}
```

- [ ] **Step 4: Log the warnings in `InitUploadManager`**

After the existing init log line:

```go
	for _, warning := range IOWarnings(cfg) {
		log.Printf("[storage] WARNING: %s", warning)
	}
```

- [ ] **Step 5: Run the tests**

Run: `go test -race ./pkg/storage -run '^TestIOWarnings$' -count=1`
Expected: PASS.

- [ ] **Step 6: Update `CLAUDE.md`**

Add a Common Pitfalls bullet (English) that records:

- The 2026-11-22 incident shape: 36 synchronized 60-minute recordings, 28.9 GiB
  uploaded in ~3.5 min, 4 upload slots saturated, SDK default 5×64 MB range
  reads per slot, `reader too slow` + `ErrDiscard` on the record subscribers.
- The knobs: `global.upload.readratembps` (start 128–256 on SATA/NVMe shared with
  recording), `trailerwriteratembps` (64–128), `maxconcurrentuploads` (2–4),
  `maxqueueduploads` (8), `global.storage.s3.concurrency` (2), and
  `global.publish.buffertime` (3–5 s) for record-heavy nodes.
- That `maxqueueduploads` spills overflow to `pending_uploads` and relies on the
  `UploadRetryScheduler` (5-minute tick), so a spill is durable but deliberately
  delayed.
- That values must be validated on the target hardware; the plan only makes the
  knobs exist and safe.

- [ ] **Step 7: Commit**

```bash
git add pkg/storage/io_diagnostics.go pkg/storage/io_diagnostics_test.go pkg/storage/upload_manager.go CLAUDE.md
git commit -m "feat(storage): warn about untuned upload IO settings at startup"
```

---

### Task 5: Record-side buffer guardrail

**Files:**
- Create: `plugin/mp4/recording_io_warnings.go`
- Test: `plugin/mp4/recording_io_warnings_test.go`
- Modify: `plugin/mp4/index.go` (`MP4Plugin.Start`)

**Interfaces:**
- Consumes: `(*m7s.Plugin).GetCommonConf().Publish.BufferTime` and
  `(*m7s.Server).GetStorageStatus().ActiveType`.
- Produces: `func recordingIOWarnings(publishBuffer time.Duration, storageKey string) []string`.

- [ ] **Step 1: Write the failing test**

Create `plugin/mp4/recording_io_warnings_test.go`:

```go
package plugin_mp4

import (
	"strings"
	"testing"
	"time"
)

func TestRecordingIOWarnings(t *testing.T) {
	if got := recordingIOWarnings(3*time.Second, "s3"); len(got) != 0 {
		t.Fatalf("tuned config returned warnings: %v", got)
	}
	for _, storageKey := range []string{"s3", "oss", "cos"} {
		got := strings.Join(recordingIOWarnings(0, storageKey), "\n")
		if !strings.Contains(got, "publish.buffertime") {
			t.Fatalf("%s with 0 buffer must warn about publish.buffertime, got %q", storageKey, got)
		}
	}
	if got := recordingIOWarnings(0, "local"); len(got) != 0 {
		t.Fatalf("local storage must not warn, got %v", got)
	}
	if got := recordingIOWarnings(0, ""); len(got) != 0 {
		t.Fatalf("unknown storage must not warn, got %v", got)
	}
}
```

- [ ] **Step 2: Run it and verify it fails**

Run: `go test ./plugin/mp4 -run '^TestRecordingIOWarnings$' -count=1`
Expected: FAIL — `undefined: recordingIOWarnings`.

- [ ] **Step 3: Implement `plugin/mp4/recording_io_warnings.go`**

```go
package plugin_mp4

import (
	"fmt"
	"time"
)

// recordingIOWarnings flags an untuned ring buffer on record-heavy deployments.
// The ring is created by the publisher from config.Publish (pkg/track.go) and
// shared by every subscriber, so the recorder cannot enlarge it on its own:
// warn, do not rewrite configuration.
func recordingIOWarnings(publishBuffer time.Duration, storageKey string) []string {
	if storageKey == "" || storageKey == "local" || publishBuffer > 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"record with %s storage and publish.buffertime=0: the shared publisher ring holds ~1 GOP, so a short disk stall forces reader jumps/discards; consider global.publish.buffertime: 3s-5s",
		storageKey)}
}
```

- [ ] **Step 4: Wire it into `MP4Plugin.Start`**

`server.initStorage()` runs at `server.go:320`, before plugins start, so
`p.Server.GetStorageStatus().ActiveType` is populated here. Insert immediately
after `p.applyRecordIndexConfig()` in `plugin/mp4/index.go` `Start`:

```go
	storageType := ""
	if p.Server != nil {
		storageType = p.Server.GetStorageStatus().ActiveType
	}
	for _, warning := range recordingIOWarnings(p.GetCommonConf().Publish.BufferTime, storageType) {
		p.Warn("recording IO configuration", "warning", warning)
	}
```

`p.GetCommonConf()` is the accessor already used by `publisher.go:253` for the
same `Publish.BufferTime` value; do not read a plugin-local config field.

- [ ] **Step 5: Run the tests**

Run: `go test -race ./plugin/mp4 -run '^TestRecordingIOWarnings$' -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add plugin/mp4/recording_io_warnings.go plugin/mp4/recording_io_warnings_test.go plugin/mp4/index.go
git commit -m "feat(mp4): warn when record runs on object storage without a publisher buffer"
```

---

## Verification (whole plan)

Run, in order:

```bash
gofmt -l pkg/storage plugin/mp4
go vet ./pkg/storage/... ./plugin/mp4/...
go vet -tags s3 ./pkg/storage
go test -race ./pkg/storage ./plugin/mp4/pkg -count=1
go test -race -tags s3 ./pkg/storage -count=1
go test -race ./plugin/mp4 -run '^TestRecordingIOWarnings$' -count=1
go build ./...
go build -tags s3 ./...
```

Expected: `gofmt -l` prints nothing; all commands PASS. The full `./...` suite
still hits the documented upstream gotask `EventLoop` data race under `-race`
(CLAUDE.md Known Issues); that is pre-existing and not part of this plan's gate.

## Operational rollout (not code)

- Measure the host's sequential read/write bandwidth (`fio` or `dd`) before
  choosing rates; keep total finalization IO below ~50% of measured bandwidth.
- For a 36-camera, 60-minute, S3-backed deployment start with:
  `readratembps: 128`, `trailerwriteratembps: 64`, `maxconcurrentuploads: 4`,
  `maxqueueduploads: 8`, `storage.s3.concurrency: 2`,
  `publish.buffertime: 3s`.
- Keep `storage.s3.partsize` at 64 MiB unless disk latency is high; concurrency
  and read rate are the primary knobs. Parts below 5 MiB are rejected by S3.
- Bound the fallback so a spill cannot fill the disk:
  `upload.pendingdiskminfreemb: 2048`, `upload.pendingmaxsizemb`,
  `upload.pendingmaxfiles`. `handleFailedUpload` keeps the file in place when
  the pending directory is over budget.
- Watch `[storage] uploading ... active=N/4`, `upload backlog full` warnings,
  `reader too slow` counts, and the pending directory watermark after the first
  synchronized stop window.
