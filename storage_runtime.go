package m7s

import (
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"m7s.live/v5/pkg/storage"
)

type StorageStatus struct {
	DesiredType    string    `json:"desiredType"`
	ActiveType     string    `json:"activeType"`
	Degraded       bool      `json:"degraded"`
	FallbackActive bool      `json:"fallbackActive"`
	LastCheckTime  time.Time `json:"lastCheckTime"`
	LastError      string    `json:"lastError,omitempty"`
}

type storageSnapshot struct {
	backend storage.Storage
	status  StorageStatus
}

type storageRuntime struct {
	snapshot atomic.Pointer[storageSnapshot]
	registry *storage.Registry
}

func (s *Server) activateStorage(backend storage.Storage, status StorageStatus) {
	if s.storageRuntime == nil {
		s.storageRuntime = &storageRuntime{}
	}
	s.storageRuntime.snapshot.Store(&storageSnapshot{backend: backend, status: status})
}

func (s *Server) loadStorageSnapshot() (storage.Storage, StorageStatus) {
	if s.storageRuntime == nil {
		return nil, StorageStatus{}
	}
	snapshot := s.storageRuntime.snapshot.Load()
	if snapshot == nil {
		return nil, StorageStatus{}
	}
	return snapshot.backend, snapshot.status
}

func (s *Server) GetStorage() storage.Storage {
	backend, _ := s.loadStorageSnapshot()
	return backend
}

func (s *Server) GetStorageStatus() StorageStatus {
	_, status := s.loadStorageSnapshot()
	return status
}

func normalizeRecordStorageType(storageType string) string {
	if storageType == "" {
		return string(storage.StorageTypeLocal)
	}
	return storageType
}

func (s *Server) GetStorageForType(storageType string) (storage.Storage, error) {
	normalized := normalizeRecordStorageType(storageType)
	if s.storageRuntime == nil || s.storageRuntime.registry == nil {
		return nil, storage.ErrStorageNotAvailable
	}
	resolved, err := s.storageRuntime.registry.GetOrCreate(normalized)
	if err != nil {
		return nil, fmt.Errorf("resolve record storage %s: %w", normalized, err)
	}
	return resolved, nil
}

func storageErrorSummary(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, storage.ErrUnsupportedStorageType):
		return "configured storage type is unavailable in this build"
	case errors.Is(err, storage.ErrStorageTypeNotConfigured):
		return "record storage type is no longer configured"
	case errors.Is(err, storage.ErrInvalidStorageConfig):
		return "configured storage settings are invalid"
	case storage.IsPermanentConnectionError(err):
		return "configured storage authentication or settings are invalid"
	default:
		return "configured storage is temporarily unavailable"
	}
}

func storageErrorCategory(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, storage.ErrUnsupportedStorageType):
		return "unsupported_build"
	case errors.Is(err, storage.ErrStorageTypeNotConfigured):
		return "not_configured"
	case errors.Is(err, storage.ErrInvalidStorageConfig):
		return "invalid_config"
	case storage.IsPermanentConnectionError(err):
		return "permanent_connection"
	default:
		return "transient_connection"
	}
}

func newStorageStatus(desired, active string, degraded, fallback bool, checkedAt time.Time, err error) StorageStatus {
	return StorageStatus{
		DesiredType: desired, ActiveType: active, Degraded: degraded,
		FallbackActive: fallback, LastCheckTime: checkedAt, LastError: storageErrorSummary(err),
	}
}
