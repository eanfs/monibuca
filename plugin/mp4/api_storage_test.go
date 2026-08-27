package plugin_mp4

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/storage"
)

type redirectStorage struct{ key string }

func (s *redirectStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *redirectStorage) OpenFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *redirectStorage) Delete(context.Context, string) error { return nil }
func (s *redirectStorage) Exists(context.Context, string) (bool, error) {
	return true, nil
}
func (s *redirectStorage) GetSize(context.Context, string) (int64, error) { return 1, nil }
func (s *redirectStorage) GetURL(context.Context, string) (string, error) {
	return "https://object.invalid/record.mp4", nil
}
func (s *redirectStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *redirectStorage) Close() error   { return nil }
func (s *redirectStorage) GetKey() string { return s.key }

func TestDownloadSingleFileResolvesRecordStorageInsteadOfActiveStorage(t *testing.T) {
	const objectType = "mp4-object-test"
	objectBackend := &redirectStorage{key: objectType}
	var resolvedType string
	resolver := func(storageType string) (storage.Storage, error) {
		resolvedType = storageType
		return objectBackend, nil
	}
	plugin := &MP4Plugin{}
	stream := &m7s.RecordStream{StorageType: objectType, FilePath: "record.mp4"}
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test?id=1", nil)
	response := httptest.NewRecorder()

	plugin.downloadSingleFileWithResolver(resolver, *stream, 0, response, request)

	if resolvedType != objectType {
		t.Fatalf("resolved type=%q, want %q", resolvedType, objectType)
	}
	if response.Code != http.StatusFound {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != "https://object.invalid/record.mp4" {
		t.Fatalf("location=%q", response.Header().Get("Location"))
	}
}
