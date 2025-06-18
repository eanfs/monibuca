package rtmp

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"time"

	"github.com/deepch/vdk/codec/h264parser"

	. "m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
)

var _ IAVFrame = (*Video)(nil)

type Video struct {
	RTMPData
	CTS uint32
}

// 过滤掉异常的 NALU
func (avcc *Video) filterH264(naluSizeLen int) {
	reader := avcc.NewReader()
	lenReader := reader.NewReader()
	reader.Skip(5)
	var afterFilter util.Memory
	lenReader.RangeN(5, afterFilter.AppendOne)
	allocator := avcc.GetAllocator()
	var hasBadNalu bool
	for {
		naluLen, err := reader.ReadBE(naluSizeLen)
		if err != nil {
			break
		}
		var lenBuffer net.Buffers
		lenReader.RangeN(naluSizeLen, func(b []byte) {
			lenBuffer = append(lenBuffer, b)
		})
		lenReader.Skip(int(naluLen))
		var naluBuffer net.Buffers
		reader.RangeN(int(naluLen), func(b []byte) {
			naluBuffer = append(naluBuffer, b)
		})
		badType := codec.ParseH264NALUType(naluBuffer[0][0])
		// 替换之前打印 badType 的逻辑，解码并打印 SliceType
		if badType == 5 { // NALU type for Coded slice of a non-IDR picture or Coded slice of an IDR picture
			naluData := bytes.Join(naluBuffer, nil) // bytes 包已导入
			if len(naluData) > 0 {
				// h264parser 包已导入 as "github.com/deepch/vdk/codec/h264parser"
				// ParseSliceHeaderFromNALU 返回的第一个值就是 SliceType
				sliceType, err := h264parser.ParseSliceHeaderFromNALU(naluData)
				if err == nil {
					println("Decoded SliceType:", sliceType.String())
				} else {
					println("Error parsing H.264 slice header:", err.Error())
				}
			} else {
				println("NALU data is empty, cannot parse H.264 slice header.")
			}
		}

		switch badType {
		case 5, 6, 7, 8, 1, 2, 3, 4:
			afterFilter.Append(lenBuffer...)
			afterFilter.Append(naluBuffer...)
		default:
			hasBadNalu = true
			if allocator != nil {
				for _, nalu := range lenBuffer {
					allocator.Free(nalu)
				}
				for _, nalu := range naluBuffer {
					allocator.Free(nalu)
				}
			}
		}
	}
	if hasBadNalu {
		avcc.Memory = afterFilter
	}
}

func (avcc *Video) filterH265(naluSizeLen int) {
	//TODO
}

func (avcc *Video) Parse(old codec.ICodecCtx, f *AVFrame) (ctx codec.ICodecCtx, err error) {
	f.CTS = time.Duration(avcc.CTS) * time.Millisecond
	if avcc.Size <= 10 {
		err = io.ErrShortBuffer
		return
	}
	reader := avcc.NewReader()
	var b0 byte
	b0, err = reader.ReadByte()
	if err != nil {
		return
	}
	enhanced := b0&0b1000_0000 != 0 // https://veovera.github.io/enhanced-rtmp/docs/enhanced/enhanced-rtmp-v1.pdf
	f.IDR = b0&0b0111_0000>>4 == 1
	packetType := b0 & 0b1111
	codecId := VideoCodecID(b0 & 0x0F)
	var fourCC codec.FourCC
	parseSequence := func() (err error) {
		f.IDR = false
		var cloneFrame Video
		cloneFrame.CopyFrom(&avcc.Memory)
		switch fourCC {
		case codec.FourCC_H264:
			newCtx := &H264Ctx{
				SequenceFrame: cloneFrame,
			}
			newCtx.H264Ctx, err = codec.NewH264CtxFromRecord(cloneFrame.Buffers[0][reader.Offset():])
			if err == nil {
				if old != nil && bytes.Equal(old.(*H264Ctx).Record, newCtx.Record) {
					ctx = old
					err = ErrSkip
					return
				}
				ctx = newCtx
			}
		case codec.FourCC_H265:
			newCtx := H265Ctx{
				Enhanced:      enhanced,
				SequenceFrame: cloneFrame,
			}
			newCtx.H265Ctx, err = codec.NewH265CtxFromRecord(cloneFrame.Buffers[0][reader.Offset():])
			if err == nil {
				if old != nil && bytes.Equal(old.(*H265Ctx).Record, newCtx.Record) {
					ctx = old
					err = ErrSkip
					return
				}
				ctx = newCtx
			}
		case codec.FourCC_AV1:
			var newCtx AV1Ctx
			if err = newCtx.Unmarshal(reader); err == nil {
				ctx = &newCtx
			}
		}
		return
	}
	if enhanced {
		reader.ReadBytesTo(fourCC[:])
		switch packetType {
		case PacketTypeSequenceStart:
			err = parseSequence()
			return
		case PacketTypeCodedFrames:
			switch old.(type) {
			case *H265Ctx:
				if avcc.CTS, err = reader.ReadBE(3); err != nil {
					return old, err
				}
				// avcc.filterH265(int(ctx.RecordInfo.LengthSizeMinusOne) + 1)
			case *AV1Ctx:
				// return avcc.parseAV1(reader)
			}
		case PacketTypeCodedFramesX:
			// avcc.filterH265(int(old.(*H265Ctx).RecordInfo.LengthSizeMinusOne) + 1)
		}
	} else {
		b0, err = reader.ReadByte() //sequence frame flag
		if err != nil {
			return
		}
		if codecId == CodecID_H265 {
			fourCC = codec.FourCC_H265
		} else {
			fourCC = codec.FourCC_H264
		}
		avcc.CTS, err = reader.ReadBE(3) // cts == 0
		if err != nil {
			return
		}
		if b0 == 0 {
			if err = parseSequence(); err != nil {
				return
			}
		} else {
			// switch ctx := old.(type) {
			// case *codec.H264Ctx:
			// 	avcc.filterH264(int(ctx.RecordInfo.LengthSizeMinusOne) + 1)
			// case *H265Ctx:
			// 	avcc.filterH265(int(ctx.RecordInfo.LengthSizeMinusOne) + 1)
			// }
			// if avcc.Size <= 5 {
			// 	return old, ErrSkip
			// }
		}
	}
	if ctx == nil {
		ctx = old
	}
	return
}

func (avcc *Video) ConvertCtx(from codec.ICodecCtx) (to codec.ICodecCtx, err error) {
	var enhanced = true //TODO
	switch fromCtx := from.GetBase().(type) {
	case *codec.H264Ctx:
		ctx := &H264Ctx{H264Ctx: fromCtx}
		ctx.SequenceFrame.AppendOne(append([]byte{0x17, 0, 0, 0, 0}, fromCtx.Record...))
		//if t.Enabled(context.TODO(), TraceLevel) {
		//	c := t.FourCC().String()
		//	size := seqFrame.GetSize()
		//	data := seqFrame.String()
		//	t.Trace("decConfig", "codec", c, "size", size, "data", data)
		//}
		return ctx, err
	case *codec.H265Ctx:
		ctx := &H265Ctx{H265Ctx: fromCtx, Enhanced: enhanced}
		b := make(util.Buffer, len(ctx.Record)+5)
		if enhanced {
			b[0] = 0b1001_0000 | byte(PacketTypeSequenceStart)
			copy(b[1:], codec.FourCC_H265[:])
		} else {
			b[0], b[1], b[2], b[3], b[4] = 0x1C, 0, 0, 0, 0
		}
		copy(b[5:], ctx.Record)
		ctx.SequenceFrame.AppendOne(b)
		return ctx, err
	}
	return
}

func (avcc *Video) parseH264(ctx *H264Ctx, reader *util.MemoryReader) (any, error) {
	var nalus Nalus
	if err := nalus.ParseAVCC(reader, int(ctx.RecordInfo.LengthSizeMinusOne)+1); err != nil {
		return nalus, err
	}
	return nalus, nil
}

func (avcc *Video) parseH265(ctx *H265Ctx, reader *util.MemoryReader) (any, error) {
	var nalus Nalus
	if err := nalus.ParseAVCC(reader, int(ctx.RecordInfo.LengthSizeMinusOne)+1); err != nil {
		return nalus, err
	}
	return nalus, nil
}

func (avcc *Video) parseAV1(reader *util.MemoryReader) (any, error) {
	var obus OBUs
	if err := obus.ParseAVCC(reader); err != nil {
		return obus, err
	}
	return obus, nil
}

func (avcc *Video) Demux(codecCtx codec.ICodecCtx) (any, error) {
	reader := avcc.NewReader()
	b0, err := reader.ReadByte()
	if err != nil {
		return nil, err
	}

	enhanced := b0&0b1000_0000 != 0 // https://veovera.github.io/enhanced-rtmp/docs/enhanced/enhanced-rtmp-v1.pdf
	// frameType := b0 & 0b0111_0000 >> 4
	packetType := b0 & 0b1111

	if enhanced {
		err = reader.Skip(4) // fourcc
		if err != nil {
			return nil, err
		}
		switch packetType {
		case PacketTypeSequenceStart:
			// see Parse()
			return nil, nil
		case PacketTypeCodedFrames:
			switch ctx := codecCtx.(type) {
			case *H265Ctx:
				if avcc.CTS, err = reader.ReadBE(3); err != nil {
					return nil, err
				}
				return avcc.parseH265(ctx, reader)
			case *AV1Ctx:
				return avcc.parseAV1(reader)
			}
		case PacketTypeCodedFramesX: // no cts
			return avcc.parseH265(codecCtx.(*H265Ctx), reader)
		}
	} else {
		b0, err = reader.ReadByte() //sequence frame flag
		if err != nil {
			return nil, err
		}
		if avcc.CTS, err = reader.ReadBE(3); err != nil {
			return nil, err
		}
		var nalus Nalus
		switch ctx := codecCtx.(type) {
		case *H265Ctx:
			if b0 == 0 {
				nalus.Append(ctx.VPS())
				nalus.Append(ctx.SPS())
				nalus.Append(ctx.PPS())
			} else {
				return avcc.parseH265(ctx, reader)
			}

		case *H264Ctx:
			if b0 == 0 {
				nalus.Append(ctx.SPS())
				nalus.Append(ctx.PPS())
			} else {
				return avcc.parseH264(ctx, reader)
			}
		}
		return nalus, nil
	}
	return nil, nil
}

func (avcc *Video) muxOld26x(codecID VideoCodecID, from *AVFrame) {
	nalus := from.Raw.(Nalus)
	avcc.InitRecycleIndexes(len(nalus)) // Recycle partial data
	head := avcc.NextN(5)
	head[0] = util.Conditional[byte](from.IDR, 0x10, 0x20) | byte(codecID)
	head[1] = 1
	util.PutBE(head[2:5], from.CTS/time.Millisecond) // cts
	for _, nalu := range nalus {
		naluLenM := avcc.NextN(4)
		naluLen := uint32(nalu.Size)
		binary.BigEndian.PutUint32(naluLenM, naluLen)
		avcc.Append(nalu.Buffers...)
	}
}

func (avcc *Video) Mux(codecCtx codec.ICodecCtx, from *AVFrame) {
	avcc.Timestamp = uint32(from.Timestamp / time.Millisecond)
	switch ctx := codecCtx.(type) {
	case *AV1Ctx:
		panic(ctx)
	case *H264Ctx:
		avcc.muxOld26x(CodecID_H264, from)
	case *H265Ctx:
		if ctx.Enhanced {
			nalus := from.Raw.(Nalus)
			avcc.InitRecycleIndexes(len(nalus)) // Recycle partial data
			head := avcc.NextN(8)
			if from.IDR {
				head[0] = 0b1001_0000 | byte(PacketTypeCodedFrames)
			} else {
				head[0] = 0b1010_0000 | byte(PacketTypeCodedFrames)
			}
			copy(head[1:], codec.FourCC_H265[:])
			util.PutBE(head[5:8], from.CTS/time.Millisecond) // cts
			for _, nalu := range nalus {
				naluLenM := avcc.NextN(4)
				naluLen := uint32(nalu.Size)
				binary.BigEndian.PutUint32(naluLenM, naluLen)
				avcc.Append(nalu.Buffers...)
			}
		} else {
			avcc.muxOld26x(CodecID_H265, from)
		}
	}
}
