package mp4

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeShiftTestIndex 造一个带任意扩展记录序列的 sidecar。
func writeShiftTestIndex(t *testing.T, path string, records []IndexRecord, exts func(w *IndexWriter) error) {
	t.Helper()
	w, err := CreateIndex(path, IndexHeader{Version: 1, MediaPath: "/tmp/media.mp4"})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	for _, r := range records {
		if err := w.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if exts != nil {
		if err := exts(w); err != nil {
			t.Fatalf("write extensions: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// SHIFT / SHIFT_DONE 必须能被读回，且最后一条决定状态。
func TestIndexOffsetShiftRecordsRoundTrip(t *testing.T) {
	t.Run("shift only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.m7sidx")
		writeShiftTestIndex(t, path, nil, func(w *IndexWriter) error {
			return w.OffsetShift(1000, 4096)
		})
		got, err := ReadIndex(path)
		if err != nil {
			t.Fatalf("ReadIndex: %v", err)
		}
		if got.OffsetShift != OffsetShiftPending {
			t.Fatalf("OffsetShift = %v, want pending", got.OffsetShift)
		}
		if got.ShiftPreLen != 1000 || got.ShiftInsertLen != 4096 {
			t.Fatalf("shift = (%d,%d), want (1000,4096)", got.ShiftPreLen, got.ShiftInsertLen)
		}
	})

	t.Run("shift then done", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.m7sidx")
		writeShiftTestIndex(t, path, nil, func(w *IndexWriter) error {
			if err := w.OffsetShift(1000, 4096); err != nil {
				return err
			}
			return w.OffsetShiftDone()
		})
		got, err := ReadIndex(path)
		if err != nil {
			t.Fatalf("ReadIndex: %v", err)
		}
		if got.OffsetShift != OffsetShiftApplied {
			t.Fatalf("OffsetShift = %v, want applied", got.OffsetShift)
		}
	})

	t.Run("done then shift", func(t *testing.T) {
		// 分片复用同一 sidecar 的极端顺序：后写的 SHIFT 才是当前状态。
		path := filepath.Join(t.TempDir(), "a.m7sidx")
		writeShiftTestIndex(t, path, nil, func(w *IndexWriter) error {
			if err := w.OffsetShift(1000, 4096); err != nil {
				return err
			}
			if err := w.OffsetShiftDone(); err != nil {
				return err
			}
			return w.OffsetShift(5000, 2048)
		})
		got, err := ReadIndex(path)
		if err != nil {
			t.Fatalf("ReadIndex: %v", err)
		}
		if got.OffsetShift != OffsetShiftPending || got.ShiftPreLen != 5000 || got.ShiftInsertLen != 2048 {
			t.Fatalf("last record must win: %+v", got)
		}
	})

	t.Run("no shift", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a.m7sidx")
		writeShiftTestIndex(t, path, nil, nil)
		got, err := ReadIndex(path)
		if err != nil {
			t.Fatalf("ReadIndex: %v", err)
		}
		if got.OffsetShift != OffsetShiftNone {
			t.Fatalf("OffsetShift = %v, want none", got.OffsetShift)
		}
	})
}

// 非法 paylen 的 SHIFT 记录属于确定性损坏，读取时标记为 uncertain，
// 但不得吞掉后续记录（状态不确定由调用方拒绝恢复，不是解析失败）。
func TestIndexOffsetShiftMalformedPayloadIsUncertain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.m7sidx")
	w, err := CreateIndex(path, IndexHeader{Version: 1, MediaPath: "/tmp/m.mp4"})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	// 手工写一条 payloadLen=8 的 type=3 扩展记录。
	var frame [IndexRecordSize]byte
	frame[0] = indexFlagExtension
	binary.BigEndian.PutUint16(frame[1:3], indexExtOffsetShift)
	binary.BigEndian.PutUint32(frame[3:7], 8)
	if _, err = w.buf.Write(frame[:]); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	if _, err = w.buf.Write(make([]byte, 8)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	if err = w.Append(IndexRecord{TrackID: 1, Size: 4, Offset: 48}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := ReadIndex(path)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.OffsetShift != OffsetShiftUncertain {
		t.Fatalf("OffsetShift = %v, want uncertain", got.OffsetShift)
	}
	if len(got.Records) != 1 {
		t.Fatalf("records after malformed extension = %d, want 1", len(got.Records))
	}
}

// 恢复判定表：每条分支都必须确定性，不确定状态一律返回错误。
func TestResolveOffsetShiftDecisionTable(t *testing.T) {
	const preLen, insertLen = int64(10_000), int64(4096)
	cases := []struct {
		name      string
		file      IndexFile
		mediaSize int64
		wantShift int64
		wantErr   bool
	}{
		{
			name:      "no shift record keeps old behaviour",
			file:      IndexFile{Records: []IndexRecord{{Offset: 48}}},
			mediaSize: 12_345,
			wantShift: 0,
		},
		{
			name:      "shift done uses index as written",
			file:      IndexFile{OffsetShift: OffsetShiftApplied, ShiftPreLen: preLen, ShiftInsertLen: insertLen},
			mediaSize: preLen + insertLen,
			wantShift: 0,
		},
		{
			name:      "shift done wins even for odd sizes",
			file:      IndexFile{OffsetShift: OffsetShiftApplied, ShiftPreLen: preLen, ShiftInsertLen: insertLen},
			mediaSize: 1,
			wantShift: 0,
		},
		{
			name:      "insert not applied leaves offsets untouched",
			file:      IndexFile{OffsetShift: OffsetShiftPending, ShiftPreLen: preLen, ShiftInsertLen: insertLen},
			mediaSize: preLen,
			wantShift: 0,
		},
		{
			name:      "insert applied shifts by insert length",
			file:      IndexFile{OffsetShift: OffsetShiftPending, ShiftPreLen: preLen, ShiftInsertLen: insertLen},
			mediaSize: preLen + insertLen,
			wantShift: insertLen,
		},
		{
			name:      "insert applied plus later growth still shifts",
			file:      IndexFile{OffsetShift: OffsetShiftPending, ShiftPreLen: preLen, ShiftInsertLen: insertLen},
			mediaSize: preLen + insertLen + 500,
			wantShift: insertLen,
		},
		{
			name:      "size between pre and post insert is uncertain",
			file:      IndexFile{OffsetShift: OffsetShiftPending, ShiftPreLen: preLen, ShiftInsertLen: insertLen},
			mediaSize: preLen + insertLen - 1,
			wantErr:   true,
		},
		{
			name:      "size below pre insert is uncertain",
			file:      IndexFile{OffsetShift: OffsetShiftPending, ShiftPreLen: preLen, ShiftInsertLen: insertLen},
			mediaSize: preLen - 1,
			wantErr:   true,
		},
		{
			name:      "malformed record is uncertain at any size",
			file:      IndexFile{OffsetShift: OffsetShiftUncertain},
			mediaSize: preLen,
			wantErr:   true,
		},
		{
			name:      "zero insert length is invalid",
			file:      IndexFile{OffsetShift: OffsetShiftPending, ShiftPreLen: preLen, ShiftInsertLen: 0},
			mediaSize: preLen,
			wantErr:   true,
		},
		{
			name:      "negative preLen is invalid",
			file:      IndexFile{OffsetShift: OffsetShiftPending, ShiftPreLen: -1, ShiftInsertLen: insertLen},
			mediaSize: preLen,
			wantErr:   true,
		},
		{
			// sidecar 不可信：正数相加溢出成负数时不能把「>= 负数」当成已生效。
			name: "overflowing preLen+insertLen is invalid",
			file: IndexFile{OffsetShift: OffsetShiftPending,
				ShiftPreLen: 1 << 62, ShiftInsertLen: 1 << 62},
			mediaSize: 1,
			wantErr:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shift, err := tc.file.ResolveOffsetShift(tc.mediaSize)
			if tc.wantErr {
				if !errors.Is(err, ErrOffsetShiftUncertain) {
					t.Fatalf("err = %v, want ErrOffsetShiftUncertain", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if shift != tc.wantShift {
				t.Fatalf("shift = %d, want %d", shift, tc.wantShift)
			}
		})
	}
}

// 平移必须产出新切片，且不影响原记录。
func TestRecordsWithOffsetShift(t *testing.T) {
	file := &IndexFile{Records: []IndexRecord{
		{TrackID: 1, Size: 4, Offset: 48},
		{TrackID: 1, Size: 4, Offset: 52},
	}}
	shifted := file.RecordsWithOffsetShift(4096)
	if len(shifted) != 2 {
		t.Fatalf("shifted len = %d", len(shifted))
	}
	if shifted[0].Offset != 48+4096 || shifted[1].Offset != 52+4096 {
		t.Fatalf("shifted offsets = %d,%d", shifted[0].Offset, shifted[1].Offset)
	}
	if file.Records[0].Offset != 48 {
		t.Fatalf("original records mutated: %+v", file.Records[0])
	}
	if got := file.RecordsWithOffsetShift(0); len(got) != 2 || got[0].Offset != 48 {
		t.Fatalf("zero shift must be a no-op, got %+v", got)
	}
}

// 旧版本读取器对未知扩展 type 只按 payloadLen 跳过——新 type 3/4 必须保持
// 向后兼容：写入后旧逻辑（等价于 switch 的 default）仍能读完后续样本记录。
func TestUnknownExtensionTypesStillReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.m7sidx")
	writeShiftTestIndex(t, path, []IndexRecord{{TrackID: 1, Size: 4, Offset: 48}}, func(w *IndexWriter) error {
		if err := w.writeExtension(999, []byte("future")); err != nil {
			return err
		}
		return w.OffsetShift(100, 4096)
	})
	got, err := ReadIndex(path)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if len(got.Records) != 1 || got.OffsetShift != OffsetShiftPending {
		t.Fatalf("got records=%d shift=%v", len(got.Records), got.OffsetShift)
	}
}

// 编译期确认 writer 落盘：SHIFT 写入后不经 Close 也应可被另一个读者看到
// （其语义是「InsertRange 前必须已持久化」）。
func TestOffsetShiftIsFlushedImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.m7sidx")
	w, err := CreateIndex(path, IndexHeader{Version: 1, MediaPath: "/tmp/m.mp4"})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	if err := w.OffsetShift(1234, 4096); err != nil {
		t.Fatalf("OffsetShift: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open sidecar: %v", err)
	}
	defer f.Close()
	idx, err := readIndexFrom(f)
	if err != nil {
		t.Fatalf("readIndexFrom: %v", err)
	}
	if idx.OffsetShift != OffsetShiftPending || idx.ShiftPreLen != 1234 {
		t.Fatalf("shift not flushed: %+v", idx)
	}
}
