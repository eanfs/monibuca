package m7s

import (
	"errors"
	"strings"
	"testing"

	"m7s.live/v5/pkg/storage"
)

func installS3TestFactory(t *testing.T, factory func(any) (storage.Storage, error)) {
	t.Helper()
	original, existed := storage.Factory[string(storage.StorageTypeS3)]
	storage.Factory[string(storage.StorageTypeS3)] = factory
	t.Cleanup(func() {
		if existed {
			storage.Factory[string(storage.StorageTypeS3)] = original
		} else {
			delete(storage.Factory, string(storage.StorageTypeS3))
		}
	})
}

func newS3InitTestServer(t *testing.T, allowFallback bool, s3Error error) *Server {
	t.Helper()
	installS3TestFactory(t, func(any) (storage.Storage, error) {
		if s3Error != nil {
			return nil, s3Error
		}
		return &runtimeTestStorage{key: string(storage.StorageTypeS3)}, nil
	})
	server := &Server{ServerConfig: ServerConfig{
		Storage:                   map[string]any{string(storage.StorageTypeS3): struct{}{}},
		StorageAllowLocalFallback: allowFallback,
	}}
	t.Cleanup(server.Dispose)
	return server
}

func TestInitS3StorageFallbackPolicy(t *testing.T) {
	tests := []struct {
		name          string
		allowFallback bool
		s3Error       error
		wantActive    string
		wantDegraded  bool
		wantFallback  bool
	}{
		{name: "s3 ready", wantActive: "s3"},
		{name: "default blocks local fallback", s3Error: errors.New("connection refused"), wantActive: "s3", wantDegraded: true},
		{name: "explicit fallback is degraded local", allowFallback: true, s3Error: errors.New("connection refused"), wantActive: "local", wantDegraded: true, wantFallback: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newS3InitTestServer(t, test.allowFallback, test.s3Error)

			server.initStorage()

			backend, status := server.loadStorageSnapshot()
			if backend == nil {
				t.Fatal("active backend is nil")
			}
			if backend.GetKey() != test.wantActive || status.ActiveType != test.wantActive || status.Degraded != test.wantDegraded || status.FallbackActive != test.wantFallback {
				t.Fatalf("backend=%q status=%+v", backend.GetKey(), status)
			}
			if status.DesiredType != "s3" {
				t.Fatalf("desired type=%q, want s3", status.DesiredType)
			}
			if status.LastCheckTime.IsZero() {
				t.Fatal("last check time must be recorded")
			}
			if test.s3Error == nil && status.LastError != "" {
				t.Fatalf("healthy status has last error %q", status.LastError)
			}
			if test.s3Error != nil && (status.LastError == "" || strings.Contains(status.LastError, test.s3Error.Error())) {
				t.Fatalf("degraded status must contain only a sanitized error summary, got %q", status.LastError)
			}
			if gotWork := server.storageReconnectWork != nil; gotWork != (test.s3Error != nil) {
				t.Fatalf("reconnect work present=%v, S3 error present=%v", gotWork, test.s3Error != nil)
			}
		})
	}
}

func TestInitStorageWithoutS3RemainsHealthyLocal(t *testing.T) {
	server := &Server{ServerConfig: ServerConfig{Storage: nil}}
	t.Cleanup(server.Dispose)

	server.initStorage()

	backend, status := server.loadStorageSnapshot()
	if backend == nil {
		t.Fatal("active backend is nil")
	}
	if backend.GetKey() != "local" || status.ActiveType != "local" || status.DesiredType != "local" || status.Degraded || status.FallbackActive {
		t.Fatalf("backend=%q status=%+v", backend.GetKey(), status)
	}
	if server.storageReconnectWork != nil {
		t.Fatal("local-only startup must not create reconnect work")
	}
}
