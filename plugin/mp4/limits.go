package plugin_mp4

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	task "github.com/eanfs/gotask"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/config"
)

const (
	// defaultRecordLimitWaitTimeout 是排队等待录制名额的上限；超时请求被拒绝（API 返回 503）。
	defaultRecordLimitWaitTimeout = 30 * time.Second
	// recordLimitQueueFactor 决定有界等待队列容量：上限 × 4。
	recordLimitQueueFactor = 4
)

var (
	// ErrRecordLimitQueueFull 表示所有名额被占用且等待队列已满。
	ErrRecordLimitQueueFull = errors.New("recording limit wait queue is full")
	// ErrRecordLimitTimeout 表示在排队超时时间内没有等到名额。
	ErrRecordLimitTimeout = errors.New("timed out waiting for a free recording slot")
	// ErrRecordLimiterStale 表示 waiter 属于另一个 RecordLimiter 实例（例如插件重启后
	// 换了一个 limiter）。此时必须当作已取消处理，绝不能在新 limiter 上释放名额。
	ErrRecordLimiterStale = errors.New("recording limiter changed while waiting for a slot")
)

// disabledRecordLimiter 在插件 Start 之前（或 max=0）使用，保证调用方无需判空。
var disabledRecordLimiter = NewRecordLimiter(0)

// RecordLimiter 是录制并发软保护：一个容量为 max 的信号量 + 容量为 max×4 的
// 有界等待队列。max<=0 时完全禁用（Acquire 立即成功、Release 为空操作），
// 保持未配置时的原有行为。
//
// 等待使用 context（默认 30s 超时），不使用裸 goroutine；内部状态由 mutex 保护。
//
// 已知限制（本任务不实现租约回收，见 PR 评审裁决）：
// 名额的持有期 = 录制任务的生命周期。recoder 用 SetRetry(-1, …) 无限重试，
// 所以“启动成功但子任务持续失败”的录制会一直持有名额：配置 max=N 后，N 路
// 坏流即可让后续自动录像全部停在排队/拒绝状态，只能运维介入（重启进程或修好
// 坏流）。不要把 max 配得过小，并对 "queued auto recording" / "refused by
// concurrency limit" 日志配置告警。
type RecordLimiter struct {
	max           int
	queueCapacity int
	timeout       time.Duration

	mu      sync.Mutex
	active  int
	waiters []*recordWaiter
}

// recordWaiter 记录一个排队位置及其所属 limiter。携带 limiter 是为了让 Wait
// 能识别“来自另一个 limiter 代次”的 waiter：插件重启会新建 limiter，旧 waiter
// 在新 limiter 中并不存在，不能把 removeWaiter 的 false 误解为“名额已交接”。
type recordWaiter struct {
	ch      chan struct{}
	limiter *RecordLimiter
}

// NewRecordLimiter 创建 limiter。max<=0 表示不限制。
func NewRecordLimiter(max int) *RecordLimiter {
	l := &RecordLimiter{max: max, timeout: defaultRecordLimitWaitTimeout}
	if max > 0 {
		l.queueCapacity = max * recordLimitQueueFactor
	}
	return l
}

// Enabled 报告是否启用了并发限制。
func (l *RecordLimiter) Enabled() bool {
	return l != nil && l.max > 0
}

// Active 返回当前已占用的名额数（测试/可观测用途）。
func (l *RecordLimiter) Active() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.active
}

// Waiting 返回当前排队等待的请求数（测试/可观测用途）。
func (l *RecordLimiter) Waiting() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.waiters)
}

// CanQueue 是发布路径上的廉价预检：报告新的录制请求是否还有可能拿到名额
// （有空闲名额，或等待队列未满）。它不改变任何状态——真正的名额分配必须由
// 拿到参数的任务自己调用 Acquire 完成，这样 AddTask 被同步拒绝时不会留下
// 孤儿 waiter（C2：先 Reserve 再 AddTask 会让名额随 AddTask 的拒绝永久泄漏）。
func (l *RecordLimiter) CanQueue() bool {
	if !l.Enabled() {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active < l.max {
		return true
	}
	return len(l.waiters) < l.queueCapacity
}

// Acquire 取得一个录制名额：有空闲名额时立即返回；否则进入有界等待队列，
// 在 timeout 内被 Release 唤醒则返回 nil，否则返回 ErrRecordLimitQueueFull
// 或 ErrRecordLimitTimeout（或 ctx 错误）。
func (l *RecordLimiter) Acquire(ctx context.Context) error {
	if !l.Enabled() {
		return nil
	}
	waiter, ok := l.Reserve()
	if !ok {
		return ErrRecordLimitQueueFull
	}
	return l.Wait(ctx, waiter)
}

// Reserve 非阻塞地尝试取得名额（waiter==nil）或预留一个等待位置。
// 队列已满时返回 ok=false。返回的 waiter 必须交给**同一个 limiter** 的 Wait 处理；
// 调用方若不再等待（例如任务被取消），仍应调用 Wait 以正确回收位置。
func (l *RecordLimiter) Reserve() (waiter *recordWaiter, ok bool) {
	if !l.Enabled() {
		return nil, true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// FIFO：只有没有等待者时才允许直接占用空闲名额，避免插队饿死排队请求。
	if l.active < l.max && len(l.waiters) == 0 {
		l.active++
		return nil, true
	}
	if len(l.waiters) >= l.queueCapacity {
		return nil, false
	}
	w := &recordWaiter{ch: make(chan struct{}, 1), limiter: l}
	l.waiters = append(l.waiters, w)
	return w, true
}

// Wait 等待预留的位置被 Release 交接。waiter==nil 表示 Reserve 已直接取得名额，
// 立即返回 nil。waiter 不是由本 limiter 预留的（插件重启换了一代 limiter）时
// 返回 ErrRecordLimiterStale，调用方不得在新 limiter 上释放名额。
func (l *RecordLimiter) Wait(ctx context.Context, waiter *recordWaiter) error {
	if waiter == nil {
		return nil
	}
	if waiter.limiter != l {
		return ErrRecordLimiterStale
	}
	if !l.Enabled() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	select {
	case <-waiter.ch:
		return nil
	case <-ctx.Done():
		// 与 Release 竞争：若位置仍可移除，说明没人把名额交给我们；否则
		// 名额已经在 waiter 中，按成功处理（不泄漏名额）。
		if l.removeWaiter(waiter) {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return ErrRecordLimitTimeout
			}
			return ctx.Err()
		}
		return nil
	}
}

// Release 归还一个名额。若存在排队者，名额直接交接给队首（active 不变），
// 否则递减 active。无名额可归还时是安全的空操作（不会变成负数）。
//
// 每次成功 Acquire 必须且只能调用一次 Release；API 层用 sync.Once 包装，
// 保证任务 dispose 与错误路径不会重复归还。
func (l *RecordLimiter) Release() {
	if !l.Enabled() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.waiters) > 0 {
		w := l.waiters[0]
		l.waiters = l.waiters[1:]
		select {
		case w.ch <- struct{}{}:
			return // 名额交接给等待者，active 保持 1
		default:
			// 等待者已超时但尚未从队列摘除，跳过它继续找下一个。
		}
	}
	if l.active > 0 {
		l.active--
	}
}

func (l *RecordLimiter) removeWaiter(waiter *recordWaiter) (removed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, w := range l.waiters {
		if w == waiter {
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
			return true
		}
	}
	return false
}

// acquireRecordingSlot 为一次 API 录制请求申请名额。返回的 release 是幂等的
// （内部 sync.Once），可同时用于错误路径与任务 dispose 回调。
func (p *MP4Plugin) acquireRecordingSlot(ctx context.Context) (release func(), err error) {
	l := p.getRecordLimiter()
	if !l.Enabled() {
		// 未配置上限：不申请名额、不注册释放回调，行为与既有版本完全一致。
		return nil, nil
	}
	if err = l.Acquire(ctx); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(l.Release) }, nil
}

// AcquireRecordSlot 实现 m7s.IRecordSlotPlugin：其它插件（GB28181 下载录像）
// 直接对本插件发起录制时，复用同一个 maxconcurrentrecordings 名额。
// 返回的 release 幂等，调用方须在录制任务 dispose 或启动失败时各调用一次。
func (p *MP4Plugin) AcquireRecordSlot(ctx context.Context) (release func(), err error) {
	return p.acquireRecordingSlot(ctx)
}

// 编译期断言：MP4 插件必须提供跨插件的录制名额入口。
var _ m7s.IRecordSlotPlugin = (*MP4Plugin)(nil)

func (p *MP4Plugin) getRecordLimiter() *RecordLimiter {
	if l := p.recordLimiter.Load(); l != nil {
		return l
	}
	return disabledRecordLimiter
}

// recordLimitRefusalError 把 limiter 的拒绝转换成 gRPC 状态错误。
// codes.Unavailable 经 grpc-gateway 默认映射为 HTTP 503。
func recordLimitRefusalError(err error) error {
	switch {
	case errors.Is(err, ErrRecordLimitQueueFull):
		return status.Error(codes.Unavailable, "recording concurrency limit reached: wait queue is full")
	case errors.Is(err, ErrRecordLimitTimeout):
		return status.Error(codes.Unavailable, "recording concurrency limit reached: timed out waiting for a free slot")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.Unavailable, "recording concurrency limit reached: request canceled while waiting")
	default:
		return err
	}
}

// GateRecord 实现 m7s.IRecordGatePlugin，为自动录像提供并发保护。onpub 与
// pull_on_start 两条路径都经由 m7s.Plugin.recordOnPublish 调用本方法。该回调
// 运行在发布路径上，禁止阻塞，也**不允许预占名额**（见下）。
//
// 名额所有权：本方法只做 CanQueue() 廉价预检并把录制参数交给任务系统；真正的
// 名额由 pendingAutoRecordTask 在自己的 Go() 里 Acquire。这样 AddTask 被同步
// 拒绝（插件已停止 / 键冲突 / 事件循环输入通道满）时，没有任何 waiter 或名额
// 被遗留——旧实现“先 Reserve 再 AddTask”每次拒绝都会永久少一个名额，最终只能
// 重启进程恢复（PR #10 C2）。
//
// 绕过本 gate 的录制入口：mp4 的 StartRecord / EventStart 走 acquireRecordingSlot
// 直接申请名额（同步 API，需立即返回结果，不能排队到任务里）；GB28181 下载录像
// 通过 m7s.IRecordSlotPlugin.AcquireRecordSlot 复用同一 limiter；flv/hls 使用
// 各自的 recorder，不受 mp4.limits.maxconcurrentrecordings 约束。
func (p *MP4Plugin) GateRecord(pub *m7s.Publisher, conf config.Record) (release func(), result m7s.RecordGateResult) {
	l := p.getRecordLimiter()
	if !l.Enabled() {
		return nil, m7s.RecordGateAdmit
	}
	// 插件已停止时 AddTask 会拒绝待录任务，直接拒绝并交给调用方记日志。
	if p.Context != nil && p.IsStopped() {
		return nil, m7s.RecordGateRefused
	}
	if !l.CanQueue() {
		return nil, m7s.RecordGateRefused
	}
	if !p.queueAutoRecording(pub, conf) {
		return nil, m7s.RecordGateRefused
	}
	return nil, m7s.RecordGateQueued
}

// queueAutoRecording 把一次被推迟的自动录像参数交给任务系统，由
// pendingAutoRecordTask.Go() 自行申请名额后启动录制。AddTask 被同步拒绝时返回
// false：此时任务不会运行，也没有任何名额/等待位置被持有。
func (p *MP4Plugin) queueAutoRecording(pub *m7s.Publisher, conf config.Record) bool {
	todo := &pendingAutoRecordTask{plugin: p, pub: pub, conf: conf}
	// 每个待录任务必须有唯一 key，否则 AddTask 会因键冲突拒绝后一个任务。
	todo.key = fmt.Sprintf("onpub-record:%p", todo)
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	t := p.AddTask(todo, logger.With("streamPath", pub.StreamPath, "filePath", conf.FilePath))
	if m7s.AddTaskRejected(p, t) {
		p.Error("queued auto recording rejected by task system",
			"streamPath", pub.StreamPath, "filePath", conf.FilePath, "reason", t.StopReason())
		return false
	}
	return true
}

// pendingAutoRecordTask 保存一次被推迟的自动录像参数，在 Go() 中**自行**申请
// 名额后启动录制。使用 Go()（独立 goroutine，不阻塞父事件循环）等待名额。
//
// limiter 在 Go() 运行时才从插件读取：名额由本次 Acquire 从零开始申请，与插件
// 重启前的 limiter 代次没有关系（不存在跨代 waiter 交接问题）。
type pendingAutoRecordTask struct {
	task.Task
	plugin *MP4Plugin
	pub    *m7s.Publisher
	conf   config.Record
	key    string
}

func (t *pendingAutoRecordTask) GetKey() string { return t.key }

func (t *pendingAutoRecordTask) Go() error {
	l := t.plugin.getRecordLimiter()
	// ctx 是任务自身：插件停止时 Context 随之取消，Acquire 立即返回错误，
	// 不会留下 waiter，也不会持有名额。
	if err := l.Acquire(t); err != nil {
		t.Error("auto recording refused by recording concurrency limit",
			"streamPath", t.pub.StreamPath, "filePath", t.conf.FilePath, "error", err)
		return task.ErrTaskComplete
	}
	var once sync.Once
	release := func() { once.Do(l.Release) }
	// publisher 的 dispose 会在它自己的事件循环 goroutine 上遍历/截断 resources
	// slice，而 pub.Using(job) 是裸 append（gotask Task.Using）。启动前先确认
	// publisher 仍存活，缩小这个交叉窗口；残余窗口内录制任务会因订阅失败自行
	// 退出并归还名额。
	if t.pub.GetTask().IsStopped() {
		release()
		t.Info("queued auto recording skipped: publisher already stopped",
			"streamPath", t.pub.StreamPath, "filePath", t.conf.FilePath)
		return task.ErrTaskComplete
	}
	t.startRecording(release)
	return task.ErrTaskComplete
}

// startRecording 启动录制并把名额所有权移交给录制任务。启动失败时立即归还名额。
func (t *pendingAutoRecordTask) startRecording(release func()) {
	job := t.plugin.RecordWithDispose(t.pub, t.conf, nil, release)
	if err := job.WaitStarted(); err != nil {
		release()
		t.Error("queued auto recording failed to start",
			"streamPath", t.pub.StreamPath, "filePath", t.conf.FilePath, "error", err)
		return
	}
	t.Info("queued auto recording started",
		"streamPath", t.pub.StreamPath, "filePath", t.conf.FilePath, "taskPtr", uint64(job.GetTaskPointer()))
}
