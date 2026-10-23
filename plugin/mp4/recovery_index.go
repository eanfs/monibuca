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
	"sync/atomic"
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
	// Truncated 表示只恢复了有效前缀：索引略微超前媒体，或索引中段存在
	// 结构性损坏（Truncated/DroppedAt）。
	Truncated bool
	// DroppedTrackIDs 是索引头声明、但没有任何可用样本而被丢弃的轨道。
	// 非空时产物缺少这些轨道，DB 回写不得声明对应 codec。
	DroppedTrackIDs []uint16
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
	// MinAge 是**手动恢复**（API/CLI）的「仍在写入」判定阈值：sidecar/媒体
	// mtime 新于该值时跳过，避免把活跃录像的 sidecar 改名、媒体文件覆盖。
	// 生效值无条件夹紧为 max(MinAge, 5s, 2×索引落盘间隔)；显式传负数表示
	// 关闭该保护（仅测试/特殊场景）。配置默认 5m（spec 值）。
	//
	// 启动自动扫描**不使用**该值，而是固定使用短阈值 startupMinAge()
	// = max(5s, 2×索引落盘间隔)：崩溃后容器秒级重启时残留 age 只有数秒，
	// 若沿用 5m 会被全部跳过且没有第二次自动扫描（spec §2 要求无需人工介入）。
	// 5s 下限只用于覆盖跨进程重启瞬间仍在写的文件；本进程内的活跃 recorder
	// 由 pkg 的 activeIndexRegistry 权威判定，先于新鲜度闸门生效。
	MinAge time.Duration
	// RerunInterval 是**周期重扫**间隔：启动扫描之后每隔该时长自动再扫一轮，
	// 只处理残留 sidecar（重试启动时因上传后端未就绪/挂载点抖动等原因失败的
	// 文件），兑现「恢复无需人工介入」的承诺。0/负值关闭周期重扫。
	// 周期重扫只在 RunOnStart=true（自动恢复开启）时注册：onstart=false 承诺
	// 「不做自动扫描」，不能用另一个键绕过它。
	RerunInterval time.Duration
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

	// observer 是可选的单文件结果观察者（恢复 API 的进度与最近结果）。
	// 必须在任务启动前设置；回调可能来自并发 worker，实现必须线程安全。
	// runID 标识本次结果归属的手动运行；启动扫描为空，避免把扫描的逐文件
	// 结果计入排队中的手动 run。
	observer func(runID, file string, outcome string, err error, samples int, truncated bool)

	// runMu 串行化整个扫描：启动扫描与 API 手动触发不能同时处理同一文件。
	runMu sync.Mutex

	// executingRunID 是当前真正在执行的手动 run（已取得 runMu）。启动扫描为空。
	executingRunID atomic.Pointer[string]
}

// setExecutingRunID 标记/清除当前执行中的手动 run。
func (t *IndexRecoveryTask) setExecutingRunID(id string) {
	if id == "" {
		t.executingRunID.Store(nil)
		return
	}
	t.executingRunID.Store(&id)
}

func (t *IndexRecoveryTask) currentRunID() string {
	if id := t.executingRunID.Load(); id != nil {
		return *id
	}
	return ""
}

const (
	defaultRecoveryConcurrency = 2
	// defaultRecoveryMinAge 是「仍在写入」判定阈值的**下限**（floor），不是配置
	// 默认值。索引默认每秒落盘，2× 落盘间隔通常已足够区分活跃录像；取 5s 作为
	// 下限避免误判。配置默认值是 5m（spec），见 MP4Plugin.RecordConfig.Recovery。
	defaultRecoveryMinAge = 5 * time.Second
)

func (t *IndexRecoveryTask) Start() error {
	// 恢复扫描默认不再包含系统临时目录（全局可写，见 mediaTempDir）。S3 未
	// 显式配置 tempdir 时暂存文件仍落在 /tmp，崩溃残留将永远扫描不到——这是
	// 安全与可恢复性之间的取舍，必须让运维知道。Start 只执行一次，天然只记一条。
	if t.s3TempDirUnconfigured() {
		t.Warn("storage.s3.tempdir is not configured: object-storage crash leftovers in the system temp dir will not be auto-recovered",
			"hint", "set storage.s3.tempdir to a persistent directory and keep it in the scan set: leave record.recovery.dirs empty (tempdir is one of the defaults) or list the tempdir explicitly",
			"runbook", "docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md §2.2")
	}
	t.Info("index recovery task started",
		"concurrency", t.effectiveConcurrency(),
		"startupMinAge", t.startupMinAge(),
		"manualMinAge", t.effectiveMinAge(),
		"runOnStart", t.RunOnStart,
		"runDBBackfill", t.RunDBBackfill,
		"rerunInterval", t.RerunInterval,
		"dirs", t.Dirs)
	// 不能在 Start 里等待子任务（会阻塞事件循环）。run task 在自己的
	// goroutine 中执行，完成后自行记录日志。
	switch {
	case t.RunOnStart:
		t.AddTask(&recoveryRunTask{
			owner:  t,
			minAge: t.startupMinAge(),
			stats:  &RecoveryStats{},
			done:   make(chan struct{}),
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
	// 周期重扫：启动扫描之后每隔 reruninterval 自动再扫一轮，重试启动时因
	// 暂时性故障（上传后端未就绪/挂载点抖动/目录暂时不可读）失败的残留 sidecar。
	// Tick 在事件循环上执行，只做 TryLock + AddTask；真正的扫描在 run task 的
	// Go() 里跑，不会阻塞事件循环。
	if t.periodicRerunEnabled() {
		t.AddTask(&recoveryRerunTask{owner: t}, t.Logger)
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
		return t.finish(t.runRecovery(dirs, false, t.effectiveMinAge(), false))
	}

	stats := RecoveryStats{}
	done := make(chan struct{})
	child := t.AddTask(&recoveryRunTask{
		owner:  t,
		dirs:   dirs,
		minAge: t.effectiveMinAge(),
		stats:  &stats,
		done:   done,
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

// defaultRecoveryRerunInterval 是 RerunInterval 未配置时的兜底（仅用于
// GetTickInterval，正常路径由 record.recovery.reruninterval 的 default 注入）。
const defaultRecoveryRerunInterval = 10 * time.Minute

// periodicRerunEnabled 报告是否应注册周期重扫。
//
// 两个条件缺一不可：
//   - RerunInterval > 0：显式的关闭开关（0/负值 = 关闭）；
//   - RunOnStart：onstart=false 的语义是「启动时不做自动扫描」，周期扫描同样
//     属于自动恢复，不能用一个默认开启的新键绕过既有配置的意图。
func (t *IndexRecoveryTask) periodicRerunEnabled() bool {
	return t.RunOnStart && t.RerunInterval > 0
}

// recoveryRerunTask 周期触发重扫的 TickTask。
//
// Tick 在 owner 的事件循环上执行：只做 TryLock + AddTask，绝不调用 RunOnce
// （后者会 WaitStarted 并等待整轮扫描完成，阻塞整个 Work 的事件循环）。
type recoveryRerunTask struct {
	task.TickTask
	owner *IndexRecoveryTask
}

func (r *recoveryRerunTask) GetTickInterval() time.Duration {
	if r.owner.RerunInterval > 0 {
		return r.owner.RerunInterval
	}
	return defaultRecoveryRerunInterval
}

func (r *recoveryRerunTask) Tick(any) {
	owner := r.owner
	// runMu.TryLock 既判断「是否已有扫描在跑」（启动扫描/手动 run/上一轮周期
	// 重扫），又不会在事件循环里等待。取到后立刻释放：真正的锁由 run task 的
	// Go() 持有；这里只借用它做一次非阻塞的占用检测，避免 tick 堆积。
	if !owner.runMu.TryLock() {
		owner.Debug("periodic recovery scan skipped: another scan is still running")
		return
	}
	owner.runMu.Unlock()
	owner.AddTask(&recoveryRunTask{
		owner:    owner,
		minAge:   owner.startupMinAge(),
		stats:    &RecoveryStats{},
		done:     make(chan struct{}),
		periodic: true,
	}, owner.Logger)
}

// recoveryMinAgeFloor 是新鲜度阈值的下限：max(5s, 2×索引落盘间隔)。
// 2×flushinterval 保证活跃 sidecar 的最大落盘滞后不会超过阈值；
// 5s 用于覆盖跨进程重启瞬间仍在写入的文件。
func recoveryMinAgeFloor() time.Duration {
	floor := 2 * mp4pkg.GetIndexConfig().FlushInterval
	if floor < defaultRecoveryMinAge {
		floor = defaultRecoveryMinAge
	}
	return floor
}

// startupMinAge 返回**启动自动扫描**使用的新鲜度阈值。
//
// 启动扫描只跑一次（没有周期性重跑），必须保证崩溃后秒级重启的残留能被处理：
// 因此不采用配置的 minage（默认 5m），固定使用短阈值
// max(5s, 2×index.flushinterval)。显式配置负数仍表示关闭保护（测试/特殊场景）。
//
// 安全性：本进程内的活跃 recorder 由 pkg 的 activeIndexRegistry 权威判定，
// 先于该阈值生效；mtime 只兜底跨进程重启的瞬间。
func (t *IndexRecoveryTask) startupMinAge() time.Duration {
	if t.MinAge < 0 {
		return t.MinAge
	}
	return recoveryMinAgeFloor()
}

// effectiveMinAge 返回**手动恢复**（API/CLI）的「仍在写入」判定阈值，语义为：
//   - 负值：显式关闭保护（仅测试/特殊场景）；
//   - 其余：max(配置值, 5s, 2×索引落盘间隔) 无条件夹紧。
//
// 夹紧必须无条件生效：配置默认值 5m 并非 0，若只在「未配置」时夹紧，
// 运维把 record.index.flushinterval 调到 >2.5s 后，活跃 sidecar 的最大
// 落盘滞后会超过 5s 下限，活跃保护被削弱。
func (t *IndexRecoveryTask) effectiveMinAge() time.Duration {
	if t.MinAge < 0 {
		return t.MinAge
	}
	minAge := t.MinAge
	if floor := recoveryMinAgeFloor(); minAge < floor {
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
	// minAge 是本次运行使用的新鲜度阈值：启动扫描用 startupMinAge()，
	// 手动运行用 effectiveMinAge()。随运行实例传递，避免并发扫描间串值。
	minAge time.Duration
	stats  *RecoveryStats
	done   chan struct{}
	// backfillOnly 为 true 时只跑 DB 补录（onstart=false 但 AutoRecovery 开启）。
	backfillOnly bool
	// periodic 标记周期重扫：只重试残留 sidecar，不重复 DB 补录，无残留时不记
	// Info 汇总日志（每 10 分钟一条噪声没有价值）。
	periodic bool
}

func (r *recoveryRunTask) Go() error {
	defer close(r.done)
	r.owner.runMu.Lock()
	defer r.owner.runMu.Unlock()
	if r.backfillOnly {
		*r.stats = r.owner.runDBBackfill()
		return nil
	}
	*r.stats = r.owner.runRecovery(r.dirs, true, r.minAge, r.periodic)
	return nil
}

type recoveryOutcome int

const (
	recoveryFailed recoveryOutcome = iota
	recoveryRecovered
	recoverySkipped
)

func (o recoveryOutcome) String() string {
	switch o {
	case recoveryRecovered:
		return "recovered"
	case recoverySkipped:
		return "skipped"
	default:
		return "failed"
	}
}

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
	minAge    time.Duration
	wg        *sync.WaitGroup
	collector *recoveryCollector
}

func (w *recoveryWorkerTask) Go() error {
	defer w.wg.Done()
	for sidecar := range w.jobs {
		outcome, _, err := w.owner.recoverSidecar(sidecar, w.minAge)
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
// minAge 是本次运行的新鲜度阈值（启动扫描/手动运行取值不同）。
// periodic 表示本轮由周期重扫触发：跳过 DB 补录，且无残留时不记 Info 汇总。
func (t *IndexRecoveryTask) runRecovery(dirs []string, useTasks bool, minAge time.Duration, periodic bool) RecoveryStats {
	startedAt := time.Now()
	stats := RecoveryStats{}

	// 1. 先跑既有 DB 补录（旧 AutoRecovery 行为），再定稿索引。
	// 周期重扫只重试残留 sidecar：DB 补录是启动扫描（AutoRecovery）的语义，
	// 每 reruninterval 全表扫一次没有新信息，只会增加 DB 压力。
	if !periodic {
		stats.merge(t.runDBBackfill())
	}

	resolved, dirErrs := t.resolveDirs(dirs)
	stats.Errors = append(stats.Errors, dirErrs...)

	files, bareTmp, truncated, walkErrs := discoverSidecars(resolved, recoveryVisitedEntryLimit, recoveryScanEntryLimit)
	stats.BareTmp = bareTmp
	stats.Errors = append(stats.Errors, walkErrs...)
	stats.Scanned += len(files)
	if truncated != "" {
		// 截断必须显式暴露：运维需要知道本轮有目录没扫完（剩余 sidecar 要等
		// 下一轮，或先缩小 record.recovery.dirs）。limit 必须是本次真正命中
		// 的那个预算，否则现场会去调错配置。
		t.Warn("recovery scan truncated at entry limit: remaining entries were not scanned",
			"reason", string(truncated),
			"limit", truncated.limit(),
			"visitedLimit", recoveryVisitedEntryLimit,
			"collectedLimit", recoveryScanEntryLimit,
			"dirs", resolved)
	}

	if bareTmp > 0 {
		// Task 7 负责无索引 tmp 的反向重建；此处只统计，不删除、不移动。
		t.Info("recovery scan found bare media tmp without sidecar (skipped)", "count", bareTmp)
	}

	if len(files) > 0 {
		if useTasks && t.effectiveConcurrency() > 1 && len(files) > 1 {
			concurrent := t.processWithWorkers(files, minAge)
			stats.merge(concurrent)
		} else {
			for _, sidecar := range files {
				outcome, _, err := t.recoverSidecar(sidecar, minAge)
				stats.recordOutcome(sidecar, outcome, err)
			}
		}
	}

	// 完成日志只记 errors=N，明细必须逐条可见：目录遍历失败、DB 补录失败
	// 等错误没有其它出口，只给数字的话现场无法定位是哪个文件/目录出了问题。
	t.logRecoveryErrors(stats.Errors)

	// 周期重扫无残留时保持安静：每 reruninterval 一条 scanned=0 的 Info 只是
	// 噪声。错误仍由上面的 Error 逐条输出，截断仍有独立 Warn，不会漏报。
	if !periodic || stats.Scanned > 0 || stats.BareTmp > 0 || len(stats.Errors) > 0 {
		t.Info("index recovery completed",
			"scanned", stats.Scanned,
			"recovered", stats.Recovered,
			"failed", stats.Failed,
			"skipped", stats.Skipped,
			"bareTmp", stats.BareTmp,
			"errors", len(stats.Errors),
			"elapsedMs", time.Since(startedAt).Milliseconds())
	}
	return stats
}

// recoveryErrorLogLimit 是逐条错误日志的上限：目录被大量坏 sidecar 塞满时
// 不能把启动日志刷爆，超出部分只输出总条数（日志检索仍能拿到 errors 计数）。
const recoveryErrorLogLimit = 20

// logRecoveryErrors 逐条输出本轮扫描收集到的错误（Error 级）。
func (t *IndexRecoveryTask) logRecoveryErrors(errs []error) {
	for i, err := range errs {
		if i >= recoveryErrorLogLimit {
			t.Error("index recovery errors truncated",
				"logged", recoveryErrorLogLimit, "total", len(errs))
			return
		}
		t.Error("index recovery error", "error", err, "index", i+1, "total", len(errs))
	}
}

func (t *IndexRecoveryTask) processWithWorkers(files []string, minAge time.Duration) RecoveryStats {
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
			minAge:    minAge,
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
// 空目录一律跳过：mediaTempDir 在未显式配置时为空的语义必须体现在这里。
func (t *IndexRecoveryTask) defaultDirs() []string {
	dirs := make([]string, 0, 4)
	add := func(dir string) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		dirs = append(dirs, dir)
	}
	add(t.mediaTempDir())
	if pending := storage.GetPendingDir(); pending != "" {
		add(pending)
	}
	if t.plugin != nil {
		if common := t.plugin.GetCommonConf(); common != nil {
			for _, conf := range common.OnPub.Record {
				if conf.FilePath != "" {
					add(filepath.Dir(conf.FilePath))
				}
			}
		}
		if t.plugin.EventRecordFilePath != "" {
			add(filepath.Dir(t.plugin.EventRecordFilePath))
		}
	}
	return dirs
}

// s3TempDirUnconfigured 报告「S3 已配置但未显式配置 tempdir」：
// 此时暂存文件仍写在系统临时目录（pkg/storage/s3.go 的 effectiveTempDir），
// 而默认扫描目录已不再包含它，崩溃残留无法被自动恢复。
func (t *IndexRecoveryTask) s3TempDirUnconfigured() bool {
	if t.plugin == nil || t.plugin.Server == nil {
		return false
	}
	if _, ok := t.plugin.Server.ServerConfig.Storage[string(storage.StorageTypeS3)]; !ok {
		return false
	}
	return strings.TrimSpace(t.mediaTempDir()) == ""
}

// mediaTempDir 返回对象存储暂存文件目录（storage.s3.tempdir），未配置时返回空。
//
// 绝不回落到 os.TempDir()：系统临时目录通常全局可写，任何本机进程都能在其中
// 投放 sidecar，把恢复扫描变成「攻击者指定的文件/对象键」的入口。需要在
// 临时目录恢复时，必须在 storage.s3.tempdir 中显式配置。
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
	return ""
}

// sidecarScanTruncated 说明 discoverSidecars 为何提前结束；空字符串表示未截断。
type sidecarScanTruncated string

const (
	// sidecarScanTruncatedVisited：遍历条目预算（目录 + 普通文件）耗尽。
	sidecarScanTruncatedVisited sidecarScanTruncated = "visited"
	// sidecarScanTruncatedCollected：已收集的 sidecar 数达到上限。
	sidecarScanTruncatedCollected sidecarScanTruncated = "collected"
)

// limit 返回本次截断命中的上限值，供日志指明「是哪个预算」。
func (s sidecarScanTruncated) limit() int {
	switch s {
	case sidecarScanTruncatedVisited:
		return recoveryVisitedEntryLimit
	case sidecarScanTruncatedCollected:
		return recoveryScanEntryLimit
	default:
		return 0
	}
}

// discoverSidecars 递归收集 *.m7sidx 与 *.m7sidx.recovering。
// 同一媒体同时存在两种标记时优先 .recovering；无 sidecar 的 s3writer_*.tmp
// 只计数（Task 7 处理），不进入恢复队列。
//
// 两个预算必须分开，语义不同：
//   - visitedLimit 限制**遍历条目总数**（含目录与普通文件）。普通文件只计数，
//     WalkDir 不会 Stat，内存成本可忽略；因此它取得很大，避免「旧文件多」把
//     字典序靠后的最新录像挤出扫描范围（默认扫描目录包含整个录像根目录，
//     文件名带时间戳前缀，被截断的恰好是最新记录，且每轮都稳定漏掉同一批）。
//   - collectLimit 限制**收集到的 sidecar 数**，这才是真正的内存风险所在。
//
// 任一预算耗尽即 truncated 非空，剩余目录不再访问。
func discoverSidecars(dirs []string, visitedLimit, collectLimit int) (files []string, bareTmp int, truncated sidecarScanTruncated, errs []error) {
	preferred := make(map[string]string) // base(.m7sidx) -> 实际路径
	bare := make(map[string]bool)
	visited := 0

	for _, dir := range dirs {
		if truncated != "" {
			break
		}
		walkErr := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				errs = append(errs, fmt.Errorf("walk %s: %w", path, err))
				return nil
			}
			visited++
			if visited > visitedLimit {
				truncated = sidecarScanTruncatedVisited
				return filepath.SkipAll
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			switch {
			case strings.HasSuffix(name, mp4pkg.IndexExtension+recoveringSuffix):
				key := strings.TrimSuffix(path, recoveringSuffix)
				if _, ok := preferred[key]; ok {
					// 同一媒体的 .recovering 优先于普通 .m7sidx，不新增条目。
					preferred[key] = path
					return nil
				}
				if len(preferred) >= collectLimit {
					truncated = sidecarScanTruncatedCollected
					return filepath.SkipAll
				}
				preferred[key] = path
			case strings.HasSuffix(name, mp4pkg.IndexExtension):
				if _, ok := preferred[path]; ok {
					return nil
				}
				if len(preferred) >= collectLimit {
					truncated = sidecarScanTruncatedCollected
					return filepath.SkipAll
				}
				preferred[path] = path
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
	return files, len(bare), truncated, errs
}

func hasIndexSidecar(mediaPath string) bool {
	index := mediaPath + mp4pkg.IndexExtension
	if fileExists(index) || fileExists(index+recoveringSuffix) {
		return true
	}
	return false
}

// recoverSidecar 定稿单个 sidecar，并输出一条字段固定的「恢复结果」日志。
//
// 固定字段（file/elapsedMs，失败时额外 error）是现场日志检索契约的一部分。
// 单文件结果统一在这里输出，避免各分支各写一条、字段互不一致。
func (t *IndexRecoveryTask) recoverSidecar(sidecar string, minAge time.Duration) (recoveryOutcome, RecoveryItem, error) {
	startedAt := time.Now()
	outcome, item, err := t.recoverSidecarFile(sidecar, minAge)
	t.logRecoveryResult(sidecar, item, outcome, err, time.Since(startedAt))
	return outcome, item, err
}

// logRecoveryResult 输出「恢复扫描」中的单个文件结果。
func (t *IndexRecoveryTask) logRecoveryResult(sidecar string, item RecoveryItem,
	outcome recoveryOutcome, err error, elapsed time.Duration) {
	file := item.OutputPath
	if file == "" {
		file = mediaPathFromSidecar(sidecar)
	}
	elapsedMs := elapsed.Milliseconds()
	if t.observer != nil {
		t.observer(t.currentRunID(), file, outcome.String(), err, len(item.Records), item.Truncated)
	}
	switch outcome {
	case recoveryRecovered:
		attrs := []any{
			"file", file,
			"outcome", "recovered",
			"elapsedMs", elapsedMs,
			"sidecar", sidecar,
			"samples", len(item.Records),
			"truncated", item.Truncated,
		}
		if len(item.DroppedTrackIDs) > 0 {
			attrs = append(attrs, "droppedTracks", item.DroppedTrackIDs)
		}
		t.Info("recovery result", attrs...)
	case recoverySkipped:
		t.Info("recovery result",
			"file", file,
			"outcome", "skipped",
			"elapsedMs", elapsedMs,
			"sidecar", sidecar)
	default:
		t.Error("recovery result",
			"file", file,
			"outcome", "failed",
			"elapsedMs", elapsedMs,
			"sidecar", sidecar,
			"error", err)
	}
}

// recoverSidecarFile 定稿单个 sidecar。任何返回错误的分支都保留可重试状态，
// 除非错误被明确判定为永久性（索引损坏/无可用样本 → .failed）。
func (t *IndexRecoveryTask) recoverSidecarFile(sidecar string, minAge time.Duration) (recoveryOutcome, RecoveryItem, error) {
	// 进程内活跃 recorder 是权威判据：mtime 可能因拉流重连退避/低帧率而变旧，
	// 单靠新鲜度会把活跃录制判死并覆盖其媒体。必须在任何 rename/读取之前拦截。
	if t.isActiveRecording(sidecar) {
		t.Info("skip recovery of active recording (process registry)",
			"sidecar", sidecar, "media", mediaPathFromSidecar(sidecar))
		return recoverySkipped, RecoveryItem{}, nil
	}

	// 实际读写的路径由索引内容决定，必须独立于请求参数再做一次白名单校验，
	// 否则白名单内的 sidecar 可以把读写引到白名单之外（含符号链接逃逸）。
	// guard 同时确认 sidecar 自身不是符号链接。
	guard, err := t.newRecoveryPathGuard(sidecar)
	if err != nil {
		return recoveryFailed, RecoveryItem{MediaPath: mediaPathFromSidecar(sidecar)}, err
	}
	index, err := mp4pkg.ReadIndex(sidecar)
	if err != nil {
		// 活跃录制中的 sidecar 刚创建时可能短暂不可读。先看 mtime：
		// 仍在写入的文件不判失败，留给下次 RunOnce。
		if fresh, age := t.isFreshlyWritten(minAge, sidecar, mediaPathFromSidecar(sidecar)); fresh {
			t.Info("skip recovery of sidecar still being written (read failed)",
				"sidecar", sidecar, "age", age, "minAge", minAge, "err", err)
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

	// header 里的媒体路径同样可能属于活跃 recorder（历史 sidecar 的 sidecar
	// 路径未被登记，但 recorder 登记的是媒体路径）。再查一次，避免漏判。
	if mp4pkg.IsActiveRecordingPath(index.MediaPath) {
		t.Info("skip recovery of active recording (media registry)",
			"sidecar", sidecar, "media", index.MediaPath)
		return recoverySkipped, RecoveryItem{}, nil
	}

	recorded := index.MediaPath
	if recorded == "" {
		recorded = mediaPathFromSidecar(sidecar)
	}
	// 候选逐个过白名单：最后一条 RENAME 可能指向白名单外的 trailer 临时文件
	// （Task 6 预写的 RENAME），但 sidecar 派生的原始媒体仍在白名单内。
	// 取「第一个存在的候选」再整体拒绝会把这个可恢复窗口永久判死。
	source, found, locationErr := locateRecoveryMedia(recorded, sidecar, guard)

	item := RecoveryItem{
		Header:      index.Header,
		Records:     index.Records,
		SidecarPath: sidecar,
	}

	// 存在候选、但全部落在白名单之外：保持拒绝恢复语义，绝不读写白名单外的
	// 路径（后续不再 rename/remove 任何文件）。
	if locationErr != nil {
		return recoveryFailed, item, locationErr
	}
	output, err := guard.allowOutput(recoveryOutputPath(recorded), "recovery output")
	if err != nil && found {
		// 按 recorded 推导的产物同样越界（recoveryOutputPath 只换后缀/同目录）：
		// 改用已通过白名单的 source 推导产物，恢复走实际定位到的媒体。
		output, err = guard.allowOutput(recoveryOutputPath(source), "recovery output")
	}
	if err != nil {
		return recoveryFailed, item, err
	}
	item.MediaPath = source
	item.OutputPath = output

	// sidecar 内容是不可信输入：FilePath（以及 FilePath 为空时的 StorageKey）
	// 会被写进 record_streams.file_path（下载接口可能直接 os.Open 它），
	// 也会被当作上传对象键。必须与本次读写的媒体路径绑定——该媒体路径刚刚
	// 通过了白名单 guard，因此绑定等价于「逻辑路径必须落在受信目录内」。
	// 必须在任何 rename/覆盖/入队之前拦截。
	bindingHeader := index.Header
	bindingMedia := recorded
	if source != "" {
		// source 已通过 guard，是本次真正读写的媒体路径。
		bindingMedia = source
	}
	if bindingMedia != "" {
		bindingHeader.MediaPath = bindingMedia
	}
	if err = mp4pkg.ValidateIndexHeaderPaths(bindingHeader); err != nil {
		t.Error("reject recovery: index header filePath is not bound to mediaPath",
			"sidecar", sidecar,
			"filePath", index.Header.FilePath,
			"storageKey", index.Header.StorageKey,
			"mediaPath", bindingHeader.MediaPath,
			"err", err)
		return recoveryFailed, item, err
	}
	// 下游（上传/DB 回写）必须拿到与校验一致的 MediaPath，而不是 sidecar 里的
	// 原值：MediaPath 可能被 RENAME 扩展记录覆盖，或与实际定位到的 source 不同。
	// 校验通过后统一成同一份数据，避免「校验用 A、写库用 B」。
	item.Header = bindingHeader

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

	// C2：跳过仍在写入的文件。活跃 recorder 的 sidecar 同样未提交、与崩溃残留
	// 不可区分；若此时 markRecovering 并重建，会 rename 走活跃 sidecar、
	// 覆盖活跃媒体文件。mtime 新鲜即视为活跃，计入 Skipped。
	//
	// 这道闸门必须先于 C1：本地实时录制的 trailer INSERT_RANGE / moov 前移
	// 窗口内文件已有顶层 moov 但仍在被写，此时 C1 会误判「已收尾」并提前
	// 入队、删掉活跃 sidecar（Task 6 落地后还会上传半成品）。先做新鲜度判定
	// 才能同时保护两条分支。
	if fresh, age := t.isFreshlyWritten(minAge, source, sidecar); fresh {
		t.Info("skip recovery of file still being written",
			"sidecar", sidecar, "media", source, "age", age, "minAge", minAge)
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
	//
	// 必须同时要求 source == output：locateRecoveryMedia 可能定位到与
	// header.MediaPath 不同的候选（RENAME 目标尚未落盘的窗口），此时 output 由
	// recorded 推导，是一个从未被校验、也未创建过的路径。直接入队只会让上传
	// 任务永远等不到文件（fail-safe 但这条录像永远不会完成）。
	if source == output && index.Committed && isMP4Path(source) && hasTopLevelMoov(source) {
		if err := t.enqueue(item); err != nil {
			return recoveryFailed, item, err
		}
		t.afterEnqueueSidecar(sidecar)
		return recoverySkipped, item, nil
	}

	info, err := os.Stat(source)
	if err != nil {
		// 保留 sidecar，下次重试；Stat 失败多半是 IO/挂载问题。
		t.Warn("stat recovery media failed, will retry on next run", "err", err, "media", source)
		return recoveryFailed, item, fmt.Errorf("stat media: %w", err)
	}

	// INSERT_RANGE 崩溃窗口：trailer 用 fallocate 把 mdat 后移后、索引还没按
	// 新坐标重写时进程被杀。旧偏移仍落在文件长度内，RestoreFromIndex 不会
	// 报 ErrIndexTruncated，会把 moov 字节当载荷写出「结构合法但内容全错」
	// 的 MP4 并报告成功。判定必须确定性：状态不确定就拒绝恢复、保留 sidecar
	// （不 markFailed），交给人工处理。
	shift, shiftErr := index.ResolveOffsetShift(info.Size())
	if shiftErr != nil {
		t.Error("reject recovery: index offset shift state uncertain",
			"sidecar", sidecar, "media", source, "mediaSize", info.Size(),
			"shiftState", int(index.OffsetShift),
			"preLen", index.ShiftPreLen, "insertLen", index.ShiftInsertLen,
			"err", shiftErr)
		return recoveryFailed, item, shiftErr
	}
	records := index.RecordsWithOffsetShift(shift)
	if shift != 0 {
		t.Info("applying index offset shift for interrupted INSERT_RANGE",
			"sidecar", sidecar, "media", source, "shift", shift)
		// 上传/入库链路必须拿到与产物一致的坐标。
		item.Records = records
	}

	marker, err := t.markRecovering(sidecar)
	if err != nil {
		return recoveryFailed, item, err
	}
	// 入队时 sidecar 已改名为 .recovering：上传任务成功后按该路径提交式删除。
	item.SidecarPath = marker

	muxer := mp4pkg.NewMuxer(0)
	report, restoreErr := muxer.RestoreFromIndex(index.Header, records, info.Size())
	if len(report.DamagedTrackIDs) > 0 {
		// stsd 结构性损坏不会靠重试修复：下面的永久失败分支会 markFailed。
		t.Error("reject recovery: sample description is structurally corrupt",
			"sidecar", sidecar,
			"media", source,
			"damagedTracks", report.DamagedTrackIDs,
			"err", restoreErr)
	}
	switch {
	case errors.Is(restoreErr, mp4pkg.ErrNoUsableSamples):
		// 媒体长度为 0（文件仍在创建/暂存中断）不是永久失败：保留 sidecar
		// 等下次运行重试；只有确实存在非空媒体时才落 .failed。
		if info.Size() <= 0 {
			t.Warn("no usable samples in empty media file; keeping sidecar for a later run",
				"sidecar", sidecar, "media", source, "err", restoreErr)
			return recoveryFailed, item, fmt.Errorf("no usable samples: %w", restoreErr)
		}
		t.markFailed(marker)
		return recoveryFailed, item, fmt.Errorf("no usable samples: %w", restoreErr)
	case restoreErr != nil && !errors.Is(restoreErr, mp4pkg.ErrIndexTruncated):
		t.markFailed(marker)
		return recoveryFailed, item, fmt.Errorf("restore index: %w", restoreErr)
	}
	// ErrIndexTruncated 表示索引略微超前媒体（S3 用户态缓冲/崩溃截断），
	// 有效前缀已经恢复，继续产出文件。IndexFile.Truncated（索引中段结构性
	// 损坏）同样只丢了尾部，合并报告给日志与恢复 API。
	item.Truncated = report.Truncated || index.Truncated ||
		errors.Is(restoreErr, mp4pkg.ErrIndexTruncated)
	item.DroppedTrackIDs = report.DroppedTrackIDs
	if index.Truncated {
		t.Warn("index is structurally damaged; recovered the usable prefix only",
			"sidecar", sidecar,
			"media", source,
			"droppedAt", index.DroppedAt,
			"records", len(index.Records))
	}
	if len(report.DroppedTrackIDs) > 0 {
		t.Warn("recovery dropped declared tracks with no usable samples",
			"outcome", "recovered",
			"droppedTracks", report.DroppedTrackIDs,
			"sidecar", sidecar,
			"media", source)
	}

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
	return recoveryRecovered, item, nil
}

func (t *IndexRecoveryTask) enqueue(item RecoveryItem) error {
	if t.EnqueueUpload != nil {
		return t.EnqueueUpload(item)
	}
	return mp4pkg.EnqueueRecoveryUploadWithOptions(mp4pkg.RecoveryUpload{
		Header:          item.Header,
		Records:         item.Records,
		MediaPath:       item.MediaPath,
		OutputPath:      item.OutputPath,
		SidecarPath:     item.SidecarPath,
		DroppedTrackIDs: item.DroppedTrackIDs,
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

// recoveryMediaCandidates 按优先级列出候选媒体路径：
// 索引记录的最后一条 RENAME → sidecar 派生路径 → pending 暂存目录。
func recoveryMediaCandidates(recorded, sidecar string) []string {
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
	return candidates
}

// locateRecoveryMedia 定位本次恢复真正要读的源媒体，候选逐个通过 guard 校验。
//
// 只按「存在」挑第一个候选是不够的：Task 6 会在 FinalizeFromTemp 之前预写一条
// 指向 trailer 临时文件（os.CreateTemp("", "*.mp4")）的 RENAME，配置了非默认
// storage.s3.tempdir（或本地录像目录不在 /tmp）时该路径落在恢复白名单之外，
// 而 sidecar 派生的原始媒体仍在白名单内。旧实现命中越界候选后直接拒绝，等于把
// 这个可恢复窗口永久判死。越界/符号链接候选必须跳过并继续尝试下一个。
//
// 返回：
//   - (path, true, nil)：命中且通过白名单（path 为解析符号链接后的 canon）；
//   - ("", false, nil)：没有任何候选存在，可由后续运行重试；
//   - ("", false, err)：存在候选但全部被白名单拒绝（errRecoveryPathDenied），
//     调用方必须继续拒绝恢复。
func locateRecoveryMedia(recorded, sidecar string, guard *recoveryPathGuard) (string, bool, error) {
	var denied error
	for _, candidate := range recoveryMediaCandidates(recorded, sidecar) {
		if !fileExists(candidate) {
			continue
		}
		allowed, err := guard.allowExisting(candidate, "recovery media")
		if err != nil {
			if denied == nil {
				denied = err
			}
			continue
		}
		return allowed, true, nil
	}
	return "", false, denied
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

// isActiveRecording 报告 sidecar 或其推导出的媒体路径是否被本进程活跃 recorder 持有。
func (t *IndexRecoveryTask) isActiveRecording(sidecar string) bool {
	return mp4pkg.IsActiveRecordingPath(sidecar, mediaPathFromSidecar(sidecar))
}

// isFreshlyWritten 判断给定文件中最新的 mtime 是否新于 minAge 阈值。
// minAge 由调用方按运行类型传入（启动扫描 startupMinAge / 手动 effectiveMinAge），
// 用于区分「仍在录制的活跃文件」与「崩溃残留」；活跃文件跳过而不是定稿。
func (t *IndexRecoveryTask) isFreshlyWritten(minAge time.Duration, paths ...string) (bool, time.Duration) {
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

// 顶层 / moov 子 box 的扫描上限：只用于防止损坏文件把「判是否收尾」拖成
// O(文件大小/8) 次读，合法 mp4 远低于这些值。
const (
	maxTopLevelBoxScan  = 1024
	maxMoovChildBoxScan = 1024
)

// hasTopLevelMoov 扫描顶层 box，判断文件是否已经包含**完整**的 moov。
// 只用于识别“已正常收尾”的本地录像，避免用重建结果覆盖完好文件。
//
// 只认 box 头是不够的：trailer 把 moov 写到文件头（或尾部）的中途掉电时，
// 4 字节 size 与 type 可能已经落盘而 moov 内容尚未写完。旧实现读到 "moov"
// 头就返回 true，C1 分支会把它当成品入队并删掉 sidecar，恢复侧从此再也不会
// 重建这份录像。因此这里额外要求：moov 的声明尺寸完整落在文件内，且 moov
// 内含 mvhd（合法 moov 的第一个子 box）。
func hasTopLevelMoov(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	fileSize := info.Size()

	var header [16]byte
	// 顶层 box 数量有界：合法 mp4 不超过十几个。上限只防「大量 8 字节 box
	// 让扫描退化成 O(文件大小/8) 次读」的损坏文件。
	for pos, seen := int64(0), 0; pos+8 <= fileSize && seen < maxTopLevelBoxScan; seen++ {
		if _, err = f.ReadAt(header[:8], pos); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		typ := string(header[4:8])
		extra := int64(0)
		if size == 1 {
			if pos+16 > fileSize {
				return false
			}
			if _, err = f.ReadAt(header[8:16], pos+8); err != nil {
				return false
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			extra = 8
		} else if size == 0 {
			return false
		}
		// 声明尺寸小于最小 box 或越过文件尾：该 box 没有完整落盘。
		if size < 8+extra || pos > fileSize-size {
			return false
		}
		if typ == "moov" {
			return moovContainsMvhd(f, pos+8+extra, size-8-extra)
		}
		pos += size
	}
	return false
}

// moovContainsMvhd 扫描 moov 的子 box，确认存在完整落在 moov 内的 mvhd。
// 任一子 box 结构非法即判为「moov 未写完」，不再继续猜测。
func moovContainsMvhd(f io.ReaderAt, start, length int64) bool {
	var header [16]byte
	// 合法 moov 的子 box 数量是个位数；上限只防损坏文件把扫描拖长。
	for pos, seen := int64(0), 0; pos+8 <= length && seen < maxMoovChildBoxScan; seen++ {
		if _, err := f.ReadAt(header[:8], start+pos); err != nil {
			return false
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		extra := int64(0)
		if size == 1 {
			if pos+16 > length {
				return false
			}
			if _, err := f.ReadAt(header[8:16], start+pos+8); err != nil {
				return false
			}
			size = int64(binary.BigEndian.Uint64(header[8:16]))
			extra = 8
		}
		if size < 8+extra || pos > length-size {
			return false
		}
		if string(header[4:8]) == "mvhd" {
			return true
		}
		pos += size
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
