package mp4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"m7s.live/v5/plugin/mp4/pkg/box"
)

// restoreTestSTSD 造一个完整的 stsd box，模拟录制时写进索引头的字节。
// 恢复路径不解析码流，只把这段字节原样注入 stbl，因此 avcC 内容无需真实可解。
func restoreTestSTSD(t *testing.T, video bool) []byte {
	t.Helper()
	var entry box.IBox
	if video {
		entry = box.CreateVisualSampleEntry(box.TypeAVC1, 320, 240,
			box.CreateDataBox(box.TypeAVCC, []byte{0x01, 0x64, 0x00, 0x1f, 0xff, 0xe0, 0x00}))
	} else {
		entry = box.CreateAudioSampleEntry(box.TypeALAW, 1, 16, 8000, nil)
	}
	var buf bytes.Buffer
	if _, err := box.WriteTo(&buf, box.CreateSTSDBox(entry)); err != nil {
		t.Fatalf("serialize stsd: %v", err)
	}
	return buf.Bytes()
}

// buildHalfWrittenMedia 用真实 muxer 的 init segment 造一个未 finalize 的
// progressive MP4：[ftyp(32)][free(8)][mdat 占位头(8)] + payload，载荷起点 48。
func buildHalfWrittenMedia(t *testing.T, payload []byte) (path string, mediaSize, payloadStart int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "half-written.tmp")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create media: %v", err)
	}
	defer f.Close()
	m := NewMuxer(0)
	if err = m.WriteInitSegment(f); err != nil {
		t.Fatalf("write init segment: %v", err)
	}
	payloadStart = m.CurrentOffset
	if payloadStart != 48 {
		t.Fatalf("payload start = %d, want 48", payloadStart)
	}
	if _, err = f.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatalf("stat media: %v", err)
	}
	return path, st.Size(), payloadStart
}

// parseRestoreOutput 顺序扫描顶层 box，并解析 moov。
func parseRestoreOutput(t *testing.T, data []byte) (boxes []topBox, moov *box.MoovBox) {
	t.Helper()
	boxes = scanTopLevelBoxes(data)
	for _, b := range boxes {
		if b.typ != "moov" {
			continue
		}
		parsed, err := box.ReadFrom(bytes.NewReader(data[b.offset : b.offset+b.size]))
		if err != nil {
			t.Fatalf("parse moov: %v", err)
		}
		m, ok := parsed.(*box.MoovBox)
		if !ok {
			t.Fatalf("moov parsed as %T, want *box.MoovBox", parsed)
		}
		moov = m
	}
	if moov == nil {
		t.Fatalf("no moov in output; boxes: %+v", boxes)
	}
	return
}

// findTopBox 返回指定类型的第一个顶层 box。
func findTopBox(t *testing.T, boxes []topBox, typ string) topBox {
	t.Helper()
	for _, b := range boxes {
		if b.typ == typ {
			return b
		}
	}
	t.Fatalf("no %s box in output; boxes: %+v", typ, boxes)
	return topBox{}
}

// 恢复后 moov 可被 box 解析，stsz 样本数/大小与索引一致，stco 指向输出
// ftyp|moov|mdat 布局中的真实载荷位置。
func TestRestoreFromIndexBuildsPlayableMoov(t *testing.T) {
	sample0 := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	sample1 := []byte{0x01, 0x02, 0x03, 0x04, 0x05}
	payload := append(append([]byte(nil), sample0...), sample1...)
	mediaPath, mediaSize, payloadStart := buildHalfWrittenMedia(t, payload)

	header := IndexHeader{Tracks: []IndexTrack{{
		ID: 1, FourCC: "avc1", Width: 320, Height: 240,
		STSD: restoreTestSTSD(t, true),
	}}}
	records := []IndexRecord{
		{Flags: indexFlagKeyFrame, TrackID: 1, Size: uint32(len(sample0)), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		{TrackID: 1, Size: uint32(len(sample1)), Offset: uint64(payloadStart) + uint64(len(sample0)), DTS: 40, Duration: 40},
	}

	m := NewMuxer(0)
	if _, err := m.RestoreFromIndex(header, records, mediaSize); err != nil {
		t.Fatalf("RestoreFromIndex: %v", err)
	}
	var out bytes.Buffer
	if err := m.WriteMoovOnly(&out); err != nil {
		t.Fatalf("WriteMoovOnly: %v", err)
	}

	// 调用方按恢复契约拷贝载荷。
	start, size, ok := m.RestorePayloadRange()
	if !ok {
		t.Fatalf("RestorePayloadRange: not restored")
	}
	raw, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	if start != payloadStart || size != mediaSize-payloadStart {
		t.Fatalf("payload range = [%d,%d), want [%d,%d)", start, start+size, payloadStart, mediaSize)
	}
	out.Write(raw[start : start+size])

	boxes, moov := parseRestoreOutput(t, out.Bytes())
	if len(boxes) != 3 || boxes[0].typ != "ftyp" || boxes[1].typ != "moov" || boxes[2].typ != "mdat" {
		t.Fatalf("output layout = %+v, want ftyp|moov|mdat", boxes)
	}
	mdat := findTopBox(t, boxes, "mdat")
	if wantPayload := mdat.offset + 8; wantPayload != int(m.CurrentOffset) {
		t.Fatalf("mdat payload offset = %d, muxer CurrentOffset = %d", wantPayload, m.CurrentOffset)
	}
	if got := len(out.Bytes()) - (mdat.offset + 8); got != len(payload) {
		t.Fatalf("mdat payload = %d bytes, want %d", got, len(payload))
	}
	if !bytes.Equal(out.Bytes()[mdat.offset+8:], payload) {
		t.Fatalf("payload bytes were not copied verbatim")
	}

	if len(moov.Tracks) != 1 {
		t.Fatalf("moov tracks = %d, want 1", len(moov.Tracks))
	}
	stbl := moov.Tracks[0].MDIA.MINF.STBL
	if stbl.STSZ.SampleCount != 2 {
		t.Fatalf("stsz sample count = %d, want 2", stbl.STSZ.SampleCount)
	}
	if len(stbl.STSZ.EntrySizelist) != 2 ||
		stbl.STSZ.EntrySizelist[0] != uint32(len(sample0)) ||
		stbl.STSZ.EntrySizelist[1] != uint32(len(sample1)) {
		t.Fatalf("stsz sizes = %v, want [%d %d]", stbl.STSZ.EntrySizelist, len(sample0), len(sample1))
	}
	if stbl.STSD == nil || len(stbl.STSD.Entries) != 1 || stbl.STSD.Entries[0].Type() != box.TypeAVC1 {
		t.Fatalf("stsd was not injected verbatim: %+v", stbl.STSD)
	}
	if stbl.STSS == nil || len(stbl.STSS.Entries) != 1 || stbl.STSS.Entries[0] != 1 {
		t.Fatalf("stss should mark sample 1 as keyframe, got %+v", stbl.STSS)
	}

	// 两个样本在源文件里连续，按 ISO 规则合并为同一个 chunk：
	// stco 只含 chunk 起点，stsc 声明该 chunk 有 2 个样本。
	wantPayloadOffset := uint64(mdat.offset + 8)
	if len(stbl.STCO.Entries) != 1 || stbl.STCO.Entries[0] != wantPayloadOffset {
		t.Fatalf("stco entries = %v, want [%d]", stbl.STCO.Entries, wantPayloadOffset)
	}
	if len(stbl.STSC.Entries) != 1 || stbl.STSC.Entries[0].SamplesPerChunk != 2 {
		t.Fatalf("stsc entries = %+v, want one chunk with 2 samples", stbl.STSC.Entries)
	}
	samples := moov.Tracks[0].ParseSamples()
	if len(samples) != 2 || int64(samples[0].Offset) != int64(wantPayloadOffset) ||
		int64(samples[1].Offset) != int64(wantPayloadOffset)+int64(len(sample0)) {
		t.Fatalf("parsed samples = %+v, want offsets at payload start", samples)
	}
}

// 越界记录（Offset+Size 超过媒体长度）必须被丢弃：只保留越界前的有效前缀，
// 返回 ErrIndexTruncated，且恢复输出不得因该记录被撑大。
func TestRestoreFromIndexTruncatesOverrun(t *testing.T) {
	payload := []byte{0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF} // 6 字节
	mediaPath, mediaSize, payloadStart := buildHalfWrittenMedia(t, payload)

	header := IndexHeader{Tracks: []IndexTrack{{
		ID: 1, FourCC: "avc1", Width: 320, Height: 240,
		STSD: restoreTestSTSD(t, true),
	}}}
	records := []IndexRecord{
		{TrackID: 1, Size: 4, Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		// 越界：52+8=60 > mediaSize=54
		{TrackID: 1, Size: 8, Offset: uint64(payloadStart) + 4, DTS: 40, Duration: 40},
	}

	m := NewMuxer(0)
	_, err := m.RestoreFromIndex(header, records, mediaSize)
	if !errors.Is(err, ErrIndexTruncated) {
		t.Fatalf("RestoreFromIndex error = %v, want ErrIndexTruncated", err)
	}
	track := m.Tracks[1]
	if track == nil || len(track.Samplelist) != 1 {
		t.Fatalf("valid prefix not kept: %+v", track)
	}

	var out bytes.Buffer
	if err := m.WriteMoovOnly(&out); err != nil {
		t.Fatalf("WriteMoovOnly: %v", err)
	}
	start, size, ok := m.RestorePayloadRange()
	if !ok || start != payloadStart || size != mediaSize-payloadStart {
		t.Fatalf("payload range = [%d,%d) ok=%v, want [%d,%d)", start, start+size, ok, payloadStart, mediaSize)
	}
	raw, err := os.ReadFile(mediaPath)
	if err != nil || int64(len(raw)) != mediaSize {
		t.Fatalf("media file unexpectedly changed: %v (len %d)", err, len(raw))
	}
	// 载荷区间由媒体实际长度决定：被丢弃的尾部记录指向的残留字节
	// 可以留在 mdat 里，但不能被 stsz/stco 引用。
	out.Write(raw[start : start+size])

	boxes, moov := parseRestoreOutput(t, out.Bytes())
	mdat := findTopBox(t, boxes, "mdat")
	stbl := moov.Tracks[0].MDIA.MINF.STBL
	if stbl.STSZ.SampleCount != 1 {
		t.Fatalf("stsz sample count = %d, want 1 (overrun record dropped)", stbl.STSZ.SampleCount)
	}
	if len(stbl.STCO.Entries) != 1 {
		t.Fatalf("stco entries = %v, want 1", stbl.STCO.Entries)
	}
	payloadOffset := uint64(mdat.offset + 8)
	if stbl.STCO.Entries[0] != payloadOffset {
		t.Fatalf("stco[0] = %d, want payload offset %d", stbl.STCO.Entries[0], payloadOffset)
	}
	if stbl.STSZ.SampleSize != 4 {
		t.Fatalf("stsz constant sample size = %d, want 4", stbl.STSZ.SampleSize)
	}
	// 唯一被接受的样本必须完整落在输出内（不因越界记录把文件撑大/越界引用）。
	if end := payloadOffset + uint64(stbl.STSZ.SampleSize); end > uint64(out.Len()) {
		t.Fatalf("accepted sample ends at %d beyond output %d", end, out.Len())
	}
	if out.Len() != int(payloadOffset)+int(size) {
		t.Fatalf("output length = %d, want %d (payload range must not be extended)", out.Len(), int(payloadOffset)+int(size))
	}
}

// 源文件 >4GiB：平移后的偏移仍可能超出 32 位，stco 必须升级 co64，
// 且 moov 变大后偏移要再次平移直到收敛（与 PrepareFrontMoov 同类回归）。
func TestRestoreFromIndexCO64Upgrade(t *testing.T) {
	const payloadStart = int64(48)
	const farOffset = uint64(0x1_0000_1000) // 略超 4GiB，保证载荷本身也需 largesize
	header := IndexHeader{Tracks: []IndexTrack{{
		ID: 1, FourCC: "avc1", Width: 320, Height: 240,
		STSD: restoreTestSTSD(t, true),
	}}}
	records := []IndexRecord{
		{TrackID: 1, Size: 4, Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		{TrackID: 1, Size: 4, Offset: farOffset, DTS: 40, Duration: 40},
	}
	mediaSize := int64(farOffset) + 4

	m := NewMuxer(0)
	if _, err := m.RestoreFromIndex(header, records, mediaSize); err != nil {
		t.Fatalf("RestoreFromIndex: %v", err)
	}
	var out bytes.Buffer
	if err := m.WriteMoovOnly(&out); err != nil {
		t.Fatalf("WriteMoovOnly: %v", err)
	}
	// 载荷 >4GiB 时 mdat 头是 16 字节 largesize 形式，顶层扫描因缺少
	// 载荷会在 mdat 处停下，这里直接用 muxer 记录的载荷起点校验。
	boxes, moov := parseRestoreOutput(t, out.Bytes())
	moovBox := findTopBox(t, boxes, "moov")
	wantStart := uint64(moovBox.offset + moovBox.size + 16)
	if uint64(m.CurrentOffset) != wantStart {
		t.Fatalf("payload start = %d, want %d", m.CurrentOffset, wantStart)
	}
	hdr := out.Bytes()[wantStart-16 : wantStart]
	if binary.BigEndian.Uint32(hdr) != 1 || string(hdr[4:8]) != "mdat" {
		t.Fatalf("mdat header = %x, want largesize form", hdr)
	}
	if got := binary.BigEndian.Uint64(hdr[8:]); got != uint64(mediaSize)-uint64(payloadStart)+16 {
		t.Fatalf("largesize = %d, want %d", got, uint64(mediaSize)-uint64(payloadStart)+16)
	}

	stbl := moov.Tracks[0].MDIA.MINF.STBL
	if stbl.STCO == nil || stbl.STCO.Type() != box.TypeCO64 {
		t.Fatalf("expected co64 for >4GiB offsets, got %v", stbl.STCO)
	}
	if stbl.STCO.Entries[0] != wantStart {
		t.Fatalf("stco[0] = %d, want %d", stbl.STCO.Entries[0], wantStart)
	}
	wantFar := wantStart + (farOffset - uint64(payloadStart))
	if stbl.STCO.Entries[1] != wantFar {
		t.Fatalf("stco[1] = %d, want %d", stbl.STCO.Entries[1], wantFar)
	}
	if stbl.STCO.Entries[1] <= 0xFFFFFFFF {
		t.Fatalf("far offset %#x should stay above 32-bit range", stbl.STCO.Entries[1])
	}
}

// moov 中按 track id 查找 trak（map 迭代顺序不定，不能按下标断言）。
func findMoovTrack(t *testing.T, moov *box.MoovBox, id uint32) *box.TrakBox {
	t.Helper()
	for _, tr := range moov.Tracks {
		if tr.TKHD.TrackID == id {
			return tr
		}
	}
	return nil
}

// 索引头声明了轨道但没有任何记录：该轨不能以只有 stsd 的残缺 stbl 写进 moov，
// 否则严格解析器会整档拒绝，连累有好样本的轨道。
func TestRestoreFromIndexOmitsEmptyTrack(t *testing.T) {
	sample := []byte{0xCA, 0xFE, 0xBA, 0xBE}
	_, mediaSize, payloadStart := buildHalfWrittenMedia(t, sample)

	header := IndexHeader{Tracks: []IndexTrack{
		{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: restoreTestSTSD(t, true)},
		{ID: 2, FourCC: "alaw", SampleRate: 8000, Channels: 1, STSD: restoreTestSTSD(t, false)},
	}}
	records := []IndexRecord{
		{TrackID: 1, Size: uint32(len(sample)), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		// 轨道 2 一条记录都没有。
	}

	m := NewMuxer(0)
	if _, err := m.RestoreFromIndex(header, records, mediaSize); err != nil {
		t.Fatalf("RestoreFromIndex: %v", err)
	}
	var out bytes.Buffer
	if err := m.WriteMoovOnly(&out); err != nil {
		t.Fatalf("WriteMoovOnly: %v", err)
	}
	_, moov := parseRestoreOutput(t, out.Bytes())
	if len(moov.Tracks) != 1 {
		t.Fatalf("moov tracks = %d, want 1 (empty track must be omitted)", len(moov.Tracks))
	}
	trak := findMoovTrack(t, moov, 1)
	if trak == nil {
		t.Fatalf("track 1 missing from moov")
	}
	stbl := trak.MDIA.MINF.STBL
	if stbl.STSD == nil || stbl.STSZ == nil || stbl.STTS == nil || stbl.STSC == nil || stbl.STCO == nil {
		t.Fatalf("track 1 stbl is incomplete: %+v", stbl)
	}
}

// 唯一一条记录越界时没有任何可用样本：必须返回显式错误，且不得进入已恢复状态，
// 以免调用方用一个几十字节的空壳 mp4 覆盖原始 .tmp。
func TestRestoreFromIndexNoUsableSamples(t *testing.T) {
	payload := []byte{0x00, 0x01, 0x02, 0x03}
	_, mediaSize, _ := buildHalfWrittenMedia(t, payload)

	header := IndexHeader{Tracks: []IndexTrack{{
		ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: restoreTestSTSD(t, true),
	}}}
	records := []IndexRecord{
		{TrackID: 1, Size: 8, Offset: uint64(mediaSize) + 100, DTS: 0, Duration: 40},
	}

	m := NewMuxer(0)
	_, err := m.RestoreFromIndex(header, records, mediaSize)
	if !errors.Is(err, ErrNoUsableSamples) {
		t.Fatalf("RestoreFromIndex error = %v, want ErrNoUsableSamples", err)
	}
	if _, _, ok := m.RestorePayloadRange(); ok {
		t.Fatalf("muxer must not report a restored payload after ErrNoUsableSamples")
	}
	if err := m.WriteMoovOnly(&bytes.Buffer{}); err == nil {
		t.Fatalf("WriteMoovOnly must fail when there are no usable samples")
	}
}

// rewriteIndexRecords/rebuildIndex 会按轨道分组重写记录，offset 不再单调。
// 首个越界记录之后的合法记录（这里是整条音轨）不能被 break 连带丢弃。
func TestRestoreFromIndexNonMonotonicKeepsLaterTracks(t *testing.T) {
	audio0 := bytes.Repeat([]byte{0x11}, 10)
	audio1 := bytes.Repeat([]byte{0x22}, 10)
	payload := append(append([]byte(nil), audio0...), audio1...)
	mediaPath, mediaSize, payloadStart := buildHalfWrittenMedia(t, payload)

	header := IndexHeader{Tracks: []IndexTrack{
		{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: restoreTestSTSD(t, true)},
		{ID: 2, FourCC: "alaw", SampleRate: 8000, Channels: 1, STSD: restoreTestSTSD(t, false)},
	}}
	records := []IndexRecord{
		// 轨道 1 分组在前，且越界。
		{TrackID: 1, Size: 8, Offset: uint64(mediaSize) + 100, DTS: 0, Duration: 40},
		// 轨道 2 分组在后，仍然完全合法。
		{TrackID: 2, Size: uint32(len(audio0)), Offset: uint64(payloadStart), DTS: 0, Duration: 20},
		{TrackID: 2, Size: uint32(len(audio1)), Offset: uint64(payloadStart) + uint64(len(audio0)), DTS: 20, Duration: 20},
	}

	m := NewMuxer(0)
	_, err := m.RestoreFromIndex(header, records, mediaSize)
	if !errors.Is(err, ErrIndexTruncated) {
		t.Fatalf("RestoreFromIndex error = %v, want ErrIndexTruncated", err)
	}
	if track := m.Tracks[1]; track == nil || len(track.Samplelist) != 0 {
		t.Fatalf("overrun track 1 should be empty, got %+v", track)
	}
	if track := m.Tracks[2]; track == nil || len(track.Samplelist) != 2 {
		t.Fatalf("track 2 valid records were dropped: %+v", track)
	}

	var out bytes.Buffer
	if err := m.WriteMoovOnly(&out); err != nil {
		t.Fatalf("WriteMoovOnly: %v", err)
	}
	start, size, ok := m.RestorePayloadRange()
	if !ok || start != payloadStart || size != mediaSize-payloadStart {
		t.Fatalf("payload range = [%d,%d) ok=%v, want [%d,%d)", start, start+size, ok, payloadStart, mediaSize)
	}
	raw, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read media: %v", err)
	}
	out.Write(raw[start : start+size])

	_, moov := parseRestoreOutput(t, out.Bytes())
	if len(moov.Tracks) != 1 {
		t.Fatalf("moov tracks = %d, want 1 (empty track 1 omitted)", len(moov.Tracks))
	}
	trak := findMoovTrack(t, moov, 2)
	if trak == nil {
		t.Fatalf("track 2 missing from moov")
	}
	if got := trak.MDIA.MINF.STBL.STSZ.SampleCount; got != 2 {
		t.Fatalf("track 2 stsz sample count = %d, want 2", got)
	}
}

// 音轨恢复：stsd 原样注入、tkhd 音量、smhd 与样本表均正确。
func TestRestoreFromIndexAudioTrack(t *testing.T) {
	sample := bytes.Repeat([]byte{0x55}, 320)
	_, mediaSize, payloadStart := buildHalfWrittenMedia(t, sample)

	header := IndexHeader{Tracks: []IndexTrack{{
		ID: 2, FourCC: "alaw", SampleRate: 8000, Channels: 1,
		STSD: restoreTestSTSD(t, false),
	}}}
	records := []IndexRecord{
		{TrackID: 2, Size: uint32(len(sample)), Offset: uint64(payloadStart), DTS: 0, Duration: 20},
	}

	m := NewMuxer(0)
	if _, err := m.RestoreFromIndex(header, records, mediaSize); err != nil {
		t.Fatalf("RestoreFromIndex: %v", err)
	}
	var out bytes.Buffer
	if err := m.WriteMoovOnly(&out); err != nil {
		t.Fatalf("WriteMoovOnly: %v", err)
	}
	_, moov := parseRestoreOutput(t, out.Bytes())
	if len(moov.Tracks) != 1 {
		t.Fatalf("moov tracks = %d, want 1", len(moov.Tracks))
	}
	trak := moov.Tracks[0]
	if trak.TKHD.TrackID != 2 {
		t.Fatalf("tkhd track id = %d, want 2", trak.TKHD.TrackID)
	}
	if trak.TKHD.Volume != 0x0100 {
		t.Fatalf("audio tkhd volume = %#x, want 0x100", trak.TKHD.Volume)
	}
	if trak.MDIA.MINF.SMHD == nil || trak.MDIA.MINF.VMHD != nil {
		t.Fatalf("audio track must use smhd, got %+v", trak.MDIA.MINF)
	}
	stbl := trak.MDIA.MINF.STBL
	if stbl.STSD == nil || len(stbl.STSD.Entries) != 1 || stbl.STSD.Entries[0].Type() != box.TypeALAW {
		t.Fatalf("stsd was not injected verbatim: %+v", stbl.STSD)
	}
	if stbl.STSZ.SampleCount != 1 {
		t.Fatalf("stsz sample count = %d, want 1", stbl.STSZ.SampleCount)
	}
}

// 索引里的 STSD 是不可信输入：伪造的 box 声明尺寸会让 box.ReadFrom 用无符号
// 运算算负载长度(size-8)，size<8 时回绕成 ~4.29GB 的分配(OOM)，size==8 的
// FullBox 会在 payload[0] 越界 panic。恢复入口必须在交给 box 解析前拦下，
// 且任何输入都不得 panic。size==1 的 largesize 形式同样会下溢。
func TestRestoreTrackRejectsInvalidSTSD(t *testing.T) {
	endian32 := func(v uint32) []byte {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], v)
		return b[:]
	}
	endian64 := func(v uint64) []byte {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], v)
		return b[:]
	}
	stsdHead := func(size uint32) []byte {
		return append(endian32(size), []byte("stsd")...)
	}
	largeHead := func(size uint64) []byte {
		return append(append(endian32(1), []byte("stsd")...), endian64(size)...)
	}
	// fullPayload 是 stsd 头部之后的合法负载：version/flags(4) + entry_count(4)。
	fullPayload := func() []byte { return []byte{0, 0, 0, 0, 0, 0, 0, 0} }

	cases := []struct {
		name string
		stsd []byte
	}{
		{"empty", nil},
		{"short_header", []byte{0, 0, 0, 16, 's', 't', 's'}},
		{"size_zero", stsdHead(0)},
		{"size_four", stsdHead(4)},
		// size==8 是 FullBox：payload 为空，box.ReadFrom 里 payload[0] 越界。
		{"size_eight_fullbox", stsdHead(8)},
		// size==12 只够 version/flags，entry_count 仍缺 4 字节。
		{"size_twelve_missing_entry_count", append(stsdHead(12), fullPayload()...)},
		{"size_gt_len", stsdHead(0x100)},
		{"size_max_uint32", stsdHead(0xFFFFFFFF)},
		// 16 字节(恢复入口最小合法) + entry_count=2^32-1：不能变成 68GB 分配。
		{"size16_huge_entry_count", append(stsdHead(16), []byte{0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}...)},
		// size==1 是 largesize 形式，8 字节尺寸小于 16 会让 make 下溢。
		{"largesize_zero", largeHead(0)},
		{"largesize_eight", largeHead(8)},
		{"largesize_sixteen_empty_payload", largeHead(16)},
		{"largesize_gt_len", append(largeHead(0x100), fullPayload()...)},
		// largesize 本身被截断，读不满 8 字节。
		{"largesize_truncated", []byte{0, 0, 0, 1, 's', 't', 's', 'd', 0, 0}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("restoreTrackFromIndex panicked: %v", r)
				}
			}()
			it := IndexTrack{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: c.stsd}
			track, err := restoreTrackFromIndex(it)
			if err == nil {
				t.Fatalf("expected error, got track %+v", track)
			}
			if track != nil {
				t.Fatalf("track returned alongside error: %+v", track)
			}
		})
	}
}

// 合法 STSD 必须仍能恢复：边界校验不能误伤录制路径写出的真实索引。
func TestRestoreTrackAcceptsValidSTSD(t *testing.T) {
	video := restoreTestSTSD(t, true)
	track, err := restoreTrackFromIndex(IndexTrack{
		ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: video,
	})
	if err != nil {
		t.Fatalf("valid video stsd rejected: %v", err)
	}
	if track.STSD == nil || len(track.STSD.Entries) != 1 || track.STSD.Entries[0].Type() != box.TypeAVC1 {
		t.Fatalf("stsd not injected verbatim: %+v", track.STSD)
	}
	if track.restoreWidth != 320 || track.restoreHeight != 240 {
		t.Fatalf("dimensions = %dx%d, want 320x240", track.restoreWidth, track.restoreHeight)
	}

	audio := restoreTestSTSD(t, false)
	track, err = restoreTrackFromIndex(IndexTrack{ID: 2, FourCC: "alaw", STSD: audio})
	if err != nil {
		t.Fatalf("valid audio stsd rejected: %v", err)
	}
	if !track.restoreAudio {
		t.Fatal("audio track not flagged as audio")
	}
}

// 恢复入口的 stsd 上界必须有测试钉住：只有下界检查时，size==len+1 这类
// "多声明 1 字节"仍会放行到 box 层（那里靠剩余量兜底，但恢复入口不该依赖它）。
func TestValidateSTSDBoundsRejectsOverrun(t *testing.T) {
	// 常规形式：16 字节合法；声明 17 > 实际 16 必须拒绝。
	regular := func(declared uint32) []byte {
		b := append([]byte{0, 0, 0, 0, 's', 't', 's', 'd'}, make([]byte, 8)...)
		binary.BigEndian.PutUint32(b[:4], declared)
		return b
	}
	if err := validateSTSDBounds(IndexTrack{ID: 1, STSD: regular(16)}); err != nil {
		t.Fatalf("size == len should pass: %v", err)
	}
	if err := validateSTSDBounds(IndexTrack{ID: 1, STSD: regular(17)}); err == nil {
		t.Fatal("size == len+1 must be rejected")
	}

	// largesize 形式：24 字节合法；声明 25 > 实际 24 必须拒绝。
	large := func(declared uint64) []byte {
		b := append([]byte{0, 0, 0, 1, 's', 't', 's', 'd'}, make([]byte, 16)...)
		binary.BigEndian.PutUint64(b[8:16], declared)
		return b
	}
	if err := validateSTSDBounds(IndexTrack{ID: 1, STSD: large(24)}); err != nil {
		t.Fatalf("largesize == len should pass: %v", err)
	}
	if err := validateSTSDBounds(IndexTrack{ID: 1, STSD: large(25)}); err == nil {
		t.Fatal("largesize == len+1 must be rejected")
	}
}

// 索引头声明了 video+audio，但音频记录全部越界：音轨被静默丢弃时调用方无从
// 得知，恢复结果看起来"成功"却没有声音。RestoreFromIndex 必须在报告里列出
// 被丢弃的轨道，调用方才能告警并避免回写 AudioCodec。
func TestRestoreFromIndexReportsDroppedTracks(t *testing.T) {
	sample := bytes.Repeat([]byte{0x11}, 8)
	_, mediaSize, payloadStart := buildHalfWrittenMedia(t, sample)

	header := IndexHeader{Tracks: []IndexTrack{
		{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: restoreTestSTSD(t, true)},
		{ID: 2, FourCC: "alaw", SampleRate: 8000, Channels: 1, STSD: restoreTestSTSD(t, false)},
	}}
	records := []IndexRecord{
		{TrackID: 1, Size: uint32(len(sample)), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
		// 音轨记录全部越界。
		{TrackID: 2, Size: 16, Offset: uint64(mediaSize) + 100, DTS: 0, Duration: 20},
		{TrackID: 2, Size: 16, Offset: uint64(mediaSize) + 200, DTS: 20, Duration: 20},
	}

	m := NewMuxer(0)
	report, err := m.RestoreFromIndex(header, records, mediaSize)
	if !errors.Is(err, ErrIndexTruncated) {
		t.Fatalf("RestoreFromIndex error = %v, want ErrIndexTruncated", err)
	}
	if !report.Truncated {
		t.Fatalf("report.Truncated = false, want true alongside ErrIndexTruncated")
	}
	if len(report.DroppedTrackIDs) != 1 || report.DroppedTrackIDs[0] != 2 {
		t.Fatalf("DroppedTrackIDs = %v, want [2]", report.DroppedTrackIDs)
	}
	if track := m.Tracks[2]; track == nil || len(track.Samplelist) != 0 {
		t.Fatalf("audio track must stay empty, got %+v", track)
	}

	var out bytes.Buffer
	if err := m.WriteMoovOnly(&out); err != nil {
		t.Fatalf("WriteMoovOnly: %v", err)
	}
	_, moov := parseRestoreOutput(t, out.Bytes())
	if len(moov.Tracks) != 1 || moov.Tracks[0].TKHD.TrackID != 1 {
		t.Fatalf("moov tracks = %+v, want only video track 1", moov.Tracks)
	}
}

// 每一条记录都能落到轨道上时不得报告丢弃。
func TestRestoreFromIndexReportsNoDroppedTracks(t *testing.T) {
	sample := bytes.Repeat([]byte{0x22}, 4)
	_, mediaSize, payloadStart := buildHalfWrittenMedia(t, sample)
	header := IndexHeader{Tracks: []IndexTrack{
		{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: restoreTestSTSD(t, true)},
	}}
	records := []IndexRecord{
		{TrackID: 1, Size: uint32(len(sample)), Offset: uint64(payloadStart), DTS: 0, Duration: 40},
	}

	m := NewMuxer(0)
	report, err := m.RestoreFromIndex(header, records, mediaSize)
	if err != nil {
		t.Fatalf("RestoreFromIndex: %v", err)
	}
	if len(report.DroppedTrackIDs) != 0 || report.Truncated || len(report.DamagedTrackIDs) != 0 {
		t.Fatalf("report = %+v, want zero report", report)
	}
}

// stsd 结构性损坏（entry_count 与实际条目数不符）无法通过重试修复。恢复入口
// 必须拿到明确错误（包装 ErrSTSDCorrupt）并在报告里标出损坏的轨道，便于调用
// 方 markFailed 并记录日志。
func TestRestoreFromIndexReportsCorruptSTSD(t *testing.T) {
	// size=16 的 stsd：version/flags(4) + entry_count=1，但没有任何 entry。
	corrupt := []byte{0, 0, 0, 16, 's', 't', 's', 'd', 0, 0, 0, 0, 0, 0, 0, 1}
	header := IndexHeader{Tracks: []IndexTrack{
		{ID: 1, FourCC: "avc1", Width: 320, Height: 240, STSD: restoreTestSTSD(t, true)},
		{ID: 2, FourCC: "alaw", SampleRate: 8000, Channels: 1, STSD: corrupt},
	}}

	m := NewMuxer(0)
	report, err := m.RestoreFromIndex(header, nil, -1)
	if err == nil {
		t.Fatal("expected error for structurally corrupt stsd")
	}
	if !errors.Is(err, box.ErrSTSDCorrupt) {
		t.Fatalf("error = %v, want box.ErrSTSDCorrupt", err)
	}
	if len(report.DamagedTrackIDs) != 1 || report.DamagedTrackIDs[0] != 2 {
		t.Fatalf("DamagedTrackIDs = %v, want [2]", report.DamagedTrackIDs)
	}
}
