package m7s

import "testing"

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
