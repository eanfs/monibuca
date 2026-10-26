package plugin_mp4

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// shiftMediaHead 模拟 fallocate(INSERT_RANGE)：在文件头插入 insertLen 个零字节，
// 原有内容整体后移。返回插入后的文件长度。
func shiftMediaHead(t *testing.T, path string, insertLen int) int64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	out := make([]byte, insertLen+len(data))
	copy(out[insertLen:], data)
	if err = os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("rewrite shifted media: %v", err)
	}
	return int64(len(out))
}

// writeRecoverySidecarWithShift 重写 sidecar：样本记录 + SHIFT 扩展记录 + commit。
// 用公开 API 重建（CreateIndex 会截断），与生产端 writeExtension 等价。
func writeRecoverySidecarWithShift(t *testing.T, sidecar string, header mp4pkg.IndexHeader,
	records []mp4pkg.IndexRecord, preLen, insertLen int64) {
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
	if err = w.OffsetShift(preLen, insertLen); err != nil {
		t.Fatalf("append offset shift: %v", err)
	}
	if err = w.Commit(80, uint64(len(records))); err != nil {
		t.Fatalf("commit index: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}
}

// assertRecoveredPayload 校验恢复产物的 mdat 载荷逐字节等于原始媒体载荷。
// 这是「结构合法但内容全错」的唯一有效反证：只断言可解析/样本数不够。
func assertRecoveredPayload(t *testing.T, output string, want []byte) {
	t.Helper()
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read recovered output: %v", err)
	}
	boxes := scanRecoveryTopBoxes(data)
	if len(boxes) < 3 || boxes[2].typ != "mdat" {
		t.Fatalf("recovered layout = %+v, want ftyp|moov|mdat", boxes)
	}
	mdat := boxes[2]
	got := data[mdat.offset+8:]
	if !bytes.Equal(got, want) {
		t.Fatalf("recovered payload = %x, want %x", got, want)
	}
}

// Important 1：INSERT_RANGE 已把 mdat 后移、索引还是旧偏移时，恢复必须把
// 记录偏移 +insertLen 后再建 moov，载荷逐字节正确；否则会产出结构合法但
// 内容错位的 MP4 且报告成功。
func TestRecoveryAppliesOffsetShiftAfterInsertRange(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	mediaPath, sidecar, records := buildRecoveryFixtureWithStorage(t, dir, "s3writer_shift.tmp", payload, "s3")
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("stat media: %v", err)
	}
	preLen := info.Size()
	const insertLen = 4096

	// 模拟「INSERT_RANGE 已生效、rewriteIndexRecords 未执行」：媒体后移，
	// 索引仍是旧偏移，sidecar 末尾带一条 SHIFT。
	mediaSize := shiftMediaHead(t, mediaPath, insertLen)
	idx, err := mp4pkg.ReadIndex(sidecar)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	writeRecoverySidecarWithShift(t, sidecar, idx.Header, records, preLen, insertLen)
	ageRecoveryFiles(t, mediaPath, sidecar)

	var items []RecoveryItem
	recovery := &IndexRecoveryTask{EnqueueUpload: func(item RecoveryItem) error {
		items = append(items, item)
		return nil
	}}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	if len(items) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(items))
	}
	// 交给上传/入库的记录必须是平移后的坐标，与产物一致。
	if got, want := items[0].Records[0].Offset, records[0].Offset+insertLen; got != want {
		t.Fatalf("enqueued offset = %d, want %d", got, want)
	}

	output := filepath.Join(dir, "s3writer_shift.mp4")
	assertRecoveredPlayable(t, output, 2)
	assertRecoveredPayload(t, output, payload)
	if mediaSize != preLen+insertLen {
		t.Fatalf("fixture media size = %d, want %d", mediaSize, preLen+insertLen)
	}
}

// Important 1：文件长度既不是插入前也不是插入后（截断/部分执行）→ 状态不确定，
// 必须拒绝恢复：保留 sidecar（不进 .failed）、不产出错位 MP4。
func TestRecoveryRefusesUncertainOffsetShift(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x0A, 0x0B, 0x0C, 0x0D}
	mediaPath, sidecar, records := buildRecoveryFixtureWithStorage(t, dir, "s3writer_uncertain.tmp", payload, "s3")
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("stat media: %v", err)
	}
	preLen := info.Size()
	const insertLen = 4096

	// 只后移一部分：模拟 INSERT_RANGE 与截断之间的中间状态。
	shiftMediaHead(t, mediaPath, insertLen/2)
	idx, err := mp4pkg.ReadIndex(sidecar)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	writeRecoverySidecarWithShift(t, sidecar, idx.Header, records, preLen, insertLen)
	ageRecoveryFiles(t, mediaPath, sidecar)

	enqueued := 0
	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error {
		enqueued++
		return nil
	}}
	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("RunOnce must report the refused recovery")
	}
	if stats.Failed != 1 || stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	// RunOnce 只聚合计数，具体原因在 stats.Errors 里——必须是平移状态不确定。
	if len(stats.Errors) != 1 || !errors.Is(stats.Errors[0], mp4pkg.ErrOffsetShiftUncertain) {
		t.Fatalf("stats.Errors = %v, want ErrOffsetShiftUncertain", stats.Errors)
	}
	if enqueued != 0 {
		t.Fatalf("uncertain state must not be enqueued, got %d", enqueued)
	}
	if _, err = os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar must be kept for manual handling: %v", err)
	}
	if _, err = os.Stat(sidecar + failedSuffix); !os.IsNotExist(err) {
		t.Fatal("uncertain state must not be marked .failed")
	}
	if _, err = os.Stat(filepath.Join(dir, "s3writer_uncertain.mp4")); !os.IsNotExist(err) {
		t.Fatal("uncertain state must not produce an output mp4")
	}
}

// INSERT_RANGE 尚未生效（文件长度仍是 preLen）时，索引偏移保持原样。
func TestRecoveryWithoutInsertRangeKeepsOffsets(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x11, 0x22, 0x33, 0x44}
	mediaPath, sidecar, records := buildRecoveryFixtureWithStorage(t, dir, "s3writer_noshift.tmp", payload, "s3")
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatalf("stat media: %v", err)
	}
	idx, err := mp4pkg.ReadIndex(sidecar)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	writeRecoverySidecarWithShift(t, sidecar, idx.Header, records, info.Size(), 4096)
	ageRecoveryFiles(t, mediaPath, sidecar)

	var items []RecoveryItem
	recovery := &IndexRecoveryTask{EnqueueUpload: func(item RecoveryItem) error {
		items = append(items, item)
		return nil
	}}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 {
		t.Fatalf("stats = %+v, want recovered=1", stats)
	}
	if len(items) != 1 || items[0].Records[0].Offset != records[0].Offset {
		t.Fatalf("offsets must stay unchanged: %+v", items)
	}
	output := filepath.Join(dir, "s3writer_noshift.mp4")
	assertRecoveredPlayable(t, output, 2)
	assertRecoveredPayload(t, output, payload)
}
