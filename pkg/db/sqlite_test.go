//go:build sqlite

package db

import (
	"strings"
	"testing"
)

func TestBuildSQLiteDSN(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"纯文件路径注入 busy_timeout", "/data/m7s.db", "file:/data/m7s.db?_pragma=busy_timeout(5000)"},
		{"相对路径同样处理", "m7s.db", "file:m7s.db?_pragma=busy_timeout(5000)"},
		{"内存库保持原样", ":memory:", ":memory:"},
		{"已带参数保持原样(平台自负)", "file:/data/m7s.db?_pragma=journal_mode(WAL)", "file:/data/m7s.db?_pragma=journal_mode(WAL)"},
		{"file: 前缀保持原样", "file:/data/m7s.db", "file:/data/m7s.db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildSQLiteDSN(c.in); got != c.want {
				t.Fatalf("buildSQLiteDSN(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestBuildSQLiteDSN_NoWALNoSync 钉死回归:默认注入的 DSN 绝不含 WAL 或
// synchronous。ncruces WAL-index mmap 在 64K 页 ARM 内核上不兼容会导致
// crashloop;非 WAL 下 synchronous=NORMAL 断电可能损坏整库。详见 buildSQLiteDSN。
func TestBuildSQLiteDSN_NoWALNoSync(t *testing.T) {
	got := strings.ToLower(buildSQLiteDSN("/data/m7s.db"))
	if strings.Contains(got, "wal") {
		t.Fatalf("默认 DSN 不得含 WAL,否则 64K 页 ARM crashloop: got %q", got)
	}
	if strings.Contains(got, "synchronous") {
		t.Fatalf("默认 DSN 不得注入 synchronous,非 WAL 下 NORMAL 断电可能损坏整库: got %q", got)
	}
}
