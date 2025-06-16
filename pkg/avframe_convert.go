package pkg

import (
	"reflect"

	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
)

type AVFrameConvert[T IAVFrame] struct {
	FromTrack, ToTrack *AVTrack
	lastFromCodecCtx   codec.ICodecCtx
}

func NewAVFrameConvert[T IAVFrame](fromTrack *AVTrack, toTrack *AVTrack) *AVFrameConvert[T] {
	ret := &AVFrameConvert[T]{}
	ret.FromTrack = fromTrack
	ret.ToTrack = toTrack
	if ret.FromTrack == nil {
		ret.FromTrack = &AVTrack{
			RingWriter: &RingWriter{
				Ring: util.NewRing[AVFrame](1),
			},
		}
	}
	if ret.ToTrack == nil {
		ret.ToTrack = &AVTrack{
			RingWriter: &RingWriter{
				Ring: util.NewRing[AVFrame](1),
			},
		}
		var to T
		ret.ToTrack.FrameType = reflect.TypeOf(to).Elem()
	}
	return ret
}

func (c *AVFrameConvert[T]) ConvertFromAVFrame(avFrame *AVFrame) (to T, err error) {
	to = reflect.New(c.ToTrack.FrameType).Interface().(T)
	if c.ToTrack.ICodecCtx == nil {
		if c.ToTrack.ICodecCtx, c.ToTrack.SequenceFrame, err = to.ConvertCtx(c.FromTrack.ICodecCtx); err != nil {
			return
		}
	}
	if avFrame.Raw == nil {
		if err = avFrame.Demux(c.FromTrack.ICodecCtx); err != nil {
			return
		}
	}
	to.SetAllocator(avFrame.Wraps[0].GetAllocator())
	to.Mux(c.ToTrack.ICodecCtx, avFrame)
	return
}

func (c *AVFrameConvert[T]) Convert(frame IAVFrame) (to T, err error) {
	to = reflect.New(c.ToTrack.FrameType).Interface().(T)
	// Not From Publisher
	if c.FromTrack.LastValue == nil {
		err = frame.Parse(c.FromTrack)
		if err != nil {
			return
		}
	}
	if c.ToTrack.ICodecCtx == nil || c.lastFromCodecCtx != c.FromTrack.ICodecCtx {
		if c.ToTrack.ICodecCtx, c.ToTrack.SequenceFrame, err = to.ConvertCtx(c.FromTrack.ICodecCtx); err != nil {
			return
		}
	}
	c.lastFromCodecCtx = c.FromTrack.ICodecCtx
	if c.FromTrack.Value.Raw == nil {
		if c.FromTrack.Value.Raw, err = frame.Demux(c.FromTrack.ICodecCtx); err != nil {
			return
		}
	}
	to.SetAllocator(frame.GetAllocator())
	to.Mux(c.ToTrack.ICodecCtx, &c.FromTrack.Value)
	return
}
