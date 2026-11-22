package plugin_mp4

import (
	"context"
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
//     - 活跃（含收尾中的 trailer/upload）→ 等待；
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
// 记录由 HTTP API（Start/StopRecord）与看护 Tick 并发访问。
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

// decideResume 是续录决策的纯函数（便于单测，不触碰 Server/任务系统）。
//
//   - active：该 streamPath 当前是否仍有活跃录制任务（含收尾中的）；
//   - streamUp：该 streamPath 的 publisher 当前是否在线且未停止；
//   - maxWait<=0 表示不限等待；maxSegments<=0 表示不限段数。
func decideResume(it *resumeIntent, active, streamUp bool, now time.Time, maxWait time.Duration, maxSegments int) (resumeAction, string) {
	// 1) 原请求的总时长已用尽：不再续录（无论流是否还在）。
	if !it.deadline.IsZero() && !now.Before(it.deadline) {
		return resumeActionGiveUp, "duration deadline reached"
	}
	// 2) 录制仍活跃（可能正在写 trailer/上传）：等它收尾，绝不并发开第二段。
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

// autoResumeTask 是续录看护任务（TickTask）：周期性检查意图表并决策。
type autoResumeTask struct {
	task.TickTask
	plugin *MP4Plugin
}

func (t *autoResumeTask) GetTickInterval() time.Duration {
	return effectiveInterval(t.plugin.RecordConfig.AutoResume.Interval)
}

func (t *autoResumeTask) Tick(any) {
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
// duration<=0（无限录制）或未开启 autoResume 时不登记。
func (p *MP4Plugin) registerResumeIntent(streamPath string, conf config.Record, duration time.Duration) {
	if !p.RecordConfig.AutoResume.Enable || duration <= 0 {
		return
	}
	p.resumeStore.put(&resumeIntent{
		streamPath: streamPath,
		conf:       conf,
		deadline:   time.Now().Add(duration),
		segments:   1,
	})
	p.Debug("auto resume intent registered", "streamPath", streamPath, "duration", duration)
}

// forgetResumeIntent 删除意图（用户主动 stop 或录制正常结束时调用）。
func (p *MP4Plugin) forgetResumeIntent(streamPath string) {
	if p.resumeStore.remove(streamPath) {
		p.Debug("auto resume intent removed", "streamPath", streamPath)
	}
}

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
		up := streamUp != nil && streamUp(it.streamPath)
		action, reason := decideResume(it, active, up, now, maxWait, cfg.MaxSegments)
		switch action {
		case resumeActionWait:
			continue
		case resumeActionResume:
			if resume != nil {
				resume(it)
			}
		case resumeActionGiveUp:
			if p.resumeStore.remove(it.streamPath) {
				p.Warn("auto resume stopped",
					"streamPath", it.streamPath, "reason", reason, "segments", it.segments)
			}
		}
	}
}

// hasActiveRecording 报告该 streamPath 当前是否仍有录制任务（含 dispose 收尾中）。
// 录制任务在完成 trailer 改写与上传交接后才离开 Server.Records，因此这里为真
// 期间绝不续录，避免同一段素材被重复写两份。
func (p *MP4Plugin) hasActiveRecording(streamPath string) bool {
	if p.Server == nil {
		return false
	}
	_, ok := p.Server.Records.Find(func(job *m7s.RecordJob) bool {
		return job.StreamPath == streamPath
	})
	return ok
}

// streamOnline 报告该 streamPath 的 publisher 是否在线且未停止。
func (p *MP4Plugin) streamOnline(streamPath string) bool {
	if p.Server == nil {
		return false
	}
	pub, ok := p.Server.Streams.SafeGet(streamPath)
	if !ok {
		return false
	}
	return !m7s.PublisherStopped(pub)
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
	pub, ok := p.Server.Streams.SafeGet(it.streamPath)
	if !ok || m7s.PublisherStopped(pub) {
		// 流在这轮检查与启动之间又消失：保持意图，下轮再看。
		return
	}
	ctx, cancel := context.WithTimeout(p.Context, 10*time.Second)
	defer cancel()
	release, err := p.acquireRecordingSlot(ctx)
	if err != nil {
		// 名额不足：保持意图，下轮重试（超过 MaxWait 自然放弃）。
		p.Warn("auto resume skipped: recording slot unavailable",
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
		p.Error("auto resume failed to start", "streamPath", it.streamPath, "error", err)
		return
	}
	it.segments++
	it.waitingSince = time.Time{}
	p.Info("auto recording resumed",
		"streamPath", it.streamPath, "segments", it.segments, "remaining", remaining)
}
