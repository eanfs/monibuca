//go:build s3

package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"golang.org/x/time/rate"
)

// TestS3UploadBodyIsThrottled pins that the production S3 upload wraps the
// temp-file body with the process-wide upload read limiter. A missing wrapper
// is invisible in the upload result (the object still arrives intact), so only
// token accounting can tell the two wirings apart: this test fails if
// uploadTempFile goes back to putting w.tempFile on the wire.
func TestS3UploadBodyIsThrottled(t *testing.T) {
	var mu sync.Mutex
	sizes := map[string]int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/test": // HeadBucket from testS3Connection
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut:
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Lock()
			sizes[r.URL.Path] = n
			mu.Unlock()
			w.Header().Set("ETag", `"test"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead: // post-upload size verify
			mu.Lock()
			n, ok := sizes[r.URL.Path]
			mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	// InitUploadManager installs the upload-slot semaphore (acquired inside
	// uploadTempFile) and clears uploadReadLimiter; the test installs its own
	// limiter afterwards. Cleanup resets the package globals for other tests.
	InitUploadManager(UploadConfig{PendingDir: t.TempDir()})
	t.Cleanup(func() { InitUploadManager(UploadConfig{PendingDir: t.TempDir()}) })
	// 64 KiB/s with an 8 MiB burst: the 2 MiB upload never sleeps, but the
	// consumed tokens cannot be refilled meaningfully in test time (kilobytes).
	uploadReadLimiter = rate.NewLimiter(rate.Limit(64<<10), 8<<20)

	st, err := NewS3Storage(&S3StorageConfig{
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		Bucket:          "test",
		ForcePathStyle:  true,
		UseSSL:          false,
		TempDir:         t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}

	f, err := st.CreateFile(context.Background(), "records/x.mp4")
	if err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if _, err := f.Write(make([]byte, 2<<20)); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	before := uploadReadLimiter.Tokens()
	// 2 MiB is below the uploader part size, so Close issues a single PutObject.
	if err := f.Close(); err != nil {
		t.Fatalf("Close (upload): %v", err)
	}

	mu.Lock()
	got, ok := sizes["/test/records/x.mp4"]
	mu.Unlock()
	if !ok || got != 2<<20 {
		t.Fatalf("fake S3 recorded object size %d (present=%v), want %d", got, ok, 2<<20)
	}

	// Refill is kilobytes in test time, so a drop of at least ~1.5 MiB proves
	// the 2 MiB upload body was read through the limiter.
	drop := before - uploadReadLimiter.Tokens()
	if drop < float64(1536<<10) {
		t.Fatalf("upload consumed %.0f limiter tokens, want >= %d: the S3 upload body is not throttled", drop, 1536<<10)
	}
}
