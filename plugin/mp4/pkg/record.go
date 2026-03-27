package mp4

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	task "github.com/langhuihui/gotask"
	"gorm.io/gorm"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
	"m7s.live/v5/pkg/util"
	"m7s.live/v5/plugin/mp4/pkg/box"

	"github.com/langhuihui/gomem"
)

type WriteTrailerQueueTask struct {
	task.Work
}

var writeTrailerQueueTask WriteTrailerQueueTask

type writeTrailerTask struct {
	task.Task
	muxer      *Muxer
	file       storage.File
	filePath   string
	durationMs uint32   // 录像时长（毫秒），用于上传 S3 元数据
	streamPath string   // 关联流路径（用于失败追踪）
	storageKey string   // 存储类型 key（s3/oss/cos/local）
	db         *gorm.DB // 数据库连接（用于保存失败记录）
}

func (task *writeTrailerTask) Start() (err error) {
	task.Info("write trailer start")
	if err = task.muxer.WriteTrailer(task.file); err != nil {
		task.Error("write trailer", "err", err)
		// 关闭文件，忽略关闭错误以保留原始错误
		if task.file != nil {
			task.file.Close()
			task.file = nil
		}
	}
	return
}

const BeforeMdatData = 16 // free box + mdat box header or big mdat box header

// 将 moov 从末尾移动到前方
// 将 ftyp + free(optional) + moov + mdat 写入临时文件, 然后替换原文件
// 采用先写后替换策略：完整写入临时文件并验证后才覆盖原文件，确保原子性
func (t *writeTrailerTask) Run() (err error) {
	t.Info("write trailer")

	// 确保任何错误路径下 t.file 都被关闭
	defer func() {
		if err != nil && t.file != nil {
			t.file.Close()
			t.file = nil
		}
	}()

	var temp *os.File
	temp, err = os.CreateTemp("", "*.mp4")
	if err != nil {
		t.Error("create temp file", "err", err)
		return
	}
	tempPath := temp.Name()
	// 错误时保留临时文件用于手动恢复，成功时删除
	tempCleanup := true
	defer func() {
		temp.Close()
		if tempCleanup {
			os.Remove(tempPath)
		} else {
			t.Error("preserving temp file for recovery", "tempPath", tempPath)
		}
	}()

	_, err = t.file.Seek(0, io.SeekStart)
	if err != nil {
		t.Error("seek file", "err", err)
		tempCleanup = false
		return
	}
	// 复制 mdat box之前的内容
	_, err = io.CopyN(temp, t.file, int64(t.muxer.mdatOffset)-BeforeMdatData)
	if err != nil {
		t.Error("copy pre-mdat data", "err", err)
		tempCleanup = false
		return
	}
	for _, track := range t.muxer.Tracks {
		for i := range len(track.Samplelist) {
			track.Samplelist[i].Offset += int64(t.muxer.moov.Size())
		}
	}
	err = t.muxer.WriteMoov(temp)
	if err != nil {
		t.Error("write moov to temp", "err", err)
		tempCleanup = false
		return
	}
	// 复制 mdat box
	_, err = io.CopyN(temp, t.file, int64(t.muxer.mdatSize)+BeforeMdatData)
	if err != nil {
		if err == pkg.ErrSkip {
			return task.ErrTaskComplete
		}
		t.Error("copy mdat data", "err", err)
		tempCleanup = false
		return
	}

	// 验证临时文件完整性
	tempStat, statErr := temp.Stat()
	if statErr != nil {
		err = statErr
		t.Error("stat temp file", "err", err)
		tempCleanup = false
		return
	}
	expectedSize := tempStat.Size()
	if expectedSize == 0 {
		err = fmt.Errorf("temp file is empty after MOOV rewrite")
		t.Error("temp file empty", "err", err)
		tempCleanup = false
		return
	}

	// 临时文件已包含完整数据，现在安全覆盖原文件
	if _, err = t.file.Seek(0, io.SeekStart); err != nil {
		t.Error("seek file for overwrite", "err", err)
		tempCleanup = false
		return
	}
	if _, err = temp.Seek(0, io.SeekStart); err != nil {
		t.Error("seek temp file", "err", err)
		tempCleanup = false
		return
	}
	written, copyErr := io.Copy(t.file, temp)
	if copyErr != nil {
		err = copyErr
		t.Error("copy temp to file", "err", err, "written", written, "expected", expectedSize)
		tempCleanup = false // 保留临时文件用于恢复
		return
	}
	if written != expectedSize {
		err = fmt.Errorf("MOOV rewrite incomplete: expected %d bytes, wrote %d", expectedSize, written)
		t.Error("incomplete overwrite", "err", err)
		tempCleanup = false
		return
	}

	// 在关闭前设置元数据（文件大小 + 时长）
	metadata := map[string]string{
		"video-size-bytes": fmt.Sprintf("%d", expectedSize),
	}
	t.file.SetMetadata("video-size-bytes", fmt.Sprintf("%d", expectedSize))
	if t.durationMs > 0 {
		metadata["video-duration-ms"] = fmt.Sprintf("%d", t.durationMs)
		t.file.SetMetadata("video-duration-ms", fmt.Sprintf("%d", t.durationMs))
	}
	if err = t.file.Close(); err != nil {
		t.Error("upload failed after retries", "err", err,
			"filePath", t.filePath, "streamPath", t.streamPath,
			"storageType", t.storageKey, "durationMs", t.durationMs)
		t.file = nil
		// 将 MOOV 重写后的临时文件移到暂存目录，保存失败记录到 DB 供定时补传
		if tempCleanup && t.db != nil {
			// tempCleanup=true 意味着 MOOV 临时文件还在，可以用来补传
			pendingPath, moveErr := storage.MoveToPendingDir(tempPath)
			if moveErr == nil {
				tempCleanup = false // 已移走，不需 defer 删除
				m7s.SaveFailedUpload(t.db, pendingPath, t.filePath, t.storageKey,
					t.streamPath, expectedSize, t.durationMs, metadata, err)
				t.Info("saved failed upload for retry",
					"pendingPath", pendingPath, "objectKey", t.filePath)
			} else {
				t.Error("move to pending dir failed", "err", moveErr)
			}
		}
		return
	}
	t.file = nil
	return
}

func init() {
	m7s.Servers.AddTask(&writeTrailerQueueTask)
}

func NewRecorder(conf config.Record) m7s.IRecorder {
	return &Recorder{}
}

type Recorder struct {
	m7s.DefaultRecorder
	muxer           *Muxer
	file            storage.File
	firstVideoFrame bool // 标记是否是第一个视频帧
}

func (r *Recorder) writeTailer(end time.Time) {
	r.WriteTail(end, &writeTrailerQueueTask)
	var db *gorm.DB
	if r.RecordJob.Plugin != nil {
		db = r.RecordJob.Plugin.DB
	}
	st := r.RecordJob.GetStorage()
	var storageKey string
	if st != nil {
		storageKey = st.GetKey()
	}
	writeTrailerQueueTask.AddTask(&writeTrailerTask{
		muxer:      r.muxer,
		file:       r.file,
		filePath:   r.Event.FilePath,
		durationMs: r.Event.Duration,
		streamPath: r.Event.StreamPath,
		storageKey: storageKey,
		db:         db,
	}, r.Logger)
}

var CustomFileName = func(job *m7s.RecordJob) string {
	// 如果指定了文件名，使用指定的文件名
	if fn := job.RecConf.FileName; fn != "" {
		// 安全验证：清理文件名，移除路径分隔符，防止路径遍历攻击
		fn = filepath.Base(fn)
		// 验证文件名不为空且不是特殊路径
		if fn == "" || fn == "." || fn == ".." {
			// 回退到默认命名
			goto defaultNaming
		}
		// 确保文件名包含 .mp4 扩展名
		if !strings.HasSuffix(strings.ToLower(fn), ".mp4") {
			fn = fn + ".mp4"
		}
		return filepath.Join(job.RecConf.FilePath, fn)
	}
defaultNaming:
	// 否则使用时间戳生成文件名
	now := time.Now()
	return filepath.Join(job.RecConf.FilePath, fmt.Sprintf("%s_%09d.mp4", time.Now().Local().Format("2006-01-02-15-04-05"), now.Nanosecond()))
}

func (r *Recorder) createStream(start time.Time) (err error) {
	if r.RecordJob.RecConf.Type == "" {
		r.RecordJob.RecConf.Type = "mp4"
	}
	err = r.CreateStream(start, CustomFileName)
	if err != nil {
		return
	}

	// 注意: 不要在这里关闭旧文件,因为它已经被传递给 writeTrailerTask
	// writeTrailerTask 会负责关闭旧文件
	// 直接创建新文件并覆盖 r.file

	// 获取存储实例
	st := r.RecordJob.GetStorage()

	if st == nil {
		return fmt.Errorf("global storage is nil")
	}
	// 使用存储抽象层
	r.file, err = st.CreateFile(context.Background(), r.Event.FilePath)
	if err != nil {
		return
	}

	if r.Event.Type == "fmp4" {
		r.muxer = NewMuxerWithStreamPath(FLAG_FRAGMENT, r.Event.StreamPath)
	} else {
		r.muxer = NewMuxerWithStreamPath(0, r.Event.StreamPath)
	}

	r.firstVideoFrame = true // 重置第一个视频帧标志
	return r.muxer.WriteInitSegment(r.file)
}

func (r *Recorder) Dispose() {
	if r.muxer != nil {
		r.writeTailer(time.Now())
		// 注意: 文件的关闭由 writeTrailerTask.Run() 负责
		// 不在这里关闭,避免在异步任务执行前文件被关闭
	} else {
		// 如果没有 muxer,需要在这里关闭文件
		if r.file != nil {
			r.file.Close()
		}
	}
}

func (r *Recorder) Run() (err error) {
	recordJob := &r.RecordJob
	sub := recordJob.Subscriber
	var audioTrack, videoTrack *Track
	var at, vt *pkg.AVTrack
	// totalElapsedMs 累计整个录制任务的时长（毫秒），不受分片 ResetAbsTime 的影响
	var totalElapsedMs uint32
	var lastAbsTimeMs uint32 // 上次分片时的 absTime 基线
	checkEventRecordStop := func(absTime uint32) (err error) {
		if absTime >= recordJob.Event.AfterDuration+recordJob.Event.BeforeDuration {
			r.RecordJob.Stop(task.ErrStopByUser)
		}
		return
	}

	checkDurationStop := func(absTime uint32) error {
		if recordJob.RecConf.Duration > 0 {
			elapsed := totalElapsedMs + (absTime - lastAbsTimeMs)
			if time.Duration(elapsed)*time.Millisecond >= recordJob.RecConf.Duration {
				r.Info("duration reached, stopping recording",
					"duration", recordJob.RecConf.Duration,
					"elapsed", time.Duration(elapsed)*time.Millisecond)
				return task.ErrTaskComplete
			}
		}
		return nil
	}

	checkFragment := func(reader *pkg.AVRingReader) (err error) {
		if duration := int64(reader.AbsTime); time.Duration(duration)*time.Millisecond >= recordJob.RecConf.Fragment {
			// 分片前累计已过去的时长
			totalElapsedMs += reader.AbsTime - lastAbsTimeMs
			r.writeTailer(reader.Value.WriteTime)
			err = r.createStream(reader.Value.WriteTime)
			if err != nil {
				return
			}
			at, vt = nil, nil
			if vr := sub.VideoReader; vr != nil {
				vr.ResetAbsTime()
			}
			if ar := sub.AudioReader; ar != nil {
				ar.ResetAbsTime()
			}
			lastAbsTimeMs = 0
		}
		return
	}

	return m7s.PlayBlock(sub, func(audio *AudioFrame) error {
		if r.Event.StartTime.IsZero() {
			err = r.createStream(sub.AudioReader.Value.WriteTime)
			if err != nil {
				return err
			}
		}
		r.Event.Duration = sub.AudioReader.AbsTime
		if sub.VideoReader == nil {
			if recordJob.Event != nil {
				err = checkEventRecordStop(sub.AudioReader.AbsTime)
				if err != nil {
					return err
				}
			}
			if recordJob.RecConf.Duration > 0 {
				if err = checkDurationStop(sub.AudioReader.AbsTime); err != nil {
					return err
				}
			}
			if recordJob.RecConf.Fragment != 0 {
				err = checkFragment(sub.AudioReader)
				if err != nil {
					return err
				}
			}
		}
		if at == nil {
			at = sub.AudioReader.Track
			switch at.ICodecCtx.GetBase().(type) {
			case *codec.AACCtx:
				track := r.muxer.AddTrack(box.MP4_CODEC_AAC)
				audioTrack = track
				track.ICodecCtx = at.ICodecCtx
			case *codec.PCMACtx:
				track := r.muxer.AddTrack(box.MP4_CODEC_G711A)
				audioTrack = track
				track.ICodecCtx = at.ICodecCtx
			case *codec.PCMUCtx:
				track := r.muxer.AddTrack(box.MP4_CODEC_G711U)
				audioTrack = track
				track.ICodecCtx = at.ICodecCtx
			}
			if audioTrack == nil {
				return fmt.Errorf("unsupported audio codec for mp4 record: %T", at.ICodecCtx.GetBase())
			}
		}
		sample := box.Sample{
			Timestamp: sub.AudioReader.AbsTime,
			Memory:    audio.Memory,
		}
		return r.muxer.WriteSample(r.file, audioTrack, sample)
	}, func(video *VideoFrame) error {
		if r.Event.StartTime.IsZero() {
			err = r.createStream(sub.VideoReader.Value.WriteTime)
			if err != nil {
				return err
			}
		}
		r.Event.Duration = sub.VideoReader.AbsTime
		if sub.VideoReader.Value.IDR {
			if recordJob.Event != nil {
				err = checkEventRecordStop(sub.VideoReader.AbsTime)
				if err != nil {
					return err
				}
			}
			if recordJob.RecConf.Duration > 0 {
				if err = checkDurationStop(sub.VideoReader.AbsTime); err != nil {
					return err
				}
			}
			if recordJob.RecConf.Fragment != 0 {
				err = checkFragment(sub.VideoReader)
				if err != nil {
					return err
				}
			}
		}

		if vt == nil {
			vt = sub.VideoReader.Track
			switch video.ICodecCtx.GetBase().(type) {
			case *codec.H264Ctx:
				track := r.muxer.AddTrack(box.MP4_CODEC_H264)
				videoTrack = track
				track.ICodecCtx = video.ICodecCtx
			case *codec.H265Ctx:
				track := r.muxer.AddTrack(box.MP4_CODEC_H265)
				videoTrack = track
				track.ICodecCtx = video.ICodecCtx
			}
			if videoTrack == nil {
				return fmt.Errorf("unsupported video codec for mp4 record: %T", video.ICodecCtx.GetBase())
			}
		}
		//ctx := video.ICodecCtx.(pkg.IVideoCodecCtx)
		//if videoTrackCtx, ok := videoTrack.ICodecCtx.(pkg.IVideoCodecCtx); ok && videoTrackCtx != ctx {
		//	width, height := uint32(ctx.Width()), uint32(ctx.Height())
		//	oldWidth, oldHeight := uint32(videoTrackCtx.Width()), uint32(videoTrackCtx.Height())
		//	r.Info("ctx  changed, restarting recording",
		//		"old", fmt.Sprintf("%dx%d", oldWidth, oldHeight),
		//		"new", fmt.Sprintf("%dx%d", width, height))
		//	r.writeTailer(sub.VideoReader.Value.WriteTime)
		//	err = r.createStream(sub.VideoReader.Value.WriteTime)
		//	if err != nil {
		//		return nil
		//	}
		//	at, vt = nil, nil
		//	if vr := sub.VideoReader; vr != nil {
		//		vr.ResetAbsTime()
		//		vt = vr.Track
		//		switch video.ICodecCtx.GetBase().(type) {
		//		case *codec.H264Ctx:
		//			track := r.muxer.AddTrack(box.MP4_CODEC_H264)
		//			videoTrack = track
		//			track.ICodecCtx = video.ICodecCtx
		//		case *codec.H265Ctx:
		//			track := r.muxer.AddTrack(box.MP4_CODEC_H265)
		//			videoTrack = track
		//			track.ICodecCtx = video.ICodecCtx
		//		}
		//	}
		//	if ar := sub.AudioReader; ar != nil {
		//		ar.ResetAbsTime()
		//	}
		//}
		sample := box.Sample{
			Timestamp: sub.VideoReader.AbsTime,
			KeyFrame:  video.IDR,
			CTS:       video.GetCTS32(),
			Memory:    video.Memory,
		}
		// 如果是第一个视频 I 帧，将参数集放在 I 帧前面一起写入
		//if r.firstVideoFrame && video.IDR {
		if video.IDR {
			// 创建包含参数集的 Memory
			var combinedMemory gomem.Memory
			var naluSizeLen int = 4
			var sps, pps, vps []byte

			switch ctx := video.ICodecCtx.GetBase().(type) {
			case *codec.H264Ctx:
				naluSizeLen = int(ctx.RecordInfo.LengthSizeMinusOne) + 1
				sps = ctx.SPS()
				pps = ctx.PPS()
				if len(sps) > 0 && len(pps) > 0 {
					// 写入 SPS
					sizeBuf := make([]byte, naluSizeLen)
					util.PutBE(sizeBuf, uint32(len(sps)))
					combinedMemory.Push(sizeBuf)
					combinedMemory.Push(sps)
					// 写入 PPS
					sizeBuf = make([]byte, naluSizeLen)
					util.PutBE(sizeBuf, uint32(len(pps)))
					combinedMemory.Push(sizeBuf)
					combinedMemory.Push(pps)
				}
			case *codec.H265Ctx:
				naluSizeLen = int(ctx.RecordInfo.LengthSizeMinusOne) + 1
				vps = ctx.VPS()
				sps = ctx.SPS()
				pps = ctx.PPS()
				if len(vps) > 0 && len(sps) > 0 && len(pps) > 0 {
					// 写入 VPS
					sizeBuf := make([]byte, naluSizeLen)
					util.PutBE(sizeBuf, uint32(len(vps)))
					combinedMemory.Push(sizeBuf)
					combinedMemory.Push(vps)
					// 写入 SPS
					sizeBuf = make([]byte, naluSizeLen)
					util.PutBE(sizeBuf, uint32(len(sps)))
					combinedMemory.Push(sizeBuf)
					combinedMemory.Push(sps)
					// 写入 PPS
					sizeBuf = make([]byte, naluSizeLen)
					util.PutBE(sizeBuf, uint32(len(pps)))
					combinedMemory.Push(sizeBuf)
					combinedMemory.Push(pps)
				}
			}
			// 将原始视频帧数据追加到参数集后面
			combinedMemory.Push(video.Memory.Buffers...)
			sample.Memory = combinedMemory
			r.firstVideoFrame = false
		} else if r.firstVideoFrame {
			r.firstVideoFrame = false
		}
		return r.muxer.WriteSample(r.file, videoTrack, sample)
	})
}
