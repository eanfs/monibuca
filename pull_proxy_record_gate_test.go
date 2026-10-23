package m7s

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	task "github.com/eanfs/gotask"
	"m7s.live/v5/pkg/config"
)

// 这些用例钉住 C1：pull_on_start 自动录像必须与 onpub 走同一条准入路径
// （Plugin.recordOnPublish → IRecordGatePlugin.GateRecord），不得直接 Record。

type fakeGatePullProxy struct {
	task.Task
	cfg    *PullProxyConfig
	status byte
}

func (f *fakeGatePullProxy) GetBase() *BasePullProxy     { return nil }
func (f *fakeGatePullProxy) GetStreamPath() string       { return f.cfg.StreamPath }
func (f *fakeGatePullProxy) GetConfig() *PullProxyConfig { return f.cfg }
func (f *fakeGatePullProxy) ChangeStatus(status byte)    { f.status = status }
func (f *fakeGatePullProxy) GetPullJob() *PullJob        { return nil }
func (f *fakeGatePullProxy) Pull()                       {}
func (f *fakeGatePullProxy) GetKey() uint                { return f.cfg.ID }

type gatePullRecorder struct {
	task.Task
	job     RecordJob
	started chan struct{}
}

func (r *gatePullRecorder) GetRecordJob() *RecordJob { return &r.job }

func (r *gatePullRecorder) Start() error {
	select {
	case r.started <- struct{}{}:
	default:
	}
	return nil
}

// gatePlugin 同时扮演“已启动的 Plugin 任务”和 IRecordGatePlugin 处理器。
type gatePlugin struct {
	Plugin
	result   RecordGateResult
	release  func()
	calls    atomic.Int32
	lastPub  atomic.Value
	lastConf atomic.Value
}

func (g *gatePlugin) GateRecord(pub *Publisher, conf config.Record) (func(), RecordGateResult) {
	g.calls.Add(1)
	g.lastPub.Store(pub.StreamPath)
	g.lastConf.Store(conf.FilePath)
	return g.release, g.result
}

type pullGateTestEnv struct {
	server    *Server
	gate      *gatePlugin
	recorder  *gatePullRecorder
	proxy     *fakeGatePullProxy
	pluginCfg *PullProxyConfig
}

func newPullGateTestEnv(t *testing.T, result RecordGateResult) *pullGateTestEnv {
	t.Helper()

	var root task.RootManager[uint32, task.ManagerItem[uint32]]
	root.Init()

	var pluginRoot task.RootManager[string, task.ManagerItem[string]]
	pluginRoot.Init()
	// 不注册 Stop 清理：gotask v1.0.5 在 Work 的最后一个子任务 dispose 时存在
	// 已知的 EventLoop 数据竞争（event_loop.go:141/146），停止 root 会触发。
	// 测试进程结束即回收。

	rec := &gatePullRecorder{started: make(chan struct{}, 1)}
	s := &Server{}

	gate := &gatePlugin{result: result}
	gate.Logger = slog.New(slog.DiscardHandler)
	gate.Server = s
	gate.Meta = &PluginMeta{
		Name:        "MP4",
		NewRecorder: func(config.Record) IRecorder { return rec },
	}
	gate.release = func() {}
	// Plugin.handler 是插件实例（InstallPlugin 在真实流程中设置），recordOnPublish
	// 通过它查找 IRecordGatePlugin。
	gate.handler = gate
	s.Plugins.Add(&gate.Plugin)

	if err := pluginRoot.AddTask(gate).WaitStarted(); err != nil {
		t.Fatalf("start plugin: %v", err)
	}
	if err := root.AddTask(&s.Records).WaitStarted(); err != nil {
		t.Fatalf("start Records: %v", err)
	}
	if err := root.AddTask(&s.PullProxies).WaitStarted(); err != nil {
		t.Fatalf("start PullProxies: %v", err)
	}

	cfg := &PullProxyConfig{ID: 1, StreamPath: "live/pullgate", Status: PullProxyStatusOnline}
	cfg.Record.FilePath = "pull-on-start.mp4"
	proxy := &fakeGatePullProxy{cfg: cfg, status: PullProxyStatusOnline}
	if err := s.PullProxies.AddTask(proxy).WaitStarted(); err != nil {
		t.Fatalf("start pull proxy: %v", err)
	}

	return &pullGateTestEnv{server: s, gate: gate, recorder: rec, proxy: proxy, pluginCfg: cfg}
}

func (e *pullGateTestEnv) publisher() *Publisher {
	pub := &Publisher{}
	pub.StreamPath = e.pluginCfg.StreamPath
	pub.Plugin = &Plugin{Server: e.server}
	return pub
}

// pull_on_start 被拒绝时必须经过 gate（而不是直接 Record），并按原样保留参数。
func TestPullProxyOnStartGoesThroughRecordGate(t *testing.T) {
	env := newPullGateTestEnv(t, RecordGateRefused)

	env.publisher().processPullProxyOnStart()

	if got := env.gate.calls.Load(); got != 1 {
		t.Fatalf("GateRecord calls = %d, want 1 (pull_on_start bypassed the gate)", got)
	}
	if got := env.gate.lastPub.Load(); got != env.pluginCfg.StreamPath {
		t.Fatalf("gate streamPath = %v, want %s", got, env.pluginCfg.StreamPath)
	}
	if got := env.gate.lastConf.Load(); got != env.pluginCfg.FilePath {
		t.Fatalf("gate filePath = %v, want %s", got, env.pluginCfg.FilePath)
	}
	if got := env.server.Records.Length(); got != 0 {
		t.Fatalf("refused pull_on_start created %d recording job(s), want 0", got)
	}
}

// 准入通过时 pull_on_start 必须照常启动录制，并在 dispose 时释放名额。
func TestPullProxyOnStartAdmittedRecordingStarts(t *testing.T) {
	env := newPullGateTestEnv(t, RecordGateAdmit)
	var releases atomic.Int32
	env.gate.release = func() { releases.Add(1) }

	env.publisher().processPullProxyOnStart()

	if got := env.gate.calls.Load(); got != 1 {
		t.Fatalf("GateRecord calls = %d, want 1", got)
	}
	select {
	case <-env.recorder.started:
	case <-time.After(time.Second):
		t.Fatal("admitted pull_on_start recording did not start")
	}
	if got := env.server.Records.Length(); got != 1 {
		t.Fatalf("record jobs = %d, want 1", got)
	}

	// 不在此处 Stop 录制任务：gotask v1.0.5 的 EventLoop 退出存在已知数据竞争
	// （event_loop.go:141/146），本用例只验证“准入后确实启动录制”；名额释放的
	// 恰好一次语义由 plugin/mp4 的 TestRecordJobDisposeReleasesSlotExactlyOnce 钉住。
	if got := releases.Load(); got != 0 {
		t.Fatalf("release called %d times while recording is still active, want 0", got)
	}
}

// gate 返回值未显式设置（零值 RecordGateUnset）时必须 fail-closed：不启动录制。
// 该接口是本 PR 新增的，零值改造没有兼容成本；但若零值等于 Admit，任何漏赋值
// 都会静默放开并发限制。
func TestPullProxyOnStartUnsetGateResultDoesNotRecord(t *testing.T) {
	env := newPullGateTestEnv(t, RecordGateUnset)

	env.publisher().processPullProxyOnStart()

	if got := env.gate.calls.Load(); got != 1 {
		t.Fatalf("GateRecord calls = %d, want 1", got)
	}
	if got := env.server.Records.Length(); got != 0 {
		t.Fatalf("unset gate result created %d recording job(s), want 0 (fail-closed)", got)
	}
}
