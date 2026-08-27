package m7s

import (
	"errors"
	"time"

	task "github.com/eanfs/gotask"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

const (
	storageReconnectBaseInterval = 5 * time.Second
	storageReconnectMaxInterval  = 5 * time.Minute
)

// StorageReconnectTask retries S3 initialization with gotask's native
// exponential backoff until the backend recovers or a permanent error occurs.
type StorageReconnectTask struct {
	task.Task
	s *Server
}

func newStorageReconnectTask(s *Server) *StorageReconnectTask {
	reconnect := &StorageReconnectTask{s: s}
	reconnect.SetRetry(-1, storageReconnectBaseInterval)
	reconnect.GetTask().SetMaxRetryInterval(storageReconnectMaxInterval)
	return reconnect
}

func (t *StorageReconnectTask) Run() error {
	backend, err := t.s.storageRuntime.registry.GetOrCreate(string(storage.StorageTypeS3))
	checkedAt := time.Now()
	if err != nil {
		active, current := t.s.loadStorageSnapshot()
		t.s.activateStorage(active, newStorageStatus("s3", current.ActiveType, true, current.FallbackActive, checkedAt, err))
		RaiseUploadAlarm(t.s.DB, config.AlarmStorageException, "S3 storage unavailable", "", "s3", storageErrorSummary(err))
		t.Warn("S3 reconnect failed", "err", err)
		if errors.Is(err, storage.ErrUnsupportedStorageType) || storage.IsPermanentConnectionError(err) {
			return task.ErrTaskComplete
		}
		return err
	}

	t.s.activateStorage(backend, newStorageStatus("s3", "s3", false, false, checkedAt, nil))
	RaiseUploadAlarm(t.s.DB, config.AlarmStorageExceptionRecover, "S3 storage recovered", "", "s3", "S3 storage is ready")
	t.Info("S3 storage recovered", "type", "s3")
	return task.ErrTaskComplete
}

func (s *Server) scheduleStorageReconnect() {
	reconnect := newStorageReconnectTask(s)
	s.Records.OnStart(func() {
		s.Records.AddTask(reconnect)
	})
}
