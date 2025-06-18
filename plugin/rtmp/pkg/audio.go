package rtmp

import (
	"time"

	"github.com/deepch/vdk/codec/aacparser"
	. "m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
)

var _ IAVFrame = (*Audio)(nil)

type Audio struct {
	RTMPData
}

func (avcc *Audio) Parse(old codec.ICodecCtx, f *AVFrame) (ctx codec.ICodecCtx, err error) {
	reader := avcc.NewReader()
	var b byte
	b, err = reader.ReadByte()
	if err != nil {
		return
	}
	switch b & 0b1111_0000 >> 4 {
	case 7:
		if old == nil {
			var pcma codec.PCMACtx
			pcma.SampleRate = 8000
			pcma.Channels = 1
			pcma.SampleSize = 8
			ctx = &pcma
		} else {
			ctx = old
		}
	case 8:
		if old == nil {
			var ctx codec.PCMUCtx
			ctx.SampleRate = 8000
			ctx.Channels = 1
			ctx.SampleSize = 8
			old = &ctx
		} else {
			ctx = old
		}
	case 10:
		b, err = reader.ReadByte()
		if err != nil {
			return
		}
		if b == 0 {
			if old == nil || avcc.Memory.Equal(&old.(*AACCtx).SequenceFrame.Memory) {
				var c AACCtx
				c.AACCtx = &codec.AACCtx{}
				c.SequenceFrame.CopyFrom(&avcc.Memory)
				c.CodecData, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(c.SequenceFrame.Buffers[0][2:])
				ctx = &c
			} else {
				ctx = old
				err = ErrSkip
			}
		} else {
			ctx = old
		}
	}
	return
}

func (avcc *Audio) ConvertCtx(from codec.ICodecCtx) (to codec.ICodecCtx, err error) {
	switch v := from.GetBase().(type) {
	case *codec.AACCtx:
		ctx := &AACCtx{
			AACCtx: v,
		}
		ctx.SequenceFrame.AppendOne(append([]byte{0xAF, 0x00}, v.ConfigBytes...))
		to = ctx
	default:
		to = v
	}
	return
}

func (avcc *Audio) Demux(codecCtx codec.ICodecCtx) (raw any, err error) {
	reader := avcc.NewReader()
	var result util.Memory
	if _, ok := codecCtx.(*codec.AACCtx); ok {
		err = reader.Skip(2)
		reader.Range(result.AppendOne)
		return result, err
	} else {
		err = reader.Skip(1)
		reader.Range(result.AppendOne)
		return result, err
	}
}

func (avcc *Audio) Mux(codecCtx codec.ICodecCtx, from *AVFrame) {
	avcc.Timestamp = uint32(from.Timestamp / time.Millisecond)
	audioData := from.Raw.(AudioData)
	avcc.InitRecycleIndexes(1)
	switch c := codecCtx.FourCC(); c {
	case codec.FourCC_MP4A:
		head := avcc.NextN(2)
		head[0], head[1] = 0xAF, 0x01
		avcc.Append(audioData.Buffers...)
	case codec.FourCC_ALAW, codec.FourCC_ULAW:
		head := avcc.NextN(1)
		head[0] = byte(ParseAudioCodec(c))<<4 | (1 << 1)
		avcc.Append(audioData.Buffers...)
	}
}
