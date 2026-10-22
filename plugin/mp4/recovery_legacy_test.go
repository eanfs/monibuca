package plugin_mp4

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

// 取自 Task 0 现场样本的 SPS（Main/Level 4.0，1920x1080，VUI 25fps）。
// 用真实 SPS 才能覆盖 avcC 构造与 VUI 帧率解析；PPS 只需能让参数集完整。
const testLegacySPSHex = "674d0028e900f0044fcb0800001f4800061c102000"
const testLegacyPPSHex = "68ebe3cb22c0"

func legacyTestParamSets(t *testing.T) (sps, pps []byte) {
	t.Helper()
	var err error
	if sps, err = hex.DecodeString(testLegacySPSHex); err != nil {
		t.Fatalf("decode SPS: %v", err)
	}
	if pps, err = hex.DecodeString(testLegacyPPSHex); err != nil {
		t.Fatalf("decode PPS: %v", err)
	}
	return
}

// avccBytes 把若干个裸 NAL 拼成 AVCC 长度前缀形式。
func avccBytes(nals ...[]byte) []byte {
	var buf bytes.Buffer
	var length [4]byte
	for _, nal := range nals {
		binary.BigEndian.PutUint32(length[:], uint32(len(nal)))
		buf.Write(length[:])
		buf.Write(nal)
	}
	return buf.Bytes()
}

// halfWrittenTmp 用真实 muxer 的 init segment 造半成品 tmp：
// [ftyp 32][free 8][mdat 占位头 8] + payload，载荷起点固定 48。
func halfWrittenTmp(t *testing.T, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s3writer_test.tmp")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tmp: %v", err)
	}
	defer f.Close()
	m := mp4pkg.NewMuxer(0)
	if err = m.WriteInitSegment(f); err != nil {
		t.Fatalf("write init segment: %v", err)
	}
	if m.CurrentOffset != legacyPayloadOffset {
		t.Fatalf("payload offset = %d, want %d", m.CurrentOffset, legacyPayloadOffset)
	}
	if _, err = f.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	return path
}

type legacyTopBox struct {
	typ    string
	offset int64
	size   int64
}

// scanLegacyTopBoxes 顺序扫描顶层 box，支持 largesize。
func scanLegacyTopBoxes(t *testing.T, data []byte) []legacyTopBox {
	t.Helper()
	var boxes []legacyTopBox
	for pos := int64(0); pos+8 <= int64(len(data)); {
		size := int64(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		header := int64(8)
		if size == 1 {
			if pos+16 > int64(len(data)) {
				break
			}
			size = int64(binary.BigEndian.Uint64(data[pos+8:]))
			header = 16
		} else if size == 0 {
			size = int64(len(data)) - pos
		}
		if size < header || pos+size > int64(len(data)) {
			t.Fatalf("invalid top box %q size %d at %d", typ, size, pos)
		}
		boxes = append(boxes, legacyTopBox{typ: typ, offset: pos, size: size})
		pos += size
	}
	return boxes
}

func parseLegacyOutput(t *testing.T, path string) (data []byte, boxes []legacyTopBox, moov *box.MoovBox) {
	t.Helper()
	var err error
	if data, err = os.ReadFile(path); err != nil {
		t.Fatalf("read output: %v", err)
	}
	boxes = scanLegacyTopBoxes(t, data)
	for _, b := range boxes {
		if b.typ != "moov" {
			continue
		}
		parsed, err := box.ReadFrom(bytes.NewReader(data[b.offset : b.offset+b.size]))
		if err != nil {
			t.Fatalf("parse moov: %v", err)
		}
		var ok bool
		if moov, ok = parsed.(*box.MoovBox); !ok {
			t.Fatalf("moov parsed as %T", parsed)
		}
	}
	if moov == nil {
		t.Fatalf("no moov in output: %+v", boxes)
	}
	return
}

func TestRebuildFromMDATRecoversVideoOnly(t *testing.T) {
	sps, pps := legacyTestParamSets(t)
	idr := []byte{0x65, 0x88, 0x84, 0x00, 0x21}
	p1 := []byte{0x41, 0x88, 0x84, 0x00, 0x11}
	p2 := []byte{0x41, 0x88, 0x84, 0x00, 0x22}

	au1 := avccBytes(sps, pps, idr)
	au2 := avccBytes(p1)
	au3 := avccBytes(p2)
	mediaPath := halfWrittenTmp(t, bytes.Join([][]byte{au1, au2, au3}, nil))
	before, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "out.mp4")
	res, err := RebuildFromMDAT(mediaPath, LegacyOptions{FPS: 25, OutputPath: outPath})
	if err != nil {
		t.Fatalf("RebuildFromMDAT: %v", err)
	}
	if res.OutputPath != outPath {
		t.Errorf("OutputPath = %q, want %q", res.OutputPath, outPath)
	}
	if res.VideoFrames != 3 {
		t.Errorf("VideoFrames = %d, want 3", res.VideoFrames)
	}
	if res.AudioFrames != 0 || res.AudioRecovered {
		t.Errorf("audio = %d/%v, want 0/false", res.AudioFrames, res.AudioRecovered)
	}
	if res.DurationMs != 120 {
		t.Errorf("DurationMs = %d, want 120", res.DurationMs)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("expected at least one warning (audio not recovered)")
	}

	data, boxes, moov := parseLegacyOutput(t, outPath)
	if len(boxes) != 3 || boxes[0].typ != "ftyp" || boxes[1].typ != "moov" || boxes[2].typ != "mdat" {
		t.Fatalf("top boxes = %+v, want ftyp|moov|mdat", boxes)
	}
	if len(moov.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1", len(moov.Tracks))
	}
	stbl := moov.Tracks[0].MDIA.MINF.STBL
	if stbl.STSZ == nil || stbl.STSZ.SampleCount != 3 {
		t.Fatalf("stsz = %+v, want 3 samples", stbl.STSZ)
	}
	if stbl.STSS == nil || len(stbl.STSS.Entries) != 1 || stbl.STSS.Entries[0] != 1 {
		t.Errorf("stss = %+v, want [1]", stbl.STSS)
	}
	if stbl.STSD == nil || len(stbl.STSD.Entries) != 1 {
		t.Fatalf("stsd = %+v", stbl.STSD)
	}
	entry, ok := stbl.STSD.Entries[0].(*box.VisualSampleEntry)
	if !ok {
		t.Fatalf("video entry = %T", stbl.STSD.Entries[0])
	}
	if entry.Width != 1920 || entry.Height != 1080 {
		t.Errorf("dimensions = %dx%d, want 1920x1080", entry.Width, entry.Height)
	}
	if entry.ExtraData == nil || entry.ExtraData.Type() != box.TypeAVCC {
		t.Errorf("video extra data = %v, want avcC", entry.ExtraData)
	}

	samples := moov.Tracks[0].ParseSamples()
	want := [][]byte{au1, au2, au3}
	for i, sample := range samples {
		if sample.Size != len(want[i]) {
			t.Fatalf("sample %d size = %d, want %d", i, sample.Size, len(want[i]))
		}
		got := data[sample.Offset : sample.Offset+int64(sample.Size)]
		if !bytes.Equal(got, want[i]) {
			t.Errorf("sample %d bytes mismatch", i)
		}
	}

	after, err := os.ReadFile(mediaPath)
	if err != nil {
		t.Fatalf("re-read source: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("source tmp was modified")
	}
}

// 两个 slice 属于同一帧时不能拆成两帧：first_mb_in_slice > 0 的 slice 必须并入当前 AU。
func TestRebuildFromMDATGroupsMultiSliceFrame(t *testing.T) {
	sps, pps := legacyTestParamSets(t)
	idrSlice0 := []byte{0x65, 0x88, 0x84, 0x00, 0x01}
	idrSlice1 := []byte{0x65, 0x40, 0x84, 0x00, 0x02} // first_mb_in_slice = 1
	pSlice0 := []byte{0x41, 0x88, 0x84, 0x00, 0x03}

	payload := bytes.Join([][]byte{
		avccBytes(sps, pps, idrSlice0, idrSlice1),
		avccBytes(pSlice0),
	}, nil)
	mediaPath := halfWrittenTmp(t, payload)
	outPath := filepath.Join(t.TempDir(), "out.mp4")

	res, err := RebuildFromMDAT(mediaPath, LegacyOptions{FPS: 25, OutputPath: outPath})
	if err != nil {
		t.Fatalf("RebuildFromMDAT: %v", err)
	}
	if res.VideoFrames != 2 {
		t.Fatalf("VideoFrames = %d, want 2 (multi-slice frame must stay one sample)", res.VideoFrames)
	}
	_, _, moov := parseLegacyOutput(t, outPath)
	samples := moov.Tracks[0].ParseSamples()
	if len(samples) != 2 || samples[0].Size != len(avccBytes(sps, pps, idrSlice0, idrSlice1)) {
		t.Errorf("samples = %+v", samples)
	}
}

// 音频是启发式恢复：定长 G.711A 空洞可切帧，且必须显式告警。
func TestRebuildFromMDATRecoversG711Audio(t *testing.T) {
	sps, pps := legacyTestParamSets(t)
	idr := []byte{0x65, 0x88, 0x84, 0x00, 0x01}
	p := []byte{0x41, 0x88, 0x84, 0x00, 0x02}
	silence := bytes.Repeat([]byte{0xD5}, 320)
	payload := bytes.Join([][]byte{
		avccBytes(sps, pps, idr),
		silence,
		avccBytes(p),
		silence,
		avccBytes(p),
	}, nil)
	mediaPath := halfWrittenTmp(t, payload)
	outPath := filepath.Join(t.TempDir(), "out.mp4")

	res, err := RebuildFromMDAT(mediaPath, LegacyOptions{FPS: 25, OutputPath: outPath})
	if err != nil {
		t.Fatalf("RebuildFromMDAT: %v", err)
	}
	if !res.AudioRecovered || res.AudioFrames != 2 || res.AudioCodec != "pcm_alaw" {
		t.Fatalf("audio = recovered:%v frames:%d codec:%q, want true/2/pcm_alaw", res.AudioRecovered, res.AudioFrames, res.AudioCodec)
	}
	_, _, moov := parseLegacyOutput(t, outPath)
	if len(moov.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2", len(moov.Tracks))
	}
	foundAudio := false
	for _, tr := range moov.Tracks {
		stbl := tr.MDIA.MINF.STBL
		if stbl.STSD == nil || len(stbl.STSD.Entries) == 0 {
			continue
		}
		if stbl.STSD.Entries[0].Type() == box.TypeALAW {
			foundAudio = true
			if stbl.STSZ == nil || stbl.STSZ.SampleCount != 2 {
				t.Errorf("audio stsz count = %v, want 2", stbl.STSZ)
			}
		}
	}
	if !foundAudio {
		t.Errorf("no alaw track in output")
	}
	if len(res.Warnings) == 0 {
		t.Errorf("heuristic audio recovery must surface a warning")
	}
}

func TestRebuildFromMDATDryRunWritesNothing(t *testing.T) {
	sps, pps := legacyTestParamSets(t)
	payload := avccBytes(sps, pps, []byte{0x65, 0x88, 0x84, 0x00, 0x01})
	mediaPath := halfWrittenTmp(t, payload)
	outPath := filepath.Join(t.TempDir(), "should-not-exist.mp4")

	res, err := RebuildFromMDAT(mediaPath, LegacyOptions{FPS: 25, OutputPath: outPath, DryRun: true})
	if err != nil {
		t.Fatalf("RebuildFromMDAT: %v", err)
	}
	if res.VideoFrames != 1 {
		t.Errorf("VideoFrames = %d, want 1", res.VideoFrames)
	}
	if _, err = os.Stat(outPath); !os.IsNotExist(err) {
		t.Errorf("dry run wrote %s (err=%v)", outPath, err)
	}
	if res.OutputPath != "" {
		t.Errorf("dry run OutputPath = %q, want empty", res.OutputPath)
	}
}

func TestRebuildFromMDATRejectsMalformedInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.tmp")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xAB}, 256), 0o644); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	if _, err := RebuildFromMDAT(path, LegacyOptions{}); err == nil {
		t.Fatalf("expected error for non-MP4 input")
	}
}

// 尾部截断/噪声不应让整份录像失败：恢复出可用的帧并告警。
func TestRebuildFromMDATIgnoresTrailingGarbage(t *testing.T) {
	sps, pps := legacyTestParamSets(t)
	payload := bytes.Join([][]byte{
		avccBytes(sps, pps, []byte{0x65, 0x88, 0x84, 0x00, 0x01}),
		avccBytes([]byte{0x41, 0x88, 0x84, 0x00, 0x02}),
		bytes.Repeat([]byte{0x7F}, 100),
	}, nil)
	mediaPath := halfWrittenTmp(t, payload)
	outPath := filepath.Join(t.TempDir(), "out.mp4")

	res, err := RebuildFromMDAT(mediaPath, LegacyOptions{FPS: 25, OutputPath: outPath})
	if err != nil {
		t.Fatalf("RebuildFromMDAT: %v", err)
	}
	if res.VideoFrames != 2 {
		t.Errorf("VideoFrames = %d, want 2", res.VideoFrames)
	}
	if len(res.Warnings) == 0 {
		t.Errorf("expected warning about resync holes")
	}
}

// H.264 VUI 应给出精确帧率（time_scale/(2*num_units_in_tick)），而不是整数近似。
func TestParseH264VUITiming(t *testing.T) {
	sps, _ := legacyTestParamSets(t)
	fps, ok := legacyH264VUIFPS(sps)
	if !ok {
		t.Fatalf("VUI timing not found")
	}
	if fps != 25 {
		t.Errorf("fps = %v, want 25", fps)
	}
	if _, ok := legacyH264VUIFPS([]byte{0x67, 0x42, 0x00, 0x1e}); ok {
		t.Errorf("expected failure on truncated SPS")
	}
}
