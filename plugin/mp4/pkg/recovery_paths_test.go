package mp4

import (
	"errors"
	"path/filepath"
	"testing"
)

// ValidateIndexHeaderPaths 是恢复管线的信任边界：sidecar 里的路径必须与
// MediaPath 绑定，否则拒绝。这里覆盖绝对/相对/StorageKey 回落三类形态。
func TestValidateIndexHeaderPaths(t *testing.T) {
	mediaDir := t.TempDir()
	mediaPath := filepath.Join(mediaDir, "rec.tmp")
	otherDir := t.TempDir()

	cases := []struct {
		name    string
		header  IndexHeader
		wantErr bool
	}{
		{
			name:   "no paths at all",
			header: IndexHeader{},
		},
		{
			name:   "absolute filePath equals mediaPath",
			header: IndexHeader{FilePath: mediaPath, MediaPath: mediaPath},
		},
		{
			name:   "absolute filePath in media directory",
			header: IndexHeader{FilePath: filepath.Join(mediaDir, "rec.mp4"), MediaPath: mediaPath},
		},
		{
			name:   "absolute filePath in media subdirectory",
			header: IndexHeader{FilePath: filepath.Join(mediaDir, "sub", "rec.mp4"), MediaPath: mediaPath},
		},
		{
			name:    "absolute filePath outside media directory",
			header:  IndexHeader{FilePath: "/etc/shadow", MediaPath: mediaPath},
			wantErr: true,
		},
		{
			name:    "absolute filePath in a sibling directory",
			header:  IndexHeader{FilePath: filepath.Join(otherDir, "x.mp4"), MediaPath: mediaPath},
			wantErr: true,
		},
		{
			name:    "absolute filePath escaping media directory",
			header:  IndexHeader{FilePath: filepath.Join(mediaDir, "..", "escape.mp4"), MediaPath: mediaPath},
			wantErr: true,
		},
		{
			name:    "absolute filePath without mediaPath",
			header:  IndexHeader{FilePath: "/etc/shadow"},
			wantErr: true,
		},
		{
			name:   "relative object key",
			header: IndexHeader{FilePath: "live/test/rec.mp4", MediaPath: mediaPath},
		},
		{
			name:    "relative object key with traversal",
			header:  IndexHeader{FilePath: "live/../../etc/shadow", MediaPath: mediaPath},
			wantErr: true,
		},
		{
			name:   "storageKey equals mediaPath",
			header: IndexHeader{StorageKey: mediaPath, MediaPath: mediaPath},
		},
		{
			name:   "relative storageKey fallback",
			header: IndexHeader{StorageKey: "prefix/live/test/rec.mp4", MediaPath: mediaPath},
		},
		{
			name:    "unsafe storageKey fallback",
			header:  IndexHeader{StorageKey: "/etc/shadow", MediaPath: mediaPath},
			wantErr: true,
		},
		{
			name:    "dot-only filePath",
			header:  IndexHeader{FilePath: ".", MediaPath: mediaPath},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		err := ValidateIndexHeaderPaths(tc.header)
		switch {
		case tc.wantErr && err == nil:
			t.Fatalf("%s: expected rejection, got nil", tc.name)
		case !tc.wantErr && err != nil:
			t.Fatalf("%s: unexpected rejection: %v", tc.name, err)
		case tc.wantErr && !errors.Is(err, ErrIndexHeaderPathMismatch):
			t.Fatalf("%s: error = %v, want ErrIndexHeaderPathMismatch", tc.name, err)
		}
	}
}
