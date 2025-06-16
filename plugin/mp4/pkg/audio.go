package mp4

import (
	"fmt"
	"io"
	"time"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

var _ pkg.IAVFrame = (*Audio)(nil)

type Audio struct {
	box.Sample
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
func (a *Audio) Parse(t *pkg.AVTrack) error {
	// 设置音频帧的基本信息
	t.Value.IDR = false // 音频帧通常不是 IDR
	t.Value.Timestamp = time.Duration(a.Timestamp) * time.Millisecond
	t.Value.CTS = time.Duration(a.CTS) * time.Millisecond

	// 对于 MP4 音频帧，我们通常从 Sample 中获取数据
	// 这里可以添加更多的解析逻辑，比如解析编解码器信息

	return nil
}

// ConvertCtx implements pkg.IAVFrame.
func (a *Audio) ConvertCtx(ctx codec.ICodecCtx) (codec.ICodecCtx, pkg.IAVFrame, error) {
	// 返回基础编解码器上下文，不进行转换
	return ctx.GetBase(), nil, nil
}

// Demux implements pkg.IAVFrame.
func (a *Audio) Demux(codecCtx codec.ICodecCtx) (any, error) {
	if len(a.Data) == 0 {
		return nil, fmt.Errorf("no audio data to demux")
	}

	// 创建内存对象
	var result util.Memory
	result.AppendOne(a.Data)

	// 根据编解码器类型进行解复用
	switch codecCtx.(type) {
	case *codec.AACCtx:
		// 对于 AAC，直接返回原始数据
		return result, nil
	case *codec.PCMACtx, *codec.PCMUCtx:
		// 对于 PCM 格式，直接返回原始数据
		return result, nil
	default:
		// 对于其他格式，也直接返回原始数据
		return result, nil
	}
}

// Mux implements pkg.IAVFrame.
func (a *Audio) Mux(codecCtx codec.ICodecCtx, frame *pkg.AVFrame) {
	// 从 AVFrame 复制数据到 MP4 Sample
	a.KeyFrame = false // 音频帧通常不是关键帧
	a.Timestamp = uint32(frame.Timestamp.Milliseconds())
	a.CTS = uint32(frame.CTS.Milliseconds())

	// 处理原始数据
	if frame.Raw != nil {
		switch rawData := frame.Raw.(type) {
		case util.Memory: // 包括 pkg.AudioData (它是 util.Memory 的别名)
			a.Data = rawData.ToBytes()
			a.Size = len(a.Data)

		case []byte:
			// 直接复制字节数据
			a.Data = rawData
			a.Size = len(a.Data)

		default:
			// 对于其他类型，尝试转换为字节
			a.Data = nil
			a.Size = 0
		}
	} else {
		a.Data = nil
		a.Size = 0
	}
}

// GetTimestamp implements pkg.IAVFrame.
func (a *Audio) GetTimestamp() time.Duration {
	return time.Duration(a.Timestamp) * time.Millisecond
}

// GetCTS implements pkg.IAVFrame.
func (a *Audio) GetCTS() time.Duration {
	return time.Duration(a.CTS) * time.Millisecond
}

// GetSize implements pkg.IAVFrame.
func (a *Audio) GetSize() int {
	return a.Size
}

// Recycle implements pkg.IAVFrame.
func (a *Audio) Recycle() {
	// 回收资源
	if a.allocator != nil && a.Data != nil {
		// 如果数据是通过分配器分配的，这里可以进行回收
		// 由于我们使用的是复制的数据，这里暂时不需要特殊处理
	}
	a.Data = nil
	a.Size = 0
	a.KeyFrame = false
	a.Timestamp = 0
	a.CTS = 0
	a.Offset = 0
	a.Duration = 0
}

// String implements pkg.IAVFrame.
func (a *Audio) String() string {
	return fmt.Sprintf("MP4Audio[ts:%d, cts:%d, size:%d]",
		a.Timestamp, a.CTS, a.Size)
}

// Dump implements pkg.IAVFrame.
func (a *Audio) Dump(t byte, w io.Writer) {
	// 输出数据到 writer
	if a.Data != nil {
		w.Write(a.Data)
	}
}
