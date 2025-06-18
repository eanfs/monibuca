package pkg

import (
	"reflect"

	"m7s.live/v5/pkg/codec"
)

type AVFrameConvert[T IAVFrame] struct {
	AVFrame
	sourceCodecCtx, targetCodecCtx codec.ICodecCtx
	frameType                      reflect.Type
}

func NewAVFrameConvert[T IAVFrame](sourceCodecCtx codec.ICodecCtx) *AVFrameConvert[T] {
	var to T
	ret := &AVFrameConvert[T]{
		sourceCodecCtx: sourceCodecCtx,
		frameType:      reflect.TypeOf(to).Elem(),
	}
	return ret
}

func (c *AVFrameConvert[T]) Convert(frame IAVFrame) (to T, err error) {
	to = reflect.New(c.frameType).Interface().(T)
	var newSourceCodecCtx codec.ICodecCtx
	newSourceCodecCtx, err = frame.Parse(c.sourceCodecCtx, &c.AVFrame)
	if err != nil {
		return
	}
	if c.targetCodecCtx == nil || c.sourceCodecCtx != newSourceCodecCtx {
		c.sourceCodecCtx = newSourceCodecCtx
		if c.targetCodecCtx, err = to.ConvertCtx(newSourceCodecCtx); err != nil {
			return
		}
	}
	if c.AVFrame.Raw, err = frame.Demux(c.sourceCodecCtx); err != nil {
		return
	}
	to.SetAllocator(frame.GetAllocator())
	to.Mux(c.targetCodecCtx, &c.AVFrame)
	return
}
