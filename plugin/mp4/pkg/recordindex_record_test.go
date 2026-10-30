package mp4

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	m7s "m7s.live/v5"
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

	// 模拟 writeTrailerTask.Run：finalize 之前预写 RENAME，finalize 后校正。
	if err := task.recordIndexMediaPath(newMedia); err != nil {
		t.Fatalf("recordIndexMediaPath: %v", err)
	}
	if err := f.FinalizeFromTemp(newMedia); err != nil {
		t.Fatalf("FinalizeFromTemp: %v", err)
	}
	task.syncIndexMediaPath(newMedia)

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

// 分片 1 建索引失败（indexDisabled=true）不得污染分片 2：commitIndex 必须
// 无条件复位分片级状态，下一段首个 sample 重新尝试创建 sidecar。
func TestIndexDisabledInFragmentRetriedNextFragment(t *testing.T) {
	r, _, track, mediaPath := newIndexTestRecorder(t)
	sidecar := indexFilePath(mediaPath)

	// 用同名目录占住 sidecar 路径，让 CreateIndex 失败（模拟临时目录不可写）。
	if err := os.Mkdir(sidecar, 0o755); err != nil {
		t.Fatalf("block index path: %v", err)
	}
	if err := r.writeSample(track, newIndexTestSample(0, true, 1, 2, 3, 4)); err != nil {
		t.Fatalf("write sample fragment 1: %v", err)
	}
	if !r.indexDisabled || r.indexPath != "" {
		t.Fatalf("fragment 1 should disable index: disabled=%v indexPath=%q",
			r.indexDisabled, r.indexPath)
	}

	// 分片边界：writeTailer 调 commitIndex。
	if got := r.commitIndex(); got != "" {
		t.Fatalf("commitIndex = %q, want empty (no sidecar was created)", got)
	}
	if r.indexDisabled || r.index != nil || r.indexPath != "" || r.indexMediaPath != "" {
		t.Fatalf("fragment-level index state must be reset: disabled=%v index=%v indexPath=%q mediaPath=%q",
			r.indexDisabled, r.index, r.indexPath, r.indexMediaPath)
	}

	// 分片 2：障碍解除后首个 sample 必须重新建索引并成功提交。
	if err := os.Remove(sidecar); err != nil {
		t.Fatalf("unblock index path: %v", err)
	}
	if err := r.writeSample(track, newIndexTestSample(20, false, 5, 6, 7, 8)); err != nil {
		t.Fatalf("write sample fragment 2: %v", err)
	}
	if r.index == nil || r.indexPath != sidecar {
		t.Fatalf("index should be retried in next fragment: index=%v indexPath=%q",
			r.index, r.indexPath)
	}
	r.Event.Duration = 40
	if got := r.commitIndex(); got != sidecar {
		t.Fatalf("commitIndex = %q, want %q", got, sidecar)
	}
	got, err := ReadIndex(sidecar)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if !got.Committed || len(got.Records) != 1 {
		t.Fatalf("fragment 2 sidecar = %+v, want committed with 1 record", got)
	}
}

// 重建后重开失败会留下 index=nil 但 indexPath!="" 的状态。commitIndex 仍须
// 返回该路径（上传成功分支才能清理已有 sidecar），并复位状态让下一段重试。
func TestCommitIndexReturnsPathWhenHandleLost(t *testing.T) {
	r, _, track, mediaPath := newIndexTestRecorder(t)
	if err := r.writeSample(track, newIndexTestSample(0, true, 1, 2)); err != nil {
		t.Fatalf("write sample: %v", err)
	}
	sidecar := indexFilePath(mediaPath)
	if err := r.index.Close(); err != nil {
		t.Fatalf("close index handle: %v", err)
	}
	r.index = nil // 模拟重建后 openIndexAppend 失败：只剩路径，没有写句柄

	if got := r.commitIndex(); got != sidecar {
		t.Fatalf("commitIndex = %q, want %q even when handle is gone", got, sidecar)
	}
	if r.index != nil || r.indexPath != "" || r.indexMediaPath != "" || r.indexDisabled {
		t.Fatalf("state must be reset: index=%v indexPath=%q mediaPath=%q disabled=%v",
			r.index, r.indexPath, r.indexMediaPath, r.indexDisabled)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("existing sidecar must be kept on disk: %v", err)
	}
}

// —— I-2：pending 登记与 sidecar 清理 ——

func newPendingTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db handle: %v", err)
	}
	sqlDB.SetMaxOpenConns(1) // :memory: 每连接独立库，限单连接以共享同一库
	if err := db.AutoMigrate(&m7s.UploadTask{}, &m7s.AlarmInfo{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

// newPendingTestSidecar 造一个媒体文件及其已提交的 sidecar。
func newPendingTestSidecar(t *testing.T, dir string) (mediaPath, indexPath string) {
	t.Helper()
	mediaPath = filepath.Join(dir, "rec.mp4")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	indexPath = indexFilePath(mediaPath)
	w, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: mediaPath})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := w.Commit(0, 0); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return mediaPath, indexPath
}

func newPendingTestTrailerTask(indexPath string, db *gorm.DB) *writeTrailerTask {
	task := &writeTrailerTask{
		indexPath:  indexPath,
		db:         db,
		storageKey: "s3",
		streamPath: "live/test",
		filePath:   "live/test/rec.mp4",
	}
	task.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return task
}

// pending 登记成功后媒体被移出原目录，sidecar 必须一并删除，避免 MediaPath
// 指向不存在的文件；登记失败（pending 满）时媒体留在原地，sidecar 保留供重试。
func TestIndexRemovedAfterPendingRegistration(t *testing.T) {
	t.Run("registered removes sidecar", func(t *testing.T) {
		mediaDir := t.TempDir()
		storage.InitUploadManager(storage.UploadConfig{PendingDir: t.TempDir()})
		mediaPath, indexPath := newPendingTestSidecar(t, mediaDir)
		db := newPendingTestDB(t)
		task := newPendingTestTrailerTask(indexPath, db)

		if registered := task.recoverLocalFile(mediaPath, 5, errors.New("upload failed")); !registered {
			t.Fatal("recoverLocalFile should report successful pending registration")
		}
		if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
			t.Fatalf("sidecar must be removed after pending registration: %v", err)
		}
		if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
			t.Fatalf("media should have been moved to pending: %v", err)
		}
		var count int64
		if err := db.Model(&m7s.UploadTask{}).Count(&count).Error; err != nil {
			t.Fatalf("count pending uploads: %v", err)
		}
		if count != 1 {
			t.Fatalf("pending upload rows = %d, want 1", count)
		}
	})

	t.Run("pending full keeps sidecar", func(t *testing.T) {
		mediaDir := t.TempDir()
		pendingDir := t.TempDir()
		// 占满文件数上限，使 MoveToPendingDir 返回 ErrPendingDirFull。
		if err := os.WriteFile(filepath.Join(pendingDir, "existing.mp4"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed pending dir: %v", err)
		}
		storage.InitUploadManager(storage.UploadConfig{PendingDir: pendingDir, PendingMaxFiles: 1})
		mediaPath, indexPath := newPendingTestSidecar(t, mediaDir)
		task := newPendingTestTrailerTask(indexPath, newPendingTestDB(t))

		if registered := task.recoverLocalFile(mediaPath, 5, errors.New("upload failed")); registered {
			t.Fatal("recoverLocalFile must not report registration when pending dir is full")
		}
		if _, err := os.Stat(indexPath); err != nil {
			t.Fatalf("sidecar must be kept when pending registration fails: %v", err)
		}
		if _, err := os.Stat(mediaPath); err != nil {
			t.Fatalf("media must stay in place when pending registration fails: %v", err)
		}
	})
}
