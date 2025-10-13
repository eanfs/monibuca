package hls

import (
	"io"

	"github.com/langhuihui/gomem"
	"m7s.live/v5/pkg/codec"
	mpegts "m7s.live/v5/pkg/format/ts"
	"m7s.live/v5/pkg/util"
)

type TsInMemory struct {
	PMT util.Buffer
	gomem.RecyclableMemory
}

func (ts *TsInMemory) WritePMTPacket(audio, video codec.FourCC) {
	ts.PMT.Reset()
	mpegts.WritePMTPacket(&ts.PMT, video, audio)
}

func (ts *TsInMemory) WriteTo(w io.Writer) (int64, error) {
	w.Write(mpegts.DefaultPATPacket)
	w.Write(ts.PMT)
	return ts.RecyclableMemory.WriteTo(w)
}
