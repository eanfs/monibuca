package mp4

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	task "github.com/eanfs/gotask"
	"gorm.io/gorm"
	"m7s.live/v5"
	"m7s.live/v5/pkg/storage"
)

// RecoveryUpload 是恢复产物移交给上传队列的契约。
//
// Header 来自 sidecar，保留 streamPath/filePath/storageType 等元数据；
// MediaPath 是恢复所用的源媒体（本地 .mp4 或对象存储暂存 .tmp）；
// OutputPath 是已原子 rename、可直接上传的 moov-first MP4；
// SidecarPath 是伴随索引路径（可能是 .m7sidx 或 .m7sidx.recovering），
// 只有在产物上传成功或 pending 登记成功后才允许删除。
type RecoveryUpload struct {
	Header      IndexHeader
	Records     []IndexRecord
	MediaPath   string
	OutputPath  string
	SidecarPath string
	// DroppedTrackIDs 是索引头声明、但产物里没有可用样本的轨道。回写
	// record_streams 时不得为它们写 codec。
	DroppedTrackIDs []uint16
}

// RecoveryStorageResolver 按录像自己的存储类型解析上传后端。
//
// 生产实现是 (*m7s.Server).GetStorageForType：它只解析 header 声明的类型，
// 不会回落到“当前活跃存储”。测试通过注入该函数来避免依赖真实 S3。
type RecoveryStorageResolver func(storageType string) (storage.Storage, error)

// RecoveryUploadOptions 是恢复上传的可注入依赖；零值即生产默认行为。
type RecoveryUploadOptions struct {
	// Resolver 为空时遍历 m7s.Servers 调用 GetStorageForType。
	Resolver RecoveryStorageResolver
	// DB 用于 pending 登记、幂等查询与恢复成功后的 record_streams 回写。
	// 为空时上传失败不会登记 pending，也不会回写 DB；调用方仍须保留 sidecar
	// 与产物（上传失败分支）。
	DB *gorm.DB
	// Logger 为空时使用 slog.Default()。
	Logger *slog.Logger
	// enqueue 是上传队列注入点（测试用）；为空时投递到包级 uploadQueueTask。
	enqueue func(*finalizeUploadTask, *slog.Logger) error
}

// ErrRecoveryStorageMismatch 表示解析出的后端与 header 声明的存储类型不一致。
// 出现该错误时绝不上传、绝不清理源文件——宁可下次重试，也不能把 s3 录像
// 静默写进 local。
var ErrRecoveryStorageMismatch = errors.New("mp4: recovery upload storage type mismatch")

// recoveryUploadsInFlight 记录已交给上传队列但尚未结束的产物，避免同一次恢复
// 扫描/崩溃窗口重入时重复上传同一文件。键包含存储类型、对象键与本地产物路径。
var recoveryUploadsInFlight sync.Map

// recoveryIndexRecoveringSuffix 与 plugin/mp4 的 recoveringSuffix 保持一致。
const recoveryIndexRecoveringSuffix = ".recovering"

// recoveryUploadSourceMetadataKey 把「本次恢复产物」的路径写进 pending 行
// metadata。pending 登记会把文件移进 pending 目录、LocalPath 随之改变，
// 因此崩溃重入时必须靠这个源标记识别出「仍是同一份产物」。
const recoveryUploadSourceMetadataKey = "recovery-source-path"

// EnqueueRecoveryUpload 使用生产默认依赖（全局 server + 包级上传队列）移交
// 恢复产物。失败返回真实错误，调用方据此保留源文件与 sidecar。
func EnqueueRecoveryUpload(upload RecoveryUpload) error {
	return EnqueueRecoveryUploadWithOptions(upload, RecoveryUploadOptions{})
}

// NewRecoveryUpload 校验一次恢复产物移交并返回规范化副本。所有入队入口都必须
// 经由它，保证任何调用方（包括测试注入路径）都不会把无效移交带进上传流程。
//
// 校验是“失败即保留”语义的一部分：调用方拿到错误时必须保留源媒体与 sidecar，
// 等人工或后续扫描重试。绝不放宽成猜一个默认值：
//   - Header.StorageType 为空 → 无法判定后端；猜 local 会直接把尚未上传的
//     对象存储录像当成本地产物处理（回写后删 sidecar，原对象永远不存在）；
//   - OutputPath / SidecarPath 必须是绝对路径 → 相对路径取决于进程工作目录，
//     恢复任务与上传队列可能在不同目录语义下运行，会删错/读错文件。
//
// storageType 两端的空白会被规范化，避免与后端 GetKey() 的比较因空白失败。
func NewRecoveryUpload(upload RecoveryUpload) (RecoveryUpload, error) {
	storageType := strings.TrimSpace(upload.Header.StorageType)
	if storageType == "" {
		return RecoveryUpload{}, fmt.Errorf(
			"mp4: recovery upload header missing storageType for %q; refusing to guess", upload.OutputPath)
	}
	if !filepath.IsAbs(upload.OutputPath) {
		return RecoveryUpload{}, fmt.Errorf(
			"mp4: recovery upload output path %q must be absolute", upload.OutputPath)
	}
	if !filepath.IsAbs(upload.SidecarPath) {
		return RecoveryUpload{}, fmt.Errorf(
			"mp4: recovery upload sidecar path %q must be absolute", upload.SidecarPath)
	}
	upload.Header.StorageType = storageType
	return upload, nil
}

// EnqueueRecoveryUploadWithOptions 按 header.StorageType 解析后端并把产物交给
// 既有上传队列（finalizeUploadTask）：
//
//   - 解析失败（未配置/未启用/创建失败）→ 返回错误，源文件与 sidecar 保留；
//   - 本地存储 → 产物已在最终路径，无需上传，提交式删除 sidecar；
//   - 对象存储 → 创建 storage.File 并在上传任务中 FinalizeFromTemp，复用
//     Close 的重试/并发控制；成功后删 sidecar + 本地产物，失败后移入
//     pending_uploads 并登记 upload_tasks，只有登记确认后才删 sidecar。
func EnqueueRecoveryUploadWithOptions(upload RecoveryUpload, opts RecoveryUploadOptions) error {
	// 构造校验先于任何副作用：无效移交（缺 storageType / 相对路径）不解析后端、
	// 不投递任务、不碰任何文件，调用方据此保留源媒体与 sidecar。
	validated, err := NewRecoveryUpload(upload)
	if err != nil {
		return err
	}
	upload = validated

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	storageType := upload.Header.StorageType

	backend, err := recoveryResolveStorage(storageType, opts.Resolver)
	if err != nil {
		logger.Warn("recovery upload backend unavailable; keeping recovered file for retry",
			"storageType", storageType,
			"output", upload.OutputPath,
			"err", err)
		return err
	}
	if backend == nil {
		return fmt.Errorf("mp4: recovery upload backend %q resolved to nil: %w",
			storageType, storage.ErrStorageNotAvailable)
	}
	// 绝不降级：解析结果必须与 header 声明一致。若用户把 s3 配错成 local，
	// GetStorageForType 会返回 local；此时宁可失败重试，也不写错后端。
	if resolved := backend.GetKey(); resolved != storageType {
		return fmt.Errorf("%w: header=%q resolved=%q", ErrRecoveryStorageMismatch, storageType, resolved)
	}

	// 本地存储没有上传动作：恢复产物已在最终路径，回写 record_streams
	// 后删除 sidecar 即完成。DB 失败时保留 sidecar，下次恢复会重试回写。
	if storageType == string(storage.StorageTypeLocal) {
		// 纵深防御：sidecar 声明的产物可能已被外部删除/移动。此时既不能回写
		// record_streams（会留下指向不存在文件的记录），也不能删 sidecar
		// （那是下次恢复重试的唯一线索）。
		if _, err := os.Stat(upload.OutputPath); err != nil {
			return fmt.Errorf("mp4: stat recovered local output %s: %w", upload.OutputPath, err)
		}
		if err := updateRecoveredRecord(opts.DB, logger, upload.Header, upload.Records, upload.DroppedTrackIDs); err != nil {
			return fmt.Errorf("mp4: update record_streams for recovered local file %s: %w", upload.OutputPath, err)
		}
		if err := removeRecoverySidecar(upload.SidecarPath); err != nil {
			logger.Warn("remove recovery sidecar failed", "err", err, "sidecar", upload.SidecarPath)
		}
		return nil
	}

	objectKey := recoveryObjectKey(upload.Header)
	if objectKey == "" {
		return fmt.Errorf("mp4: recovery upload missing object path for %s", upload.OutputPath)
	}

	inflightKey := storageType + "\x00" + objectKey + "\x00" + upload.OutputPath
	if _, loaded := recoveryUploadsInFlight.LoadOrStore(inflightKey, struct{}{}); loaded {
		logger.Info("recovery upload already in flight; skipping duplicate",
			"objectKey", objectKey, "output", upload.OutputPath)
		return nil
	}
	release := func() { recoveryUploadsInFlight.Delete(inflightKey) }

	// 崩溃窗口幂等：上一次已登记过 pending（可能尚未补传）或已上传成功时，
	// 不再新建 upload_tasks 行，也不重复上传。
	registered, registerErr := recoveryUploadRegistered(opts.DB, objectKey, upload.OutputPath)
	if registerErr != nil {
		// 判重结果不可确定：既不能重复上传，也不能删 sidecar（那会让这次恢复
		// 永久失去重试线索）。原样保留现场，交给下次扫描。
		release()
		logger.Error("recovery upload registration state unknown; keeping file and sidecar",
			"error", registerErr, "objectKey", objectKey, "output", upload.OutputPath)
		return fmt.Errorf("mp4: check recovery upload registration for %s: %w", upload.OutputPath, registerErr)
	}
	if registered {
		release()
		logger.Info("recovery upload already registered; skipping duplicate",
			"objectKey", objectKey, "output", upload.OutputPath)
		// 上一次恢复可能已入队但尚未回写（进程在两者之间被杀）。这里补写一次；
		// 失败必须保留 sidecar：它是「下次启动再回写 record_streams」的唯一线索，
		// 删掉就等于永久丢失这条录像的 DB 记录。
		if err := updateRecoveredRecord(opts.DB, logger, upload.Header, upload.Records, upload.DroppedTrackIDs); err != nil {
			logger.Error("update record_streams after recovery failed; keeping sidecar",
				"error", err, "objectKey", objectKey, "output", upload.OutputPath)
			return fmt.Errorf("mp4: update record_streams for registered recovery upload %s: %w",
				upload.OutputPath, err)
		}
		if err := removeRecoverySidecar(upload.SidecarPath); err != nil {
			logger.Warn("remove recovery sidecar failed", "err", err, "sidecar", upload.SidecarPath)
		}
		return nil
	}

	info, err := os.Stat(upload.OutputPath)
	if err != nil {
		release()
		return fmt.Errorf("mp4: stat recovery output %s: %w", upload.OutputPath, err)
	}

	file, err := backend.CreateFile(context.Background(), objectKey)
	if err != nil {
		release()
		return fmt.Errorf("mp4: create recovery upload file %s: %w", objectKey, err)
	}
	file.SetMetadata("video-size-bytes", strconv.FormatInt(info.Size(), 10))

	finalizer, ok := file.(storage.TempFileFinalizer)
	if !ok {
		release()
		return fmt.Errorf("mp4: recovery upload backend %s does not support TempFileFinalizer", storageType)
	}

	metadata := map[string]string{
		"video-size-bytes":              strconv.FormatInt(info.Size(), 10),
		recoveryUploadSourceMetadataKey: upload.OutputPath,
	}
	uploadTask := &finalizeUploadTask{
		file:       file,
		filePath:   objectKey,
		streamPath: upload.Header.StreamPath,
		db:         opts.DB,
		// 上传成功后才回写 record_streams，与正常录制路径语义一致：
		// 文件可播放/已持久化前不留下“已完成”的 DB 记录。
		dbWrite: func(task.IJob) error {
			return updateRecoveredRecord(opts.DB, logger, upload.Header, upload.Records, upload.DroppedTrackIDs)
		},
		// 产物移交给 storage.File 的动作放到任务内部执行：任务若未能启动，
		// 源的拥有权仍在恢复任务，不会出现“文件已被接管却没人上传”的悬空状态。
		prepare:   func() error { return finalizer.FinalizeFromTemp(upload.OutputPath) },
		indexPath: upload.SidecarPath,
		size:      info.Size(),
		onDone:    release,
		onFail: func(cause error) {
			registerRecoveryPending(logger, opts.DB, upload, objectKey, storageType, info.Size(), metadata, cause)
		},
	}

	enqueue := opts.enqueue
	if enqueue == nil {
		enqueue = enqueueFinalizeUploadTask
	}
	if err := enqueue(uploadTask, logger); err != nil {
		release()
		return fmt.Errorf("mp4: enqueue recovery upload %s: %w", objectKey, err)
	}
	logger.Info("upload enqueued",
		"file", objectKey,
		"size", info.Size(),
		"retryCount", 0,
		"streamPath", upload.Header.StreamPath,
		"recovered", true)
	return nil
}

// enqueueFinalizeUploadTask 把任务投递到包级上传队列（与正常录制收尾共用）。
// 与 record.go 的 handoffToUploadQueue 一致，不等待任务启动：队列在 server
// 启动时已就绪，等待只会在队列未运行（CLI/测试）时无限阻塞。
func enqueueFinalizeUploadTask(u *finalizeUploadTask, logger *slog.Logger) error {
	return enqueueUploadTask(&uploadQueueTask, u, logger)
}

// enqueueUploadTask 投递上传任务，并把 AddTask 的同步拒绝转成错误。
//
// 任务被拒时永远不会执行 Go()，因此 onDone（在途去重键释放）也不会触发。
// 调用方必须把错误当作「未入队」处理：保留媒体与 sidecar，并释放 in-flight
// 键。否则该文件会在进程存活期间一直命中「recovery upload already in flight」
// 被静默跳过——周期重扫也救不回来，只能等重启。
//
// queue 参数化是为了让测试能构造「已停止的队列」这一拒绝形态。
func enqueueUploadTask(queue task.IJob, u *finalizeUploadTask, logger *slog.Logger) error {
	child := queue.AddTask(u, logger)
	if rejected, reason := m7s.AddTaskRejected(queue, child); rejected {
		if reason != nil {
			return fmt.Errorf("mp4: recovery upload task rejected by task system: %w", reason)
		}
		return errors.New("mp4: recovery upload task rejected by task system")
	}
	return nil
}

// registerRecoveryPending 在上传失败后登记补传。
//
// 只有「媒体已离开原路径（移入 pending）且 upload_tasks 行确认存在」时才删除
// sidecar；原地登记（pending 目录不可用）时媒体还在原路径，sidecar 仍指向真实
// 文件，保留它作为额外恢复保险——下次扫描会因已登记而跳过上传，只回写 DB 并
// 清理 sidecar。其余情况（无 DB / 入库失败）一律保留 sidecar。
func registerRecoveryPending(logger *slog.Logger, db *gorm.DB, upload RecoveryUpload,
	objectKey, storageType string, size int64, metadata map[string]string, cause error) {
	pendingPath, err := m7s.RecoverFailedUpload(logger, db, upload.OutputPath, objectKey, storageType,
		upload.Header.StreamPath, size, 0, metadata, cause)
	if err != nil {
		logger.Error("recovery upload failed and pending registration failed; keeping sidecar",
			"error", err, "cause", cause, "output", upload.OutputPath,
			"pendingPath", pendingPath, "objectKey", objectKey)
		return
	}
	if pendingPath == "" {
		logger.Error("recovery upload failed without pending registration; keeping sidecar",
			"cause", cause, "output", upload.OutputPath, "objectKey", objectKey)
		return
	}
	// RecoverFailedUpload 已经做过一次确认；这里再查一次是纵深防御：任何一步
	// 的「大概写进去了」都不能成为删除 sidecar 的依据。
	confirmed, confirmErr := m7s.ConfirmPendingRegistration(db, pendingPath, objectKey)
	if confirmErr != nil {
		logger.Error("recovery pending registration check failed; keeping sidecar",
			"error", confirmErr, "cause", cause, "pendingPath", pendingPath, "objectKey", objectKey)
		return
	}
	if !confirmed {
		logger.Error("recovery pending registration not confirmed; keeping sidecar",
			"cause", cause, "pendingPath", pendingPath, "objectKey", objectKey)
		return
	}
	if pendingPath == upload.OutputPath {
		logger.Warn("recovery upload failed; registered in place, keeping sidecar",
			"cause", cause, "output", upload.OutputPath, "objectKey", objectKey)
		return
	}
	if err := removeRecoverySidecar(upload.SidecarPath); err != nil {
		logger.Warn("remove recovery sidecar after pending registration failed",
			"err", err, "sidecar", upload.SidecarPath)
	}
	logger.Warn("recovery upload failed; file moved to pending for retry",
		"cause", cause, "pendingPath", pendingPath, "objectKey", objectKey)
}

// recoveryResolveStorage 解析 header 声明的存储类型。绝不使用 GetStorage()
// （当前活跃存储），否则 S3 不可用时会静默把对象存储录像写进 local。
func recoveryResolveStorage(storageType string, resolver RecoveryStorageResolver) (storage.Storage, error) {
	if resolver != nil {
		return resolver(storageType)
	}
	servers := m7s.Servers.ToList()
	if len(servers) == 0 {
		return nil, fmt.Errorf("mp4: no server available to resolve storage %q: %w",
			storageType, storage.ErrStorageNotAvailable)
	}
	var errs []error
	for _, server := range servers {
		backend, err := server.GetStorageForType(storageType)
		if err == nil && backend != nil {
			return backend, nil
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("mp4: storage %q resolved to nil: %w",
			storageType, storage.ErrStorageNotAvailable)
	}
	return nil, fmt.Errorf("mp4: resolve recovery storage %q: %w", storageType, errors.Join(errs...))
}

// recoveryObjectKey 返回传给 storage.CreateFile 的路径。
//
// 优先 FilePath：它是录制时传给 CreateFile 的原始路径，后端会自行追加 prefix。
// StorageKey 是 File.Name()，S3 场景已含 prefix，再传一次会重复前缀。
//
// 纵深防御：header 来自 sidecar，属于不可信输入。绝对路径或含 ".." 段的值
// 一律拒绝（返回空串，调用方已有 "missing object path" 错误分支），避免把
// 恢复产物写成任意绝对路径或穿越到预期目录之外的对象键。
func recoveryObjectKey(header IndexHeader) string {
	key := indexHeaderRecordPath(header)
	if !isSafeRecoveryObjectKey(key) {
		return ""
	}
	return key
}

// isSafeRecoveryObjectKey 判断对象键是否可安全交给存储后端：
// 必须是相对路径，任何一段都不得是 ".."（同时覆盖 / 与 \ 分隔符），
// 且至少要有一段有效名称（"." 之类不是合法的录像路径）。
//
// 与 ValidateIndexHeaderPaths 共用 objectKeyInvalidReason 的规则实现——这里
// 只是第二个调用点（纵深防御：即使上游漏调校验，这里也必须自己拦住），规则
// 本身不再复制一份。
func isSafeRecoveryObjectKey(key string) bool {
	return objectKeyInvalidReason(key) == ""
}

// recoveryUploadRegistered 判断 upload_tasks 是否已经接管了「本次产物」。
//
// 判重必须绑定产物而不是对象键：固定 fileName + fragment>0 时每个分片复用
// 同一对象键，但本地产物路径各不相同。若只看 object_key（或只看“该键下有行
// 且本地文件存在”），分片 A 的 pending 行会把分片 B 误判为已登记，导致 B
// 被跳过、sidecar 被删、B 的 .mp4 留在磁盘却永不补传。
//
// 因此只有以下两种情况视为重复：
//   - 行的本地路径与本次产物路径完全相同（同一文件的崩溃窗口重入）；
//   - 行 metadata 里记录的源产物路径与本次产物路径相同（该产物上传失败后
//     已被移入 pending 目录，LocalPath 变了，但仍是同一份产物）。
//
// 上传成功的行其本地文件已被 MarkUploadSuccess 删除，也不再指向本次产物，
// 因此不会挡住后续复用同名的全新录像。
//
// 返回 (registered, err)：err 非空表示查询失败、结果**不可确定**。过去这种
// 情况被当成「未登记」继续上传，既可能重复上传，也可能把已登记的 sidecar
// 留在磁盘上无人回收。调用方必须按「未确定」处理：不删 sidecar、不上传、
// 保留现场等下次重试。
func recoveryUploadRegistered(db *gorm.DB, objectKey, localPath string) (bool, error) {
	// 没有产物路径就无法把“已登记”绑定到具体产物；宁可重复上传（对象覆盖，
	// 幂等）也不能误删另一份录像的 sidecar。
	if db == nil || localPath == "" {
		return false, nil
	}
	query := db.Model(&m7s.UploadTask{})
	if objectKey != "" {
		query = query.Where("object_key = ? OR local_path = ?", objectKey, localPath)
	} else {
		query = query.Where("local_path = ?", localPath)
	}
	var tasks []m7s.UploadTask
	if err := query.Find(&tasks).Error; err != nil {
		return false, fmt.Errorf("query upload_tasks for %s: %w", localPath, err)
	}
	for _, task := range tasks {
		if task.LocalPath == localPath {
			return true, nil
		}
		if recoverySourcePath(task) == localPath {
			return true, nil
		}
	}
	return false, nil
}

// recoverySourcePath 读取 pending 行里记录的源产物路径。
// 旧版本登记的行没有该标记，返回空串（不会被误认成任意产物）。
func recoverySourcePath(task m7s.UploadTask) string {
	if task.MetadataJSON == "" {
		return ""
	}
	var metadata map[string]string
	if err := json.Unmarshal([]byte(task.MetadataJSON), &metadata); err != nil {
		return ""
	}
	return metadata[recoveryUploadSourceMetadataKey]
}

// removeRecoverySidecar 提交式删除 sidecar；.recovering 场景同时清理同名普通索引。
func removeRecoverySidecar(path string) error {
	if path == "" {
		return nil
	}
	err := RemoveIndex(path)
	if plain := strings.TrimSuffix(path, recoveryIndexRecoveringSuffix); plain != path {
		if plainErr := RemoveIndex(plain); err == nil {
			err = plainErr
		}
	}
	return err
}
