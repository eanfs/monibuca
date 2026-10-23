package mp4

import (
	"bufio"
	"bytes"
	"context"
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

// progressDBInterval：fmp4 进行中切片周期刷新 EndTime 的节流间隔（REQ-MP4-001 方案 B / M1）
const progressDBInterval = 10 * time.Second

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
	// startedAt 是本段录像开片的墙钟时间，用于「录制结束」日志的 elapsedMs。
	// 与 durationMs（媒体时长）不同：断流/卡顿期间墙钟继续走，二者可对照排查。
	startedAt time.Time
	// dbWrite 在文件完整写入后执行数据库更新，为 nil 时跳过（无 DB 或测试模式）。
	// 返回 error 表示文件已持久化成功但 record_streams 入库失败（孤儿文件）。
	dbWrite func(tailJob task.IJob) error
	// indexPath 是本段录像的伴随索引；上传成功后才删除，失败/pending 保留。
	indexPath string
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

// indexFlagKeyFrame 标记索引记录对应一个关键帧（恢复时据此重建 stss）。
// 其余位当前保留，读取方只应依赖这一位。
const indexFlagKeyFrame uint8 = 0x01

// IndexFlagKeyFrame 是 indexFlagKeyFrame 的导出别名，供包外恢复路径
// （无索引 tmp 反向重建）构造 IndexRecord 时使用。
const IndexFlagKeyFrame = indexFlagKeyFrame

// indexFilePath 返回媒体文件对应的 sidecar 路径：<media>.m7sidx。
// 不新增 storage.File 接口，路径由录制器已知的本地媒体路径推导。
func indexFilePath(mediaPath string) string { return mediaPath + IndexExtension }

// absoluteMediaPath 把媒体路径统一成绝对路径。
//
// storage.local.path 可以是 "."（默认）或任意相对目录，此时 LocalFile.Name()
// 是相对路径；若原样写进 sidecar，恢复侧的路径白名单（要求绝对路径）会拒绝，
// 导致 sidecar 常驻、每次启动重试、永远恢复不了。header.FilePath 保持原样：
// 它是逻辑录像路径，用于匹配 record_streams / 对象键。
func absoluteMediaPath(mediaPath string) string {
	if mediaPath == "" {
		return ""
	}
	abs, err := filepath.Abs(mediaPath)
	if err != nil {
		return mediaPath
	}
	return filepath.Clean(abs)
}

// localMediaPath 返回 storage.File 当前数据所在的本地路径。
// 对象存储后端在本地暂存文件（s3writer_*.tmp），sidecar 与它同目录。
// 不新增 storage.File 接口：LocalFd 已由 RangeInserter 暴露。
func localMediaPath(f storage.File) string {
	if f == nil {
		return ""
	}
	if inserter, ok := f.(storage.RangeInserter); ok {
		if fd := inserter.LocalFd(); fd != nil {
			return fd.Name()
		}
	}
	return ""
}

// 将 moov 从文件末尾移动到文件头：先把 [ftyp][moov][mdat] 写入临时文件，
// 然后让 storage.File 承载这份临时文件。
//
// 阶段 2 优先走 TempFileFinalizer 快路径：临时文件本身已是完整 moov-first MP4，
// 直接移交给 storage.File（对象存储=上传源；本地=rename 到目标路径），省去一次
// 全量回拷。未实现 TempFileFinalizer 的 File 回退到旧的 io.Copy 路径。
func (t *writeTrailerTask) Run() (err error) {
	t.Info("write trailer")
	// 「录制结束」事件：在 trailer 改写前记录本段录像的最终统计。
	// size 取改写前的文件长度（已写入的媒体总量），断流/失败路径同样能报出。
	t.logRecordStop(t.recordedSize())

	// 确保任何错误路径下 t.file 都被关闭
	defer func() {
		if err != nil && t.file != nil {
			t.file.Close()
			t.file = nil
		}
	}()

	// trailer 改写前的媒体本地路径。FinalizeFromTemp 可能把上传源换成另一个
	// 临时文件（对象存储），此时需要向 sidecar 追加 RENAME 记录。
	originalMediaPath := localMediaPath(t.file)

	// fMP4(fragment)文件本身已是最终形态 [ftyp][moov(init)][moof/mdat...][mfra]，
	// 不需要 moov 前移重写；走下方 progressive 全量重写会把它毁掉。
	// 这里只设置元数据并触发最终持久化(对象存储=上传；本地=已就位)。
	if t.muxer != nil && t.muxer.isFragment() {
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
		t.handoffToUploadQueue(size, func(cause error) {
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

	// recoverToPending 把已写好的临时文件登记进补传队列。
	// 用于 FinalizeFromTemp 失败路径:此刻 tempPath 已是完整的 moov-first MP4,
	// 绝不能随 defer 一起删掉。tempOwned 由 recoverTempAfterFinalizeFailure
	// 决定，只有「明确没有补传队列」时才允许 defer 删除。
	recoverToPending := func(cause error) {
		tempOwned = t.recoverTempAfterFinalizeFailure(tempPath, expectedSize, metadata, cause)
	}

	// ---- 阶段 2：让 storage.File 承载这份临时文件 ----
	finalizer, ok := t.file.(storage.TempFileFinalizer)
	if !ok {
		// 所有内置后端(local/s3/oss/cos)均实现 TempFileFinalizer。未实现的
		// 自定义 File 直接报错并把完整的 tempPath 移入 pending 补传(补传走
		// UploadLocalFile,不依赖 TempFileFinalizer,数据可恢复)。
		// 不再保留旧的 io.Copy 全量回拷:那条路径的异步失败恢复闭包引用的
		// tempPath 会被 defer 提前删除,存在误删唯一副本的隐患。
		err = fmt.Errorf("storage file %T does not implement TempFileFinalizer", t.file)
		t.Error("finalize from temp", "err", err)
		recoverToPending(err)
		return
	}
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

	t.syncIndexMediaPath(originalMediaPath)
	// moov 前移后 sample 偏移整体右移，同步 sidecar 记录，保证恢复时偏移有效。
	if idxErr := rewriteIndexRecords(t.indexPath, t.muxer, t.durationMs); idxErr != nil {
		t.Error("rewrite index records after trailer", "err", idxErr, "indexPath", t.indexPath)
	}

	// ---- 阶段 3：最终持久化（对象存储=上传）移交独立上传队列,不再阻塞 trailer 队列 ----
	// 上传失败时:MOOV 重写后的文件仍保留在 tempPath（storage.File 在 Close 失败路径
	// 均不删除它），登记补传。
	//
	// 唯一允许删除临时文件的场景是「明确无法补传」：无 DB 部署，没有补传队列
	// 可登记。其余失败（pending 满 / DB 写失败 / 回查失败）一律保留媒体与
	// sidecar——tempPath 是这份录像的唯一副本。
	t.handoffToUploadQueue(expectedSize, func(cause error) {
		if keepMedia, _ := t.handleFailedUpload(tempPath, expectedSize, metadata, cause); keepMedia {
			return
		}
		// 明确没有补传队列：按旧行为清理临时文件，避免堆积（已 Warn）。
		os.Remove(tempPath)
	})
	return
}

// syncIndexMediaPath 在 FinalizeFromTemp 把媒体换成另一个本地文件后，
// 向 sidecar 追加 RENAME 扩展记录。对象存储上传源是 trailer 临时文件；
// 本地存储 rename 到目标路径后路径不变，无需记录。
func (t *writeTrailerTask) syncIndexMediaPath(originalMediaPath string) {
	if t.indexPath == "" {
		return
	}
	newMediaPath := absoluteMediaPath(localMediaPath(t.file))
	if newMediaPath == "" || newMediaPath == absoluteMediaPath(originalMediaPath) {
		return
	}
	if err := appendIndexRename(t.indexPath, newMediaPath); err != nil {
		t.Error("append index rename record failed", "err", err,
			"indexPath", t.indexPath, "mediaPath", newMediaPath)
	}
}

// rewriteIndexRecords 用 muxer 的最终 sample 表重写 sidecar 记录。
// trailer 重写（moov 前移 / INSERT_RANGE）会把 mdat 整体后移，索引里的旧偏移
// 随即失效；重建时以 mediaPath 处的实际文件为准，必须同步成 shift 后的偏移。
// 头部（含最终 MediaPath）从现有 sidecar 读取并原样保留。
func rewriteIndexRecords(indexPath string, m *Muxer, durationMs uint32) error {
	if indexPath == "" || m == nil {
		return nil
	}
	old, err := ReadIndex(indexPath)
	if err != nil {
		return err
	}
	tmpPath := indexPath + ".tmp"
	w, err := CreateIndex(tmpPath, old.Header)
	if err != nil {
		return err
	}
	abort := func(err error) error {
		w.Close()
		os.Remove(tmpPath)
		return err
	}
	var samples uint64
	for id := uint32(1); id < m.nextTrackId; id++ {
		track := m.Tracks[id]
		if track == nil {
			continue
		}
		for _, sample := range track.Samplelist {
			if err := w.Append(indexRecord(track, sample, sample.Offset)); err != nil {
				return abort(err)
			}
			samples++
		}
	}
	if err := w.Commit(durationMs, samples); err != nil {
		return abort(err)
	}
	if err := w.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, indexPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
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

// recoverTempAfterFinalizeFailure 处理 FinalizeFromTemp 失败（tempPath 已是一份
// 完整的 moov-first MP4）：登记补传，并返回 tempPath 是否仍由调用方（defer）
// 负责删除。
//
// 只有「明确没有补传队列」（无 DB / local 存储）时才返回 true。其余情况——
// 包括 pending 目录满、upload_tasks 写不进去、回查失败——tempPath 都是这份
// 录像的唯一副本，返回 false，调用方绝不能删除：曾经这里按「移动成功才交还
// 所有权」判断，移动失败时 defer 会直接删掉唯一副本，而日志还写着
// "keeping media"，数据静默丢失。
func (t *writeTrailerTask) recoverTempAfterFinalizeFailure(tempPath string, fileSize int64, metadata map[string]string, cause error) (deleteTemp bool) {
	keepMedia, _ := t.handleFailedUpload(tempPath, fileSize, metadata, cause)
	return !keepMedia
}

// handleFailedUpload 是「上传失败但本地仍有完整媒体」的唯一保命入口：
// 把 mediaPath 登记进补传队列（优先移入 pending，pending 目录不可用时原地
// 登记），并按媒体的实际位置决定 sidecar 的去留。
//
// 返回值描述调用方必须如何处置 mediaPath：
//   - keepMedia：mediaPath 仍可能是这份录像的唯一副本，调用方绝不能删除它。
//     只有「明确没有补传队列」（无 DB / local 存储）时才返回 false。
//   - movedToPending：媒体已被移入 pending 目录，原路径不再有媒体，
//     sidecar 已作为孤儿一并清理；false 表示媒体仍在原路径，sidecar 保留
//     （它仍指向真实媒体，可作为恢复的额外保险）。
//
// 登记未确认时媒体与 sidecar 全部保留，等下次恢复扫描重试；错误明细已在
// m7s.RecoverFailedUpload 内以 Error 级记录（并负责把媒体退回原路径）。
func (t *writeTrailerTask) handleFailedUpload(mediaPath string, fileSize int64, metadata map[string]string, cause error) (keepMedia, movedToPending bool) {
	// 复制一份再补字段：调用方传入的 map 可能被复用，这里不修改它。
	uploadMetadata := make(map[string]string, len(metadata)+2)
	for key, value := range metadata {
		uploadMetadata[key] = value
	}
	uploadMetadata["video-size-bytes"] = fmt.Sprintf("%d", fileSize)
	if t.durationMs > 0 {
		uploadMetadata["video-duration-ms"] = fmt.Sprintf("%d", t.durationMs)
	}
	registeredPath, err := m7s.RecoverFailedUpload(t.Logger, t.db, mediaPath, t.filePath, t.storageKey,
		t.streamPath, fileSize, t.durationMs, uploadMetadata, cause)
	if err != nil {
		t.Error("failed upload registration not confirmed; keeping media and index sidecar",
			"error", err, "cause", cause, "mediaPath", mediaPath,
			"registeredPath", registeredPath, "filePath", t.filePath)
		return true, false
	}
	if registeredPath == "" {
		// 无 DB / local 存储：没有补传队列可登记，调用方按旧行为处理临时文件。
		t.Warn("failed upload has no retry queue; local temp copy will be discarded",
			"cause", cause, "mediaPath", mediaPath, "filePath", t.filePath)
		return false, false
	}
	movedToPending = registeredPath != mediaPath
	// 媒体已离开原路径时 sidecar 才是孤儿；原地登记时它仍指向真实媒体。
	t.removeIndexAfterPendingRegistration(movedToPending)
	return true, movedToPending
}

// recoverLocalFile 最终持久化失败后,把仍在本地路径的录像文件登记进补传队列。
// 用于 insert-range 快路径与 fragment(fMP4)finalize 的失败兜底。
//
// 返回媒体是否被移入 pending 目录（此时原路径已无媒体，sidecar 已一并清理）；
// 原地登记或登记失败时返回 false，媒体与 sidecar 都保留。
func (t *writeTrailerTask) recoverLocalFile(localPath string, fileSize int64, cause error) (movedToPending bool) {
	_, movedToPending = t.handleFailedUpload(localPath, fileSize, nil, cause)
	return movedToPending
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

	// INSERT_RANGE 把 mdat 后移了 insertLen，索引记录也要跟着平移。
	if idxErr := rewriteIndexRecords(t.indexPath, t.muxer, t.durationMs); idxErr != nil {
		t.Error("rewrite index records after insert-range", "err", idxErr, "indexPath", t.indexPath)
	}

	// 持久化（对象存储=上传）移交独立上传队列,不阻塞 trailer 队列。
	t.handoffToUploadQueue(uploadSize, func(cause error) {
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
	// indexPath 是伴随索引路径；只有上传成功分支才删除它。
	indexPath string
	// prepare 在上传前执行。恢复上传用它把已恢复的产物移交给 storage.File；
	// 正常录制路径为 nil（文件在录制期间已写入）。
	prepare func() error
	// onDone 在任务结束时（成功或失败）执行，用于清理在途去重标记。
	onDone func()
	// size 是待持久化文件的字节数，仅用于结构化日志。
	size int64
	// retryCount 是本任务已执行的重试次数（正常队列首次尝试为 0），
	// 供「上传失败」日志的 retryCount 字段使用。
	retryCount int
}

func (u *finalizeUploadTask) Go() (err error) {
	if u.onDone != nil {
		defer u.onDone()
	}
	if u.prepare != nil {
		if err = u.prepare(); err != nil {
			u.logUploadFailure(err)
			// FinalizeFromTemp 失败时源文件仍归调用方；不调用 Close（实现内部
			// 句柄已不可用，Close 可能对空句柄上传）。交给 onFail 处理补传。
			u.file = nil
			if u.onFail != nil {
				u.onFail(err)
			}
			return
		}
	}
	if err = u.file.Close(); err != nil {
		u.logUploadFailure(err)
		u.file = nil
		if u.onFail != nil {
			u.onFail(err)
		}
		return
	}
	u.file = nil
	u.Info("upload succeeded",
		"file", u.filePath,
		"size", u.size,
		"retryCount", u.retryCount,
		"streamPath", u.streamPath)
	// 媒体已完整持久化，sidecar 的使命结束，此时才提交式删除。
	// 上传失败/pending 分支绝不走到这里，索引保留供恢复使用。
	if idxErr := removeIndexAfterUpload(u.indexPath); idxErr != nil {
		u.Warn("remove index sidecar after upload failed", "err", idxErr, "indexPath", u.indexPath)
	}
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

// recordedSize 返回 trailer 改写前已写入的字节数；无法取值时返回 0。
func (t *writeTrailerTask) recordedSize() int64 {
	if t.file == nil {
		return 0
	}
	size, err := t.file.Seek(0, io.SeekEnd)
	if err != nil || size < 0 {
		return 0
	}
	return size
}

// logRecordStop 输出「录制结束」事件。字段名是现场日志检索契约的一部分
// （streamPath/file/durationMs/size/samples/tracks/elapsedMs），勿随意改名。
func (t *writeTrailerTask) logRecordStop(size int64) {
	tracks, samples := 0, 0
	samplesSource := "samplelist"
	if t.muxer != nil {
		// fragment 模式每个轨道只保留尚未写出的当前一帧，累计量由 muxer 计数，
		// 否则无论录多久 samples 都只有轨道数（1–2），会被误判为没录到数据。
		if t.muxer.isFragment() {
			samplesSource = "fragment"
			samples = int(t.muxer.fragmentSamples)
		}
		for id := uint32(1); id < t.muxer.nextTrackId; id++ {
			track := t.muxer.Tracks[id]
			if track == nil {
				continue
			}
			tracks++
			// progressive: Samplelist 即全部样本；
			// fragment: Samplelist 是尚未刷出的缓冲帧，计入总量。
			samples += len(track.Samplelist)
		}
	}
	var elapsedMs int64
	if !t.startedAt.IsZero() {
		if elapsed := time.Since(t.startedAt).Milliseconds(); elapsed > 0 {
			elapsedMs = elapsed
		}
	}
	t.Info("record stopped",
		"streamPath", t.streamPath,
		"file", t.filePath,
		"durationMs", t.durationMs,
		"size", size,
		"samples", samples,
		"samplesSource", samplesSource,
		"tracks", tracks,
		"elapsedMs", elapsedMs)
}

// uploadErrorClass 是 storage.UploadErrorClass 的包内别名，供日志字段使用。
func uploadErrorClass(err error) string {
	return storage.UploadErrorClass(err)
}

// logUploadFailure 输出「上传失败」事件。固定字段（file/size/retryCount/
// errorClass/nextRetryAt）供现场按日志检索；nextRetryAt 为空表示没有补传
// 队列可登记（无 DB 部署），该文件不会被自动重试。
func (u *finalizeUploadTask) logUploadFailure(cause error) {
	var nextRetryAt time.Time
	if u.db != nil {
		nextRetryAt = m7s.NextUploadRetryAt(u.retryCount)
	}
	u.Error("upload failed",
		"file", u.filePath,
		"size", u.size,
		"retryCount", u.retryCount,
		"errorClass", uploadErrorClass(cause),
		"nextRetryAt", nextRetryAt,
		"error", cause,
		"streamPath", u.streamPath)
}

// handoffToUploadQueue 把 t.file 的最终持久化移交独立上传队列,trailer 队列
// 立即空出处理下一个录像的磁盘工作。onFail 用于上传失败后的补传登记。
func (t *writeTrailerTask) handoffToUploadQueue(size int64, onFail func(cause error)) {
	ft := &finalizeUploadTask{
		file:       t.file,
		filePath:   t.filePath,
		streamPath: t.streamPath,
		db:         t.db,
		dbWrite:    t.dbWrite,
		onFail:     onFail,
		indexPath:  t.indexPath,
		size:       size,
	}
	t.file = nil // 所有权移交
	t.logUploadEnqueued(size)
	uploadQueueTask.AddTask(ft, t.Logger)
}

// logUploadEnqueued 输出「上传入队」事件。retryCount 固定为 0：入队即首次
// 尝试，后续重试由补传调度负责并各自记录 retryCount。
func (t *writeTrailerTask) logUploadEnqueued(size int64) {
	t.Info("upload enqueued",
		"file", t.filePath,
		"size", size,
		"retryCount", 0,
		"streamPath", t.streamPath)
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
	muxer            *Muxer
	file             storage.File
	firstVideoFrame  bool // 标记是否是第一个视频帧
	creating         bool
	createDone       chan error
	sampleBuffer     []bufferedSample
	audioTrack       *Track // FMP4 预置轨，避免与 WriteMoov 竞态
	videoTrack       *Track
	lastProgressDB   time.Time // fmp4 上次刷新 EndTime 的墙钟，用于节流
	progressClosedID uint      // 已交给 writeTrailer 定稿的记录 ID，禁止再被进度刷新回退
	// recordStartedAt 是本段录像开片的墙钟时间，用于「录制结束」日志的 elapsedMs。
	recordStartedAt time.Time
	// lastSampleWriteTime：最后成功写入样本的帧墙钟，Dispose 收尾用它写 EndTime，避免断流读超时把区间拖长。
	// Confirmed via 寸止: BUG-021 方案 B
	lastSampleWriteTime time.Time

	// 崩溃恢复伴随索引（progressive MP4 才有）。mediaPath + ".m7sidx"，
	// 与媒体数据同目录（对象存储=本地暂存文件旁）。
	index           *IndexWriter
	indexPath       string
	indexMediaPath  string
	indexSamples    uint64
	indexTrackCount int  // 建索引头时 muxer 的轨道数，用于发现后到的轨道
	indexWarned     bool // 索引写失败只告警一次，避免每帧刷日志
	// indexDisabled 表示本段录像的 sidecar 创建失败（如临时目录不可写），
	// 不再逐帧重试文件操作。下一段录像/下一次 reset 会重新尝试。
	indexDisabled bool
}

// maybeFlushProgress 节流更新进行中 fmp4 记录的 EndTime/Duration，供 list/点播查询命中。
// 普通 mp4 不刷新（进行中文件尚无 moov，写入 EndTime 会造成「可见但不可播」）。
// Confirmed via 寸止: REQ-MP4-001 方案 B / M1+M4
func (r *Recorder) maybeFlushProgress(end time.Time, duration uint32) {
	if r.creating || r.Event.Type != "fmp4" || r.Event.ID == 0 {
		return
	}
	id := r.Event.ID
	// M4：已 writeTailer 的记录禁止再刷，避免与 deferred Save 竞态把 EndTime 写回去
	if id == r.progressClosedID {
		return
	}
	db := r.RecordJob.Plugin.DB
	if db == nil || r.RecordJob.RecConf.Mode == config.RecordModeTest {
		return
	}
	// 首次立即刷新；之后按 progressDBInterval 节流
	if !r.lastProgressDB.IsZero() && end.Sub(r.lastProgressDB) < progressDBInterval {
		return
	}
	r.lastProgressDB = end
	r.Event.EndTime = end
	r.Event.Duration = duration
	dbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 仅允许 end_time 单调前进，防止迟到的 Update 覆盖定稿值
	if result := db.WithContext(dbCtx).Model(&m7s.RecordStream{}).
		Where("id = ? AND end_time < ?", id, end).
		Updates(map[string]any{
			"end_time": end,
			"duration": duration,
		}); result.Error != nil {
		r.Warn("db progress update failed", "id", id, "err", result.Error)
	}
}

func (r *Recorder) writeTailer(end time.Time) {
	// M4：先锁定当前记录 ID，阻断后续 maybeFlushProgress（含在途 goroutine 语义上的迟到写）
	if r.Event.ID != 0 {
		r.progressClosedID = r.Event.ID
	}
	// 正常收尾只 Commit + Close，不删除 sidecar；删除发生在上传成功之后。
	indexPath := r.commitIndex()
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
		indexPath:  indexPath,
		startedAt:  r.recordStartedAt,
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
	r.lastProgressDB = time.Time{}
	r.lastSampleWriteTime = time.Time{}
	r.recordStartedAt = time.Now()
	t0 := time.Now()
	err = r.CreateStream(start, CustomFileName)
	r.Info("createStream step1 CreateStream", "elapsed", time.Since(t0))
	if err != nil {
		return
	}
	// fmp4：开片后记一次进度时间戳，避免紧接着的 maybeFlushProgress 重复写库
	if r.Event.Type == "fmp4" && !r.Event.EndTime.IsZero() {
		r.lastProgressDB = r.Event.EndTime
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

	r.audioTrack, r.videoTrack = nil, nil
	if r.Event.Type == "fmp4" {
		r.muxer = NewMuxerWithStreamPath(FLAG_FRAGMENT, r.Event.StreamPath)
	} else {
		r.muxer = NewMuxerWithStreamPath(0, r.Event.StreamPath)
	}
	t2 := time.Now()
	err = r.muxer.WriteInitSegment(r.file)
	r.Info("createStream step3 WriteInitSegment", "elapsed", time.Since(t2))
	if err != nil {
		return
	}
	// FMP4：在首个 moof 前写入含 mvex 的 moov（与 HTTP 点播路径一致）
	if r.muxer.isFragment() {
		t3 := time.Now()
		if err = r.initFragmentTracks(); err != nil {
			r.Error("createStream initFragmentTracks", "err", err)
			return
		}
		if err = r.muxer.WriteMoov(r.file); err != nil {
			r.Error("createStream WriteMoov", "err", err)
			return
		}
		r.Info("createStream step4 WriteMoov", "elapsed", time.Since(t3))
	}
	r.Info("createStream total", "elapsed", time.Since(t0))
	r.logRecordStart(st)
	r.SetDescription("startTime", start.Format("2006-01-02 15:04:05"))
	return
}

// logRecordStart 输出「录制开始」事件。固定字段（streamPath/file/type/
// storageType/codec）是现场日志检索契约的一部分，勿随意改名。
// codec 优先取视频编码（avc1.64001f 等），纯音频录像回退音频编码。
func (r *Recorder) logRecordStart(st storage.Storage) {
	storageType := r.Event.StorageType
	if st != nil {
		storageType = st.GetKey()
	}
	codecName := r.Event.VideoCodec
	if codecName == "" {
		codecName = r.Event.AudioCodec
	}
	if codecName == "" {
		codecName = "unknown"
	}
	r.Info("record started",
		"streamPath", r.Event.StreamPath,
		"file", r.Event.FilePath,
		"type", r.Event.Type,
		"storageType", storageType,
		"codec", codecName)
}

// initFragmentTracks 按 Publisher 已就绪轨预置 FMP4 Track，保证 WriteMoov 含全部轨
func (r *Recorder) initFragmentTracks() (err error) {
	sub := r.RecordJob.Subscriber
	if sub == nil || sub.Publisher == nil {
		return nil
	}
	pub := sub.Publisher
	if pub.HasVideoTrack() && sub.SubVideo {
		v := pub.VideoTrack.AVTrack
		if err = v.WaitReady(); err != nil {
			return
		}
		var codecID box.MP4_CODEC_TYPE
		switch v.ICodecCtx.FourCC() {
		case codec.FourCC_H264:
			codecID = box.MP4_CODEC_H264
		case codec.FourCC_H265:
			codecID = box.MP4_CODEC_H265
		default:
			r.Warn("fmp4 skip unsupported video codec", "fourcc", v.ICodecCtx.FourCC())
		}
		if codecID != 0 {
			track := r.muxer.AddTrack(codecID)
			track.ICodecCtx = v.ICodecCtx
			r.videoTrack = track
		}
	}
	if pub.HasAudioTrack() && sub.SubAudio {
		a := pub.AudioTrack.AVTrack
		if err = a.WaitReady(); err != nil {
			return
		}
		var codecID box.MP4_CODEC_TYPE
		switch a.ICodecCtx.FourCC() {
		case codec.FourCC_MP4A:
			codecID = box.MP4_CODEC_AAC
		case codec.FourCC_ALAW:
			codecID = box.MP4_CODEC_G711A
		case codec.FourCC_ULAW:
			codecID = box.MP4_CODEC_G711U
		case codec.FourCC_OPUS:
			codecID = box.MP4_CODEC_OPUS
		default:
			r.Warn("fmp4 skip unsupported audio codec", "fourcc", a.ICodecCtx.FourCC())
		}
		if codecID != 0 {
			track := r.muxer.AddTrack(codecID)
			track.ICodecCtx = a.ICodecCtx
			r.audioTrack = track
		}
	}
	return nil
}

// —— 崩溃恢复伴随索引（sidecar） ——
//
// 仅 progressive MP4 需要：fMP4 的 init moov + moof/mdat 自描述，崩溃后无需索引。
// 写入顺序固定为「先媒体、后索引」，索引永远不超前于已交给存储层的数据。
// 正常停止只 Commit + Close，不删除；sidecar 保留到上传成功后才由
// removeIndexAfterUpload 提交式删除。

// indexHeader 按当前 muxer 轨道生成索引头。
func (r *Recorder) indexHeader(mediaPath string) IndexHeader {
	h := IndexHeader{
		Version:    1,
		StreamPath: r.Event.StreamPath,
		// MediaPath 必须是绝对路径：恢复侧的路径 guard 只接受绝对路径。
		MediaPath:  absoluteMediaPath(mediaPath),
		TargetName: filepath.Base(r.Event.FilePath),
		FilePath:   r.Event.FilePath,
		StartTime:  r.Event.StartTime,
	}
	if r.file != nil {
		// LocalFile.Name() 是本地绝对路径；S3File.Name() 是含 prefix 的对象键。
		h.StorageKey = r.file.Name()
	}
	if st := r.RecordJob.GetStorage(); st != nil {
		h.StorageType = st.GetKey()
	}
	for id := uint32(1); id < r.muxer.nextTrackId; id++ {
		track := r.muxer.Tracks[id]
		if track == nil || track.ICodecCtx == nil {
			continue
		}
		h.Tracks = append(h.Tracks, newIndexTrack(track))
	}
	return h
}

func newIndexTrack(track *Track) IndexTrack {
	fourCC := box.GetCodecNameWithCodecId(track.Cid)
	info := IndexTrack{ID: uint16(track.TrackId), FourCC: string(fourCC[:])}
	switch ctx := track.ICodecCtx.(type) {
	case pkg.IVideoCodecCtx:
		info.Width, info.Height = ctx.Width(), ctx.Height()
	case pkg.IAudioCodecCtx:
		info.SampleRate, info.Channels = ctx.GetSampleRate(), ctx.GetChannels()
	}
	var buf bytes.Buffer
	if _, err := box.WriteTo(&buf, track.makeStsd()); err == nil {
		info.STSD = append([]byte(nil), buf.Bytes()...)
	}
	return info
}

func indexRecord(track *Track, sample box.Sample, offset int64) IndexRecord {
	var flags uint8
	if sample.KeyFrame {
		flags = indexFlagKeyFrame
	}
	return IndexRecord{
		Flags:    flags,
		TrackID:  uint16(track.TrackId),
		Size:     uint32(sample.Size),
		Offset:   uint64(offset),
		DTS:      sample.Timestamp,
		Duration: sample.Duration,
	}
}

// openIndexAppend 以追加方式打开已有 sidecar，用于追加扩展记录或重建后续写。
func openIndexAppend(path string) (*IndexWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &IndexWriter{
		f:             f,
		buf:           bufio.NewWriterSize(f, indexWriteBufferSize),
		flushInterval: GetIndexConfig().FlushInterval,
	}, nil
}

// ensureIndex 为当前媒体文件创建 sidecar。本地路径未知（自定义 storage.File
// 未实现 RangeInserter）时静默跳过。
func (r *Recorder) ensureIndex() error {
	if !GetIndexConfig().Enable || r.index != nil || r.muxer == nil || r.muxer.isFragment() {
		return nil
	}
	mediaPath := absoluteMediaPath(localMediaPath(r.file))
	if mediaPath == "" {
		return nil
	}
	h := r.indexHeader(mediaPath)
	path := indexFilePath(mediaPath)
	w, err := CreateIndex(path, h)
	if err != nil {
		return err
	}
	r.index = w
	r.indexPath = path
	r.indexMediaPath = mediaPath
	r.indexSamples = 0
	r.indexTrackCount = len(r.muxer.Tracks)
	r.indexWarned = false
	// 进程级活跃登记：恢复扫描据此跳过仍在录制的文件（先于 mtime 判据）。
	RegisterActiveIndex(path, mediaPath)
	r.Info("index sidecar created", "indexPath", path, "tracks", len(h.Tracks))
	return nil
}

// maybeRebuildIndex 在轨道数变化时重建索引头。轨道是在首个 sample 到达时才
// 加入 muxer 的，晚到的第二轨无法再写进已落盘的 JSON 头，只能原子重写整个
// sidecar（先写 .tmp 再 rename，崩溃时旧文件仍完整）。
func (r *Recorder) maybeRebuildIndex() error {
	if r.index == nil || len(r.muxer.Tracks) == r.indexTrackCount {
		return nil
	}
	return r.rebuildIndex()
}

func (r *Recorder) rebuildIndex() error {
	if r.index == nil || r.indexMediaPath == "" {
		return nil
	}
	path := r.indexPath
	if err := r.index.Close(); err != nil {
		r.Warn("close index before rebuild", "err", err, "indexPath", path)
	}
	r.index = nil

	h := r.indexHeader(r.indexMediaPath)
	tmpPath := path + ".tmp"
	w, err := CreateIndex(tmpPath, h)
	if err != nil {
		// 旧 sidecar 仍然完整，尽量继续追加，避免后续 ensureIndex 截断它。
		if nw, rerr := openIndexAppend(path); rerr == nil {
			r.index = nw
		}
		return err
	}
	var samples uint64
	for id := uint32(1); id < r.muxer.nextTrackId; id++ {
		track := r.muxer.Tracks[id]
		if track == nil {
			continue
		}
		for _, sample := range track.Samplelist {
			if err := w.Append(indexRecord(track, sample, sample.Offset)); err != nil {
				w.Close()
				os.Remove(tmpPath)
				return err
			}
			samples++
		}
	}
	if err := w.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		// rename 失败时旧文件未变，恢复写入句柄继续追加。
		if nw, rerr := openIndexAppend(path); rerr == nil {
			r.index = nw
		}
		return err
	}
	nw, err := openIndexAppend(path)
	if err != nil {
		return err
	}
	r.index = nw
	r.indexSamples = samples
	r.indexTrackCount = len(r.muxer.Tracks)
	r.Info("index sidecar rebuilt for new track",
		"indexPath", path, "tracks", len(h.Tracks), "samples", samples)
	return nil
}

// writeSample 先写媒体数据，成功后才追加索引记录。
// 偏移必须在 WriteSample 之前取：WriteSample 内部只会修改它的值拷贝。
func (r *Recorder) writeSample(track *Track, sample box.Sample) error {
	if err := r.maybeRebuildIndex(); err != nil && !r.indexWarned {
		r.Warn("rebuild index sidecar", "err", err, "indexPath", r.indexPath)
		r.indexWarned = true
	}
	fragment := r.muxer.isFragment()
	offset := r.muxer.CurrentOffset
	if err := r.muxer.WriteSample(r.file, track, sample); err != nil {
		return err
	}
	if fragment {
		return nil
	}
	r.appendIndexRecord(track, sample, offset)
	return nil
}

func (r *Recorder) appendIndexRecord(track *Track, sample box.Sample, offset int64) {
	if !GetIndexConfig().Enable {
		return
	}
	if r.index == nil {
		// 已有 sidecar 但写句柄不可用（如重建后重开失败）：绝不能重建截断它，
		// 否则历史记录会静默丢失。
		if r.indexPath != "" || r.indexDisabled {
			return
		}
		if err := r.ensureIndex(); err != nil {
			r.indexDisabled = true
			r.warnIndex(err)
			return
		}
		if r.index == nil {
			return
		}
	}
	if err := r.index.Append(indexRecord(track, sample, offset)); err != nil {
		r.warnIndex(err)
		return
	}
	r.indexSamples++
}

func (r *Recorder) warnIndex(err error) {
	if r.indexWarned {
		return
	}
	r.indexWarned = true
	r.logIndexError(err)
}

// logIndexError 输出「索引写入异常」事件。固定字段（streamPath/file/
// error/recordsWritten）供现场日志检索；indexPath 为附加定位信息。
func (r *Recorder) logIndexError(err error) {
	r.Error("index write failed",
		"streamPath", r.Event.StreamPath,
		"file", r.Event.FilePath,
		"error", err,
		"recordsWritten", r.indexSamples,
		"indexPath", r.indexPath)
}

func (r *Recorder) resetIndex() {
	// commitIndex/closeIndex 都经这里收尾：注销活跃登记，恢复扫描才能接管。
	UnregisterActiveIndex(r.indexPath, r.indexMediaPath)
	r.index = nil
	r.indexPath = ""
	r.indexMediaPath = ""
	r.indexSamples = 0
	r.indexTrackCount = 0
	r.indexWarned = false
	r.indexDisabled = false
}

// commitIndex 追加 commit 扩展记录并关闭索引，返回 sidecar 路径供后续
// 上传成功时删除。sidecar 本身保留在磁盘上（崩溃窗口内仍可恢复）。
//
// 无论本段是否还持有写句柄都无条件 resetIndex：indexPath/indexDisabled 是
// 分片级状态，CreateIndex 失败或重建后重开失败都不能残留到下一段——否则
// 第 1 个分片失败会导致后续所有分片都不再尝试建 sidecar。
// 句柄为空但 sidecar 已存在时仍返回其路径，让上传成功分支能清理它。
func (r *Recorder) commitIndex() (indexPath string) {
	idx, path := r.index, r.indexPath
	if idx != nil {
		r.index = nil // 防止重入时重复 Commit
		if err := idx.Commit(r.Event.Duration, r.indexSamples); err != nil {
			r.logIndexError(err)
		}
		if err := idx.Close(); err != nil {
			r.Warn("close index sidecar", "err", err, "indexPath", path)
		}
	}
	r.resetIndex()
	return path
}

// closeIndex 不写 commit 直接关闭索引（媒体文件被丢弃的分支）。
// 按提交式删除约定，这里也不删除 sidecar；同样无条件复位分片级索引状态。
func (r *Recorder) closeIndex() {
	if r.index != nil {
		if err := r.index.Close(); err != nil {
			r.Warn("close index sidecar", "err", err, "indexPath", r.indexPath)
		}
	}
	r.resetIndex()
}

// appendIndexRename 在已 Committed 的 sidecar 末尾追加 RENAME 扩展记录。
// 用于 FinalizeFromTemp 把媒体换成另一个本地文件（对象存储上传源）的场景。
func appendIndexRename(indexPath, mediaPath string) error {
	w, err := openIndexAppend(indexPath)
	if err != nil {
		return err
	}
	err = w.Rename(mediaPath)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// removeIndexAfterUpload 是 sidecar 提交式删除的唯一入口。
// 只在媒体上传成功后调用；失败/pending 分支保留索引供恢复使用。
func removeIndexAfterUpload(indexPath string) error {
	if indexPath == "" {
		return nil
	}
	return RemoveIndex(indexPath)
}

// removeIndexAfterPendingRegistration 在媒体已成功移入 pending 暂存目录后
// 删除 sidecar。此时原路径的媒体已不存在，索引无法再用于恢复，留着只会
// 成为指向不存在文件的孤儿。pending 登记失败（如 pending 满）时保留，
// 供后续重试使用。
func (t *writeTrailerTask) removeIndexAfterPendingRegistration(registered bool) {
	if !registered || t.indexPath == "" {
		return
	}
	if err := RemoveIndex(t.indexPath); err != nil {
		t.Warn("remove index sidecar after pending registration failed",
			"err", err, "indexPath", t.indexPath)
	}
}

func (r *Recorder) Dispose() {
	r.audioTrack, r.videoTrack = nil, nil
	if r.creating {
		// 异步分片 createStream 正在进行:OLD 文件已在 checkFragment 的 writeTailer 中移交给 writeTrailerTask。
		// 等待 goroutine 结束,避免它在 retry Run() 启动后仍然修改 r.muxer/r.file 造成竞争。
		if r.createDone != nil {
			<-r.createDone
		}
		r.creating = false
		// goroutine 若成功,r.muxer/r.file 已指向新文件(仅含 init segment)。关闭并丢弃它。
		// 这个新文件的索引同样不提交：文件没有被最终定稿，按提交式删除约定也不删除。
		r.closeIndex()
		if r.muxer != nil && r.file != nil {
			r.file.Close()
		}
		r.muxer = nil
		r.file = nil
		return
	}
	if r.muxer != nil {
		// {{ AURA-X: Modify - Dispose 用最后一帧 WriteTime 写 EndTime，与 checkFragment 一致；无样本则回退 Now. Confirmed via 寸止 BUG-021 B. }}
		end := time.Now()
		if !r.lastSampleWriteTime.IsZero() {
			end = r.lastSampleWriteTime
		}
		r.writeTailer(end)
		// 关键修复:将 muxer 和 file 置 nil,切断重试 Run() 对旧 muxer/file 的访问。
		// 文件的关闭由 writeTrailerTask.Run() 负责。若不置 nil,重试 Run() 会向
		// writeTrailerTask 正在处理的同一 muxer 写入新数据,导致 mdatSize 不匹配→EOF。
		r.muxer = nil
		r.file = nil
	} else {
		r.closeIndex()
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
			r.audioTrack, r.videoTrack = nil, nil
			go func() {
				createErr := r.createStream(startTime)
				r.Info("check fragment end async", "err", createErr)
				r.createDone <- createErr
			}()
			at, vt = nil, nil
			audioTrack, videoTrack = nil, nil
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
		audioTrack, videoTrack = r.audioTrack, r.videoTrack
		for _, bs := range r.sampleBuffer {
			if bs.isAudio {
				if at == nil {
					at = sub.AudioReader.Track
					if audioTrack == nil {
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
						r.audioTrack = audioTrack
					}
				}
				if err := r.writeSample(audioTrack, bs.sample); err != nil {
					return err
				}
			} else {
				if vt == nil {
					vt = sub.VideoReader.Track
					if videoTrack == nil {
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
						r.videoTrack = videoTrack
					}
				}
				if err := r.writeSample(videoTrack, bs.sample); err != nil {
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
			if r.audioTrack != nil {
				audioTrack = r.audioTrack
			} else {
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
				r.audioTrack = audioTrack
			}
			if audioTrack == nil {
				return fmt.Errorf("unsupported audio codec for mp4 record: %T", at.ICodecCtx.GetBase())
			}
		}
		if err = r.writeSample(audioTrack, sample); err != nil {
			return err
		}
		r.lastSampleWriteTime = sub.AudioReader.Value.WriteTime
		// 仅音轨录制时也推进进行中 EndTime
		if sub.VideoReader == nil {
			r.maybeFlushProgress(sub.AudioReader.Value.WriteTime, sub.AudioReader.AbsTime)
		}
		return nil
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
			if r.videoTrack != nil {
				videoTrack = r.videoTrack
			} else {
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
				r.videoTrack = videoTrack
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
		if err = r.writeSample(videoTrack, sample); err != nil {
			return err
		}
		r.lastSampleWriteTime = sub.VideoReader.Value.WriteTime
		r.maybeFlushProgress(sub.VideoReader.Value.WriteTime, sub.VideoReader.AbsTime)
		return nil
	})
}
