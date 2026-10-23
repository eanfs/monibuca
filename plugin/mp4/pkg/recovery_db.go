package mp4

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
	"m7s.live/v5"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

// recoveryDBTimeout 是恢复回写 record_streams 的超时。恢复是后台任务，
// 不受录制帧回调的 2s 限制；取与正常延迟入库一致的 10s。
const recoveryDBTimeout = 10 * time.Second

// recoveredRecordDuration 由索引记录算出本段录像时长（毫秒）。
// 索引已按文件实际长度截断，最后一条 sample 的 DTS+Duration 即媒体时长。
func recoveredRecordDuration(records []IndexRecord) uint32 {
	var end uint64
	for _, record := range records {
		finish := uint64(record.DTS) + uint64(record.Duration)
		if finish > end {
			end = finish
		}
	}
	if end > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(end)
}

// truncateRecordTime 把时间归一到毫秒。
//
// record_streams.start_time/end_time 在 MySQL 上是 DATETIME(3)（driver 默认
// DefaultDatetimePrecision=3）：写入时 MySQL 会把纳秒截断/四舍五入到毫秒，
// 而查询谓词绑定的字面量仍带纳秒精度，等值比较永远不成立。于是恢复重跑时
// 精确查询落空 → 走 fallback → fallback 也落空（end_time 参数同样是纳秒）
// → Create 出重复行。写入值与所有查询谓词必须使用同一毫秒精度。
func truncateRecordTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Truncate(time.Millisecond)
}

// recoveredTrackCodecTag 返回索引头轨道在录制路径下的 codec 表示。
//
// 录制路径写的是 ICodecCtx.String()，即 RFC 6381 字符串（H.264 avc1.4D401E、
// H.265 hvc1.1.6.L120.00、AAC mp4a.40.2、G.711 alaw/ulaw、opus）。索引头里
// 保存的 stsd 足以推导同样的字段，因此恢复回写也用它，而不是粗粒度的裸
// FourCC——后者会把录制方写下的精确值降级。
// stsd 缺失/不可解析时回退到 FourCC：宁可粗粒度，也不能写空值。
func recoveredTrackCodecTag(track IndexTrack) string {
	if tag := codecTagFromSTSD(track.STSD); tag != "" {
		return tag
	}
	return track.FourCC
}

// codecTagFromSTSD 从 stsd box 推导 RFC 6381 codec 字符串。只处理恢复路径会
// 遇到的 sample entry 类型；其它形态返回空串由调用方回退。
//
// stsd 来自不可信索引头：结构合法但语义畸形的 esds/hvcC 会让底层解析器直接
// panic（gomedia BitStream 在 1 字节 esds 上报 "OUT OF RANGE"；vdk h265parser
// 在 30/31 字节 hvcC 上索引 b[31] 越界）。这里用 recover 兜底并降级为空串
// （调用方回退 FourCC）：返回值只是纯字符串，不存在"半初始化状态"泄漏；
// 恢复流程也绝不会因一条畸形 codec 记录而中断整份录像的 DB 回写。
func codecTagFromSTSD(stsdBytes []byte) (tag string) {
	defer func() {
		if recover() != nil {
			tag = ""
		}
	}()
	if len(stsdBytes) == 0 {
		return ""
	}
	parsed, err := box.ReadFrom(bytes.NewReader(stsdBytes))
	if err != nil {
		return ""
	}
	stsd, ok := parsed.(*box.STSDBox)
	if !ok || len(stsd.Entries) == 0 {
		return ""
	}
	switch entry := stsd.Entries[0].(type) {
	case *box.AudioSampleEntry:
		switch entry.Type() {
		case box.TypeALAW:
			return "alaw"
		case box.TypeULAW:
			return "ulaw"
		case box.TypeOPUS:
			return "opus"
		case box.TypeMP4A:
			esds, ok := entry.ExtraData.(*box.ESDSBox)
			if !ok {
				return ""
			}
			_, record := box.DecodeESDescriptor(esds.Data)
			ctx, _ := codec.NewAACCtxFromRecord(record)
			if ctx != nil && ctx.Config.ObjectType > 0 {
				return ctx.String()
			}
		}
	case *box.VisualSampleEntry:
		data, ok := entry.ExtraData.(*box.DataBox)
		if !ok {
			return ""
		}
		switch entry.Type() {
		case box.TypeAVC1:
			ctx, _ := codec.NewH264CtxFromRecord(data.Data)
			// 构造器即使因缺 SPS/PPS 报错，也已解析出 RecordInfo；
			// profile/level 为 0 说明记录本身不可用，交给 FourCC 回退。
			if ctx != nil && ctx.RecordInfo.AVCProfileIndication != 0 && ctx.RecordInfo.AVCLevelIndication != 0 {
				return ctx.String()
			}
		case box.TypeHVC1, box.TypeHEV1:
			ctx, _ := codec.NewH265CtxFromRecord(data.Data)
			if ctx != nil && ctx.RecordInfo.AVCProfileIndication != 0 && ctx.RecordInfo.AVCLevelIndication != 0 {
				return ctx.String()
			}
		}
	}
	return ""
}

// trackKindsDeclared 报告索引头里是否存在未被丢弃的音轨/视频轨。它只看
// FourCC，不解析 stsd，因此可以在任何写入之前安全调用。
func trackKindsDeclared(header IndexHeader, dropped map[uint16]bool) (audio, video bool) {
	for _, track := range header.Tracks {
		if dropped[track.ID] {
			continue
		}
		cid, ok := codecTypeFromFourCC(track.FourCC)
		if !ok {
			continue
		}
		if cid.IsVideo() {
			video = true
		} else {
			audio = true
		}
	}
	return
}

// recoveredTrackCodecTagFor 返回指定类型中第一条未被丢弃轨道的 codec 表示。
// 只在确实要写入该列时调用：内部会解析不可信的 stsd（见 codecTagFromSTSD）。
func recoveredTrackCodecTagFor(header IndexHeader, dropped map[uint16]bool, wantVideo bool) string {
	for _, track := range header.Tracks {
		if dropped[track.ID] {
			continue
		}
		cid, ok := codecTypeFromFourCC(track.FourCC)
		if !ok || cid.IsVideo() != wantVideo {
			continue
		}
		return recoveredTrackCodecTag(track)
	}
	return ""
}

// droppedTrackKinds 报告被丢弃的轨道里是否包含音轨/视频轨。
func droppedTrackKinds(header IndexHeader, dropped map[uint16]bool) (audioDropped, videoDropped bool) {
	for _, track := range header.Tracks {
		if !dropped[track.ID] {
			continue
		}
		cid, ok := codecTypeFromFourCC(track.FourCC)
		if !ok {
			continue
		}
		if cid.IsVideo() {
			videoDropped = true
		} else {
			audioDropped = true
		}
	}
	return
}

// 恢复回写的「尚未收尾」谓词。
//
// 绝不能绑定 time.Time{}：驱动会把零值时间渲染成零日期字符串，而 MySQL
// DATETIME 的合法下界是 1000-01-01。严格模式下直接报 "Incorrect datetime
// value"（整条回写失败），非严格模式被钳成零日期、谓词永不成立 —— 于是
// fallback 认领不到开录时写入的半成品行，重跑时 Create 出重复记录。
// 这里改用方言无关的字符串字面量比较：
//   - IS NULL 覆盖 GORM `default:NULL` 写入的未收尾行（正常形态）；
//   - < '1970-01-01' 覆盖历史版本把零值 time.Time 直接落库的行：sqlite 存
//     '0001-01-01 ...'、MySQL 非严格模式钳成 '0000-00-00 ...'，两者都小于
//     该下界；真实录像时间远晚于 1970，不会误伤。
const (
	unclosedRecordPredicate           = "(end_time IS NULL OR end_time < '1970-01-01')"
	unclosedOrMatchingRecordPredicate = "(end_time IS NULL OR end_time < '1970-01-01' OR (end_time BETWEEN ? AND ? AND duration = ?))"
)

// updateRecoveredRecord 在恢复成功后回写 record_streams。
//
// 崩溃前开录已插入一行（end_time 空、duration=0），旧的 DB 补录（RunDBBackfill）
// 对已有记录直接跳过，因此恢复流程必须自己补偿：按 header 的 FilePath 匹配，
// 更新 end_time/duration/file_path/storage_type；记录不存在则新建。
//
// end_time = header.StartTime + 索引算出的时长（spec §2.5）。File 保持原样：
// header.FilePath 是逻辑录像路径，也是 record_streams.file_path 的匹配键。
//
// droppedTrackIDs 是索引头声明、但恢复产物里没有可用样本的轨道：命中既有行
// 时它们的 codec 列会被显式清空（"不更新"会保留开录时写入的旧声明）。
// codec 一律使用 RFC 6381 表示（与录制路径的 ICodecCtx.String() 同口径），
// 且既有非空值不会被降级覆盖。
func updateRecoveredRecord(db *gorm.DB, logger *slog.Logger, header IndexHeader, records []IndexRecord, droppedTrackIDs []uint16) error {
	if db == nil {
		return nil
	}
	if err := ValidateIndexHeaderPaths(header); err != nil {
		// 不阻塞上传：产物已经落盘/可上传，只是这条元数据不可信。
		// 写进 DB 会让下载接口按 file_path 直接打开任意文件。
		logger.Warn("recovered record filePath is not bound to mediaPath; skip record_streams update",
			"err", err,
			"streamPath", header.StreamPath,
			"filePath", header.FilePath,
			"mediaPath", header.MediaPath)
		return nil
	}

	filePath := strings.TrimSpace(header.FilePath)
	if filePath == "" {
		filePath = strings.TrimSpace(header.StorageKey)
	}
	if filePath == "" {
		logger.Warn("recovered record has no filePath/storageKey; skip record_streams update",
			"streamPath", header.StreamPath, "mediaPath", header.MediaPath)
		return nil
	}

	// 入口即归一化：写入值与精确查询谓词共用同一 startTime。
	startTime := truncateRecordTime(header.StartTime)
	duration := recoveredRecordDuration(records)
	endTime := time.Time{}
	if !startTime.IsZero() && duration > 0 {
		endTime = startTime.Add(time.Duration(duration) * time.Millisecond)
	}

	updates := map[string]any{
		"file_path": filePath,
	}
	if duration > 0 {
		updates["duration"] = duration
	}
	if !endTime.IsZero() {
		updates["end_time"] = endTime
	}
	if storageType := strings.TrimSpace(header.StorageType); storageType != "" {
		updates["storage_type"] = storageType
	}
	// codec 的回写策略依赖"命中行的既有值"（见下方 found 分支），这里只做
	// 不解析 stsd 的纯计算：被丢弃轨道的类型、各类型是否还有存活轨道。
	// 真正的 codec 推导推迟到确定要写入时，缩小不可信 stsd 的解析面。
	dropped := make(map[uint16]bool, len(droppedTrackIDs))
	for _, id := range droppedTrackIDs {
		dropped[id] = true
	}
	audioDropped, videoDropped := droppedTrackKinds(header, dropped)
	audioPresent, videoPresent := trackKindsDeclared(header, dropped)

	dbCtx, cancel := context.WithTimeout(context.Background(), recoveryDBTimeout)
	defer cancel()

	// 判存必须用 SELECT，不能用 RowsAffected：MySQL 未开 clientFoundRows 时，
	// 对值完全相同的 UPDATE 返回 0。恢复成功写库后若 sidecar 删除失败、或进程
	// 在两者之间被杀，下次启动会再恢复一次——查询能命中行但 UPDATE 是 no-op，
	// 若据此退化到 Create 就会产生重复的 record_streams 行。
	//
	// 定位顺序：
	//  1. file_path + start_time 精确定位本段录像（分片录制可能复用同一
	//     fileName，只按 file_path 会一次改到多条记录）；start_time 已是毫秒精度，
	//     与列精度一致，新版本写入的行必被命中；
	//  2. 历史行（旧版本按纳秒写入，MySQL DATETIME(3) 可能四舍五入到相邻毫秒）
	//     用 ±1ms 窄区间再兜底一次；
	//  3. 历史 sidecar 的 start_time 与 DB 不一致时，退化为该路径下可认领的
	//     行：尚未收尾（end_time 为空），或与本次恢复结果完全一致
	//     （end_time + duration 相同——上次 fallback 已写入，重跑必须幂等）。
	//     后者避免“第一次 fallback 写入后该行不再未收尾，重跑又 Create”；
	//     已完成的历史分片 end_time 各不相同，不会被误认领；
	//  4. 以上都为空才 Create。
	var existing m7s.RecordStream
	found := false

	exact := db.WithContext(dbCtx).Where("file_path = ?", filePath)
	if !startTime.IsZero() {
		exact = exact.Where("start_time = ?", startTime)
	} else {
		// 无起始时间时只能定位「尚未收尾」的那一行。
		exact = exact.Where(unclosedRecordPredicate)
	}
	findErr := exact.First(&existing).Error
	switch {
	case findErr == nil:
		found = true
	case !errors.Is(findErr, gorm.ErrRecordNotFound):
		return findErr
	}

	if !found && !startTime.IsZero() {
		// 历史行兜底：旧版本写入时未做精度归一化，MySQL 对 DATETIME(3) 的
		// 舍入可让库内毫秒值与 header 截断值相差 1ms。
		historical := db.WithContext(dbCtx).Where("file_path = ?", filePath).
			Where("start_time BETWEEN ? AND ?",
				startTime.Add(-time.Millisecond), startTime.Add(time.Millisecond))
		historicalErr := historical.First(&existing).Error
		switch {
		case historicalErr == nil:
			found = true
		case !errors.Is(historicalErr, gorm.ErrRecordNotFound):
			return historicalErr
		}
	}

	if !found && !startTime.IsZero() {
		fallback := db.WithContext(dbCtx).Where("file_path = ?", filePath)
		if !endTime.IsZero() && duration > 0 {
			// end_time 的 ±1ms 同样服务于旧版本写入的历史行。
			fallback = fallback.Where(unclosedOrMatchingRecordPredicate,
				endTime.Add(-time.Millisecond), endTime.Add(time.Millisecond), duration)
		} else {
			fallback = fallback.Where(unclosedRecordPredicate)
		}
		fallbackErr := fallback.First(&existing).Error
		switch {
		case fallbackErr == nil:
			found = true
		case !errors.Is(fallbackErr, gorm.ErrRecordNotFound):
			return fallbackErr
		}
	}

	if found {
		// 被丢弃的轨道必须**显式清空** codec 列。命中行是开录时写入的
		// （recoder.go 已填好 AudioCodec/VideoCodec），只"不写该 key"会把
		// 旧声明原样保留，API 仍会宣称产物里有一条实际不存在的音轨。
		if audioDropped && !audioPresent {
			updates["audio_codec"] = ""
		}
		if videoDropped && !videoPresent {
			updates["video_codec"] = ""
		}
		// 绝不降级：既有非空值是录制方用 ICodecCtx.String() 写入的权威
		// RFC 6381 表示（avc1.4D401E），恢复只有在该列为空时才补写——也只有在
		// 这时才解析（不可信的）stsd 推导 codec。
		if audioPresent && strings.TrimSpace(existing.AudioCodec) == "" {
			if tag := recoveredTrackCodecTagFor(header, dropped, false); tag != "" {
				updates["audio_codec"] = tag
			}
		}
		if videoPresent && strings.TrimSpace(existing.VideoCodec) == "" {
			if tag := recoveredTrackCodecTagFor(header, dropped, true); tag != "" {
				updates["video_codec"] = tag
			}
		}
		// 只按主键更新命中行；即使值完全相同（MySQL RowsAffected=0）也算成功。
		if err := db.WithContext(dbCtx).Model(&m7s.RecordStream{}).
			Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
			return err
		}
		logger.Info("record_streams updated after recovery",
			"filePath", filePath,
			"recordId", existing.ID,
			"duration", duration,
			"storageType", header.StorageType)
		return nil
	}

	// 走到这里说明按 file_path + start_time 没有找到既有行。若本次的绝对
	// file_path 只是以「MediaPath 尾部」形态通过绑定（recoveryBindingMediaSuffix：
	// MediaPath = <basePath> + FilePath），则不允许新建行，只 Warn。
	//
	// 为什么 DB 写入要额外收窄：
	//   - 后缀关系在 sidecar 不可信时可以**被攻击者自造**——他同时控制
	//     MediaPath 与 FilePath：先投放媒体 <恢复目录>/etc/shadow，再写
	//     FilePath=/etc/shadow，后缀即成立。
	//   - 而绝对 file_path 在下载侧是直接打开的：api.go:151-158 的
	//     http.ServeFile / os.Open，GetFullPath 只用于相对路径分支（api.go:186）。
	//     所以"新建一行 file_path=/etc/shadow"= 任意文件读取。
	//   - 该形态只出现在「local + 绝对 record.filepath + 非空 basePath」的既有
	//     坏配置上（下载本来就打不开），因此只更新既有行是正确取舍：
	//     既有行由录制进程自己写入，其 file_path 必然是真实的逻辑路径。
	if isSuffixBoundAbsoluteFilePath(header) {
		logger.Warn("recovered record filePath only matched as a suffix of mediaPath and no existing row was found; skip creating record_streams row",
			"filePath", filePath,
			"mediaPath", header.MediaPath,
			"streamPath", header.StreamPath,
			"storageType", header.StorageType)
		return nil
	}

	record := m7s.RecordStream{
		StartTime:  startTime,
		EndTime:    endTime,
		Duration:   duration,
		FileName:   filepath.Base(filePath),
		Type:       "mp4",
		FilePath:   filePath,
		StreamPath: header.StreamPath,
		// 新建行没有可保留的既有值，直接写入推导结果（缺失时为空）。
		AudioCodec:   recoveredTrackCodecTagFor(header, dropped, false),
		VideoCodec:   recoveredTrackCodecTagFor(header, dropped, true),
		StorageLevel: 1,
		StorageType:  strings.TrimSpace(header.StorageType),
	}
	if err := db.WithContext(dbCtx).Create(&record).Error; err != nil {
		return err
	}
	logger.Info("record_streams created after recovery",
		"filePath", filePath,
		"duration", duration,
		"storageType", header.StorageType)
	return nil
}
