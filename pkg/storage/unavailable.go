package storage

import "context"

type unavailableStorage struct {
	configuredType string
}

func NewUnavailableStorage(configuredType string) Storage {
	return &unavailableStorage{configuredType: configuredType}
}

func (s *unavailableStorage) CreateFile(context.Context, string) (File, error) {
	return nil, ErrStorageNotAvailable
}
func (s *unavailableStorage) OpenFile(context.Context, string) (File, error) {
	return nil, ErrStorageNotAvailable
}
func (s *unavailableStorage) Delete(context.Context, string) error { return ErrStorageNotAvailable }
func (s *unavailableStorage) Exists(context.Context, string) (bool, error) {
	return false, ErrStorageNotAvailable
}
func (s *unavailableStorage) GetSize(context.Context, string) (int64, error) {
	return 0, ErrStorageNotAvailable
}
func (s *unavailableStorage) GetURL(context.Context, string) (string, error) {
	return "", ErrStorageNotAvailable
}
func (s *unavailableStorage) List(context.Context, string) ([]FileInfo, error) {
	return nil, ErrStorageNotAvailable
}
func (s *unavailableStorage) Close() error   { return nil }
func (s *unavailableStorage) GetKey() string { return s.configuredType }
