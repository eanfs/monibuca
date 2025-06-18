package hls

import (
	"fmt"
	"io"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
	mpegts "m7s.live/v5/plugin/hls/pkg/ts"
)

var _ pkg.IAVFrame = (*Audio)(nil)

type Audio struct {
	mpegts.MpegTsPESPacket
	PES       *mpegts.MpegtsPESFrame
	allocator *util.ScalableMemoryAllocator
}

// GetAllocator implements pkg.IAVFrame.
func (a *Audio) GetAllocator() *util.ScalableMemoryAllocator {
	return a.allocator
}

// SetAllocator implements pkg.IAVFrame.
func (a *Audio) SetAllocator(allocator *util.ScalableMemoryAllocator) {
	a.allocator = allocator
}

// Parse implements pkg.IAVFrame.
func (Audio) Parse(old codec.ICodecCtx, f *pkg.AVFrame) (new codec.ICodecCtx, err error) {
	return old, nil
}

// ConvertCtx implements pkg.IAVFrame.
func (Audio) ConvertCtx(ctx codec.ICodecCtx) (codec.ICodecCtx, error) {
	return ctx.GetBase(), nil
}

// Demux implements pkg.IAVFrame.
func (a *Audio) Demux(codecCtx codec.ICodecCtx) (any, error) {
	// 从 PES 包中提取音频数据
	if a.Payload.Len() > 0 {
		// 返回音频数据作为 AudioData (util.Memory)
		data := make([]byte, a.Payload.Len())
		copy(data, a.Payload.Bytes())
		return util.Memory{Buffers: [][]byte{data}, Size: len(data)}, nil
	}
	return nil, pkg.ErrNotFound
}

// Mux implements pkg.IAVFrame.
func (a *Audio) Mux(codecCtx codec.ICodecCtx, frame *pkg.AVFrame) {
	// 从 AVFrame 复制数据到 HLS Audio
	if frame.Raw != nil {
		switch rawData := frame.Raw.(type) {
		case util.Memory:
			a.Payload.Reset()
			a.Payload.Write(rawData.ToBytes())
		case []byte:
			a.Payload.Reset()
			a.Payload.Write(rawData)
		default:
			a.Payload.Reset()
		}
	} else {
		a.Payload.Reset()
	}

	// 设置时间戳
	if frame.Timestamp > 0 {
		// 转换为 90kHz 时间戳
		a.Header.Pts = uint64(frame.Timestamp.Nanoseconds() * 90 / 1000000000)
		a.Header.PtsDtsFlags = 0x80 // 只有 PTS

		if frame.CTS > 0 {
			a.Header.Dts = uint64((frame.Timestamp - frame.CTS).Nanoseconds() * 90 / 1000000000)
			a.Header.PtsDtsFlags = 0xC0 // PTS 和 DTS 都存在
		}
	}
}

// GetTimestamp implements pkg.IAVFrame.
func (a *Audio) GetTimestamp() time.Duration {
	if a.Header.PtsDtsFlags&0x80 != 0 { // PTS 存在
		// 从 90kHz 转换回 time.Duration
		return time.Duration(a.Header.Pts) * time.Microsecond * 1000 / 90
	}
	return 0
}

// GetCTS implements pkg.IAVFrame.
func (a *Audio) GetCTS() time.Duration {
	if a.Header.PtsDtsFlags&0xC0 == 0xC0 { // PTS 和 DTS 都存在
		pts := time.Duration(a.Header.Pts) * time.Microsecond * 1000 / 90
		dts := time.Duration(a.Header.Dts) * time.Microsecond * 1000 / 90
		return pts - dts
	}
	return 0
}

// GetSize implements pkg.IAVFrame.
func (a *Audio) GetSize() int {
	return a.Payload.Len()
}

// Recycle implements pkg.IAVFrame.
func (a *Audio) Recycle() {
	// 回收资源
	if a.allocator != nil {
		// 如果数据是通过分配器分配的，这里可以进行回收
	}

	// 重置数据
	a.Payload.Reset()
	a.Buffers = nil

	// 重置 Header
	a.Header = mpegts.MpegTsPESHeader{}

	// 重置 PES 信息
	if a.PES != nil {
		*a.PES = mpegts.MpegtsPESFrame{}
	}
}

// String implements pkg.IAVFrame.
func (a *Audio) String() string {
	return fmt.Sprintf("HLSAudio[pts:%d, size:%d, pid:%d]",
		a.Header.Pts, a.Payload.Len(), func() uint16 {
			if a.PES != nil {
				return a.PES.Pid
			}
			return 0
		}())
}

// Dump implements pkg.IAVFrame.
func (a *Audio) Dump(t byte, w io.Writer) {
	// 输出音频数据到 writer
	if a.Payload.Len() > 0 {
		w.Write(a.Payload.Bytes())
	}
}
