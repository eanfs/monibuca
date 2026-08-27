package storage

import (
	"errors"
	"fmt"
	"maps"
	"sync"
)

type storageCreator func(string, any) (Storage, error)

type registryEntry struct {
	mu       sync.Mutex
	config   any
	instance Storage
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]*registryEntry
	creator storageCreator
}

func NewRegistry(configs map[string]any) *Registry {
	return newRegistry(configs, CreateStorage)
}

func newRegistry(configs map[string]any, creator storageCreator) *Registry {
	cloned := maps.Clone(configs)
	if cloned == nil {
		cloned = make(map[string]any)
	}
	if _, ok := cloned[string(StorageTypeLocal)]; !ok {
		cloned[string(StorageTypeLocal)] = "."
	}
	entries := make(map[string]*registryEntry, len(cloned))
	for key, config := range cloned {
		entries[key] = &registryEntry{config: config}
	}
	return &Registry{entries: entries, creator: creator}
}

func (r *Registry) HasConfig(storageType string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.entries[storageType]
	return ok
}

func (r *Registry) GetOrCreate(storageType string) (Storage, error) {
	r.mu.RLock()
	entry, ok := r.entries[storageType]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrStorageTypeNotConfigured, storageType)
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.instance != nil {
		return entry.instance, nil
	}
	instance, err := r.creator(storageType, entry.config)
	if err != nil {
		return nil, fmt.Errorf("create storage %s: %w", storageType, err)
	}
	entry.instance = instance
	return instance, nil
}

func (r *Registry) Close() error {
	r.mu.RLock()
	entries := make([]*registryEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		entries = append(entries, entry)
	}
	r.mu.RUnlock()

	var closeErrors []error
	for _, entry := range entries {
		entry.mu.Lock()
		instance := entry.instance
		entry.instance = nil
		entry.mu.Unlock()
		if instance != nil {
			if err := instance.Close(); err != nil {
				closeErrors = append(closeErrors, err)
			}
		}
	}
	return errors.Join(closeErrors...)
}
