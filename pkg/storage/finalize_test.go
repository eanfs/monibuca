package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 写一个带内容的文件, 返回路径
func writeTempWithContent(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestAdoptUploadTempFile_ReplacesOldFile(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeTempWithContent(t, dir, "old.tmp", "OLD")
	srcPath := writeTempWithContent(t, dir, "src.tmp", "NEW-CONTENT")

	old, err := os.OpenFile(oldPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}

	f, err := adoptUploadTempFile(old, oldPath, srcPath)
	if err != nil {
		t.Fatalf("adoptUploadTempFile: %v", err)
	}
	defer f.Close()

	// 旧文件应被删除
	if _, statErr := os.Stat(oldPath); !os.IsNotExist(statErr) {
		t.Fatalf("old file should be removed, stat err = %v", statErr)
	}
	// 返回的句柄应指向 src 内容
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if got := string(buf[:n]); got != "NEW-CONTENT" {
		t.Fatalf("adopted file content = %q, want %q", got, "NEW-CONTENT")
	}
}

func TestAdoptUploadTempFile_NilOldHandle(t *testing.T) {
	dir := t.TempDir()
	srcPath := writeTempWithContent(t, dir, "src.tmp", "DATA")
	f, err := adoptUploadTempFile(nil, "", srcPath)
	if err != nil {
		t.Fatalf("adoptUploadTempFile with nil old: %v", err)
	}
	defer f.Close()
}

func TestAdoptUploadTempFile_SrcMissing(t *testing.T) {
	f, err := adoptUploadTempFile(nil, "", "/nonexistent/path/xyz.tmp")
	if err == nil {
		f.Close()
		t.Fatal("expected error when srcPath missing")
	}
}
