package m7s

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/quic-go/quic-go"

	gatewayRuntime "github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	myip "github.com/husanpao/ip"
	"google.golang.org/grpc"
	"gorm.io/gorm"

	task "github.com/eanfs/gotask"
	. "m7s.live/v5/pkg"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/db"
	"m7s.live/v5/pkg/util"
)

type (
	DefaultYaml    string
	OnExitHandler  func()
	AuthPublisher  = func(*Publisher) *util.Promise
	AuthSubscriber = func(*Subscriber) *util.Promise

	PluginMeta struct {
		Name                string
		Version             string //插件版本
		Type                reflect.Type
		DefaultYaml         DefaultYaml //默认配置
		ServiceDesc         *grpc.ServiceDesc
		RegisterGRPCHandler func(context.Context, *gatewayRuntime.ServeMux, *grpc.ClientConn) error
		NewPuller           PullerFactory
		NewPusher           PusherFactory
		NewRecorder         RecorderFactory
		NewTransformer      TransformerFactory
		NewPullProxy        PullProxyFactory
		NewPushProxy        PushProxyFactory
		OnExit              OnExitHandler
		OnAuthPub           AuthPublisher
		OnAuthSub           AuthSubscriber
	}

	iPlugin interface {
		nothing()
	}

	IPlugin interface {
		task.IJob
		Pull(string, config.Pull, *config.Publish) (*PullJob, error)
		Push(string, config.Push, *config.Subscribe)
		Transform(*Publisher, config.Transform)
	}

	IRegisterHandler interface {
		RegisterHandler() map[string]http.HandlerFunc
	}

	IPullerPlugin interface {
		GetPullableList() []string
	}

	ITCPPlugin interface {
		OnTCPConnect(conn net.Conn) task.ITask
	}

	IUDPPlugin interface {
		OnUDPConnect(conn *net.UDPConn) task.ITask
	}

	IQUICPlugin interface {
		OnQUICConnect(*quic.Conn) task.ITask
	}

	IPublishHookPlugin interface {
		OnPublish(pub *Publisher)
	}

	// RecordGateResult 描述自动录像（onpub / pull_on_start）在并发保护下的准入结果。
	RecordGateResult int

	// RecordAdmission 是 GateRecord 的准入结论。
	//
	// Release 非空时表示插件已为本次录制持有一个必须归还的资源（并发名额等），
	// 由调用方负责调用。**Release 必须幂等**：调用方会在“启动观测任务被拒”
	// “RecordJob 被 Records 拒绝”“注册后发现 publisher 已停止”等提前失败路径上
	// 调用一次，之后 dispose 回调还会再调用一次（实现应使用 sync.Once 包裹）。
	//
	// Release 允许为 nil，表示插件没有需要归还的资源；调用方必须容忍 nil，
	// 绝不能因为缺少 release 就跳过启动录制或解引用它。
	//
	// Result 与 Release 必须一起返回（而不是两个裸返回值）：只拿到 result 的
	// 调用方无法判断“要不要归还”，旧签名让漏接 release 变成静默的名额泄漏。
	RecordAdmission struct {
		Result  RecordGateResult
		Release func()
	}

	// IRecordGatePlugin 由希望限制自动录像并发的插件实现。
	// GateRecord 在发布路径上被调用，禁止阻塞。
	IRecordGatePlugin interface {
		// GateRecord 在自动录制启动前决定是否准入：
		//   Result 为 Admit 表示立即允许录制（Release 可为 nil）；
		//   Result 为 Queued 表示插件已保存录制参数、稍后会自行启动；
		//   Result 为 Refused 表示本次录制无法启动，调用方记录 Error；
		//   Result 为 Unset 或未知值视为拒绝（fail-closed）；
		//   Release 的调用与幂等契约见 RecordAdmission。
		GateRecord(pub *Publisher, conf config.Record) RecordAdmission
	}

	// IRecordSlotPlugin 由实现了录制并发保护的插件实现，供其它插件在直接发起
	// 录制时申请同一份名额（例如 GB28181 下载录像复用 MP4 插件的
	// maxconcurrentrecordings）。未实现该接口的插件不受并发限制。
	IRecordSlotPlugin interface {
		// AcquireRecordSlot 取得一个录制名额。返回的 release 必须且只能调用一次：
		// 录制任务 dispose 时由 RecordWithDispose 的回调调用；任务启动失败或
		// 录制根本没启动时由调用方显式调用。release 自身幂等。
		AcquireRecordSlot(ctx context.Context) (release func(), err error)
	}

	ISubscribeHookPlugin interface {
		OnSubscribe(streamPath string, args url.Values)
	}
)

const (
	// RecordGateUnset 是零值：插件没有给出明确结论。调用方必须按拒绝处理
	// （fail-closed），否则任何漏赋值的路径都会静默绕过并发限制。
	RecordGateUnset RecordGateResult = iota
	// RecordGateAdmit 表示立即允许录制。
	RecordGateAdmit
	// RecordGateQueued 表示插件已保存录制参数并会在取得名额后启动。
	RecordGateQueued
	// RecordGateRefused 表示名额与等待队列均已满，本次录制被拒绝。
	RecordGateRefused
)

var plugins []PluginMeta

// errPublisherStoppedDuringRegistration 是在 RecordWithDispose 注册后复核时发现
// publisher 已停止所用的停止原因：job 从未真正开始录制，必须立即回收。
var errPublisherStoppedDuringRegistration = errors.New("publisher stopped during recording registration")

func (plugin *PluginMeta) Init(s *Server, userConfig map[string]any) (p *Plugin) {
	instance, ok := reflect.New(plugin.Type).Interface().(IPlugin)
	if !ok {
		panic("plugin " + plugin.Name + " must implement IPlugin")
	}
	p = reflect.ValueOf(instance).Elem().FieldByName("Plugin").Addr().Interface().(*Plugin)
	p.handler = instance
	p.Meta = plugin
	p.Server = s
	p.Logger = s.Logger.With("plugin", plugin.Name)
	upperName := strings.ToUpper(plugin.Name)
	if os.Getenv(upperName+"_ENABLE") == "false" {
		p.disable("env")
		p.Warn("disabled by env")
		return
	}
	p.Config.Parse(p.GetCommonConf(), upperName)
	p.Config.Parse(instance, upperName)
	for _, fname := range MergeConfigs {
		if name := strings.ToLower(fname); p.Config.Has(name) {
			p.Config.Get(name).ParseGlobal(s.Config.Get(name))
		}
	}
	if plugin.DefaultYaml != "" {
		var defaultConf map[string]any
		if err := yaml.Unmarshal([]byte(plugin.DefaultYaml), &defaultConf); err != nil {
			p.Error("parsing default config", "error", err)
		} else {
			p.Config.ParseDefaultYaml(defaultConf)
		}
	}
	p.Config.ParseUserFile(userConfig)
	p.SetDescription("version", plugin.Version)
	if userConfig != nil {
		p.SetDescription("userConfig", userConfig)
	}
	finalConfig, _ := yaml.Marshal(p.Config.GetMap())
	p.Logger.Handler().(*MultiLogHandler).SetLevel(ParseLevel(p.config.LogLevel))
	p.Debug("config", "detail", string(finalConfig))
	if userConfig["enable"] == false || (s.DisableAll && userConfig["enable"] != true) {
		p.disable("config")
		return
	}
	p.Info("init", "version", plugin.Version)
	var err error
	if p.config.DSN == s.GetCommonConf().DSN {
		p.DB = s.DB
	} else if p.config.DSN != "" {
		if factory, ok := db.Factory[p.config.DBType]; ok {
			p.DB, err = gorm.Open(factory(p.config.DSN), &gorm.Config{})
			if err != nil {
				s.Error("failed to connect database", "error", err, "dsn", s.config.DSN, "type", s.config.DBType)
				p.disable(fmt.Sprintf("database %v", err))
				return
			}
		}
	}
	if p.DB != nil && p.Meta.NewRecorder != nil {
		if err = p.DB.AutoMigrate(&RecordStream{}); err != nil {
			p.disable(fmt.Sprintf("auto migrate record stream failed %v", err))
			return
		}
		if err = p.DB.AutoMigrate(&EventRecordStream{}); err != nil {
			p.disable(fmt.Sprintf("auto migrate event record stream failed %v", err))
			return
		}
	}
	if err = s.AddTask(instance).WaitStarted(); err != nil {
		p.disable(instance.StopReason().Error())
		return
	}
	if err = p.listen(); err != nil {
		p.Stop(err)
		p.disable(err.Error())
		return
	}
	if p.Meta.ServiceDesc != nil && s.grpcServer != nil {
		s.grpcServer.RegisterService(p.Meta.ServiceDesc, p.handler)
		if p.Meta.RegisterGRPCHandler != nil {
			if err = p.Meta.RegisterGRPCHandler(p.Context, s.config.HTTP.GetGRPCMux(), s.grpcClientConn); err != nil {
				p.Stop(err)
				p.disable(fmt.Sprintf("grpc %v", err))
				return
			} else {
				p.Info("grpc handler registered")
			}
		}
	}
	if p.config.Hook != nil {
		if hook, ok := p.config.Hook[config.HookOnServerKeepAlive]; ok && hook.Interval > 0 {
			p.AddTask(&ServerKeepAliveTask{plugin: p})
		}
	}
	var handlers map[string]http.HandlerFunc
	if v, ok := instance.(IRegisterHandler); ok {
		handlers = v.RegisterHandler()
	}
	p.registerHandler(handlers)
	p.OnDispose(func() {
		s.Plugins.Remove(p)
	})
	s.Plugins.Add(p)
	return
}

// InstallPlugin 安装插件
func InstallPlugin[C iPlugin](meta PluginMeta) error {
	var c *C
	meta.Type = reflect.TypeOf(c).Elem()
	if meta.Name == "" {
		meta.Name = strings.TrimSuffix(meta.Type.Name(), "Plugin")
	}
	_, pluginFilePath, _, _ := runtime.Caller(1)
	configDir := filepath.Dir(pluginFilePath)
	if meta.Version == "" {
		if _, after, found := strings.Cut(configDir, "@"); found {
			meta.Version = after
		} else {
			meta.Version = "dev"
		}
	}
	plugins = append(plugins, meta)
	return nil
}

var _ IPlugin = (*Plugin)(nil)

type Plugin struct {
	task.Work
	config.Config
	Disabled           bool
	Meta               *PluginMeta
	PushAddr, PlayAddr []string
	config             config.Common
	handler            IPlugin
	Server             *Server
	DB                 *gorm.DB
}

func (Plugin) nothing() {

}

func (p *Plugin) GetKey() string {
	return p.Meta.Name
}

func (p *Plugin) GetGlobalCommonConf() *config.Common {
	return p.Server.GetCommonConf()
}

func (p *Plugin) GetCommonConf() *config.Common {
	return &p.config
}

func (p *Plugin) GetHandler() IPlugin {
	return p.handler
}

func (p *Plugin) GetPublicIP(netcardIP string) string {
	if p.config.PublicIP != "" {
		return p.config.PublicIP
	}
	if publicIP := util.GetPublicIP(netcardIP); publicIP != netcardIP {
		return publicIP
	}
	localIp := myip.InternalIPv4()
	if publicIP := util.GetPublicIP(localIp); publicIP != localIp {
		return publicIP
	}
	return localIp
}

func (p *Plugin) disable(reason string) {
	p.Disabled = true
	p.SetDescription("disableReason", reason)
	p.Warn("plugin disabled")
	p.Server.disabledPlugins = append(p.Server.disabledPlugins, p)
}

func (p *Plugin) listen() (err error) {
	httpConf := &p.config.HTTP

	if httpConf.ListenAddrTLS != "" && (httpConf.ListenAddrTLS != p.Server.config.HTTP.ListenAddrTLS) {
		p.SetDescription("httpTLS", strings.TrimPrefix(httpConf.ListenAddrTLS, ":"))
		p.AddDependTask(CreateHTTPSWork(httpConf, p.Logger))
	}

	if httpConf.ListenAddr != "" && (httpConf.ListenAddr != p.Server.config.HTTP.ListenAddr) {
		p.SetDescription("http", strings.TrimPrefix(httpConf.ListenAddr, ":"))
		p.AddDependTask(CreateHTTPWork(httpConf, p.Logger))
	}

	if tcphandler, ok := p.handler.(ITCPPlugin); ok {
		tcpConf := &p.config.TCP
		if tcpConf.ListenAddr != "" {
			if tcpConf.AutoListen {
				if err = p.AddTask(tcpConf.CreateTCPWork(p.Logger, tcphandler.OnTCPConnect)).WaitStarted(); err != nil {
					return
				}
			}
			p.SetDescription("tcp", strings.TrimPrefix(tcpConf.ListenAddr, ":"))
		}
		if tcpConf.ListenAddrTLS != "" {
			if tcpConf.AutoListen {
				if err = p.AddTask(tcpConf.CreateTCPTLSWork(p.Logger, tcphandler.OnTCPConnect)).WaitStarted(); err != nil {
					return
				}
			}
			p.SetDescription("tcpTLS", strings.TrimPrefix(tcpConf.ListenAddrTLS, ":"))
		}
	}

	if udpHandler, ok := p.handler.(IUDPPlugin); ok {
		udpConf := &p.config.UDP
		if udpConf.ListenAddr != "" {
			if udpConf.AutoListen {
				if err = p.AddTask(udpConf.CreateUDPWork(p.Logger, udpHandler.OnUDPConnect)).WaitStarted(); err != nil {
					return
				}
			}
			p.SetDescription("udp", strings.TrimPrefix(udpConf.ListenAddr, ":"))
		}
	}

	if quicHandler, ok := p.handler.(IQUICPlugin); ok {
		quicConf := &p.config.Quic
		if quicConf.ListenAddr != "" {
			if quicConf.AutoListen {
				if err = p.AddTask(quicConf.CreateQUICWork(p.Logger, quicHandler.OnQUICConnect)).WaitStarted(); err != nil {
					return
				}
			}
			p.SetDescription("quic", strings.TrimPrefix(quicConf.ListenAddr, ":"))
		}
	}
	return
}

type WebHookQueueTask struct {
	task.Work
}

var webHookQueueTask WebHookQueueTask

type WebHookTask struct {
	task.Task
	plugin   *Plugin
	conf     config.Webhook
	data     any
	jsonData []byte
	alarm    AlarmInfo
}

func (t *WebHookTask) Start() error {
	if t.conf.URL == "" {
		return task.ErrTaskComplete
	}
	t.SetDescription("url", t.conf.URL)
	t.SetDescription("method", t.conf.Method)

	// 处理AlarmInfo数据
	if t.data != nil {
		// 获取主机名和IP地址
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown"
		}

		// 获取本机IP地址
		var ipAddr string
		addrs, err := net.InterfaceAddrs()
		if err == nil {
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
					if ipnet.IP.To4() != nil {
						ipAddr = ipnet.IP.String()
						break
					}
				}
			}
		}
		if ipAddr == "" {
			ipAddr = "unknown"
		}

		// 直接使用t.data作为AlarmInfo
		alarmInfo, ok := t.data.(AlarmInfo)
		if !ok {
			return fmt.Errorf("data is not of type AlarmInfo")
		}

		// 更新服务器信息
		if alarmInfo.ServerInfo == "" {
			alarmInfo.ServerInfo = fmt.Sprintf("%s (%s)", hostname, ipAddr)
		}

		// 确保时间戳已设置
		if alarmInfo.CreatedAt.IsZero() {
			alarmInfo.CreatedAt = time.Now()
		}
		if alarmInfo.UpdatedAt.IsZero() {
			alarmInfo.UpdatedAt = time.Now()
		}

		// 将AlarmInfo序列化为JSON
		jsonData, err := json.Marshal(alarmInfo)
		if err != nil {
			return fmt.Errorf("marshal AlarmInfo to json: %w", err)
		}

		t.jsonData = jsonData
		t.alarm = alarmInfo
		t.SetDescription("alarmType", fmt.Sprintf("0x%x", alarmInfo.AlarmType))
		t.SetDescription("alarmName", alarmInfo.AlarmName)
		if alarmInfo.StreamPath != "" {
			t.SetDescription("stream", alarmInfo.StreamPath)
		}
	}

	// 禁用 gotask 自动重试：自动重试的 sleep 发生在 EventLoop goroutine 内，
	// 会导致 EventLoop 阻塞，使后续所有任务堆积在 INIT 状态。
	// 重试逻辑已移入 Go()，在任务自己的 goroutine 里执行，不影响 EventLoop。
	t.SetRetry(0, 0)
	return nil
}

// doHTTPRequest 执行一次 HTTP webhook 请求，成功返回 nil
func (t *WebHookTask) doHTTPRequest(dbID uint) error {
	req, err := http.NewRequest(t.conf.Method, t.conf.URL, bytes.NewBuffer(t.jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.conf.Headers {
		req.Header.Set(k, v)
	}
	httpTimeout := time.Duration(t.conf.TimeoutSeconds) * time.Second
	if httpTimeout <= 0 {
		httpTimeout = 5 * time.Second
	}
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		t.plugin.Error("webhook请求失败", "error", err)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if t.conf.SaveAlarm && t.plugin.DB != nil && dbID > 0 {
			t.alarm.IsSent = true
			dbTimeout := httpTimeout
			dbCtx, dbCancel := context.WithTimeout(context.Background(), dbTimeout)
			if err := t.plugin.DB.WithContext(dbCtx).Model(&AlarmInfo{}).Where("id = ?", dbID).Update("is_sent", true).Error; err != nil {
				t.plugin.Error("更新告警发送状态失败", "error", err)
			} else {
				t.plugin.Info("告警发送状态已更新", "id", dbID, "is_sent", true)
			}
			dbCancel()
		}
		return nil
	}
	return fmt.Errorf("webhook响应状态码：%d", resp.StatusCode)
}

func (t *WebHookTask) Go() error {
	// 检查全局布防状态，撤防时不发送
	if !t.plugin.Server.ServerConfig.Armed {
		t.plugin.Debug("WebHook skipped due to disarmed state", "url", t.conf.URL)
		return task.ErrTaskComplete
	}

	// 仅首次保存告警到数据库（有超时保护）
	var dbID uint
	if t.conf.SaveAlarm && t.plugin.DB != nil {
		t.alarm.IsSent = false
		dbTimeout := time.Duration(t.conf.TimeoutSeconds) * time.Second
		if dbTimeout <= 0 {
			dbTimeout = 5 * time.Second
		}
		t.SetDescription("phase", "db")
		dbCtx, dbCancel := context.WithTimeout(context.Background(), dbTimeout)
		if err := t.plugin.DB.WithContext(dbCtx).Create(&t.alarm).Error; err != nil {
			t.plugin.Error("保存告警到数据库失败", "error", err)
			t.SetDescription("dbErr", err.Error())
		} else {
			dbID = t.alarm.ID
			t.plugin.Info("告警已保存到数据库", "id", dbID)
			t.SetDescription("dbID", fmt.Sprintf("%d", dbID))
		}
		dbCancel()
	}

	// 在本 goroutine 内做重试，sleep 不会阻塞 EventLoop
	maxRetry := t.conf.RetryTimes
	if maxRetry < 0 {
		maxRetry = 0
	}
	retryInterval := t.conf.RetryInterval
	if retryInterval <= 0 {
		retryInterval = time.Second
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetry; attempt++ {
		if attempt > 0 {
			delay := retryInterval
			if attempt > 1 {
				if exp := attempt - 1; exp < 30 {
					delay = retryInterval * time.Duration(1<<exp)
				}
			}
			t.SetDescription("phase", fmt.Sprintf("retry %d/%d (wait %s)", attempt, maxRetry, delay))
			select {
			case <-time.After(delay):
			case <-t.Context.Done():
				return task.ErrTaskComplete
			}
			t.plugin.Warn("webhook重试", "attempt", attempt, "maxRetry", maxRetry, "error", lastErr)
		} else {
			t.SetDescription("phase", "http")
		}
		lastErr = t.doHTTPRequest(dbID)
		if lastErr == nil {
			t.SetDescription("phase", "done")
			return task.ErrTaskComplete
		}
		t.SetDescription("lastErr", lastErr.Error())
	}
	if lastErr != nil && maxRetry > 0 {
		t.plugin.Error("webhook达到最大重试次数", "maxRetry", maxRetry, "error", lastErr)
	}
	t.SetDescription("phase", "failed")
	return task.ErrTaskComplete
}

func (p *Plugin) SendWebhook(conf config.Webhook, data any) *task.Task {
	webhookTask := &WebHookTask{
		plugin: p,
		conf:   conf,
		data:   data,
	}
	return webHookQueueTask.AddTask(webhookTask)
}

// TODO: use alias stream
func (p *Plugin) onPublish(pub *Publisher) {
	onPublish := p.config.OnPub
	if p.Meta.NewPusher != nil {
		for r, pushConf := range onPublish.Push {
			if pushConf.URL = r.Replace(pub.StreamPath, pushConf.URL); pushConf.URL != "" {
				p.Push(pub.StreamPath, pushConf, nil)
			}
		}
	}
	if p.Meta.NewRecorder != nil {
		for r, recConf := range onPublish.Record {
			if recConf.FilePath = r.Replace(pub.StreamPath, recConf.FilePath); recConf.FilePath != "" {
				p.recordOnPublish(pub, recConf)
			}
		}
	}
	var owner = pub.Value(Owner)
	var isTransformer bool
	if owner != nil {
		_, isTransformer = owner.(ITransformer)
	}
	if p.Meta.NewTransformer != nil && !isTransformer {
		for r, tranConf := range onPublish.Transform {
			if group := r.FindStringSubmatch(pub.StreamPath); group != nil {
				for j, to := range tranConf.Output {
					for i, g := range group {
						to.Target = strings.ReplaceAll(to.Target, fmt.Sprintf("$%d", i), g)
					}
					targetUrl, err := url.Parse(to.Target)
					if err == nil {
						to.StreamPath = strings.TrimPrefix(targetUrl.Path, "/")
					}
					tranConf.Output[j] = to
				}
				p.Transform(pub, tranConf)
			}
		}
	}
	if publishHookPlugin, ok := p.handler.(IPublishHookPlugin); ok {
		publishHookPlugin.OnPublish(pub)
	}
}

func (p *Plugin) auth(streamPath string, key string, secret string, expire string) (err error) {
	if unixTime, err := strconv.ParseInt(expire, 16, 64); err != nil || time.Now().Unix() > unixTime {
		return fmt.Errorf("auth failed expired")
	}
	if len(secret) != 32 {
		return fmt.Errorf("auth failed secret length must be 32")
	}
	trueSecret := md5.Sum([]byte(key + streamPath + expire))
	if secret == hex.EncodeToString(trueSecret[:]) {
		return nil
	}
	return fmt.Errorf("auth failed invalid secret")
}

func (p *Plugin) onSubscribe(streamPath string, args url.Values) {
	//	var avoidTrans bool
	//AVOID:
	//	for trans := range server.Transforms.Range {
	//		for _, output := range trans.Config.Output {
	//			if output.StreamPath == s.StreamPath {
	//				avoidTrans = true
	//				break AVOID
	//			}
	//		}
	//	}
	for reg, conf := range p.config.OnSub.Pull {
		if p.Meta.NewPuller != nil && reg.MatchString(streamPath) {
			conf.Args = config.HTTPValues(args)
			conf.URL = reg.Replace(streamPath, conf.URL)
			if job, err := p.handler.Pull(streamPath, conf, nil); err == nil {
				if w, ok := p.Server.Waiting.Get(streamPath); ok {
					job.Progress = &w.Progress
				}
			}
		}
	}
	if subscribeHookPlugin, ok := p.handler.(ISubscribeHookPlugin); ok {
		subscribeHookPlugin.OnSubscribe(streamPath, args)
	}
	//if !avoidTrans {
	//	for reg, conf := range plugin.GetCommonConf().OnSub.Transform {
	//		if plugin.Meta.Transformer != nil {
	//			if reg.MatchString(s.StreamPath) {
	//				if group := reg.FindStringSubmatch(s.StreamPath); group != nil {
	//					for j, c := range conf.Output {
	//						for i, value := range group {
	//							c.Target = strings.Replace(c.Target, fmt.Sprintf("$%d", i), value, -1)
	//						}
	//						conf.Output[j] = c
	//					}
	//				}
	//				plugin.handler.Transform(s.StreamPath, conf)
	//			}
	//		}
	//	}
	//}
}

func (p *Plugin) PublishWithConfig(ctx context.Context, streamPath string, conf config.Publish) (publisher *Publisher, err error) {
	publisher = &Publisher{Publish: conf}
	publisher.Type = conf.PubType
	publisher.ID = task.GetNextTaskID()
	publisher.Plugin = p
	if conf.PublishTimeout > 0 {
		publisher.TimeoutTimer = time.NewTimer(conf.PublishTimeout)
	} else {
		publisher.TimeoutTimer = time.NewTimer(time.Hour * 24 * 365)
	}
	publisher.Logger = p.Logger.With("streamPath", streamPath, "pId", publisher.ID)
	publisher.Init(streamPath, &publisher.Publish)
	if p.config.EnableAuth && publisher.Type == PublishTypeServer {
		onAuthPub := p.Meta.OnAuthPub
		if onAuthPub == nil {
			onAuthPub = p.Server.Meta.OnAuthPub
		}
		if onAuthPub != nil {
			if err = onAuthPub(publisher).Await(); err != nil {
				p.Warn("auth failed", "error", err)
				return
			}
		} else if conf.Key != "" {
			if err = p.auth(publisher.StreamPath, conf.Key, publisher.Args.Get("secret"), publisher.Args.Get("expire")); err != nil {
				p.Warn("auth failed", "error", err)
				return
			}
		}
	}
	for {
		err = p.Server.Streams.Add(publisher, ctx).WaitStarted()
		if err == nil {
			if sender, webhook := p.getHookSender(config.HookOnPublishEnd); sender != nil {
				publisher.OnDispose(func() {
					alarmInfo := AlarmInfo{
						AlarmName:  string(config.HookOnPublishEnd),
						AlarmDesc:  publisher.StopReason().Error(),
						AlarmType:  config.AlarmPublishOffline,
						StreamPath: publisher.StreamPath,
					}
					sender(webhook, alarmInfo)
				})
			}
			if sender, webhook := p.getHookSender(config.HookOnPublishStart); sender != nil {
				alarmInfo := AlarmInfo{
					AlarmName:  string(config.HookOnPublishStart),
					AlarmType:  config.AlarmPublishRecover,
					StreamPath: publisher.StreamPath,
				}
				sender(webhook, alarmInfo)
			}
			return
		} else if oldStream := new(task.ExistTaskError); errors.As(err, oldStream) {
			if conf.KickExist {
				publisher.takeOver(oldStream.Task.(*Publisher))
				oldStream.Task.WaitStopped()
			} else {
				return nil, ErrStreamExist
			}
		} else {
			return
		}
	}
}

func (p *Plugin) Publish(ctx context.Context, streamPath string) (publisher *Publisher, err error) {
	return p.PublishWithConfig(ctx, streamPath, p.config.Publish)
}

func (p *Plugin) SubscribeWithConfig(ctx context.Context, streamPath string, conf config.Subscribe) (subscriber *Subscriber, err error) {
	subscriber = createSubscriber(p, streamPath, conf)
	if p.config.EnableAuth && subscriber.Type == SubscribeTypeServer {
		onAuthSub := p.Meta.OnAuthSub
		if onAuthSub == nil {
			onAuthSub = p.Server.Meta.OnAuthSub
		}
		if onAuthSub != nil {
			if err = onAuthSub(subscriber).Await(); err != nil {
				p.Warn("auth failed", "error", err)
				return
			}
		} else if conf.Key != "" {
			if err = p.auth(subscriber.StreamPath, conf.Key, subscriber.Args.Get("secret"), subscriber.Args.Get("expire")); err != nil {
				p.Warn("auth failed", "error", err)
				return
			}
		}
	}
	err = p.Server.Streams.AddTask(subscriber, ctx).WaitStarted()
	if err == nil {
		// Use a self-timeout to prevent the subscriber from waiting forever
		// if CheckSubWaitTimeout is not running (e.g., its Tick panicked and
		// crashed the Streams event loop). The external checkTimeout is a
		// secondary safety net; this select timeout is the primary one.
		waitTimeout := conf.WaitTimeout
		if waitTimeout <= 0 {
			waitTimeout = 10 * time.Second
		}
		select {
		case <-subscriber.waitPublishDone:
			waitAudio := conf.WaitTrack == "all" || strings.Contains(conf.WaitTrack, "audio")
			waitVideo := conf.WaitTrack == "all" || strings.Contains(conf.WaitTrack, "video")
			err = subscriber.Publisher.WaitTrack(waitAudio, waitVideo)
		case <-subscriber.Done():
			err = subscriber.StopReason()
		case <-time.After(waitTimeout):
			subscriber.Stop(ErrSubscribeTimeout)
			err = ErrSubscribeTimeout
		}
	}
	if err == nil {
		if sender, webhook := p.getHookSender(config.HookOnSubscribeEnd); sender != nil {
			subscriber.OnDispose(func() {
				alarmInfo := AlarmInfo{
					AlarmName:  string(config.HookOnSubscribeEnd),
					AlarmDesc:  subscriber.StopReason().Error(),
					AlarmType:  config.AlarmSubscribeOffline,
					StreamPath: subscriber.StreamPath,
				}
				sender(webhook, alarmInfo)
			})
		}
		if sender, webhook := p.getHookSender(config.HookOnSubscribeStart); sender != nil {
			alarmInfo := AlarmInfo{
				AlarmName:  string(config.HookOnSubscribeStart),
				AlarmType:  config.AlarmSubscribeRecover,
				StreamPath: subscriber.StreamPath,
			}
			sender(webhook, alarmInfo)
		}
	}
	return
}

func (p *Plugin) Subscribe(ctx context.Context, streamPath string) (subscriber *Subscriber, err error) {
	return p.SubscribeWithConfig(ctx, streamPath, p.config.Subscribe)
}

func (p *Plugin) Pull(streamPath string, conf config.Pull, pubConf *config.Publish) (job *PullJob, err error) {
	puller := p.Meta.NewPuller(conf)
	if puller == nil {
		return nil, ErrNotFound
	}
	job = puller.GetPullJob()
	job.Init(puller, p, streamPath, conf, pubConf)
	// If AddTask rejected the job (e.g. ExistTaskError — a PullJob for this
	// streamPath is already active), propagate the error so callers don't
	// store a stopped job reference and lose track of the real active job.
	if job.IsStopped() {
		err = job.StopReason()
		p.Warn("[fix3] Plugin.Pull: AddTask rejected phantom job, blocking caller from overwriting real job", "streamPath", streamPath, "reason", err)
		job = nil
	}
	return
}

func (p *Plugin) Push(streamPath string, conf config.Push, subConf *config.Subscribe) {
	pusher := p.Meta.NewPusher()
	pusher.GetPushJob().Init(pusher, p, streamPath, conf, subConf)
}

// AddTaskRejected 报告 Job.AddTask 是否同步拒绝了任务，并在拒绝时返回 startup
// promise 的错误（即拒绝原因；可能为 nil）。
//
// gotask 的 AddTask 返回 *task.Task 而不是 error：拒绝时它把该任务的 startup
// promise 置为 rejected，任务永远不会进入 Start()/Go()。拒绝有两条路径：
//   - 父 Job 已停止：任务的 Context 随父上下文取消，IsStopped() 为真；
//   - 键冲突 / 事件循环输入通道满（ErrTooManyChildren）：任务被立刻从父 Job
//     的子任务表里删除，但 Context 仍然有效。
//
// 调用方必须为被拒任务预留的资源（名额、run 状态等）自行回收，否则会永久泄漏。
// 实现只依赖 context 状态与 sync.Map 成员关系，不读 Task.state（该字段由事件
// 循环 goroutine 无锁写入，跨 goroutine 读会触发 -race）。
//
// reason 取自已拒绝的 startup promise：WaitStarted 在 promise 已 reject 时立即
// 返回，不会阻塞（见下：只有“从未登记”的任务才会走到这里，而 AddTask 的每条
// 未登记路径都会同步 Reject）。StopReason() 在键冲突/通道满这两条路径上是 nil，
// 不能当原因用。
func AddTaskRejected(parent task.IJob, t *task.Task) (rejected bool, reason error) {
	if parent == nil || t == nil {
		return false, nil
	}
	registered := false
	parent.RangeSubTask(func(child task.ITask) bool {
		if child.GetTask() == t {
			registered = true
		}
		return true
	})
	// 仍登记在子任务表里 = AddTask 已接纳（可能正在运行或已启动待回收）。
	// 这里必须先判登记、再判父 Job 是否停止：父 Job 停止后它仍会等已接纳的
	// 子任务收尾，直接把“已接纳”误判成拒绝会触发错误的资源回收。
	if registered {
		return false, nil
	}
	if t.IsStopped() && !parent.IsStopped() {
		// 上下文已取消且父 Job 仍在跑：任务跑过并已从子任务表移除（dispose），
		// 不是 AddTask 的同步拒绝。
		return false, nil
	}
	// 未登记 = AddTask 同步拒绝（父 Job 已停止 / 键冲突 / 通道满）。
	// 这三条路径都在 AddTask 内部同步 Reject 了 startup promise，
	// 所以 WaitStarted 立即返回，不会阻塞。
	if err := t.WaitStarted(); err != nil {
		return true, err
	}
	return true, nil
}

// recordOnPublish 启动一次自动录像；若插件实现了 IRecordGatePlugin，
// 先由插件做并发准入判断。
func (p *Plugin) recordOnPublish(pub *Publisher, conf config.Record) {
	gate, gated := p.handler.(IRecordGatePlugin)
	if !gated {
		p.Record(pub, conf, nil)
		return
	}
	admission := gate.GateRecord(pub, conf)
	release := admission.Release
	switch admission.Result {
	case RecordGateAdmit:
		job := p.RecordWithDispose(pub, conf, nil, release)
		if release != nil {
			if job.GetTask().IsStopped() {
				// RecordJob 的 AddTask 已被同步拒绝（Records 已停止）：任务不会
				// 启动、dispose 也不会触发，必须在此归还名额，否则永久泄漏。
				release()
				p.Error("auto recording not started: record job rejected",
					"streamPath", pub.StreamPath, "filePath", conf.FilePath, "reason", job.GetTask().StopReason())
				return
			}
			// 自动录像不等待启动；若异步启动失败，用短命任务归还名额。
			watcher := &recordStartWatcher{job: job, onStartFailure: release}
			watcher.key = fmt.Sprintf("record-start-watch:%p", watcher)
			if rejected, reason := AddTaskRejected(p, p.AddTask(watcher, p.Logger)); rejected {
				// 没人观察启动结果。release 幂等且 stopOnce 保证最多归还一次：
				// 宁可短暂放宽上限（插件正在停止，录制很快也会停），也不能永久泄漏。
				release()
				p.Error("record start watcher rejected: released recording slot eagerly",
					"streamPath", pub.StreamPath, "filePath", conf.FilePath, "reason", reason)
			}
		}
	case RecordGateQueued:
		// 插件已保存录制参数，取得名额后会自行启动。这里只打 Debug：Queued 在
		// 限流启用时对每一路自动录像都会出现（有空闲名额也一样），不是饱和信号；
		// 真正的饱和由插件在排队时打 Info（含 active/waiting）与 Refused 的 Error。
		p.Debug("auto recording queued by gate", "streamPath", pub.StreamPath, "filePath", conf.FilePath)
	case RecordGateRefused:
		// 不得静默丢弃：记录完整参数，便于人工通过 API 补录。
		p.Error("auto recording refused by concurrency limit", "streamPath", pub.StreamPath, "filePath", conf.FilePath)
	default:
		// RecordGateUnset 或未知值：fail-closed，绝不启动录制。
		p.Error("auto recording skipped: record gate returned unset/unknown result",
			"streamPath", pub.StreamPath, "filePath", conf.FilePath, "result", int(admission.Result))
	}
}

// recordStartWatcher 是短命任务：录制任务异步启动失败时调用 onStartFailure
// （例如归还并发名额），成功后立即结束。避免在发布路径上同步等待启动。
type recordStartWatcher struct {
	task.Task
	job            *RecordJob
	onStartFailure func()
	key            string
}

func (w *recordStartWatcher) GetKey() string { return w.key }

func (w *recordStartWatcher) Go() error {
	if err := w.job.WaitStarted(); err != nil {
		w.onStartFailure()
		w.Error("record job failed to start", "error", err)
	}
	return task.ErrTaskComplete
}

func (p *Plugin) Record(pub *Publisher, conf config.Record, subConf *config.Subscribe) *RecordJob {
	return p.RecordWithDispose(pub, conf, subConf, nil)
}

// RecordWithDispose 与 Record 相同，但在录制任务入队前注册 onDispose 回调，
// 用于在录制生命周期结束时释放资源（如并发配额）。
//
// 必须使用本方法而不是 Record 之后再调用 job.OnDispose：gotask 的 dispose
// 监听器在任务开始前注册才会被消费，任务已进入 DISPOSING/DISPOSED 后再注册
// 永远不会触发（与 OnStart 的顺序约束同源）。
//
// 并发/生命周期约束：
//   - pub.Using(job) 是裸 append（Task.Using），publisher 的 dispose 会在它自己
//     的事件循环 goroutine 上遍历并截断同一 slice。调用方若不在 publisher 所属
//     goroutine 上，应先确认 pub.GetTask().IsStopped() 为假（见 plugin/mp4 的
//     pendingAutoRecordTask.Go）以缩小窗口；窗口本身由本方法末尾的“注册后复核”
//     兜底：复核发现 publisher 已停止就主动 Stop 该 job，由 dispose 回调回收资源。
//     注意不能用“订阅失败自愈”兜底——落在窗口里的 job 不会被 publisher stop，
//     而 recorder 是无限重试（SetRetry(-1,…)），它会一直活着并持有名额。
//   - RecordJob 的 AddTask 若被 Records 同步拒绝，任务不会启动、onDispose 也不会
//     触发：调用方必须检查返回值并在该路径显式释放资源（AddTaskRejected）。
func (p *Plugin) RecordWithDispose(pub *Publisher, conf config.Record, subConf *config.Subscribe, onDispose func()) *RecordJob {
	recorder := p.Meta.NewRecorder(conf)
	job := recorder.GetRecordJob()
	// 注册在 RecordJob 自身：只要任务成功启动，dispose 时必然回调；
	// 启动失败的任务不会进入 dispose，由调用方在 WaitStarted 错误路径显式释放。
	if onDispose != nil {
		job.OnDispose(onDispose)
	}
	// 注册后复核的“先手”：OnStart 在 RecordJob 自己的事件循环 goroutine 上运行，
	// 在这里 Stop 不会与 gotask 的 start() 并发写 task 字段（直接跨 goroutine
	// Stop 一个正在启动的任务会触发 gotask v1.0.5 的 Stop/start 数据竞争）。
	// 必须在 job.Init（即 AddTask）之前注册：OnStart 只在启动序列里消费一次。
	var started, stopRequested atomic.Bool
	job.OnStart(func() {
		started.Store(true)
		if stopRequested.Load() {
			job.GetTask().Stop(errPublisherStoppedDuringRegistration)
		}
	})
	job.Init(recorder, p, pub.StreamPath, conf, subConf)
	pub.Using(job)
	// 注册后复核：publisher 可能在上面的检查之后、Using 之前/期间停止，而它的
	// dispose 已经遍历完 resources（或即将截断 slice）——这个 job 就再也没人
	// stop。Records 只提供只读 Range/Find，没有别的停止入口；recorder 又是无限
	// 重试，孤儿 RecordJob 会永久持有资源（并发名额、文件句柄），直到进程重启。
	// 这里主动 Stop：Stop → dispose → OnDispose 回调（release 必须幂等，见
	// IRecordGatePlugin 文档），不会双释放。
	//   - 任务尚未启动：置位 stopRequested，由上面的 OnStart 在事件循环上 Stop；
	//   - 任务已启动：OnStart 已消费（started==true），直接 Stop（此时 start()
	//     已返回、字段写入对我们是可见的，无竞态）。
	if PublisherStopped(pub) {
		stopRequested.Store(true)
		if started.Load() {
			job.GetTask().Stop(errPublisherStoppedDuringRegistration)
		}
		p.Error("recording job stopped: publisher stopped during registration",
			"streamPath", pub.StreamPath, "filePath", conf.FilePath)
	}
	return job
}

func (p *Plugin) Transform(pub *Publisher, conf config.Transform) {
	transformer := p.Meta.NewTransformer()
	pub.Using(transformer.GetTransformJob().Init(transformer, p, pub, conf))
}

func (p *Plugin) registerHandler(handlers map[string]http.HandlerFunc) {
	t := reflect.TypeOf(p.handler)
	v := reflect.ValueOf(p.handler)
	// 注册http响应
	for i, j := 0, t.NumMethod(); i < j; i++ {
		name := t.Method(i).Name
		if name == "ServeHTTP" {
			continue
		}
		switch handler := v.Method(i).Interface().(type) {
		case func(http.ResponseWriter, *http.Request):
			patten := strings.ToLower(strings.ReplaceAll(name, "_", "/"))
			p.handle(patten, http.HandlerFunc(handler))
		}
	}
	for patten, handler := range handlers {
		p.handle(patten, handler)
	}
	if p.config.EnableAuth && p.Server.ServerConfig.Admin.EnableLogin {
		p.handle("/api/secret/{type}/{streamPath...}", http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(rw, "missing authorization header", http.StatusUnauthorized)
				return
			}

			tokenString := strings.TrimPrefix(authHeader, "Bearer ")
			_, err := p.Server.ValidateToken(tokenString)
			if err != nil {
				http.Error(rw, "invalid token", http.StatusUnauthorized)
				return
			}
			streamPath := r.PathValue("streamPath")
			t := r.PathValue("type")
			expire := r.URL.Query().Get("expire")
			switch t {
			case "publish":
				secret := md5.Sum([]byte(p.config.Publish.Key + streamPath + expire))
				rw.Write([]byte(hex.EncodeToString(secret[:])))
			case "subscribe":
				secret := md5.Sum([]byte(p.config.Subscribe.Key + streamPath + expire))
				rw.Write([]byte(hex.EncodeToString(secret[:])))
			}
		}))
	}
	if rootHandler, ok := p.handler.(http.Handler); ok {
		p.handle("/", rootHandler)
	}
}

func (p *Plugin) logHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p.Debug("visit", "path", r.URL.String(), "remote", r.RemoteAddr)
		name := strings.ToLower(p.Meta.Name)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/"+name)
		handler.ServeHTTP(rw, r)
	})
}

func (p *Plugin) handle(pattern string, handler http.Handler) {
	if p == nil {
		return
	}
	last := pattern == "/"
	if !strings.HasPrefix(pattern, "/") {
		pattern = "/" + pattern
	}
	handler = p.logHandler(handler)
	p.config.HTTP.Handle(pattern, handler, last)
	if p.Server != p.handler {
		pattern = "/" + strings.ToLower(p.Meta.Name) + pattern
		p.Debug("http handle added to Server", "pattern", pattern)
		p.Server.config.HTTP.Handle(pattern, handler, last)
	}
	p.Server.apiList = append(p.Server.apiList, pattern)
}

func (p *Plugin) getHookSender(hookType config.HookType) (sender func(webhook config.Webhook, data any) *task.Task, conf config.Webhook) {
	if p.config.Hook != nil {
		if _, ok := p.config.Hook[hookType]; ok {
			sender = p.SendWebhook
			conf = p.config.Hook[hookType]
		} else if _, ok := p.config.Hook[config.HookDefault]; ok {
			sender = p.SendWebhook
			conf = p.config.Hook[config.HookDefault]
		} else if p.Server.config.Hook != nil {
			if _, ok := p.Server.config.Hook[hookType]; ok {
				conf = p.Server.config.Hook[hookType]
				sender = p.Server.SendWebhook
			} else if _, ok := p.Server.config.Hook[config.HookDefault]; ok {
				sender = p.Server.SendWebhook
				conf = p.Server.config.Hook[config.HookDefault]
			}
		}
	}
	return
}

type ServerKeepAliveTask struct {
	task.TickTask
	plugin *Plugin
}

func (t *ServerKeepAliveTask) GetTickInterval() time.Duration {
	return time.Duration(t.plugin.config.Hook[config.HookOnServerKeepAlive].Interval) * time.Second
}

func (t *ServerKeepAliveTask) Tick(now any) {
	sender, webhook := t.plugin.getHookSender(config.HookOnServerKeepAlive)
	if sender == nil {
		return
	}
	//s := t.plugin.Server
	alarmInfo := AlarmInfo{
		AlarmName:  string(config.HookOnServerKeepAlive),
		AlarmType:  config.AlarmKeepAliveOnline,
		StreamPath: "",
	}
	sender(webhook, alarmInfo)
	//webhookData := map[string]interface{}{
	//	"event":           config.HookOnServerKeepAlive,
	//	"timestamp":       time.Now().Unix(),
	//	"streams":         s.Streams.Length,
	//	"subscribers":     s.Subscribers.Length,
	//	"publisherCount":  s.Streams.Length,
	//	"subscriberCount": s.Subscribers.Length,
	//	"uptime":          time.Since(s.StartTime).Seconds(),
	//}
	//sender(webhook, webhookData)
}
