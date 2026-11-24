package mp4

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"m7s.live/v5/pkg/storage"
)

// blockingUploadFile 让正常上传在 Close（对象存储=上传）处停住，从而制造
// 「已 handoff 但未完成」的在途窗口。LocalFd 暴露暂存文件路径，
// handoffToUploadQueueWith 用它推导与恢复扫描一致的 in-flight 键。
type blockingUploadFile struct {
	fakeStorageFile
	fd         *os.File
	closeSeen  chan struct{}
	allowClose chan struct{}
}

func (f *blockingUploadFile) LocalFd() *os.File { return f.fd }

func (f *blockingUploadFile) Close() error {
	close(f.closeSeen)
	<-f.allowClose
	f.closed = true
	return nil
}

var _ storage.RangeInserter = (*blockingUploadFile)(nil)

// 回归（issue #40）：正常上传在途（已 handoff、尚未完成）时，同一 tmp 文件
// 仍留在扫描目录里，恢复扫描不得把它当崩溃残留再次入队/上传，否则同一录像
// 会被上传两次并在 record_streams 写入两行。
//
// 键必须与恢复扫描完全一致：storageType + 对象键 + 恢复产物路径
// （.tmp → .mp4，与 recoveryOutputPath 相同）。
func TestNormalUploadInFlightBlocksRecoveryScanOfSameTemp(t *testing.T) {
	// 这里刻意用「已停止的队列」触发 enqueueFinalizeOrRunInline 的内联兜底：
	// handoff 在测试 goroutine 上同步执行，可在 Close 处精确停住，不依赖
	// gotask 事件循环时序。
	queue := newStoppedUploadQueue(t)

	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "s3writer_inflight.tmp")
	if err := os.WriteFile(tmpPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("write temp media: %v", err)
	}
	fd, err := os.OpenFile(tmpPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open temp media: %v", err)
	}
	defer fd.Close()

	// 恢复扫描先写出 moov-first 产物再入队；键里的产物路径就是这个 .mp4。
	outputPath := filepath.Join(dir, "s3writer_inflight.mp4")
	if err := os.WriteFile(outputPath, []byte("recovered-media"), 0o644); err != nil {
		t.Fatalf("write recovered output: %v", err)
	}
	sidecar := tmpPath + IndexExtension
	if err := os.WriteFile(sidecar, []byte("index"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	file := &blockingUploadFile{
		fd:         fd,
		closeSeen:  make(chan struct{}),
		allowClose: make(chan struct{}),
	}
	trailer := &writeTrailerTask{
		file:       file,
		filePath:   "live/test/rec.mp4",
		streamPath: "live/test",
		storageKey: "s3",
	}
	trailer.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	handoffDone := make(chan struct{})
	go func() {
		defer close(handoffDone)
		trailer.handoffToUploadQueueWith(queue, 5, nil)
	}()

	select {
	case <-file.closeSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("normal upload never reached Close; cannot simulate the in-flight window")
	}

	backend := &fakeRecoveryStorage{key: "s3"}
	queued := 0
	err = EnqueueRecoveryUploadWithOptions(RecoveryUpload{
		Header: IndexHeader{
			Version:     1,
			StreamPath:  "live/test",
			MediaPath:   tmpPath,
			FilePath:    "live/test/rec.mp4",
			StorageKey:  "bucket-prefix/live/test/rec.mp4",
			StorageType: "s3",
		},
		MediaPath:   tmpPath,
		OutputPath:  outputPath,
		SidecarPath: sidecar,
	}, RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) { return backend, nil },
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("recovery enqueue: %v", err)
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0: recovery must skip a temp file whose normal upload is still in flight", queued)
	}
	if backend.createdPath != "" || backend.file != nil {
		t.Fatalf("recovery must not upload again while the normal upload is in flight: created=%q file=%+v",
			backend.createdPath, backend.file)
	}

	close(file.allowClose)
	select {
	case <-handoffDone:
	case <-time.After(5 * time.Second):
		t.Fatal("normal upload did not finish after being released")
	}

	// 任务结束后必须释放 in-flight 键：否则该文件在进程存活期间会被永久
	// 跳过（周期重扫也救不回来），只能等重启。这里用同一产物再次入队验证。
	queued = 0
	backend2 := &fakeRecoveryStorage{key: "s3"}
	if err := EnqueueRecoveryUploadWithOptions(RecoveryUpload{
		Header: IndexHeader{
			Version:     1,
			StreamPath:  "live/test",
			MediaPath:   tmpPath,
			FilePath:    "live/test/rec.mp4",
			StorageType: "s3",
		},
		MediaPath:   tmpPath,
		OutputPath:  outputPath,
		SidecarPath: sidecar,
	}, RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) { return backend2, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			queued++
			return task.Go()
		},
	}); err != nil {
		t.Fatalf("second recovery enqueue after the upload finished: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued tasks = %d, want 1: the in-flight key must be released when the upload finishes", queued)
	}
	if backend2.file == nil || backend2.file.finalizedSrc != outputPath {
		t.Fatalf("recovery after completion was not handed to the backend: %+v", backend2.file)
	}
}

// spillToPending 同样是任务终止路径：即使上传从未开始（未抢到队列槽位），
// in-flight 键也必须释放，否则后续真实崩溃恢复会被永久跳过。
func TestSpillToPendingReleasesRecoveryInFlightKey(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{MaxQueuedUploads: 1, PendingDir: t.TempDir()})
	if !storage.TryAcquireUploadQueue() {
		t.Fatal("test setup: failed to reserve the only queued-upload slot")
	}
	// 槽位由本测试持有，handoff 必然走 spill 分支。
	defer storage.ReleaseUploadQueue()

	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "s3writer_spill.tmp")
	if err := os.WriteFile(tmpPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("write temp media: %v", err)
	}
	fd, err := os.OpenFile(tmpPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open temp media: %v", err)
	}
	defer fd.Close()
	outputPath := filepath.Join(dir, "s3writer_spill.mp4")
	if err := os.WriteFile(outputPath, []byte("recovered-media"), 0o644); err != nil {
		t.Fatalf("write recovered output: %v", err)
	}
	sidecar := tmpPath + IndexExtension
	if err := os.WriteFile(sidecar, []byte("index"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	trailer := &writeTrailerTask{
		file:       &blockingUploadFile{fd: fd},
		filePath:   "live/test/spill.mp4",
		streamPath: "live/test",
		storageKey: "s3",
		db:         newPendingTestDB(t),
	}
	trailer.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// onFail 返回 false：不登记补传、不写 record_streams，只验证 spill 分支的键释放。
	trailer.handoffToUploadQueueWith(&uploadQueueTask, 5, func(error) bool { return false })

	queued := 0
	if err := EnqueueRecoveryUploadWithOptions(RecoveryUpload{
		Header: IndexHeader{
			Version:     1,
			StreamPath:  "live/test",
			MediaPath:   tmpPath,
			FilePath:    "live/test/spill.mp4",
			StorageType: "s3",
		},
		MediaPath:   tmpPath,
		OutputPath:  outputPath,
		SidecarPath: sidecar,
	}, RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) { return &fakeRecoveryStorage{key: "s3"}, nil },
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	}); err != nil {
		t.Fatalf("recovery enqueue after spill: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued tasks = %d, want 1: spillToPending must release the in-flight key", queued)
	}
}

// 软链部署（容器挂载点、macOS /var→/private/var）下，正常收尾与恢复扫描
// 拿到的产物路径字符串不同：前者是 filepath.Abs+Clean，后者是父目录
// EvalSymlinks+原文件名。键构造函数必须把两者归一到同一坐标系，否则
// LoadOrStore 互相看不见，去重静默失效（复审 C4）。
func TestInFlightKeyCanonicalizesSymlinkedOutputDir(t *testing.T) {
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	direct := recoveryUploadInFlightKey("s3", "live/test/rec.mp4", filepath.Join(realDir, "s3writer_x.mp4"))
	viaLink := recoveryUploadInFlightKey("s3", "live/test/rec.mp4", filepath.Join(linkDir, "s3writer_x.mp4"))
	if direct != viaLink {
		t.Fatalf("in-flight key differs across symlinked dir:\n direct = %q\n link   = %q", direct, viaLink)
	}
	// 文件名必须保持原样（不同产物不能被归一成同一个键）。
	other := recoveryUploadInFlightKey("s3", "live/test/rec.mp4", filepath.Join(linkDir, "s3writer_y.mp4"))
	if other == direct {
		t.Fatal("distinct output files must not share an in-flight key")
	}
}

// failingUploadFile 的 Close（对象存储=上传）直接失败，不阻塞。
type failingUploadFile struct {
	fakeStorageFile
	fd *os.File
}

func (f *failingUploadFile) LocalFd() *os.File { return f.fd }

func (f *failingUploadFile) Close() error {
	f.closed = true
	return errors.New("upload failed")
}

var _ storage.RangeInserter = (*failingUploadFile)(nil)

// 复审 I3：上传失败也必须释放 in-flight 键。run() 的 defer 若被挪走或漏掉，
// 该文件在进程存活期间会被恢复扫描永久跳过（同一键永不释放）——本用例钉住
// 成功/失败/spill 三条终止路径的对称性：失败后再入队必须能成功。
func TestInFlightKeyReleasedWhenUploadFails(t *testing.T) {
	queue := newStoppedUploadQueue(t)

	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "s3writer_fail.tmp")
	if err := os.WriteFile(tmpPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("write temp media: %v", err)
	}
	fd, err := os.OpenFile(tmpPath, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open temp media: %v", err)
	}
	defer fd.Close()

	outputPath := filepath.Join(dir, "s3writer_fail.mp4")
	if err := os.WriteFile(outputPath, []byte("recovered-media"), 0o644); err != nil {
		t.Fatalf("write recovered output: %v", err)
	}
	sidecar := tmpPath + IndexExtension
	if err := os.WriteFile(sidecar, []byte("index"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	trailer := &writeTrailerTask{
		file:       &failingUploadFile{fd: fd},
		filePath:   "live/test/rec.mp4",
		streamPath: "live/test",
		storageKey: "s3",
	}
	trailer.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	// 已停止的队列 → 内联执行 run()，上传失败在该 goroutine 上同步完成。
	// onFail 返回 false：不保留媒体、不登记补传，只验证键的释放。
	trailer.handoffToUploadQueueWith(queue, 5, func(error) bool { return false })

	queued := 0
	if err := EnqueueRecoveryUploadWithOptions(RecoveryUpload{
		Header: IndexHeader{
			Version:     1,
			StreamPath:  "live/test",
			MediaPath:   tmpPath,
			FilePath:    "live/test/rec.mp4",
			StorageType: "s3",
		},
		MediaPath:   tmpPath,
		OutputPath:  outputPath,
		SidecarPath: sidecar,
	}, RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) { return &fakeRecoveryStorage{key: "s3"}, nil },
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	}); err != nil {
		t.Fatalf("recovery enqueue after failed upload: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued tasks = %d, want 1: a failed upload must release the in-flight key", queued)
	}
}
