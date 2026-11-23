//go:build s3

package storage

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"m7s.live/v5/pkg/config"
)

func newTestS3Session(t *testing.T) *session.Session {
	t.Helper()
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Credentials: credentials.NewStaticCredentials("test", "test", ""),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return sess
}

func TestNewS3UploaderAppliesConfig(t *testing.T) {
	sess := newTestS3Session(t)

	u := newS3Uploader(sess, &S3StorageConfig{PartSize: 32 << 20, Concurrency: 2})
	if u.PartSize != 32<<20 {
		t.Fatalf("PartSize = %d, want %d", u.PartSize, 32<<20)
	}
	if u.Concurrency != 2 {
		t.Fatalf("Concurrency = %d, want 2", u.Concurrency)
	}

	// Zero keeps the SDK default (5): the runtime default 2 comes from the
	// `default:"2"` tag, applied by the config system, not by a struct literal.
	u = newS3Uploader(sess, &S3StorageConfig{})
	if u.Concurrency != 5 {
		t.Fatalf("default Concurrency = %d, want 5", u.Concurrency)
	}

	// Negative is clamped to 1: the SDK panics on a negative concurrency
	// (zero is reset to the SDK default).
	u = newS3Uploader(sess, &S3StorageConfig{Concurrency: -3})
	if u.Concurrency != 1 {
		t.Fatalf("clamped Concurrency = %d, want 1", u.Concurrency)
	}
}

// TestNewS3StorageWiresUploaderConcurrency pins the production wiring:
// NewS3Storage must build its uploader through newS3Uploader. Reverting it to an
// inline s3manager.NewUploader would leave TestNewS3UploaderAppliesConfig green
// while the configured 2-way concurrency silently stops being applied.
func TestNewS3StorageWiresUploaderConcurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead { // HeadBucket from testS3Connection
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()

	st, err := NewS3Storage(&S3StorageConfig{
		Endpoint:        srv.URL,
		Region:          "us-east-1",
		AccessKeyID:     "test",
		SecretAccessKey: "test",
		Bucket:          "test",
		ForcePathStyle:  true,
		UseSSL:          false,
		TempDir:         t.TempDir(),
		Concurrency:     2,
	})
	if err != nil {
		t.Fatalf("NewS3Storage: %v", err)
	}
	if st.uploader == nil {
		t.Fatal("uploader is nil")
	}
	if st.uploader.Concurrency != 2 {
		t.Fatalf("uploader Concurrency = %d, want 2: NewS3Storage is not using newS3Uploader", st.uploader.Concurrency)
	}
}

// TestS3ConcurrencyConfigDefault pins the runtime default: 2 comes from the
// `default:"2"` struct tag applied by the config system, and an explicit
// `concurrency` key overrides it.
func TestS3ConcurrencyConfigDefault(t *testing.T) {
	var cfg S3StorageConfig
	config.Parse(&cfg, map[string]any{})
	if cfg.Concurrency != 2 {
		t.Fatalf("default Concurrency = %d, want 2 from the default tag", cfg.Concurrency)
	}
	config.Parse(&cfg, map[string]any{"concurrency": 7})
	if cfg.Concurrency != 7 {
		t.Fatalf("concurrency key = %d, want 7", cfg.Concurrency)
	}
}
