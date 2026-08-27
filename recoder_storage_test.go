package m7s

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"m7s.live/v5/pkg/storage"
)

func TestRecordingStorageRejectsDegradedWithoutFallback(t *testing.T) {
	server := &Server{}
	server.activateStorage(storage.NewUnavailableStorage("s3"), StorageStatus{
		DesiredType: "s3",
		ActiveType:  "s3",
		Degraded:    true,
		LastError:   "s3 endpoint https://private.example bucket=secret is unavailable",
	})

	resolved, err := server.recordingStorage()
	if resolved != nil {
		t.Fatalf("storage=%T, want nil", resolved)
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
	if got, want := status.Convert(err).Message(), "configured storage is not ready"; got != want {
		t.Fatalf("message=%q, want %q", got, want)
	}
}

func TestRecordingStorageAllowsExplicitFallback(t *testing.T) {
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	server.activateStorage(local, StorageStatus{
		DesiredType:    "s3",
		ActiveType:     "local",
		Degraded:       true,
		FallbackActive: true,
	})

	resolved, err := server.recordingStorage()
	if err != nil {
		t.Fatalf("recordingStorage() error = %v", err)
	}
	if resolved != local {
		t.Fatalf("storage=%T %p, want local %p", resolved, resolved, local)
	}
	if got := resolved.GetKey(); got != "local" {
		t.Fatalf("storage key=%q, want local", got)
	}
}

func TestRecordingStorageRejectsNilBackendWhenNotDegraded(t *testing.T) {
	server := &Server{}
	server.activateStorage(nil, StorageStatus{
		DesiredType: "local",
		ActiveType:  "local",
	})

	resolved, err := server.recordingStorage()
	if resolved != nil {
		t.Fatalf("storage=%T, want nil", resolved)
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("code=%s err=%v", status.Code(err), err)
	}
	if got, want := status.Convert(err).Message(), "storage is not initialized"; got != want {
		t.Fatalf("message=%q, want %q", got, want)
	}
}

func TestValidateRecordingStorageContract(t *testing.T) {
	tests := []struct {
		name        string
		backend     storage.Storage
		status      StorageStatus
		wantCode    codes.Code
		wantMessage string
	}{
		{
			name:        "required S3 is degraded",
			backend:     storage.NewUnavailableStorage("s3"),
			status:      StorageStatus{DesiredType: "s3", ActiveType: "s3", Degraded: true, LastError: "private failure detail"},
			wantCode:    codes.Unavailable,
			wantMessage: "configured storage is not ready",
		},
		{
			name:    "explicit local fallback is active",
			backend: storage.NewUnavailableStorage("local"),
			status: StorageStatus{
				DesiredType:    "s3",
				ActiveType:     "local",
				Degraded:       true,
				FallbackActive: true,
			},
			wantCode: codes.OK,
		},
		{
			name:     "configured backend is healthy",
			backend:  storage.NewUnavailableStorage("s3"),
			status:   StorageStatus{DesiredType: "s3", ActiveType: "s3"},
			wantCode: codes.OK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{}
			server.activateStorage(tt.backend, tt.status)

			err := server.ValidateRecordingStorage()
			if got := status.Code(err); got != tt.wantCode {
				t.Fatalf("code=%s, want %s; err=%v", got, tt.wantCode, err)
			}
			if tt.wantMessage != "" {
				if got := status.Convert(err).Message(); got != tt.wantMessage {
					t.Fatalf("message=%q, want %q", got, tt.wantMessage)
				}
			}
		})
	}
}
