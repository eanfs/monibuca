package mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

// 时间戳回退时 Duration 不应被 uint32 回绕污染成天量值。
func TestAddSampleEntryTimestampRegression(t *testing.T) {
	track := &Track{Timescale: 1000}
	track.AddSampleEntry(box.Sample{Timestamp: 5})
	track.AddSampleEntry(box.Sample{Timestamp: 25})
	if track.Duration != 20 {
		t.Fatalf("normal delta: duration = %d, want 20", track.Duration)
	}
	// 回退:25 -> 3
	track.AddSampleEntry(box.Sample{Timestamp: 3})
	if track.Duration != 21 {
		t.Fatalf("regressed delta: duration = %d, want 21 (20+1)", track.Duration)
	}
	if d := track.Samplelist[1].Duration; d != 1 {
		t.Fatalf("regressed sample duration = %d, want 1", d)
	}
}

func newProgressiveMuxerWithOffsets(offsets []uint64) (*Muxer, *Track) {
	m := NewMuxer(0)
	track := m.AddTrack(box.MP4_CODEC_G711A)
	track.ICodecCtx = codec.NewPCMACtx()
	for i, off := range offsets {
		track.Samplelist = append(track.Samplelist, box.Sample{
			Timestamp: uint32(i * 20),
			Offset:    int64(off),
		})
	}
	m.MakeMoov() // 模拟 WriteTrailer 在 Start 阶段已构建过 moov-at-tail
	return m, track
}

// 小偏移:PrepareFrontMoov 一轮收敛,偏移右移 moov 尺寸,仍为 stco。
func TestPrepareFrontMoovSmallOffsets(t *testing.T) {
	m, track := newProgressiveMuxerWithOffsets([]uint64{100, 600})
	moov, err := m.PrepareFrontMoov()
	if err != nil {
		t.Fatalf("PrepareFrontMoov: %v", err)
	}
	var buf bytes.Buffer
	n, err := box.WriteTo(&buf, moov)
	if err != nil {
		t.Fatalf("serialize moov: %v", err)
	}
	if n != int64(moov.Size()) {
		t.Fatalf("moov wrote %d != declared %d", n, moov.Size())
	}
	if !bytes.Contains(buf.Bytes(), []byte("stco")) || bytes.Contains(buf.Bytes(), []byte("co64")) {
		t.Fatalf("small offsets should stay stco")
	}
	want := uint64(100) + moov.Size()
	if track.STCO.Entries[0] != want {
		t.Fatalf("shifted offset = %d, want %d", track.STCO.Entries[0], want)
	}
}

// 偏移右移后跨过 4GB:stco 升级 co64、moov 变大,迭代平移直到收敛,
// 最终偏移 = 原偏移 + 最终 moov 尺寸(回归保护:旧实现偏移按旧尺寸平移导致错位)。
func TestPrepareFrontMoovCO64Upgrade(t *testing.T) {
	base := uint64(0xFFFFFFFF - 200) // 平移前 < 4GB,平移后必然跨过
	m, track := newProgressiveMuxerWithOffsets([]uint64{base, base + 50})
	moov, err := m.PrepareFrontMoov()
	if err != nil {
		t.Fatalf("PrepareFrontMoov: %v", err)
	}
	var buf bytes.Buffer
	n, err := box.WriteTo(&buf, moov)
	if err != nil {
		t.Fatalf("serialize moov: %v", err)
	}
	if n != int64(moov.Size()) {
		t.Fatalf("moov wrote %d != declared %d", n, moov.Size())
	}
	if !bytes.Contains(buf.Bytes(), []byte("co64")) || bytes.Contains(buf.Bytes(), []byte("stco")) {
		t.Fatalf("crossed-4GB offsets should be co64")
	}
	want := base + moov.Size()
	if track.STCO.Entries[0] != want {
		t.Fatalf("shifted offset = %#x, want %#x (base %#x + final moov size %d)",
			track.STCO.Entries[0], want, base, moov.Size())
	}
	if track.STCO.Entries[0] <= 0xFFFFFFFF {
		t.Fatalf("offset %#x should exceed 32-bit range", track.STCO.Entries[0])
	}
}

// scanTopLevelBoxes 顺序解析文件顶层 box,返回 (type, offset, size) 列表。
type topBox struct {
	typ    string
	offset int
	size   int
}

func scanTopLevelBoxes(data []byte) (boxes []topBox) {
	for i := 0; i+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[i : i+4]))
		typ := string(data[i+4 : i+8])
		if size == 1 && i+16 <= len(data) {
			size = int(binary.BigEndian.Uint64(data[i+8 : i+16]))
		}
		if size < 8 || i+size > len(data) {
			break
		}
		boxes = append(boxes, topBox{typ: typ, offset: i, size: size})
		i += size
	}
	return
}

// fMP4 录制必须在首个 moof 之前写出含 mvex/trex 的 init moov,
// 否则文件没有轨道描述整档不可播(回归保护:旧实现只写 ftyp)。
func TestFMP4InitSegmentMoov(t *testing.T) {
	path := filepath.Join(t.TempDir(), "frag.mp4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()

	m := NewMuxer(FLAG_FRAGMENT)
	if err = m.WriteInitSegment(f); err != nil {
		t.Fatalf("write init segment: %v", err)
	}
	track := m.AddTrack(box.MP4_CODEC_G711A)
	track.ICodecCtx = codec.NewPCMACtx()

	for i := 0; i < 3; i++ {
		sample := box.Sample{Timestamp: uint32(i * 20), KeyFrame: true}
		sample.PushOne([]byte{0x01, 0x02, 0x03, 0x04})
		if err = m.WriteSample(f, track, sample); err != nil {
			t.Fatalf("write sample %d: %v", i, err)
		}
	}
	if err = m.WriteTrailer(f); err != nil {
		t.Fatalf("write trailer: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	boxes := scanTopLevelBoxes(data)
	if len(boxes) == 0 {
		t.Fatalf("no boxes parsed")
	}
	if boxes[0].typ != "ftyp" {
		t.Fatalf("first box = %s, want ftyp", boxes[0].typ)
	}
	moovIdx, moofIdx := -1, -1
	for i, b := range boxes {
		if b.typ == "moov" && moovIdx < 0 {
			moovIdx = i
		}
		if b.typ == "moof" && moofIdx < 0 {
			moofIdx = i
		}
	}
	if moovIdx < 0 {
		t.Fatalf("no moov (init segment) found; boxes: %+v", boxes)
	}
	if moofIdx < 0 {
		t.Fatalf("no moof found; boxes: %+v", boxes)
	}
	if moovIdx > moofIdx {
		t.Fatalf("moov (idx %d) must precede first moof (idx %d)", moovIdx, moofIdx)
	}
	moovBytes := data[boxes[moovIdx].offset : boxes[moovIdx].offset+boxes[moovIdx].size]
	if !bytes.Contains(moovBytes, []byte("mvex")) || !bytes.Contains(moovBytes, []byte("trex")) {
		t.Fatalf("init moov must contain mvex/trex")
	}

	// 有 ffprobe 时做真实可播性校验:能解析出流即证明 init segment 有效。
	if ffprobe, lookErr := exec.LookPath("ffprobe"); lookErr == nil {
		out, probeErr := exec.Command(ffprobe, "-v", "error", "-show_streams", "-of", "json", path).CombinedOutput()
		if probeErr != nil {
			t.Fatalf("ffprobe validation failed: %v\n%s", probeErr, out)
		}
		if !bytes.Contains(out, []byte("codec_type")) {
			t.Fatalf("ffprobe found no streams:\n%s", out)
		}
	}
}
