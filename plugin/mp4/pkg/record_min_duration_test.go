package mp4

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/gormlite"
	"gorm.io/gorm"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

// discardProbeFile 是「对象存储暂存文件」的替身：Close 代表上传，Discard 代表
// 丢弃（关闭句柄 + 删除本地暂存文件，绝不上传）。两个计数用于断言丢弃路径
// 既不 Close 也不产生任何上传。
type discardProbeFile struct {
	fakeStorageFile
	mediaPath    string
	discardCalls int
	closeCalls   int
}

func (f *discardProbeFile) Discard() error {
	f.discardCalls++
	if f.mediaPath == "" {
		return nil
	}
	return os.Remove(f.mediaPath)
}

func (f *discardProbeFile) Close() error {
	f.closeCalls++
	f.closed = true
	return nil
}

var _ storage.Discarder = (*discardProbeFile)(nil)

func newDiscardTestDB(t *testing.T) *gorm.DB {
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
	if err := db.AutoMigrate(&m7s.RecordStream{}, &m7s.EventRecordStream{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return db
}

// newDiscardTestSegment 造一个「录制已启动」的片段：媒体文件、sidecar 与
// record_streams 初始行都已存在，模拟 CreateStream 之后、收尾之前的现场状态。
func newDiscardTestSegment(t *testing.T, db *gorm.DB, duration time.Duration) (*writeTrailerTask, *discardProbeFile, *m7s.RecordStream, string, string) {
	t.Helper()
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "rec.mp4")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	indexPath := indexFilePath(mediaPath)
	if err := os.WriteFile(indexPath, []byte("index"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	row := &m7s.RecordStream{
		StreamPath: "live/test",
		FilePath:   "live/test/rec.mp4",
		Type:       "mp4",
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create initial record_streams row: %v", err)
	}

	file := &discardProbeFile{mediaPath: mediaPath}
	trailer := &writeTrailerTask{
		file:        file,
		filePath:    "live/test/rec.mp4",
		streamPath:  "live/test",
		durationMs:  uint32(duration / time.Millisecond),
		minDuration: 5 * time.Second,
		db:          db,
		recordID:    row.ID,
		indexPath:   indexPath,
	}
	trailer.Logger = slog.New(&captureHandler{})
	return trailer, file, row, mediaPath, indexPath
}

func countRows(t *testing.T, db *gorm.DB, model any) int64 {
	t.Helper()
	var count int64
	if err := db.Model(model).Count(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

func captureOf(t *testing.T, trailer *writeTrailerTask) *captureHandler {
	t.Helper()
	h, ok := trailer.Logger.Handler().(*captureHandler)
	if !ok {
		t.Fatalf("test logger is not a captureHandler: %T", trailer.Logger.Handler())
	}
	return h
}

func hasMessage(h *captureHandler, msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.logs {
		if entry.Msg == msg {
			return true
		}
	}
	return false
}

// 回归（issue #41）：磁盘高 IO 时 ErrDiscard 会触发原地重录制，产生大量
// 短命片段。低于 record.minduration 的片段必须整段丢弃：不上传（不 Close）、
// 不写 record_streams、本地媒体与 sidecar 都删除，且不输出 record stopped /
// upload enqueued 这类被日志检索依赖的事件。
func TestShortSegmentBelowMinDurationIsDiscarded(t *testing.T) {
	db := newDiscardTestDB(t)
	trailer, file, _, mediaPath, indexPath := newDiscardTestSegment(t, db, 800*time.Millisecond)
	dbWriteCalls := 0
	trailer.dbWrite = func(task.IJob) error {
		dbWriteCalls++
		return nil
	}

	if err := trailer.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if file.discardCalls != 1 {
		t.Fatalf("Discard calls = %d, want 1", file.discardCalls)
	}
	if file.closeCalls != 0 {
		t.Fatalf("Close calls = %d, want 0: a discarded segment must not trigger an upload", file.closeCalls)
	}
	if dbWriteCalls != 0 {
		t.Fatalf("dbWrite calls = %d, want 0: a discarded segment must not finalize record_streams", dbWriteCalls)
	}
	if _, err := os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("media file should be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(indexPath); !os.IsNotExist(err) {
		t.Fatalf("index sidecar should be deleted, stat err = %v", err)
	}
	// 初始行必须真正消失：软删除行会被 cascade 的「恢复记录」路径清除
	// deleted_at 后重新放出，因此丢弃走 Unscoped 硬删除。
	if got := countRows(t, db, &m7s.RecordStream{}); got != 0 {
		t.Fatalf("record_streams rows = %d, want 0", got)
	}
	if got := countRows(t, db.Unscoped(), &m7s.RecordStream{}); got != 0 {
		t.Fatalf("record_streams rows (unscoped) = %d, want 0: the initial row must be hard-deleted", got)
	}

	h := captureOf(t, trailer)
	entry := h.mustFind(t, "record discarded: below minimum duration", slog.LevelInfo)
	if got := requireIntAttr(t, entry, "durationMs"); got != 800 {
		t.Fatalf("durationMs = %d, want 800", got)
	}
	requireStringAttr(t, entry, "filePath", "live/test/rec.mp4")
	requireStringAttr(t, entry, "streamPath", "live/test")
	if got := requireAttr(t, entry, "minDuration"); got != 5*time.Second {
		t.Fatalf("minDuration = %v, want 5s", got)
	}
	for _, forbidden := range []string{"record stopped", "upload enqueued", "upload succeeded", "write trailer"} {
		if hasMessage(h, forbidden) {
			t.Errorf("discarded segment must not emit %q", forbidden)
		}
	}
}

// 丢弃时录制启动写入的 event_record_streams 初始行（与 record_streams 共用
// 主键 ID）也必须清理，否则事件录像列表会留下 end_time=NULL 的幽灵行。
func TestDiscardRemovesInitialEventRecordRow(t *testing.T) {
	db := newDiscardTestDB(t)
	trailer, _, row, _, _ := newDiscardTestSegment(t, db, 500*time.Millisecond)
	if err := db.Save(&m7s.EventRecordStream{
		RecordEvent: &config.RecordEvent{EventId: "evt-1", EventLevel: config.EventLevelLow},
		RecordStream: m7s.RecordStream{
			ID:         row.ID,
			StreamPath: "live/test",
			FilePath:   "live/test/rec.mp4",
			Type:       "mp4",
		},
	}).Error; err != nil {
		t.Fatalf("create initial event_record_streams row: %v", err)
	}
	trailer.hasEventRecord = true

	if err := trailer.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := countRows(t, db, &m7s.EventRecordStream{}); got != 0 {
		t.Fatalf("event_record_streams rows = %d, want 0", got)
	}
	if got := countRows(t, db.Unscoped(), &m7s.EventRecordStream{}); got != 0 {
		t.Fatalf("event_record_streams rows (unscoped) = %d, want 0", got)
	}
}

// 达标段（>= 阈值）与未配置阈值（0）必须保持原行为：不丢弃。
func TestMinDurationThresholdBoundaries(t *testing.T) {
	cases := []struct {
		name        string
		minDuration time.Duration
		duration    time.Duration
		want        bool
	}{
		{name: "disabled by default", minDuration: 0, duration: 100 * time.Millisecond, want: false},
		{name: "below threshold", minDuration: 5 * time.Second, duration: 4999 * time.Millisecond, want: true},
		{name: "zero duration below threshold", minDuration: 5 * time.Second, duration: 0, want: true},
		{name: "equal to threshold is kept", minDuration: 5 * time.Second, duration: 5 * time.Second, want: false},
		{name: "above threshold is kept", minDuration: 5 * time.Second, duration: 6 * time.Second, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			trailer := &writeTrailerTask{
				durationMs:  uint32(c.duration / time.Millisecond),
				minDuration: c.minDuration,
			}
			if got := trailer.shouldDiscardBelowMinDuration(); got != c.want {
				t.Fatalf("shouldDiscardBelowMinDuration(%v, %v) = %v, want %v",
					c.minDuration, c.duration, got, c.want)
			}
		})
	}
}

// 达标段走完上游收尾：不调用 Discard、不删初始行，上传队列照常收到任务
// （这里用已停止的队列走内联兜底，断言 Close 与延迟入库都执行）。
func TestSegmentAtMinDurationKeepsExistingFinalizePath(t *testing.T) {
	db := newDiscardTestDB(t)
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "rec.mp4")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	row := &m7s.RecordStream{StreamPath: "live/test", FilePath: "live/test/rec.mp4", Type: "fmp4"}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("create initial row: %v", err)
	}

	file := &discardProbeFile{mediaPath: mediaPath}
	dbWriteCalls := 0
	trailer := &writeTrailerTask{
		file:        file,
		filePath:    "live/test/rec.mp4",
		streamPath:  "live/test",
		storageKey:  "local",
		durationMs:  5000,
		minDuration: 5 * time.Second,
		db:          db,
		recordID:    row.ID,
		dbWrite:     func(task.IJob) error { dbWriteCalls++; return nil },
	}
	trailer.Logger = slog.New(&captureHandler{})

	if trailer.shouldDiscardBelowMinDuration() {
		t.Fatal("segment at the threshold must be kept")
	}
	trailer.handoffToUploadQueueWith(newStoppedUploadQueue(t), 6, func(error) bool {
		t.Error("normal finalize must not fail")
		return false
	})

	if file.discardCalls != 0 {
		t.Fatalf("Discard calls = %d, want 0: a kept segment must not be discarded", file.discardCalls)
	}
	if file.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1: the normal finalize path must still persist the file", file.closeCalls)
	}
	if dbWriteCalls != 1 {
		t.Fatalf("dbWrite calls = %d, want 1: a kept segment must still finalize record_streams", dbWriteCalls)
	}
	if got := countRows(t, db, &m7s.RecordStream{}); got != 1 {
		t.Fatalf("record_streams rows = %d, want 1: a kept segment must not delete its row", got)
	}
	h := captureOf(t, trailer)
	if hasMessage(h, "record discarded: below minimum duration") {
		t.Fatal("kept segment must not log the discard event")
	}
	if !hasMessage(h, "upload succeeded") {
		t.Fatal("kept segment must still reach the upload/finalize path")
	}
}

// 装配线回归（复审 C1）：record.minduration / recordID / hasEventRecord 由
// writeTailer 经 applyDiscardPolicy 注入。这三个字段漏赋值不会编译失败，
// 只会让「配置不生效」或「丢弃后残留幽灵行」静默发生——删掉任一赋值
// 本用例必须红。
func TestApplyDiscardPolicyWiresConfigAndRowIDs(t *testing.T) {
	t.Cleanup(func() { SetRecordConfig(RecordConfig{}) })
	SetRecordConfig(RecordConfig{MinDuration: 3 * time.Second})

	var task writeTrailerTask
	task.applyDiscardPolicy(GetRecordConfig(), 42, true)

	if task.minDuration != 3*time.Second {
		t.Fatalf("minDuration = %v, want 3s (record.minduration must reach the trailer task)", task.minDuration)
	}
	if task.recordID != 42 {
		t.Fatalf("recordID = %d, want 42 (discard must be able to remove the initial row)", task.recordID)
	}
	if !task.hasEventRecord {
		t.Fatal("hasEventRecord = false, want true (event row must be cleaned up too)")
	}

	// 装配后的字段必须真正驱动丢弃决策：1s 的段在 3s 阈值下丢弃，5s 的保留。
	task.durationMs = 1000
	if !task.shouldDiscardBelowMinDuration() {
		t.Fatal("1s segment must be discarded when minDuration is 3s")
	}
	task.durationMs = 5000
	if task.shouldDiscardBelowMinDuration() {
		t.Fatal("5s segment must be kept when minDuration is 3s")
	}
}

// plainProbeFile 实现 RangeInserter（能暴露本地暂存路径）但不实现 Discarder，
// 模拟第三方存储后端。
type plainProbeFile struct {
	fakeStorageFile
	fd *os.File
}

func (f *plainProbeFile) LocalFd() *os.File { return f.fd }

var _ storage.RangeInserter = (*plainProbeFile)(nil)

// 复审 I5：未实现 Discarder 的后端走 os.Remove 兜底。绝不能退化成 Close——
// 对象存储的 Close 会把即将丢弃的内容上传，正好与丢弃相反。
func TestDiscardStorageFileFallsBackToRemoveWithoutClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s3writer_plain.tmp")
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	fd, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open media: %v", err)
	}
	defer fd.Close()

	file := &plainProbeFile{fd: fd}
	trailer := &writeTrailerTask{
		file:       file,
		filePath:   "live/test/rec.mp4",
		streamPath: "live/test",
	}
	trailer.Logger = slog.New(&captureHandler{})

	trailer.discardStorageFile()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("fallback must remove the local temp file, stat err = %v", err)
	}
	if file.closed {
		t.Fatal("fallback must never call Close: on object storage Close uploads the file")
	}
}
