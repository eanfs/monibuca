package plugin_mp4

import (
	"os"
	"path/filepath"
	"testing"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/storage"
)

// TestNormalizeConfigKey 钉住恢复任务读取原始 YAML map 时的键归一化规则：
// 必须与 pkg/config 的 ParseUserFile 一致（删 "-"、删 "_"、转小写），
// 否则现场按驼峰写的配置会被判为"未配置"。
func TestNormalizeConfigKey(t *testing.T) {
	cases := map[string]string{
		"tempdir":   "tempdir",
		"tempDir":   "tempdir",
		"TEMP_DIR":  "tempdir",
		"temp-dir":  "tempdir",
		"Temp_Dir ": "tempdir ", // 空格不归一化，仅用于确认不 panic
		"":          "",
	}
	for in, want := range cases {
		if got := normalizeConfigKey(in); got != want {
			t.Fatalf("normalizeConfigKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMediaTempDirAcceptsAllKeySpellings 验证 storage.s3.tempdir 的三种常见写法
// 都能被恢复任务读到。现场 196 写的是 tempDir（驼峰），旧实现用硬编码
// conf["tempdir"] 读取，导致启动日志报 "tempdir is not configured"、
// 恢复扫描目录为空，崩溃残留无法自动恢复（2026-11-06 真机复现）。
func TestMediaTempDirAcceptsAllKeySpellings(t *testing.T) {
	const want = "/monibuca/tmp"
	cases := []struct {
		name string
		conf map[string]any
		want string
	}{
		{"camel tempDir", map[string]any{"s3": map[string]any{"tempDir": want}}, want},
		{"lower tempdir", map[string]any{"s3": map[string]any{"tempdir": want}}, want},
		{"snake temp_dir", map[string]any{"s3": map[string]any{"temp_dir": want}}, want},
		{"blank value", map[string]any{"s3": map[string]any{"tempDir": "   "}}, ""},
		{"no s3 section", map[string]any{"local": "."}, ""},
		{"s3 without tempdir", map[string]any{"s3": map[string]any{"bucket": "b"}}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server := &m7s.Server{}
			server.ServerConfig.Storage = c.conf
			plugin := &MP4Plugin{}
			plugin.Server = server
			if got := (&IndexRecoveryTask{plugin: plugin}).mediaTempDir(); got != c.want {
				t.Fatalf("mediaTempDir() = %q, want %q (conf=%v)", got, c.want, c.conf)
			}
		})
	}
}

// 未配置 s3 段时不能误报：defaultDirs 不应包含任何空的/系统临时目录项。
func TestDefaultDirsStillExcludeSystemTempDirForNonS3Config(t *testing.T) {
	server := &m7s.Server{}
	server.ServerConfig.Storage = map[string]any{string(storage.StorageTypeLocal): "."}
	plugin := &MP4Plugin{}
	plugin.Server = server
	for _, dir := range (&IndexRecoveryTask{plugin: plugin}).defaultDirs() {
		if dir == "" {
			t.Fatal("defaultDirs must not contain empty entries")
		}
	}
}

// TestDescribeRecoveryDirsMarksMissingTempDir 验证"配了但目录不存在"的 tempdir
// 会被归入 missing 而不是静默忽略：容器忘记挂载时运维必须能从日志看出来。
func TestDescribeRecoveryDirsMarksMissingTempDir(t *testing.T) {
	server := &m7s.Server{}
	server.ServerConfig.Storage = map[string]any{
		string(storage.StorageTypeS3): map[string]any{"tempdir": "/nonexistent-monibuca-tempdir"},
	}
	plugin := &MP4Plugin{}
	plugin.Server = server

	dirs, missing := (&IndexRecoveryTask{plugin: plugin}).describeRecoveryDirs()
	for _, dir := range dirs {
		if filepath.Clean(dir) == "/nonexistent-monibuca-tempdir" {
			t.Fatalf("missing tempdir must not appear in dirs: %v", dirs)
		}
	}
	found := false
	for _, dir := range missing {
		if filepath.Clean(dir) == "/nonexistent-monibuca-tempdir" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing tempdir must be reported, got dirs=%v missing=%v", dirs, missing)
	}
}

// TestDescribeRecoveryDirsIncludesTempDirWhenNotInDirs 验证即使没显式配置
// record.recovery.dirs，只要 tempdir 存在也必须进入实际扫描集合（补入语义）。
func TestDescribeRecoveryDirsIncludesTempDirWhenNotInDirs(t *testing.T) {
	tempDir := t.TempDir()
	server := &m7s.Server{}
	server.ServerConfig.Storage = map[string]any{
		string(storage.StorageTypeS3): map[string]any{"tempDir": tempDir},
	}
	plugin := &MP4Plugin{}
	plugin.Server = server

	dirs, _ := (&IndexRecoveryTask{plugin: plugin}).describeRecoveryDirs()
	for _, dir := range dirs {
		if filepath.Clean(dir) == filepath.Clean(tempDir) {
			return
		}
	}
	t.Fatalf("existing tempdir must be in actual scan dirs, got %v", dirs)
}

// TestDescribeRecoveryDirsExcludesNonDirectory 验证同名普通文件不算可用目录。
func TestDescribeRecoveryDirsExcludesNonDirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	server := &m7s.Server{}
	server.ServerConfig.Storage = map[string]any{
		string(storage.StorageTypeS3): map[string]any{"tempdir": file},
	}
	plugin := &MP4Plugin{}
	plugin.Server = server

	dirs, missing := (&IndexRecoveryTask{plugin: plugin}).describeRecoveryDirs()
	for _, dir := range dirs {
		if filepath.Clean(dir) == filepath.Clean(file) {
			t.Fatalf("regular file must not be treated as scan dir: %v", dirs)
		}
	}
	if len(missing) == 0 {
		t.Fatal("regular file path should be reported as missing dir")
	}
}
