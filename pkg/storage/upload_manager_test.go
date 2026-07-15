package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestGetPendingDirUsage 验证 pending 目录的字节数 / 文件数统计。
func TestGetPendingDirUsage(t *testing.T) {
	dir := t.TempDir()
	InitUploadManager(UploadConfig{PendingDir: dir})

	for name, sz := range map[string]int{"a.mp4": 100, "b.mp4": 200, "c.mp4": 300} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, sz), 0644); err != nil {
			t.Fatalf("write seed file: %v", err)
		}
	}

	total, count, err := GetPendingDirUsage()
	if err != nil {
		t.Fatalf("GetPendingDirUsage: %v", err)
	}
	if count != 3 {
		t.Errorf("文件数期望 3，实际 %d", count)
	}
	if total != 600 {
		t.Errorf("总字节数期望 600，实际 %d", total)
	}
}

// TestMoveToPendingDir_FileLimit 验证 PendingMaxFiles 水位：
// 暂存满 2 个后，第 3 个被 ErrPendingDirFull 拒绝。
func TestMoveToPendingDir_FileLimit(t *testing.T) {
	srcDir := t.TempDir()
	pendingDir := t.TempDir()
	InitUploadManager(UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 2})

	mkSrc := func(name string) string {
		p := filepath.Join(srcDir, name)
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("write src: %v", err)
		}
		return p
	}

	if _, err := MoveToPendingDir(mkSrc("a.mp4")); err != nil {
		t.Fatalf("第 1 个应成功: %v", err)
	}
	if _, err := MoveToPendingDir(mkSrc("b.mp4")); err != nil {
		t.Fatalf("第 2 个应成功: %v", err)
	}
	if _, err := MoveToPendingDir(mkSrc("c.mp4")); !errors.Is(err, ErrPendingDirFull) {
		t.Fatalf("第 3 个应返回 ErrPendingDirFull，实际 %v", err)
	}
}

// TestMoveToPendingDir_NoLimit 验证未配置水位（默认 0）时行为不变。
func TestMoveToPendingDir_NoLimit(t *testing.T) {
	srcDir := t.TempDir()
	pendingDir := t.TempDir()
	InitUploadManager(UploadConfig{PendingDir: pendingDir})

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		p := filepath.Join(srcDir, name+".mp4")
		os.WriteFile(p, []byte("x"), 0644)
		if _, err := MoveToPendingDir(p); err != nil {
			t.Fatalf("未配水位时 %s 应成功: %v", name, err)
		}
	}
}

// TestGetDiskFreeBytes 验证磁盘剩余空间查询：Unix 返回 >0，Windows 降级返回 err。
func TestGetDiskFreeBytes(t *testing.T) {
	free, err := GetDiskFreeBytes(t.TempDir())
	if err == nil && free == 0 {
		t.Error("Unix 下磁盘可用空间应 > 0")
	}
}

// TestMoveToPendingDir_NameCollisionUnique 守护同名冲突覆盖(review #16):
// 多个同 basename 的文件先后进 pending,冲突后缀必须唯一,
// 绝不能 rename 覆盖仍在等待补传的旧文件。
func TestMoveToPendingDir_NameCollisionUnique(t *testing.T) {
	srcDir := t.TempDir()
	pendingDir := t.TempDir()
	InitUploadManager(UploadConfig{PendingDir: pendingDir})

	dsts := make(map[string]struct{})
	for i := 0; i < 3; i++ {
		p := filepath.Join(srcDir, "same.mp4")
		if err := os.WriteFile(p, []byte{byte(i)}, 0644); err != nil {
			t.Fatal(err)
		}
		dst, err := MoveToPendingDir(p)
		if err != nil {
			t.Fatalf("move %d: %v", i, err)
		}
		if _, dup := dsts[dst]; dup {
			t.Fatalf("第 %d 次冲突产生重复目标路径 %s(会静默覆盖待补传文件)", i, dst)
		}
		dsts[dst] = struct{}{}
	}
	// 三个文件都必须还在 pending 目录里
	entries, err := os.ReadDir(pendingDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("pending 目录应有 3 个文件,实际 %d", len(entries))
	}
}
