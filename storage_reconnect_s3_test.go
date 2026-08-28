//go:build s3

package m7s

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func invalidEndpointServerConfig(allowFallback bool) ServerConfig {
	return ServerConfig{
		Storage: map[string]any{
			"s3": map[string]any{
				"endpoint":        "http://%",
				"accesskeyid":     "unit-test-access",
				"secretaccesskey": "unit-test-secret",
				"bucket":          "unit-test-bucket",
				"usessl":          false,
			},
		},
		StorageAllowLocalFallback: allowFallback,
	}
}

func TestInvalidS3EndpointStopsReconnectAndKeepsDegraded(t *testing.T) {
	tests := []struct {
		name             string
		allowFallback    bool
		wantActive       string
		wantFallbackFlag bool
	}{
		{name: "required S3", wantActive: "s3"},
		{name: "temporary local fallback", allowFallback: true, wantActive: "local", wantFallbackFlag: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{ServerConfig: invalidEndpointServerConfig(test.allowFallback)}
			server.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			server.initStorage()
			t.Cleanup(server.Dispose)

			beforeBackend, beforeStatus := server.loadStorageSnapshot()
			if beforeBackend == nil || beforeBackend.GetKey() != test.wantActive {
				t.Fatalf("initial active type=%v, want %s", beforeBackend, test.wantActive)
			}
			if beforeStatus.DesiredType != "s3" || !beforeStatus.Degraded || beforeStatus.FallbackActive != test.wantFallbackFlag {
				t.Fatalf("initial status=%+v", beforeStatus)
			}
			if server.storageReconnectWork == nil {
				t.Fatal("invalid S3 endpoint must create reconnect work before permanent classification")
			}
			response := httptest.NewRecorder()
			server.GetStorageStatusHTTP(response, httptest.NewRequest(http.MethodGet, "/api/storage/status", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("initial readiness=%d, want 503", response.Code)
			}

			current := time.Unix(4_000, 0)
			reconnect := newStorageReconnectTask(server)
			reconnect.Logger = server.Logger
			reconnect.now = func() time.Time { return current }
			reconnect.nextAttempt = current
			reconnect.Tick(nil)

			if !reconnect.completed {
				t.Fatal("invalid S3 endpoint must permanently complete reconnect")
			}
			if !reconnect.nextAttempt.IsZero() {
				t.Fatalf("invalid S3 endpoint scheduled another attempt at %v", reconnect.nextAttempt)
			}
			afterBackend, afterStatus := server.loadStorageSnapshot()
			if afterBackend != beforeBackend {
				t.Fatal("permanent endpoint error changed the active backend")
			}
			if afterStatus.ActiveType != beforeStatus.ActiveType || !afterStatus.Degraded || afterStatus.FallbackActive != beforeStatus.FallbackActive {
				t.Fatalf("after status=%+v, before=%+v", afterStatus, beforeStatus)
			}
			response = httptest.NewRecorder()
			server.GetStorageStatusHTTP(response, httptest.NewRequest(http.MethodGet, "/api/storage/status", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("readiness after permanent classification=%d, want 503", response.Code)
			}
		})
	}
}
