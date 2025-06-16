package mp4

import (
	"strings"
	"time"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/task"
	"m7s.live/v5/pkg/util"
)

type (
	RecordReader struct {
		m7s.RecordFilePuller
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
		if err = demuxerRange.Demux(p.Context, func(a *Audio) error {
			if !publisher.HasAudioTrack() {
				publisher.SetCodecCtx(demuxerRange.AudioTrack.ICodecCtx, a)
			}
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
			if int64(a.Timestamp)+tsOffset < 0 {
				ts = 0
			} else {
				ts = int64(a.Timestamp) + tsOffset
			}
			a.Timestamp = uint32(ts)
			return publisher.WriteAudio(a)
		}, func(v *Video) error {
			if !publisher.HasVideoTrack() {
				publisher.SetCodecCtx(demuxerRange.VideoTrack.ICodecCtx, v)
			}
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
			if int64(v.Timestamp)+tsOffset < 0 {
				ts = 0
			} else {
				ts = int64(v.Timestamp) + tsOffset
			}

			// 更新实时时间
			realTime = time.Now() // 这里可以根据需要调整为更精确的时间计算
			v.Timestamp = uint32(ts)
			return publisher.WriteVideo(v)
		}); err != nil {
			if err == pkg.ErrSkip {
				loop--
				continue
			}
			return err
		}
	}
	return
}
