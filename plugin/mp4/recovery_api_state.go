package plugin_mp4

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	task "github.com/eanfs/gotask"
)

// 恢复 API 的运行状态机：运行登记/幂等判定、进度观测、历史淘汰。
// 所有可变字段由 recoveryAPIState.mu 保护；RecoveryStats 由 Task 5 产出。

// recoveryRunState 是一次手动恢复运行的状态。字段由 recoveryAPIState.mu 保护。
type recoveryRunState struct {
	id         string
	key        string
	status     string
	dirs       []string
	file       string
	startedAt  time.Time
	finishedAt time.Time
	stats      RecoveryStats
	err        string
}

// recoveryAPIState 保存运行历史与最近恢复结果。零值可直接使用。
type recoveryAPIState struct {
	mu      sync.Mutex
	runs    map[string]*recoveryRunState
	order   []string
	current *recoveryRunState
	recent  []recoveryRecentEntry
	seq     uint64
}

// errRecoveryTaskStopped 用于 owner 已停止但 StopReason 为空（无 cause）的兜底，
// 避免把 nil 拼进错误链。
var errRecoveryTaskStopped = errors.New("recovery task stopped")

// recoveryAPIJob 在任务系统中执行一次 API 触发的手动恢复。
type recoveryAPIJob struct {
	task.Task
	owner *IndexRecoveryTask
	state *recoveryAPIState
	run   *recoveryRunState
}

func (t *recoveryAPIJob) Go() error {
	stats, runErr := t.owner.runRequest(t.run.id, t.run.dirs, t.run.file)
	t.state.finishRun(t.run, stats, runErr)
	return nil
}

// recoveryStartWatcher 是短命任务：在后台等待本次 run 的子任务真正启动。
//
// 为什么不在 handler 里等：gotask 的 util.Promise 只有阻塞 Await()，没有
// context 感知的等待；事件循环被阻塞（或任务长期排队）时 WaitStarted 可能
// 永不返回，HTTP 连接会一直挂住。watcher 把等待搬进任务系统：启动失败时
// 回填 run 状态并记 Error，调用方通过 GET /status/{id} 观察。
//
// 注意：如需注册 OnStart 必须在 AddTask **之前**（本仓已知陷阱）；本实现
// 用 Go()，不依赖 OnStart。
type recoveryStartWatcher struct {
	task.Task
	// waitStarted 等待本次 run 的子任务启动。生产路径传 child.WaitStarted；
	// 抽成字段只为测试注入启动失败。
	waitStarted func() error
	state       *recoveryAPIState
	run         *recoveryRunState
	key         string
}

func (w *recoveryStartWatcher) GetKey() string { return w.key }

func (w *recoveryStartWatcher) Go() error {
	// 等待的是本次 run 自己的子任务，属于该 run 的启动生命周期，
	// 不是在 handler 或无关任务里直接驱动别的 task。
	if err := w.waitStarted(); err != nil {
		w.state.failStart(w.run, err)
		w.Error("recovery run failed to start", "runId", w.run.id, "error", err)
	}
	return task.ErrTaskComplete
}

// runRequest 串行执行一次手动恢复。dirs 为空表示使用配置/默认目录；
// file 非空表示只处理单个 sidecar（优先于 dirs）。
// 新鲜度（record.recovery.minage）与活跃录制保护由 recoverSidecar 内部的
// isFreshlyWritten 统一裁决，手动触发不会绕过。
//
// runID 只在取得 runMu（即真正开始执行）后发布给 observer：run 登记后可能
// 长时间阻塞在在途的启动扫描后面，期间扫描的逐文件结果绝不能被计入它。
func (t *IndexRecoveryTask) runRequest(runID string, dirs []string, file string) (RecoveryStats, error) {
	t.runMu.Lock()
	defer t.runMu.Unlock()
	t.setExecutingRunID(runID)
	defer t.setExecutingRunID("")
	// 手动 run 使用配置的 minage（默认 5m）；启动自动扫描走短阈值，
	// 由 recoveryRunTask.minAge 单独传入。
	minAge := t.effectiveMinAge()
	if file != "" {
		stats := RecoveryStats{Scanned: 1}
		outcome, _, err := t.recoverSidecar(file, minAge)
		stats.recordOutcome(file, outcome, err)
		return t.finish(stats)
	}
	return t.finish(t.runRecovery(dirs, true, minAge))
}

func (p *MP4Plugin) recoveryStateStore() *recoveryAPIState {
	return &p.recoveryState
}

// observeRecovery 是 IndexRecoveryTask.observer 的实现。
func (p *MP4Plugin) observeRecovery(runID, file, outcome string, err error, samples int, truncated bool) {
	p.recoveryStateStore().observe(runID, file, outcome, err, samples, truncated)
}

func (s *recoveryAPIState) ensureRunsLocked() {
	if s.runs == nil {
		s.runs = make(map[string]*recoveryRunState)
	}
}

// startRun 登记一次运行。返回 (id, deduplicated, conflict, error)。
//   - 相同目标已在运行 → 返回同一 id，deduplicated=true（HTTP 200）；
//   - 不同目标正在运行 → conflict=true，id 为在途运行 id（HTTP 409）。
//
// 登记成功后立即返回，不在调用方（HTTP handler）里等待任务启动：启动结果由
// recoveryStartWatcher 在后台回填。只有 owner 在登记瞬间已停止（两个 AddTask
// 都被拒、watcher 也永远不会启动）时才同步返回错误。
func (s *recoveryAPIState) startRun(owner *IndexRecoveryTask, dirs []string, file string) (string, bool, bool, error) {
	key := recoveryRunKey(dirs, file)

	s.mu.Lock()
	s.ensureRunsLocked()
	if s.current != nil && s.current.status == recoveryRunRunning {
		id := s.current.id
		if s.current.key == key {
			s.mu.Unlock()
			return id, true, false, nil
		}
		s.mu.Unlock()
		return id, false, true, nil
	}

	s.seq++
	id := fmt.Sprintf("rec-%d-%d", time.Now().UnixNano(), s.seq)
	run := &recoveryRunState{
		id:        id,
		key:       key,
		status:    recoveryRunRunning,
		dirs:      append([]string(nil), dirs...),
		file:      file,
		startedAt: time.Now(),
	}
	s.runs[id] = run
	s.order = append(s.order, id)
	s.current = run
	s.evictLocked()
	s.mu.Unlock()

	child := owner.AddTask(&recoveryAPIJob{owner: owner, state: s, run: run}, owner.Logger)
	watcher := &recoveryStartWatcher{waitStarted: child.WaitStarted, state: s, run: run}
	// watcher 必须有唯一 key：默认 key 是任务 ID，这里用自身指针，语义明确且
	// 不会与同批其它 watcher 冲突。
	watcher.key = fmt.Sprintf("recovery-start-watch:%p", watcher)
	owner.AddTask(watcher, owner.Logger)

	// 两个 AddTask 都只是入队，不会阻塞；若 owner 在登记瞬间已停止，两次
	// AddTask 都会被立即拒绝，watcher 永远不会运行、没人能回填状态。
	// 这是唯一的同步失败路径（正常运行时 owner 只在进程退出时停止）。
	if owner.IsStopped() {
		reason := owner.StopReason()
		if reason == nil {
			reason = errRecoveryTaskStopped
		}
		s.failStart(run, reason)
		return id, false, false, fmt.Errorf("start recovery run: %w", reason)
	}
	return id, false, false, nil
}

// failStart 记录「任务未能启动」的最终状态。
//
// 与 finishRun 分开：启动失败时恢复流程从未执行，没有任何逐文件结果，
// 不能与「执行后失败」共用同一判定（finishRun 还会覆盖 stats）。
// 若运行已被 finishRun 收尾则保持原样。
func (s *recoveryAPIState) failStart(run *recoveryRunState, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if run.status != recoveryRunRunning {
		return
	}
	run.status = recoveryRunFailed
	run.err = fmt.Sprintf("start recovery run: %v", err)
	run.finishedAt = time.Now()
	if s.current == run {
		s.current = nil
	}
}

// finishRun 记录运行最终结果并释放 in-flight 标记。
func (s *recoveryAPIState) finishRun(run *recoveryRunState, stats RecoveryStats, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run.stats = stats
	run.finishedAt = time.Now()
	run.status = recoveryRunCompleted
	if runErr != nil {
		run.err = runErr.Error()
		if stats.Scanned == 0 && stats.Recovered+stats.Failed+stats.Skipped == 0 {
			run.status = recoveryRunFailed
		}
	}
	if s.current == run {
		s.current = nil
	}
}

func (s *recoveryAPIState) evictLocked() {
	if len(s.order) <= recoveryRunHistoryLimit {
		return
	}
	drop := len(s.order) - recoveryRunHistoryLimit
	for _, id := range s.order[:drop] {
		run, ok := s.runs[id]
		if !ok || run.status == recoveryRunRunning {
			continue
		}
		delete(s.runs, id)
	}
	s.order = append([]string(nil), s.order[drop:]...)
}

func (s *recoveryAPIState) getRun(id string) (*recoveryRunState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[id]
	return run, ok
}

// observe 接收恢复任务的单文件结果回调：写入最近结果并推进对应运行的进度。
// 回调可能来自并发 worker，必须线程安全。
//
// 进度只累计到 runID 匹配且真正在执行的运行；启动扫描（runID 为空）以及
// 尚在排队（未取得 runMu）的手动 run 都不会被污染。
func (s *recoveryAPIState) observe(runID, file, outcome string, err error, samples int, truncated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if outcome == "recovered" {
		s.recent = append(s.recent, recoveryRecentEntry{
			File:      file,
			At:        time.Now(),
			Samples:   samples,
			Truncated: truncated,
		})
		if len(s.recent) > recoveryRecentHistoryLimit {
			s.recent = append([]recoveryRecentEntry(nil), s.recent[len(s.recent)-recoveryRecentHistoryLimit:]...)
		}
	}
	if runID == "" {
		return
	}
	run, ok := s.runs[runID]
	if !ok || run.status != recoveryRunRunning {
		return
	}
	switch outcome {
	case "recovered":
		run.stats.Recovered++
	case "skipped":
		run.stats.Skipped++
	default:
		run.stats.Failed++
		if err != nil {
			run.stats.Errors = append(run.stats.Errors, fmt.Errorf("%s: %w", file, err))
		}
	}
}

func (s *recoveryAPIState) recentSnapshot(limit int) []recoveryRecentEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.recent) {
		limit = len(s.recent)
	}
	out := make([]recoveryRecentEntry, limit)
	copy(out, s.recent[len(s.recent)-limit:])
	return out
}

func (s *recoveryAPIState) runView(run *recoveryRunState) recoveryRunView {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := recoveryRunView{
		Code:      0,
		ID:        run.id,
		Status:    run.status,
		StartedAt: run.startedAt,
		Progress:  newRecoveryStatsView(run.stats),
		Error:     run.err,
		Target: recoveryRunTargetView{
			Dirs: append([]string(nil), run.dirs...),
			File: run.file,
		},
	}
	if !run.finishedAt.IsZero() {
		finished := run.finishedAt
		view.FinishedAt = &finished
		result := newRecoveryStatsView(run.stats)
		view.Result = &result
	}
	return view
}

// recoveryRunKey 用于幂等判定：同一目标目录/文件视为同一请求。
func recoveryRunKey(dirs []string, file string) string {
	if file != "" {
		return "file:" + file
	}
	if len(dirs) == 0 {
		return "dir:<default>"
	}
	normalized := append([]string(nil), dirs...)
	sort.Strings(normalized)
	return "dir:" + strings.Join(normalized, "\x00")
}
