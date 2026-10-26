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
	f, err := os.CreateTemp(func() string { d, _ := effectiveTempDir(&cfg); return d }(), "s3writer_*.tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if filepath.Dir(f.Name()) != dir {
		t.Fatalf("temp file dir = %s, want %s", filepath.Dir(f.Name()), dir)
	}
}

// TestS3CreateTempFileCreatesConfiguredDir 直接调用生产路径 createTempFile，
// 证明它使用了配置目录，且嵌套目录会被 MkdirAll 创建（t.TempDir 下本无 a/b）。
func TestS3CreateTempFileCreatesConfiguredDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	w := &S3File{storage: &S3Storage{config: &S3StorageConfig{TempDir: dir}}}

	if err := w.createTempFile(); err != nil {
		t.Fatalf("createTempFile() error = %v", err)
	}
	defer w.cleanup(true)

	if got := filepath.Dir(w.filePath); got != dir {
		t.Fatalf("temp file dir = %s, want %s", got, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("configured temp dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", dir)
	}
	if _, err := os.Stat(w.filePath); err != nil {
		t.Fatalf("temp file %s not created: %v", w.filePath, err)
	}
}

// TestS3CreateTempFileNoSilentFallback 让 tempdir 位于一个普通文件之下，
// MkdirAll 必然失败；此时必须返回错误，且不得回退到 os.TempDir()。
func TestS3CreateTempFileNoSilentFallback(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	before := listSystemTempWriters(t)
	w := &S3File{storage: &S3Storage{config: &S3StorageConfig{TempDir: filepath.Join(blocker, "sub")}}}

	if err := w.createTempFile(); err == nil {
		w.cleanup(true)
		t.Fatal("createTempFile() succeeded, want error for unreachable temp dir")
	}

	after := listSystemTempWriters(t)
	for name := range after {
		if !before[name] {
			t.Fatalf("silent fallback: new %s found in os.TempDir()", name)
		}
	}
}

// listSystemTempWriters 返回 os.TempDir() 下当前存在的 s3writer_*.tmp 集合。
func listSystemTempWriters(t *testing.T) map[string]bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "s3writer_*.tmp"))
	if err != nil {
		t.Fatalf("glob os.TempDir(): %v", err)
	}
	set := make(map[string]bool, len(matches))
	for _, m := range matches {
		set[filepath.Base(m)] = true
	}
	return set
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
			if got, _ := effectiveTempDir(tc.cfg); got != os.TempDir() {
				t.Fatalf("effectiveTempDir(%v) = %q, want %q", tc.cfg, got, os.TempDir())
			}
		})
	}
}
