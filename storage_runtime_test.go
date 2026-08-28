package m7s

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"m7s.live/v5/pkg/storage"
)

type runtimeTestStorage struct{ key string }

func (s *runtimeTestStorage) CreateFile(context.Context, string) (storage.File, error) {
	return nil, nil
}
func (s *runtimeTestStorage) OpenFile(context.Context, string) (storage.File, error) { return nil, nil }
func (s *runtimeTestStorage) Delete(context.Context, string) error                   { return nil }
func (s *runtimeTestStorage) Exists(context.Context, string) (bool, error)           { return true, nil }
func (s *runtimeTestStorage) GetSize(context.Context, string) (int64, error)         { return 0, nil }
func (s *runtimeTestStorage) GetURL(context.Context, string) (string, error)         { return "", nil }
func (s *runtimeTestStorage) List(context.Context, string) ([]storage.FileInfo, error) {
	return nil, nil
}
func (s *runtimeTestStorage) Close() error   { return nil }
func (s *runtimeTestStorage) GetKey() string { return s.key }

func TestGetStorageForTypeDoesNotDependOnActiveBackend(t *testing.T) {
	const objectType = "storage-runtime-object-test"
	original, existed := storage.Factory[objectType]
	t.Cleanup(func() {
		if existed {
			storage.Factory[objectType] = original
		} else {
			delete(storage.Factory, objectType)
		}
	})
	objectBackend := &runtimeTestStorage{key: objectType}
	storage.Factory[objectType] = func(any) (storage.Storage, error) { return objectBackend, nil }

	server := &Server{storageRuntime: &storageRuntime{registry: storage.NewRegistry(map[string]any{objectType: struct{}{}})}}
	server.activateStorage(&runtimeTestStorage{key: "local"}, StorageStatus{DesiredType: objectType, ActiveType: "local", Degraded: true, FallbackActive: true})

	resolved, err := server.GetStorageForType(objectType)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != objectBackend {
		t.Fatalf("resolved %p, expected %p", resolved, objectBackend)
	}
	if server.GetStorage().GetKey() != "local" {
		t.Fatal("resolving a historical backend must not change active storage")
	}
}

func TestGetStorageForTypeNormalizesLegacyLocal(t *testing.T) {
	server := &Server{storageRuntime: &storageRuntime{registry: storage.NewRegistry(nil)}}
	for _, storageType := range []string{"", "local"} {
		resolved, err := server.GetStorageForType(storageType)
		if err != nil {
			t.Fatalf("type %q: %v", storageType, err)
		}
		if resolved.GetKey() != "local" {
			t.Fatalf("type %q resolved to %q", storageType, resolved.GetKey())
		}
	}
}

func TestGetStorageForTypeReturnsUnavailableWithoutRegistry(t *testing.T) {
	tests := []struct {
		name   string
		server *Server
	}{
		{name: "runtime is nil", server: &Server{}},
		{name: "registry is nil", server: &Server{storageRuntime: &storageRuntime{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.server.GetStorageForType("local")
			if !errors.Is(err, storage.ErrStorageNotAvailable) {
				t.Fatalf("GetStorageForType() error = %v, want ErrStorageNotAvailable", err)
			}
		})
	}
}

func TestGetStorageForTypePreservesRegistryErrorChain(t *testing.T) {
	registry := storage.NewRegistry(nil)
	server := &Server{storageRuntime: &storageRuntime{registry: registry}}

	_, err := server.GetStorageForType("not-configured")
	if !errors.Is(err, storage.ErrStorageTypeNotConfigured) {
		t.Fatalf("unconfigured error = %v, want ErrStorageTypeNotConfigured", err)
	}

	if err = registry.Close(); err != nil {
		t.Fatalf("close registry: %v", err)
	}
	_, err = server.GetStorageForType("local")
	if !errors.Is(err, storage.ErrStorageRegistryClosed) {
		t.Fatalf("closed error = %v, want ErrStorageRegistryClosed", err)
	}
}

func TestStorageSnapshotSwitchIsAtomic(t *testing.T) {
	server := &Server{}
	local := &runtimeTestStorage{key: "local"}
	s3 := &runtimeTestStorage{key: "s3"}
	localStatus := StorageStatus{DesiredType: "s3", ActiveType: "local", Degraded: true, FallbackActive: true}
	s3Status := StorageStatus{DesiredType: "s3", ActiveType: "s3", LastCheckTime: time.Now()}
	server.activateStorage(local, localStatus)

	const iterations = 1_000
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for range iterations {
			server.activateStorage(s3, s3Status)
			server.activateStorage(local, localStatus)
		}
	}()
	go func() {
		defer wait.Done()
		for range iterations * 2 {
			backend, status := server.loadStorageSnapshot()
			if backend == nil || backend.GetKey() != status.ActiveType {
				t.Errorf("inconsistent snapshot: backend=%v status=%+v", backend, status)
				return
			}
		}
	}()
	wait.Wait()
}

func TestInitStorageFallsBackFromInvalidExplicitLocalConfig(t *testing.T) {
	server := &Server{
		ServerConfig: ServerConfig{Storage: map[string]any{
			string(storage.StorageTypeLocal): 42,
		}},
	}
	server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	server.initStorage()

	active, ok := server.GetStorage().(*storage.LocalStorage)
	if !ok {
		t.Fatalf("invalid explicit local config must fall back to healthy local storage, got %T", server.GetStorage())
	}
	if got := active.GetStoragePath(1); got != "." {
		t.Errorf("fallback local path = %q, want .", got)
	}
	registry := server.storageRuntime.registry
	owned, err := registry.GetOrCreate(string(storage.StorageTypeLocal))
	if err != nil {
		t.Fatalf("get registry-owned fallback: %v", err)
	}
	if owned != active {
		t.Error("active fallback must be owned by the installed registry")
	}

	server.Dispose()
	if _, err = registry.GetOrCreate(string(storage.StorageTypeLocal)); !errors.Is(err, storage.ErrStorageRegistryClosed) {
		t.Fatalf("Dispose must close fallback registry, got %v", err)
	}
}
