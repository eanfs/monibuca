package mp4

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/storage"
)

type openFileStorage struct {
	key  string
	file storage.File
}

func (s *openFileStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *openFileStorage) OpenFile(context.Context, string) (storage.File, error) {
	return s.file, nil
}
func (s *openFileStorage) Delete(context.Context, string) error { return nil }
func (s *openFileStorage) Exists(context.Context, string) (bool, error) {
	return true, nil
}
func (s *openFileStorage) GetSize(context.Context, string) (int64, error) { return 0, nil }
func (s *openFileStorage) GetURL(context.Context, string) (string, error) { return "", nil }
func (s *openFileStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *openFileStorage) Close() error   { return nil }
func (s *openFileStorage) GetKey() string { return s.key }

func TestOpenRecordFileUsesRecordStorageType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "record.mp4")
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
	osFile, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	expectedFile := &storage.LocalFile{File: osFile}
	var resolvedType string
	demuxer := &DemuxerRange{StorageResolver: func(storageType string) (storage.Storage, error) {
		resolvedType = storageType
		return &openFileStorage{file: expectedFile, key: storageType}, nil
	}}
	stream := m7s.RecordStream{StorageType: "s3", FilePath: "records/a.mp4"}

	file, cleanup, err := demuxer.openRecordFile(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if resolvedType != "s3" || file != expectedFile {
		t.Fatalf("resolvedType=%q file=%p", resolvedType, file)
	}
}
