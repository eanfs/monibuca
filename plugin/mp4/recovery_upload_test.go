package plugin_mp4

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	"m7s.live/v5"
	"m7s.live/v5/pkg/storage"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
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

// M3 回归：崩溃前开录已插入 record_streams（end_time 空、duration=0），恢复
// 成功后必须回写 end_time/duration/storage_type（local 分支同样要写），
// 否则列表里永远是 0 时长。
func TestIndexRecoveryTaskUpdatesRecordStream(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	mediaPath, payloadStart := writeHalfWrittenRecording(t, dir, "db_record.mp4", payload)
	split := len(payload) / 2
	records := []mp4pkg.IndexRecord{
		{Flags: 0x01, TrackID: 1, Size: uint32(split), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		{TrackID: 1, Size: uint32(len(payload) - split), Offset: uint64(payloadStart) + uint64(split), DTS: 40, Duration: 40},
	}
	start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	// 正常录制写入 file_path 的是逻辑录像路径；这里用绝对媒体路径以便直接校验。
	filePath := mediaPath
	header := mp4pkg.IndexHeader{
		Version:     1,
		StreamPath:  "live/db_record",
		MediaPath:   mediaPath,
		FilePath:    filePath,
		TargetName:  "db_record.mp4",
		StorageType: "local",
		StartTime:   start,
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240,
			STSD: recoveryTestSTSD(t),
		}},
	}
	sidecar := mediaPath + mp4pkg.IndexExtension
	writeRecoverySidecar(t, sidecar, header, records)
	ageRecoveryFiles(t, mediaPath, sidecar)

	db, err := gorm.Open(gormlite.Open(filepath.Join(dir, "records.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err = db.AutoMigrate(&m7s.RecordStream{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// 模拟崩溃前开录插入的半成品行。
	if err = db.Create(&m7s.RecordStream{
		StartTime:   start,
		FilePath:    filePath,
		StreamPath:  "live/db_record",
		Type:        "mp4",
		StorageType: "local",
	}).Error; err != nil {
		t.Fatalf("seed record: %v", err)
	}

	backend := &recoveryUploadFakeStorage{key: "local"}
	recovery := &IndexRecoveryTask{
		DB: db,
		ResolveUploadStorage: func(storageType string) (storage.Storage, error) {
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

	var got m7s.RecordStream
	if err = db.Where("file_path = ?", filePath).First(&got).Error; err != nil {
		t.Fatalf("query record: %v", err)
	}
	if got.EndTime.IsZero() {
		t.Fatal("end_time must be set after recovery")
	}
	if got.Duration != 80 {
		t.Fatalf("duration = %d, want 80", got.Duration)
	}
	wantEnd := start.Add(80 * time.Millisecond)
	if got.EndTime.UnixMilli() != wantEnd.UnixMilli() {
		t.Fatalf("end_time = %v, want %v (start+duration)", got.EndTime, wantEnd)
	}
	if got.StorageType != "local" {
		t.Fatalf("storage_type = %q, want local", got.StorageType)
	}
	var count int64
	if err = db.Model(&m7s.RecordStream{}).Where("file_path = ?", filePath).Count(&count).Error; err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 1 {
		t.Fatalf("record count = %d, want 1 (update, not duplicate)", count)
	}
}
