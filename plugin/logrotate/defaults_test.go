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
	if plugin.Formatter != "2006-01-02T15" {
		t.Errorf("parsed Formatter = %q, want hourly layout %q", plugin.Formatter, "2006-01-02T15")
	}
}

// TestRotoslogHourlySizeRotation pins the behaviour rotoslog actually provides
// for the configured hourly layout + size cap: it rotates on size and names the
// rotated file using the date layout. The threshold is checked *before* a write,
// so a rotated file may exceed the cap by at most one record.
// NOTE: within the same hour a second rotation renames onto the same file name
// and overwrites the previous rotated file (see task-10 report). This test
// intentionally exercises only a single rotation.
func TestRotoslogHourlySizeRotation(t *testing.T) {
	dir := t.TempDir()
	const maxSize = 1 << 20 // 1 MB
	const recordSize = 4096

	h, err := rotoslog.NewHandler(
		rotoslog.LogHandlerBuilder(rotoslog.NewTextHandler),
		rotoslog.LogDir(dir),
		rotoslog.MaxFileSize(maxSize),
		rotoslog.DateTimeLayout("2006-01-02T15"),
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
	rotatedRe := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}\.log$`)
	for _, e := range entries {
		switch {
		case e.Name() == "current.log":
			current = append(current, e.Name())
		case rotatedRe.MatchString(e.Name()):
			rotated = append(rotated, e.Name())
		default:
			t.Errorf("unexpected log file name %q", e.Name())
		}
	}
	if len(current) != 1 {
		t.Fatalf("expected exactly one current.log, got %v", current)
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

// TestRotoslogSameHourRotationOverwrites is a characterisation test for a
// known rotoslog v0.2.2 limitation: the rotated file name is formatted with the
// hour-only layout, so a second rotation within the same hour renames the
// current file onto the same path and overwrites the previous rotated file.
// The size cap therefore does NOT bound how much log data is retained at high
// volume: at most one rotated file per hour survives.
// This test is expected to fail (turn red) if upstream fixes the collision;
// when that happens the restriction should be lifted and Task 10's report updated.
func TestRotoslogSameHourRotationOverwrites(t *testing.T) {
	dir := t.TempDir()
	const maxSize = 1 << 20 // 1 MB
	const recordSize = 8192

	var written int64
	h, err := rotoslog.NewHandler(
		rotoslog.LogHandlerBuilder(func(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
			return slog.NewTextHandler(countingWriter{w: w, n: &written}, opts)
		}),
		rotoslog.LogDir(dir),
		rotoslog.MaxFileSize(maxSize),
		rotoslog.DateTimeLayout("2006-01-02T15"),
		rotoslog.MaxRotatedFiles(750),
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	hourBefore := time.Now().Format("2006-01-02T15")
	logger := slog.New(h)
	payload := strings.Repeat("x", recordSize)
	// ~2.5 MB with a 1 MB cap: two rotations.
	for i := 0; i < 320; i++ {
		logger.Info(payload)
	}
	if hourAfter := time.Now().Format("2006-01-02T15"); hourAfter != hourBefore {
		t.Skip("crossed an hour boundary during the test; rotation names are expected to differ")
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
		if e.Name() != "current.log" {
			rotated++
		}
	}
	if rotated != 1 {
		t.Fatalf("expected the two same-hour rotations to collapse into 1 file, got %d files in %v", rotated, entries)
	}
	if onDisk >= written {
		t.Fatalf("expected one rotated file to be overwritten (on disk %d >= formatter output %d); behaviour may have been fixed upstream", onDisk, written)
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
