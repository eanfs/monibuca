package plugin_gb28181pro

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"m7s.live/v5/pkg/storage"
	gb28181 "m7s.live/v5/plugin/gb28181/pkg"
)

type lazyDownloadTestFile struct {
	storage.File
	sourcePath      string
	seekCalls       int
	firstSeekOffset int64
	firstSeekWhence int
	seekErr         error
}

func (f *lazyDownloadTestFile) Seek(offset int64, whence int) (int64, error) {
	if f.seekCalls == 0 {
		f.firstSeekOffset = offset
		f.firstSeekWhence = whence
	}
	f.seekCalls++
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	if f.File == nil {
		file, err := os.Open(f.sourcePath)
		if err != nil {
			return 0, err
		}
		f.File = &storage.LocalFile{File: file}
	}
	return f.File.Seek(offset, whence)
}

func (f *lazyDownloadTestFile) Stat() (os.FileInfo, error) {
	if f.File == nil {
		return nil, errors.New("file not initialized")
	}
	return f.File.Stat()
}

func (f *lazyDownloadTestFile) Close() error {
	if f.File == nil {
		return nil
	}
	return f.File.Close()
}

type objectDownloadTestStorage struct {
	file        storage.File
	openCalls   int
	getURLCalls int
	openedKey   string
}

func (s *objectDownloadTestStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *objectDownloadTestStorage) OpenFile(_ context.Context, key string) (storage.File, error) {
	s.openCalls++
	s.openedKey = key
	return s.file, nil
}
func (s *objectDownloadTestStorage) Delete(context.Context, string) error           { return nil }
func (s *objectDownloadTestStorage) Exists(context.Context, string) (bool, error)   { return true, nil }
func (s *objectDownloadTestStorage) GetSize(context.Context, string) (int64, error) { return 0, nil }
func (s *objectDownloadTestStorage) GetURL(context.Context, string) (string, error) {
	s.getURLCalls++
	return "https://bucket.example/record.mp4?X-Amz-Credential=AKIA_TEST&X-Amz-Signature=top-secret", nil
}
func (s *objectDownloadTestStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *objectDownloadTestStorage) Close() error   { return nil }
func (s *objectDownloadTestStorage) GetKey() string { return string(storage.StorageTypeS3) }

func TestServeStoredRecordFileUsesOpenFileWithoutLeakingURL(t *testing.T) {
	filePath := t.TempDir() + "/record.mp4"
	const content = "object-storage-video"
	if err := os.WriteFile(filePath, []byte(content), 0600); err != nil {
		t.Fatalf("write object fixture: %v", err)
	}
	lazyFile := &lazyDownloadTestFile{sourcePath: filePath}
	st := &objectDownloadTestStorage{file: lazyFile}
	var logs bytes.Buffer
	plugin := &GB28181Plugin{}
	plugin.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	record := &gb28181.GB28181Record{
		DownloadId: "download-1",
		FilePath:   "records/camera-1.mp4",
		Status:     "completed",
	}
	request := httptest.NewRequest(http.MethodGet, "/gb28181/download?downloadId=download-1", nil)
	response := httptest.NewRecorder()

	plugin.serveStoredRecordFile(st, response, request, record)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", response.Code, http.StatusOK, response.Body.String())
	}
	if response.Body.String() != content {
		t.Errorf("body = %q, want %q", response.Body.String(), content)
	}
	if lazyFile.seekCalls == 0 {
		t.Fatal("lazy object file must be initialized with Seek before Stat")
	}
	if lazyFile.firstSeekOffset != 0 || lazyFile.firstSeekWhence != io.SeekStart {
		t.Errorf("first Seek = (%d, %d), want (0, io.SeekStart)", lazyFile.firstSeekOffset, lazyFile.firstSeekWhence)
	}
	if st.openCalls != 1 || st.openedKey != record.FilePath {
		t.Errorf("OpenFile calls=%d key=%q, want one call for %q", st.openCalls, st.openedKey, record.FilePath)
	}
	if st.getURLCalls != 0 {
		t.Errorf("GetURL must not be called for object download, calls=%d", st.getURLCalls)
	}
	if disposition := response.Header().Get("Content-Disposition"); !strings.Contains(disposition, "camera-1.mp4") {
		t.Errorf("filename must derive from object key, disposition=%q", disposition)
	}
	for _, secret := range []string{"X-Amz-Signature", "X-Amz-Credential", "top-secret", "AKIA_TEST", "https://"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain presigned credential %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "recordId=download-1") || !strings.Contains(logs.String(), "objectKey=records/camera-1.mp4") {
		t.Errorf("safe record context missing from logs: %s", logs.String())
	}
}

func TestServeStoredRecordFileReturnsNotFoundWhenLazySeekFails(t *testing.T) {
	lazyFile := &lazyDownloadTestFile{seekErr: storage.ErrFileNotFound}
	st := &objectDownloadTestStorage{file: lazyFile}
	var logs bytes.Buffer
	plugin := &GB28181Plugin{}
	plugin.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	record := &gb28181.GB28181Record{
		DownloadId: "missing-download",
		FilePath:   "records/missing.mp4",
		Status:     "completed",
	}
	request := httptest.NewRequest(http.MethodGet, "/gb28181/download?downloadId=missing-download", nil)
	response := httptest.NewRecorder()

	plugin.serveStoredRecordFile(st, response, request, record)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body=%q", response.Code, http.StatusNotFound, response.Body.String())
	}
	if lazyFile.seekCalls != 1 {
		t.Errorf("Seek calls = %d, want 1", lazyFile.seekCalls)
	}
	if st.getURLCalls != 0 {
		t.Errorf("GetURL must not be called after lazy Seek failure, calls=%d", st.getURLCalls)
	}
	if !strings.Contains(logs.String(), "errorCategory=notFound") {
		t.Errorf("safe error category missing from logs: %s", logs.String())
	}
	for _, secret := range []string{"X-Amz-Signature", "X-Amz-Credential", "top-secret", "AKIA_TEST", "https://"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain presigned credential %q: %s", secret, logs.String())
		}
	}
}
