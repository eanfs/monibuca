package pkg

import (
	"testing"
	"time"
)

func TestTsTamer_Tame(t *testing.T) {
	type args struct {
		ts         time.Duration
		wantResult time.Duration
	}
	tss := []time.Duration{10, 20, 30, 1000, 1010}
	wants := []time.Duration{1, 10, 20, 30, 40}

	tr := &TsTamer{}
	for i, tt := range tss {
		if gotResult := tr.Tame(tt*time.Millisecond, 100); gotResult != wants[i]*time.Millisecond {
			t.Errorf("TsTamer.Tame() = %v, want %v", gotResult, wants[i]*time.Millisecond)
		}
	}
}
