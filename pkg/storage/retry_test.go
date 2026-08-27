package storage

import (
	"errors"
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
