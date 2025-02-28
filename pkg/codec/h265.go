package codec

import (
	"fmt"

	"github.com/deepch/vdk/codec/h265parser"
)

type H265NALUType byte

func (H265NALUType) Parse(b byte) H265NALUType {
	return H265NALUType(b & 0x7E >> 1)
}

func ParseH265NALUType(b byte) H265NALUType {
	return H265NALUType(b & 0x7E >> 1)
}

var AudNalu = []byte{0x00, 0x00, 0x00, 0x01, 0x46, 0x01, 0x10}

type (
	H265Ctx struct {
		h265parser.CodecData
	}
)

func (ctx *H265Ctx) GetInfo() string {
	return fmt.Sprintf("fps: %d, resolution: %s", ctx.FPS(), ctx.Resolution())
}

func (*H265Ctx) FourCC() FourCC {
	return FourCC_H265
}

func (h265 *H265Ctx) GetBase() ICodecCtx {
	return h265
}

func (h265 *H265Ctx) GetRecord() []byte {
	return h265.Record
}

func (h265 *H265Ctx) String() string {
	return fmt.Sprintf("hvc1.%02X%02X%02X", h265.RecordInfo.AVCProfileIndication, h265.RecordInfo.ProfileCompatibility, h265.RecordInfo.AVCLevelIndication)
}
