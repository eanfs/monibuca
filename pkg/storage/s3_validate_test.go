//go:build s3

package storage

import (
	"errors"
	"strings"
	"testing"
)

func TestS3StorageConfigValidateWrapsInvalidConfigSentinel(t *testing.T) {
	tests := []struct {
		name      string
		config    S3StorageConfig
		forbidden []string
	}{
		{
			name: "missing access key",
			config: S3StorageConfig{
				SecretAccessKey: "provided-secret-value",
				Bucket:          "provided-bucket-value",
			},
			forbidden: []string{"provided-secret-value", "provided-bucket-value"},
		},
		{
			name: "missing secret key",
			config: S3StorageConfig{
				AccessKeyID: "provided-access-value",
				Bucket:      "provided-bucket-value",
			},
			forbidden: []string{"provided-access-value", "provided-bucket-value"},
		},
		{
			name: "missing bucket",
			config: S3StorageConfig{
				AccessKeyID:     "provided-access-value",
				SecretAccessKey: "provided-secret-value",
			},
			forbidden: []string{"provided-access-value", "provided-secret-value"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if !errors.Is(err, ErrInvalidStorageConfig) {
				t.Fatalf("Validate() error=%v, want ErrInvalidStorageConfig", err)
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("Validate() leaked configured value %q: %v", forbidden, err)
				}
			}
		})
	}
}
