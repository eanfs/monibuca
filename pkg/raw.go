package pkg

import (
	"fmt"
	"io"
	"time"

	"github.com/deepch/vdk/codec/aacparser"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/codec/h265parser"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
)

var _ IAVFrame = (*RawAudio)(nil)

type RawAudio struct {
	codec.FourCC
	Timestamp time.Duration
	util.RecyclableMemory
}

func (r *RawAudio) Parse(old codec.ICodecCtx, f *AVFrame) (new codec.ICodecCtx, err error) {
	if old == nil {
		switch r.FourCC {
		case codec.FourCC_MP4A:
			ctx := &codec.AACCtx{}
			ctx.CodecData, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(r.ToBytes())
			if err != nil {
				return
			}
			new = ctx
		case codec.FourCC_ALAW:
			new = &codec.PCMACtx{
				AudioCtx: codec.AudioCtx{
					SampleRate: 8000,
					Channels:   1,
					SampleSize: 8,
				},
			}
		case codec.FourCC_ULAW:
			new = &codec.PCMUCtx{
				AudioCtx: codec.AudioCtx{
					SampleRate: 8000,
					Channels:   1,
					SampleSize: 8,
				},
			}
		}
	} else {
		new = old
	}
	return
}

func (RawAudio) ConvertCtx(ctx codec.ICodecCtx) (codec.ICodecCtx, error) {
	return ctx.GetBase(), nil
}

func (r *RawAudio) Demux(ctx codec.ICodecCtx) (any, error) {
	return r.Memory, nil
}

func (r *RawAudio) Mux(ctx codec.ICodecCtx, frame *AVFrame) {
	r.InitRecycleIndexes(0)
	r.FourCC = ctx.FourCC()
	r.Memory = frame.Raw.(util.Memory)
	r.Timestamp = frame.Timestamp
}

func (r *RawAudio) GetTimestamp() time.Duration {
	return r.Timestamp
}

func (r *RawAudio) GetSize() int {
	return r.Size
}

func (r *RawAudio) String() string {
	return fmt.Sprintf("RawAudio{FourCC: %s, Timestamp: %s, Size: %d}", r.FourCC, r.Timestamp, r.Size)
}

func (r *RawAudio) Dump(b byte, writer io.Writer) {
	//TODO implement me
	panic("implement me")
}

var _ IAVFrame = (*H26xFrame)(nil)

type H26xFrame struct {
	codec.FourCC
	Timestamp time.Duration
	CTS       time.Duration
	Nalus
	util.RecyclableMemory
}

func (h *H26xFrame) Parse(old codec.ICodecCtx, f *AVFrame) (new codec.ICodecCtx, err error) {
	f.CTS = h.CTS
	var hasVideoFrame bool
	new = old
	// First determine the codec type from existing context or FourCC
	if old != nil {
		switch base := old.GetBase().(type) {
		case *codec.H264Ctx:
			ctx := base
			for _, nalu := range h.Nalus {
				switch codec.ParseH264NALUType(nalu.Buffers[0][0]) {
				case h264parser.NALU_SPS:
					ctx = &codec.H264Ctx{}
					new = ctx
					ctx.RecordInfo.SPS = [][]byte{nalu.ToBytes()}
					if ctx.SPSInfo, err = h264parser.ParseSPS(ctx.SPS()); err != nil {
						return
					}
				case h264parser.NALU_PPS:
					ctx.RecordInfo.PPS = [][]byte{nalu.ToBytes()}
					ctx.CodecData, err = h264parser.NewCodecDataFromSPSAndPPS(ctx.SPS(), ctx.PPS())
					if err != nil {
						return
					}
				case codec.NALU_IDR_Picture:
					f.IDR = true
					hasVideoFrame = true
				case codec.NALU_Non_IDR_Picture:
					hasVideoFrame = true
				}
			}
		case *codec.H265Ctx:
			ctx := base
			for _, nalu := range h.Nalus {
				switch codec.ParseH265NALUType(nalu.Buffers[0][0]) {
				case h265parser.NAL_UNIT_VPS:
					ctx = &codec.H265Ctx{}
					ctx.RecordInfo.VPS = [][]byte{nalu.ToBytes()}
					new = ctx
				case h265parser.NAL_UNIT_SPS:
					ctx.RecordInfo.SPS = [][]byte{nalu.ToBytes()}
					if ctx.SPSInfo, err = h265parser.ParseSPS(ctx.SPS()); err != nil {
						return
					}
				case h265parser.NAL_UNIT_PPS:
					ctx.RecordInfo.PPS = [][]byte{nalu.ToBytes()}
					ctx.CodecData, err = h265parser.NewCodecDataFromVPSAndSPSAndPPS(ctx.VPS(), ctx.SPS(), ctx.PPS())
				case h265parser.NAL_UNIT_CODED_SLICE_BLA_W_LP,
					h265parser.NAL_UNIT_CODED_SLICE_BLA_W_RADL,
					h265parser.NAL_UNIT_CODED_SLICE_BLA_N_LP,
					h265parser.NAL_UNIT_CODED_SLICE_IDR_W_RADL,
					h265parser.NAL_UNIT_CODED_SLICE_IDR_N_LP,
					h265parser.NAL_UNIT_CODED_SLICE_CRA:
					f.IDR = true
					hasVideoFrame = true
				case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9:
					hasVideoFrame = true
				}
			}
		}
	} else {
		// Fallback to FourCC when no old context is available
		switch h.FourCC {
		case codec.FourCC_H264:
			var ctx *codec.H264Ctx
			for _, nalu := range h.Nalus {
				switch codec.ParseH264NALUType(nalu.Buffers[0][0]) {
				case h264parser.NALU_SPS:
					ctx = &codec.H264Ctx{}
					new = ctx
					ctx.RecordInfo.SPS = [][]byte{nalu.ToBytes()}
					if ctx.SPSInfo, err = h264parser.ParseSPS(ctx.SPS()); err != nil {
						return
					}
				case h264parser.NALU_PPS:
					ctx.RecordInfo.PPS = [][]byte{nalu.ToBytes()}
					ctx.CodecData, err = h264parser.NewCodecDataFromSPSAndPPS(ctx.SPS(), ctx.PPS())
					if err != nil {
						return
					}
				case codec.NALU_IDR_Picture:
					f.IDR = true
					hasVideoFrame = true
				case codec.NALU_Non_IDR_Picture:
					hasVideoFrame = true
				}
			}
		case codec.FourCC_H265:
			var ctx *codec.H265Ctx
			for _, nalu := range h.Nalus {
				switch codec.ParseH265NALUType(nalu.Buffers[0][0]) {
				case h265parser.NAL_UNIT_VPS:
					ctx = &codec.H265Ctx{}
					ctx.RecordInfo.VPS = [][]byte{nalu.ToBytes()}
					new = ctx
				case h265parser.NAL_UNIT_SPS:
					ctx.RecordInfo.SPS = [][]byte{nalu.ToBytes()}
					if ctx.SPSInfo, err = h265parser.ParseSPS(ctx.SPS()); err != nil {
						return
					}
				case h265parser.NAL_UNIT_PPS:
					ctx.RecordInfo.PPS = [][]byte{nalu.ToBytes()}
					ctx.CodecData, err = h265parser.NewCodecDataFromVPSAndSPSAndPPS(ctx.VPS(), ctx.SPS(), ctx.PPS())
				case h265parser.NAL_UNIT_CODED_SLICE_BLA_W_LP,
					h265parser.NAL_UNIT_CODED_SLICE_BLA_W_RADL,
					h265parser.NAL_UNIT_CODED_SLICE_BLA_N_LP,
					h265parser.NAL_UNIT_CODED_SLICE_IDR_W_RADL,
					h265parser.NAL_UNIT_CODED_SLICE_IDR_N_LP,
					h265parser.NAL_UNIT_CODED_SLICE_CRA:
					f.IDR = true
					hasVideoFrame = true
				case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9:
					hasVideoFrame = true
				}
			}
		}
	}

	// Return ErrSkip if no video frames are present (only metadata NALUs)
	if !hasVideoFrame {
		return nil, ErrSkip
	}

	return
}

func (H26xFrame) ConvertCtx(ctx codec.ICodecCtx) (codec.ICodecCtx, error) {
	return ctx.GetBase(), nil
}

func (h *H26xFrame) Demux(ctx codec.ICodecCtx) (any, error) {
	return h.Nalus, nil
}

func (h *H26xFrame) Mux(ctx codec.ICodecCtx, frame *AVFrame) {
	h.FourCC = ctx.FourCC()
	h.Nalus = frame.Raw.(Nalus)
	h.Timestamp = frame.Timestamp
	h.CTS = frame.CTS
}

func (h *H26xFrame) GetTimestamp() time.Duration {
	return h.Timestamp
}

func (h *H26xFrame) GetCTS() time.Duration {
	return h.CTS
}

func (h *H26xFrame) GetSize() int {
	var size int
	for _, nalu := range h.Nalus {
		size += nalu.Size
	}
	return size
}

func (h *H26xFrame) String() string {
	return fmt.Sprintf("H26xFrame{FourCC: %s, Timestamp: %s, CTS: %s}", h.FourCC, h.Timestamp, h.CTS)
}
