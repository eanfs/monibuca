//go:build s3

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestS3CreateTempUsesConfiguredDir(t *testing.T) {
	dir := t.TempDir()
	cfg := S3StorageConfig{TempDir: dir}
	f, err := os.CreateTemp(effectiveTempDir(&cfg), "s3writer_*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if filepath.Dir(f.Name()) != dir {
		t.Fatalf("temp file dir = %s, want %s", filepath.Dir(f.Name()), dir)
	}
}

func TestEffectiveTempDirDefaultsToSystemTemp(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *S3StorageConfig
	}{
		{name: "nil config", cfg: nil},
		{name: "empty", cfg: &S3StorageConfig{}},
		{name: "whitespace", cfg: &S3StorageConfig{TempDir: "   "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveTempDir(tc.cfg); got != os.TempDir() {
				t.Fatalf("effectiveTempDir(%v) = %q, want %q", tc.cfg, got, os.TempDir())
			}
		})
	}
}
