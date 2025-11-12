//go:build s3

package storage

import (
    "context"
    "os"
    "path/filepath"
    "testing"
)

func TestS3Storage_CreateWriteExists(t *testing.T) {
    endpoint := os.Getenv("S3_ENDPOINT")
    region := os.Getenv("S3_REGION")
    accessKey := os.Getenv("S3_ACCESS_KEY_ID")
    secretKey := os.Getenv("S3_SECRET_ACCESS_KEY")
    bucket := os.Getenv("S3_BUCKET")
    if accessKey == "" || secretKey == "" || bucket == "" {
        t.Skip("S3 credentials not provided; skipping")
    }
    useSSL := os.Getenv("S3_USE_SSL") != "false"
    forcePath := os.Getenv("S3_FORCE_PATH_STYLE") == "true"
    prefix := os.Getenv("S3_PATH_PREFIX")

    conf := &S3StorageConfig{
        Endpoint:        endpoint,
        Region:          region,
        AccessKeyID:     accessKey,
        SecretAccessKey: secretKey,
        Bucket:          bucket,
        PathPrefix:      prefix,
        UseSSL:          useSSL,
        ForcePathStyle:  forcePath,
    }

    s, err := NewS3Storage(conf)
    if err != nil {
        t.Fatalf("NewS3Storage error: %v", err)
    }
    defer s.Close()

    path := filepath.Join("monibuca_s3_test", "test.bin")
    f, err := s.CreateFile(context.Background(), path)
    if err != nil {
        t.Fatalf("CreateFile error: %v", err)
    }
    defer func() { _ = f.Close() }()

    payload := []byte("hello_s3")
    if _, err = f.Write(payload); err != nil {
        t.Fatalf("Write error: %v", err)
    }
    if err = f.Sync(); err != nil {
        t.Fatalf("Sync error: %v", err)
    }

    exists, err := s.Exists(context.Background(), path)
    if err != nil {
        t.Fatalf("Exists error: %v", err)
    }
    if !exists {
        t.Fatalf("object not exists after upload")
    }

    size, err := s.GetSize(context.Background(), path)
    if err != nil {
        t.Fatalf("GetSize error: %v", err)
    }
    if size != int64(len(payload)) {
        t.Fatalf("size mismatch: got %d want %d", size, len(payload))
    }
}
