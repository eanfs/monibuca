package hls

import (
    "path/filepath"
    "testing"
    m7s "m7s.live/v5"
    "m7s.live/v5/pkg/config"
)

func TestCustomFileName_TS_WithFileName(t *testing.T) {
    job := &m7s.RecordJob{RecConf: &config.Record{FilePath: "/tmp/monibuca_test", FileName: "segment1"}}
    got := CustomFileName(job)
    want := filepath.Join(job.RecConf.FilePath, "segment1.ts")
    if got != want {
        t.Fatalf("got %s want %s", got, want)
    }
}

