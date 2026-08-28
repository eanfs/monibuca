package m7s

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"m7s.live/v5/pkg/storage"
)

type retryCountingStorage struct {
	runtimeTestStorage
	createCalls int
	remoteDir   string
}

func (s *retryCountingStorage) CreateFile(context.Context, string) (storage.File, error) {
	s.createCalls++
	file, err := os.CreateTemp(s.remoteDir, "uploaded-*")
	if err != nil {
		return nil, err
	}
	return &storage.LocalFile{File: file}, nil
}

func TestRetryUploadSkipsMismatchedActiveStorage(t *testing.T) {
	db := newUploadTestDB(t)
	localPath := filepath.Join(t.TempDir(), "pending.mp4")
	if err := os.WriteFile(localPath, []byte("pending upload"), 0600); err != nil {
		t.Fatalf("write pending file: %v", err)
	}
	upload := UploadTask{
		LocalPath:   localPath,
		ObjectKey:   "records/pending.mp4",
		StorageType: string(storage.StorageTypeS3),
		Status:      UploadStatusFailed,
		MaxRetries:  10,
	}
	if err := db.Create(&upload).Error; err != nil {
		t.Fatalf("seed upload task: %v", err)
	}

	active := &retryCountingStorage{
		runtimeTestStorage: runtimeTestStorage{key: string(storage.StorageTypeLocal)},
		remoteDir:          t.TempDir(),
	}
	server := &Server{}
	server.DB = db
	server.activateStorage(active, StorageStatus{ActiveType: string(storage.StorageTypeLocal)})
	scheduler := &UploadRetryScheduler{s: server}
	scheduler.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	scheduler.retryUpload(upload)

	if active.createCalls != 0 {
		t.Errorf("mismatched backend must not create an upload target, calls=%d", active.createCalls)
	}
	var persisted UploadTask
	if err := db.First(&persisted, upload.ID).Error; err != nil {
		t.Fatalf("reload upload task: %v", err)
	}
	if persisted.Status != UploadStatusFailed {
		t.Errorf("mismatched backend must preserve Failed status, got=%d", persisted.Status)
	}
	if persisted.RetryCount != upload.RetryCount {
		t.Errorf("mismatched backend must not consume retry count, got=%d", persisted.RetryCount)
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Errorf("mismatched backend must preserve pending file: %v", err)
	}
}
