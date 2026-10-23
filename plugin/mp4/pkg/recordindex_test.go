package mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndexWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tmp.m7sidx")
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	h := IndexHeader{
		Version:     1,
		StreamPath:  "live/a",
		MediaPath:   "/tmp/a.tmp",
		TargetName:  "a.mp4",
		StorageKey:  "record/a.mp4",
		FilePath:    "/tmp/a.mp4",
		StorageType: "s3",
		StartTime:   start,
		Tracks: []IndexTrack{
			{ID: 1, FourCC: "avc1", Width: 1920, Height: 1080, STSD: []byte{1, 2, 3, 4}},
			{ID: 2, FourCC: "mp4a", SampleRate: 8000, Channels: 1, STSD: []byte{9, 8, 7}},
		},
	}
	w, err := CreateIndex(p, h)
	if err != nil {
		t.Fatal(err)
	}
	recs := []IndexRecord{
		{Flags: 1, TrackID: 1, Size: 100, Offset: 48, DTS: 0, Duration: 40},
		{TrackID: 2, Size: 200, Offset: 148, DTS: 40, Duration: 40},
	}
	for _, r := range recs {
		if err := w.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(80, 2); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := ReadIndex(p)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Committed || len(f.Records) != 2 || f.Records[1].Offset != 148 {
		t.Fatalf("got %+v", f)
	}
	if f.Records[0] != recs[0] || f.Records[1] != recs[1] {
		t.Fatalf("records = %+v, want %+v", f.Records, recs)
	}
	hh := f.Header
	if hh.Version != 1 || hh.StreamPath != "live/a" || hh.MediaPath != "/tmp/a.tmp" ||
		hh.TargetName != "a.mp4" || hh.StorageKey != "record/a.mp4" ||
		hh.FilePath != "/tmp/a.mp4" || hh.StorageType != "s3" || !hh.StartTime.Equal(start) {
		t.Fatalf("header = %+v", hh)
	}
	if len(hh.Tracks) != 2 || hh.Tracks[0].FourCC != "avc1" || hh.Tracks[0].Width != 1920 ||
		hh.Tracks[0].Height != 1080 || string(hh.Tracks[0].STSD) != string([]byte{1, 2, 3, 4}) ||
		hh.Tracks[1].SampleRate != 8000 || hh.Tracks[1].Channels != 1 {
		t.Fatalf("tracks = %+v", hh.Tracks)
	}
	if f.MediaPath != "/tmp/a.tmp" {
		t.Fatalf("MediaPath = %q", f.MediaPath)
	}
}

// 半条记录被截断时，读取应返回已完整读到的记录且不报错。
func TestIndexTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tmp.m7sidx")
	w, err := CreateIndex(p, IndexHeader{Version: 1, MediaPath: "/tmp/a.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := w.Append(IndexRecord{Flags: 1, TrackID: 1, Size: 100, Offset: uint64(48 + i*100), DTS: uint32(i * 40), Duration: 40}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Commit(80, 2); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	headerLen := int(binary.BigEndian.Uint32(data[7:11]))
	recStart := 11 + headerLen

	// 截断在第二条记录中间。
	p2 := filepath.Join(dir, "b.tmp.m7sidx")
	if err := os.WriteFile(p2, data[:recStart+IndexRecordSize+10], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ReadIndex(p2)
	if err != nil {
		t.Fatalf("truncated record should not error: %v", err)
	}
	if len(f.Records) != 1 || f.Records[0].Offset != 48 || f.Committed {
		t.Fatalf("got %+v", f)
	}

	// 截断在 commit 扩展记录的载荷中间。
	p3 := filepath.Join(dir, "c.tmp.m7sidx")
	commitStart := recStart + 2*IndexRecordSize
	if err := os.WriteFile(p3, data[:commitStart+IndexRecordSize+5], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err = ReadIndex(p3)
	if err != nil {
		t.Fatalf("truncated commit should not error: %v", err)
	}
	if len(f.Records) != 2 || f.Committed {
		t.Fatalf("got %+v", f)
	}

	// 任意截断点都不得 panic，已读记录数单调不减。
	n := 0
	for i := 0; i <= len(data); i++ {
		f, _ := readIndexFrom(bytes.NewReader(data[:i]))
		if f == nil {
			continue
		}
		if len(f.Records) < n {
			t.Fatalf("records shrank at cut %d: %d < %d", i, len(f.Records), n)
		}
		n = len(f.Records)
	}
	if n != 2 {
		t.Fatalf("full data yields %d records, want 2", n)
	}
}

// 崩溃/掉电/预分配/S3 中转拷贝都可能留下零填充尾。23 个 0x00 恰好凑满一条
// 记录，但解码结果是 {trackID:0,size:0,offset:0} 的幽灵样本，必须拒绝：
// 读取应在零填充处停止，返回此前的记录且不报错(与截断同一契约)。
func TestIndexZeroFilledTail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tmp.m7sidx")
	w, err := CreateIndex(p, IndexHeader{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	first := IndexRecord{Flags: 1, TrackID: 1, Size: 100, Offset: 48, DTS: 0, Duration: 40}
	if err := w.Append(first); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}

	second := IndexRecord{TrackID: 1, Size: 100, Offset: 148, DTS: 40, Duration: 40}
	var secondFrame [IndexRecordSize]byte
	encodeIndexRecord(secondFrame[:], second)

	build := func(name string, tail ...[]byte) string {
		out := append([]byte(nil), data...)
		for _, b := range tail {
			out = append(out, b...)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, out, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 有效记录 + 23 个 0x00 + 另一条有效记录：必须停在零填充处。
	zero := make([]byte, IndexRecordSize)
	f, err := ReadIndex(build("zero.m7sidx", zero, secondFrame[:]))
	if err != nil {
		t.Fatalf("zero-filled tail should not error: %v", err)
	}
	if len(f.Records) != 1 || f.Records[0] != first {
		t.Fatalf("records = %+v, want exactly the first record", f.Records)
	}
	for _, r := range f.Records {
		if r.TrackID == 0 || r.Offset == 0 {
			t.Fatalf("ghost record leaked: %+v", r)
		}
	}

	// track ID 0 同样不是合法轨道：即使 size/offset 非零也应停止。
	var badTrack [IndexRecordSize]byte
	encodeIndexRecord(badTrack[:], IndexRecord{Size: 100, Offset: 148, DTS: 40, Duration: 40})
	f, err = ReadIndex(build("track0.m7sidx", badTrack[:], secondFrame[:]))
	if err != nil {
		t.Fatalf("track ID 0 should not error: %v", err)
	}
	if len(f.Records) != 1 || f.Records[0] != first {
		t.Fatalf("records = %+v, want exactly the first record", f.Records)
	}
}

// ReadIndex 只负责格式，不感知媒体长度；越界记录原样返回，由调用方截断。
func TestIndexReadDoesNotCheckMediaBounds(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tmp.m7sidx")
	w, err := CreateIndex(p, IndexHeader{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	// 故意越界：offset+size 远大于任何合理媒体长度。
	r := IndexRecord{TrackID: 1, Size: 1 << 20, Offset: 1 << 40, DTS: 7, Duration: 40}
	if err := w.Append(r); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	f, err := ReadIndex(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Records) != 1 || f.Records[0] != r {
		t.Fatalf("got %+v", f.Records)
	}
	if f.Committed {
		t.Fatal("uncommitted index reported committed")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIndexRenameAndRemove(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tmp.m7sidx")
	w, err := CreateIndex(p, IndexHeader{Version: 1, MediaPath: "/tmp/a.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(IndexRecord{TrackID: 1, Size: 10, Offset: 48}); err != nil {
		t.Fatal(err)
	}
	newPath := "/record/live/a/a.mp4"
	if err := w.Rename(newPath); err != nil {
		t.Fatal(err)
	}
	if err := w.Commit(80, 1); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	f, err := ReadIndex(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.MediaPath != newPath || f.Header.MediaPath != newPath {
		t.Fatalf("MediaPath = %q, header = %q", f.MediaPath, f.Header.MediaPath)
	}
	if len(f.Records) != 1 || !f.Committed {
		t.Fatalf("got %+v", f)
	}

	if err := RemoveIndex(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("index still exists: %v", err)
	}
	// 删除不存在的索引是幂等的。
	if err := RemoveIndex(p); err != nil {
		t.Fatalf("remove missing index: %v", err)
	}
}

func TestIndexFlushIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.tmp.m7sidx")
	w, err := CreateIndex(p, IndexHeader{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(IndexRecord{TrackID: 1, Size: 1, Offset: 48}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := w.Flush(); err != nil {
			t.Fatalf("flush #%d: %v", i, err)
		}
	}
	f, err := ReadIndex(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Records) != 1 {
		t.Fatalf("records = %d", len(f.Records))
	}
	// flush 之后仍可继续追加。
	if err := w.Append(IndexRecord{TrackID: 1, Size: 1, Offset: 49, DTS: 40}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	f, err = ReadIndex(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Records) != 2 {
		t.Fatalf("records = %d", len(f.Records))
	}
}

func TestIndexBadHeader(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadIndex(filepath.Join(dir, "missing.m7sidx")); err == nil {
		t.Fatal("missing file should error")
	}
	empty := filepath.Join(dir, "empty.m7sidx")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(empty); err == nil {
		t.Fatal("empty file should error")
	}
	badMagic := filepath.Join(dir, "magic.m7sidx")
	if err := os.WriteFile(badMagic, append([]byte("XXXXXXXXXXX"), []byte("{}")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(badMagic); err == nil {
		t.Fatal("bad magic should error")
	}
	badLen := filepath.Join(dir, "len.m7sidx")
	buf := append([]byte(IndexMagic), 0, 0, 0xFF, 0xFF)
	if err := os.WriteFile(badLen, append(buf, []byte("{}")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadIndex(badLen); err == nil {
		t.Fatal("oversized header length should error")
	}
}
