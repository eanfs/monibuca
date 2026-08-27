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
	storageReconnectPollInterval = time.Second
)

// StorageReconnectWork isolates connection polling from recording and upload
// retry tasks so reconnect backoff cannot block the Records event loop.
type StorageReconnectWork struct {
	task.Work
}

// StorageReconnectTask polls on a short fixed ticker and manages its own
// retry deadline. It never returns connection errors to gotask.
type StorageReconnectTask struct {
	task.TickTask
	s            *Server
	now          func() time.Time
	tickInterval time.Duration
	attempt      int
	nextAttempt  time.Time
	completed    bool
}

func newStorageReconnectTask(s *Server) *StorageReconnectTask {
	now := time.Now
	return &StorageReconnectTask{
		s:            s,
		now:          now,
		tickInterval: storageReconnectPollInterval,
		attempt:      1,
		nextAttempt:  now().Add(storageReconnectBackoff(1)),
	}
}

func (t *StorageReconnectTask) GetTickInterval() time.Duration {
	if t.tickInterval > 0 {
		return t.tickInterval
	}
	return storageReconnectPollInterval
}

func (t *StorageReconnectTask) Tick(any) {
	if t.completed {
		return
	}
	checkedAt := t.currentTime()
	if checkedAt.Before(t.nextAttempt) {
		return
	}

	backend, err := t.s.storageRuntime.registry.GetOrCreate(string(storage.StorageTypeS3))
	if err != nil {
		t.handleFailure(checkedAt, err)
		return
	}

	t.nextAttempt = time.Time{}
	t.s.activateStorage(backend, newStorageStatus("s3", "s3", false, false, checkedAt, nil))
	RaiseUploadAlarm(t.s.DB, config.AlarmStorageExceptionRecover, "S3 storage recovered", "", "s3", "S3 storage is ready")
	t.Info("S3 storage recovered", "type", "s3")
	t.complete()
}

func (t *StorageReconnectTask) handleFailure(checkedAt time.Time, err error) {
	active, current := t.s.loadStorageSnapshot()
	t.s.activateStorage(active, newStorageStatus("s3", current.ActiveType, true, current.FallbackActive, checkedAt, err))
	summary := storageErrorSummary(err)
	RaiseUploadAlarm(t.s.DB, config.AlarmStorageException, "S3 storage unavailable", "", "s3", summary)
	t.Warn("S3 reconnect failed",
		"errorCategory", storageErrorCategory(err),
		"error", summary)

	if errors.Is(err, storage.ErrUnsupportedStorageType) || storage.IsPermanentConnectionError(err) {
		t.nextAttempt = time.Time{}
		t.complete()
		return
	}

	t.attempt++
	t.nextAttempt = checkedAt.Add(storageReconnectBackoff(t.attempt))
}

func (t *StorageReconnectTask) complete() {
	t.completed = true
	state := t.GetState()
	if state >= task.TASK_STATE_STARTED && state < task.TASK_STATE_DISPOSING {
		t.Stop(task.ErrTaskComplete)
	}
}

func (t *StorageReconnectTask) currentTime() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

func storageReconnectBackoff(attempt int) time.Duration {
	if attempt <= 1 {
		return storageReconnectBaseInterval
	}
	delay := storageReconnectBaseInterval
	for step := 1; step < attempt; step++ {
		if delay >= storageReconnectMaxInterval/2 {
			return storageReconnectMaxInterval
		}
		delay *= 2
	}
	return delay
}

func newStorageReconnectWork(reconnect *StorageReconnectTask) *StorageReconnectWork {
	work := &StorageReconnectWork{}
	work.OnStart(func() {
		work.AddTask(reconnect)
	})
	return work
}

func (s *Server) scheduleStorageReconnect() {
	s.storageReconnectWork = newStorageReconnectWork(newStorageReconnectTask(s))
}
