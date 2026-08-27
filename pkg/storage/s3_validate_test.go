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

func endpointValidationConfig(endpoint string, useSSL bool) S3StorageConfig {
	return S3StorageConfig{
		Endpoint:        endpoint,
		AccessKeyID:     "unit-test-access",
		SecretAccessKey: "unit-test-secret",
		Bucket:          "unit-test-bucket",
		UseSSL:          useSSL,
	}
}

func TestS3StorageConfigValidateRejectsInvalidEndpointSafely(t *testing.T) {
	tests := []struct {
		name      string
		endpoint  string
		forbidden []string
	}{
		{
			name:      "malformed endpoint",
			endpoint:  "://malformed-endpoint",
			forbidden: []string{"malformed-endpoint"},
		},
		{
			name:      "unsupported scheme",
			endpoint:  "ftp://operator:credential@storage.invalid/path?X-Amz-Signature=synthetic-marker",
			forbidden: []string{"storage.invalid", "operator", "credential", "X-Amz-Signature", "synthetic-marker"},
		},
		{
			name:      "missing host",
			endpoint:  "https:///bucket/path",
			forbidden: []string{"bucket/path"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testConfig := endpointValidationConfig(test.endpoint, false)
			err := testConfig.Validate()
			if !errors.Is(err, ErrInvalidStorageConfig) {
				t.Fatal("Validate() must wrap ErrInvalidStorageConfig for an invalid endpoint")
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatal("Validate() leaked raw endpoint material")
				}
			}
		})
	}
}

func TestS3StorageConfigValidateAcceptsSupportedEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		useSSL   bool
	}{
		{name: "AWS default endpoint", endpoint: ""},
		{name: "MinIO host port over HTTP", endpoint: "minio.internal:9000"},
		{name: "MinIO host port over HTTPS", endpoint: "minio.internal:9000", useSSL: true},
		{name: "explicit HTTP", endpoint: "http://minio.internal:9000"},
		{name: "explicit HTTPS", endpoint: "https://minio.internal:9000"},
		{name: "uppercase explicit scheme", endpoint: "HTTP://minio.internal:9000"},
		{name: "bracketed IPv6 host port", endpoint: "[::1]:9000"},
		{name: "explicit bracketed IPv6", endpoint: "https://[::1]:9000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testConfig := endpointValidationConfig(test.endpoint, test.useSSL)
			if err := testConfig.Validate(); err != nil {
				t.Fatal("Validate() rejected a supported endpoint form")
			}
		})
	}
}

func TestNormalizeS3Endpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		useSSL   bool
		want     string
	}{
		{name: "AWS default endpoint", want: ""},
		{name: "host port HTTP", endpoint: "minio.internal:9000", want: "http://minio.internal:9000"},
		{name: "host port HTTPS", endpoint: "minio.internal:9000", useSSL: true, want: "https://minio.internal:9000"},
		{name: "uppercase scheme", endpoint: "HTTP://minio.internal:9000", want: "http://minio.internal:9000"},
		{name: "explicit HTTPS path", endpoint: "https://minio.internal:9000/s3", want: "https://minio.internal:9000/s3"},
		{name: "IPv6 host port", endpoint: "[::1]:9000", want: "http://[::1]:9000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeS3Endpoint(test.endpoint, test.useSSL)
			if err != nil {
				t.Fatal("normalizeS3Endpoint rejected a supported endpoint")
			}
			if got != test.want {
				t.Fatalf("normalized endpoint=%q, want %q", got, test.want)
			}
		})
	}
}
