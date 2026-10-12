package plugin_mp4

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"m7s.live/v5/pkg/storage"
)

type redirectTestStorage struct {
	url string
}

func (s *redirectTestStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *redirectTestStorage) OpenFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *redirectTestStorage) Delete(context.Context, string) error           { return nil }
func (s *redirectTestStorage) Exists(context.Context, string) (bool, error)   { return true, nil }
func (s *redirectTestStorage) GetSize(context.Context, string) (int64, error) { return 0, nil }
func (s *redirectTestStorage) GetURL(context.Context, string) (string, error) { return s.url, nil }
func (s *redirectTestStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *redirectTestStorage) Close() error   { return nil }
func (s *redirectTestStorage) GetKey() string { return string(storage.StorageTypeS3) }

func TestRedirectToStorageURLDoesNotLogPresignedCredentials(t *testing.T) {
	const signedURL = "https://bucket.example/record.mp4?X-Amz-Credential=AKIA_TEST&X-Amz-Signature=top-secret"
	var logs bytes.Buffer
	plugin := &MP4Plugin{}
	plugin.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	request := httptest.NewRequest(http.MethodGet, "/mp4/download/live.mp4", nil)
	response := httptest.NewRecorder()

	err := plugin.redirectToStorageURL(
		&redirectTestStorage{url: signedURL},
		string(storage.StorageTypeS3),
		"records/record.mp4",
		response,
		request,
	)
	if err != nil {
		t.Fatalf("redirectToStorageURL: %v", err)
	}
	if response.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusFound)
	}
	if location := response.Header().Get("Location"); !strings.Contains(location, "top-secret") {
		t.Fatalf("redirect must retain presigned URL, location=%q", location)
	}
	for _, secret := range []string{signedURL, "X-Amz-Signature", "X-Amz-Credential", "top-secret", "AKIA_TEST"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain presigned credential %q: %s", secret, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "storageType=s3") {
		t.Errorf("safe storage context missing from logs: %s", logs.String())
	}
}
