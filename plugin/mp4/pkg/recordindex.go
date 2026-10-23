package mp4

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// 录像伴随索引(sidecar)格式。
//
// 每个媒体文件旁边有一个同名 + ".m7sidx" 的文件，录制过程中逐帧追加，
// 进程崩溃后可用它重建 moov，而不必反向解析裸 mdat。格式完全自包含，
// 不依赖 muxer/box 包。
//
// 文件布局:
//
//	[magic 7B "M7SIDX1"][headerLen uint32 BE][headerJSON UTF-8][record]...
//
// 普通记录固定 23 字节，大端:
//
//	flags(1) trackID(2) size(4) offset(8) dts(4) duration(4)
//
// flags 的语义由录制方定义(如关键帧位)，本包只原样保存；0xFF 保留给扩展记录。
//
// 扩展记录(flags = 0xFF)与普通记录共用 23 字节帧头，其后紧跟 payload:
//
//	flags(1)=0xFF | type(2) | payloadLen(4) | reserved(8+4+4, 全 0) | payload(payloadLen)
//
//	type 1 = commit: payload = totalDuration(uint32 BE) | samples(uint64 BE)，共 12 字节
//	type 2 = rename: payload = 新的媒体路径(UTF-8)
//	type 3 = offset-shift: payload = preLen(int64 BE) | insertLen(int64 BE)，共 16 字节。
//	         写在 fallocate(INSERT_RANGE) 之前，表示「若该插入已生效，索引偏移需 +insertLen」。
//	type 4 = offset-shift-done: 无 payload，表示索引已按平移后的偏移重写完成。
//
// 未知 type 的扩展记录按 payloadLen 跳过(向前兼容)。payloadLen 超过上限视为
// 损坏，读取在该处停止。整帧全零同样视为损坏尾部——崩溃、掉电、预分配和
// S3 中转拷贝都可能留下零填充，全零帧不是合法样本(track ID 0 也不是合法轨道)。
// 除这些结构性判据外，帧内容损坏无法在无校验和的情况下检测，调用方仍需按
// trackID/size 兜底。
const (
	// IndexExtension 是伴随索引文件的扩展名。
	IndexExtension = ".m7sidx"
	// IndexMagic 是索引文件头魔数(7 字节)。
	IndexMagic = "M7SIDX1"
	// IndexRecordSize 是普通索引记录的固定长度(字节)。
	IndexRecordSize = 23

	indexFlushInterval   = time.Second
	indexWriteBufferSize = 4096
	maxIndexHeaderSize   = 4 << 20
	maxIndexPayloadSize  = 1 << 20
	// maxIndexRecords 限制单次读取载入内存的样本记录条数(23 字节/条)。
	// 索引内容是不可信输入：不设上限时，一个伪造的 10GB sidecar 会把记录
	// 全部读进内存(约 14GB)导致 OOM。
	//
	// 容量口径：单轨 20fps ≈ 11 天；双轨(视频 25fps + AAC) ≈ 43 记录/秒 ≈ 5.4 天，
	// 即 20M / 43 / 86400 ≈ 5.38。分片录制会按片重建索引，不受此限约束；但
	// fragment:0 的长期单文件一旦越限，读取会被判 ErrIndexCorrupt → 恢复侧按
	// 永久失败落 .failed，不会自动恢复，故该值需与"最长单文件录制时长"对齐。
	maxIndexRecords = 20_000_000

	// minSTSDBoxSize 是恢复路径接受的最小 stsd box 声明尺寸。stsd 是 FullBox，
	// 合法负载至少是 version/flags(4) + entry_count(4)，加上 8 字节 box 头即 16。
	// 声明尺寸更小时 box.ReadFrom 会对空 payload 取 payload[0] 或 entry_count 而
	// panic；小于 8 则会让 make(size-8) 的无符号运算下溢成 ~4.29GB 的分配。
	minSTSDBoxSize = 16
	// minSTSDLargeBoxSize 是 largesize(size==1) 形式的最小声明尺寸：
	// size(4) + type(4) + largesize(8) + version/flags(4) + entry_count(4)。
	minSTSDLargeBoxSize = 24

	indexFlagExtension      = 0xFF
	indexExtCommit          = 1
	indexExtRename          = 2
	indexExtOffsetShift     = 3
	indexExtOffsetShiftDone = 4
	indexCommitPayload      = 12
	indexOffsetShiftPayload = 16
	indexPrefixSize         = len(IndexMagic) + 4
)

var (
	// ErrIndexClosed 表示索引写入器已经关闭。
	ErrIndexClosed = errors.New("mp4: index writer closed")
	// ErrIndexBadMagic 表示文件不是本格式的索引文件。
	ErrIndexBadMagic = errors.New("mp4: bad index magic")
	// ErrIndexCorrupt 标记确定性的结构性损坏（坏魔数、非法头长度、头被截断、
	// JSON 解析失败）。这类文件不会自行恢复，调用方可以永久放弃；
	// 除此之外的读取错误（权限、挂载点暂时不可用等）应视为可重试。
	ErrIndexCorrupt = errors.New("mp4: corrupt index file")
)

// IndexTrack 保存重建 moov 所需的单轨信息。
type IndexTrack struct {
	ID         uint16 `json:"id"`
	FourCC     string `json:"fourCC"`
	Width      int    `json:"width,omitempty"`
	Height     int    `json:"height,omitempty"`
	SampleRate int    `json:"sampleRate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	// STSD 是完整的 stsd box 字节，恢复时直接注入，避免重新解析码流。
	STSD []byte `json:"stsd,omitempty"`
}

// IndexHeader 是索引文件的头部信息，随文件创建写一次。
type IndexHeader struct {
	Version     int          `json:"version"`
	StreamPath  string       `json:"streamPath,omitempty"`
	MediaPath   string       `json:"mediaPath,omitempty"`
	TargetName  string       `json:"targetName,omitempty"`
	StorageKey  string       `json:"storageKey,omitempty"`
	FilePath    string       `json:"filePath,omitempty"`
	StorageType string       `json:"storageType,omitempty"`
	StartTime   time.Time    `json:"startTime,omitempty"`
	Tracks      []IndexTrack `json:"tracks,omitempty"`
}

// IndexRecord 描述媒体文件中的一个 sample。
type IndexRecord struct {
	Flags    uint8
	TrackID  uint16
	Size     uint32
	Offset   uint64
	DTS      uint32
	Duration uint32
}

// OffsetShiftState 描述 sidecar 末尾 INSERT_RANGE 平移记录的状态。
// 录制收尾可能用 fallocate(INSERT_RANGE) 把 mdat 整体后移，索引偏移也要跟着
// 后移；两步之间被 kill 会留下「媒体已后移、索引仍是旧偏移」的现场。SHIFT /
// SHIFT_DONE 扩展记录让恢复侧能确定性地识别该现场（见 ResolveOffsetShift）。
type OffsetShiftState int

const (
	// OffsetShiftNone 没有平移记录：索引偏移就是录制时的原始坐标（旧行为）。
	OffsetShiftNone OffsetShiftState = iota
	// OffsetShiftPending 最后一条是 SHIFT：InsertRange 是否已生效取决于媒体
	// 文件长度，必须用 ResolveOffsetShift 判定，不能猜。
	OffsetShiftPending
	// OffsetShiftApplied 最后一条是 SHIFT_DONE：索引已按平移后的偏移重写。
	OffsetShiftApplied
	// OffsetShiftUncertain 平移记录本身不完整/非法，状态无法确定。
	OffsetShiftUncertain
)

// ErrOffsetShiftUncertain 表示 sidecar 记录了 INSERT_RANGE 平移意图，但媒体
// 文件长度既不是平移前也不是平移后——进程在两步之间被杀，无法确定数据落在
// 哪个坐标系。调用方必须拒绝恢复并保留 sidecar（人工可介入）。
var ErrOffsetShiftUncertain = errors.New("mp4: index offset shift state uncertain")

// IndexFile 是 ReadIndex 的结果。Records 只包含完整读出的记录；
// 尾部被截断时截断点之后的记录全部丢弃，Committed 保持 false。
type IndexFile struct {
	Header    IndexHeader
	Records   []IndexRecord
	Committed bool
	// MediaPath 是最终媒体路径：header 中的值，若存在 rename 扩展记录则被其覆盖。
	MediaPath string
	// OffsetShift 是 sidecar 末尾 INSERT_RANGE 平移记录的状态；多条记录时
	// 以最后一条为准。
	OffsetShift OffsetShiftState
	// ShiftPreLen / ShiftInsertLen 是最后一条 SHIFT 记录携带的媒体文件长度
	// （插入前）与插入长度。
	ShiftPreLen    int64
	ShiftInsertLen int64
}

// ResolveOffsetShift 根据媒体文件实际长度，确定索引记录需要的偏移平移量。
//
// 判定表（确定性，不确定即拒绝）：
//
//	最后一条是 SHIFT + size == preLen              → 0（INSERT_RANGE 未生效）
//	最后一条是 SHIFT + size >= preLen + insertLen  → insertLen（已生效、索引未重写）
//	最后一条是 SHIFT + 其它长度                    → ErrOffsetShiftUncertain
//	有 SHIFT_DONE（任意长度）                      → 0（索引已按新偏移重写）
//	无 SHIFT                                       → 0（旧行为）
func (f *IndexFile) ResolveOffsetShift(mediaSize int64) (int64, error) {
	switch f.OffsetShift {
	case OffsetShiftNone, OffsetShiftApplied:
		return 0, nil
	case OffsetShiftPending:
		if f.ShiftPreLen <= 0 || f.ShiftInsertLen <= 0 {
			return 0, fmt.Errorf("%w: invalid shift record preLen=%d insertLen=%d",
				ErrOffsetShiftUncertain, f.ShiftPreLen, f.ShiftInsertLen)
		}
		// sidecar 是不可信输入：两个正数相加仍可能溢出成负数，那样
		// mediaSize >= 负数 会恒真，把损坏记录误判成「插入已生效」。
		postLen := f.ShiftPreLen + f.ShiftInsertLen
		if postLen < f.ShiftPreLen {
			return 0, fmt.Errorf("%w: shift record overflows preLen=%d insertLen=%d",
				ErrOffsetShiftUncertain, f.ShiftPreLen, f.ShiftInsertLen)
		}
		switch {
		case mediaSize == f.ShiftPreLen:
			return 0, nil
		case mediaSize >= postLen:
			return f.ShiftInsertLen, nil
		}
		return 0, fmt.Errorf("%w: media size %d, preLen %d, insertLen %d",
			ErrOffsetShiftUncertain, mediaSize, f.ShiftPreLen, f.ShiftInsertLen)
	default:
		return 0, fmt.Errorf("%w: malformed offset shift records", ErrOffsetShiftUncertain)
	}
}

// RecordsWithOffsetShift 返回把所有样本记录偏移右移 shift 的新切片，调用方
// 应把它交给 muxer 而不是就地修改 ReadIndex 的结果。shift<=0 时原样返回。
func (f *IndexFile) RecordsWithOffsetShift(shift int64) []IndexRecord {
	if shift <= 0 || len(f.Records) == 0 {
		return f.Records
	}
	out := make([]IndexRecord, len(f.Records))
	for i, r := range f.Records {
		r.Offset = uint64(int64(r.Offset) + shift)
		out[i] = r
	}
	return out
}

// IndexWriter 顺序追加索引记录。写入经过 4KB 缓冲，最多每秒落盘一次；
// 显式调用 Flush/Commit/Rename/Close 会立即落盘。
type IndexWriter struct {
	mu        sync.Mutex
	f         *os.File
	buf       *bufio.Writer
	lastFlush time.Time
	// flushInterval 是自动落盘节流间隔；0 表示使用包级默认值(1s)。
	// 由 CreateIndex/openIndexAppend 从 IndexConfig 注入。
	flushInterval time.Duration
	closed        bool
	err           error
}

// CreateIndex 创建(或截断)索引文件并写入头部。
func CreateIndex(path string, h IndexHeader) (*IndexWriter, error) {
	if h.Version == 0 {
		h.Version = 1
	}
	data, err := encodeIndexHeader(h)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	w := &IndexWriter{
		f:             f,
		buf:           bufio.NewWriterSize(f, indexWriteBufferSize),
		flushInterval: GetIndexConfig().FlushInterval,
	}
	if _, err := w.buf.Write(data); err != nil {
		_ = f.Close()
		return nil, err
	}
	// 头部必须立刻可见，否则崩溃后文件不可用。
	if err := w.flushLocked(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

func encodeIndexHeader(h IndexHeader) ([]byte, error) {
	js, err := json.Marshal(h)
	if err != nil {
		return nil, fmt.Errorf("mp4: marshal index header: %w", err)
	}
	if len(js) > maxIndexHeaderSize {
		return nil, fmt.Errorf("mp4: index header too large: %d", len(js))
	}
	out := make([]byte, 0, indexPrefixSize+len(js))
	out = append(out, IndexMagic...)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(js)))
	out = append(out, l[:]...)
	out = append(out, js...)
	return out, nil
}

// Append 追加一条 sample 记录。按 1 秒节流自动落盘。
func (w *IndexWriter) Append(r IndexRecord) error {
	if r.Flags == indexFlagExtension {
		return errors.New("mp4: flags 0xFF is reserved for index extension records")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrIndexClosed
	}
	if w.err != nil {
		return w.err
	}
	var frame [IndexRecordSize]byte
	encodeIndexRecord(frame[:], r)
	if _, err := w.buf.Write(frame[:]); err != nil {
		w.err = err
		return err
	}
	interval := w.flushInterval
	if interval <= 0 {
		interval = indexFlushInterval
	}
	if time.Since(w.lastFlush) >= interval {
		return w.flushLocked()
	}
	return nil
}

// SetFlushInterval 覆盖自动落盘节流间隔。<=0 时保持默认 1s。
// 只影响之后的 Append 调用；显式 Flush/Commit/Rename/Close 仍然立即落盘。
func (w *IndexWriter) SetFlushInterval(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if d <= 0 {
		d = indexFlushInterval
	}
	w.flushInterval = d
}

// Flush 把缓冲区和文件系统缓存全部落盘。可重复调用。
func (w *IndexWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushLocked()
}

func (w *IndexWriter) flushLocked() error {
	if w.closed {
		return nil
	}
	if w.err != nil {
		return w.err
	}
	if err := w.buf.Flush(); err != nil {
		w.err = err
		return err
	}
	if err := w.f.Sync(); err != nil {
		w.err = err
		return err
	}
	w.lastFlush = time.Now()
	return nil
}

// Commit 追加一条 commit 扩展记录，表示录制正常结束。
func (w *IndexWriter) Commit(totalDuration uint32, samples uint64) error {
	payload := make([]byte, indexCommitPayload)
	binary.BigEndian.PutUint32(payload[0:4], totalDuration)
	binary.BigEndian.PutUint64(payload[4:12], samples)
	return w.writeExtension(indexExtCommit, payload)
}

// Rename 追加一条 rename 扩展记录，记录媒体文件最终落到的路径。
// 它不移动索引文件本身。
func (w *IndexWriter) Rename(mediaPath string) error {
	return w.writeExtension(indexExtRename, []byte(mediaPath))
}

// OffsetShift 追加一条平移意图记录：preLen 是 INSERT_RANGE 之前的文件长度，
// insertLen 是即将在文件头插入的字节数。
//
// 必须在真正执行 INSERT_RANGE **之前**调用：writeExtension 会立即
// Flush+Sync，保证崩溃后恢复侧一定能看到「本次插入可能已生效」的意图，
// 而不是面对「数据已后移、索引还是旧偏移」的静默错位。
func (w *IndexWriter) OffsetShift(preLen, insertLen int64) error {
	if preLen < 0 || insertLen <= 0 {
		return fmt.Errorf("mp4: invalid offset shift preLen=%d insertLen=%d", preLen, insertLen)
	}
	payload := make([]byte, indexOffsetShiftPayload)
	binary.BigEndian.PutUint64(payload[0:8], uint64(preLen))
	binary.BigEndian.PutUint64(payload[8:16], uint64(insertLen))
	return w.writeExtension(indexExtOffsetShift, payload)
}

// OffsetShiftDone 追加一条「索引已按平移后的偏移重写完成」记录。
// 必须在 rewriteIndexRecords 成功之后调用。
func (w *IndexWriter) OffsetShiftDone() error {
	return w.writeExtension(indexExtOffsetShiftDone, nil)
}

func (w *IndexWriter) writeExtension(extType uint16, payload []byte) error {
	if len(payload) > maxIndexPayloadSize {
		return fmt.Errorf("mp4: index extension payload too large: %d", len(payload))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrIndexClosed
	}
	if w.err != nil {
		return w.err
	}
	// 扩展记录必须排在已追加的记录之后。
	if err := w.flushLocked(); err != nil {
		return err
	}
	var frame [IndexRecordSize]byte
	frame[0] = indexFlagExtension
	binary.BigEndian.PutUint16(frame[1:3], extType)
	binary.BigEndian.PutUint32(frame[3:7], uint32(len(payload)))
	if _, err := w.buf.Write(frame[:]); err != nil {
		w.err = err
		return err
	}
	if _, err := w.buf.Write(payload); err != nil {
		w.err = err
		return err
	}
	return w.flushLocked()
}

// Close 落盘并关闭文件。可重复调用。
func (w *IndexWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	err := w.flushLocked()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.closed = true
	if err != nil {
		w.err = err
	}
	return err
}

// RemoveIndex 删除索引文件；文件不存在时视为成功。
func RemoveIndex(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadIndex 读取索引文件。只校验格式，不感知媒体文件长度：
// 调用方应自行在 offset+size 超过媒体大小时停止使用记录。
// 尾部被截断、零填充或结构损坏(如 track ID 为 0)时返回已读出的部分，不报错。
// 样本记录数超过 maxIndexRecords 时视为确定性损坏：返回 ErrIndexCorrupt 并
// 丢弃全部记录，避免伪造的超大索引决定进程内存占用。
func ReadIndex(path string) (*IndexFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readIndexFrom(f)
}

// ReadIndexHeader 只读取并解析索引头部，不加载样本记录。
// 用于在真正恢复前校验 header.MediaPath 等元数据（完整索引可能有数百万条记录）。
func ReadIndexHeader(path string) (IndexHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return IndexHeader{}, err
	}
	defer f.Close()
	return readIndexHeader(f)
}

func readIndexHeader(r io.Reader) (IndexHeader, error) {
	prefix := make([]byte, indexPrefixSize)
	if _, err := io.ReadFull(r, prefix); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			// 头都没写完：崩溃残留。确定性损坏，不会自行变完整。
			return IndexHeader{}, fmt.Errorf("%w: read index header: %w", ErrIndexCorrupt, err)
		}
		return IndexHeader{}, fmt.Errorf("mp4: read index header: %w", err)
	}
	if string(prefix[:len(IndexMagic)]) != IndexMagic {
		return IndexHeader{}, fmt.Errorf("%w: %w", ErrIndexCorrupt, ErrIndexBadMagic)
	}
	headerLen := binary.BigEndian.Uint32(prefix[len(IndexMagic):])
	if headerLen > maxIndexHeaderSize {
		return IndexHeader{}, fmt.Errorf("%w: index header too large: %d", ErrIndexCorrupt, headerLen)
	}
	js := make([]byte, headerLen)
	if _, err := io.ReadFull(r, js); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return IndexHeader{}, fmt.Errorf("%w: truncated index header: %w", ErrIndexCorrupt, err)
		}
		return IndexHeader{}, fmt.Errorf("mp4: truncated index header: %w", err)
	}
	var h IndexHeader
	if err := json.Unmarshal(js, &h); err != nil {
		return IndexHeader{}, fmt.Errorf("%w: invalid index header: %w", ErrIndexCorrupt, err)
	}
	return h, nil
}

func readIndexFrom(r io.Reader) (*IndexFile, error) {
	return readIndexFromLimit(r, maxIndexRecords)
}

// readIndexFromLimit 是 readIndexFrom 的实现，maxRecords 是样本记录数上限。
// 上限做成参数只为让单测用小值覆盖该分支：真正跑满上限要分配数百 MB 的记录
// 数组，不适合放进常规单测。
func readIndexFromLimit(r io.Reader, maxRecords int) (*IndexFile, error) {
	h, err := readIndexHeader(r)
	if err != nil {
		return nil, err
	}
	idx := &IndexFile{Header: h, MediaPath: h.MediaPath}

	var frame [IndexRecordSize]byte
	for {
		if _, err := io.ReadFull(r, frame[:]); err != nil {
			// EOF 或不完整帧都表示尾部被截断：返回已读部分。
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return idx, nil
			}
			return idx, err
		}
		// 零填充尾不是样本：把它当作损坏尾部停止，与截断同一契约。
		// 必须放在扩展记录分派之前——全零帧的 type 也是 0，不能被当成扩展。
		if isZeroIndexFrame(frame[:]) {
			return idx, nil
		}
		if frame[0] != indexFlagExtension {
			r := decodeIndexRecord(frame[:])
			// track ID 0 不是合法轨道，遇到即说明该帧内容已损坏。其余内容
			// 损坏无校验和可查，调用方仍需按 trackID/size 兜底。
			if r.TrackID == 0 {
				return idx, nil
			}
			// 超限是不可接受的：宁可丢弃整个索引，也不能把"读了一半"当成功
			// 返回——调用方会据此写出缺样本的 moov，比不恢复更危险。
			if len(idx.Records) >= maxRecords {
				return nil, fmt.Errorf("%w: index exceeds %d records", ErrIndexCorrupt, maxRecords)
			}
			idx.Records = append(idx.Records, r)
			continue
		}
		extType := binary.BigEndian.Uint16(frame[1:3])
		payloadLen := binary.BigEndian.Uint32(frame[3:7])
		if payloadLen > maxIndexPayloadSize {
			return idx, nil
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(r, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return idx, nil
			}
			return idx, err
		}
		switch extType {
		case indexExtCommit:
			if payloadLen == indexCommitPayload {
				idx.Committed = true
			}
		case indexExtRename:
			idx.MediaPath = string(payload)
			idx.Header.MediaPath = idx.MediaPath
		case indexExtOffsetShift:
			// 多条 SHIFT 时以最后一条为准；负载长度不对属于确定性损坏，
			// 记录状态置为 uncertain 让恢复侧拒绝，而不是当作「没有平移」。
			if payloadLen != indexOffsetShiftPayload {
				idx.OffsetShift = OffsetShiftUncertain
				continue
			}
			idx.OffsetShift = OffsetShiftPending
			idx.ShiftPreLen = int64(binary.BigEndian.Uint64(payload[0:8]))
			idx.ShiftInsertLen = int64(binary.BigEndian.Uint64(payload[8:16]))
		case indexExtOffsetShiftDone:
			if payloadLen != 0 {
				idx.OffsetShift = OffsetShiftUncertain
				continue
			}
			idx.OffsetShift = OffsetShiftApplied
			idx.ShiftPreLen, idx.ShiftInsertLen = 0, 0
		}
	}
}

// isZeroIndexFrame 报告整帧是否全为 0，用于识别零填充尾。
func isZeroIndexFrame(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func encodeIndexRecord(b []byte, r IndexRecord) {
	b[0] = r.Flags
	binary.BigEndian.PutUint16(b[1:3], r.TrackID)
	binary.BigEndian.PutUint32(b[3:7], r.Size)
	binary.BigEndian.PutUint64(b[7:15], r.Offset)
	binary.BigEndian.PutUint32(b[15:19], r.DTS)
	binary.BigEndian.PutUint32(b[19:23], r.Duration)
}

func decodeIndexRecord(b []byte) IndexRecord {
	return IndexRecord{
		Flags:    b[0],
		TrackID:  binary.BigEndian.Uint16(b[1:3]),
		Size:     binary.BigEndian.Uint32(b[3:7]),
		Offset:   binary.BigEndian.Uint64(b[7:15]),
		DTS:      binary.BigEndian.Uint32(b[15:19]),
		Duration: binary.BigEndian.Uint32(b[19:23]),
	}
}
