package mp4

import (
	"path/filepath"
	"testing"
)

// indexHeader 必须把相对媒体路径绝对化：sidecar 的 MediaPath 是恢复侧
// 路径白名单的唯一依据，相对路径会被判越界（M1）。
func TestIndexHeaderStoresAbsoluteMediaPath(t *testing.T) {
	r := &Recorder{muxer: NewMuxer(0)}
	h := r.indexHeader(filepath.Join("record", "live", "relative.mp4"))
	if !filepath.IsAbs(h.MediaPath) {
		t.Fatalf("header.MediaPath = %q, want absolute", h.MediaPath)
	}
	want, err := filepath.Abs(filepath.Join("record", "live", "relative.mp4"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if h.MediaPath != want {
		t.Fatalf("header.MediaPath = %q, want %q", h.MediaPath, want)
	}
	// header.FilePath 保持原样：它是逻辑录像路径（DB 匹配/对象键），不能绝对化。
	r.Event.FilePath = "live/relative.mp4"
	h = r.indexHeader(filepath.Join("record", "relative.mp4"))
	if h.FilePath != "live/relative.mp4" {
		t.Fatalf("header.FilePath = %q, want unchanged logical path", h.FilePath)
	}
}

// commitIndex（以及 closeIndex，共用 resetIndex）必须注销活跃登记，否则恢复
// 扫描会永远跳过已经结束的录像（M2）。
func TestCommitIndexUnregistersActiveRecording(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "rec.mp4")
	indexPath := indexFilePath(mediaPath)
	w, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: mediaPath})
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	r := &Recorder{
		index:          w,
		indexPath:      indexPath,
		indexMediaPath: mediaPath,
	}
	r.Event.Duration = 1000
	RegisterActiveIndex(indexPath, mediaPath)
	if !IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("registered recording must be reported active")
	}

	r.commitIndex()
	if IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("commitIndex must unregister the active recording")
	}
	if r.indexPath != "" || r.indexMediaPath != "" {
		t.Fatalf("resetIndex left state behind: indexPath=%q mediaPath=%q", r.indexPath, r.indexMediaPath)
	}
}

func TestActiveIndexRegistryNormalizesPaths(t *testing.T) {
	mediaPath := filepath.Join("record", "live", "x.mp4")
	RegisterActiveIndex(mediaPath+IndexExtension, mediaPath)
	t.Cleanup(func() { UnregisterActiveIndex(mediaPath+IndexExtension, mediaPath) })

	abs, err := filepath.Abs(mediaPath)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	// 相对/绝对写法必须命中同一个键。
	if !IsActiveRecordingPath(abs) {
		t.Fatal("absolute form should match relative registration")
	}
	if IsActiveRecordingPath(filepath.Join("record", "live", "other.mp4")) {
		t.Fatal("unrelated path must not be active")
	}
	UnregisterActiveIndex(abs+IndexExtension, abs)
	if IsActiveRecordingPath(abs) {
		t.Fatal("unregister via absolute form must clear relative registration")
	}
}
