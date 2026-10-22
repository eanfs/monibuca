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
)

// disabledRecordLimiter 在插件 Start 之前（或 max=0）使用，保证调用方无需判空。
var disabledRecordLimiter = NewRecordLimiter(0)

// RecordLimiter 是录制并发软保护：一个容量为 max 的信号量 + 容量为 max×4 的
// 有界等待队列。max<=0 时完全禁用（Acquire 立即成功、Release 为空操作），
// 保持未配置时的原有行为。
//
// 等待使用 context（默认 30s 超时），不使用裸 goroutine；内部状态由 mutex 保护。
type RecordLimiter struct {
	max           int
	queueCapacity int
	timeout       time.Duration

	mu      sync.Mutex
	active  int
	waiters []chan struct{}
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
// 队列已满时返回 ok=false。返回的 waiter 必须交给 Wait 处理；
// 调用方若不再等待（例如任务被取消），仍应调用 Wait 以正确回收位置。
func (l *RecordLimiter) Reserve() (waiter <-chan struct{}, ok bool) {
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
	ch := make(chan struct{}, 1)
	l.waiters = append(l.waiters, ch)
	return ch, true
}

// Wait 等待预留的位置被 Release 交接。waiter==nil 表示 Reserve 已直接取得名额，
// 立即返回 nil。
func (l *RecordLimiter) Wait(ctx context.Context, waiter <-chan struct{}) error {
	if waiter == nil {
		return nil
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
	case <-waiter:
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
		ch := l.waiters[0]
		l.waiters = l.waiters[1:]
		select {
		case ch <- struct{}{}:
			return // 名额交接给等待者，active 保持 1
		default:
			// 等待者已超时但尚未从队列摘除，跳过它继续找下一个。
		}
	}
	if l.active > 0 {
		l.active--
	}
}

func (l *RecordLimiter) removeWaiter(waiter <-chan struct{}) (removed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, ch := range l.waiters {
		if ch == waiter {
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

// GateRecord 实现 m7s.IRecordGatePlugin，为自动录像（onpub / pull_on_start）
// 提供并发保护。该回调运行在发布路径上，禁止阻塞：
//   - 有空闲名额：立即准入，release 交给录制任务 dispose 时调用；
//   - 名额已满但等待队列有位置：把录制参数保存到 pendingAutoRecordTask，
//     由任务系统在队首等待名额后再启动录制（不丢参数、不静默停录）；
//   - 名额与队列都满：返回 Refused，调用方记录 Error（含 streamPath/filePath）。
func (p *MP4Plugin) GateRecord(pub *m7s.Publisher, conf config.Record) (release func(), result m7s.RecordGateResult) {
	l := p.getRecordLimiter()
	if !l.Enabled() {
		return nil, m7s.RecordGateAdmit
	}
	// 插件已停止时 AddTask 会拒绝待录任务，直接拒绝并交给调用方记日志，
	// 避免名额/排队位置无人消费。
	if p.Context != nil && p.IsStopped() {
		return nil, m7s.RecordGateRefused
	}
	waiter, ok := l.Reserve()
	if !ok {
		return nil, m7s.RecordGateRefused
	}
	if waiter == nil {
		var once sync.Once
		return func() { once.Do(l.Release) }, m7s.RecordGateAdmit
	}
	todo := &pendingAutoRecordTask{
		plugin: p,
		pub:    pub,
		conf:   conf,
		waiter: waiter,
	}
	// 每个待录任务必须有唯一 key，否则 AddTask 会因键冲突拒绝后一个任务。
	todo.key = fmt.Sprintf("onpub-record:%p", todo)
	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	p.AddTask(todo, logger.With("streamPath", pub.StreamPath, "filePath", conf.FilePath))
	return nil, m7s.RecordGateQueued
}

// pendingAutoRecordTask 保存一次被推迟的自动录制参数，在排队取得名额后启动录制。
// 使用 Go()（独立 goroutine，不阻塞父事件循环）等待名额。
type pendingAutoRecordTask struct {
	task.Task
	plugin *MP4Plugin
	pub    *m7s.Publisher
	conf   config.Record
	waiter <-chan struct{}
	key    string
}

func (t *pendingAutoRecordTask) GetKey() string { return t.key }

func (t *pendingAutoRecordTask) Go() error {
	l := t.plugin.getRecordLimiter()
	if !l.Enabled() {
		// 插件在排队期间被重新配置为不限制：直接启动。
		t.startRecording(func() {})
		return task.ErrTaskComplete
	}
	if err := l.Wait(t, t.waiter); err != nil {
		t.Error("onpub recording refused by recording concurrency limit",
			"streamPath", t.pub.StreamPath, "filePath", t.conf.FilePath, "error", err)
		return task.ErrTaskComplete
	}
	var once sync.Once
	release := func() { once.Do(l.Release) }
	if t.pub.GetTask().IsStopped() {
		release()
		t.Info("queued onpub recording skipped: publisher already stopped",
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
		t.Error("queued onpub recording failed to start",
			"streamPath", t.pub.StreamPath, "filePath", t.conf.FilePath, "error", err)
		return
	}
	t.Info("queued onpub recording started",
		"streamPath", t.pub.StreamPath, "filePath", t.conf.FilePath, "taskPtr", uint64(job.GetTaskPointer()))
}
