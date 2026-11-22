package plugin_mp4

import (
	"context"
	"errors"
	"sync"
	"time"

	task "github.com/eanfs/gotask"
	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/config"
)

// 录制断流自动恢复（mp4.record.autoresume）。
//
// 背景：录制任务的所有权挂在 publisher 上（core.RecordWithDispose 里
// pub.Using(job)），流一断 publisher 销毁，RecordJob 被连带 Stop，recorder 的
// 重试又因 parent.IsStopped 被阻断——录制就此终结，要靠业务系统重新调
// /mp4/api/start 才能续上（现场补偿滞后达 9 分钟，60 分钟录像被切成多段）。
//
// 本文件不改 publisher 生命周期语义，而是在插件层维护「录制意图」：
//  1. /mp4/api/start 成功时登记意图（streamPath + 录制参数 + 绝对截止时间）；
//  2. 看护 Tick 周期性判断该 streamPath 是否还有活跃录制：
//     - 活跃（Server.Records 中尚未进入 disposing）→ 等待，避免同一段素材并行写两份；
//     - 不活跃且流已恢复、截止时间未到 → 重新启动录制（**新文件**，因为
//       progressive MP4 无法跨连接续写同一 mdat）；
//     - 截止时间到 / 等待超时 / 段数上限 → 放弃并清理意图。
//  3. /mp4/api/stop 显式删除意图（用户主动停止不续录）。
//
// 只对 API 启动且指定了 duration 的连续录制生效：duration=0 的无限录制与
// 事件录像、GB28181 下载录像都不登记意图，避免无限续录或误续事件片段。

// resumeIntent 是一次「可自动续录」的录制意图。
type resumeIntent struct {
	streamPath string
	conf       config.Record // 原始录制参数（续录时按剩余时长覆盖 Duration）
	// deadline 是绝对截止时间（首次启动时刻 + duration）。续录任务的
	// Duration 取 deadline 剩余量，从而保证多段累计不超过原请求的总时长。
	deadline time.Time
	// segments 是已产生的段数（首段计入）。用于 MaxSegments 上限。
	segments int
	// waitingSince 是「录制已结束、正在等流恢复」的起点；录制活跃时为零值。
	// 由看护 Tick 维护，用于 MaxWait 超时判定。
	waitingSince time.Time
}

// autoResumeStore 是按 streamPath 索引的意图表（并发安全）。
// 记录由 HTTP API（Start/StopRecord）与看护 Tick 并发访问。锁只保护 map 成员
// （增删/查找）；resumeIntent 的字段由看护 Tick 单 goroutine 写入，HTTP API
// 只整体替换或删除条目，不原地修改已登记意图的字段。
type autoResumeStore struct {
	mu sync.Mutex
	m  map[string]*resumeIntent
}

func (s *autoResumeStore) put(it *resumeIntent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]*resumeIntent)
	}
	s.m[it.streamPath] = it
}

func (s *autoResumeStore) remove(streamPath string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[streamPath]; !ok {
		return false
	}
	delete(s.m, streamPath)
	return true
}

func (s *autoResumeStore) snapshot() []*resumeIntent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) == 0 {
		return nil
	}
	out := make([]*resumeIntent, 0, len(s.m))
	for _, it := range s.m {
		out = append(out, it)
	}
	return out
}

// isCurrent 报告 it 是否仍是该 streamPath 的最新意图。
//
// 看护 Tick 先 snapshot 再决策，期间 /mp4/api/stop 可能已删除意图，或
// /mp4/api/start 已用新意图替换旧意图。若不加校验，正在收尾的旧快照会把用户
// 刚停掉的录制重新拉起——显式 stop 必须能压过自动续录。
func (s *autoResumeStore) isCurrent(it *resumeIntent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.m[it.streamPath]
	return ok && cur == it
}

// resumeAction 是看护轮次对单个意图的处置。
type resumeAction int

const (
	// resumeActionWait 继续等待（录制仍活跃 / 流未恢复且未超时）。
	resumeActionWait resumeAction = iota
	// resumeActionResume 录制已结束且流已恢复，立即续录。
	resumeActionResume
	// resumeActionGiveUp 放弃续录并清理意图。
	resumeActionGiveUp
)

// resumeReasonDeadlineReached 表示录制时长已正常用尽——这是预期结束，不是异常，
// 不要用 Warn 刷日志（每场 36 路录制正常结束会各刷一条）。
const resumeReasonDeadlineReached = "duration deadline reached"

// decideResume 是续录决策的纯函数（便于单测，不触碰 Server/任务系统）。
//
//   - active：该 streamPath 当前是否仍有运行中的录制任务（Server.Records 只暴露
//     未进入 disposing 的任务；trailer 收尾与上传交接不阻塞续录）；
//   - streamUp：该 streamPath 的 publisher 当前是否在线且未停止；
//   - maxWait<=0 表示不限等待；maxSegments<=0 表示不限段数。
func decideResume(it *resumeIntent, active, streamUp bool, now time.Time, maxWait time.Duration, maxSegments int) (resumeAction, string) {
	// 1) 原请求的总时长已用尽：不再续录（无论流是否还在）。
	if !it.deadline.IsZero() && !now.Before(it.deadline) {
		return resumeActionGiveUp, resumeReasonDeadlineReached
	}
	// 2) 录制仍活跃：等它结束，避免同一段素材被重复写两份。
	if active {
		return resumeActionWait, ""
	}
	// 3) 段数上限（首段已计入）。
	if maxSegments > 0 && it.segments >= maxSegments {
		return resumeActionGiveUp, "max segments reached"
	}
	// 4) 流未恢复：等；超过 MaxWait 则放弃，避免意图永久驻留。
	if !streamUp {
		if maxWait > 0 && !it.waitingSince.IsZero() && now.Sub(it.waitingSince) >= maxWait {
			return resumeActionGiveUp, "stream did not recover within max wait"
		}
		return resumeActionWait, ""
	}
	// 5) 录制已结束且流已恢复：续录。
	return resumeActionResume, ""
}

// effectiveMaxWait/effectiveInterval 对配置做兜底，避免 0/负值导致行为歧义。
func effectiveMaxWait(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func effectiveInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return 5 * time.Second
	}
	return d
}

// autoResumeTask 是续录看护任务（AsyncTickTask）：周期性检查意图表并决策。
//
// Tick 必须跑在独立 goroutine（AsyncTickTask）：它要访问 Server.Records /
// Server.Streams，并可能同步等待续录任务启动（WaitStarted 最长 10s）。内联
// 在 MP4 插件的事件循环里会阻塞插件的全部任务调度。
type autoResumeTask struct {
	task.AsyncTickTask
	plugin *MP4Plugin
}

func (t *autoResumeTask) GetTickInterval() time.Duration {
	return effectiveInterval(t.plugin.RecordConfig.AutoResume.Interval)
}

func (t *autoResumeTask) Tick(any) {
	// 单轮 panic 不能杀掉看护任务本身（否则自动续录静默失效）。
	defer func() {
		if r := recover(); r != nil {
			t.Error("auto resume tick panic", "panic", r)
		}
	}()
	if t.plugin == nil || !t.plugin.RecordConfig.AutoResume.Enable {
		return
	}
	t.plugin.autoResumeTickWith(
		t.plugin.hasActiveRecording,
		t.plugin.streamOnline,
		t.plugin.resumeRecording,
		time.Now(),
	)
}

// registerResumeIntent 在 API 启动录制成功后登记意图。
// 录制参数里的 Duration<=0（无限录制）或未开启 autoResume 时不登记。
func (p *MP4Plugin) registerResumeIntent(streamPath string, conf config.Record) {
	duration := conf.Duration
	if !p.RecordConfig.AutoResume.Enable || duration <= 0 {
		return
	}
	p.resumeStore.put(&resumeIntent{
		streamPath: streamPath,
		conf:       conf,
		deadline:   time.Now().Add(duration),
		segments:   1,
	})
	p.Info("auto resume intent registered", "streamPath", streamPath, "duration", duration)
}

// forgetResumeIntent 删除意图（用户主动 stop 或录制正常结束时调用）。
// 返回是否确实删除了意图，供 StopRecord 区分「无录制但取消了待续录」与「完全
// 不存在」两种情况。
func (p *MP4Plugin) forgetResumeIntent(streamPath string) bool {
	removed := p.resumeStore.remove(streamPath)
	if removed {
		p.Debug("auto resume intent removed", "streamPath", streamPath)
	}
	return removed
}

// OnRecordTaskStopped 实现 m7s.IRecordTaskStopHook：通用任务接口 /api/task/stop
// 直接 Stop RecordJob，不会经过 /mp4/api/stop；这里补清意图，保证显式 stop 优先。
func (p *MP4Plugin) OnRecordTaskStopped(streamPath string) {
	p.forgetResumeIntent(streamPath)
}

var _ m7s.IRecordTaskStopHook = (*MP4Plugin)(nil)

// autoResumeTickWith 是看护的一轮逻辑；依赖以参数注入，便于单测。
func (p *MP4Plugin) autoResumeTickWith(
	hasActive func(string) bool,
	streamUp func(string) bool,
	resume func(*resumeIntent),
	now time.Time,
) {
	cfg := p.RecordConfig.AutoResume
	maxWait := effectiveMaxWait(cfg.MaxWait)
	for _, it := range p.resumeStore.snapshot() {
		active := hasActive != nil && hasActive(it.streamPath)
		// 维护等待起点：录制活跃时清零；刚从活跃转入不活跃时开始计时。
		if active {
			it.waitingSince = time.Time{}
		} else if it.waitingSince.IsZero() {
			it.waitingSince = now
		}
		// 录制活跃时 decideResume 只会走到「等待」或 deadline 到期的「放弃」，
		// 不读 streamUp；跳过探测，避免活跃期间每 tick 每意图都查一次
		// Server.Streams（36 路录制约 2.6 万次/小时）。
		var up bool
		if !active {
			up = streamUp != nil && streamUp(it.streamPath)
		}
		action, reason := decideResume(it, active, up, now, maxWait, cfg.MaxSegments)
		switch action {
		case resumeActionWait:
			continue
		case resumeActionResume:
			// 意图可能在 snapshot 之后被 stop 删除或被新 start 替换，只有仍是最新
			// 意图时才续录（见 isCurrent）。
			if resume != nil && p.resumeStore.isCurrent(it) {
				resume(it)
			}
		case resumeActionGiveUp:
			if p.resumeStore.remove(it.streamPath) {
				if reason == resumeReasonDeadlineReached {
					p.Debug("auto resume finished",
						"streamPath", it.streamPath, "reason", reason, "segments", it.segments)
				} else {
					p.Warn("auto resume stopped",
						"streamPath", it.streamPath, "reason", reason, "segments", it.segments)
				}
			}
		}
	}
}

// hasActiveRecording 报告该 streamPath 当前是否仍有运行中的录制任务。
// Server.Records 的 Find/Range 会过滤掉已进入 disposing 的任务，因此 trailer
// 收尾与上传交接期间这里为 false；此时续录会写新文件，不会覆盖旧段。
func (p *MP4Plugin) hasActiveRecording(streamPath string) bool {
	if p.Server == nil {
		return false
	}
	_, ok := p.Server.Records.Find(func(job *m7s.RecordJob) bool {
		return job.StreamPath == streamPath
	})
	return ok
}

// livePublisher 返回在线且未停止的 publisher；流不存在、publisher 已停止或
// Server 尚未就绪时返回 nil。
func (p *MP4Plugin) livePublisher(streamPath string) *m7s.Publisher {
	if p.Server == nil {
		return nil
	}
	pub, ok := p.Server.Streams.SafeGet(streamPath)
	if !ok || m7s.PublisherStopped(pub) {
		return nil
	}
	return pub
}

// streamOnline 报告该 streamPath 的 publisher 是否在线且未停止。
func (p *MP4Plugin) streamOnline(streamPath string) bool {
	return p.livePublisher(streamPath) != nil
}

// resumeRecording 为意图启动新的一段录制。
//
// 要点：
//   - 重新申请并发录制名额（原名额已在上一段 dispose 时释放）；
//   - Duration 取 deadline 剩余量，多段累计不超过原请求总时长；
//   - 指定了 fileName 时必须去掉，否则每段都写同一路径、后段覆盖前段
//     （progressive MP4 + fragment=0 下 CustomFileName 固定返回该文件名）。
func (p *MP4Plugin) resumeRecording(it *resumeIntent) {
	if p.Server == nil {
		return
	}
	remaining := time.Until(it.deadline)
	if remaining <= 0 {
		p.forgetResumeIntent(it.streamPath)
		return
	}
	pub := p.livePublisher(it.streamPath)
	if pub == nil {
		// 流在这轮检查与启动之间又消失：保持意图，下轮再看。
		return
	}
	ctx, cancel := context.WithTimeout(p.Context, 10*time.Second)
	defer cancel()
	release, err := p.acquireRecordingSlot(ctx)
	if err != nil {
		// 名额不足：保持意图，下轮重试（超过 MaxWait 自然放弃）。
		// 限流饱和时每 5s 每意图都会走到这里，Warn 会刷屏，降为 Debug。
		p.Debug("auto resume skipped: recording slot unavailable",
			"streamPath", it.streamPath, "error", err)
		return
	}
	conf := it.conf
	conf.Duration = remaining
	if conf.FileName != "" {
		p.Warn("auto resume: fixed fileName dropped to avoid overwrite",
			"streamPath", it.streamPath, "fileName", conf.FileName)
		conf.FileName = ""
	}
	job := p.RecordWithDispose(pub, conf, nil, release)
	if err := job.WaitStarted(); err != nil {
		if release != nil {
			release()
		}
		if _, ok := errors.AsType[task.ExistTaskError](err); ok {
			// 上一段的 RecordJob 仍在 Records 中做 trailer/上传交接，同 key 注册被拒。
			// 这是正常收尾而非故障：保持意图，下个 tick 再试，不要把收尾刷成 Error。
			p.Debug("auto resume deferred: previous recording still finishing",
				"streamPath", it.streamPath, "error", err)
			return
		}
		p.Error("auto resume failed to start", "streamPath", it.streamPath, "error", err)
		return
	}
	// 启动期间用户可能已 /mp4/api/stop：StopRecord 先删意图、再查 Records，
	// 若它查询时本任务尚未注册就会被漏停。这里补一次校验，尊重显式 stop。
	if !p.resumeStore.isCurrent(it) {
		job.GetTask().Stop(task.ErrStopByUser)
		p.Debug("auto resume abandoned: intent replaced or stopped during start",
			"streamPath", it.streamPath)
		return
	}
	// publisher 可能在注册与 WaitStarted 返回之间停止，任务随即被连带 Stop——此时
	// 没有真正录到数据。不能计段、不能报续录成功，保留意图等下一轮重新决策。
	if job.GetTask().IsStopped() {
		p.Debug("auto resume job stopped before recording, keep intent",
			"streamPath", it.streamPath, "reason", job.GetTask().StopReason())
		return
	}
	it.segments++
	it.waitingSince = time.Time{}
	p.Info("auto recording resumed",
		"streamPath", it.streamPath, "segments", it.segments, "remaining", remaining)
}
