package m7s

import (
	"path/filepath"
	"testing"
	"time"

	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

func TestSegmentRecordPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		seq  int
		want string
	}{
		{"first segment unchanged", "live/ch1/rec-1.mp4", 0, "live/ch1/rec-1.mp4"},
		{"second segment", "live/ch1/rec-1.mp4", 1, "live/ch1/rec-1-001.mp4"},
		{"three digits padded", "live/ch1/rec-1.mp4", 42, "live/ch1/rec-1-042.mp4"},
		{"extension only on base name", "live/v1.2/rec.1.mp4", 7, "live/v1.2/rec.1-007.mp4"},
		{"no extension", "live/ch1/rec-1", 3, "live/ch1/rec-1-003"},
		{"beyond three digits", "live/ch1/rec-1.mp4", 1000, "live/ch1/rec-1-1000.mp4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := segmentRecordPath(tt.path, tt.seq); got != tt.want {
				t.Fatalf("segmentRecordPath(%q, %d) = %q, want %q", tt.path, tt.seq, got, tt.want)
			}
		})
	}
}

func newSegmentTestRecorder(t *testing.T, fileName string, st storage.Storage, status StorageStatus) *DefaultRecorder {
	t.Helper()
	server := &Server{}
	server.activateStorage(st, status)
	rec := &DefaultRecorder{}
	rec.RecordJob.Plugin = &Plugin{Server: server}
	rec.RecordJob.Subscriber = &Subscriber{Publisher: &Publisher{}}
	rec.RecordJob.RecConf = &config.Record{FilePath: "live/ch1", FileName: fileName, Type: "mp4"}
	return rec
}

func fixedName(*RecordJob) string { return "live/ch1/rec-1.mp4" }

func TestCreateStreamAssignsUniqueNamesAcrossSegments(t *testing.T) {
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := newSegmentTestRecorder(t, "rec-1.mp4", local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	want := []string{"rec-1.mp4", "rec-1-001.mp4", "rec-1-002.mp4", "rec-1-003.mp4"}
	for i, w := range want {
		if err := rec.CreateStream(time.Now(), fixedName); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		if got := filepath.Base(rec.Event.FilePath); got != w {
			t.Fatalf("segment %d path=%q, want %q", i, got, w)
		}
		if rec.Event.FileName != w {
			t.Fatalf("segment %d RecordStream.FileName=%q, want %q", i, rec.Event.FileName, w)
		}
	}
}

func TestCreateStreamDoesNotConsumeSegmentOnStorageFailure(t *testing.T) {
	rec := newSegmentTestRecorder(t, "rec-1.mp4", storage.NewUnavailableStorage("s3"),
		StorageStatus{DesiredType: "s3", ActiveType: "s3", Degraded: true})
	if err := rec.CreateStream(time.Now(), fixedName); err == nil {
		t.Fatal("expected storage error")
	}
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordJob.Plugin.Server.activateStorage(local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	if err := rec.CreateStream(time.Now(), fixedName); err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(rec.Event.FilePath); got != "rec-1.mp4" {
		t.Fatalf("recovered first segment path=%q, want %q", got, "rec-1.mp4")
	}
}

func TestCreateStreamKeepsTimestampNamingWhenFileNameEmpty(t *testing.T) {
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := newSegmentTestRecorder(t, "", local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	names := []string{
		"live/ch1/2026-11-22-20-00-00_000000001.mp4",
		"live/ch1/2026-11-22-20-00-01_000000002.mp4",
	}
	for i, name := range names {
		if err := rec.CreateStream(time.Now(), func(*RecordJob) string { return name }); err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		if rec.Event.FilePath != name {
			t.Fatalf("segment %d path=%q, want %q", i, rec.Event.FilePath, name)
		}
	}
}

func TestCreateStreamReusesNextNameAfterLaterSegmentFailure(t *testing.T) {
	local, err := storage.NewLocalStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec := newSegmentTestRecorder(t, "rec-1.mp4", local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	if err := rec.CreateStream(time.Now(), fixedName); err != nil {
		t.Fatalf("first segment: %v", err)
	}
	// The second segment's storage resolution fails; it must not consume -001.
	rec.RecordJob.Plugin.Server.activateStorage(storage.NewUnavailableStorage("s3"),
		StorageStatus{DesiredType: "s3", ActiveType: "s3", Degraded: true})
	if err := rec.CreateStream(time.Now(), fixedName); err == nil {
		t.Fatal("expected a storage error on the second segment")
	}
	// Recovered: the next successful segment is still -001, not -002.
	rec.RecordJob.Plugin.Server.activateStorage(local, StorageStatus{DesiredType: "local", ActiveType: "local"})
	if err := rec.CreateStream(time.Now(), fixedName); err != nil {
		t.Fatalf("third segment: %v", err)
	}
	if got := filepath.Base(rec.Event.FilePath); got != "rec-1-001.mp4" {
		t.Fatalf("after failed second segment path=%q, want %q", got, "rec-1-001.mp4")
	}
}
