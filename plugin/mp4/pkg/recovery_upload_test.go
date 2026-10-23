package mp4

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/storage"
)

// recoverySourceMetadata 构造 pending 行里记录的源产物标记（JSON）。
func recoverySourceMetadata(sourcePath string) string {
	b, _ := json.Marshal(map[string]string{recoveryUploadSourceMetadataKey: sourcePath})
	return string(b)
}

// fakeRecoveryStorage 是 storage.Storage 的最小实现，避免测试依赖真实 S3。
type fakeRecoveryStorage struct {
	key       string
	file      *fakeRecoveryFile
	createErr error

	createdPath string
}

func (s *fakeRecoveryStorage) CreateFile(_ context.Context, path string) (storage.File, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	s.createdPath = path
	if s.file == nil {
		s.file = &fakeRecoveryFile{storage: s}
	}
	return s.file, nil
}
func (s *fakeRecoveryStorage) OpenFile(context.Context, string) (storage.File, error) {
	return nil, storage.ErrFileNotFound
}
func (s *fakeRecoveryStorage) Delete(context.Context, string) error { return nil }
func (s *fakeRecoveryStorage) Exists(context.Context, string) (bool, error) {
	return false, nil
}
func (s *fakeRecoveryStorage) GetSize(context.Context, string) (int64, error) { return 0, nil }
func (s *fakeRecoveryStorage) GetURL(context.Context, string) (string, error) { return "", nil }
func (s *fakeRecoveryStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *fakeRecoveryStorage) Close() error { return nil }
func (s *fakeRecoveryStorage) GetKey() string {
	return s.key
}

// fakeRecoveryFile 模拟对象存储文件：Close 成功 = 上传成功并删除本地产物；
// Close 失败 = 保留文件供 pending 补传。
type fakeRecoveryFile struct {
	storage *fakeRecoveryStorage
	// closeErr 非空时模拟上传失败。
	closeErr error
	// finalizeErr 非空时模拟 FinalizeFromTemp 失败。
	finalizeErr error

	finalizedSrc string
	metadata     map[string]string
	closed       bool
}

func (f *fakeRecoveryFile) Write(p []byte) (int, error)               { return len(p), nil }
func (f *fakeRecoveryFile) WriteAt(p []byte, off int64) (int, error)  { return len(p), nil }
func (f *fakeRecoveryFile) Read(p []byte) (int, error)                { return 0, io.EOF }
func (f *fakeRecoveryFile) ReadAt(p []byte, off int64) (int, error)   { return 0, io.EOF }
func (f *fakeRecoveryFile) Seek(off int64, whence int) (int64, error) { return 0, nil }
func (f *fakeRecoveryFile) Sync() error                               { return nil }
func (f *fakeRecoveryFile) Stat() (os.FileInfo, error)                { return nil, os.ErrInvalid }
func (f *fakeRecoveryFile) Name() string                              { return f.storage.key }
func (f *fakeRecoveryFile) SetMetadata(key, value string) {
	if f.metadata == nil {
		f.metadata = make(map[string]string)
	}
	f.metadata[key] = value
}

func (f *fakeRecoveryFile) FinalizeFromTemp(srcPath string) error {
	if f.finalizeErr != nil {
		return f.finalizeErr
	}
	f.finalizedSrc = srcPath
	return nil
}

func (f *fakeRecoveryFile) Close() error {
	f.closed = true
	if f.closeErr != nil {
		return f.closeErr
	}
	if f.finalizedSrc != "" {
		return os.Remove(f.finalizedSrc)
	}
	return nil
}

var _ storage.TempFileFinalizer = (*fakeRecoveryFile)(nil)

type recoveryUploadFixture struct {
	output  string
	sidecar string
	header  IndexHeader
}

func newRecoveryUploadFixture(t *testing.T, storageType string) recoveryUploadFixture {
	t.Helper()
	dir := t.TempDir()
	output := filepath.Join(dir, "s3writer_test.mp4")
	if err := os.WriteFile(output, []byte("recovered-media"), 0o644); err != nil {
		t.Fatalf("write output: %v", err)
	}
	sidecar := output + IndexExtension
	if err := os.WriteFile(sidecar, []byte("index"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	return recoveryUploadFixture{
		output:  output,
		sidecar: sidecar,
		header: IndexHeader{
			Version:     1,
			StreamPath:  "live/test",
			MediaPath:   output,
			FilePath:    "live/test/rec.mp4",
			StorageKey:  "bucket-prefix/live/test/rec.mp4",
			StorageType: storageType,
		},
	}
}

func (f recoveryUploadFixture) upload() RecoveryUpload {
	return RecoveryUpload{
		Header:      f.header,
		MediaPath:   f.output,
		OutputPath:  f.output,
		SidecarPath: f.sidecar,
	}
}

func (f recoveryUploadFixture) assertRetained(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(f.output); err != nil {
		t.Fatalf("recovered output must be retained: %v", err)
	}
	if _, err := os.Stat(f.sidecar); err != nil {
		t.Fatalf("sidecar must be retained: %v", err)
	}
}

// 解析必须使用 header 声明的 s3，而不是当前活跃/本地存储；上传成功后
// 删除本地产物与 sidecar。
func TestEnqueueRecoveryUploadResolvesDeclaredStorage(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "s3")
	backend := &fakeRecoveryStorage{key: "s3"}

	var resolvedTypes []string
	var queued int
	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		Resolver: func(storageType string) (storage.Storage, error) {
			resolvedTypes = append(resolvedTypes, storageType)
			return backend, nil
		},
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			queued++
			return task.Go()
		},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if len(resolvedTypes) != 1 || resolvedTypes[0] != "s3" {
		t.Fatalf("resolved types = %v, want [s3] (never local)", resolvedTypes)
	}
	if queued != 1 {
		t.Fatalf("queued tasks = %d, want 1", queued)
	}
	// 对象键必须是录制时的原始路径（后端自加 prefix），不能直接复用已含
	// prefix 的 StorageKey。
	if backend.createdPath != fixture.header.FilePath {
		t.Fatalf("created path = %q, want %q", backend.createdPath, fixture.header.FilePath)
	}
	if backend.file.finalizedSrc != fixture.output {
		t.Fatalf("finalized src = %q, want %q", backend.file.finalizedSrc, fixture.output)
	}
	if _, err := os.Stat(fixture.output); !os.IsNotExist(err) {
		t.Fatalf("local recovered file should be removed after upload: %v", err)
	}
	if _, err := os.Stat(fixture.sidecar); !os.IsNotExist(err) {
		t.Fatalf("sidecar should be removed after upload: %v", err)
	}
}

// S3 无法解析时必须返回错误并保留全部源文件，绝不降级 local。
func TestEnqueueRecoveryUploadUnresolvedStorageKeepsFile(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "s3")
	resolveErr := errors.New("s3 not configured")

	queued := 0
	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) { return nil, resolveErr },
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	})
	if !errors.Is(err, resolveErr) {
		t.Fatalf("error = %v, want %v", err, resolveErr)
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0", queued)
	}
	fixture.assertRetained(t)
}

// header 缺 storageType 时不能猜测 local（对象存储暂存文件会被误删）。
func TestEnqueueRecoveryUploadMissingStorageTypeKeepsFile(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "")
	queued := 0
	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		Resolver: func(storageType string) (storage.Storage, error) {
			t.Fatalf("resolver must not be called for missing storageType, got %q", storageType)
			return nil, nil
		},
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	})
	if err == nil {
		t.Fatal("missing storageType must return an error")
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0", queued)
	}
	fixture.assertRetained(t)
}

// 解析结果与 header 不一致（例如 s3 被错配/回落到 local）时必须拒绝。
func TestEnqueueRecoveryUploadRefusesLocalFallback(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "s3")
	queued := 0
	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) {
			return &fakeRecoveryStorage{key: "local"}, nil
		},
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	})
	if !errors.Is(err, ErrRecoveryStorageMismatch) {
		t.Fatalf("error = %v, want ErrRecoveryStorageMismatch", err)
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0", queued)
	}
	fixture.assertRetained(t)
}

// 上传失败（重试耗尽）→ 文件移入 pending 且登记 upload_tasks，只有登记
// 确认后才删除 sidecar。
func TestEnqueueRecoveryUploadFailureRegistersPendingThenRemovesSidecar(t *testing.T) {
	db := newPendingTestDB(t)
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	fixture := newRecoveryUploadFixture(t, "s3")
	backend := &fakeRecoveryStorage{key: "s3", file: &fakeRecoveryFile{closeErr: errors.New("upload failed")}}

	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		DB:       db,
		Resolver: func(string) (storage.Storage, error) { return backend, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			_ = task.Go() // 上传失败只触发 onFail，不影响"已入队"语义
			return nil
		},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	if _, err := os.Stat(fixture.output); !os.IsNotExist(err) {
		t.Fatalf("output should be moved into pending: %v", err)
	}
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		t.Fatalf("read pending dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("pending files = %d, want 1", len(entries))
	}
	var ut m7s.UploadTask
	if err := db.First(&ut).Error; err != nil {
		t.Fatalf("upload task not registered: %v", err)
	}
	if ut.StorageType != "s3" || ut.ObjectKey != fixture.header.FilePath ||
		ut.LocalPath != filepath.Join(pendingDir, entries[0].Name()) {
		t.Fatalf("unexpected upload task: %+v", ut)
	}
	if _, err := os.Stat(fixture.sidecar); !os.IsNotExist(err) {
		t.Fatalf("sidecar should be removed after pending registration: %v", err)
	}
}

// 无 DB / 无法登记 pending 时，sidecar 与产物都必须保留。
func TestEnqueueRecoveryUploadFailureWithoutRegistrationKeepsSidecar(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir()})
	fixture := newRecoveryUploadFixture(t, "s3")
	backend := &fakeRecoveryStorage{key: "s3", file: &fakeRecoveryFile{closeErr: errors.New("upload failed")}}

	if err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) { return backend, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			_ = task.Go()
			return nil
		},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	fixture.assertRetained(t)
}

// 同一产物重复入队（崩溃窗口/并发扫描）不得重复上传或重复建行。
func TestEnqueueRecoveryUploadDuplicateEnqueueIsIdempotent(t *testing.T) {
	t.Run("in flight", func(t *testing.T) {
		fixture := newRecoveryUploadFixture(t, "s3")
		backend := &fakeRecoveryStorage{key: "s3"}
		var queued []*finalizeUploadTask
		opts := RecoveryUploadOptions{
			Resolver: func(string) (storage.Storage, error) { return backend, nil },
			enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
				queued = append(queued, task) // 暂不执行，模拟在途
				return nil
			},
		}
		if err := EnqueueRecoveryUploadWithOptions(fixture.upload(), opts); err != nil {
			t.Fatalf("first enqueue: %v", err)
		}
		if err := EnqueueRecoveryUploadWithOptions(fixture.upload(), opts); err != nil {
			t.Fatalf("duplicate enqueue: %v", err)
		}
		if len(queued) != 1 {
			t.Fatalf("queued tasks = %d, want 1", len(queued))
		}
		if err := queued[0].Go(); err != nil {
			t.Fatalf("run queued task: %v", err)
		}
		if _, err := os.Stat(fixture.sidecar); !os.IsNotExist(err) {
			t.Fatalf("sidecar should be removed after upload: %v", err)
		}
	})

	t.Run("already registered", func(t *testing.T) {
		db := newPendingTestDB(t)
		fixture := newRecoveryUploadFixture(t, "s3")
		// pending 文件仍在：补传队列会接手，恢复不能重复上传。
		pendingPath := filepath.Join(t.TempDir(), "pending.mp4")
		if err := os.WriteFile(pendingPath, []byte("pending"), 0o644); err != nil {
			t.Fatalf("write pending file: %v", err)
		}
		if err := db.Create(&m7s.UploadTask{
			LocalPath:   pendingPath,
			ObjectKey:   fixture.header.FilePath,
			StorageType: "s3",
			Status:      m7s.UploadStatusFailed,
			MaxRetries:  10,
			// 真实的 pending 登记会把源产物路径写进 metadata：文件移入
			// pending 后 LocalPath 改变，只能靠源标记识别同一产物。
			MetadataJSON: recoverySourceMetadata(fixture.output),
		}).Error; err != nil {
			t.Fatalf("seed upload task: %v", err)
		}
		queued := 0
		err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
			DB:       db,
			Resolver: func(string) (storage.Storage, error) { return &fakeRecoveryStorage{key: "s3"}, nil },
			enqueue: func(*finalizeUploadTask, *slog.Logger) error {
				queued++
				return nil
			},
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if queued != 0 {
			t.Fatalf("queued tasks = %d, want 0 for already registered upload", queued)
		}
		var count int64
		if err := db.Model(&m7s.UploadTask{}).Count(&count).Error; err != nil {
			t.Fatalf("count upload tasks: %v", err)
		}
		if count != 1 {
			t.Fatalf("upload task rows = %d, want 1", count)
		}
		if _, err := os.Stat(fixture.sidecar); !os.IsNotExist(err) {
			t.Fatalf("sidecar should be removed once registration is confirmed: %v", err)
		}
	})
}

// 固定 fileName 会复用对象键；历史上已上传成功（本地文件已删除）的
// upload_tasks 行不得把新录像误判为重复而永久跳过。
func TestEnqueueRecoveryUploadStaleSuccessRowDoesNotBlockNewRecording(t *testing.T) {
	db := newPendingTestDB(t)
	fixture := newRecoveryUploadFixture(t, "s3")
	if err := db.Create(&m7s.UploadTask{
		LocalPath:   filepath.Join(t.TempDir(), "already-uploaded.mp4"), // 不存在
		ObjectKey:   fixture.header.FilePath,
		StorageType: "s3",
		Status:      m7s.UploadStatusSuccess,
		MaxRetries:  10,
	}).Error; err != nil {
		t.Fatalf("seed upload task: %v", err)
	}

	queued := 0
	if err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		DB:       db,
		Resolver: func(string) (storage.Storage, error) { return &fakeRecoveryStorage{key: "s3"}, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			queued++
			return task.Go()
		},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued tasks = %d, want 1 for a new recording reusing the object key", queued)
	}
	if _, err := os.Stat(fixture.output); !os.IsNotExist(err) {
		t.Fatalf("new recovered file should be uploaded and removed: %v", err)
	}
}

// 固定 fileName + 分片会让不同录像复用同一对象键。分片 A 上传失败落入
// pending 后，分片 B 是另一份产物：必须照常上传，不能被 A 的 pending 行
// 误判为“已登记”而跳过（否则 B 的 sidecar 被删、mp4 留在磁盘永不补传）。
func TestEnqueueRecoveryUploadSeparatesArtifactsSharingObjectKey(t *testing.T) {
	db := newPendingTestDB(t)
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	fragmentA := newRecoveryUploadFixture(t, "s3")
	fragmentB := newRecoveryUploadFixture(t, "s3")
	// 同一路录制的两个分片：对象键相同，本地产物路径不同。
	fragmentB.header.FilePath = fragmentA.header.FilePath
	fragmentB.header.StorageKey = fragmentA.header.StorageKey
	if fragmentA.output == fragmentB.output {
		t.Fatal("test setup: fragments must have distinct local artifacts")
	}

	backendA := &fakeRecoveryStorage{key: "s3", file: &fakeRecoveryFile{closeErr: errors.New("upload failed")}}
	if err := EnqueueRecoveryUploadWithOptions(fragmentA.upload(), RecoveryUploadOptions{
		DB:       db,
		Resolver: func(string) (storage.Storage, error) { return backendA, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			_ = task.Go()
			return nil
		},
	}); err != nil {
		t.Fatalf("enqueue fragment A: %v", err)
	}
	if _, err := os.Stat(fragmentA.output); !os.IsNotExist(err) {
		t.Fatalf("fragment A should be moved into pending: %v", err)
	}
	var pendingRows []m7s.UploadTask
	if err := db.Find(&pendingRows).Error; err != nil {
		t.Fatalf("load pending rows: %v", err)
	}
	if len(pendingRows) != 1 {
		t.Fatalf("pending rows = %d, want 1", len(pendingRows))
	}
	if got := recoverySourcePath(pendingRows[0]); got != fragmentA.output {
		t.Fatalf("pending row source marker = %q, want %q", got, fragmentA.output)
	}
	// 模拟补传调度尚未处理：A 的 pending 文件仍在（旧判重逻辑的触发条件）。
	if _, err := os.Stat(pendingRows[0].LocalPath); err != nil {
		t.Fatalf("fragment A pending file must still exist: %v", err)
	}

	backendB := &fakeRecoveryStorage{key: "s3"}
	queued := 0
	if err := EnqueueRecoveryUploadWithOptions(fragmentB.upload(), RecoveryUploadOptions{
		DB:       db,
		Resolver: func(string) (storage.Storage, error) { return backendB, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			queued++
			return task.Go()
		},
	}); err != nil {
		t.Fatalf("enqueue fragment B: %v", err)
	}
	if queued != 1 {
		t.Fatalf("queued tasks = %d, want 1 for the second artifact sharing the object key", queued)
	}
	if backendB.file == nil || backendB.file.finalizedSrc != fragmentB.output {
		t.Fatalf("fragment B was not handed to the upload backend: %+v", backendB.file)
	}
	if _, err := os.Stat(fragmentB.output); !os.IsNotExist(err) {
		t.Fatalf("fragment B should be uploaded and removed: %v", err)
	}
	if _, err := os.Stat(fragmentB.sidecar); !os.IsNotExist(err) {
		t.Fatalf("fragment B sidecar should be removed after its own upload: %v", err)
	}
}

// 本地存储没有上传动作：产物留在最终路径，只删除 sidecar，绝不投递上传任务。
func TestEnqueueRecoveryUploadLocalIsNoop(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "local")
	queued := 0
	if err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) { return &fakeRecoveryStorage{key: "local"}, nil },
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0 for local storage", queued)
	}
	if _, err := os.Stat(fixture.output); err != nil {
		t.Fatalf("local output must stay in place: %v", err)
	}
	if _, err := os.Stat(fixture.sidecar); !os.IsNotExist(err) {
		t.Fatalf("sidecar should be removed for local output: %v", err)
	}
}

// prepare 失败（FinalizeFromTemp 拒绝接管）也必须走 pending 登记，不能丢产物。
func TestEnqueueRecoveryUploadPrepareFailureRegistersPending(t *testing.T) {
	db := newPendingTestDB(t)
	pendingDir := t.TempDir()
	storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir})

	fixture := newRecoveryUploadFixture(t, "s3")
	backend := &fakeRecoveryStorage{key: "s3", file: &fakeRecoveryFile{finalizeErr: errors.New("adopt failed")}}
	if err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		DB:       db,
		Resolver: func(string) (storage.Storage, error) { return backend, nil },
		enqueue: func(task *finalizeUploadTask, _ *slog.Logger) error {
			_ = task.Go()
			return nil
		},
	}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	entries, err := os.ReadDir(pendingDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("pending files = %v (err=%v), want 1", entries, err)
	}
	var count int64
	if err := db.Model(&m7s.UploadTask{}).Count(&count).Error; err != nil {
		t.Fatalf("count upload tasks: %v", err)
	}
	if count != 1 {
		t.Fatalf("upload task rows = %d, want 1", count)
	}
	if _, err := os.Stat(fixture.sidecar); !os.IsNotExist(err) {
		t.Fatalf("sidecar should be removed after pending registration: %v", err)
	}
}
