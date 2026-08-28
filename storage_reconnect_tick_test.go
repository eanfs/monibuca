package m7s

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	"m7s.live/v5/pkg/storage"
)

func TestStorageReconnectBackoff(t *testing.T) {
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: 5 * time.Second},
		{attempt: 1, want: 5 * time.Second},
		{attempt: 2, want: 10 * time.Second},
		{attempt: 3, want: 20 * time.Second},
		{attempt: 4, want: 40 * time.Second},
		{attempt: 5, want: 80 * time.Second},
		{attempt: 6, want: 160 * time.Second},
		{attempt: 7, want: 5 * time.Minute},
		{attempt: 100, want: 5 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.want.String(), func(t *testing.T) {
			if got := storageReconnectBackoff(test.attempt); got != test.want {
				t.Fatalf("attempt %d backoff = %v, want %v", test.attempt, got, test.want)
			}
		})
	}
}

func TestNewStorageReconnectTaskUsesProductionPollingDefaults(t *testing.T) {
	before := time.Now()
	reconnect := newStorageReconnectTask(&Server{})
	after := time.Now()

	if got := reconnect.GetTickInterval(); got != time.Second {
		t.Fatalf("tick interval = %v, want 1s", got)
	}
	reconnect.tickInterval = 0
	if got := reconnect.GetTickInterval(); got != time.Second {
		t.Fatalf("zero tick interval fallback = %v, want 1s", got)
	}
	if reconnect.attempt != 1 {
		t.Fatalf("initial attempt = %d, want 1", reconnect.attempt)
	}
	minimum := before.Add(storageReconnectBaseInterval)
	maximum := after.Add(storageReconnectBaseInterval)
	if reconnect.nextAttempt.Before(minimum) || reconnect.nextAttempt.After(maximum) {
		t.Fatalf("next attempt = %v, want between %v and %v", reconnect.nextAttempt, minimum, maximum)
	}
	reconnect.now = nil
	clockBefore := time.Now()
	gotNow := reconnect.currentTime()
	clockAfter := time.Now()
	if gotNow.Before(clockBefore) || gotNow.After(clockAfter) {
		t.Fatalf("default current time = %v, want between %v and %v", gotNow, clockBefore, clockAfter)
	}
}

func TestStorageReconnectTickHonorsBackoffAndRecovers(t *testing.T) {
	var attempts int
	recovered := &runtimeTestStorage{key: "s3"}
	installS3TestFactory(t, func(any) (storage.Storage, error) {
		attempts++
		if attempts <= 2 {
			return nil, errors.New("connection refused")
		}
		return recovered, nil
	})
	registry := storage.NewRegistry(map[string]any{"s3": struct{}{}})
	if _, err := registry.GetOrCreate("s3"); err == nil {
		t.Fatal("initial creation must fail")
	}
	server := &Server{storageRuntime: &storageRuntime{registry: registry}}
	t.Cleanup(server.Dispose)
	fallback := &runtimeTestStorage{key: "local"}
	server.activateStorage(fallback, newStorageStatus("s3", "local", true, true, time.Now(), errors.New("initial failure")))

	current := time.Unix(1_000, 0)
	reconnect := newStorageReconnectTask(server)
	reconnect.now = func() time.Time { return current }
	reconnect.attempt = 1
	reconnect.nextAttempt = current.Add(5 * time.Second)

	current = current.Add(4 * time.Second)
	reconnect.Tick(nil)
	if attempts != 1 {
		t.Fatalf("factory called during backoff, attempts=%d", attempts)
	}

	current = current.Add(time.Second)
	reconnect.Tick(nil)
	if attempts != 2 || reconnect.attempt != 2 || !reconnect.nextAttempt.Equal(current.Add(10*time.Second)) {
		t.Fatalf("after transient attempt: attempts=%d taskAttempt=%d next=%v", attempts, reconnect.attempt, reconnect.nextAttempt)
	}
	if active, status := server.loadStorageSnapshot(); active != fallback || !status.Degraded || !status.FallbackActive || !status.LastCheckTime.Equal(current) || status.LastError == "" {
		t.Fatalf("transient active=%v status=%+v", active, status)
	}

	current = current.Add(9 * time.Second)
	reconnect.Tick(nil)
	if attempts != 2 {
		t.Fatalf("factory called before second backoff elapsed, attempts=%d", attempts)
	}
	current = current.Add(time.Second)
	reconnect.Tick(nil)
	if active, status := server.loadStorageSnapshot(); active != recovered || status.Degraded || status.FallbackActive || !status.LastCheckTime.Equal(current) || status.LastError != "" {
		t.Fatalf("recovered active=%v status=%+v", active, status)
	}
	if !reconnect.completed {
		t.Fatal("recovered reconnect task must complete")
	}
	reconnect.Tick(nil)
	if attempts != 3 {
		t.Fatalf("completed task retried, attempts=%d", attempts)
	}
}

func TestStorageReconnectTickStopsRetryingPermanentError(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "invalid config", err: storage.ErrInvalidStorageConfig},
		{name: "credential error", err: errors.New("SignatureDoesNotMatch")},
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
			current := time.Unix(2_000, 0)
			reconnect := newStorageReconnectTask(server)
			reconnect.now = func() time.Time { return current }
			reconnect.nextAttempt = current

			reconnect.Tick(nil)

			if active, status := server.loadStorageSnapshot(); active != fallback || !status.Degraded || !status.FallbackActive {
				t.Fatalf("permanent active=%v status=%+v", active, status)
			}
			if reconnect.nextAttempt.After(current) {
				t.Fatalf("permanent error must not schedule another attempt: %v", reconnect.nextAttempt)
			}
			if !reconnect.completed {
				t.Fatal("permanent reconnect error must complete the task")
			}
		})
	}
}

func TestInitS3FailureCreatesDedicatedReconnectWork(t *testing.T) {
	server := newS3InitTestServer(t, true, errors.New("connection refused"))

	server.initStorage()

	if server.storageReconnectWork == nil {
		t.Fatal("S3 failure must create dedicated reconnect work")
	}
}

func TestS3ReconnectSecretsNeverEscapeStatusAlarmOrLogs(t *testing.T) {
	const (
		secretMarker = "secret-access-marker"
		secretURL    = "https://AKIA_TEST:super-secret@example.invalid/bucket?X-Amz-Credential=secret-access-marker"
	)
	rawError := errors.New("dial failed " + secretMarker + " endpoint=" + secretURL)
	installS3TestFactory(t, func(any) (storage.Storage, error) {
		return nil, rawError
	})
	var logs bytes.Buffer
	server := &Server{ServerConfig: ServerConfig{
		Storage:                   map[string]any{"s3": struct{}{}},
		StorageAllowLocalFallback: true,
	}}
	server.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	server.DB = newUploadTestDB(t)
	if err := server.DB.AutoMigrate(&AlarmInfo{}); err != nil {
		t.Fatalf("migrate alarms: %v", err)
	}
	t.Cleanup(server.Dispose)

	server.initStorage()
	current := time.Unix(3_000, 0)
	reconnect := newStorageReconnectTask(server)
	reconnect.Logger = server.Logger
	reconnect.now = func() time.Time { return current }
	reconnect.nextAttempt = current
	reconnect.Tick(nil)

	var alarms []AlarmInfo
	if err := server.DB.Find(&alarms).Error; err != nil {
		t.Fatalf("query alarms: %v", err)
	}
	if len(alarms) != 1 {
		t.Fatalf("alarm count=%d, want 1", len(alarms))
	}
	status := server.GetStorageStatus()
	observed := strings.Join([]string{logs.String(), status.LastError, alarms[0].AlarmName, alarms[0].AlarmDesc, alarms[0].FilePath}, "\n")
	for _, forbidden := range []string{secretMarker, secretURL, "super-secret", "X-Amz-Credential"} {
		if strings.Contains(observed, forbidden) {
			t.Fatalf("secret %q escaped through logs/status/alarm: %s", forbidden, observed)
		}
	}
	if !strings.Contains(logs.String(), "errorCategory") || status.LastError == "" {
		t.Fatalf("sanitized diagnostics missing: logs=%s status=%+v", logs.String(), status)
	}
}

func TestDedicatedStorageReconnectWorkLifecycleNonRace(t *testing.T) {
	var attempts int
	installS3TestFactory(t, func(any) (storage.Storage, error) {
		attempts++
		return nil, errors.New("connection refused")
	})
	server := &Server{ServerConfig: ServerConfig{
		Storage:                   map[string]any{"s3": struct{}{}},
		StorageAllowLocalFallback: true,
	}}
	t.Cleanup(server.Dispose)
	server.initStorage()
	if attempts != 1 || server.storageReconnectWork == nil {
		t.Fatalf("initial attempts=%d work=%v", attempts, server.storageReconnectWork)
	}

	reconnectWork := server.storageReconnectWork
	if err := Servers.AddTask(reconnectWork).WaitStarted(); err != nil {
		t.Fatalf("start reconnect work: %v", err)
	}
	t.Cleanup(func() { stopTaskForLifecycleTest(reconnectWork.GetTask()) })

	uploadRetry := &UploadRetryScheduler{s: server, retryInterval: time.Hour}
	server.Records.OnStart(func() {
		server.Records.AddTask(uploadRetry)
	})
	if err := Servers.AddTask(&server.Records).WaitStarted(); err != nil {
		t.Fatalf("start Records: %v", err)
	}
	t.Cleanup(func() { stopTaskForLifecycleTest(server.Records.GetTask()) })

	deadline := time.Now().Add(500 * time.Millisecond)
	for uploadRetry.GetState() < task.TASK_STATE_STARTED && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if uploadRetry.GetState() < task.TASK_STATE_STARTED {
		t.Fatalf("UploadRetryScheduler did not start while reconnect was backing off; state=%v", uploadRetry.GetState())
	}
	if attempts != 1 {
		t.Fatalf("reconnect must still be in initial 5s backoff, attempts=%d", attempts)
	}

	shutdownStarted := time.Now()
	reconnectWork.Stop(task.ErrTaskComplete)
	if err := reconnectWork.WaitStopped(); err != nil && !errors.Is(err, task.ErrTaskComplete) {
		t.Fatalf("stop reconnect work: %v", err)
	}
	if elapsed := time.Since(shutdownStarted); elapsed > time.Second {
		t.Fatalf("reconnect work shutdown took %v; must not wait for backoff", elapsed)
	}
}

func stopTaskForLifecycleTest(target *task.Task) {
	if target.GetState() < task.TASK_STATE_DISPOSING {
		target.Stop(task.ErrTaskComplete)
	}
	if target.GetState() >= task.TASK_STATE_STARTED {
		_ = target.WaitStopped()
	}
}
