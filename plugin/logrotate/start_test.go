package plugin_logrotate

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"m7s.live/v5"
)

// newStartTestPlugin 构造一个可在测试中直接 Start 的插件：Days<=0 时 Start
// 不会注册清理任务，因此不需要启动任务系统；Server 只需非空以挂载 handler。
func newStartTestPlugin(t *testing.T) (*LogRotatePlugin, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	p := &LogRotatePlugin{Path: t.TempDir()}
	p.Server = &m7s.Server{}
	p.Logger = slog.New(slog.NewTextHandler(logs, nil))
	return p, logs
}

// Size/MaxFiles 的零值不能透传给 rotoslog：MaxFileSize(0) 会让每次写入都触发
// 轮转（written > 0），MaxRotatedFiles(0) 会在轮转后立刻删掉刚生成的文件。
// 必须回退到 default tag 的值并各记一条 Warn。
func TestStartClampsZeroRotateLimits(t *testing.T) {
	p, logs := newStartTestPlugin(t)
	p.Formatter = defaultFormatter
	p.FilePrefix = defaultFilePrefix
	p.Size = 0
	p.MaxFiles = 0
	p.Days = 0

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "log size is zero") {
		t.Errorf("expected a warning about zero Size; logs = %q", out)
	}
	if !strings.Contains(out, "max files is zero") {
		t.Errorf("expected a warning about zero MaxFiles; logs = %q", out)
	}
	if want := strconv.FormatUint(defaultRotateSize, 10); !strings.Contains(out, want) {
		t.Errorf("Size warning must name the effective default %s; logs = %q", want, out)
	}
	if want := strconv.FormatUint(defaultRotateMaxFiles, 10); !strings.Contains(out, want) {
		t.Errorf("MaxFiles warning must name the effective default %s; logs = %q", want, out)
	}
}

// 显式配置的值不得被夹紧，也不应产生对应 Warn。
func TestResolveRotateParamsKeepsConfiguredValues(t *testing.T) {
	p, logs := newStartTestPlugin(t)
	p.Formatter = defaultFormatter
	p.Size = 1234
	p.MaxFiles = 7

	size, maxFiles, err := p.resolveRotateParams()
	if err != nil {
		t.Fatalf("resolveRotateParams: %v", err)
	}
	if size != 1234 || maxFiles != 7 {
		t.Errorf("resolved = (%d, %d), want (1234, 7)", size, maxFiles)
	}
	if out := logs.String(); strings.Contains(out, "zero") {
		t.Errorf("configured values must not warn about zero values; logs = %q", out)
	}
}

// Formatter 为空时无法命名轮转文件，也无法界定清理范围：必须 fail fast，
// 不能静默回退到任何默认值（否则运维改错配置后日志命名会悄然变化）。
func TestStartRejectsEmptyFormatter(t *testing.T) {
	p, _ := newStartTestPlugin(t)
	p.Formatter = ""
	p.Size = 1024
	p.MaxFiles = 3
	p.Days = 0

	err := p.Start()
	if err == nil {
		t.Fatal("empty Formatter must make Start fail")
	}
	if !strings.Contains(err.Error(), "formatter") {
		t.Errorf("error = %v, want it to mention formatter", err)
	}
}

// 夹紧值来自常量，配置默认值来自 struct tag；两者必须一致，否则“零值回退”
// 得到的结果会和未配置时的默认行为不同。
func TestClampDefaultsMatchTags(t *testing.T) {
	if got := uint64(wantDefaultSize); defaultRotateSize != got {
		t.Errorf("defaultRotateSize = %d, want %d (struct tag default)", defaultRotateSize, got)
	}
	if got := uint64(wantDefaultMaxFiles); defaultRotateMaxFiles != got {
		t.Errorf("defaultRotateMaxFiles = %d, want %d (struct tag default)", defaultRotateMaxFiles, got)
	}
}

// Start 必须一次性列出（绝不删除）上一份 fileprefix/formatter 配置留下的日志：
// 名称不带当前前缀、但时间戳能被当前 Formatter 解析的 *.log。样本有条数上限，
// 但 total 必须是完整数量。这些文件不在本插件清理范围内，只能人工处理。
func TestStartWarnsAboutLogsFromPreviousConfig(t *testing.T) {
	p, logs := newStartTestPlugin(t)
	p.Formatter = defaultFormatter
	p.FilePrefix = defaultFilePrefix
	p.Size = 1024
	p.MaxFiles = 3
	p.Days = 0

	writeLog := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(p.Path, name), []byte("line\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
	}
	// 当前前缀的文件：属于本插件，不在提示范围。
	writeLog(defaultFilePrefix + "2026-10-22T20-37" + logFileExt)
	writeLog(defaultFilePrefix + currentFileSuffix + logFileExt)
	// 非日志文件 / 时间戳不可解析：不能确定是旧配置留下的日志。
	writeLog("other-app.log")
	writeLog("2026-10-22" + logFileExt)
	// 12 个旧配置遗留文件（无当前前缀，但时间戳可解析）。
	var stale []string
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("2026-10-22T20-%02d%s", i, logFileExt)
		stale = append(stale, name)
		writeLog(name)
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	out := logs.String()
	if !strings.Contains(out, "do not match the current fileprefix/formatter") {
		t.Fatalf("expected a single warning about stale logs; logs = %q", out)
	}
	if !strings.Contains(out, "total=12") {
		t.Errorf("warning must report the full count total=12; logs = %q", out)
	}
	// 样本有上限：完整列表会远超告警可读性。
	if !strings.Contains(out, stale[0]) {
		t.Errorf("warning sample must include the first stale file %s; logs = %q", stale[0], out)
	}
	if strings.Contains(out, stale[len(stale)-1]) {
		t.Errorf("warning sample must be bounded (%d entries), but includes %s; logs = %q",
			staleLogSampleLimit, stale[len(stale)-1], out)
	}
	staleNames, err := foreignLogFiles(p.Path, p.FilePrefix, p.Formatter)
	if err != nil {
		t.Fatalf("foreignLogFiles: %v", err)
	}
	if got := len(staleNames); got != len(stale) {
		t.Errorf("foreignLogFiles count = %d, want %d", got, len(stale))
	}
	// 提示只能是 Warn：删掉别人的文件（或旧的取证日志）不可接受。
	for _, name := range stale {
		if _, err := os.Stat(filepath.Join(p.Path, name)); err != nil {
			t.Fatalf("stale log %s must never be deleted: %v", name, err)
		}
	}
}

// FilePrefix 为空时无法界定“旧配置遗留”与“其它进程日志”，扫描必须整体跳过：
// HasPrefix(name, "") 恒真，任何提示都会把别人的日志当成我们的。
func TestStartSkipsStaleScanWhenPrefixEmpty(t *testing.T) {
	p, logs := newStartTestPlugin(t)
	p.Formatter = defaultFormatter
	p.FilePrefix = ""
	p.Size = 1024
	p.MaxFiles = 3
	p.Days = 0
	if err := os.WriteFile(filepath.Join(p.Path, "2026-10-22T20-37"+logFileExt), []byte("line\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if out := logs.String(); strings.Contains(out, "do not match the current fileprefix/formatter") {
		t.Errorf("empty FilePrefix must skip the stale-log scan; logs = %q", out)
	}
}

func TestForeignLogFilesMissingDir(t *testing.T) {
	got, err := foreignLogFiles(filepath.Join(t.TempDir(), "absent"), defaultFilePrefix, defaultFormatter)
	if err != nil {
		t.Fatalf("foreignLogFiles: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("foreignLogFiles = %v, want none for a missing dir", got)
	}
}
