package m7s

import (
	"context"
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
