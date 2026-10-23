package mp4

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// ValidateIndexHeaderPaths 是恢复管线的信任边界：sidecar 里的路径必须与
// MediaPath（本地存储）或对象键规则（s3/cos/oss）一致，否则拒绝。
// 错误信息必须点明命中的是哪条规则，便于现场定位。
func TestValidateIndexHeaderPaths(t *testing.T) {
	mediaDir := t.TempDir()
	mediaPath := filepath.Join(mediaDir, "rec.tmp")
	otherDir := t.TempDir()
	// 绝对 record.filepath 与 LocalStorage 拼出的 MediaPath（basePath + FilePath）。
	absoluteRecordPath := "/data/records/rec.mp4"
	absoluteRecordBase := t.TempDir()

	cases := []struct {
		name    string
		header  IndexHeader
		wantErr string // 空表示应通过；否则为必须出现的规则标签
	}{
		{
			name:   "no paths at all",
			header: IndexHeader{},
		},

		// ---- 本地存储 ----
		{
			name:   "local: absolute filePath equals mediaPath",
			header: IndexHeader{StorageType: "local", FilePath: mediaPath, MediaPath: mediaPath},
		},
		{
			name:   "local: absolute filePath in media directory",
			header: IndexHeader{StorageType: "local", FilePath: filepath.Join(mediaDir, "rec.mp4"), MediaPath: mediaPath},
		},
		{
			name:   "local: absolute filePath in media subdirectory",
			header: IndexHeader{StorageType: "local", FilePath: filepath.Join(mediaDir, "sub", "rec.mp4"), MediaPath: mediaPath},
		},
		{
			name:    "local: absolute filePath outside media directory",
			header:  IndexHeader{StorageType: "local", FilePath: "/etc/shadow", MediaPath: mediaPath},
			wantErr: "rule=local-directory",
		},
		{
			name:    "local: absolute filePath in a sibling directory",
			header:  IndexHeader{StorageType: "local", FilePath: filepath.Join(otherDir, "x.mp4"), MediaPath: mediaPath},
			wantErr: "rule=local-directory",
		},
		{
			name:    "local: absolute filePath without mediaPath",
			header:  IndexHeader{StorageType: "local", FilePath: "/etc/shadow"},
			wantErr: "rule=local-directory",
		},
		{
			name:   "local: relative filePath needs no directory binding",
			header: IndexHeader{StorageType: "local", FilePath: "live/test/rec.mp4", MediaPath: mediaPath},
		},
		{
			name:    "local: relative filePath with traversal",
			header:  IndexHeader{StorageType: "local", FilePath: "live/../../etc/shadow", MediaPath: mediaPath},
			wantErr: "rule=traversal",
		},
		{
			name:   "local: storageKey equals mediaPath",
			header: IndexHeader{StorageType: "local", StorageKey: mediaPath, MediaPath: mediaPath},
		},
		{
			// LocalStorage.CreateFile 用 filepath.Join(basePath, FilePath)：
			// 绝对 record.filepath 会被拼到 basePath 之后，MediaPath 以 FilePath 结尾。
			name: "local: absolute filePath joined under the storage base",
			header: IndexHeader{
				StorageType: "local",
				FilePath:    absoluteRecordPath,
				MediaPath:   filepath.Join(absoluteRecordBase, absoluteRecordPath),
			},
		},
		{
			name: "local: unrelated absolute filePath is not a MediaPath suffix",
			header: IndexHeader{
				StorageType: "local",
				FilePath:    "/etc/shadow",
				MediaPath:   filepath.Join(absoluteRecordBase, absoluteRecordPath),
			},
			wantErr: "rule=local-directory",
		},

		// ---- 对象存储（s3/cos/oss）----
		{
			name: "s3: relative object key with local staging mediaPath",
			header: IndexHeader{
				StorageType: "s3",
				FilePath:    "live/test/rec.mp4",
				MediaPath:   filepath.Join(t.TempDir(), "a.tmp"),
			},
		},
		{
			name: "s3: relative object key in a nested prefix",
			header: IndexHeader{
				StorageType: "s3",
				FilePath:    "prefix/live/test/rec.mp4",
				MediaPath:   filepath.Join(t.TempDir(), "a.tmp"),
			},
		},
		{
			name:    "s3: absolute filePath",
			header:  IndexHeader{StorageType: "s3", FilePath: "/etc/shadow", MediaPath: mediaPath},
			wantErr: "rule=object-key-absolute",
		},
		{
			// 对象键不应是绝对路径：即使恰好落在本地暂存目录内也拒绝。
			name:    "s3: absolute filePath inside media directory",
			header:  IndexHeader{StorageType: "s3", FilePath: filepath.Join(mediaDir, "rec.mp4"), MediaPath: mediaPath},
			wantErr: "rule=object-key-absolute",
		},
		{
			name:    "s3: filePath with traversal",
			header:  IndexHeader{StorageType: "s3", FilePath: "live/../../etc/shadow", MediaPath: mediaPath},
			wantErr: "rule=traversal",
		},
		{
			name:   "s3: relative storageKey fallback",
			header: IndexHeader{StorageType: "s3", StorageKey: "prefix/live/test/rec.mp4", MediaPath: mediaPath},
		},
		{
			name:    "s3: absolute storageKey fallback",
			header:  IndexHeader{StorageType: "s3", StorageKey: "/etc/shadow", MediaPath: mediaPath},
			wantErr: "rule=object-key-absolute",
		},
		{
			name:    "s3: dot-only filePath",
			header:  IndexHeader{StorageType: "s3", FilePath: ".", MediaPath: mediaPath},
			wantErr: "rule=object-key-invalid",
		},

		// ---- 未声明存储类型：按对象键从严处理 ----
		{
			name:    "empty storageType: absolute filePath",
			header:  IndexHeader{FilePath: filepath.Join(mediaDir, "rec.mp4"), MediaPath: mediaPath},
			wantErr: "rule=object-key-absolute",
		},
		{
			name:   "empty storageType: relative filePath",
			header: IndexHeader{FilePath: "live/test/rec.mp4", MediaPath: mediaPath},
		},
	}

	for _, tc := range cases {
		err := ValidateIndexHeaderPaths(tc.header)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Fatalf("%s: unexpected rejection: %v", tc.name, err)
		case tc.wantErr != "" && err == nil:
			t.Fatalf("%s: expected rejection (%s), got nil", tc.name, tc.wantErr)
		case tc.wantErr != "" && !errors.Is(err, ErrIndexHeaderPathMismatch):
			t.Fatalf("%s: error = %v, want ErrIndexHeaderPathMismatch", tc.name, err)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Fatalf("%s: error = %v, want it to mention %q", tc.name, err, tc.wantErr)
		}
	}
}
