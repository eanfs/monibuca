package m7s

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	"m7s.live/v5/pkg/config"
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
}

func TestStorageReconnectRunKeepsFallbackDegradedUntilSuccess(t *testing.T) {
	var attempts atomic.Int32
	recovered := &runtimeTestStorage{key: "s3"}
	installS3TestFactory(t, func(any) (storage.Storage, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("connection refused")
		}
		return recovered, nil
	})
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = local.Close() })
	server := &Server{storageRuntime: &storageRuntime{
		registry: storage.NewRegistry(map[string]any{"s3": struct{}{}}),
	}}
	t.Cleanup(server.Dispose)
	server.activateStorage(local, newStorageStatus("s3", "local", true, true, time.Now(), errors.New("connection refused")))
	taskUnderTest := &StorageReconnectTask{s: server}

	if err := taskUnderTest.Run(); err == nil {
		t.Fatal("first run must request retry")
	}
	if current := server.GetStorageStatus(); !current.Degraded || !current.FallbackActive || server.GetStorage().GetKey() != "local" {
		t.Fatalf("first status=%+v active=%s", current, server.GetStorage().GetKey())
	}
	if err := taskUnderTest.Run(); !errors.Is(err, task.ErrTaskComplete) {
		t.Fatalf("second run=%v", err)
	}
	if current := server.GetStorageStatus(); current.Degraded || current.FallbackActive || server.GetStorage() != recovered {
		t.Fatalf("recovered status=%+v active=%s", current, server.GetStorage().GetKey())
	}
}

func TestStorageReconnectRunStopsOnPermanentErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "access denied", err: errors.New("AccessDenied")},
		{name: "invalid access key", err: errors.New("InvalidAccessKeyId")},
		{name: "signature mismatch", err: errors.New("SignatureDoesNotMatch")},
		{name: "unsupported build", err: storage.ErrUnsupportedStorageType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			installS3TestFactory(t, func(any) (storage.Storage, error) {
				return nil, test.err
			})
			server := &Server{storageRuntime: &storageRuntime{
				registry: storage.NewRegistry(map[string]any{"s3": struct{}{}}),
			}}
			t.Cleanup(server.Dispose)
			fallback := &runtimeTestStorage{key: "local"}
			server.activateStorage(fallback, newStorageStatus("s3", "local", true, true, time.Now(), errors.New("initial failure")))

			err := (&StorageReconnectTask{s: server}).Run()

			if !errors.Is(err, task.ErrTaskComplete) {
				t.Fatalf("Run() error = %v, want ErrTaskComplete", err)
			}
			active, status := server.loadStorageSnapshot()
			if active != fallback || !status.Degraded || !status.FallbackActive || status.LastCheckTime.IsZero() {
				t.Fatalf("active=%v status=%+v", active, status)
			}
			if status.LastError == "" || strings.Contains(status.LastError, test.err.Error()) {
				t.Fatalf("permanent error must remain sanitized, got %q", status.LastError)
			}
		})
	}
}

func TestNewStorageReconnectTaskRetriesWithoutLimit(t *testing.T) {
	reconnect := newStorageReconnectTask(&Server{})
	if got := reconnect.GetTask().GetMaxRetry(); got != -1 {
		t.Fatalf("max retry = %d, want -1", got)
	}
}

func TestStorageReconnectRunWritesSanitizedFailureAndRecoveryAlarms(t *testing.T) {
	var attempts atomic.Int32
	rawError := errors.New("connection refused credential=do-not-persist")
	installS3TestFactory(t, func(any) (storage.Storage, error) {
		if attempts.Add(1) == 1 {
			return nil, rawError
		}
		return &runtimeTestStorage{key: "s3"}, nil
	})
	database := newUploadTestDB(t)
	if err := database.AutoMigrate(&AlarmInfo{}); err != nil {
		t.Fatalf("migrate alarms: %v", err)
	}
	server := &Server{storageRuntime: &storageRuntime{
		registry: storage.NewRegistry(map[string]any{"s3": struct{}{}}),
	}}
	server.DB = database
	t.Cleanup(server.Dispose)
	server.activateStorage(&runtimeTestStorage{key: "local"}, newStorageStatus("s3", "local", true, true, time.Now(), rawError))
	reconnect := &StorageReconnectTask{s: server}

	if err := reconnect.Run(); err == nil || errors.Is(err, task.ErrTaskComplete) {
		t.Fatalf("transient Run() error = %v, want retryable error", err)
	}
	if err := reconnect.Run(); !errors.Is(err, task.ErrTaskComplete) {
		t.Fatalf("recovery Run() error = %v", err)
	}

	var alarms []AlarmInfo
	if err := database.Order("id ASC").Find(&alarms).Error; err != nil {
		t.Fatalf("query alarms: %v", err)
	}
	if len(alarms) != 2 {
		t.Fatalf("alarm count = %d, want 2", len(alarms))
	}
	if alarms[0].AlarmType != config.AlarmStorageException || alarms[1].AlarmType != config.AlarmStorageExceptionRecover {
		t.Fatalf("alarm types = [%d %d]", alarms[0].AlarmType, alarms[1].AlarmType)
	}
	for _, alarm := range alarms {
		if strings.Contains(alarm.AlarmDesc, "do-not-persist") {
			t.Fatalf("alarm contains raw credential: %+v", alarm)
		}
	}
}
