package storage

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type registryTestStorage struct {
	key        string
	closeCount atomic.Int32
}

func (s *registryTestStorage) CreateFile(context.Context, string) (File, error) { return nil, nil }
func (s *registryTestStorage) OpenFile(context.Context, string) (File, error)   { return nil, nil }
func (s *registryTestStorage) Delete(context.Context, string) error             { return nil }
func (s *registryTestStorage) Exists(context.Context, string) (bool, error)     { return true, nil }
func (s *registryTestStorage) GetSize(context.Context, string) (int64, error)   { return 0, nil }
func (s *registryTestStorage) GetURL(context.Context, string) (string, error)   { return "", nil }
func (s *registryTestStorage) List(context.Context, string) ([]FileInfo, error) { return nil, nil }
func (s *registryTestStorage) Close() error {
	s.closeCount.Add(1)
	return nil
}
func (s *registryTestStorage) GetKey() string { return s.key }

func TestRegistryRejectsUnconfiguredType(t *testing.T) {
	registry := newRegistry(map[string]any{}, func(string, any) (Storage, error) {
		t.Fatal("creator must not run for an unconfigured type")
		return nil, nil
	})

	_, err := registry.GetOrCreate("s3")
	if !errors.Is(err, ErrStorageTypeNotConfigured) {
		t.Fatalf("expected ErrStorageTypeNotConfigured, got %v", err)
	}
}

func TestRegistryCachesOnlySuccessfulConstruction(t *testing.T) {
	var attempts atomic.Int32
	expected := &registryTestStorage{key: "fake"}
	registry := newRegistry(map[string]any{"fake": map[string]any{"enabled": true}}, func(storageType string, _ any) (Storage, error) {
		if storageType != "fake" {
			t.Fatalf("unexpected type %q", storageType)
		}
		if attempts.Add(1) <= 2 {
			return nil, errors.New("connection refused")
		}
		return expected, nil
	})

	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := registry.GetOrCreate("fake"); err == nil {
			t.Fatalf("attempt %d should fail", attempt)
		}
	}
	first, err := registry.GetOrCreate("fake")
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.GetOrCreate("fake")
	if err != nil {
		t.Fatal(err)
	}
	if first != expected || second != expected || attempts.Load() != 3 {
		t.Fatalf("success must be cached: first=%p second=%p attempts=%d", first, second, attempts.Load())
	}
}

func TestRegistryConcurrentGetOrCreateBuildsOnce(t *testing.T) {
	var attempts atomic.Int32
	expected := &registryTestStorage{key: "fake"}
	registry := newRegistry(map[string]any{"fake": struct{}{}}, func(string, any) (Storage, error) {
		attempts.Add(1)
		return expected, nil
	})

	const workers = 32
	results := make(chan Storage, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			instance, err := registry.GetOrCreate("fake")
			if err != nil {
				t.Errorf("GetOrCreate: %v", err)
				return
			}
			results <- instance
		}()
	}
	wait.Wait()
	close(results)
	for instance := range results {
		if instance != expected {
			t.Fatalf("unexpected instance %p", instance)
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected one construction, got %d", attempts.Load())
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if expected.closeCount.Load() != 1 {
		t.Fatalf("expected one close, got %d", expected.closeCount.Load())
	}
}
