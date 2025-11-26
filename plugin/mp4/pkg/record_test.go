package mp4

import (
    "path/filepath"
    "testing"
    m7s "m7s.live/v5"
    "m7s.live/v5/pkg/config"
)

func TestCustomFileName_MP4_WithFileName(t *testing.T) {
    job := &m7s.RecordJob{RecConf: &config.Record{FilePath: "/tmp/monibuca_test", FileName: "video1"}}
    got := CustomFileName(job)
    want := filepath.Join(job.RecConf.FilePath, "video1.mp4")
    if got != want {
        t.Fatalf("got %s want %s", got, want)
    }
}

func TestCustomFileName_MP4_WithFileNameHasExt(t *testing.T) {
    job := &m7s.RecordJob{RecConf: &config.Record{FilePath: "/tmp/monibuca_test", FileName: "video2.mp4"}}
    got := CustomFileName(job)
    want := filepath.Join(job.RecConf.FilePath, "video2.mp4")
    if got != want {
        t.Fatalf("got %s want %s", got, want)
    }
}
