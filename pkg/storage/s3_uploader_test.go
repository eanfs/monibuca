//go:build s3

package storage

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
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

	// Negative is clamped to 1: a zero/negative SDK concurrency panics or stalls.
	u = newS3Uploader(sess, &S3StorageConfig{Concurrency: -3})
	if u.Concurrency != 1 {
		t.Fatalf("clamped Concurrency = %d, want 1", u.Concurrency)
	}
}
