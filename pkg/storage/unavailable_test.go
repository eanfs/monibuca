package storage

import (
	"context"
	"errors"
	"testing"
)

func TestUnavailableStorageReturnsSentinel(t *testing.T) {
	unavailable := NewUnavailableStorage("s3")
	if unavailable.GetKey() != "s3" {
		t.Fatalf("key = %q", unavailable.GetKey())
	}
	if _, err := unavailable.CreateFile(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
		t.Fatalf("CreateFile error = %v", err)
	}
	if _, err := unavailable.OpenFile(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
		t.Fatalf("OpenFile error = %v", err)
	}
	if err := unavailable.Delete(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
		t.Fatalf("Delete error = %v", err)
	}
	if _, err := unavailable.Exists(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
		t.Fatalf("Exists error = %v", err)
	}
	if _, err := unavailable.GetSize(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
		t.Fatalf("GetSize error = %v", err)
	}
	if _, err := unavailable.GetURL(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
		t.Fatalf("GetURL error = %v", err)
	}
	if _, err := unavailable.List(context.Background(), ""); !errors.Is(err, ErrStorageNotAvailable) {
		t.Fatalf("List error = %v", err)
	}
	if err := unavailable.Close(); err != nil {
		t.Fatalf("Close error = %v", err)
	}
}
