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

// EnqueueRecoveryUploadWithOptions 按 header.StorageType 解析后端并把产物交给
// 既有上传队列（finalizeUploadTask）：
//
//   - 解析失败（未配置/未启用/创建失败）→ 返回错误，源文件与 sidecar 保留；
//   - 本地存储 → 产物已在最终路径，无需上传，提交式删除 sidecar；
//   - 对象存储 → 创建 storage.File 并在上传任务中 FinalizeFromTemp，复用
//     Close 的重试/并发控制；成功后删 sidecar + 本地产物，失败后移入
//     pending_uploads 并登记 upload_tasks，只有登记确认后才删 sidecar。
func EnqueueRecoveryUploadWithOptions(upload RecoveryUpload, opts RecoveryUploadOptions) error {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// header 缺 storageType 时不能按 local 处理：这可能是个对象存储暂存文件，
	// 静默当成本地会直接删掉未上传的录像。宁可报错保留，等人工/后续修复。
	storageType := strings.TrimSpace(upload.Header.StorageType)
	if storageType == "" {
		return fmt.Errorf("mp4: recovery upload header missing storageType for %s; refusing to guess",
			upload.OutputPath)
	}

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
		if err := updateRecoveredRecord(opts.DB, logger, upload.Header, upload.Records); err != nil {
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
	if recoveryUploadRegistered(opts.DB, objectKey, upload.OutputPath) {
		release()
		logger.Info("recovery upload already registered; skipping duplicate",
			"objectKey", objectKey, "output", upload.OutputPath)
		// 上一次恢复可能已入队但尚未回写（进程在两者之间被杀）。这里补写一次；
		// 失败只告警——上传已登记，sidecar 不能再留着阻塞后续扫描。
		if err := updateRecoveredRecord(opts.DB, logger, upload.Header, upload.Records); err != nil {
			logger.Warn("update record_streams after recovery failed",
				"err", err, "objectKey", objectKey, "output", upload.OutputPath)
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
			return updateRecoveredRecord(opts.DB, logger, upload.Header, upload.Records)
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
// 启动时已就绪，等待只会在队列未运行（CLI/测试）时无限阻塞。即使任务未能
// 执行，恢复任务也会因 sidecar 未删除而在下次启动重试。
func enqueueFinalizeUploadTask(u *finalizeUploadTask, logger *slog.Logger) error {
	uploadQueueTask.AddTask(u, logger)
	return nil
}

// registerRecoveryPending 在上传失败后登记 pending 补传。
// 只有「文件已移入 pending 且 upload_tasks 行确认存在」时才删除 sidecar；
// 其余情况（无 DB / pending 满 / 入库失败）保留 sidecar 供下次恢复重试。
func registerRecoveryPending(logger *slog.Logger, db *gorm.DB, upload RecoveryUpload,
	objectKey, storageType string, size int64, metadata map[string]string, cause error) {
	pendingPath, err := m7s.RecoverFailedUpload(logger, db, upload.OutputPath, objectKey, storageType,
		upload.Header.StreamPath, size, 0, metadata, cause)
	if err != nil {
		logger.Error("recovery upload failed and pending registration failed; keeping sidecar",
			"err", err, "cause", cause, "output", upload.OutputPath, "objectKey", objectKey)
		return
	}
	if pendingPath == "" {
		logger.Error("recovery upload failed without pending registration; keeping sidecar",
			"cause", cause, "output", upload.OutputPath, "objectKey", objectKey)
		return
	}
	if !recoveryPendingConfirmed(db, pendingPath) {
		logger.Error("recovery pending registration not confirmed; keeping sidecar",
			"cause", cause, "pendingPath", pendingPath, "objectKey", objectKey)
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
	key := strings.TrimSpace(header.FilePath)
	if key == "" {
		key = strings.TrimSpace(header.StorageKey)
	}
	if !isSafeRecoveryObjectKey(key) {
		return ""
	}
	return key
}

// isSafeRecoveryObjectKey 判断对象键是否可安全交给存储后端：
// 必须是相对路径，任何一段都不得是 ".."（同时覆盖 / 与 \ 分隔符），
// 且至少要有一段有效名称（"." 之类不是合法的录像路径）。
//
// 与 ValidateIndexHeaderPaths 独立：即使上游漏调校验，这里也必须自己拦住。
func isSafeRecoveryObjectKey(key string) bool {
	if key == "" || filepath.IsAbs(key) ||
		strings.HasPrefix(key, "/") || strings.HasPrefix(key, `\`) ||
		filepath.VolumeName(key) != "" {
		return false
	}
	if hasTraversalSegment(key) {
		return false
	}
	return hasMeaningfulObjectKeySegment(key)
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
func recoveryUploadRegistered(db *gorm.DB, objectKey, localPath string) bool {
	// 没有产物路径就无法把“已登记”绑定到具体产物；宁可重复上传（对象覆盖，
	// 幂等）也不能误删另一份录像的 sidecar。
	if db == nil || localPath == "" {
		return false
	}
	query := db.Model(&m7s.UploadTask{})
	if objectKey != "" {
		query = query.Where("object_key = ? OR local_path = ?", objectKey, localPath)
	} else {
		query = query.Where("local_path = ?", localPath)
	}
	var tasks []m7s.UploadTask
	if err := query.Find(&tasks).Error; err != nil {
		return false
	}
	for _, task := range tasks {
		if task.LocalPath == localPath {
			return true
		}
		if recoverySourcePath(task) == localPath {
			return true
		}
	}
	return false
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

// recoveryPendingConfirmed 确认 pending 登记真的落库。
// m7s.RecoverFailedUpload 不返回 DB 写入错误，因此必须回查。
func recoveryPendingConfirmed(db *gorm.DB, pendingPath string) bool {
	if db == nil || pendingPath == "" {
		return false
	}
	var count int64
	if err := db.Model(&m7s.UploadTask{}).Where("local_path = ?", pendingPath).Count(&count).Error; err != nil {
		return false
	}
	return count > 0
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
