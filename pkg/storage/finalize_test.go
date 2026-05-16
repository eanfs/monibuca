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

func TestLocalFileFinalizeFromTemp_SameDir(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "dest.mp4")

	// 模拟 LocalStorage.CreateFile：以 O_RDWR 打开目标文件
	destFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	lf := &LocalFile{destFile}

	srcPath := writeTempWithContent(t, dir, "rewritten.tmp", "MOOV-FIRST-MP4")
	if err := lf.FinalizeFromTemp(srcPath); err != nil {
		t.Fatalf("FinalizeFromTemp: %v", err)
	}
	// 调用方随后会 Close
	if err := lf.Close(); err != nil {
		t.Fatalf("Close after finalize: %v", err)
	}

	// src 应已移走
	if _, statErr := os.Stat(srcPath); !os.IsNotExist(statErr) {
		t.Fatalf("src should be moved away, stat err = %v", statErr)
	}
	// dest 应含 src 内容
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "MOOV-FIRST-MP4" {
		t.Fatalf("dest content = %q, want %q", string(got), "MOOV-FIRST-MP4")
	}
}

// 验证 LocalFile 实现了 TempFileFinalizer 接口（编译期断言）
var _ TempFileFinalizer = (*LocalFile)(nil)
