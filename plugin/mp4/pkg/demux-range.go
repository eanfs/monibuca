package mp4

import (
	"context"
	"os"
	"time"

	"github.com/deepch/vdk/codec/aacparser"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/codec/h265parser"
	"m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

type DemuxerRange struct {
	StartTime, EndTime     time.Time
	Streams                []m7s.RecordStream
	AudioTrack, VideoTrack *pkg.AVTrack
}

func (d *DemuxerRange) Demux(ctx context.Context, onAudio func(*Audio) error, onVideo func(*Video) error) error {
	var ts, tsOffset int64
	allocator := util.NewScalableMemoryAllocator(1 << 10)
	defer allocator.Recycle()
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
			case box.MP4_CODEC_H264:
				var h264Ctx codec.H264Ctx
				h264Ctx.CodecData, err = h264parser.NewCodecDataFromAVCDecoderConfRecord(track.ExtraData)
				if err == nil {
					if d.VideoTrack == nil {
						d.VideoTrack = pkg.NewAVTrack(&Video{
							allocator: allocator,
						}, &h264Ctx)
					} else {
						// 如果已经有视频轨道，使用现有的轨道
						d.VideoTrack.ICodecCtx = &h264Ctx
					}
				}
			case box.MP4_CODEC_H265:
				var h265Ctx codec.H265Ctx
				h265Ctx.CodecData, err = h265parser.NewCodecDataFromAVCDecoderConfRecord(track.ExtraData)
				if err == nil {
					if d.VideoTrack == nil {
						d.VideoTrack = pkg.NewAVTrack(&Video{
							allocator: allocator,
						}, &h265Ctx)
					} else {
						// 如果已经有视频轨道，使用现有的轨道
						d.VideoTrack.ICodecCtx = &h265Ctx
					}
				}
			case box.MP4_CODEC_AAC:
				var aacCtx codec.AACCtx
				aacCtx.CodecData, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(track.ExtraData)
				if err == nil {
					if d.AudioTrack == nil {
						d.AudioTrack = pkg.NewAVTrack(&Audio{
							allocator: allocator,
						}, &aacCtx)
					} else {
						// 如果已经有音频轨道，使用现有的轨道
						d.AudioTrack.ICodecCtx = &aacCtx
					}
				}
			case box.MP4_CODEC_G711A, box.MP4_CODEC_G711U:
				if d.AudioTrack == nil {
					d.AudioTrack = pkg.NewAVTrack(&Audio{
						allocator: allocator,
					})
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
				if err := onVideo(&Video{
					Sample:    sample,
					allocator: allocator,
				}); err != nil {
					return err
				}
			case box.MP4_CODEC_AAC, box.MP4_CODEC_G711A, box.MP4_CODEC_G711U:
				if err := onAudio(&Audio{
					Sample:    sample,
					allocator: allocator,
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

type DemuxerConverterRange[TA pkg.IAVFrame, TV pkg.IAVFrame] struct {
	DemuxerRange
	audioConverter *pkg.AVFrameConvert[TA]
	videoConverter *pkg.AVFrameConvert[TV]
}

func (d *DemuxerConverterRange[TA, TV]) Demux(ctx context.Context, onAudio func(TA) error, onVideo func(TV) error) error {
	d.DemuxerRange.Demux(ctx, func(audio *Audio) error {
		if d.audioConverter == nil {
			d.audioConverter = pkg.NewAVFrameConvert[TA](d.AudioTrack, nil)
		}
		target, err := d.audioConverter.Convert(audio)
		if err == nil {
			err = onAudio(target)
		}
		return err
	}, func(video *Video) error {
		if d.videoConverter == nil {
			d.videoConverter = pkg.NewAVFrameConvert[TV](d.VideoTrack, nil)
		}
		target, err := d.videoConverter.Convert(video)
		if err == nil {
			err = onVideo(target)
		}
		return err
	})
	return nil
}
