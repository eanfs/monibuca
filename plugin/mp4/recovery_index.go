package plugin_mp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	task "github.com/eanfs/gotask"
	"gorm.io/gorm"
	"m7s.live/v5/pkg/storage"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// 恢复状态标记：
//
//	<media>.m7sidx            待恢复
//	<media>.m7sidx.recovering 已开始定稿（崩溃后下次启动会重试）
//	<media>.m7sidx.failed     确定性失败（无可用样本/索引损坏），不再重试
const (
	recoveringSuffix = ".recovering"
	failedSuffix     = ".failed"
)

// RecoveryItem 是恢复任务产出的单个文件，也是移交给上传队列的契约。
// Task 6 负责基于 Header.StorageType 解析上传目标与 pending 语义。
type RecoveryItem struct {
	Header     mp4pkg.IndexHeader
	Records    []mp4pkg.IndexRecord
	MediaPath  string
	OutputPath string
	// SidecarPath 是本次恢复的伴随索引（可能是 .m7sidx 或 .recovering）。
	// 只有产物上传成功或 pending 登记成功后才允许删除。
	SidecarPath string
}

// IndexRecoveryTask 启动时扫描残留伴随索引并定稿为可播放 MP4。
//
// 它与旧的 RecordRecoveryTask 合并为一次启动扫描：先做 DB 补录（可选），
// 再做索引定稿。定稿流程是“写临时文件 → fsync → 原子 rename → 入上传队列 →
// 清理源暂存/sidecar”，因此进程在任意点被杀都能在下次启动继续。
type IndexRecoveryTask struct {
	task.Work

	// DB 仅用于合并旧的 DB 补录扫描；nil 时跳过。
	DB *gorm.DB
	// Dirs 是配置的额外扫描目录（record.recovery.dirs）；空则使用默认目录。
	Dirs []string
	// Concurrency 是单文件定稿的并发上限；<=0 时使用默认 2。
	Concurrency int
	// MinAge 是「仍在写入」判定阈值：sidecar/媒体 mtime 新于该值时跳过，
	// 避免把活跃录像的 sidecar 改名、媒体文件覆盖。生效值无条件夹紧为
	// max(MinAge, 5s, 2×索引落盘间隔)；显式传负数表示关闭该保护
	// （仅测试/特殊场景）。
	MinAge time.Duration
	// RunOnStart 控制任务注册后是否立即跑一次。
	RunOnStart bool
	// RunDBBackfill 对应旧的 AutoRecovery：补录 DB 记录。它与索引定稿解耦：
	// RunOnStart=false 只关闭索引扫描，DB 补录仍然执行。
	RunDBBackfill bool
	// Backfill 可注入 DB 补录实现（测试/定制）；nil 时使用 RecordRecoveryTask。
	Backfill func() (RecoveryStats, error)
	// EnqueueUpload 可注入，用于测试或 Task 6 自定义移交；nil 时走 pkg 上传队列。
	EnqueueUpload func(RecoveryItem) error
	// ResolveUploadStorage 可注入上传后端解析（测试用）；nil 时由 pkg 按
	// m7s.Servers 调用 GetStorageForType。
	ResolveUploadStorage mp4pkg.RecoveryStorageResolver

	plugin *MP4Plugin

	// runMu 串行化整个扫描：启动扫描与 API 手动触发不能同时处理同一文件。
	runMu sync.Mutex
}

const (
	defaultRecoveryConcurrency = 2
	// defaultRecoveryMinAge 是「仍在写入」判定阈值的下限。索引默认每秒落盘，
	// 2× 落盘间隔通常已足够区分活跃录像；取 5s 作为下限避免误判。
	defaultRecoveryMinAge = 5 * time.Second
)

func (t *IndexRecoveryTask) Start() error {
	t.Info("index recovery task started",
		"concurrency", t.effectiveConcurrency(),
		"minAge", t.effectiveMinAge(),
		"runOnStart", t.RunOnStart,
		"runDBBackfill", t.RunDBBackfill,
		"dirs", t.Dirs)
	// 不能在 Start 里等待子任务（会阻塞事件循环）。run task 在自己的
	// goroutine 中执行，完成后自行记录日志。
	switch {
	case t.RunOnStart:
		t.AddTask(&recoveryRunTask{
			owner: t,
			stats: &RecoveryStats{},
			done:  make(chan struct{}),
		}, t.Logger)
	case t.RunDBBackfill:
		// I1：DB 补录与索引定稿解耦。onstart=false 只关闭索引扫描，
		// 旧 AutoRecovery 的 DB 补录必须照常执行，否则会静默失效。
		t.Warn("index recovery disabled by record.recovery.onstart=false; running DB backfill only")
		t.AddTask(&recoveryRunTask{
			owner:        t,
			stats:        &RecoveryStats{},
			done:         make(chan struct{}),
			backfillOnly: true,
		}, t.Logger)
	}
	return nil
}

// RunOnce 同步执行一轮扫描并返回统计。空 dirs 表示使用配置/默认目录。
// 任务已启动时通过子任务执行（并发受 task.Work 管理）；未启动时退化为
// 串行执行，便于单元测试与 CLI 直接调用。
func (t *IndexRecoveryTask) RunOnce(dirs []string) (RecoveryStats, error) {
	if !t.taskStarted() {
		t.runMu.Lock()
		defer t.runMu.Unlock()
		return t.finish(t.runRecovery(dirs, false))
	}

	stats := RecoveryStats{}
	done := make(chan struct{})
	child := t.AddTask(&recoveryRunTask{
		owner: t,
		dirs:  dirs,
		stats: &stats,
		done:  done,
	}, t.Logger)
	if err := child.WaitStarted(); err != nil {
		return RecoveryStats{}, fmt.Errorf("start index recovery: %w", err)
	}
	<-done
	return t.finish(stats)
}

func (t *IndexRecoveryTask) finish(stats RecoveryStats) (RecoveryStats, error) {
	if len(stats.Errors) > 0 {
		return stats, fmt.Errorf("recovery completed with %d errors", len(stats.Errors))
	}
	return stats, nil
}

func (t *IndexRecoveryTask) effectiveConcurrency() int {
	if t.Concurrency > 0 {
		return t.Concurrency
	}
	return defaultRecoveryConcurrency
}

// effectiveMinAge 返回「仍在写入」判定阈值，语义为：
//   - 负值：显式关闭保护（仅测试/特殊场景）；
//   - 其余：max(配置值, 5s, 2×索引落盘间隔) 无条件夹紧。
//
// 夹紧必须无条件生效：配置默认值 5s 并非 0，若只在「未配置」时夹紧，
// 运维把 record.index.flushinterval 调到 >2.5s 后，活跃 sidecar 的最大
// 落盘滞后会超过 5s，活跃保护被削弱。
func (t *IndexRecoveryTask) effectiveMinAge() time.Duration {
	if t.MinAge < 0 {
		return t.MinAge
	}
	minAge := t.MinAge
	floor := 2 * mp4pkg.GetIndexConfig().FlushInterval
	if floor < defaultRecoveryMinAge {
		floor = defaultRecoveryMinAge
	}
	if minAge < floor {
		minAge = floor
	}
	return minAge
}

func (t *IndexRecoveryTask) taskStarted() bool {
	state := t.GetState()
	return state >= task.TASK_STATE_STARTED && state < task.TASK_STATE_DISPOSING
}

// recoveryRunTask 在独立 goroutine 中跑完整扫描（task.Work 管理其生命周期）。
type recoveryRunTask struct {
	task.Task
	owner *IndexRecoveryTask
	dirs  []string
	stats *RecoveryStats
	done  chan struct{}
	// backfillOnly 为 true 时只跑 DB 补录（onstart=false 但 AutoRecovery 开启）。
	backfillOnly bool
}

func (r *recoveryRunTask) Go() error {
	defer close(r.done)
	r.owner.runMu.Lock()
	defer r.owner.runMu.Unlock()
	if r.backfillOnly {
		*r.stats = r.owner.runDBBackfill()
		return nil
	}
	*r.stats = r.owner.runRecovery(r.dirs, true)
	return nil
}

type recoveryOutcome int

const (
	recoveryFailed recoveryOutcome = iota
	recoveryRecovered
	recoverySkipped
)

type recoveryCollector struct {
	mu    sync.Mutex
	stats RecoveryStats
}

func (c *recoveryCollector) record(path string, outcome recoveryOutcome, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.recordOutcome(path, outcome, err)
}

func (c *recoveryCollector) addError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Errors = append(c.stats.Errors, err)
}

func (c *recoveryCollector) snapshot() RecoveryStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stats
	out.Errors = append([]error(nil), c.stats.Errors...)
	return out
}

func (s *RecoveryStats) recordOutcome(path string, outcome recoveryOutcome, err error) {
	if err != nil {
		s.Errors = append(s.Errors, fmt.Errorf("%s: %w", path, err))
	}
	switch outcome {
	case recoveryRecovered:
		s.Recovered++
	case recoverySkipped:
		s.Skipped++
	case recoveryFailed:
		s.Failed++
	}
}

// recoveryWorkerTask 是并发池中的 worker，从 jobs 取 sidecar 并逐个定稿。
type recoveryWorkerTask struct {
	task.Task
	owner     *IndexRecoveryTask
	jobs      <-chan string
	wg        *sync.WaitGroup
	collector *recoveryCollector
}

func (w *recoveryWorkerTask) Go() error {
	defer w.wg.Done()
	for sidecar := range w.jobs {
		outcome, _, err := w.owner.recoverSidecar(sidecar)
		w.collector.record(sidecar, outcome, err)
	}
	return nil
}

// runDBBackfill 执行旧 AutoRecovery 的 DB 补录。它独立于索引定稿，
// 因此 record.recovery.onstart=false 不会静默关闭它。
func (t *IndexRecoveryTask) runDBBackfill() RecoveryStats {
	if !t.RunDBBackfill {
		return RecoveryStats{}
	}
	if t.Backfill != nil {
		stats, err := t.Backfill()
		if err != nil {
			stats.Errors = append(stats.Errors, err)
		}
		return stats
	}
	if t.DB == nil {
		return RecoveryStats{}
	}
	backfill := &RecordRecoveryTask{DB: t.DB, plugin: t.plugin}
	stats, _ := backfill.RunBackfill()
	return stats
}

// runRecovery 是扫描主流程，调用方必须持有 runMu。
func (t *IndexRecoveryTask) runRecovery(dirs []string, useTasks bool) RecoveryStats {
	stats := RecoveryStats{}

	// 1. 先跑既有 DB 补录（旧 AutoRecovery 行为），再定稿索引。
	stats.merge(t.runDBBackfill())

	resolved, dirErrs := t.resolveDirs(dirs)
	stats.Errors = append(stats.Errors, dirErrs...)

	files, bareTmp, walkErrs := discoverSidecars(resolved)
	stats.BareTmp = bareTmp
	stats.Errors = append(stats.Errors, walkErrs...)
	stats.Scanned += len(files)

	if bareTmp > 0 {
		// Task 7 负责无索引 tmp 的反向重建；此处只统计，不删除、不移动。
		t.Info("recovery scan found bare media tmp without sidecar (skipped)", "count", bareTmp)
	}

	if len(files) > 0 {
		if useTasks && t.effectiveConcurrency() > 1 && len(files) > 1 {
			concurrent := t.processWithWorkers(files)
			stats.merge(concurrent)
		} else {
			for _, sidecar := range files {
				outcome, _, err := t.recoverSidecar(sidecar)
				stats.recordOutcome(sidecar, outcome, err)
			}
		}
	}

	t.Info("index recovery completed",
		"scanned", stats.Scanned,
		"recovered", stats.Recovered,
		"failed", stats.Failed,
		"skipped", stats.Skipped,
		"bareTmp", stats.BareTmp,
		"errors", len(stats.Errors))
	return stats
}

func (t *IndexRecoveryTask) processWithWorkers(files []string) RecoveryStats {
	workers := t.effectiveConcurrency()
	if workers > len(files) {
		workers = len(files)
	}
	jobs := make(chan string, len(files))
	for _, file := range files {
		jobs <- file
	}
	close(jobs)

	collector := &recoveryCollector{}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		worker := &recoveryWorkerTask{
			owner:     t,
			jobs:      jobs,
			wg:        &wg,
			collector: collector,
		}
		child := t.AddTask(worker, t.Logger)
		if err := child.WaitStarted(); err != nil {
			wg.Done()
			collector.addError(fmt.Errorf("start recovery worker: %w", err))
		}
	}
	wg.Wait()
	return collector.snapshot()
}

// resolveDirs 解析扫描目录。显式/配置目录不存在时报错；默认目录不存在时静默跳过。
func (t *IndexRecoveryTask) resolveDirs(explicit []string) ([]string, []error) {
	dirs := explicit
	strict := len(explicit) > 0
	if len(dirs) == 0 {
		dirs = t.Dirs
		strict = len(dirs) > 0
	}
	if len(dirs) == 0 {
		dirs = t.defaultDirs()
	}

	seen := make(map[string]bool, len(dirs))
	var resolved []string
	var errs []error
	for _, dir := range dirs {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("resolve recovery dir %q: %w", dir, err))
			continue
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		info, err := os.Stat(abs)
		if err != nil {
			if strict || !os.IsNotExist(err) {
				errs = append(errs, fmt.Errorf("recovery dir %s: %w", abs, err))
			}
			continue
		}
		if !info.IsDir() {
			if strict {
				errs = append(errs, fmt.Errorf("recovery dir %s is not a directory", abs))
			}
			continue
		}
		resolved = append(resolved, abs)
	}
	return resolved, errs
}

// defaultDirs = 对象存储本地暂存目录 + pending_uploads + 录像输出目录。
// 录像输出目录保证本地存储场景下崩溃残留的 sidecar 也能被发现。
func (t *IndexRecoveryTask) defaultDirs() []string {
	dirs := []string{t.mediaTempDir()}
	if pending := storage.GetPendingDir(); pending != "" {
		dirs = append(dirs, pending)
	}
	if t.plugin != nil {
		if common := t.plugin.GetCommonConf(); common != nil {
			for _, conf := range common.OnPub.Record {
				if conf.FilePath != "" {
					dirs = append(dirs, filepath.Dir(conf.FilePath))
				}
			}
		}
		if t.plugin.EventRecordFilePath != "" {
			dirs = append(dirs, filepath.Dir(t.plugin.EventRecordFilePath))
		}
	}
	return dirs
}

// mediaTempDir 返回对象存储暂存文件目录（storage.s3.tempdir），未配置时用系统临时目录。
func (t *IndexRecoveryTask) mediaTempDir() string {
	if t.plugin != nil && t.plugin.Server != nil {
		if raw, ok := t.plugin.Server.ServerConfig.Storage[string(storage.StorageTypeS3)]; ok {
			if conf, ok := raw.(map[string]any); ok {
				// 不引用 storage.S3StorageConfig：s3.go 有 build tag，
				// 未启用 s3 时该类型不存在。配置键在解析后统一小写。
				if tempDir, ok := conf["tempdir"].(string); ok && strings.TrimSpace(tempDir) != "" {
					return tempDir
				}
			}
		}
	}
	return os.TempDir()
}

// discoverSidecars 递归收集 *.m7sidx 与 *.m7sidx.recovering。
// 同一媒体同时存在两种标记时优先 .recovering；无 sidecar 的 s3writer_*.tmp
// 只计数（Task 7 处理），不进入恢复队列。
func discoverSidecars(dirs []string) (files []string, bareTmp int, errs []error) {
	preferred := make(map[string]string) // base(.m7sidx) -> 实际路径
	bare := make(map[string]bool)

	for _, dir := range dirs {
		walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
				return nil
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			switch {
			case strings.HasSuffix(name, mp4pkg.IndexExtension+recoveringSuffix):
				preferred[strings.TrimSuffix(path, recoveringSuffix)] = path
			case strings.HasSuffix(name, mp4pkg.IndexExtension):
				if _, ok := preferred[path]; !ok {
					preferred[path] = path
				}
			case strings.HasSuffix(name, mp4pkg.IndexExtension+".tmp"),
				strings.HasSuffix(name, mp4pkg.IndexExtension+failedSuffix):
				// 重写中间态或已判定失败的标记：忽略，避免与恢复队列重复。
			case strings.HasPrefix(name, "s3writer_") && strings.HasSuffix(name, ".tmp"):
				if !hasIndexSidecar(path) {
					bare[path] = true
				}
			}
			return nil
		})
		if walkErr != nil {
			errs = append(errs, fmt.Errorf("walk recovery dir %s: %w", dir, walkErr))
		}
	}

	for _, path := range preferred {
		files = append(files, path)
	}
	sort.Strings(files)
	return files, len(bare), errs
}

func hasIndexSidecar(mediaPath string) bool {
	index := mediaPath + mp4pkg.IndexExtension
	if fileExists(index) || fileExists(index+recoveringSuffix) {
		return true
	}
	return false
}

// recoverSidecar 定稿单个 sidecar。任何返回错误的分支都保留可重试状态，
// 除非错误被明确判定为永久性（索引损坏/无可用样本 → .failed）。
func (t *IndexRecoveryTask) recoverSidecar(sidecar string) (recoveryOutcome, RecoveryItem, error) {
	index, err := mp4pkg.ReadIndex(sidecar)
	if err != nil {
		// 活跃录制中的 sidecar 刚创建时可能短暂不可读。先看 mtime：
		// 仍在写入的文件不判失败，留给下次 RunOnce。
		if fresh, age := t.isFreshlyWritten(sidecar, mediaPathFromSidecar(sidecar)); fresh {
			t.Info("skip recovery of sidecar still being written (read failed)",
				"sidecar", sidecar, "age", age, "minAge", t.effectiveMinAge(), "err", err)
			return recoverySkipped, RecoveryItem{}, nil
		}
		// 只有确定性损坏才永久落 .failed；IO/挂载点暂时不可用必须保留，
		// 否则一次抖动就会永久放弃可恢复的录像（Task 9 无重试入口）。
		if isPermanentIndexError(err) {
			t.markFailed(sidecar)
			return recoveryFailed, RecoveryItem{}, fmt.Errorf("read index: %w", err)
		}
		t.Warn("read index failed, will retry on next run", "err", err, "sidecar", sidecar)
		return recoveryFailed, RecoveryItem{}, fmt.Errorf("read index: %w", err)
	}

	recorded := index.MediaPath
	if recorded == "" {
		recorded = mediaPathFromSidecar(sidecar)
	}
	source, found := locateRecoveryMedia(recorded, sidecar)
	output := recoveryOutputPath(recorded)

	item := RecoveryItem{
		Header:      index.Header,
		Records:     index.Records,
		MediaPath:   source,
		OutputPath:  output,
		SidecarPath: sidecar,
	}

	if !found {
		// 崩溃窗口：产物已 rename，但源暂存文件已删除、sidecar 尚未清理。
		if output != "" && fileExists(output) {
			if err := t.enqueue(item); err != nil {
				return recoveryFailed, item, err
			}
			t.afterEnqueueSidecar(sidecar)
			t.Info("recovery output already in place", "sidecar", sidecar, "output", output)
			return recoveryRecovered, item, nil
		}
		// 媒体文件暂时不可见（挂载点抖动/尚未落盘）不是永久失败：
		// 保留 sidecar，下次 RunOnce 再试。
		t.Warn("recovery media not found, will retry on next run",
			"sidecar", sidecar, "media", recorded)
		return recoveryFailed, item, fmt.Errorf("media file not found: %s", recorded)
	}

	item.MediaPath = source
	item.OutputPath = output

	// C2：跳过仍在写入的文件。活跃 recorder 的 sidecar 同样未提交、与崩溃残留
	// 不可区分；若此时 markRecovering 并重建，会 rename 走活跃 sidecar、
	// 覆盖活跃媒体文件。mtime 新鲜即视为活跃，计入 Skipped。
	//
	// 这道闸门必须先于 C1：本地实时录制的 trailer INSERT_RANGE / moov 前移
	// 窗口内文件已有顶层 moov 但仍在被写，此时 C1 会误判「已收尾」并提前
	// 入队、删掉活跃 sidecar（Task 6 落地后还会上传半成品）。先做新鲜度判定
	// 才能同时保护两条分支。
	if fresh, age := t.isFreshlyWritten(source, sidecar); fresh {
		t.Info("skip recovery of file still being written",
			"sidecar", sidecar, "media", source, "age", age, "minAge", t.effectiveMinAge())
		return recoverySkipped, item, nil
	}

	// C1：本地录制的源与输出是同一路径。若文件已有顶层 moov，说明要么已正常
	// 收尾、要么上一次恢复的产物已 rename 到位（进程在入队/清理之间被杀）。
	// 此时绝不能再次 RestoreFromIndex：索引 Offset 是原坐标系，而产物已
	// moov-first、载荷整体后移，旧偏移仍在文件范围内，不会触发
	// ErrIndexTruncated —— 会把 moov 字节当载荷，写出结构合法但内容全错的
	// mp4 并覆盖正确产物。直接入队 + 清 sidecar 即可。
	if source == output && isMP4Path(source) && hasTopLevelMoov(source) {
		if err := t.enqueue(item); err != nil {
			return recoveryFailed, item, err
		}
		t.afterEnqueueSidecar(sidecar)
		t.Info("recovery output already contains moov, skipping rebuild", "sidecar", sidecar, "output", output)
		return recoverySkipped, item, nil
	}

	// 录制已正常收尾（committed）且本地 mp4 已有 moov：无需重建，
	// 只需重新移交上传队列（上传失败/pending 补传场景）。
	if index.Committed && isMP4Path(source) && hasTopLevelMoov(source) {
		if err := t.enqueue(item); err != nil {
			return recoveryFailed, item, err
		}
		t.afterEnqueueSidecar(sidecar)
		return recoverySkipped, item, nil
	}

	marker, err := t.markRecovering(sidecar)
	if err != nil {
		return recoveryFailed, item, err
	}
	// 入队时 sidecar 已改名为 .recovering：上传任务成功后按该路径提交式删除。
	item.SidecarPath = marker

	info, err := os.Stat(source)
	if err != nil {
		// 保留 .recovering，下次重试；Stat 失败多半是 IO/挂载问题。
		t.Warn("stat recovery media failed, will retry on next run", "err", err, "media", source)
		return recoveryFailed, item, fmt.Errorf("stat media: %w", err)
	}

	muxer := mp4pkg.NewMuxer(0)
	restoreErr := muxer.RestoreFromIndex(index.Header, index.Records, info.Size())
	switch {
	case errors.Is(restoreErr, mp4pkg.ErrNoUsableSamples):
		t.markFailed(marker)
		return recoveryFailed, item, fmt.Errorf("no usable samples: %w", restoreErr)
	case restoreErr != nil && !errors.Is(restoreErr, mp4pkg.ErrIndexTruncated):
		t.markFailed(marker)
		return recoveryFailed, item, fmt.Errorf("restore index: %w", restoreErr)
	}
	// ErrIndexTruncated 表示索引略微超前媒体（S3 用户态缓冲/崩溃截断），
	// 有效前缀已经恢复，继续产出文件。

	if err = writeRecoveredMedia(muxer, source, output, info.Mode().Perm()); err != nil {
		// 磁盘/IO 错误不是永久性的：保留 .recovering，下次启动重试。
		return recoveryFailed, item, err
	}

	if err = t.enqueue(item); err != nil {
		// 产物已 rename 但未成功入队：保留源暂存与 .recovering，下次重试。
		return recoveryFailed, item, err
	}

	// 只有产物已 rename 且已入队后，才允许删除源暂存文件。
	if source != output {
		if rmErr := os.Remove(source); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Warn("remove recovered source temp failed", "err", rmErr, "source", source)
		}
	}
	t.afterEnqueueSidecar(marker)
	t.Info("recovered recording from index",
		"sidecar", sidecar,
		"output", output,
		"samples", len(index.Records),
		"truncated", errors.Is(restoreErr, mp4pkg.ErrIndexTruncated))
	return recoveryRecovered, item, nil
}

func (t *IndexRecoveryTask) enqueue(item RecoveryItem) error {
	if t.EnqueueUpload != nil {
		return t.EnqueueUpload(item)
	}
	return mp4pkg.EnqueueRecoveryUploadWithOptions(mp4pkg.RecoveryUpload{
		Header:      item.Header,
		Records:     item.Records,
		MediaPath:   item.MediaPath,
		OutputPath:  item.OutputPath,
		SidecarPath: item.SidecarPath,
	}, mp4pkg.RecoveryUploadOptions{
		Resolver: t.ResolveUploadStorage,
		DB:       t.DB,
		Logger:   t.Logger,
	})
}

// afterEnqueueSidecar 处理「入队成功」后的 sidecar。
//
// 默认上传队列接管 sidecar 生命周期：上传成功或 pending 登记成功后才删除。
// 注入的 EnqueueUpload（测试/定制）没有该语义，保持原有「入队即清理」行为。
func (t *IndexRecoveryTask) afterEnqueueSidecar(sidecar string) {
	if t.EnqueueUpload != nil {
		t.removeSidecar(sidecar)
	}
}

func (t *IndexRecoveryTask) markRecovering(sidecar string) (string, error) {
	if strings.HasSuffix(sidecar, recoveringSuffix) {
		return sidecar, nil
	}
	marker := sidecar + recoveringSuffix
	if err := os.Rename(sidecar, marker); err != nil {
		return sidecar, fmt.Errorf("mark recovering: %w", err)
	}
	syncParentDir(sidecar)
	return marker, nil
}

func (t *IndexRecoveryTask) markFailed(sidecar string) {
	base := strings.TrimSuffix(sidecar, recoveringSuffix)
	failed := base + failedSuffix
	if err := os.Rename(sidecar, failed); err != nil {
		if !os.IsNotExist(err) {
			t.Warn("mark recovery failed", "err", err, "sidecar", sidecar, "target", failed)
		}
		return
	}
	if plain := strings.TrimSuffix(sidecar, recoveringSuffix); plain != sidecar {
		// 极端情况下 plain 与 .recovering 同时存在：只保留 .failed，
		// 避免下次扫描又处理一遍已判定失败的索引。
		_ = os.Remove(plain)
	}
	syncParentDir(sidecar)
	t.Warn("recording recovery failed permanently", "sidecar", failed)
}

func (t *IndexRecoveryTask) removeSidecar(sidecar string) {
	if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
		t.Warn("remove recovery sidecar failed", "err", err, "sidecar", sidecar)
	}
	if plain := strings.TrimSuffix(sidecar, recoveringSuffix); plain != sidecar {
		_ = os.Remove(plain)
	}
	syncParentDir(sidecar)
}

// writeRecoveredMedia 写 ftyp+moov+mdat 头与载荷到同目录临时文件，
// fsync 后原子 rename 到 output。source 永远不会被原地修改。
func writeRecoveredMedia(muxer *mp4pkg.Muxer, source, output string, mode os.FileMode) (err error) {
	dir := filepath.Dir(output)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(output)+".recovering-*")
	if err != nil {
		return fmt.Errorf("create recovery temp: %w", err)
	}
	tmpPath := tmp.Name()
	tmpOpen := true
	defer func() {
		if tmpOpen {
			_ = tmp.Close()
		}
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if err = muxer.WriteMoovOnly(tmp); err != nil {
		return fmt.Errorf("write recovered moov: %w", err)
	}
	start, size, ok := muxer.RestorePayloadRange()
	if !ok {
		return fmt.Errorf("recovered payload range unavailable")
	}
	src, openErr := os.Open(source)
	if openErr != nil {
		return fmt.Errorf("open source media: %w", openErr)
	}
	defer src.Close()
	if _, err = io.Copy(tmp, io.NewSectionReader(src, start, size)); err != nil {
		return fmt.Errorf("copy media payload: %w", err)
	}
	if mode != 0 {
		_ = tmp.Chmod(mode)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("fsync recovered media: %w", err)
	}
	if err = tmp.Close(); err != nil {
		tmpOpen = false
		return fmt.Errorf("close recovered media: %w", err)
	}
	tmpOpen = false
	if err = os.Rename(tmpPath, output); err != nil {
		return fmt.Errorf("rename recovered media: %w", err)
	}
	syncParentDir(output)
	return nil
}

func locateRecoveryMedia(recorded, sidecar string) (string, bool) {
	var candidates []string
	if recorded != "" {
		candidates = append(candidates, recorded)
	}
	if derived := mediaPathFromSidecar(sidecar); derived != "" && derived != recorded {
		candidates = append(candidates, derived)
	}
	if recorded != "" {
		if pending := storage.GetPendingDir(); pending != "" {
			candidates = append(candidates, filepath.Join(pending, filepath.Base(recorded)))
		}
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func mediaPathFromSidecar(sidecar string) string {
	path := strings.TrimSuffix(sidecar, recoveringSuffix)
	return strings.TrimSuffix(path, mp4pkg.IndexExtension)
}

// recoveryOutputPath 决定本地恢复产物路径：
//   - 对象存储暂存 s3writer_*.tmp → 同目录同名 .mp4（供上传，唯一性由 tmp 名保证）
//   - 本地 .mp4（崩溃残留）→ 原路径（临时文件写好后原子覆盖）
func recoveryOutputPath(mediaPath string) string {
	if mediaPath == "" {
		return ""
	}
	if strings.HasSuffix(mediaPath, ".tmp") {
		return strings.TrimSuffix(mediaPath, ".tmp") + ".mp4"
	}
	if isMP4Path(mediaPath) {
		return mediaPath
	}
	return mediaPath + ".mp4"
}

func isMP4Path(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".mp4")
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// isFreshlyWritten 判断给定文件中最新的 mtime 是否新于 MinAge 阈值。
// 用于区分「仍在录制的活跃文件」与「崩溃残留」；活跃文件跳过而不是定稿。
func (t *IndexRecoveryTask) isFreshlyWritten(paths ...string) (bool, time.Duration) {
	minAge := t.effectiveMinAge()
	if minAge <= 0 {
		return false, 0
	}
	var newest time.Time
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	if newest.IsZero() {
		return false, 0
	}
	age := time.Since(newest)
	return age < minAge, age
}

// isPermanentIndexError 判断索引读取错误是否属于确定性损坏。
// 解析类错误（坏魔数/非法头/头截断/JSON 失败）不会自行恢复；
// 其余错误（权限、IO、挂载点抖动）按可重试处理。
func isPermanentIndexError(err error) bool {
	return errors.Is(err, mp4pkg.ErrIndexCorrupt)
}

// hasTopLevelMoov 扫描顶层 box，判断文件是否已经包含 moov。
// 只用于识别“已正常收尾”的本地录像，避免用重建结果覆盖完好文件。
func hasTopLevelMoov(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()

	var header [16]byte
	for range 64 {
		if _, err = io.ReadFull(f, header[:8]); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		typ := string(header[4:8])
		if typ == "moov" {
			return true
		}
		extra := int64(0)
		if size == 1 {
			if _, err = io.ReadFull(f, header[8:16]); err != nil {
				return false
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			extra = 8
		} else if size == 0 {
			return false
		}
		if size < 8+extra {
			return false
		}
		if _, err = f.Seek(size-8-extra, io.SeekCurrent); err != nil {
			return false
		}
	}
	return false
}

func syncParentDir(path string) {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return
	}
	_ = dir.Sync()
	_ = dir.Close()
}
