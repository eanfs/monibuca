package plugin_mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	"m7s.live/v5/pkg/config"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

// recoveryTestSTSD 造一个可被 muxer 原样注入的 stsd。
func recoveryTestSTSD(t *testing.T) []byte {
	t.Helper()
	entry := box.CreateVisualSampleEntry(box.TypeAVC1, 320, 240,
		box.CreateDataBox(box.TypeAVCC, []byte{0x01, 0x64, 0x00, 0x1f, 0xff, 0xe0, 0x00}))
	var buf bytes.Buffer
	if _, err := box.WriteTo(&buf, box.CreateSTSDBox(entry)); err != nil {
		t.Fatalf("serialize stsd: %v", err)
	}
	return buf.Bytes()
}

// writeHalfWrittenRecording 造进程被杀时磁盘上的 progressive MP4：
// [ftyp][free][mdat 占位头] + 已写入的载荷（还没有 moov）。
func writeHalfWrittenRecording(t *testing.T, dir, name string, payload []byte) (mediaPath string, payloadStart int64) {
	t.Helper()
	mediaPath = filepath.Join(dir, name)
	f, err := os.Create(mediaPath)
	if err != nil {
		t.Fatalf("create media: %v", err)
	}
	defer f.Close()
	m := mp4pkg.NewMuxer(0)
	if err = m.WriteInitSegment(f); err != nil {
		t.Fatalf("write init segment: %v", err)
	}
	payloadStart = m.CurrentOffset
	if _, err = f.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return mediaPath, payloadStart
}

func writeRecoverySidecar(t *testing.T, sidecar string, header mp4pkg.IndexHeader, records []mp4pkg.IndexRecord) {
	t.Helper()
	w, err := mp4pkg.CreateIndex(sidecar, header)
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	for _, record := range records {
		if err = w.Append(record); err != nil {
			t.Fatalf("append index record: %v", err)
		}
	}
	if err = w.Commit(80, uint64(len(records))); err != nil {
		t.Fatalf("commit index: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}
}

// buildRecoveryFixture 造一个“半成品媒体 + sidecar”的完整现场。
func buildRecoveryFixture(t *testing.T, dir, name string, payload []byte) (mediaPath, sidecar string, records []mp4pkg.IndexRecord) {
	t.Helper()
	mediaPath, payloadStart := writeHalfWrittenRecording(t, dir, name, payload)
	split := len(payload) / 2
	if split == 0 {
		split = len(payload)
	}
	records = []mp4pkg.IndexRecord{
		{Flags: 0x01, TrackID: 1, Size: uint32(split), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		{TrackID: 1, Size: uint32(len(payload) - split), Offset: uint64(payloadStart) + uint64(split), DTS: 40, Duration: 40},
	}
	header := mp4pkg.IndexHeader{
		Version:     1,
		StreamPath:  "live/test",
		MediaPath:   mediaPath,
		FilePath:    "live/test/rec.mp4",
		TargetName:  "rec.mp4",
		StorageType: "s3",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240,
			STSD: recoveryTestSTSD(t),
		}},
	}
	sidecar = mediaPath + mp4pkg.IndexExtension
	writeRecoverySidecar(t, sidecar, header, records)
	return mediaPath, sidecar, records
}

type recoveryTopBox struct {
	typ    string
	offset int
	size   int
}

func scanRecoveryTopBoxes(data []byte) (boxes []recoveryTopBox) {
	for i := 0; i+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[i : i+4]))
		typ := string(data[i+4 : i+8])
		if size == 1 && i+16 <= len(data) {
			size = int(binary.BigEndian.Uint64(data[i+8 : i+16]))
		}
		if size < 8 || i+size > len(data) {
			break
		}
		boxes = append(boxes, recoveryTopBox{typ: typ, offset: i, size: size})
		i += size
	}
	return
}

func parseRecoveredMoov(t *testing.T, data []byte) *box.MoovBox {
	t.Helper()
	boxes := scanRecoveryTopBoxes(data)
	if len(boxes) < 3 || boxes[0].typ != "ftyp" || boxes[1].typ != "moov" || boxes[2].typ != "mdat" {
		t.Fatalf("output layout = %+v, want ftyp|moov|mdat", boxes)
	}
	parsed, err := box.ReadFrom(bytes.NewReader(data[boxes[1].offset : boxes[1].offset+boxes[1].size]))
	if err != nil {
		t.Fatalf("parse moov: %v", err)
	}
	moov, ok := parsed.(*box.MoovBox)
	if !ok {
		t.Fatalf("moov parsed as %T", parsed)
	}
	return moov
}

func assertRecoveredPlayable(t *testing.T, output string, wantSamples uint32) {
	t.Helper()
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read recovered output: %v", err)
	}
	moov := parseRecoveredMoov(t, data)
	if len(moov.Tracks) != 1 {
		t.Fatalf("moov tracks = %d, want 1", len(moov.Tracks))
	}
	stbl := moov.Tracks[0].MDIA.MINF.STBL
	if stbl.STSZ == nil || stbl.STSZ.SampleCount != wantSamples {
		t.Fatalf("stsz = %+v, want %d samples", stbl.STSZ, wantSamples)
	}
}

// 基本定稿：sidecar 被消费，源 tmp 被删除，产物是 moov-first 可解析 MP4。
func TestIndexRecoveryTaskRecoversSidecar(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_123.tmp", []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02})
	output := filepath.Join(dir, "s3writer_123.mp4")

	var mu sync.Mutex
	var enqueued []RecoveryItem
	recovery := &IndexRecoveryTask{
		EnqueueUpload: func(item RecoveryItem) error {
			mu.Lock()
			defer mu.Unlock()
			enqueued = append(enqueued, item)
			return nil
		},
	}

	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Scanned != 1 || stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want scanned=1 recovered=1", stats)
	}
	assertRecoveredPlayable(t, output, 2)

	if _, err = os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("source tmp still exists: %v", err)
	}
	if _, err = os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("sidecar still exists: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(enqueued) != 1 {
		t.Fatalf("enqueued = %d items, want 1", len(enqueued))
	}
	if enqueued[0].OutputPath != output || enqueued[0].Header.StreamPath != "live/test" {
		t.Fatalf("enqueued item = %+v", enqueued[0])
	}

	// 第二次运行必须是 no-op（幂等）。
	stats, err = recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if stats.Scanned != 0 || stats.Recovered != 0 || stats.Failed != 0 {
		t.Fatalf("second run stats = %+v, want all zero", stats)
	}
	if len(enqueued) != 1 {
		t.Fatalf("second run enqueued again: %d items", len(enqueued))
	}
}

// 崩溃在定稿中途：sidecar 已改名为 .recovering，下次启动必须继续完成。
func TestIndexRecoveryTaskRetriesRecoveringMarker(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_456.tmp", []byte{0xAA, 0xBB, 0xCC, 0xDD})
	marker := sidecar + recoveringSuffix
	if err := os.Rename(sidecar, marker); err != nil {
		t.Fatalf("rename to recovering: %v", err)
	}

	var enqueued int
	recovery := &IndexRecoveryTask{
		EnqueueUpload: func(RecoveryItem) error { enqueued++; return nil },
	}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	output := filepath.Join(dir, "s3writer_456.mp4")
	assertRecoveredPlayable(t, output, 2)
	if _, err = os.Stat(mediaPath); !os.IsNotExist(err) {
		t.Fatalf("source tmp still exists after retry")
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf(".recovering marker still exists after retry")
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", enqueued)
	}
}

// 入队失败时不得删除源 tmp / sidecar；下一次运行必须能重试并成功。
func TestIndexRecoveryTaskRetriesWhenEnqueueFails(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_789.tmp", []byte{0x10, 0x20, 0x30, 0x40})
	output := filepath.Join(dir, "s3writer_789.mp4")

	fail := true
	var enqueued int
	recovery := &IndexRecoveryTask{
		EnqueueUpload: func(RecoveryItem) error {
			if fail {
				return errors.New("queue unavailable")
			}
			enqueued++
			return nil
		},
	}

	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("expected error when enqueue fails")
	}
	if stats.Failed != 1 || stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if _, statErr := os.Stat(mediaPath); statErr != nil {
		t.Fatalf("source tmp must be kept when enqueue fails: %v", statErr)
	}
	if _, statErr := os.Stat(sidecar + recoveringSuffix); statErr != nil {
		t.Fatalf("recovering marker must be kept when enqueue fails: %v", statErr)
	}
	if _, statErr := os.Stat(output); statErr != nil {
		t.Fatalf("recovered output should already be renamed: %v", statErr)
	}

	fail = false
	stats, err = recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("retry RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("retry stats = %+v, want recovered=1", stats)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1 after retry", enqueued)
	}
	if _, statErr := os.Stat(mediaPath); !os.IsNotExist(statErr) {
		t.Fatalf("source tmp should be removed after successful retry")
	}
	if _, statErr := os.Stat(sidecar + recoveringSuffix); !os.IsNotExist(statErr) {
		t.Fatalf("recovering marker should be removed after successful retry")
	}
}

// 崩溃发生在“产物已 rename、源已删除、sidecar 未清理”的窗口：直接清理并移交。
func TestIndexRecoveryTaskOutputAlreadyInPlace(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_999.tmp", []byte{0x01, 0x02, 0x03, 0x04})
	output := filepath.Join(dir, "s3writer_999.mp4")
	if err := os.Rename(mediaPath, output); err != nil {
		t.Fatalf("simulate renamed output: %v", err)
	}

	var enqueued int
	recovery := &IndexRecoveryTask{
		EnqueueUpload: func(RecoveryItem) error { enqueued++; return nil },
	}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	if _, err = os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatalf("sidecar still exists")
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", enqueued)
	}
}

// 零可用样本是永久性失败：不产出空壳 mp4，标记 .failed，避免每次启动重试。
func TestIndexRecoveryTaskMarksUnrecoverableFailed(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	mediaPath, payloadStart := writeHalfWrittenRecording(t, dir, "s3writer_bad.tmp", payload)
	sidecar := mediaPath + mp4pkg.IndexExtension
	header := mp4pkg.IndexHeader{
		Version: 1, StreamPath: "live/bad", MediaPath: mediaPath, StorageType: "s3",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: recoveryTestSTSD(t),
		}},
	}
	writeRecoverySidecar(t, sidecar, header, []mp4pkg.IndexRecord{
		// 唯一记录越界 → ErrNoUsableSamples。
		{TrackID: 1, Size: 8, Offset: uint64(payloadStart) + uint64(len(payload)) + 100, DTS: 0, Duration: 40},
	})

	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error { return nil }}
	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("expected error for unrecoverable sidecar")
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "s3writer_bad.mp4")); !os.IsNotExist(statErr) {
		t.Fatalf("must not create output from no usable samples")
	}
	if _, statErr := os.Stat(sidecar + failedSuffix); statErr != nil {
		t.Fatalf(".failed marker missing: %v", statErr)
	}
	if _, statErr := os.Stat(sidecar); !os.IsNotExist(statErr) {
		t.Fatalf("original sidecar should be moved to .failed")
	}
}

// 无 sidecar 的 s3writer_*.tmp 只统计（Task 7 处理），不得移动/删除。
func TestIndexRecoveryTaskCountsBareTmp(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "s3writer_1000.tmp")
	if err := os.WriteFile(bare, []byte("no index here"), 0o644); err != nil {
		t.Fatalf("write bare tmp: %v", err)
	}

	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error { return nil }}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Scanned != 0 || stats.BareTmp != 1 {
		t.Fatalf("stats = %+v, want scanned=0 bareTmp=1", stats)
	}
	if _, statErr := os.Stat(bare); statErr != nil {
		t.Fatalf("bare tmp must be left untouched: %v", statErr)
	}
}

// 已启动的 task.Work 路径：并发 worker 定稿多个文件，结果与串行一致。
func TestIndexRecoveryTaskConcurrentRunOnce(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.tmp", "b.tmp", "c.tmp", "d.tmp"} {
		buildRecoveryFixture(t, dir, name, []byte{0x00, 0x01, 0x02, 0x03})
	}

	var root task.RootManager[uint32, *task.Work]
	root.Init()
	// 故意不 root.Stop：gotask v1.0.5 的 Job 销毁路径(waitChildrenDispose →
	// EventLoop.active)会与旧 loop goroutine 竞争写 mt.blocked，-race 下必报
	// （CLAUDE.md 已记录的上游缺陷，与仓库既有 TestOnStart* 测试同源）。
	// 测试进程退出会回收该 goroutine；不要为了“干净”把它变成 flaky。

	var mu sync.Mutex
	enqueued := 0
	recovery := &IndexRecoveryTask{
		Concurrency: 2,
		EnqueueUpload: func(RecoveryItem) error {
			mu.Lock()
			defer mu.Unlock()
			enqueued++
			return nil
		},
	}
	if err := root.AddTask(recovery).WaitStarted(); err != nil {
		t.Fatalf("start recovery task: %v", err)
	}

	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Scanned != 4 || stats.Recovered != 4 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want scanned=4 recovered=4", stats)
	}
	for _, name := range []string{"a", "b", "c", "d"} {
		assertRecoveredPlayable(t, filepath.Join(dir, name+".mp4"), 2)
	}
	mu.Lock()
	defer mu.Unlock()
	if enqueued != 4 {
		t.Fatalf("enqueued = %d, want 4", enqueued)
	}
}

// 配置默认值必须与既有行为等价：写索引、1s 落盘、启动恢复、并发 2。
// 同时确认 yaml 键是 record（字段名 RecordConfig 不能遮蔽 Plugin.Record 方法）。
func TestMP4RecordConfigDefaultsAndOverride(t *testing.T) {
	field, ok := reflect.TypeOf(MP4Plugin{}).FieldByName("RecordConfig")
	if !ok {
		t.Fatal("MP4Plugin.RecordConfig missing")
	}
	if got := field.Tag.Get("yaml"); got != "record" {
		t.Fatalf("RecordConfig yaml tag = %q, want record", got)
	}

	var plugin MP4Plugin
	var conf config.Config
	conf.Parse(&plugin.RecordConfig)
	if !plugin.RecordConfig.Index.Enable {
		t.Fatal("record.index.enable must default to true")
	}
	if plugin.RecordConfig.Index.FlushInterval != time.Second {
		t.Fatalf("record.index.flushinterval = %v, want 1s", plugin.RecordConfig.Index.FlushInterval)
	}
	if !plugin.RecordConfig.Recovery.OnStart {
		t.Fatal("record.recovery.onstart must default to true")
	}
	if plugin.RecordConfig.Recovery.Concurrency != 2 {
		t.Fatalf("record.recovery.concurrency = %d, want 2", plugin.RecordConfig.Recovery.Concurrency)
	}

	conf.ParseModifyFile(map[string]any{
		"index": map[string]any{
			"enable":        false,
			"flushinterval": "2s",
		},
		"recovery": map[string]any{
			"onstart":     false,
			"concurrency": 5,
			"dirs":        []any{"/tmp/recovery-a"},
		},
	})
	if plugin.RecordConfig.Index.Enable {
		t.Fatal("record.index.enable override was ignored")
	}
	if plugin.RecordConfig.Index.FlushInterval != 2*time.Second {
		t.Fatalf("flushinterval = %v, want 2s", plugin.RecordConfig.Index.FlushInterval)
	}
	if plugin.RecordConfig.Recovery.OnStart || plugin.RecordConfig.Recovery.Concurrency != 5 {
		t.Fatalf("recovery override = %+v", plugin.RecordConfig.Recovery)
	}
	if len(plugin.RecordConfig.Recovery.Dirs) != 1 || plugin.RecordConfig.Recovery.Dirs[0] != "/tmp/recovery-a" {
		t.Fatalf("recovery dirs = %v", plugin.RecordConfig.Recovery.Dirs)
	}
}

// 显式配置的目录不存在时必须报错（供 API 提示），但不能阻塞或 panic。
func TestIndexRecoveryTaskMissingConfiguredDir(t *testing.T) {
	recovery := &IndexRecoveryTask{
		Dirs:          []string{filepath.Join(t.TempDir(), "missing")},
		EnqueueUpload: func(RecoveryItem) error { return nil },
	}
	done := make(chan struct{})
	var stats RecoveryStats
	var err error
	go func() {
		stats, err = recovery.RunOnce(nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce blocked on missing directory")
	}
	if err == nil {
		t.Fatal("explicit missing configured dir should be reported as an error")
	}
	if stats.Scanned != 0 || len(stats.Errors) == 0 {
		t.Fatalf("stats = %+v, want scanned=0 with errors", stats)
	}
}
