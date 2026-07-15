package box

import (
	"encoding/binary"
	"io"
)

// aligned(8) class ChunkOffsetBox
//     extends FullBox('stco', version = 0, 0) {
//         unsigned int(32) entry_count;
//         for (i=1; i <= entry_count; i++) {
//             unsigned int(32) chunk_offset;
//     }
// }
// aligned(8) class ChunkLargeOffsetBox
//     extends FullBox('co64', version = 0, 0) {
//         unsigned int(32) entry_count;
//         for (i=1; i <= entry_count; i++) {
//             unsigned int(64) chunk_offset;
//         }
// }

type STCOBox struct {
	FullBox
	Entries []uint64
}

type CO64Box STCOBox

func CreateSTCOBox(entries []uint64) *STCOBox {
	// 任一 chunk offset 超出 32 位范围时自动升级为 co64(64 位偏移),
	// 否则 >4GB 录像的偏移会被 uint32 截断,moov 内偏移全错、文件损坏。
	for _, e := range entries {
		if e > 0xFFFFFFFF {
			return (*STCOBox)(CreateCO64Box(entries))
		}
	}
	return &STCOBox{

		FullBox: FullBox{
			BaseBox: BaseBox{
				typ:  TypeSTCO,
				size: uint32(FullBoxLen + 4 + len(entries)*4),
			},
			Version: 0,
			Flags:   [3]byte{0, 0, 0},
		},

		Entries: entries,
	}
}

func CreateCO64Box(entries []uint64) *CO64Box {
	return &CO64Box{
		FullBox: FullBox{
			BaseBox: BaseBox{
				typ:  TypeCO64,
				size: uint32(FullBoxLen + 4 + len(entries)*8),
			},
		},

		Entries: entries,
	}
}

func (box *STCOBox) WriteTo(w io.Writer) (n int64, err error) {
	// CreateSTCOBox 检测到大偏移时会把 typ 升级为 co64,此处按类型分发写出格式。
	if box.typ == TypeCO64 {
		return (*CO64Box)(box).WriteTo(w)
	}
	buf := make([]byte, 4+len(box.Entries)*4)

	// Write entry count
	binary.BigEndian.PutUint32(buf[:4], uint32(len(box.Entries)))

	// Write entries
	for i, chunkOffset := range box.Entries {
		binary.BigEndian.PutUint32(buf[4+i*4:], uint32(chunkOffset))
	}

	_, err = w.Write(buf)
	return int64(len(buf)), err
}

func (box *CO64Box) WriteTo(w io.Writer) (n int64, err error) {
	buf := make([]byte, 4+len(box.Entries)*8)

	// Write entry count
	binary.BigEndian.PutUint32(buf[:4], uint32(len(box.Entries)))

	// Write entries
	for i, chunkOffset := range box.Entries {
		binary.BigEndian.PutUint64(buf[4+i*8:], chunkOffset)
	}

	_, err = w.Write(buf)
	return int64(len(buf)), err
}

func (box *STCOBox) Unmarshal(buf []byte) (IBox, error) {
	entryCount := binary.BigEndian.Uint32(buf[:4])
	box.Entries = make([]uint64, entryCount)

	if len(buf) < 4+int(entryCount)*4 {
		return nil, io.ErrShortBuffer
	}

	idx := 4
	for i := 0; i < int(entryCount); i++ {
		box.Entries[i] = uint64(binary.BigEndian.Uint32(buf[idx:]))
		idx += 4
	}
	return box, nil
}

func (box *CO64Box) Unmarshal(buf []byte) (IBox, error) {
	entryCount := binary.BigEndian.Uint32(buf[:4])
	box.Entries = make([]uint64, entryCount)

	if len(buf) < 4+int(entryCount)*8 {
		return nil, io.ErrShortBuffer
	}

	idx := 4
	for i := 0; i < int(entryCount); i++ {
		box.Entries[i] = binary.BigEndian.Uint64(buf[idx:])
		idx += 8
	}
	return box, nil
}

func init() {
	RegisterBox[*STCOBox](TypeSTCO)
	RegisterBox[*CO64Box](TypeCO64)
}
