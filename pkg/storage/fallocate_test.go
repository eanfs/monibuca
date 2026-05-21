//go:build linux

package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInsertRange 验证 fallocate INSERT_RANGE / COLLAPSE_RANGE 的行为：
// 在文件头插入一个块后原内容整体后移、新空间可写；COLLAPSE 后恢复原状。
// t.TempDir() 所在文件系统不支持时跳过。
func TestInsertRange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "insert.bin")

	// 原始内容：一个块（4096 字节）的 'A'
	orig := bytes.Repeat([]byte{'A'}, 4096)
	if err := os.WriteFile(p, orig, 0644); err != nil {
		t.Fatalf("write orig: %v", err)
	}
	f, err := os.OpenFile(p, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	// 在文件头插入一个块
	if err := InsertRange(f, 0, 4096); err != nil {
		if errors.Is(err, ErrRangeInsertUnsupported) {
			t.Skipf("文件系统不支持 INSERT_RANGE: %v", err)
		}
		t.Fatalf("InsertRange: %v", err)
	}

	// 文件应增长一个块
	if st, _ := f.Stat(); st.Size() != 8192 {
		t.Fatalf("size after insert = %d, want 8192", st.Size())
	}
	// 原内容应整体后移到 [4096, 8192)
	moved := make([]byte, 4096)
	if _, err := f.ReadAt(moved, 4096); err != nil {
		t.Fatalf("read moved content: %v", err)
	}
	if !bytes.Equal(moved, orig) {
		t.Fatal("original content not preserved after insert")
	}
	// 新空间 [0, 4096) 可写入并回读
	hdr := bytes.Repeat([]byte{'H'}, 4096)
	if _, err := f.WriteAt(hdr, 0); err != nil {
		t.Fatalf("write header into hole: %v", err)
	}
	got := make([]byte, 4096)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("read header back: %v", err)
	}
	if !bytes.Equal(got, hdr) {
		t.Fatal("header written into inserted range not readable back")
	}

	// COLLAPSE_RANGE 撤销插入，文件回到原状
	if err := CollapseRange(f, 0, 4096); err != nil {
		t.Fatalf("CollapseRange: %v", err)
	}
	if st, _ := f.Stat(); st.Size() != 4096 {
		t.Fatalf("size after collapse = %d, want 4096", st.Size())
	}
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatalf("read after collapse: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("content not restored after collapse")
	}
}
