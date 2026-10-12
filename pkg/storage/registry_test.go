package storage

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestRegistryRejectsGetOrCreateAfterClose(t *testing.T) {
	var attempts atomic.Int32
	expected := &registryTestStorage{key: "fake"}
	registry := newRegistry(map[string]any{"fake": struct{}{}}, func(string, any) (Storage, error) {
		attempts.Add(1)
		return expected, nil
	})

	instance, err := registry.GetOrCreate("fake")
	if err != nil {
		t.Fatal(err)
	}
	if instance != expected {
		t.Fatalf("unexpected instance %p", instance)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.GetOrCreate("fake"); !errors.Is(err, ErrStorageRegistryClosed) {
		t.Fatalf("expected ErrStorageRegistryClosed, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("creator called after close: attempts=%d", attempts.Load())
	}
	if expected.closeCount.Load() != 1 {
		t.Fatalf("expected one close, got %d", expected.closeCount.Load())
	}
}

type registryGetResult struct {
	instance Storage
	err      error
}

func TestRegistryCloseWaitsForInFlightConstruction(t *testing.T) {
	var attempts atomic.Int32
	creatorStarted := make(chan struct{})
	releaseCreator := make(chan struct{})
	expected := &registryTestStorage{key: "fake"}
	registry := newRegistry(map[string]any{"fake": struct{}{}}, func(string, any) (Storage, error) {
		attempts.Add(1)
		close(creatorStarted)
		<-releaseCreator
		return expected, nil
	})

	getResult := make(chan registryGetResult, 1)
	go func() {
		instance, err := registry.GetOrCreate("fake")
		getResult <- registryGetResult{instance: instance, err: err}
	}()
	waitForRegistryTestValue(t, creatorStarted, "creator to start")

	closeCallStarted := make(chan struct{})
	closeResult := make(chan error, 1)
	go func() {
		close(closeCallStarted)
		closeResult <- registry.Close()
	}()
	waitForRegistryTestValue(t, closeCallStarted, "Close call to start")
	waitForRegistryCloseWriter(t, registry)

	select {
	case err := <-closeResult:
		t.Fatalf("Close returned before creator completed: %v", err)
	default:
	}

	close(releaseCreator)
	created := waitForRegistryTestValue(t, getResult, "GetOrCreate to finish")
	if created.err != nil {
		t.Fatalf("GetOrCreate: %v", created.err)
	}
	if created.instance != expected {
		t.Fatalf("unexpected instance %p", created.instance)
	}
	if err := waitForRegistryTestValue(t, closeResult, "Close to finish"); err != nil {
		t.Fatal(err)
	}
	if expected.closeCount.Load() != 1 {
		t.Fatalf("instance must close before Close returns: close count=%d", expected.closeCount.Load())
	}
	if _, err := registry.GetOrCreate("fake"); !errors.Is(err, ErrStorageRegistryClosed) {
		t.Fatalf("expected ErrStorageRegistryClosed after Close, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("creator called after close: attempts=%d", attempts.Load())
	}
}

type registryBlockingCloseStorage struct {
	registryTestStorage
	started   chan struct{}
	startOnce sync.Once
	release   <-chan struct{}
	closeErr  error
}

func (s *registryBlockingCloseStorage) Close() error {
	s.closeCount.Add(1)
	s.startOnce.Do(func() { close(s.started) })
	<-s.release
	return s.closeErr
}

func TestRegistryConcurrentAndRepeatedCloseReturnsSameResult(t *testing.T) {
	releaseClose := make(chan struct{})
	expectedCloseErr := errors.New("close failed")
	expected := &registryBlockingCloseStorage{
		registryTestStorage: registryTestStorage{key: "fake"},
		started:             make(chan struct{}),
		release:             releaseClose,
		closeErr:            expectedCloseErr,
	}
	registry := newRegistry(map[string]any{"fake": struct{}{}}, func(string, any) (Storage, error) {
		return expected, nil
	})
	if _, err := registry.GetOrCreate("fake"); err != nil {
		t.Fatal(err)
	}

	firstCloseResult := make(chan error, 1)
	go func() { firstCloseResult <- registry.Close() }()
	waitForRegistryTestValue(t, expected.started, "first Close to reach storage")

	secondCallStarted := make(chan struct{})
	secondCloseResult := make(chan error, 1)
	go func() {
		close(secondCallStarted)
		secondCloseResult <- registry.Close()
	}()
	waitForRegistryTestValue(t, secondCallStarted, "second Close call to start")
	close(releaseClose)

	firstErr := waitForRegistryTestValue(t, firstCloseResult, "first Close to finish")
	secondErr := waitForRegistryTestValue(t, secondCloseResult, "second Close to finish")
	if !errors.Is(firstErr, expectedCloseErr) {
		t.Fatalf("first Close error = %v", firstErr)
	}
	if firstErr != secondErr {
		t.Fatalf("concurrent Close returned different errors: first=%v second=%v", firstErr, secondErr)
	}
	if thirdErr := registry.Close(); thirdErr != firstErr {
		t.Fatalf("repeated Close returned different error: first=%v repeated=%v", firstErr, thirdErr)
	}
	if expected.closeCount.Load() != 1 {
		t.Fatalf("expected one storage close, got %d", expected.closeCount.Load())
	}
}

func waitForRegistryCloseWriter(t *testing.T, registry *Registry) {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		if !registry.mu.TryRLock() {
			return
		}
		registry.mu.RUnlock()
		select {
		case <-timeout.C:
			t.Fatal("Close did not acquire the lifecycle write guard")
		default:
			runtime.Gosched()
		}
	}
}

func waitForRegistryTestValue[T any](t *testing.T, values <-chan T, description string) T {
	t.Helper()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	select {
	case value := <-values:
		return value
	case <-timeout.C:
		t.Fatalf("timed out waiting for %s", description)
		var zero T
		return zero
	}
}
