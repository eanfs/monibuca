package mp4

import (
	"strings"
	"time"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/task"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/mp4/pkg/box"
	rtmp "m7s.live/v5/plugin/rtmp/pkg"
)

type (
	RecordReader struct {
		m7s.RecordFilePuller
		demuxer *Demuxer
	}
)

func NewPuller(conf config.Pull) m7s.IPuller {
	if strings.HasPrefix(conf.URL, "http") || strings.HasSuffix(conf.URL, ".mp4") {
		p := &HTTPReader{}
		p.SetDescription(task.OwnerTypeKey, "Mp4Reader")
		return p
	}
	if conf.Args.Get(util.StartKey) != "" {
		p := &RecordReader{}
		p.Type = "mp4"
		p.SetDescription(task.OwnerTypeKey, "Mp4RecordReader")
		return p
	}
	return nil
}

func (p *RecordReader) Run() (err error) {
	pullJob := &p.PullJob
	publisher := pullJob.Publisher
	if publisher == nil {
		return pkg.ErrDisabled
	}

	var realTime time.Time
	publisher.OnGetPosition = func() time.Time {
		return realTime
	}

	// 简化的时间戳管理变量
	var ts int64       // 当前时间戳
	var tsOffset int64 // 时间戳偏移量

	// 创建可复用的 DemuxerRange 实例
	demuxerRange := &DemuxerRange{}
	// 设置音视频额外数据回调（序列头）
	demuxerRange.OnVideoExtraData = func(codecType box.MP4_CODEC_TYPE, data []byte) error {
		switch codecType {
		case box.MP4_CODEC_H264:
			var sequence rtmp.RTMPVideo
			sequence.Append([]byte{0x17, 0x00, 0x00, 0x00, 0x00}, data)
			err = publisher.WriteVideo(&sequence)
		case box.MP4_CODEC_H265:
			var sequence rtmp.RTMPVideo
			sequence.Append([]byte{0b1001_0000 | rtmp.PacketTypeSequenceStart}, codec.FourCC_H265[:], data)
			err = publisher.WriteVideo(&sequence)
		}
		return err
	}

	demuxerRange.OnAudioExtraData = func(codecType box.MP4_CODEC_TYPE, data []byte) error {
		if codecType == box.MP4_CODEC_AAC {
			var sequence rtmp.RTMPAudio
			sequence.Append([]byte{0xaf, 0x00}, data)
			err = publisher.WriteAudio(&sequence)
		}
		return err
	}

	// 设置视频样本回调
	demuxerRange.OnVideoSample = func(codecType box.MP4_CODEC_TYPE, sample box.Sample) error {
		if publisher.Paused != nil {
			publisher.Paused.Await()
		}

		// 检查是否需要跳转
		if needSeek, seekErr := p.CheckSeek(); seekErr != nil {
			return seekErr
		} else if needSeek {
			return pkg.ErrSkip
		}

		// 简化的时间戳处理
		if int64(sample.Timestamp)+tsOffset < 0 {
			ts = 0
		} else {
			ts = int64(sample.Timestamp) + tsOffset
		}

		// 更新实时时间
		realTime = time.Now() // 这里可以根据需要调整为更精确的时间计算

		// 根据编解码器类型处理视频帧
		switch codecType {
		case box.MP4_CODEC_H264:
			var videoFrame rtmp.RTMPVideo
			videoFrame.CTS = sample.CTS
			videoFrame.Timestamp = uint32(ts)
			videoFrame.Append([]byte{util.Conditional[byte](sample.KeyFrame, 0x17, 0x27), 0x01, byte(videoFrame.CTS >> 24), byte(videoFrame.CTS >> 8), byte(videoFrame.CTS)}, sample.Data)
			err = publisher.WriteVideo(&videoFrame)
		case box.MP4_CODEC_H265:
			var videoFrame rtmp.RTMPVideo
			videoFrame.CTS = sample.CTS
			videoFrame.Timestamp = uint32(ts)
			var head []byte
			var b0 byte = 0b1010_0000
			if sample.KeyFrame {
				b0 = 0b1001_0000
			}
			if videoFrame.CTS == 0 {
				head = videoFrame.NextN(5)
				head[0] = b0 | rtmp.PacketTypeCodedFramesX
			} else {
				head = videoFrame.NextN(8)
				head[0] = b0 | rtmp.PacketTypeCodedFrames
				util.PutBE(head[5:8], videoFrame.CTS) // cts
			}
			copy(head[1:], codec.FourCC_H265[:])
			videoFrame.AppendOne(sample.Data)
			err = publisher.WriteVideo(&videoFrame)
		}
		return err
	}

	// 设置音频样本回调
	demuxerRange.OnAudioSample = func(codecType box.MP4_CODEC_TYPE, sample box.Sample) error {
		if publisher.Paused != nil {
			publisher.Paused.Await()
		}

		// 检查是否需要跳转
		if needSeek, seekErr := p.CheckSeek(); seekErr != nil {
			return seekErr
		} else if needSeek {
			return pkg.ErrSkip
		}

		// 简化的时间戳处理
		if int64(sample.Timestamp)+tsOffset < 0 {
			ts = 0
		} else {
			ts = int64(sample.Timestamp) + tsOffset
		}

		// 根据编解码器类型处理音频帧
		switch codecType {
		case box.MP4_CODEC_AAC:
			var audioFrame rtmp.RTMPAudio
			audioFrame.Timestamp = uint32(ts)
			audioFrame.Append([]byte{0xaf, 0x01}, sample.Data)
			err = publisher.WriteAudio(&audioFrame)
		case box.MP4_CODEC_G711A:
			var audioFrame rtmp.RTMPAudio
			audioFrame.Timestamp = uint32(ts)
			audioFrame.Append([]byte{0x72}, sample.Data)
			err = publisher.WriteAudio(&audioFrame)
		case box.MP4_CODEC_G711U:
			var audioFrame rtmp.RTMPAudio
			audioFrame.Timestamp = uint32(ts)
			audioFrame.Append([]byte{0x82}, sample.Data)
			err = publisher.WriteAudio(&audioFrame)
		}
		return err
	}

	for loop := 0; loop < p.Loop; loop++ {
		// 每次循环时更新时间戳偏移量以保持连续性
		tsOffset = ts

		demuxerRange.StartTime = p.PullStartTime
		if !p.PullEndTime.IsZero() {
			demuxerRange.EndTime = p.PullEndTime
		} else if p.MaxTS > 0 {
			demuxerRange.EndTime = p.PullStartTime.Add(time.Duration(p.MaxTS) * time.Millisecond)
		} else {
			demuxerRange.EndTime = time.Now()
		}
		if err = demuxerRange.Demux(p.Context); err != nil {
			if err == pkg.ErrSkip {
				loop--
				continue
			}
			return err
		}
	}
	return
}
