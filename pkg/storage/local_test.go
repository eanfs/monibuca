package storage

import (
    "context"
    "os"
    "path/filepath"
    "testing"
)

func TestLocalStorage_CreateWriteRead(t *testing.T) {
    base := filepath.Join(os.TempDir(), "monibuca_local_storage_test")
    s, err := NewLocalStorage(LocalStorageConfig(base))
    if err != nil {
        t.Fatalf("NewLocalStorage error: %v", err)
    }
    defer s.Close()

    path := filepath.Join(base, "a", "b", "test.bin")
    f, err := s.CreateFile(context.Background(), path)
    if err != nil {
        t.Fatalf("CreateFile error: %v", err)
    }
    defer func() {
        _ = f.Close()
        _ = s.Delete(context.Background(), path)
    }()

    data := []byte("hello")
    if _, err = f.Write(data); err != nil {
        t.Fatalf("Write error: %v", err)
    }
    if err = f.Sync(); err != nil {
        t.Fatalf("Sync error: %v", err)
    }

    exists, err := s.Exists(context.Background(), path)
    if err != nil || !exists {
        t.Fatalf("Exists error or not exists: %v, exists=%v", err, exists)
    }

    size, err := s.GetSize(context.Background(), path)
    if err != nil {
        t.Fatalf("GetSize error: %v", err)
    }
    if size != int64(len(data)) {
        t.Fatalf("size mismatch: got %d want %d", size, len(data))
    }
}
