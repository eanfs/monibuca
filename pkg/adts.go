package pkg

import (
	"bytes"
	"fmt"
	"time"

	"github.com/deepch/vdk/codec/aacparser"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
)

var _ IAVFrame = (*ADTS)(nil)

type ADTS struct {
	DTS time.Duration
	util.RecyclableMemory
}

func (A *ADTS) Parse(old codec.ICodecCtx, f *AVFrame) (new codec.ICodecCtx, err error) {
	if old == nil {
		var ctx = &codec.AACCtx{}
		var reader = A.NewReader()
		var adts []byte
		adts, err = reader.ReadBytes(7)
		if err != nil {
			return
		}
		var hdrlen, framelen, samples int
		ctx.Config, hdrlen, framelen, samples, err = aacparser.ParseADTSHeader(adts)
		if err != nil {
			return
		}
		b := &bytes.Buffer{}
		aacparser.WriteMPEG4AudioConfig(b, ctx.Config)
		ctx.ConfigBytes = b.Bytes()
		new = ctx
		if false {
			println("ADTS", "hdrlen", hdrlen, "framelen", framelen, "samples", samples, "config", ctx.Config)
		}
		// track.Info("ADTS", "hdrlen", hdrlen, "framelen", framelen, "samples", samples)
	} else {
		new = old
	}
	return
}

func (ADTS) ConvertCtx(ctx codec.ICodecCtx) (codec.ICodecCtx, error) {
	return ctx.GetBase(), nil
}

func (A *ADTS) Demux(ctx codec.ICodecCtx) (any, error) {
	var reader = A.NewReader()
	err := reader.Skip(7)
	var mem util.Memory
	reader.Range(mem.AppendOne)
	return mem, err
}

func (A *ADTS) Mux(ctx codec.ICodecCtx, frame *AVFrame) {
	A.InitRecycleIndexes(1)
	A.DTS = frame.Timestamp * 90 / time.Millisecond
	aacCtx, ok := ctx.GetBase().(*codec.AACCtx)
	if !ok {
		A.Append(frame.Raw.(util.Memory).Buffers...)
		return
	}
	adts := A.NextN(7)
	raw := frame.Raw.(util.Memory)
	aacparser.FillADTSHeader(adts, aacCtx.Config, raw.Size/aacCtx.GetSampleSize(), raw.Size)
	A.Append(raw.Buffers...)
}

func (A *ADTS) GetTimestamp() time.Duration {
	return A.DTS * time.Millisecond / 90
}

func (A *ADTS) GetCTS() time.Duration {
	return 0
}

func (A *ADTS) GetSize() int {
	return A.Size
}

func (A *ADTS) String() string {
	return fmt.Sprintf("ADTS{size:%d}", A.Size)
}
