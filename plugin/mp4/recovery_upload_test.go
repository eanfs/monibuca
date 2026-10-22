package plugin_mp4

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// recoveryUploadFakeStorage 是 storage.Storage 的最小实现。这些测试只关心
// 存储类型解析，不真正上传，因此 CreateFile 不应被调用（除本地 no-op 场景
// 也不会调用）。
type recoveryUploadFakeStorage struct {
	key         string
	createCalls int
}

func (s *recoveryUploadFakeStorage) CreateFile(context.Context, string) (storage.File, error) {
	s.createCalls++
	return nil, errors.New("unexpected CreateFile in storage resolution test")
}
func (s *recoveryUploadFakeStorage) OpenFile(context.Context, string) (storage.File, error) {
	return nil, storage.ErrFileNotFound
}
func (s *recoveryUploadFakeStorage) Delete(context.Context, string) error { return nil }
func (s *recoveryUploadFakeStorage) Exists(context.Context, string) (bool, error) {
	return false, nil
}
func (s *recoveryUploadFakeStorage) GetSize(context.Context, string) (int64, error) { return 0, nil }
func (s *recoveryUploadFakeStorage) GetURL(context.Context, string) (string, error) { return "", nil }
func (s *recoveryUploadFakeStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *recoveryUploadFakeStorage) Close() error { return nil }
func (s *recoveryUploadFakeStorage) GetKey() string {
	return s.key
}

// 恢复任务必须按 sidecar 里的 StorageType 解析后端；解析失败时保留唯一的
// 源暂存、产物与 .recovering 标记，且绝不回落到 local。
func TestIndexRecoveryTaskResolvesDeclaredStorageType(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixtureWithStorage(t, dir, "s3writer_declared.tmp",
		[]byte{0x11, 0x22, 0x33, 0x44}, "s3")
	output := filepath.Join(dir, "s3writer_declared.mp4")

	resolveErr := errors.New("s3 not ready")
	var resolved []string
	recovery := &IndexRecoveryTask{
		ResolveUploadStorage: func(storageType string) (storage.Storage, error) {
			resolved = append(resolved, storageType)
			return nil, resolveErr
		},
	}

	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("unresolved storage must surface as a run error")
	}
	if stats.Failed != 1 || stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if len(resolved) != 1 || resolved[0] != "s3" {
		t.Fatalf("resolved types = %v, want [s3] (never local)", resolved)
	}
	if _, statErr := os.Stat(mediaPath); statErr != nil {
		t.Fatalf("source tmp must be retained: %v", statErr)
	}
	if _, statErr := os.Stat(sidecar + recoveringSuffix); statErr != nil {
		t.Fatalf("recovering marker must be retained: %v", statErr)
	}
	if _, statErr := os.Stat(output); statErr != nil {
		t.Fatalf("recovered output must be retained: %v", statErr)
	}
}

// 本地存储没有上传动作：定稿成功后产物留在最终路径，sidecar 提交式删除，
// 且不会投递上传任务。
func TestIndexRecoveryTaskLocalStorageFinishesWithoutUpload(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixtureWithStorage(t, dir, "local_rec.mp4",
		[]byte{0xDE, 0xAD, 0xBE, 0xEF}, "local")
	if output := recoveryOutputPath(mediaPath); output != mediaPath {
		t.Fatalf("local output path = %s, want %s", output, mediaPath)
	}

	backend := &recoveryUploadFakeStorage{key: "local"}
	recovery := &IndexRecoveryTask{
		ResolveUploadStorage: func(storageType string) (storage.Storage, error) {
			if storageType != "local" {
				t.Fatalf("resolved storage type = %q, want local", storageType)
			}
			return backend, nil
		},
	}

	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	assertRecoveredPlayable(t, mediaPath, 2)
	if backend.createCalls != 0 {
		t.Fatalf("local storage must not create upload file, calls = %d", backend.createCalls)
	}
	if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
		t.Fatalf("sidecar must be removed after local finalize: %v", statErr)
	}
}
