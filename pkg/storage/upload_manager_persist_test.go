//go:build s3

package storage

import "testing"

// TestPendingDirIsPersistent 钉住"相对路径不算持久目录"的判据：
// 容器内相对路径落在进程工作目录（镜像层/临时层），重启即丢，
// 上传失败的兜底文件等于没有兜底（2026-11-09 51 现场复现）。
func TestPendingDirIsPersistent(t *testing.T) {
	cases := []struct {
		dir  string
		want bool
	}{
		{"/monibuca/pending_uploads", true},
		{"pending_uploads", false},
		{"./pending_uploads", false},
		{"", false},
	}
	for _, c := range cases {
		if got := pendingDirIsPersistent(c.dir); got != c.want {
			t.Fatalf("pendingDirIsPersistent(%q) = %v, want %v", c.dir, got, c.want)
		}
	}
}
