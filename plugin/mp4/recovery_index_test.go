package plugin_mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
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

// writeCompletedLocalMP4 造一个已有顶层 moov 的本地 mp4（ftyp|moov|mdat）。
// moov 必须是内容完整的（含 mvhd）：hasTopLevelMoov 除 box 头外还会校验 moov
// 是否写全，空负载的 moov 会被判为「未收尾」。
func writeCompletedLocalMP4(t *testing.T, path string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	writeBox := func(typ string, body []byte) {
		size := 8 + len(body)
		if err := binary.Write(&buf, binary.BigEndian, uint32(size)); err != nil {
			t.Fatalf("write box size: %v", err)
		}
		buf.WriteString(typ)
		buf.Write(body)
	}
	writeBox("ftyp", []byte("isom"))
	writeBox("moov", moovTestBox("mvhd", make([]byte, 100)))
	writeBox("mdat", payload)
	data := buf.Bytes()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write completed local mp4: %v", err)
	}
	return data
}

// ageRecoveryFiles 把文件 mtime 调到过去，模拟“崩溃残留”而非“仍在录制”。
// 恢复任务默认跳过 mtime 新鲜（< record.recovery.minage）的文件。
func ageRecoveryFiles(t *testing.T, paths ...string) {
	t.Helper()
	old := time.Now().Add(-10 * time.Minute)
	for _, path := range paths {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}
}

// buildRecoveryFixture 造一个“半成品媒体 + sidecar”的完整现场。
// 默认把 mtime 老化，避免被“仍在写入”保护跳过。
func buildRecoveryFixture(t *testing.T, dir, name string, payload []byte) (mediaPath, sidecar string, records []mp4pkg.IndexRecord) {
	t.Helper()
	return buildRecoveryFixtureWithStorage(t, dir, name, payload, "s3")
}

// buildRecoveryFixtureWithStorage 与 buildRecoveryFixture 相同，但可指定
// sidecar 中记录的存储类型（local/s3/...）。
func buildRecoveryFixtureWithStorage(t *testing.T, dir, name string, payload []byte, storageType string) (mediaPath, sidecar string, records []mp4pkg.IndexRecord) {
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
		StorageType: storageType,
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240,
			STSD: recoveryTestSTSD(t),
		}},
	}
	sidecar = mediaPath + mp4pkg.IndexExtension
	writeRecoverySidecar(t, sidecar, header, records)
	ageRecoveryFiles(t, mediaPath, sidecar)
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
	// 产物路径来自 guard 的 canon（macOS 上 /var -> /private/var）。
	if enqueued[0].OutputPath != canonPath(t, output) || enqueued[0].Header.StreamPath != "live/test" {
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
	ageRecoveryFiles(t, mediaPath, sidecar)

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
	if plugin.RecordConfig.Recovery.MinAge != 5*time.Minute {
		t.Fatalf("record.recovery.minage = %v, want 5m", plugin.RecordConfig.Recovery.MinAge)
	}
	if plugin.RecordConfig.Recovery.Concurrency != 2 {
		t.Fatalf("record.recovery.concurrency = %d, want 2", plugin.RecordConfig.Recovery.Concurrency)
	}
	// 周期重扫默认 10 分钟；0 表示关闭。
	if plugin.RecordConfig.Recovery.RerunInterval != 10*time.Minute {
		t.Fatalf("record.recovery.reruninterval = %v, want 10m", plugin.RecordConfig.Recovery.RerunInterval)
	}
	// 未启用登录时 run 端点默认只接受本地回环请求，必须显式放行才能对外。
	if plugin.RecordConfig.Recovery.AllowUnauthenticated {
		t.Fatal("record.recovery.allowunauthenticated must default to false")
	}

	conf.ParseModifyFile(map[string]any{
		"index": map[string]any{
			"enable":        false,
			"flushinterval": "2s",
		},
		"recovery": map[string]any{
			"onstart":              false,
			"minage":               "30s",
			"concurrency":          5,
			"reruninterval":        "90s",
			"dirs":                 []any{"/tmp/recovery-a"},
			"allowunauthenticated": true,
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
	if plugin.RecordConfig.Recovery.MinAge != 30*time.Second {
		t.Fatalf("recovery minage override = %v, want 30s", plugin.RecordConfig.Recovery.MinAge)
	}
	if plugin.RecordConfig.Recovery.RerunInterval != 90*time.Second {
		t.Fatalf("recovery reruninterval override = %v, want 90s", plugin.RecordConfig.Recovery.RerunInterval)
	}
	if len(plugin.RecordConfig.Recovery.Dirs) != 1 || plugin.RecordConfig.Recovery.Dirs[0] != "/tmp/recovery-a" {
		t.Fatalf("recovery dirs = %v", plugin.RecordConfig.Recovery.Dirs)
	}
	if !plugin.RecordConfig.Recovery.AllowUnauthenticated {
		t.Fatal("record.recovery.allowunauthenticated override was ignored")
	}
}

// Important 2：minage 配置默认值是 5m（非 0），因此夹紧必须无条件生效：
// flushinterval=10s 时，任何非负 minage 的生效值都不得低于 2×flushinterval=20s，
// 也不得低于 5s 下限。否则运维调大 flushinterval 后活跃保护会被削弱。
func TestIndexRecoveryTaskEffectiveMinAgeClamps(t *testing.T) {
	prev := mp4pkg.GetIndexConfig()
	t.Cleanup(func() { mp4pkg.SetIndexConfig(prev) })
	mp4pkg.SetIndexConfig(mp4pkg.IndexConfig{Enable: true, FlushInterval: 10 * time.Second})

	for _, configured := range []time.Duration{0, time.Second, 5 * time.Second} {
		got := (&IndexRecoveryTask{MinAge: configured}).effectiveMinAge()
		if got < 20*time.Second {
			t.Fatalf("minage=%v -> effective %v, want >= 20s (2x flushinterval)", configured, got)
		}
		if got < defaultRecoveryMinAge {
			t.Fatalf("minage=%v -> effective %v, want >= %v floor", configured, got, defaultRecoveryMinAge)
		}
	}
	// 更大的显式值不被压小。
	if got := (&IndexRecoveryTask{MinAge: time.Minute}).effectiveMinAge(); got != time.Minute {
		t.Fatalf("explicit minage 1m -> effective %v, want 1m", got)
	}
	// 负值仍表示关闭保护。
	if got := (&IndexRecoveryTask{MinAge: -time.Second}).effectiveMinAge(); got >= 0 {
		t.Fatalf("negative minage should disable protection, got %v", got)
	}

	// flushinterval 小于 2.5s 时回落到 5s 下限。
	mp4pkg.SetIndexConfig(mp4pkg.IndexConfig{Enable: true, FlushInterval: time.Second})
	if got := (&IndexRecoveryTask{}).effectiveMinAge(); got != defaultRecoveryMinAge {
		t.Fatalf("flushinterval=1s -> effective %v, want %v", got, defaultRecoveryMinAge)
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

// C1：本地场景 source == output。第一次恢复 rename 覆盖源文件后若入队失败，
// 第二次 RunOnce 不得再用旧坐标系的索引重建（会把 moov 字节当载荷，
// 写出结构合法但内容全错的 mp4，覆盖上一次正确产物）。
func TestIndexRecoveryTaskSecondRunKeepsRecoveredLocalFile(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "live.mp4", []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02})
	if output := recoveryOutputPath(mediaPath); output != mediaPath {
		t.Fatalf("local output path = %s, want source %s", output, mediaPath)
	}

	fail := true
	enqueued := 0
	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error {
		if fail {
			return errors.New("queue unavailable")
		}
		enqueued++
		return nil
	}}

	if _, err := recovery.RunOnce([]string{dir}); err == nil {
		t.Fatal("first RunOnce should fail when enqueue fails")
	}
	first, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read recovered local file: %v", err)
	}
	assertRecoveredPlayable(t, mediaPath, 2)
	if _, err = os.Stat(sidecar + recoveringSuffix); err != nil {
		t.Fatalf("recovering marker must survive enqueue failure: %v", err)
	}

	// 第二次运行：产物已含 moov，必须只入队 + 清理 sidecar，绝不重建。
	// 先老化产物 mtime：第一次恢复刚写出的文件在 minage 内属于「可能仍在写」，
	// 会被新鲜度闸门跳过（真实重启场景通常已超过 minage）。
	ageRecoveryFiles(t, mediaPath)
	fail = false
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if stats.Skipped != 1 || stats.Recovered != 0 || stats.Failed != 0 {
		t.Fatalf("second run stats = %+v, want skipped=1", stats)
	}
	second, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read local file after second run: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("second RunOnce corrupted recovered file: %d bytes -> %d bytes", len(first), len(second))
	}
	assertRecoveredPlayable(t, mediaPath, 2)
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", enqueued)
	}
	if _, err = os.Stat(sidecar + recoveringSuffix); !os.IsNotExist(err) {
		t.Fatalf("recovering marker should be removed after successful enqueue")
	}
}

// C2：活跃录制中的 sidecar/媒体 mtime 新鲜，RunOnce 必须跳过，
// 不得改名 sidecar、不得覆盖/产出媒体文件。
func TestIndexRecoveryTaskSkipsFreshlyWrittenSidecar(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_active.tmp", []byte{0x01, 0x02, 0x03, 0x04})
	now := time.Now()
	if err := os.Chtimes(mediaPath, now, now); err != nil {
		t.Fatalf("touch media: %v", err)
	}
	if err := os.Chtimes(sidecar, now, now); err != nil {
		t.Fatalf("touch sidecar: %v", err)
	}

	enqueued := 0
	recovery := &IndexRecoveryTask{
		MinAge:        time.Minute,
		EnqueueUpload: func(RecoveryItem) error { enqueued++; return nil },
	}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Scanned != 1 || stats.Skipped != 1 || stats.Recovered != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want scanned=1 skipped=1", stats)
	}
	if enqueued != 0 {
		t.Fatalf("fresh file must not be enqueued, got %d", enqueued)
	}
	if _, err = os.Stat(sidecar); err != nil {
		t.Fatalf("active sidecar must be left untouched: %v", err)
	}
	if _, err = os.Stat(sidecar + recoveringSuffix); !os.IsNotExist(err) {
		t.Fatal("active sidecar must not be renamed to .recovering")
	}
	if _, err = os.Stat(filepath.Join(dir, "s3writer_active.mp4")); !os.IsNotExist(err) {
		t.Fatal("fresh file must not produce an output mp4")
	}
}

// M2 回归：活跃 recorder 持有的 sidecar 即使 mtime 已陈旧（拉流重连退避、
// 低帧率、上游抖动导致长时间无新样本），RunOnce 也必须跳过——不得 rename
// sidecar、不得覆盖/删除媒体、不得产出恢复文件。这里显式关闭 mtime 保护
// （MinAge<0），证明拦截来自进程级活跃集合而不是新鲜度兜底。
func TestIndexRecoveryTaskSkipsActiveRecorderWithStaleSidecar(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "active_stale.tmp", []byte{0x01, 0x02, 0x03, 0x04})
	original, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	mp4pkg.RegisterActiveIndex(sidecar, mediaPath)
	t.Cleanup(func() { mp4pkg.UnregisterActiveIndex(sidecar, mediaPath) })

	enqueued := 0
	recovery := &IndexRecoveryTask{
		MinAge:        -time.Second, // 关闭 mtime 保护，隔离活跃集合判据
		EnqueueUpload: func(RecoveryItem) error { enqueued++; return nil },
	}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Scanned != 1 || stats.Skipped != 1 || stats.Recovered != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want scanned=1 skipped=1", stats)
	}
	if enqueued != 0 {
		t.Fatalf("active recording must not be enqueued, got %d", enqueued)
	}
	if _, err = os.Stat(sidecar); err != nil {
		t.Fatalf("active sidecar must be left untouched: %v", err)
	}
	if _, err = os.Stat(sidecar + recoveringSuffix); !os.IsNotExist(err) {
		t.Fatal("active sidecar must not be renamed to .recovering")
	}
	got, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media after skip: %v", err)
	}
	if !bytes.Equal(original, got) {
		t.Fatal("active media must not be modified")
	}
	if _, err = os.Stat(filepath.Join(dir, "active_stale.mp4")); !os.IsNotExist(err) {
		t.Fatal("active recording must not produce a recovery output")
	}

	// D：录制结束（commit/close）后活跃登记被移除，后续 RunOnce 必须能正常恢复。
	mp4pkg.UnregisterActiveIndex(sidecar, mediaPath)
	stats, err = recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce after commit: %v", err)
	}
	if stats.Recovered != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("stats after commit = %+v, want recovered=1", stats)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1 after commit", enqueued)
	}
	if _, err = os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatal("sidecar should be removed after recovery")
	}
	assertRecoveredPlayable(t, filepath.Join(dir, "active_stale.mp4"), 2)
}

// M1 回归：storage.local.path 为相对路径时，旧 sidecar 的 MediaPath 也是相对
// 路径。恢复必须按进程 CWD 解析为绝对路径后再做白名单校验，否则会永远
// errRecoveryPathDenied（sidecar 常驻、每次启动重试、永远恢复不了）。
func TestIndexRecoveryTaskRecoversRelativeLocalPath(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join("record", "live"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	payload := []byte{0x11, 0x22, 0x33, 0x44}
	mediaRel, payloadStart := writeHalfWrittenRecording(t, filepath.Join("record", "live"), "relative.mp4", payload)
	split := len(payload) / 2
	records := []mp4pkg.IndexRecord{
		{Flags: 0x01, TrackID: 1, Size: uint32(split), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		{TrackID: 1, Size: uint32(len(payload) - split), Offset: uint64(payloadStart) + uint64(split), DTS: 40, Duration: 40},
	}
	// 故意写入相对 MediaPath，复现旧版本 storage.local.path="." 的现场。
	header := mp4pkg.IndexHeader{
		Version:     1,
		StreamPath:  "live/relative",
		MediaPath:   mediaRel,
		FilePath:    filepath.Join("record", "live", "relative.mp4"),
		TargetName:  "relative.mp4",
		StorageType: "local",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240,
			STSD: recoveryTestSTSD(t),
		}},
	}
	sidecarRel := mediaRel + mp4pkg.IndexExtension
	writeRecoverySidecar(t, sidecarRel, header, records)
	ageRecoveryFiles(t, mediaRel, sidecarRel)

	backend := &recoveryUploadFakeStorage{key: "local"}
	recovery := &IndexRecoveryTask{
		ResolveUploadStorage: func(storageType string) (storage.Storage, error) {
			if storageType != "local" {
				t.Fatalf("storage type = %q, want local", storageType)
			}
			return backend, nil
		},
	}
	stats, err := recovery.RunOnce([]string{filepath.Join("record", "live")})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 || stats.Skipped != 0 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	absMedia := filepath.Join(dir, "record", "live", "relative.mp4")
	assertRecoveredPlayable(t, absMedia, 2)
	if _, err = os.Stat(sidecarRel); !os.IsNotExist(err) {
		t.Fatal("sidecar should be removed after local recovery")
	}
}

// Important 1：C1（本地文件已有顶层 moov）不得绕过新鲜度闸门。
// recorder 的 trailer INSERT_RANGE / moov 前移窗口内，文件已有 moov 但仍在写；
// 此时必须跳过且保留 sidecar，不得提前入队/移交半成品。老化后才走 C1 分支。
func TestIndexRecoveryTaskFreshLocalMoovIsSkipped(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "live_fresh.mp4")
	original := writeCompletedLocalMP4(t, mediaPath, []byte{0xDE, 0xAD, 0xBE, 0xEF})

	sidecar := mediaPath + mp4pkg.IndexExtension
	header := mp4pkg.IndexHeader{
		Version:     1,
		StreamPath:  "live/fresh",
		MediaPath:   mediaPath,
		FilePath:    "live/fresh.mp4",
		TargetName:  "fresh.mp4",
		StorageType: "local",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240,
			STSD: recoveryTestSTSD(t),
		}},
	}
	writeRecoverySidecar(t, sidecar, header, []mp4pkg.IndexRecord{
		{Flags: 0x01, TrackID: 1, Size: 16, Offset: 24, DTS: 0, Duration: 40},
	})
	// 故意不老化：媒体与 sidecar 都是「刚刚写入」。

	enqueued := 0
	recovery := &IndexRecoveryTask{
		MinAge:        time.Minute,
		EnqueueUpload: func(RecoveryItem) error { enqueued++; return nil },
	}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Scanned != 1 || stats.Skipped != 1 || stats.Recovered != 0 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want scanned=1 skipped=1", stats)
	}
	if enqueued != 0 {
		t.Fatalf("fresh local file with moov must not be enqueued, got %d", enqueued)
	}
	if _, err = os.Stat(sidecar); err != nil {
		t.Fatalf("active sidecar must be kept: %v", err)
	}
	if _, err = os.Stat(sidecar + recoveringSuffix); !os.IsNotExist(err) {
		t.Fatal("active sidecar must not be renamed to .recovering")
	}
	got, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	if !bytes.Equal(original, got) {
		t.Fatal("fresh media must not be modified")
	}

	// 老化后：C1 生效，仅入队 + 清 sidecar，绝不重建。
	ageRecoveryFiles(t, mediaPath, sidecar)
	stats, err = recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	if stats.Skipped != 1 || stats.Recovered != 0 || stats.Failed != 0 {
		t.Fatalf("aged stats = %+v, want skipped=1", stats)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1 after aging", enqueued)
	}
	if _, err = os.Stat(sidecar); !os.IsNotExist(err) {
		t.Fatal("sidecar should be removed after C1 handoff")
	}
	got, err = os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media after aging: %v", err)
	}
	if !bytes.Equal(original, got) {
		t.Fatal("C1 branch must not rebuild/overwrite local file")
	}

	// 幂等：sidecar 已清理，再跑为 no-op。
	stats, err = recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("third RunOnce: %v", err)
	}
	if stats.Scanned != 0 || stats.Skipped != 0 || enqueued != 1 {
		t.Fatalf("third run stats = %+v enqueued=%d, want no-op", stats, enqueued)
	}
}

// I2：媒体暂时缺失属于可重试错误，sidecar 不得被永久标记为 .failed；
// 媒体恢复后下一次 RunOnce 能正常定稿。
func TestIndexRecoveryTaskRetriesAfterTransientMediaLoss(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "live_transient.mp4", []byte{0x0A, 0x0B, 0x0C, 0x0D})
	original, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("stat media: %v", err)
	}
	if err = os.Remove(mediaPath); err != nil {
		t.Fatalf("remove media: %v", err)
	}

	enqueued := 0
	recovery := &IndexRecoveryTask{
		EnqueueUpload: func(RecoveryItem) error { enqueued++; return nil },
	}
	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("transient media loss should be reported for this run")
	}
	if stats.Failed != 1 || stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if _, err = os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar must be kept for retry: %v", err)
	}
	if _, err = os.Stat(sidecar + failedSuffix); !os.IsNotExist(err) {
		t.Fatal("transient IO/missing error must not mark .failed")
	}

	if err = os.WriteFile(mediaPath, original, info.Mode().Perm()); err != nil {
		t.Fatalf("restore media: %v", err)
	}
	ageRecoveryFiles(t, mediaPath)
	stats, err = recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("retry RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("retry stats = %+v, want recovered=1", stats)
	}
	if enqueued != 1 {
		t.Fatalf("enqueued = %d, want 1", enqueued)
	}
}

// I3：上传后端无法解析（如 S3 未配置/未就绪）时必须返回可判定错误，
// 恢复任务据此保留唯一的源文件（不得删除未上传的 .tmp），也绝不降级 local。
func TestIndexRecoveryTaskKeepsSourceWhenEnqueueUnverified(t *testing.T) {
	dir := t.TempDir()
	mediaPath, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_unverified.tmp", []byte{0x11, 0x22, 0x33, 0x44})
	output := filepath.Join(dir, "s3writer_unverified.mp4")

	if err := mp4pkg.EnqueueRecoveryUpload(mp4pkg.RecoveryUpload{MediaPath: mediaPath, OutputPath: output}); err == nil {
		t.Fatal("unresolvable storage must not report a verified enqueue")
	}

	resolveErr := errors.New("s3 not ready")
	recovery := &IndexRecoveryTask{
		ResolveUploadStorage: func(storageType string) (storage.Storage, error) {
			if storageType != "s3" {
				t.Fatalf("resolved storage type = %q, want s3 (never local)", storageType)
			}
			return nil, resolveErr
		},
	}
	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("unverified enqueue should surface as a run error")
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if _, statErr := os.Stat(mediaPath); statErr != nil {
		t.Fatalf("source tmp must be retained when enqueue is unverifiable: %v", statErr)
	}
	if _, statErr := os.Stat(sidecar + recoveringSuffix); statErr != nil {
		t.Fatalf("recovering marker must be retained when enqueue is unverifiable: %v", statErr)
	}
	if _, statErr := os.Stat(output); statErr != nil {
		t.Fatalf("recovered output should be in place: %v", statErr)
	}
}

// I1：record.recovery.onstart=false 只关闭索引扫描，不能静默关闭旧
// AutoRecovery 的 DB 补录。
func TestIndexRecoveryTaskRunsDBBackfillWhenOnStartDisabled(t *testing.T) {
	var root task.RootManager[uint32, *task.Work]
	root.Init()
	// 同 TestIndexRecoveryTaskConcurrentRunOnce：不 root.Stop，规避 gotask 竞态。

	called := make(chan struct{}, 1)
	recovery := &IndexRecoveryTask{
		RunOnStart:    false,
		RunDBBackfill: true,
		Backfill: func() (RecoveryStats, error) {
			select {
			case called <- struct{}{}:
			default:
			}
			return RecoveryStats{}, nil
		},
	}
	if err := root.AddTask(recovery).WaitStarted(); err != nil {
		t.Fatalf("start recovery task: %v", err)
	}
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("DB backfill did not run when record.recovery.onstart=false")
	}
}

// Finding 1 回归：默认 minage=5m + 启动扫描只跑一次时，崩溃后秒级重启的残留
// 曾因 age<5m 被全部跳过且再无自动重跑，与 spec §2「无需人工介入」冲突。
// 启动扫描必须使用短阈值 max(5s, 2×flushinterval)；手动 run 仍按配置值跳过。
func TestStartupRecoveryUsesShortMinAge(t *testing.T) {
	prev := mp4pkg.GetIndexConfig()
	t.Cleanup(func() { mp4pkg.SetIndexConfig(prev) })
	mp4pkg.SetIndexConfig(mp4pkg.IndexConfig{Enable: true, FlushInterval: time.Second})

	// 默认配置下：手动阈值 = 5m，启动阈值 = 5s。
	def := &IndexRecoveryTask{MinAge: 5 * time.Minute}
	if got := def.startupMinAge(); got != 5*time.Second {
		t.Fatalf("default startupMinAge = %v, want 5s", got)
	}
	if got := def.effectiveMinAge(); got != 5*time.Minute {
		t.Fatalf("default effectiveMinAge = %v, want 5m", got)
	}

	// 启动阈值仍随 flushinterval 抬高，且负 minage 仍表示显式关闭保护。
	mp4pkg.SetIndexConfig(mp4pkg.IndexConfig{Enable: true, FlushInterval: 10 * time.Second})
	if got := (&IndexRecoveryTask{MinAge: 5 * time.Minute}).startupMinAge(); got != 20*time.Second {
		t.Fatalf("startupMinAge with flushinterval=10s = %v, want 20s", got)
	}
	if got := (&IndexRecoveryTask{MinAge: -time.Second}).startupMinAge(); got >= 0 {
		t.Fatalf("negative minage should disable startup protection, got %v", got)
	}
	mp4pkg.SetIndexConfig(mp4pkg.IndexConfig{Enable: true, FlushInterval: time.Second})

	// 造「崩溃后约 10 秒重启」的残留：age 远小于默认 5m，但大于 5s 下限。
	newRestartFixture := func(t *testing.T) (string, string, string) {
		t.Helper()
		dir := t.TempDir()
		media, sidecar, _ := buildRecoveryFixture(t, dir, "restart_fresh.tmp", []byte{0x01, 0x02, 0x03, 0x04})
		recent := time.Now().Add(-10 * time.Second)
		if err := os.Chtimes(media, recent, recent); err != nil {
			t.Fatalf("touch media: %v", err)
		}
		if err := os.Chtimes(sidecar, recent, recent); err != nil {
			t.Fatalf("touch sidecar: %v", err)
		}
		return dir, media, sidecar
	}

	// 启动扫描：同一新鲜度必须恢复（不再跳过）。
	startupDir, startupMedia, startupSidecar := newRestartFixture(t)
	enqueued := 0
	startup := &IndexRecoveryTask{
		MinAge:        5 * time.Minute, // 生产默认值
		EnqueueUpload: func(RecoveryItem) error { enqueued++; return nil },
	}
	stats := startup.runRecovery([]string{startupDir}, false, startup.startupMinAge(), false)
	if stats.Recovered != 1 || stats.Skipped != 0 || stats.Failed != 0 {
		t.Fatalf("startup stats = %+v, want recovered=1", stats)
	}
	if enqueued != 1 {
		t.Fatalf("startup enqueued = %d, want 1", enqueued)
	}
	if _, err := os.Stat(recoveryOutputPath(startupMedia)); err != nil {
		t.Fatalf("startup recovery output missing: %v", err)
	}
	if _, err := os.Stat(startupSidecar); !os.IsNotExist(err) {
		t.Fatalf("startup recovery should consume sidecar: %v", err)
	}

	// 手动 run：同样的新鲜度必须按 5m 跳过，不得绕过配置。
	manualDir, manualMedia, manualSidecar := newRestartFixture(t)
	manualEnqueued := 0
	manual := &IndexRecoveryTask{
		MinAge:        5 * time.Minute,
		EnqueueUpload: func(RecoveryItem) error { manualEnqueued++; return nil },
	}
	stats, err := manual.RunOnce([]string{manualDir})
	if err != nil {
		t.Fatalf("manual RunOnce: %v", err)
	}
	if stats.Skipped != 1 || stats.Recovered != 0 || stats.Failed != 0 {
		t.Fatalf("manual stats = %+v, want skipped=1", stats)
	}
	if manualEnqueued != 0 {
		t.Fatalf("manual run must not enqueue fresh file, got %d", manualEnqueued)
	}
	if _, err = os.Stat(manualMedia + recoveringSuffix); !os.IsNotExist(err) {
		t.Fatal("manual run must not mark fresh sidecar as recovering")
	}
	if _, err = os.Stat(manualSidecar); err != nil {
		t.Fatalf("manual run must leave fresh sidecar untouched: %v", err)
	}
}

// recoveryTestAudioSTSD 造一个音轨 stsd（G.711A），用于"声明音轨但无样本"场景。
func recoveryTestAudioSTSD(t *testing.T) []byte {
	t.Helper()
	entry := box.CreateAudioSampleEntry(box.TypeALAW, 1, 16, 8000, nil)
	var buf bytes.Buffer
	if _, err := box.WriteTo(&buf, box.CreateSTSDBox(entry)); err != nil {
		t.Fatalf("serialize audio stsd: %v", err)
	}
	return buf.Bytes()
}

// 索引声明 video+audio、但音频记录全部越界：恢复成功但音轨被丢弃。必须
// (1) 在 RecoveryItem 里暴露 DroppedTrackIDs，(2) 记 Warn（含 droppedTracks），
// 让运维/DB 回写知道这版产物没有音轨。
func TestIndexRecoveryReportsDroppedTracks(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	mediaPath, payloadStart := writeHalfWrittenRecording(t, dir, "s3writer_drop.tmp", payload)
	sidecar := mediaPath + mp4pkg.IndexExtension
	header := mp4pkg.IndexHeader{
		Version: 1, StreamPath: "live/drop", MediaPath: mediaPath,
		FilePath: "live/drop/rec.mp4", StorageType: "s3",
		Tracks: []mp4pkg.IndexTrack{
			{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: recoveryTestSTSD(t)},
			{ID: 2, FourCC: "alaw", SampleRate: 8000, Channels: 1, STSD: recoveryTestAudioSTSD(t)},
		},
	}
	writeRecoverySidecar(t, sidecar, header, []mp4pkg.IndexRecord{
		{TrackID: 1, Size: uint32(len(payload)), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		{TrackID: 2, Size: 16, Offset: uint64(payloadStart) + uint64(len(payload)) + 100, DTS: 0, Duration: 20},
	})
	ageRecoveryFiles(t, mediaPath, sidecar)

	logs := &captureHandler{}
	var got RecoveryItem
	recovery := &IndexRecoveryTask{
		EnqueueUpload: func(item RecoveryItem) error { got = item; return nil },
	}
	recovery.Logger = slog.New(logs)

	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	if len(got.DroppedTrackIDs) != 1 || got.DroppedTrackIDs[0] != 2 {
		t.Fatalf("DroppedTrackIDs = %v, want [2]", got.DroppedTrackIDs)
	}
	if !got.Truncated {
		t.Fatalf("Truncated = false, want true (audio records were out of bounds)")
	}
	entry := logs.mustFind(t, "recovery dropped declared tracks with no usable samples", slog.LevelWarn)
	if entry.Attrs["droppedTracks"] == nil {
		t.Fatalf("warn log missing droppedTracks: %+v", entry.Attrs)
	}
	if entry.Attrs["outcome"] != "recovered" {
		t.Fatalf("warn log outcome = %v, want recovered", entry.Attrs["outcome"])
	}
}

// 索引中段结构性损坏（零帧）后仍有记录：读取返回可用前缀并标记 Truncated。
// 恢复必须继续产出可播放文件，同时把 truncated=true 暴露给日志与恢复 API。
func TestIndexRecoveryReportsStructuralIndexTruncation(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x01, 0x02, 0x03, 0x04}
	mediaPath, payloadStart := writeHalfWrittenRecording(t, dir, "s3writer_trunc.tmp", payload)
	sidecar := mediaPath + mp4pkg.IndexExtension
	header := mp4pkg.IndexHeader{
		Version: 1, StreamPath: "live/trunc", MediaPath: mediaPath,
		FilePath: "live/trunc/rec.mp4", StorageType: "s3",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: recoveryTestSTSD(t),
		}},
	}
	// 手工拼 sidecar：header + 1 条有效记录 + 23 字节零帧 + 1 条有效记录。
	w, err := mp4pkg.CreateIndex(sidecar, header)
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	first := mp4pkg.IndexRecord{TrackID: 1, Size: uint32(len(payload)), Offset: uint64(payloadStart), DTS: 0, Duration: 40}
	if err = w.Append(first); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	f, err := os.OpenFile(sidecar, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open sidecar: %v", err)
	}
	if _, err = f.Write(make([]byte, mp4pkg.IndexRecordSize)); err != nil {
		t.Fatalf("append zero frame: %v", err)
	}
	f.Close()
	ageRecoveryFiles(t, mediaPath, sidecar)

	var observed []bool
	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error { return nil }}
	recovery.observer = func(_, _ string, outcome string, _ error, _ int, truncated bool) {
		if outcome == "recovered" {
			observed = append(observed, truncated)
		}
	}

	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	if len(observed) != 1 || !observed[0] {
		t.Fatalf("observer truncated flags = %v, want [true]", observed)
	}
	assertRecoveredPlayable(t, filepath.Join(dir, "s3writer_trunc.mp4"), 1)
}

// stsd 结构性损坏（entry_count 与实际条目数不符）不可重试：恢复入口必须
// markFailed，避免每次启动都重复失败；日志要标出损坏的轨道。
func TestIndexRecoveryCorruptSTSDMarksFailed(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0xDE, 0xAD}
	mediaPath, payloadStart := writeHalfWrittenRecording(t, dir, "s3writer_stsd.tmp", payload)
	sidecar := mediaPath + mp4pkg.IndexExtension
	// size=16 的 stsd：version/flags(4) + entry_count=1，但没有任何 entry。
	corrupt := []byte{0, 0, 0, 16, 's', 't', 's', 'd', 0, 0, 0, 0, 0, 0, 0, 1}
	header := mp4pkg.IndexHeader{
		Version: 1, StreamPath: "live/stsd", MediaPath: mediaPath,
		FilePath: "live/stsd/rec.mp4", StorageType: "s3",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: corrupt,
		}},
	}
	writeRecoverySidecar(t, sidecar, header, []mp4pkg.IndexRecord{
		{TrackID: 1, Size: uint32(len(payload)), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
	})
	ageRecoveryFiles(t, mediaPath, sidecar)

	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error { return nil }}
	recovery.Logger = slog.New(logs)

	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("expected error for corrupt stsd")
	}
	if stats.Failed != 1 || stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if _, statErr := os.Stat(sidecar + failedSuffix); statErr != nil {
		t.Fatalf(".failed marker missing: %v", statErr)
	}
	entry := logs.mustFind(t, "reject recovery: sample description is structurally corrupt", slog.LevelError)
	if entry.Attrs["damagedTracks"] == nil {
		t.Fatalf("log missing damagedTracks: %+v", entry.Attrs)
	}
}

// 媒体长度为 0（文件仍在创建/暂存中断）不等于"无可用样本"的永久失败：
// 必须保留 sidecar 等下次重试，不能落 .failed。
func TestIndexRecoveryZeroLengthMediaKeepsSidecar(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "s3writer_empty.tmp")
	if err := os.WriteFile(mediaPath, nil, 0o644); err != nil {
		t.Fatalf("write empty media: %v", err)
	}
	sidecar := mediaPath + mp4pkg.IndexExtension
	header := mp4pkg.IndexHeader{
		Version: 1, StreamPath: "live/empty", MediaPath: mediaPath,
		FilePath: "live/empty/rec.mp4", StorageType: "s3",
		Tracks: []mp4pkg.IndexTrack{{
			ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: recoveryTestSTSD(t),
		}},
	}
	writeRecoverySidecar(t, sidecar, header, []mp4pkg.IndexRecord{
		{TrackID: 1, Size: 4, Offset: 48, DTS: 0, Duration: 40},
	})
	ageRecoveryFiles(t, mediaPath, sidecar)

	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error { return nil }}
	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("RunOnce must report the failed attempt")
	}
	if stats.Failed != 1 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if _, statErr := os.Stat(sidecar + failedSuffix); !os.IsNotExist(statErr) {
		t.Fatalf("zero-length media must not be marked .failed (stat err=%v)", statErr)
	}
	if _, statErr := os.Stat(sidecar + recoveringSuffix); statErr != nil {
		t.Fatalf(".recovering marker must be kept for retry: %v", statErr)
	}
}

// waitForRecovery 在 deadline 前轮询 ready。周期重扫的间隔由测试注入
// （100ms），用轮询而不是固定 sleep 避免在慢机器上不稳定。
func waitForRecovery(t *testing.T, deadline time.Duration, what string, ready func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if ready() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// 周期重扫（Important 9）：启动扫描因暂时性故障（本例为上传队列尚未就绪）
// 失败的残留，必须由 reruninterval 到点后的自动重扫重试成功，而不是等到下次
// 进程重启或人工 POST /api/recovery/run。这是 PR「恢复无需人工介入」承诺的
// 最后一块拼图。
func TestIndexRecoveryTaskPeriodicRerunRetriesLeftovers(t *testing.T) {
	dir := t.TempDir()
	_, sidecar, _ := buildRecoveryFixture(t, dir, "s3writer_rerun.tmp", []byte{0x01, 0x02, 0x03, 0x04})
	marker := sidecar + recoveringSuffix
	if err := os.Rename(sidecar, marker); err != nil {
		t.Fatalf("mark sidecar recovering: %v", err)
	}
	output := filepath.Join(dir, "s3writer_rerun.mp4")

	var enqueued atomic.Int32
	var attempts atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	recovery := &IndexRecoveryTask{
		Dirs:          []string{dir},
		Concurrency:   1,
		RunOnStart:    true,
		RerunInterval: 100 * time.Millisecond,
		EnqueueUpload: func(RecoveryItem) error {
			attempts.Add(1)
			if fail.Load() {
				return errors.New("upload queue not ready")
			}
			enqueued.Add(1)
			return nil
		},
	}

	var root task.RootManager[uint32, *task.Work]
	root.Init()
	// 同其它 gotask 测试：不 root.Stop，规避上游 dispose-vs-start 竞态。
	if err := root.AddTask(recovery).WaitStarted(); err != nil {
		t.Fatalf("start recovery task: %v", err)
	}
	defer recovery.Stop(errors.New("test complete"))

	// 启动扫描：产物落盘但入队失败，.recovering 必须保留（可重试状态）。
	// 必须等到启动扫描**真的尝试过**入队（失败）后再放行，否则慢机器上可能
	// 让启动扫描直接成功，测试就绕过了周期重扫这条路径。
	waitForRecovery(t, 5*time.Second, "startup scan to fail its enqueue", func() bool {
		return attempts.Load() >= 1
	})
	if _, statErr := os.Stat(output); statErr != nil {
		t.Fatalf("startup scan must leave the recovered output in place: %v", statErr)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf(".recovering marker must survive the failed startup scan: %v", statErr)
	}

	// 放行入队：下一轮周期重扫必须自动完成移交并清掉 .recovering。
	fail.Store(false)
	waitForRecovery(t, 5*time.Second, "periodic rerun to finish the handoff", func() bool {
		_, statErr := os.Stat(marker)
		return os.IsNotExist(statErr)
	})
	if got := enqueued.Load(); got != 1 {
		t.Fatalf("enqueued = %d, want 1 (exactly one successful handoff)", got)
	}
}

// 周期重扫的开关语义：reruninterval=0 关闭；onstart=false（关闭自动恢复）
// 时也不注册——该键承诺的是「启动时不做自动扫描」，周期扫描同样属于自动
// 恢复，不能绕过它。只有显式开启自动恢复且间隔为正才注册 TickTask。
func TestIndexRecoveryTaskPeriodicRerunGating(t *testing.T) {
	cases := []struct {
		name     string
		onStart  bool
		interval time.Duration
		want     bool
	}{
		{"enabled", true, 10 * time.Minute, true},
		{"zero interval disables", true, 0, false},
		{"onstart=false disables automatic recovery", false, 10 * time.Minute, false},
		{"negative interval disables", true, -time.Second, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recovery := &IndexRecoveryTask{RunOnStart: tc.onStart, RerunInterval: tc.interval}
			if got := recovery.periodicRerunEnabled(); got != tc.want {
				t.Fatalf("periodicRerunEnabled(onStart=%v, interval=%v) = %v, want %v",
					tc.onStart, tc.interval, got, tc.want)
			}
		})
	}
	if got := (&recoveryRerunTask{owner: &IndexRecoveryTask{RerunInterval: 10 * time.Minute}}).GetTickInterval(); got != 10*time.Minute {
		t.Fatalf("GetTickInterval = %v, want the configured interval", got)
	}
}

// 周期重扫在没有残留时必须极廉价：不做逐文件工作，也不产生 Info 日志
// （每 10 分钟一次的扫描不能把启动汇总日志变成周期性噪声）。
func TestPeriodicRecoveryScanIsQuietWhenNothingToDo(t *testing.T) {
	h := &captureHandler{}
	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error { return nil }}
	recovery.Logger = slog.New(h)

	stats := recovery.runRecovery([]string{t.TempDir()}, false, recovery.startupMinAge(), true)
	if stats.Scanned != 0 || stats.BareTmp != 0 || stats.Recovered != 0 || len(stats.Errors) != 0 {
		t.Fatalf("stats = %+v, want all zero", stats)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, entry := range h.logs {
		if entry.Level >= slog.LevelInfo {
			t.Fatalf("periodic scan with no leftovers logged at %v: %q %v", entry.Level, entry.Msg, entry.Attrs)
		}
	}
}

// 周期重扫只负责重试残留 sidecar，不重复执行 AutoRecovery 的启动 DB 补录：
// 后者是全表扫描，放进 10 分钟周期里没有新信息、只会增加 DB 压力。
func TestPeriodicRecoveryScanSkipsDBBackfill(t *testing.T) {
	called := false
	recovery := &IndexRecoveryTask{
		RunDBBackfill: true,
		Backfill: func() (RecoveryStats, error) {
			called = true
			return RecoveryStats{}, nil
		},
		EnqueueUpload: func(RecoveryItem) error { return nil },
	}
	recovery.runRecovery([]string{t.TempDir()}, false, recovery.startupMinAge(), true)
	if called {
		t.Fatal("periodic rerun must not repeat the startup DB backfill")
	}
	// 启动扫描（非周期）仍然要跑补录，确认上面的断言不是因为开关失效。
	recovery.runRecovery([]string{t.TempDir()}, false, recovery.startupMinAge(), false)
	if !called {
		t.Fatal("startup scan must still run the DB backfill")
	}
}
