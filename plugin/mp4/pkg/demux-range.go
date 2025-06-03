package mp4

import (
	"context"
	"os"
	"time"

	"m7s.live/v5"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

type DemuxerRange struct {
	StartTime, EndTime time.Time
	Streams            []m7s.RecordStream
	OnAudioExtraData   func(codec box.MP4_CODEC_TYPE, data []byte) error
	OnVideoExtraData   func(codec box.MP4_CODEC_TYPE, data []byte) error
	OnAudioSample      func(codec box.MP4_CODEC_TYPE, sample box.Sample) error
	OnVideoSample      func(codec box.MP4_CODEC_TYPE, sample box.Sample) error
}

func (d *DemuxerRange) Demux(ctx context.Context) error {
	var ts, tsOffset int64

	for _, stream := range d.Streams {
		// 检查流的时间范围是否在指定范围内
		if stream.EndTime.Before(d.StartTime) || stream.StartTime.After(d.EndTime) {
			continue
		}

		tsOffset = ts
		file, err := os.Open(stream.FilePath)
		if err != nil {
			continue
		}
		defer file.Close()

		demuxer := NewDemuxer(file)
		if err = demuxer.Demux(); err != nil {
			return err
		}

		// 处理每个轨道的额外数据 (序列头)
		for _, track := range demuxer.Tracks {
			switch track.Cid {
			case box.MP4_CODEC_H264, box.MP4_CODEC_H265:
				if d.OnVideoExtraData != nil {
					err := d.OnVideoExtraData(track.Cid, track.ExtraData)
					if err != nil {
						return err
					}
				}
			case box.MP4_CODEC_AAC, box.MP4_CODEC_G711A, box.MP4_CODEC_G711U:
				if d.OnAudioExtraData != nil {
					err := d.OnAudioExtraData(track.Cid, track.ExtraData)
					if err != nil {
						return err
					}
				}
			}
		}

		// 计算起始时间戳偏移
		if !d.StartTime.IsZero() {
			startTimestamp := d.StartTime.Sub(stream.StartTime).Milliseconds()
			if startTimestamp < 0 {
				startTimestamp = 0
			}
			if startSample, err := demuxer.SeekTime(uint64(startTimestamp)); err == nil {
				tsOffset = -int64(startSample.Timestamp)
			} else {
				tsOffset = 0
			}
		}

		// 读取和处理样本
		for track, sample := range demuxer.ReadSample {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			// 检查是否超出结束时间
			sampleTime := stream.StartTime.Add(time.Duration(sample.Timestamp) * time.Millisecond)
			if !d.EndTime.IsZero() && sampleTime.After(d.EndTime) {
				break
			}

			// 计算样本数据偏移和读取数据
			sampleOffset := int(sample.Offset) - int(demuxer.mdatOffset)
			if sampleOffset < 0 || sampleOffset+sample.Size > len(demuxer.mdat.Data) {
				continue
			}
			sample.Data = demuxer.mdat.Data[sampleOffset : sampleOffset+sample.Size]

			// 计算时间戳
			if int64(sample.Timestamp)+tsOffset < 0 {
				ts = 0
			} else {
				ts = int64(sample.Timestamp + uint32(tsOffset))
			}
			sample.Timestamp = uint32(ts)

			// 根据轨道类型调用相应的回调函数
			switch track.Cid {
			case box.MP4_CODEC_H264, box.MP4_CODEC_H265:
				if d.OnVideoSample != nil {
					err := d.OnVideoSample(track.Cid, sample)
					if err != nil {
						return err
					}
				}
			case box.MP4_CODEC_AAC, box.MP4_CODEC_G711A, box.MP4_CODEC_G711U:
				if d.OnAudioSample != nil {
					err := d.OnAudioSample(track.Cid, sample)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
