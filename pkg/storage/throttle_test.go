package storage

import (
	"bytes"
	"testing"
	"time"
)

func TestThrottledWriter_PacesThroughput(t *testing.T) {
	var sink bytes.Buffer
	// 10 MB/s 限速，写 5 MB，应耗时约 0.5s
	w := newThrottledWriter(&sink, 10*1024*1024)
	data := make([]byte, 1024*1024) // 1 MB chunk

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	elapsed := time.Since(start)

	if elapsed < 350*time.Millisecond {
		t.Fatalf("throttle too fast: 5MB at 10MB/s took %v, want >=350ms", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("throttle too slow: 5MB at 10MB/s took %v, want <=2s", elapsed)
	}
	if sink.Len() != 5*1024*1024 {
		t.Fatalf("sink got %d bytes, want %d", sink.Len(), 5*1024*1024)
	}
}

func TestThrottledWriter_ZeroRateNoLimit(t *testing.T) {
	var sink bytes.Buffer
	w := newThrottledWriter(&sink, 0) // 0 = 不限速
	data := make([]byte, 8*1024*1024)

	start := time.Now()
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("zero-rate writer should not sleep, took %v", elapsed)
	}
	if sink.Len() != 8*1024*1024 {
		t.Fatalf("sink got %d bytes, want %d", sink.Len(), 8*1024*1024)
	}
}
