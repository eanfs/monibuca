package m7s

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStorageStatusHTTPUsesReadinessStatusCode(t *testing.T) {
	tests := []struct {
		name     string
		status   StorageStatus
		wantCode int
	}{
		{
			name:     "ready",
			status:   StorageStatus{DesiredType: "s3", ActiveType: "s3"},
			wantCode: http.StatusOK,
		},
		{
			name: "degraded unavailable",
			status: StorageStatus{
				DesiredType: "s3",
				ActiveType:  "s3",
				Degraded:    true,
				LastError:   storageErrorSummary(errors.New("connection refused")),
			},
			wantCode: http.StatusServiceUnavailable,
		},
		{
			name: "degraded fallback",
			status: StorageStatus{
				DesiredType:    "s3",
				ActiveType:     "local",
				Degraded:       true,
				FallbackActive: true,
				LastError:      storageErrorSummary(errors.New("connection refused")),
			},
			wantCode: http.StatusServiceUnavailable,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{}
			server.activateStorage(&runtimeTestStorage{key: test.status.ActiveType}, test.status)
			response := httptest.NewRecorder()

			server.GetStorageStatusHTTP(response, httptest.NewRequest(http.MethodGet, "/api/storage/status", nil))

			if response.Code != test.wantCode {
				t.Fatalf("code=%d, want=%d, body=%s", response.Code, test.wantCode, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
				t.Errorf("Content-Type=%q, want JSON", got)
			}
			if !strings.Contains(response.Body.String(), `"lastCheckTime":`) {
				t.Errorf("body does not contain lastCheckTime: %s", response.Body.String())
			}

			var got StorageStatus
			if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.DesiredType != test.status.DesiredType || got.ActiveType != test.status.ActiveType {
				t.Errorf("storage types got=%+v, want=%+v", got, test.status)
			}
			if got.Degraded != test.status.Degraded || got.FallbackActive != test.status.FallbackActive {
				t.Errorf("readiness fields got=%+v, want=%+v", got, test.status)
			}
			if !got.LastCheckTime.Equal(test.status.LastCheckTime) {
				t.Errorf("lastCheckTime=%v, want=%v", got.LastCheckTime, test.status.LastCheckTime)
			}
			if got.LastError != test.status.LastError {
				t.Errorf("lastError=%q, want=%q", got.LastError, test.status.LastError)
			}
		})
	}
}

func TestStorageStatusHTTPRejectsNonGETMethods(t *testing.T) {
	for _, method := range []string{
		http.MethodPost,
		http.MethodPut,
		http.MethodPatch,
		http.MethodDelete,
		http.MethodHead,
		http.MethodOptions,
	} {
		t.Run(method, func(t *testing.T) {
			server := &Server{}
			response := httptest.NewRecorder()

			server.GetStorageStatusHTTP(response, httptest.NewRequest(method, "/api/storage/status", nil))

			if response.Code != http.StatusMethodNotAllowed {
				t.Fatalf("code=%d, want=%d, body=%s", response.Code, http.StatusMethodNotAllowed, response.Body.String())
			}
			if got := response.Header().Get("Allow"); got != http.MethodGet {
				t.Errorf("Allow=%q, want=%q", got, http.MethodGet)
			}
		})
	}
}

func TestStorageStatusHTTPDefensivelySanitizesRawErrors(t *testing.T) {
	const (
		secretMarker = "secret-access-key-must-not-appear"
		endpoint     = "minio.internal.example:9000"
		signature    = "X-Amz-Signature=signature-must-not-appear"
		credential   = "X-Amz-Credential=credential-must-not-appear"
	)
	rawError := "connection refused for https://" + endpoint + "/recordings?" + credential + "&" + signature + ": " + secretMarker
	status := StorageStatus{
		DesiredType: "s3",
		ActiveType:  "s3",
		Degraded:    true,
		LastError:   rawError,
	}
	server := &Server{}
	server.activateStorage(&runtimeTestStorage{key: status.ActiveType}, status)
	response := httptest.NewRecorder()

	server.GetStorageStatusHTTP(response, httptest.NewRequest(http.MethodGet, "/api/storage/status", nil))

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d, want=%d, body=%s", response.Code, http.StatusServiceUnavailable, response.Body.String())
	}
	body := response.Body.String()
	for _, forbidden := range []string{secretMarker, endpoint, signature, credential, "X-Amz-Signature", "X-Amz-Credential"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("response leaked %q: %s", forbidden, body)
		}
	}
	var got StorageStatus
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.LastError != "configured storage is temporarily unavailable" {
		t.Errorf("lastError=%q, want fixed generic summary", got.LastError)
	}
	if internal := server.GetStorageStatus(); internal.LastError != rawError {
		t.Errorf("handler modified shared snapshot: lastError=%q, want=%q", internal.LastError, rawError)
	}
}

func TestStorageStatusErrorIsSanitized(t *testing.T) {
	const secret = "secret-access-key-must-not-appear"
	unsafe := StorageStatus{DesiredType: "s3", ActiveType: "s3", Degraded: true, LastError: "connection refused: " + secret}

	got := sanitizeStorageStatusForHTTP(unsafe)

	if strings.Contains(got.LastError, secret) {
		t.Fatalf("summary leaked secret: %q", got.LastError)
	}
	if got.LastError != "configured storage is temporarily unavailable" {
		t.Errorf("lastError=%q, want fixed generic summary", got.LastError)
	}
	if unsafe.LastError != "connection refused: "+secret {
		t.Errorf("sanitizer modified input: %+v", unsafe)
	}

	safeSummary := StorageErrorSummary(errors.New("connection refused"))
	safe := StorageStatus{DesiredType: "s3", ActiveType: "local", Degraded: true, FallbackActive: true, LastError: safeSummary}
	sanitizedSafe := sanitizeStorageStatusForHTTP(safe)
	if sanitizedSafe.LastError != safeSummary {
		t.Errorf("known safe summary=%q, want=%q", sanitizedSafe.LastError, safeSummary)
	}
	if sanitizedSafe.DesiredType != safe.DesiredType || sanitizedSafe.ActiveType != safe.ActiveType || sanitizedSafe.Degraded != safe.Degraded || sanitizedSafe.FallbackActive != safe.FallbackActive || !sanitizedSafe.LastCheckTime.Equal(safe.LastCheckTime) {
		t.Errorf("sanitizer changed status fields: got=%+v, want=%+v", sanitizedSafe, safe)
	}
}
