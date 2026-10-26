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

// Important 4：commitIndex 只关句柄，**不得**注销活跃登记。sidecar 提交之后
// 文件还要经过 trailer 重写（moov 前移 / INSERT_RANGE），期间仍在被改写；
// 提前注销会让恢复扫描只剩 mtime 判据，长重写超过 minage 就被当成崩溃残留。
func TestCommitIndexKeepsActiveRegistration(t *testing.T) {
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
	if !IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("commitIndex must keep the recording active until the trailer finishes")
	}
	if r.indexPath != "" || r.indexMediaPath != "" {
		t.Fatalf("resetIndex left state behind: indexPath=%q mediaPath=%q", r.indexPath, r.indexMediaPath)
	}

	// 收尾由 writeTrailerTask 负责注销（Run 的 defer）。
	trailer := &writeTrailerTask{indexPath: indexPath, indexMediaPath: mediaPath}
	trailer.releaseActiveIndex()
	if IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("trailer completion must release the active registration")
	}
}

// closeIndex 表示媒体被丢弃（没有 trailer 任务接手），必须立刻注销。
func TestCloseIndexUnregistersActiveRecording(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "rec.mp4")
	indexPath := indexFilePath(mediaPath)
	w, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: mediaPath})
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	r := &Recorder{index: w, indexPath: indexPath, indexMediaPath: mediaPath}
	RegisterActiveIndex(indexPath, mediaPath)

	r.closeIndex()
	if IsActiveRecordingPath(indexPath, mediaPath) {
		t.Fatal("closeIndex must unregister the active recording")
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
