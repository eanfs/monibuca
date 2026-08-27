package storage

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsPermanentConnectionError(t *testing.T) {
	tests := []struct {
		message   string
		permanent bool
	}{
		{message: "AccessDenied", permanent: true},
		{message: "Forbidden", permanent: true},
		{message: "InvalidAccessKeyId", permanent: true},
		{message: "SignatureDoesNotMatch", permanent: true},
		{message: "InvalidBucketName", permanent: true},
		{message: "MalformedXML", permanent: true},
		{message: "InvalidObjectName", permanent: true},
		{message: "connection refused", permanent: false},
		{message: "i/o timeout", permanent: false},
		{message: "NoSuchBucket", permanent: false},
	}
	for _, test := range tests {
		t.Run(test.message, func(t *testing.T) {
			if got := IsPermanentConnectionError(errors.New(test.message)); got != test.permanent {
				t.Errorf("IsPermanentConnectionError(%q) = %v, want %v", test.message, got, test.permanent)
			}
		})
	}
}

func TestIsPermanentConnectionErrorHandlesNil(t *testing.T) {
	if IsPermanentConnectionError(nil) {
		t.Fatal("nil error must remain retryable")
	}
}

func TestNoSuchBucketRemainsPermanentForUpload(t *testing.T) {
	if !IsPermanentError(errors.New("NoSuchBucket")) {
		t.Fatal("NoSuchBucket must retain the existing permanent upload error semantics")
	}
	if IsPermanentConnectionError(errors.New("NoSuchBucket")) {
		t.Fatal("NoSuchBucket must remain retryable during S3 connection initialization")
	}
}

type testConnectionCodeError struct{ code string }

func (e testConnectionCodeError) Error() string { return "opaque storage connection error" }
func (e testConnectionCodeError) Code() string  { return e.code }

func TestIsPermanentConnectionErrorUsesSentinelAndCodes(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		permanent bool
	}{
		{name: "invalid config sentinel", err: fmt.Errorf("wrapped: %w", ErrInvalidStorageConfig), permanent: true},
		{name: "access denied code", err: fmt.Errorf("wrapped: %w", testConnectionCodeError{code: "AccessDenied"}), permanent: true},
		{name: "signature code", err: testConnectionCodeError{code: "SignatureDoesNotMatch"}, permanent: true},
		{name: "missing bucket remains transient", err: testConnectionCodeError{code: "NoSuchBucket"}, permanent: false},
		{name: "unknown code remains transient", err: testConnectionCodeError{code: "RequestTimeout"}, permanent: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsPermanentConnectionError(test.err); got != test.permanent {
				t.Fatalf("IsPermanentConnectionError()=%v, want %v", got, test.permanent)
			}
		})
	}
}
