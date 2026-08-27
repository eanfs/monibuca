package mp4

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"m7s.live/v5/pkg/storage"

	"m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

type DemuxerRange struct {
	*slog.Logger
	StartTime, EndTime     time.Time
	Streams                []m7s.RecordStream
	AudioCodec, VideoCodec codec.ICodecCtx
	OnAudio, OnVideo       func(box.Sample) error
	OnCodec                func(codec.ICodecCtx, codec.ICodecCtx)
	StorageResolver        func(string) (storage.Storage, error)
}

func (d *DemuxerRange) downloadRemoteFile(ctx context.Context, rawURL string) (storage.File, func(), error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, func() {}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, func() {}, err
	}
	defer response.Body.Close()

	tmpFile, err := os.CreateTemp("", "mp4-*.tmp")
	if err != nil {
		return nil, func() {}, err
	}
	tmpPath := tmpFile.Name()
	if _, err = io.Copy(tmpFile, response.Body); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return nil, func() {}, err
	}
	if err = tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, func() {}, err
	}

	localFile, err := os.Open(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, func() {}, err
	}
	file := &storage.LocalFile{File: localFile}
	return file, func() {
		file.Close()
		os.Remove(tmpPath)
	}, nil
}

func (d *DemuxerRange) openRecordFile(ctx context.Context, stream m7s.RecordStream) (storage.File, func(), error) {
	if strings.HasPrefix(stream.FilePath, "http://") || strings.HasPrefix(stream.FilePath, "https://") {
		return d.downloadRemoteFile(ctx, stream.FilePath)
	}
	if filepath.IsAbs(stream.FilePath) {
		file, err := os.Open(stream.FilePath)
		if err != nil {
			return nil, func() {}, err
		}
		return &storage.LocalFile{File: file}, func() { file.Close() }, nil
	}
	if d.StorageResolver == nil {
		return nil, func() {}, storage.ErrStorageNotAvailable
	}
	st, err := d.StorageResolver(stream.StorageType)
	if err != nil {
		return nil, func() {}, err
	}
	if stream.StorageType == "" || stream.StorageType == string(storage.StorageTypeLocal) {
		if local, ok := st.(*storage.LocalStorage); ok {
			file, openErr := os.Open(local.GetFullPath(stream.FilePath, stream.StorageLevel))
			if openErr != nil {
				return nil, func() {}, openErr
			}
			return &storage.LocalFile{File: file}, func() { file.Close() }, nil
		}
	}
	file, err := st.OpenFile(ctx, stream.FilePath)
	if err != nil {
		return nil, func() {}, err
	}
	return file, func() { file.Close() }, nil
}

func (d *DemuxerRange) Demux(ctx context.Context) error {
	var ts, tsOffset int64
	var audioInitialized, videoInitialized bool
	for _, stream := range d.Streams {
		// 检查流的时间范围是否在指定范围内
		if stream.EndTime.Before(d.StartTime) || stream.StartTime.After(d.EndTime) {
			continue
		}
		file, cleanup, err := d.openRecordFile(ctx, stream)
		if err != nil {
			d.Error("open record segment failed", "storageType", stream.StorageType, "path", stream.FilePath, "err", err)
			continue
		}

		// 保存上一个文件的最后时间戳，用于跨文件连续
		baseOffset := ts
		demuxer := NewDemuxer(file)
		if err := demuxer.Demux(); err != nil {
			cleanup()
			return err
		}

		// 处理每个轨道的额外数据 (序列头)，并检查是否需要初始化
		var newAudio, newVideo codec.ICodecCtx
		for _, track := range demuxer.Tracks {
			if track.Cid.IsAudio() {
				d.AudioCodec = track.ICodecCtx
				if !audioInitialized {
					newAudio = track.ICodecCtx
					audioInitialized = true
				}
			} else {
				d.VideoCodec = track.ICodecCtx
				if !videoInitialized {
					newVideo = track.ICodecCtx
					videoInitialized = true
				}
			}
		}
		// 只对新发现的音频或视频调用 OnCodec
		if (newAudio != nil || newVideo != nil) && d.OnCodec != nil {
			d.OnCodec(newAudio, newVideo)
		}

		// 计算起始时间戳偏移（用于 Seek）
		var seekOffset int64
		if !d.StartTime.IsZero() {
			startTimestamp := d.StartTime.Sub(stream.StartTime).Milliseconds()
			if startTimestamp < 0 {
				startTimestamp = 0
			}
			if startSample, err := demuxer.SeekTime(uint64(startTimestamp)); err == nil {
				seekOffset = -int64(startSample.Timestamp)
			}
		}
		// 合并偏移：跨文件连续偏移 + Seek 偏移
		tsOffset = baseOffset + seekOffset

		// 读取和处理样本
		for track, sample := range demuxer.ReadSample {
			if ctx.Err() != nil {
				cleanup()
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
			data := demuxer.mdat.Data[sampleOffset : sampleOffset+sample.Size]
			sample.Buffers = net.Buffers{data}

			// 计算时间戳
			if int64(sample.Timestamp)+tsOffset < 0 {
				ts = 0
			} else {
				ts = int64(sample.Timestamp) + tsOffset
			}
			sample.Timestamp = uint32(ts)
			if track.Cid.IsAudio() {
				if err := d.OnAudio(sample); err != nil {
					cleanup()
					return err
				}
			} else {
				if err := d.OnVideo(sample); err != nil {
					cleanup()
					return err
				}
			}
		}
		cleanup()
	}
	return nil
}

type DemuxerConverterRange[TA pkg.IAVFrame, TV pkg.IAVFrame] struct {
	DemuxerRange
	OnAudio func(TA) error
	OnVideo func(TV) error
}

func (d *DemuxerConverterRange[TA, TV]) Demux(ctx context.Context) error {
	var targetAudio TA
	var targetVideo TV

	targetAudioType, targetVideoType := reflect.TypeOf(targetAudio).Elem(), reflect.TypeOf(targetVideo).Elem()
	d.DemuxerRange.OnAudio = func(audio box.Sample) error {
		targetAudio = reflect.New(targetAudioType).Interface().(TA) // TODO: reuse
		var audioFrame AudioFrame
		audioFrame.ICodecCtx = d.AudioCodec
		audioFrame.BaseSample = &pkg.BaseSample{}
		audioFrame.Raw = &audio.Memory
		audioFrame.SetTS32(audio.Timestamp)
		err := pkg.ConvertFrameType(&audioFrame, targetAudio)
		if err == nil {
			err = d.OnAudio(targetAudio)
		}
		return err
	}
	d.DemuxerRange.OnVideo = func(video box.Sample) error {
		targetVideo = reflect.New(targetVideoType).Interface().(TV) // TODO: reuse
		var videoFrame VideoFrame
		videoFrame.ICodecCtx = d.VideoCodec
		videoFrame.BaseSample = &pkg.BaseSample{}
		videoFrame.Raw = &video.Memory
		videoFrame.SetTS32(video.Timestamp)
		videoFrame.IDR = video.KeyFrame
		videoFrame.CTS = time.Duration(video.CTS) / time.Millisecond
		err := pkg.ConvertFrameType(&videoFrame, targetVideo)
		if err == nil {
			err = d.OnVideo(targetVideo)
		}
		return err
	}
	return d.DemuxerRange.Demux(ctx)
}
