package mp4

import (
	"errors"
	"io"
	"strings"
	"time"

	"github.com/deepch/vdk/codec/aacparser"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/codec/h265parser"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

type HTTPReader struct {
	m7s.HTTPFilePuller
}

func (p *HTTPReader) Run() (err error) {
	pullJob := &p.PullJob
	publisher := pullJob.Publisher
	if publisher == nil {
		io.Copy(io.Discard, p.ReadCloser)
		return
	}
	allocator := util.NewScalableMemoryAllocator(1 << 10)
	var demuxer *Demuxer
	defer allocator.Recycle()
	switch v := p.ReadCloser.(type) {
	case io.ReadSeeker:
		demuxer = NewDemuxer(v)
	default:
		var content []byte
		content, err = io.ReadAll(p.ReadCloser)
		demuxer = NewDemuxer(strings.NewReader(string(content)))
	}
	if err = demuxer.Demux(); err != nil {
		return
	}
	publisher.OnSeek = func(seekTime time.Time) {
		p.Stop(errors.New("seek"))
		pullJob.Connection.Args.Set(util.StartKey, seekTime.Local().Format(util.LocalTimeFormat))
		newHTTPReader := &HTTPReader{}
		pullJob.AddTask(newHTTPReader)
	}
	if pullJob.Connection.Args.Get(util.StartKey) != "" {
		seekTime, _ := time.Parse(util.LocalTimeFormat, pullJob.Connection.Args.Get(util.StartKey))
		demuxer.SeekTime(uint64(seekTime.UnixMilli()))
	}
	for _, track := range demuxer.Tracks {
		switch track.Cid {
		case box.MP4_CODEC_H264:
			var h264Ctx codec.H264Ctx
			h264Ctx.CodecData, err = h264parser.NewCodecDataFromAVCDecoderConfRecord(track.ExtraData)
			if err == nil {
				publisher.SetCodecCtx(&h264Ctx, &Video{})
			}
		case box.MP4_CODEC_H265:
			var h265Ctx codec.H265Ctx
			h265Ctx.CodecData, err = h265parser.NewCodecDataFromAVCDecoderConfRecord(track.ExtraData)
			if err == nil {
				publisher.SetCodecCtx(&h265Ctx, &Video{
					allocator: allocator,
				})
			}
		case box.MP4_CODEC_AAC:
			var aacCtx codec.AACCtx
			aacCtx.CodecData, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(track.ExtraData)
			if err == nil {
				publisher.SetCodecCtx(&aacCtx, &Audio{
					allocator: allocator,
				})
			}
		}
	}

	// 计算最大时间戳用于累计偏移
	var maxTimestamp uint64
	for track, sample := range demuxer.ReadSample {
		timestamp := uint64(sample.Timestamp) * 1000 / uint64(track.Timescale)
		if timestamp > maxTimestamp {
			maxTimestamp = timestamp
		}
	}
	var timestampOffset uint64
	loop := p.PullJob.Loop
	for {
		demuxer.ReadSampleIdx = make([]uint32, len(demuxer.Tracks))
		for track, sample := range demuxer.ReadSample {
			if p.IsStopped() {
				return
			}
			if _, err = demuxer.reader.Seek(sample.Offset, io.SeekStart); err != nil {
				return
			}
			sample.Data = allocator.Malloc(sample.Size)
			if _, err = io.ReadFull(demuxer.reader, sample.Data); err != nil {
				allocator.Free(sample.Data)
				return
			}
			fixTimestamp := uint32(uint64(sample.Timestamp)*1000/uint64(track.Timescale) + timestampOffset)
			switch track.Cid {
			case box.MP4_CODEC_H264:
				var videoFrame = Video{
					Sample:    sample,
					allocator: allocator,
				}
				videoFrame.Timestamp = fixTimestamp
				err = publisher.WriteVideo(&videoFrame)
			case box.MP4_CODEC_H265:
				var videoFrame = Video{
					Sample:    sample,
					allocator: allocator,
				}
				videoFrame.Timestamp = fixTimestamp
				err = publisher.WriteVideo(&videoFrame)
			case box.MP4_CODEC_AAC:
				var audioFrame = Audio{
					Sample:    sample,
					allocator: allocator,
				}
				audioFrame.Timestamp = fixTimestamp
				err = publisher.WriteAudio(&audioFrame)
			case box.MP4_CODEC_G711A:
				var audioFrame = Audio{
					Sample:    sample,
					allocator: allocator,
				}
				audioFrame.Timestamp = fixTimestamp
				err = publisher.WriteAudio(&audioFrame)
			case box.MP4_CODEC_G711U:
				var audioFrame = Audio{
					Sample:    sample,
					allocator: allocator,
				}
				audioFrame.Sample = sample
				audioFrame.SetAllocator(allocator)
				audioFrame.Timestamp = fixTimestamp
				err = publisher.WriteAudio(&audioFrame)
			}
		}
		if loop >= 0 {
			loop--
			if loop == -1 {
				break
			}
		}
		// 每次循环后累计时间戳偏移，确保下次循环的时间戳是递增的
		timestampOffset += maxTimestamp + 1
	}
	return
}
