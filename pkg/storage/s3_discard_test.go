//go:build s3

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 回归（issue #41 / 复审 C1）：Discard 绝不上传。
//
// 对象存储的 File.Close() 会把内容传上去，正好与「丢弃」相反；Discard 必须
// 只关句柄、删本地暂存文件。若有人把它改成 Close，待丢弃的碎片会被传进对象
// 存储——这个用例会红。
func TestS3FileDiscardDeletesTempWithoutUpload(t *testing.T) {
	dir := t.TempDir()
	w := &S3File{storage: &S3Storage{config: &S3StorageConfig{TempDir: dir}}}
	if err := w.createTempFile(); err != nil {
		t.Fatalf("createTempFile: %v", err)
	}
	path := w.filePath
	if _, err := w.Write([]byte("discard me")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file must be removed, stat err = %v", err)
	}
	if w.tempFile != nil || w.filePath != "" || w.bw != nil {
		t.Fatalf("Discard must clear handles: tempFile=%v filePath=%q bw=%v",
			w.tempFile, w.filePath, w.bw != nil)
	}
	// 幂等：丢弃路径不保证只走一次。
	if err := w.Discard(); err != nil {
		t.Fatalf("second Discard: %v", err)
	}
}

// 复审 C1：删除失败必须返回错误。此前三个对象存储后端的 Discard 无条件
// return nil，调用方的告警分支实际上是死代码，被丢弃片段的暂存文件残留
// 没有任何痕迹（随后会被恢复扫描当崩溃残留重新上传）。
func TestS3FileDiscardReportsRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	// 目标是一个非空目录：os.Remove 必然失败（directory not empty）。
	if err := os.WriteFile(filepath.Join(dir, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write child: %v", err)
	}
	w := &S3File{storage: &S3Storage{config: &S3StorageConfig{}}, filePath: dir}
	if err := w.Discard(); err == nil {
		t.Fatal("Discard must surface a removal failure instead of returning nil")
	}
}
