package mp4

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/storage"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

func newIndexTestSample(ts uint32, keyFrame bool, payload ...byte) box.Sample {
	s := box.Sample{Timestamp: ts, KeyFrame: keyFrame}
	s.PushOne(payload)
	return s
}

// newIndexTestRecorder 构造一个写入本地临时文件的 progressive 录制器。
// 返回 recorder、muxer、首条轨道与媒体文件路径。
func newIndexTestRecorder(t *testing.T) (*Recorder, *Muxer, *Track, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rec.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create media file: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	file := &storage.LocalFile{File: f}
	m := NewMuxer(0)
	if err := m.WriteInitSegment(file); err != nil {
		t.Fatalf("write init segment: %v", err)
	}

	r := &Recorder{muxer: m, file: file}
	r.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	r.Event.StreamPath = "live/test"
	r.Event.FilePath = "live/test/rec.mp4"
	r.Event.StartTime = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

	track := m.AddTrack(box.MP4_CODEC_G711A)
	track.ICodecCtx = codec.NewPCMACtx()
	return r, m, track, path
}

// 录制两个 sample 后，同目录 sidecar 应有 2 条记录并带 commit；
// Commit 后 sidecar 必须保留（提交式删除发生在上传成功后）。
func TestIndexRecordingWritesRecordsAndCommits(t *testing.T) {
	r, _, track, mediaPath := newIndexTestRecorder(t)

	if err := r.writeSample(track, newIndexTestSample(0, true, 1, 2, 3, 4)); err != nil {
		t.Fatalf("write sample 1: %v", err)
	}
	if err := r.writeSample(track, newIndexTestSample(20, false, 5, 6, 7, 8)); err != nil {
		t.Fatalf("write sample 2: %v", err)
	}
	r.Event.Duration = 20
	r.commitIndex()

	indexPath := mediaPath + IndexExtension
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("sidecar %s missing after commit: %v", indexPath, err)
	}
	got, err := ReadIndex(indexPath)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !got.Committed {
		t.Fatal("index should be committed")
	}
	if len(got.Records) != 2 {
		t.Fatalf("records = %d, want 2 (%+v)", len(got.Records), got.Records)
	}
	want := []IndexRecord{
		{Flags: indexFlagKeyFrame, TrackID: 1, Size: 4, Offset: 48, DTS: 0},
		{Flags: 0, TrackID: 1, Size: 4, Offset: 52, DTS: 20},
	}
	for i := range want {
		if got.Records[i] != want[i] {
			t.Fatalf("record %d = %+v, want %+v", i, got.Records[i], want[i])
		}
	}

	h := got.Header
	if h.Version != 1 || h.StreamPath != "live/test" || h.FilePath != "live/test/rec.mp4" ||
		h.TargetName != "rec.mp4" || h.MediaPath != mediaPath ||
		!h.StartTime.Equal(r.Event.StartTime) {
		t.Fatalf("header = %+v", h)
	}
	if len(h.Tracks) != 1 {
		t.Fatalf("header tracks = %+v, want 1", h.Tracks)
	}
	if tr := h.Tracks[0]; tr.ID != 1 || tr.FourCC != "alaw" || len(tr.STSD) == 0 {
		t.Fatalf("track = %+v", tr)
	}
}

// 第二条轨道晚于首帧出现时，索引头需要重建以包含全部轨道，且历史记录不丢。
func TestIndexRebuiltWhenTrackAddedLater(t *testing.T) {
	r, m, videoTrack, mediaPath := newIndexTestRecorder(t)
	if err := r.writeSample(videoTrack, newIndexTestSample(0, true, 1, 2)); err != nil {
		t.Fatalf("write first sample: %v", err)
	}

	audioTrack := m.AddTrack(box.MP4_CODEC_G711A)
	audioTrack.ICodecCtx = codec.NewPCMACtx()
	if err := r.writeSample(audioTrack, newIndexTestSample(0, true, 3, 4)); err != nil {
		t.Fatalf("write second track sample: %v", err)
	}
	r.commitIndex()

	got, err := ReadIndex(mediaPath + IndexExtension)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(got.Header.Tracks) != 2 {
		t.Fatalf("header tracks = %+v, want 2", got.Header.Tracks)
	}
	if len(got.Records) != 2 {
		t.Fatalf("records = %+v, want 2", got.Records)
	}
	if got.Records[0].TrackID != 1 || got.Records[1].TrackID != 2 {
		t.Fatalf("record track ids = %d,%d", got.Records[0].TrackID, got.Records[1].TrackID)
	}
}

// pathChangingFile 模拟对象存储的 storage.File：FinalizeFromTemp 后
// 上传源（LocalFd）变成另一个本地文件。
type pathChangingFile struct {
	fakeStorageFile
	fd *os.File
}

func (f *pathChangingFile) LocalFd() *os.File { return f.fd }

func (f *pathChangingFile) FinalizeFromTemp(srcPath string) error {
	fd, err := os.OpenFile(srcPath, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if f.fd != nil {
		f.fd.Close()
	}
	f.fd = fd
	return nil
}

// finalize 换路径（对象存储把 S3 暂存文件换成 trailer 临时文件）时，
// 追加 RENAME 扩展记录，ReadIndex 的 MediaPath 必须反映新路径。
func TestIndexRenameAfterFinalize(t *testing.T) {
	dir := t.TempDir()
	oldMedia := filepath.Join(dir, "s3writer_old.tmp")
	newMedia := filepath.Join(dir, "trailer.mp4")
	for _, p := range []string{oldMedia, newMedia} {
		if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	indexPath := indexFilePath(oldMedia)
	w, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: oldMedia})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := w.Append(IndexRecord{TrackID: 1, Size: 10, Offset: 48}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Commit(40, 1); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fd, err := os.Open(oldMedia)
	if err != nil {
		t.Fatalf("open old media: %v", err)
	}
	t.Cleanup(func() { fd.Close() })
	f := &pathChangingFile{fd: fd}
	task := &writeTrailerTask{file: f, indexPath: indexPath}
	task.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))

	// 模拟 writeTrailerTask.Run 的 FinalizeFromTemp + 路径同步。
	if err := f.FinalizeFromTemp(newMedia); err != nil {
		t.Fatalf("FinalizeFromTemp: %v", err)
	}
	task.syncIndexMediaPath(oldMedia)

	got, err := ReadIndex(indexPath)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.MediaPath != newMedia {
		t.Fatalf("MediaPath = %q, want %q", got.MediaPath, newMedia)
	}
	if !got.Committed || len(got.Records) != 1 {
		t.Fatalf("rename must not disturb existing records: %+v", got)
	}
}

// trailer 重写会整体平移 mdat，索引记录必须同步为平移后的偏移。
func TestIndexRecordsRewrittenWithFinalOffsets(t *testing.T) {
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "a.mp4"+IndexExtension)
	w, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: "/tmp/a.mp4", StreamPath: "live/a"})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := w.Append(IndexRecord{TrackID: 1, Size: 10, Offset: 48, DTS: 0}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Commit(20, 1); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	m := NewMuxer(0)
	track := m.AddTrack(box.MP4_CODEC_G711A)
	track.ICodecCtx = codec.NewPCMACtx()
	s1 := box.Sample{Timestamp: 0, Offset: 96}
	s1.PushOne(make([]byte, 10))
	s2 := box.Sample{Timestamp: 20, Offset: 106}
	s2.PushOne(make([]byte, 10))
	track.Samplelist = []box.Sample{s1, s2}

	if err := rewriteIndexRecords(indexPath, m, 40); err != nil {
		t.Fatalf("rewriteIndexRecords: %v", err)
	}

	got, err := ReadIndex(indexPath)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(got.Records) != 2 {
		t.Fatalf("records = %+v, want 2", got.Records)
	}
	if got.Records[0].Offset != 96 || got.Records[1].Offset != 106 {
		t.Fatalf("offsets = %d,%d, want 96,106", got.Records[0].Offset, got.Records[1].Offset)
	}
	if !got.Committed || got.Header.StreamPath != "live/a" || got.Header.MediaPath != "/tmp/a.mp4" {
		t.Fatalf("header/commit should be preserved: %+v", got)
	}
}

// 上传成功后才删除 sidecar；上传失败必须保留以便补传/恢复。
func TestIndexRemovedOnlyAfterUploadSuccess(t *testing.T) {
	newIndex := func(t *testing.T) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "a.mp4"+IndexExtension)
		w, err := CreateIndex(p, IndexHeader{Version: 1, MediaPath: p})
		if err != nil {
			t.Fatalf("CreateIndex: %v", err)
		}
		if err := w.Commit(0, 0); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return p
	}

	t.Run("success", func(t *testing.T) {
		p := newIndex(t)
		u := &finalizeUploadTask{file: &fakeStorageFile{}, indexPath: p}
		u.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		if err := u.Go(); err != nil {
			t.Fatalf("Go: %v", err)
		}
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("sidecar should be removed after upload success: %v", err)
		}
		// 重复删除是幂等的
		if err := removeIndexAfterUpload(p); err != nil {
			t.Fatalf("removeIndexAfterUpload on missing file: %v", err)
		}
	})

	t.Run("failure", func(t *testing.T) {
		p := newIndex(t)
		u := &finalizeUploadTask{file: &fakeStorageFile{closeErr: errors.New("upload failed")}, indexPath: p}
		u.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		u.onFail = func(error) {}
		if err := u.Go(); err == nil {
			t.Fatal("Go() should return upload error")
		}
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("sidecar must be kept when upload fails: %v", err)
		}
	})
}
