package plugin_logrotate

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alchemy/rotoslog"

	"m7s.live/v5/pkg/config"
)

// Expected log rotation defaults. These values are security/forensics relevant:
// the old 1 MB / 1 day / 7 files defaults discarded crash evidence too quickly.
const (
	wantDefaultSize     uint64 = 20971520 // 20 MB
	wantDefaultDays            = 30       // 30 days
	wantDefaultMaxFiles uint64 = 750      // disk guard
)

func TestLogRotateDefaultTags(t *testing.T) {
	typ := reflect.TypeOf(LogRotatePlugin{})
	cases := []struct {
		field string
		want  string
	}{
		{"Size", "20971520"},
		{"Days", "30"},
		{"MaxFiles", "750"},
		{"Formatter", "2006-01-02T15-04"},
		{"FilePrefix", "m7s-"},
	}
	for _, c := range cases {
		f, ok := typ.FieldByName(c.field)
		if !ok {
			t.Fatalf("field %s not found on LogRotatePlugin", c.field)
		}
		if got := f.Tag.Get("default"); got != c.want {
			t.Errorf("%s default tag = %q, want %q", c.field, got, c.want)
		}
	}
}

// TestLogRotateParsedDefaults verifies that the repo's config parser actually
// materialises the tags into the plugin struct (not just that the text exists).
func TestLogRotateParsedDefaults(t *testing.T) {
	var plugin LogRotatePlugin
	var conf config.Config
	conf.Parse(&plugin)

	if plugin.Size != wantDefaultSize {
		t.Errorf("parsed Size = %d, want %d", plugin.Size, wantDefaultSize)
	}
	if plugin.Days != wantDefaultDays {
		t.Errorf("parsed Days = %d, want %d", plugin.Days, wantDefaultDays)
	}
	if plugin.MaxFiles != wantDefaultMaxFiles {
		t.Errorf("parsed MaxFiles = %d, want %d", plugin.MaxFiles, wantDefaultMaxFiles)
	}
	if plugin.Formatter != defaultFormatter {
		t.Errorf("parsed Formatter = %q, want %q", plugin.Formatter, defaultFormatter)
	}
	if plugin.FilePrefix != defaultFilePrefix {
		t.Errorf("parsed FilePrefix = %q, want %q", plugin.FilePrefix, defaultFilePrefix)
	}
}

// TestDefaultFormatterIsMinuteLevel pins the controller ruling that the default
// formatter includes minutes. Rotoslog names a rotated file only by the
// formatted timestamp and uses os.Rename, so an hour-only layout makes the
// second rotation within the same hour overwrite the first file silently.
func TestDefaultFormatterIsMinuteLevel(t *testing.T) {
	if defaultFormatter != "2006-01-02T15-04" {
		t.Fatalf("defaultFormatter = %q, want minute-level %q", defaultFormatter, "2006-01-02T15-04")
	}
	const stamp = "2026-10-22T20-37"
	parsed, err := time.Parse(defaultFormatter, stamp)
	if err != nil {
		t.Fatalf("minute-level stamp %q does not parse with defaultFormatter: %v", stamp, err)
	}
	if parsed.Minute() != 37 {
		t.Errorf("parsed minute = %d, want 37", parsed.Minute())
	}
	// An hour-only name must not parse: that is what distinguishes minute-level
	// rotations from hour-level ones during pruning.
	if _, err := time.Parse(defaultFormatter, "2026-10-22T20"); err == nil {
		t.Error("hour-only stamp unexpectedly parsed with minute-level formatter")
	}
}

// TestRotoslogSizeRotation pins the behaviour rotoslog actually provides for the
// configured minute layout + size cap: it rotates on size and names the rotated
// file using the date layout. The threshold is checked *before* a write, so a
// rotated file may exceed the cap by at most one record.
// NOTE: within the same formatted minute a second rotation renames onto the same
// file name and overwrites the previous rotated file (see task-10 report). This
// test intentionally exercises only a single rotation.
func TestRotoslogSizeRotation(t *testing.T) {
	dir := t.TempDir()
	const maxSize = 1 << 20 // 1 MB
	const recordSize = 4096

	h, err := rotoslog.NewHandler(
		rotoslog.LogHandlerBuilder(rotoslog.NewTextHandler),
		rotoslog.LogDir(dir),
		rotoslog.FilePrefix(defaultFilePrefix),
		rotoslog.MaxFileSize(maxSize),
		rotoslog.DateTimeLayout(defaultFormatter),
		rotoslog.MaxRotatedFiles(750),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	logger := slog.New(h)
	payload := make([]byte, recordSize)
	for i := range payload {
		payload[i] = 'x'
	}
	// ~1.5 MB: exactly one rotation at the 1 MB cap.
	for i := 0; i < 384; i++ {
		logger.Info(string(payload))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var current, rotated []string
	rotatedRe := regexp.MustCompile(`^m7s-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}\.log$`)
	for _, e := range entries {
		switch {
		case e.Name() == "m7s-current.log":
			current = append(current, e.Name())
		case rotatedRe.MatchString(e.Name()):
			rotated = append(rotated, e.Name())
		default:
			t.Errorf("unexpected log file name %q", e.Name())
		}
	}
	if len(current) != 1 {
		t.Fatalf("expected exactly one m7s-current.log, got %v", current)
	}
	if len(rotated) != 1 {
		t.Fatalf("expected exactly one rotated file for a single rotation, got %v", rotated)
	}

	for _, name := range append(current, rotated...) {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("Stat %s: %v", name, err)
		}
		// rotoslog checks the size before writing, so the file can overshoot
		// the cap by at most one record.
		if info.Size() > maxSize+recordSize {
			t.Errorf("%s size = %d, exceeds cap %d + one record %d", name, info.Size(), maxSize, recordSize)
		}
	}
}

// TestRotoslogSameMinuteRotationOverwrites is a characterisation test for a
// known rotoslog v0.2.2 limitation: the rotated file name is formatted with the
// configured layout, so a second rotation within the same minute renames the
// current file onto the same path and overwrites the previous rotated file.
// The minute-level default narrows (but does not eliminate) the window; a
// second-order fix would require unquoted sequence numbers that rotoslog does
// not support.
// This test is expected to fail (turn red) if upstream fixes the collision;
// when that happens the restriction should be lifted and the report updated.
func TestRotoslogSameMinuteRotationOverwrites(t *testing.T) {
	dir := t.TempDir()
	const maxSize = 1 << 20 // 1 MB
	const recordSize = 8192

	var written int64
	h, err := rotoslog.NewHandler(
		rotoslog.LogHandlerBuilder(func(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
			return slog.NewTextHandler(countingWriter{w: w, n: &written}, opts)
		}),
		rotoslog.LogDir(dir),
		rotoslog.FilePrefix(defaultFilePrefix),
		rotoslog.MaxFileSize(maxSize),
		rotoslog.DateTimeLayout(defaultFormatter),
		rotoslog.MaxRotatedFiles(750),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	minuteBefore := time.Now().Format(defaultFormatter)
	logger := slog.New(h)
	payload := strings.Repeat("x", recordSize)
	// ~2.5 MB with a 1 MB cap: two rotations.
	for i := 0; i < 320; i++ {
		logger.Info(payload)
	}
	if minuteAfter := time.Now().Format(defaultFormatter); minuteAfter != minuteBefore {
		t.Skip("crossed a minute boundary during the test; rotation names are expected to differ")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var rotated int
	var onDisk int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("Info %s: %v", e.Name(), err)
		}
		onDisk += info.Size()
		if e.Name() != "m7s-current.log" {
			rotated++
		}
	}
	if rotated != 1 {
		t.Fatalf("expected the two same-minute rotations to collapse into 1 file, got %d files in %v", rotated, entries)
	}
	if onDisk >= written {
		t.Fatalf("expected one rotated file to be overwritten (on disk %d >= formatter output %d); behaviour may have been fixed upstream", onDisk, written)
	}
}

// TestPruneOldLogsScope covers the retention rule end to end: only expired
// files that carry this plugin's prefix and match the formatter are removed.
// The current file, a fresh rotated file, unrelated files in the same directory
// and a directory shaped like a rotated log are all left alone. A second run
// must be a no-op (idempotent).
func TestPruneOldLogsScope(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	old := now.Add(-31 * 24 * time.Hour)
	fresh := now.Add(-1 * time.Hour)

	oldName := defaultFilePrefix + old.Format(defaultFormatter) + logFileExt
	freshName := defaultFilePrefix + fresh.Format(defaultFormatter) + logFileExt
	currentName := defaultFilePrefix + currentFileSuffix + logFileExt

	writeFile := func(name string, modTime time.Time) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("log line\n"), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
		if err := os.Chtimes(path, modTime, modTime); err != nil {
			t.Fatalf("Chtimes %s: %v", name, err)
		}
	}
	writeFile(oldName, old)
	writeFile(freshName, fresh)
	writeFile(currentName, old) // 即使 current 很旧也永不删除
	writeFile("other-app.log", old)
	writeFile("other-app-2020-01-01T00-00.log", old)
	writeFile(defaultFilePrefix+"not-a-timestamp.log", old)
	if err := os.Mkdir(filepath.Join(dir, defaultFilePrefix+"2020-01-01T00-00"+logFileExt), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	removed, err := pruneOldLogs(dir, defaultFilePrefix, defaultFormatter, 30, now)
	if err != nil {
		t.Fatalf("pruneOldLogs: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	wantKept := []string{freshName, currentName, "other-app.log", "other-app-2020-01-01T00-00.log", defaultFilePrefix + "not-a-timestamp.log"}
	for _, name := range wantKept {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("expected %s to be kept: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, oldName)); !os.IsNotExist(err) {
		t.Errorf("expected expired file %s to be removed, stat err = %v", oldName, err)
	}
	if _, err := os.Stat(filepath.Join(dir, defaultFilePrefix+"2020-01-01T00-00"+logFileExt)); err != nil {
		t.Errorf("expected directory with rotated-log name to be kept: %v", err)
	}

	// 幂等:再跑一次不删除任何东西。
	removed, err = pruneOldLogs(dir, defaultFilePrefix, defaultFormatter, 30, now)
	if err != nil {
		t.Fatalf("second pruneOldLogs: %v", err)
	}
	if removed != 0 {
		t.Errorf("second run removed = %d, want 0", removed)
	}
}

// TestPruneOldLogsDisabled pins the Days <= 0 semantics: retention pruning is
// disabled and nothing is deleted, regardless of file age.
func TestPruneOldLogsDisabled(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	oldName := defaultFilePrefix + now.Add(-365*24*time.Hour).Format(defaultFormatter) + logFileExt
	path := filepath.Join(dir, oldName)
	if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	for _, days := range []int{0, -1} {
		removed, err := pruneOldLogs(dir, defaultFilePrefix, defaultFormatter, days, now)
		if err != nil {
			t.Fatalf("pruneOldLogs(days=%d): %v", days, err)
		}
		if removed != 0 {
			t.Errorf("days=%d removed = %d, want 0", days, removed)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("days=%d: file should be kept: %v", days, err)
		}
	}
}

// TestPruneOldLogsMissingDir treats a not-yet-created log directory as a no-op.
func TestPruneOldLogsMissingDir(t *testing.T) {
	removed, err := pruneOldLogs(filepath.Join(t.TempDir(), "absent"), defaultFilePrefix, defaultFormatter, 30, time.Now())
	if err != nil {
		t.Fatalf("pruneOldLogs: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

type countingWriter struct {
	w io.Writer
	n *int64
}

func (c countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	*c.n += int64(n)
	return n, err
}
