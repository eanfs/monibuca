package box

import (
	"encoding/binary"
	"io"
	"time"
)

// aligned(8) class MovieHeaderBox extends FullBox(‘mvhd’, version, 0) {
// if (version==1) {
// 	unsigned int(64)  creation_time;
// 	unsigned int(64)  modification_time;
// 	unsigned int(32)  timescale;
// 	unsigned int(64)  duration;
//  } else { // version==0
// 	unsigned int(32)  creation_time;
// 	unsigned int(32)  modification_time;
// 	unsigned int(32)  timescale;
// 	unsigned int(32)  duration;
// }
// template int(32) rate = 0x00010000; // typically 1.0
// template int(16) volume = 0x0100; // typically, full volume const bit(16) reserved = 0;
// const unsigned int(32)[2] reserved = 0;
// template int(32)[9] matrix =
// { 0x00010000,0,0,0,0x00010000,0,0,0,0x40000000 };
// 	// Unity matrix
//  bit(32)[6]  pre_defined = 0;
//  unsigned int(32)  next_track_ID;
// }

type MovieHeaderBox struct {
	Creation_time     uint64
	Modification_time uint64
	Timescale         uint32
	Duration          uint64
	Rate              uint32
	Volume            uint16
	Matrix            [9]uint32
	Pre_defined       [6]uint32
	Next_track_ID     uint32
}

func NewMovieHeaderBox() *MovieHeaderBox {
	// MP4/QuickTime epoch starts from Jan 1, 1904
	// Add offset between Unix epoch (1970) and QuickTime epoch (1904)
	const mp4Epoch = 2082844800 // seconds between 1904 and 1970
	now := uint64(time.Now().Unix() + mp4Epoch)
	return &MovieHeaderBox{
		Creation_time:     now,
		Modification_time: now,
		Timescale:         1000,
		Rate:              0x00010000,
		Volume:            0x0100,
		Matrix:            [9]uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
	}
}

func (mvhd *MovieHeaderBox) Decode(r io.Reader, basebox *BasicBox) (offset int, err error) {
	var fullbox FullBox
	if offset, err = fullbox.Decode(r); err != nil {
		return 0, err
	}
	boxsize := 0
	if fullbox.Version == 0 {
		boxsize = 96
	} else {
		boxsize = 108
	}
	buf := make([]byte, boxsize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	n := 0
	if fullbox.Version == 1 {
		mvhd.Creation_time = binary.BigEndian.Uint64(buf[n:])
		n += 8
		mvhd.Modification_time = binary.BigEndian.Uint64(buf[n:])
		n += 8
		mvhd.Timescale = binary.BigEndian.Uint32(buf[n:])
		n += 4
		mvhd.Duration = binary.BigEndian.Uint64(buf[n:])
		n += 8
	} else {
		mvhd.Creation_time = uint64(binary.BigEndian.Uint32(buf[n:]))
		n += 4
		mvhd.Modification_time = uint64(binary.BigEndian.Uint32(buf[n:]))
		n += 4
		mvhd.Timescale = binary.BigEndian.Uint32(buf[n:])
		n += 4
		mvhd.Duration = uint64(binary.BigEndian.Uint32(buf[n:]))
		n += 4
	}
	mvhd.Rate = binary.BigEndian.Uint32(buf[n:])
	n += 4
	mvhd.Volume = binary.BigEndian.Uint16(buf[n:])
	n += 10

	for i, _ := range mvhd.Matrix {
		mvhd.Matrix[i] = binary.BigEndian.Uint32(buf[n:])
		n += 4
	}

	for i := 0; i < 6; i++ {
		mvhd.Pre_defined[i] = binary.BigEndian.Uint32(buf[n:])
		n += 4
	}
	mvhd.Next_track_ID = binary.BigEndian.Uint32(buf[n:])
	return n + 4 + offset, nil
}

func (mvhd *MovieHeaderBox) Encode() (int, []byte) {
	// Always use version 0 for better compatibility
	var fullbox = NewFullBox(TypeMVHD, 0)
	fullbox.Box.Size = FullBoxLen + 96 // version 0 size
	offset, buf := fullbox.Encode()

	// Version 0: all 32-bit values
	binary.BigEndian.PutUint32(buf[offset:], uint32(mvhd.Creation_time))
	offset += 4
	binary.BigEndian.PutUint32(buf[offset:], uint32(mvhd.Modification_time))
	offset += 4
	binary.BigEndian.PutUint32(buf[offset:], mvhd.Timescale)
	offset += 4
	binary.BigEndian.PutUint32(buf[offset:], uint32(mvhd.Duration))
	offset += 4

	binary.BigEndian.PutUint32(buf[offset:], mvhd.Rate)
	offset += 4
	binary.BigEndian.PutUint16(buf[offset:], mvhd.Volume)
	offset += 2
	offset += 10 // reserved

	for _, matrix := range mvhd.Matrix {
		binary.BigEndian.PutUint32(buf[offset:], matrix)
		offset += 4
	}

	offset += 24 // pre-defined
	binary.BigEndian.PutUint32(buf[offset:], mvhd.Next_track_ID)
	return offset + 4, buf
}

func MakeMvhdBox(trackid uint32, duration uint32) []byte {
	mvhd := NewMovieHeaderBox()
	mvhd.Next_track_ID = trackid
	mvhd.Duration = uint64(duration)
	_, mvhdbox := mvhd.Encode()
	return mvhdbox
}
