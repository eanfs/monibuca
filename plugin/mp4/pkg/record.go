package mp4

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	task "github.com/eanfs/gotask"
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

// UploadQueueTask 独立上传队列:承载录像文件的最终持久化(Close→对象存储上传→入库)。
// 与 writeTrailerQueueTask 分离的原因:trailer 队列单线程只做磁盘工作,若上传内联
// 其中,一个慢上传会阻塞所有后续录像的 moov 重写/finalize;拆出后每个上传跑在
// 自己的 goroutine 上,并发度由上传槽位(默认 4)真正决定。
type UploadQueueTask struct {
	task.Work
}

var uploadQueueTask UploadQueueTask

type writeTrailerTask struct {
	task.Task
	muxer      *Muxer
	file       storage.File
	filePath   string
	durationMs uint32   // 录像时长（毫秒），用于上传 S3 元数据
	streamPath string   // 关联流路径（用于失败追踪）
	storageKey string   // 存储类型 key（s3/oss/cos/local）
	db         *gorm.DB // 数据库连接（用于保存失败记录）
	// dbWrite 在文件完整写入后执行数据库更新，为 nil 时跳过（无 DB 或测试模式）。
	// 返回 error 表示文件已持久化成功但 record_streams 入库失败（孤儿文件）。
	dbWrite func(tailJob task.IJob) error
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

// 将 moov 从文件末尾移动到文件头：先把 [ftyp][moov][mdat] 写入临时文件，
// 然后让 storage.File 承载这份临时文件。
//
// 阶段 2 优先走 TempFileFinalizer 快路径：临时文件本身已是完整 moov-first MP4，
// 直接移交给 storage.File（对象存储=上传源；本地=rename 到目标路径），省去一次
// 全量回拷。未实现 TempFileFinalizer 的 File 回退到旧的 io.Copy 路径。
func (t *writeTrailerTask) Run() (err error) {
	t.Info("write trailer")

	// 确保任何错误路径下 t.file 都被关闭
	defer func() {
		if err != nil && t.file != nil {
			t.file.Close()
			t.file = nil
		}
	}()

	// fMP4(fragment)文件本身已是最终形态 [ftyp][moov(init)][moof/mdat...][mfra]，
	// 不需要 moov 前移重写；走下方 progressive 全量重写会把它毁掉。
	// 这里只设置元数据并触发最终持久化(对象存储=上传；本地=已就位)。
	if t.muxer.isFragment() {
		var size int64
		if size, err = t.file.Seek(0, io.SeekEnd); err != nil {
			t.Error("seek fragment file", "err", err)
			return
		}
		t.file.SetMetadata("video-size-bytes", fmt.Sprintf("%d", size))
		if t.durationMs > 0 {
			t.file.SetMetadata("video-duration-ms", fmt.Sprintf("%d", t.durationMs))
		}
		var localPath string
		if inserter, ok := t.file.(storage.RangeInserter); ok {
			if fd := inserter.LocalFd(); fd != nil {
				localPath = fd.Name()
			}
		}
		t.handoffToUploadQueue(func(cause error) {
			t.recoverLocalFile(localPath, size, cause)
		})
		return
	}

	// 阶段 A：progressive MP4 优先用 fallocate INSERT_RANGE 原地插 moov，
	// 把 trailer 磁盘写从 O(mdat 全量) 降到 O(moov)。不支持时回退下方全量重写。
	if handled, e := t.runInsertRangeFastPath(); handled {
		return e
	}

	var temp *os.File
	temp, err = os.CreateTemp("", "*.mp4")
	if err != nil {
		t.Error("create temp file", "err", err)
		return
	}
	tempPath := temp.Name()
	// tempOwned 表示 tempPath 文件当前是否仍由本函数负责删除。
	// 移交给 storage.File（FinalizeFromTemp 成功）或移入 pending 目录后置 false。
	tempOwned := true
	defer func() {
		temp.Close()
		if tempOwned {
			os.Remove(tempPath)
		}
	}()

	// ---- 阶段 1：把 [ftyp][moov][mdat] 写入临时文件 ----
	if _, err = t.file.Seek(0, io.SeekStart); err != nil {
		t.Error("seek file", "err", err)
		return
	}
	// trailer 重写后唯一的大块磁盘写入是这笔「写临时文件」。
	// 用限速 writer 包住 temp（速率由 storage.TrailerWriteRateMBps 配置；
	// 未配置时 NewTrailerThrottledWriter 直接返回 temp，零开销）。
	// 外层 bufio 减少写入 syscall（moov 由大量小块组成）。
	bw := bufio.NewWriterSize(storage.NewTrailerThrottledWriter(temp), 1<<20)

	// 复制 mdat box 之前的内容
	if _, err = io.CopyN(bw, t.file, int64(t.muxer.mdatOffset)-BeforeMdatData); err != nil {
		t.Error("copy pre-mdat data", "err", err)
		return
	}
	// moov 前移后 sample 偏移需整体右移；右移可能使偏移跨过 4GB 触发
	// stco→co64 升级、moov 变大,PrepareFrontMoov 内部迭代平移至尺寸收敛。
	moov, moovErr := t.muxer.PrepareFrontMoov()
	if moovErr != nil {
		err = moovErr
		t.Error("prepare front moov", "err", err)
		return
	}
	var moovN int64
	if moovN, err = box.WriteTo(bw, moov); err != nil {
		t.Error("write moov to temp", "err", err)
		return
	}
	t.muxer.CurrentOffset += moovN
	// 复制 mdat box
	if _, err = io.CopyN(bw, t.file, int64(t.muxer.mdatSize)+BeforeMdatData); err != nil {
		if err == pkg.ErrSkip {
			return task.ErrTaskComplete
		}
		t.Error("rewrite with mdat", "err", err)
		return
	}
	if err = bw.Flush(); err != nil {
		t.Error("flush temp file", "err", err)
		return
	}

	// 验证临时文件完整性
	tempStat, statErr := temp.Stat()
	if statErr != nil {
		err = statErr
		t.Error("stat temp file", "err", err)
		return
	}
	expectedSize := tempStat.Size()
	if expectedSize == 0 {
		err = fmt.Errorf("temp file is empty after MOOV rewrite")
		t.Error("temp file empty", "err", err)
		return
	}

	// 在最终持久化前设置元数据（文件大小 + 时长）
	metadata := map[string]string{
		"video-size-bytes": fmt.Sprintf("%d", expectedSize),
	}
	t.file.SetMetadata("video-size-bytes", fmt.Sprintf("%d", expectedSize))
	if t.durationMs > 0 {
		metadata["video-duration-ms"] = fmt.Sprintf("%d", t.durationMs)
		t.file.SetMetadata("video-duration-ms", fmt.Sprintf("%d", t.durationMs))
	}

	// recoverToPending 把已写好的临时文件移入 pending 目录并入库，供定时补传。
	// 用于 FinalizeFromTemp 失败路径:此刻 tempPath 已是完整的 moov-first MP4,
	// 绝不能随 defer 一起删掉。db 为 nil 或移动失败时让 defer 按 tempOwned 处理。
	recoverToPending := func(cause error) {
		if pendingPath, rerr := m7s.RecoverFailedUpload(t.Logger, t.db, tempPath, t.filePath,
			t.storageKey, t.streamPath, expectedSize, t.durationMs, metadata, cause); rerr == nil && pendingPath != "" {
			tempOwned = false // 已移走，不需 defer 删除
		}
	}

	// ---- 阶段 2：让 storage.File 承载这份临时文件 ----
	if finalizer, ok := t.file.(storage.TempFileFinalizer); ok {
		// 快路径：直接移交 tempPath，省去全量回拷。
		if err = finalizer.FinalizeFromTemp(tempPath); err != nil {
			t.Error("finalize from temp", "err", err)
			// FinalizeFromTemp 失败后 storage.File 内部状态不完整（如 S3File
			// 的 tempFile 为 nil），不能再 Close（会触发对空句柄上传）。置 nil
			// 让 err-defer 跳过 Close，并把仍完整的 tempPath 移入 pending 补传。
			t.file = nil
			recoverToPending(err)
			return
		}
		tempOwned = false // 所有权已移交 t.file
	} else {
		// 回退路径：旧的全量回拷（供未实现 TempFileFinalizer 的 File）。
		// 注意：此处的 io.Copy 写入不经限速器（限速只覆盖阶段 1 的临时文件写入）。
		// 当前 local/s3/oss/cos 全部实现了 TempFileFinalizer，此分支为死路径，
		// 仅作未来自定义 File 实现的兜底；若将来有后端走此路径需另行限速。
		if _, err = t.file.Seek(0, io.SeekStart); err != nil {
			t.Error("seek file for overwrite", "err", err)
			return
		}
		if _, err = temp.Seek(0, io.SeekStart); err != nil {
			t.Error("seek temp file", "err", err)
			return
		}
		var written int64
		if written, err = io.Copy(t.file, temp); err != nil {
			t.Error("copy temp to file", "err", err, "written", written, "expected", expectedSize)
			return
		}
		if written != expectedSize {
			err = fmt.Errorf("MOOV rewrite incomplete: expected %d bytes, wrote %d", expectedSize, written)
			t.Error("incomplete overwrite", "err", err)
			return
		}
	}

	// ---- 阶段 3：最终持久化（对象存储=上传）移交独立上传队列,不再阻塞 trailer 队列 ----
	// 上传失败时:MOOV 重写后的文件仍保留在 tempPath（storage.File 在 Close 失败路径
	// 均不删除它），移入 pending 补传;无 DB / pending 满时与旧行为一致清理临时文件。
	t.handoffToUploadQueue(func(cause error) {
		if t.db == nil {
			os.Remove(tempPath) // 无 DB 部署:无补传队列可登记,按旧行为清理
			return
		}
		if _, rerr := m7s.RecoverFailedUpload(t.Logger, t.db, tempPath, t.filePath, t.storageKey,
			t.streamPath, expectedSize, t.durationMs, metadata, cause); rerr != nil {
			os.Remove(tempPath) // pending 目录满等异常(已告警):清理避免临时文件堆积
		}
	})
	return
}

// shiftSampleOffsets 把所有 track 的 sample 偏移整体加 delta，
// 用于 INSERT_RANGE 把 mdat 逻辑后移后校正 moov 内的 chunk offset。
func (t *writeTrailerTask) shiftSampleOffsets(delta int64) {
	t.muxer.ShiftSampleOffsets(delta)
}

// writeMoovHead 在 INSERT_RANGE 撑开的 [0,insertLen) 空间写入 [ftyp][moov][free padding]，
// 并把插入后暴露在 [insertLen, ...) 的旧 [ftyp][free] 覆盖为一个 free box。
func (t *writeTrailerTask) writeMoovHead(fd *os.File, ftypBox, moov box.IBox, insertLen, ftypSize, moovSize int64) error {
	padPayload := insertLen - ftypSize - moovSize - box.BasicBoxLen
	if padPayload < 0 {
		return fmt.Errorf("moov head overflow: insertLen=%d ftyp=%d moov=%d", insertLen, ftypSize, moovSize)
	}
	if _, err := fd.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := box.WriteTo(fd, ftypBox, moov, box.CreateFreeBox(make([]byte, padPayload))); err != nil {
		return err
	}
	// 旧头部 [ftyp][free]（mdatOffset-8 字节，不含 mdat box header）插入后位于
	// [insertLen, ...)，覆盖为一个 free box 使 MP4 解析器忽略它。
	oldHeadLen := int64(t.muxer.mdatOffset) - box.BasicBoxLen
	if _, err := fd.Seek(insertLen, io.SeekStart); err != nil {
		return err
	}
	if _, err := box.WriteTo(fd, box.CreateBaseBox(box.TypeFREE, uint64(oldHeadLen))); err != nil {
		return err
	}
	return nil
}

// recoverLocalFile 最终持久化失败后,把仍在本地路径的录像文件登记到 pending
// 目录供定时补传。用于 insert-range 快路径与 fragment(fMP4)finalize 的失败兜底。
func (t *writeTrailerTask) recoverLocalFile(localPath string, fileSize int64, cause error) {
	metadata := map[string]string{"video-size-bytes": fmt.Sprintf("%d", fileSize)}
	if t.durationMs > 0 {
		metadata["video-duration-ms"] = fmt.Sprintf("%d", t.durationMs)
	}
	m7s.RecoverFailedUpload(t.Logger, t.db, localPath, t.filePath, t.storageKey,
		t.streamPath, fileSize, t.durationMs, metadata, cause)
}

// runInsertRangeFastPath 用 fallocate(INSERT_RANGE) 在文件头就地为 moov 撑开空间，
// 不重写 mdat。handled=false 表示未接管（调用方走全量重写 fallback，状态已复原）；
// handled=true 表示已接管，err 为最终结果。
func (t *writeTrailerTask) runInsertRangeFastPath() (handled bool, err error) {
	// 仅标准 progressive MP4 布局 [ftyp32][free8][mdat hdr8] 适用。
	if t.muxer.isFragment() || t.muxer.mdatOffset != 48 || t.muxer.moov == nil {
		return false, nil
	}
	// mdat 触发 64-bit large box 时头部布局非标准，回退。
	if t.muxer.mdatSize+box.BasicBoxLen > 0xFFFFFFFF {
		return false, nil
	}
	inserter, ok := t.file.(storage.RangeInserter)
	if !ok {
		return false, nil
	}
	fd := inserter.LocalFd()
	if fd == nil {
		return false, nil
	}
	stat, statErr := fd.Stat()
	if statErr != nil {
		return false, nil
	}
	preLen := stat.Size() // Start() 后文件长 = mdatOffset + mdatSize + 尾部 moovSize

	ftypBox := t.muxer.CreateFTYPBox()
	ftypSize := int64(ftypBox.Size())
	moovSize := int64(t.muxer.moov.Size())
	const blk = 4096
	insertLen := ((ftypSize + moovSize + box.BasicBoxLen + blk - 1) / blk) * blk

	// mdat 逻辑后移 insertLen，重算 moov 内 chunk offset。
	t.shiftSampleOffsets(insertLen)
	moov := t.muxer.MakeMoov()
	if int64(moov.Size()) != moovSize {
		// 偏移跨 4GB 致 stco→co64、moov 变大，insertLen 失准——撤销并回退全量重写。
		t.shiftSampleOffsets(-insertLen)
		t.muxer.MakeMoov()
		t.Info("insert-range skipped: moov size changed", "filePath", t.filePath)
		return false, nil
	}

	// —— 此后 INSERT_RANGE 改写底层文件 ——
	if e := storage.InsertRange(fd, 0, insertLen); e != nil {
		t.shiftSampleOffsets(-insertLen)
		t.muxer.MakeMoov()
		if !errors.Is(e, storage.ErrRangeInsertUnsupported) {
			t.Warn("insert-range failed, falling back to full rewrite", "err", e)
		}
		return false, nil
	}

	localPath := fd.Name()
	uploadSize := preLen + insertLen - moovSize // 截掉尾部 moov 后的最终大小

	if writeErr := t.writeMoovHead(fd, ftypBox, moov, insertLen, ftypSize, moovSize); writeErr != nil {
		// 写头失败：撤销插入，退回 moov-在-尾的可播文件再上传。
		t.Error("insert-range write head failed, rolling back", "err", writeErr)
		if ce := storage.CollapseRange(fd, 0, insertLen); ce != nil {
			t.Error("insert-range rollback failed, file may be corrupted", "err", ce)
			t.file.Close()
			t.file = nil
			err = fmt.Errorf("insert-range write+rollback failed: %w", writeErr)
			t.recoverLocalFile(localPath, preLen, err)
			return true, err
		}
		t.shiftSampleOffsets(-insertLen)
		t.muxer.MakeMoov()
		t.Warn("insert-range rolled back, uploading moov-at-tail file", "filePath", t.filePath)
		uploadSize = preLen
	} else {
		// 写头成功：截掉 Start() 留在文件尾的 moov 兜底副本。
		if e := fd.Truncate(uploadSize); e != nil {
			t.Error("insert-range truncate tail moov failed (non-fatal)", "err", e)
		}
		if e := fd.Sync(); e != nil {
			t.Error("insert-range sync failed (non-fatal)", "err", e)
		}
	}

	t.file.SetMetadata("video-size-bytes", fmt.Sprintf("%d", uploadSize))
	if t.durationMs > 0 {
		t.file.SetMetadata("video-duration-ms", fmt.Sprintf("%d", t.durationMs))
	}

	// 持久化（对象存储=上传）移交独立上传队列,不阻塞 trailer 队列。
	t.handoffToUploadQueue(func(cause error) {
		t.recoverLocalFile(localPath, uploadSize, cause)
	})
	t.Info("insert-range fast path done, upload handed off",
		"filePath", t.filePath, "moovBytes", moovSize, "insertedBytes", insertLen)
	return true, nil
}

func init() {
	m7s.Servers.AddTask(&writeTrailerQueueTask)
	m7s.Servers.AddTask(&uploadQueueTask)
}

// finalizeUploadTask 在独立 goroutine(Go)中执行单个录像文件的最终持久化:
// Close 触发对象存储上传(带槽位并发控制与重试),成功后执行延迟入库,
// 失败经 onFail 登记 pending 补传。
type finalizeUploadTask struct {
	task.Task
	file       storage.File
	filePath   string
	streamPath string
	db         *gorm.DB
	dbWrite    func(tailJob task.IJob) error
	onFail     func(cause error)
}

func (u *finalizeUploadTask) Go() (err error) {
	if err = u.file.Close(); err != nil {
		u.Error("upload failed after retries", "err", err,
			"filePath", u.filePath, "streamPath", u.streamPath)
		u.file = nil
		if u.onFail != nil {
			u.onFail(err)
		}
		return
	}
	u.file = nil
	// 文件已完整持久化,此时才将记录写入数据库(延迟入库,确保 DB 与可播放文件一致)。
	if u.dbWrite != nil {
		if dbErr := u.dbWrite(&uploadQueueTask); dbErr != nil {
			u.Error("upload ok but db record save failed — orphan file",
				"err", dbErr, "filePath", u.filePath, "streamPath", u.streamPath)
			m7s.RaiseUploadAlarm(u.db, config.AlarmStorageException,
				"record db save failed", u.streamPath, u.filePath,
				"录像已上传成功但 record_streams 入库失败(孤儿文件): "+dbErr.Error())
		}
	}
	return
}

// handoffToUploadQueue 把 t.file 的最终持久化移交独立上传队列,trailer 队列
// 立即空出处理下一个录像的磁盘工作。onFail 用于上传失败后的补传登记。
func (t *writeTrailerTask) handoffToUploadQueue(onFail func(cause error)) {
	ft := &finalizeUploadTask{
		file:       t.file,
		filePath:   t.filePath,
		streamPath: t.streamPath,
		db:         t.db,
		dbWrite:    t.dbWrite,
		onFail:     onFail,
	}
	t.file = nil // 所有权移交
	uploadQueueTask.AddTask(ft, t.Logger)
}

func NewRecorder(conf config.Record) m7s.IRecorder {
	return &Recorder{}
}

type bufferedSample struct {
	isAudio  bool
	codecCtx codec.ICodecCtx
	sample   box.Sample
}

type Recorder struct {
	m7s.DefaultRecorder
	muxer           *Muxer
	file            storage.File
	firstVideoFrame bool // 标记是否是第一个视频帧
	creating        bool
	createDone      chan error
	sampleBuffer    []bufferedSample
}

func (r *Recorder) writeTailer(end time.Time) {
	// WriteTailDeferred 仅设置 EndTime 并返回延迟 DB 写入闭包，不立即写库。
	// DB 写入将在 writeTrailerTask.Run() 成功（包含对象存储上传）后执行，
	// 确保文件可播放后才入库；本地失败时也不会留下不一致的 DB 记录。
	dbWrite := r.WriteTailDeferred(end)
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
		dbWrite:    dbWrite,
	}, r.Logger.With("filePath", r.Event.FilePath, "streamPath", r.Event.StreamPath))
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
	t0 := time.Now()
	err = r.CreateStream(start, CustomFileName)
	r.Info("createStream step1 CreateStream", "elapsed", time.Since(t0))
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
	t1 := time.Now()
	// 使用存储抽象层
	r.file, err = st.CreateFile(r.Context, r.Event.FilePath)
	r.Info("createStream step2 CreateFile", "elapsed", time.Since(t1), "path", r.Event.FilePath)
	if err != nil {
		return
	}

	if r.Event.Type == "fmp4" {
		r.muxer = NewMuxerWithStreamPath(FLAG_FRAGMENT, r.Event.StreamPath)
	} else {
		r.muxer = NewMuxerWithStreamPath(0, r.Event.StreamPath)
	}
	t2 := time.Now()
	err = r.muxer.WriteInitSegment(r.file)
	r.Info("createStream step3 WriteInitSegment", "elapsed", time.Since(t2))
	r.Info("createStream total", "elapsed", time.Since(t0))
	r.SetDescription("startTime", start.Format("2006-01-02 15:04:05"))
	return
}

func (r *Recorder) Dispose() {
	if r.creating {
		// 异步分片 createStream 正在进行:OLD 文件已在 checkFragment 的 writeTailer 中移交给 writeTrailerTask。
		// 等待 goroutine 结束,避免它在 retry Run() 启动后仍然修改 r.muxer/r.file 造成竞争。
		if r.createDone != nil {
			<-r.createDone
		}
		r.creating = false
		// goroutine 若成功,r.muxer/r.file 已指向新文件(仅含 init segment)。关闭并丢弃它。
		if r.muxer != nil && r.file != nil {
			r.file.Close()
		}
		r.muxer = nil
		r.file = nil
		return
	}
	if r.muxer != nil {
		r.writeTailer(time.Now())
		// 关键修复:将 muxer 和 file 置 nil,切断重试 Run() 对旧 muxer/file 的访问。
		// 文件的关闭由 writeTrailerTask.Run() 负责。若不置 nil,重试 Run() 会向
		// writeTrailerTask 正在处理的同一 muxer 写入新数据,导致 mdatSize 不匹配→EOF。
		r.muxer = nil
		r.file = nil
	} else {
		if r.file != nil {
			r.file.Close()
			r.file = nil
		}
	}
}

func (r *Recorder) Run() (err error) {
	// 重试时清理上一次运行的缓存状态。
	r.sampleBuffer = r.sampleBuffer[:0]
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
		if r.creating {
			return
		}
		if duration := int64(reader.AbsTime); time.Duration(duration)*time.Millisecond >= recordJob.RecConf.Fragment {
			// 分片前累计已过去的时长
			totalElapsedMs += reader.AbsTime - lastAbsTimeMs
			r.writeTailer(reader.Value.WriteTime)
			r.Info("check fragment start async", "absTime", reader.AbsTime, "seq", reader.Value.Sequence)
			startTime := reader.Value.WriteTime
			r.creating = true
			r.createDone = make(chan error, 1)
			r.sampleBuffer = r.sampleBuffer[:0]
			go func() {
				createErr := r.createStream(startTime)
				r.Info("check fragment end async", "err", createErr)
				r.createDone <- createErr
			}()
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

	// flushBuffer 将 createStream 异步执行期间缓存的帧写入新文件
	flushBuffer := func() error {
		for _, bs := range r.sampleBuffer {
			if bs.isAudio {
				if at == nil {
					at = sub.AudioReader.Track
					switch bs.codecCtx.GetBase().(type) {
					case *codec.AACCtx:
						track := r.muxer.AddTrack(box.MP4_CODEC_AAC)
						audioTrack = track
						track.ICodecCtx = bs.codecCtx
					case *codec.PCMACtx:
						track := r.muxer.AddTrack(box.MP4_CODEC_G711A)
						audioTrack = track
						track.ICodecCtx = bs.codecCtx
					case *codec.PCMUCtx:
						track := r.muxer.AddTrack(box.MP4_CODEC_G711U)
						audioTrack = track
						track.ICodecCtx = bs.codecCtx
					}
				}
				if err := r.muxer.WriteSample(r.file, audioTrack, bs.sample); err != nil {
					return err
				}
			} else {
				if vt == nil {
					vt = sub.VideoReader.Track
					switch bs.codecCtx.GetBase().(type) {
					case *codec.H264Ctx:
						track := r.muxer.AddTrack(box.MP4_CODEC_H264)
						videoTrack = track
						track.ICodecCtx = bs.codecCtx
					case *codec.H265Ctx:
						track := r.muxer.AddTrack(box.MP4_CODEC_H265)
						videoTrack = track
						track.ICodecCtx = bs.codecCtx
					}
				}
				if err := r.muxer.WriteSample(r.file, videoTrack, bs.sample); err != nil {
					return err
				}
			}
		}
		r.sampleBuffer = r.sampleBuffer[:0]
		return nil
	}

	return m7s.PlayBlock(sub, func(audio *AudioFrame) error {
		// 用 r.muxer == nil 替代 r.Event.StartTime.IsZero():
		// Dispose() 已将 r.muxer 置 nil,重试时可正确触发新建流,
		// 而 StartTime 在重试时不为零因此无法触发。
		if r.muxer == nil {
			err = r.createStream(sub.AudioReader.Value.WriteTime)
			if err != nil {
				return err
			}
			r.firstVideoFrame = true
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
		sample := box.Sample{
			Timestamp: sub.AudioReader.AbsTime,
			Memory:    audio.Memory,
		}
		// 分片 createStream 异步执行期间将帧写入缓冲区
		if r.creating {
			select {
			case createErr := <-r.createDone:
				r.creating = false
				r.firstVideoFrame = true
				if createErr != nil {
					return createErr
				}
				if err = flushBuffer(); err != nil {
					return err
				}
			default:
				// ring buffer 的内存会被复用,必须深拷贝后再缓存,否则 createStream 完成后
				// flush 时读到的是已被覆盖的数据,导致文件损坏。
				var copiedMem gomem.Memory
				copiedMem.CopyFrom(&sample.Memory)
				sample.Memory = copiedMem
				r.sampleBuffer = append(r.sampleBuffer, bufferedSample{
					isAudio:  true,
					codecCtx: sub.AudioReader.Track.ICodecCtx,
					sample:   sample,
				})
				return nil
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
		return r.muxer.WriteSample(r.file, audioTrack, sample)
	}, func(video *VideoFrame) error {
		if r.muxer == nil {
			err = r.createStream(sub.VideoReader.Value.WriteTime)
			if err != nil {
				return err
			}
			r.firstVideoFrame = true
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

		sample := box.Sample{
			Timestamp: sub.VideoReader.AbsTime,
			KeyFrame:  video.IDR,
			CTS:       video.GetCTS32(),
			Memory:    video.Memory,
		}
		// 如果是视频 I 帧，将参数集放在 I 帧前面一起写入
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
		// 分片 createStream 异步执行期间将帧写入缓冲区
		if r.creating {
			select {
			case createErr := <-r.createDone:
				r.creating = false
				r.firstVideoFrame = true
				if createErr != nil {
					return createErr
				}
				if err = flushBuffer(); err != nil {
					return err
				}
			default:
				// ring buffer 的内存会被复用,必须深拷贝后再缓存,否则 createStream 完成后
				// flush 时读到的是已被覆盖的数据,导致文件损坏。
				var copiedMem gomem.Memory
				copiedMem.CopyFrom(&sample.Memory)
				sample.Memory = copiedMem
				r.sampleBuffer = append(r.sampleBuffer, bufferedSample{
					isAudio:  false,
					codecCtx: sub.VideoReader.Track.ICodecCtx,
					sample:   sample,
				})
				return nil
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
		return r.muxer.WriteSample(r.file, videoTrack, sample)
	})
}
