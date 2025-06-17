package mp4

import (
	"fmt"
	"io"
	"slices"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

var _ pkg.IAVFrame = (*Video)(nil)

type Video struct {
	box.Sample
	allocator *util.ScalableMemoryAllocator
}

// GetAllocator implements pkg.IAVFrame.
func (v *Video) GetAllocator() *util.ScalableMemoryAllocator {
	return v.allocator
}

// SetAllocator implements pkg.IAVFrame.
func (v *Video) SetAllocator(allocator *util.ScalableMemoryAllocator) {
	v.allocator = allocator
}

// Parse implements pkg.IAVFrame.
func (v *Video) Parse(t *pkg.AVTrack) error {
	t.Value.IDR = v.KeyFrame
	return nil
}

// ConvertCtx implements pkg.IAVFrame.
func (v *Video) ConvertCtx(ctx codec.ICodecCtx) (codec.ICodecCtx, pkg.IAVFrame, error) {
	// 返回基础编解码器上下文，不进行转换
	return ctx.GetBase(), nil, nil
}

// Demux implements pkg.IAVFrame.
func (v *Video) Demux(codecCtx codec.ICodecCtx) (any, error) {
	if len(v.Data) == 0 {
		return nil, fmt.Errorf("no video data to demux")
	}

	// 创建内存读取器
	var mem util.Memory
	mem.AppendOne(v.Data)
	reader := mem.NewReader()

	var nalus pkg.Nalus

	// 根据编解码器类型进行解复用
	switch ctx := codecCtx.(type) {
	case *codec.H264Ctx:
		// 对于 H.264，解析 AVCC 格式的 NAL 单元
		if err := nalus.ParseAVCC(reader, int(ctx.RecordInfo.LengthSizeMinusOne)+1); err != nil {
			return nil, fmt.Errorf("failed to parse H.264 AVCC: %w", err)
		}
	case *codec.H265Ctx:
		// 对于 H.265，解析 AVCC 格式的 NAL 单元
		if err := nalus.ParseAVCC(reader, int(ctx.RecordInfo.LengthSizeMinusOne)+1); err != nil {
			return nil, fmt.Errorf("failed to parse H.265 AVCC: %w", err)
		}
	default:
		// 对于其他格式，尝试默认的 AVCC 解析（4字节长度前缀）
		if err := nalus.ParseAVCC(reader, 4); err != nil {
			return nil, fmt.Errorf("failed to parse AVCC with default settings: %w", err)
		}
	}

	return nalus, nil
}

// Mux implements pkg.IAVFrame.
func (v *Video) Mux(codecCtx codec.ICodecCtx, frame *pkg.AVFrame) {
	// 从 AVFrame 复制数据到 MP4 Sample
	v.KeyFrame = frame.IDR
	v.Timestamp = uint32(frame.Timestamp.Milliseconds())
	v.CTS = uint32(frame.CTS.Milliseconds())

	// 处理原始数据
	if frame.Raw != nil {
		switch rawData := frame.Raw.(type) {
		case pkg.Nalus:
			// 将 Nalus 转换为 AVCC 格式的字节数据
			var buffer util.Buffer

			// 根据编解码器类型确定 NALU 长度字段的大小
			var naluSizeLen int = 4 // 默认使用 4 字节
			switch ctx := codecCtx.(type) {
			case *codec.H264Ctx:
				naluSizeLen = int(ctx.RecordInfo.LengthSizeMinusOne) + 1
			case *codec.H265Ctx:
				naluSizeLen = int(ctx.RecordInfo.LengthSizeMinusOne) + 1
			}

			// 为每个 NALU 添加长度前缀
			for _, nalu := range rawData {
				util.PutBE(buffer.Malloc(naluSizeLen), nalu.Size) // 写入 NALU 长度
				var buffers = slices.Clone(nalu.Buffers)          // 克隆 NALU 的缓冲区
				buffers.WriteTo(&buffer)                          // 直接写入 NALU 数据
			}
			v.Data = buffer
			v.Size = len(v.Data)

		case []byte:
			// 直接复制字节数据
			v.Data = rawData
			v.Size = len(v.Data)

		default:
			// 对于其他类型，尝试转换为字节
			v.Data = nil
			v.Size = 0
		}
	} else {
		v.Data = nil
		v.Size = 0
	}
}

// GetTimestamp implements pkg.IAVFrame.
func (v *Video) GetTimestamp() time.Duration {
	return time.Duration(v.Timestamp) * time.Millisecond
}

// GetCTS implements pkg.IAVFrame.
func (v *Video) GetCTS() time.Duration {
	return time.Duration(v.CTS) * time.Millisecond
}

// GetSize implements pkg.IAVFrame.
func (v *Video) GetSize() int {
	return v.Size
}

// Recycle implements pkg.IAVFrame.
func (v *Video) Recycle() {
	// 回收资源
	if v.allocator != nil && v.Data != nil {
		// 如果数据是通过分配器分配的，这里可以进行回收
		// 由于我们使用的是复制的数据，这里暂时不需要特殊处理
	}
	v.Data = nil
	v.Size = 0
	v.KeyFrame = false
	v.Timestamp = 0
	v.CTS = 0
	v.Offset = 0
	v.Duration = 0
}

// String implements pkg.IAVFrame.
func (v *Video) String() string {
	return fmt.Sprintf("MP4Video[ts:%d, cts:%d, size:%d, keyframe:%t]",
		v.Timestamp, v.CTS, v.Size, v.KeyFrame)
}

// Dump implements pkg.IAVFrame.
func (v *Video) Dump(t byte, w io.Writer) {
	// 输出数据到 writer
	if v.Data != nil {
		w.Write(v.Data)
	}
}
