package box

import (
	"encoding/binary"
	"io"
)

// aligned(8) class TrackRunBox extends FullBox('trun', version, tr_flags) {
//      unsigned int(32) sample_count;
//      // the following are optional fields
//      signed int(32) data_offset;
//       unsigned int(32) first_sample_flags;
//      // all fields in the following array are optional
//      {
//          unsigned int(32) sample_duration;
//          unsigned int(32) sample_size;
//          unsigned int(32) sample_flags
//          if (version == 0)
//          {
//              unsigned int(32) sample_composition_time_offset;
//          }
//          else
//          {
//              signed int(32) sample_composition_time_offset;
//          }
//      }[ sample_count ]
// }

const (
	TR_FLAG_DATA_OFFSET                  uint32 = 0x000001
	TR_FLAG_DATA_FIRST_SAMPLE_FLAGS      uint32 = 0x000004
	TR_FLAG_DATA_SAMPLE_DURATION         uint32 = 0x000100
	TR_FLAG_DATA_SAMPLE_SIZE             uint32 = 0x000200
	TR_FLAG_DATA_SAMPLE_FLAGS            uint32 = 0x000400
	TR_FLAG_DATA_SAMPLE_COMPOSITION_TIME uint32 = 0x000800
)

type TrackRunBox struct {
	SampleCount      uint32
	Dataoffset       int32
	FirstSampleFlags uint32
	EntryList        []TrunEntry
}

func NewTrackRunBox() *TrackRunBox {
	return &TrackRunBox{}
}

func (trun *TrackRunBox) Size(trunFlags uint32) uint64 {
	size := uint64(8) // box header
	size += 4         // version and flags
	size += 4         // sample count

	// data offset is always present if flag is set
	if trunFlags&TR_FLAG_DATA_OFFSET != 0 {
		size += 4
	}

	// first sample flags is present if flag is set
	if trunFlags&TR_FLAG_DATA_FIRST_SAMPLE_FLAGS != 0 {
		size += 4
	}

	// calculate size for each sample entry
	for i := 0; i < int(trun.SampleCount); i++ {
		// sample duration is present if flag is set
		if trunFlags&TR_FLAG_DATA_SAMPLE_DURATION != 0 {
			size += 4
		}
		// sample size is present if flag is set
		if trunFlags&TR_FLAG_DATA_SAMPLE_SIZE != 0 {
			size += 4
		}
		// sample flags is present if flag is set
		if trunFlags&TR_FLAG_DATA_SAMPLE_FLAGS != 0 {
			size += 4
		}
		// sample composition time offset is present if flag is set
		if trunFlags&TR_FLAG_DATA_SAMPLE_COMPOSITION_TIME != 0 {
			size += 4
		}
	}
	return size
}

func (trun *TrackRunBox) Decode(r io.Reader, size uint32, dataOffset int32) (offset int, err error) {
	var fullbox FullBox
	if offset, err = fullbox.Decode(r); err != nil {
		return
	}
	buf := make([]byte, size-12)
	if _, err = io.ReadFull(r, buf); err != nil {
		return
	}

	n := 0
	trun.SampleCount = binary.BigEndian.Uint32(buf[n:])
	n += 4

	trunFlags := uint32(fullbox.Flags[0])<<16 | uint32(fullbox.Flags[1])<<8 | uint32(fullbox.Flags[2])

	if trunFlags&TR_FLAG_DATA_OFFSET != 0 {
		trun.Dataoffset = int32(binary.BigEndian.Uint32(buf[n:]))
		n += 4
	} else {
		trun.Dataoffset = dataOffset
	}

	if trunFlags&TR_FLAG_DATA_FIRST_SAMPLE_FLAGS != 0 {
		trun.FirstSampleFlags = binary.BigEndian.Uint32(buf[n:])
		n += 4
	}

	trun.EntryList = make([]TrunEntry, trun.SampleCount)
	for i := 0; i < int(trun.SampleCount); i++ {
		if trunFlags&TR_FLAG_DATA_SAMPLE_DURATION != 0 {
			trun.EntryList[i].SampleDuration = binary.BigEndian.Uint32(buf[n:])
			n += 4
		}
		if trunFlags&TR_FLAG_DATA_SAMPLE_SIZE != 0 {
			trun.EntryList[i].SampleSize = binary.BigEndian.Uint32(buf[n:])
			n += 4
		}
		if trunFlags&TR_FLAG_DATA_SAMPLE_FLAGS != 0 {
			trun.EntryList[i].SampleFlags = binary.BigEndian.Uint32(buf[n:])
			n += 4
		}
		if trunFlags&TR_FLAG_DATA_SAMPLE_COMPOSITION_TIME != 0 {
			if fullbox.Version == 0 {
				trun.EntryList[i].SampleCompositionTimeOffset = int32(binary.BigEndian.Uint32(buf[n:]))
			} else {
				trun.EntryList[i].SampleCompositionTimeOffset = int32(binary.BigEndian.Uint32(buf[n:]))
			}
			n += 4
		}
	}

	offset += n
	return
}

func (trun *TrackRunBox) Encode(trunFlags uint32) (int, []byte) {
	// Always use version 1 for signed composition time offsets
	fullbox := NewFullBox(TypeTRUN, 1)
	fullbox.Box.Size = trun.Size(trunFlags)
	fullbox.Flags[0] = byte(trunFlags >> 16)
	fullbox.Flags[1] = byte(trunFlags >> 8)
	fullbox.Flags[2] = byte(trunFlags)
	offset, buf := fullbox.Encode()

	// Write sample count
	binary.BigEndian.PutUint32(buf[offset:], trun.SampleCount)
	offset += 4

	// Write data offset if present
	if trunFlags&TR_FLAG_DATA_OFFSET != 0 {
		// Write data offset as int32
		binary.BigEndian.PutUint32(buf[offset:], uint32(trun.Dataoffset))
		offset += 4
	}

	// Write first sample flags if present
	if trunFlags&TR_FLAG_DATA_FIRST_SAMPLE_FLAGS != 0 {
		binary.BigEndian.PutUint32(buf[offset:], trun.FirstSampleFlags)
		offset += 4
	}

	// Write sample entries in the correct order
	for i := 0; i < int(trun.SampleCount); i++ {
		// Write sample duration if present
		if trunFlags&TR_FLAG_DATA_SAMPLE_DURATION != 0 {
			binary.BigEndian.PutUint32(buf[offset:], trun.EntryList[i].SampleDuration)
			offset += 4
		}
		// Write sample size if present
		if trunFlags&TR_FLAG_DATA_SAMPLE_SIZE != 0 {
			binary.BigEndian.PutUint32(buf[offset:], trun.EntryList[i].SampleSize)
			offset += 4
		}
		// Write sample flags if present
		if trunFlags&TR_FLAG_DATA_SAMPLE_FLAGS != 0 {
			binary.BigEndian.PutUint32(buf[offset:], trun.EntryList[i].SampleFlags)
			offset += 4
		}
		// Write sample composition time offset if present
		if trunFlags&TR_FLAG_DATA_SAMPLE_COMPOSITION_TIME != 0 {
			// Version 1 uses signed int32 for composition time offset
			binary.BigEndian.PutUint32(buf[offset:], uint32(trun.EntryList[i].SampleCompositionTimeOffset))
			offset += 4
		}
	}

	return offset, buf
}
