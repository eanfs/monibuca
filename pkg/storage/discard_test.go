package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 回归（issue #41）：片段被整段丢弃时，本地存储必须关闭句柄并删除目标文件，
// 且可重复调用（丢弃路径不保证只走一次）。残留文件会被恢复扫描当成崩溃残留。
func TestLocalFileDiscardRemovesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rec.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	file := &LocalFile{File: f}

	if err := file.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("media file should be removed, stat err = %v", err)
	}
	// 幂等：第二次调用不得报错（句柄已释放、文件已删除）。
	if err := file.Discard(); err != nil {
		t.Fatalf("second Discard: %v", err)
	}
}
