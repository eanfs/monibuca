package plugin_mp4

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"m7s.live/v5"
	"m7s.live/v5/pb"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
	mp4pb "m7s.live/v5/plugin/mp4/pb"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

type MP4Plugin struct {
	mp4pb.UnimplementedApiServer
	m7s.Plugin
	BeforeDuration       time.Duration `default:"30s" desc:"事件录像提前时长，不配置则默认30s"`
	AfterDuration        time.Duration `default:"30s" desc:"事件录像结束时长，不配置则默认30s"`
	RecordFileExpireDays int           `default:"0" desc:"录像自动删除的天数,0或未设置表示不自动删除"`
	DiskMaxPercent       float64       `default:"90" desc:"硬盘使用百分之上限值，超上限后触发报警，并停止当前所有磁盘写入动作。"`
	OverwritePercent     float64       `default:"0" desc:"全局磁盘使用率阈值，当 storage.local 的 overwritepercent 为 0 时使用此值作为全局兼底配置。超过阈值时自动迁移或删除最旧文件。"`
	AutoRecovery         bool          `default:"false" desc:"是否自动恢复"`
	ExceptionPostUrl     string        `desc:"第三方异常上报地址"`
	EventRecordFilePath  string        `desc:"事件录像存放地址"`

	// RecordConfig 是录像相关配置（yaml: mp4.record）。
	// 字段不能命名为 Record：会遮蔽 Plugin.Record() 方法。
	RecordConfig struct {
		Index struct {
			Enable        bool          `default:"true" desc:"是否写伴随索引（崩溃恢复的前提）"`
			FlushInterval time.Duration `default:"1s" desc:"伴随索引自动落盘间隔"`
		} `desc:"录像伴随索引配置"`
		Recovery struct {
			OnStart     bool          `default:"true" desc:"启动时是否自动扫描并定稿残留录像（不影响 AutoRecovery 的 DB 补录）"`
			MinAge      time.Duration `default:"5m" desc:"仅用于手动恢复(API/CLI)：sidecar/媒体 mtime 新于该阈值时视为仍在录制并跳过；实际生效值不小于 max(5s, 2×index.flushinterval)；启动自动扫描不受该值影响，固定使用 max(5s, 2×index.flushinterval) 短阈值以保证崩溃重启后自动恢复；本进程内的活跃 recorder 无论 mtime 均跳过；负数关闭保护"`
			Concurrency int           `default:"2" desc:"恢复定稿并发上限"`
			Dirs        []string      `desc:"额外的恢复扫描目录，空则使用录制临时目录 + pending_uploads + 录像输出目录"`
			// AllowUnauthenticated 控制未启用 admin.enablelogin 时
			// POST /api/recovery/run 是否接受任意来源的请求。默认 false：
			// 仅允许本地回环请求，避免任意本机进程借无鉴权端点触发恢复。
			AllowUnauthenticated bool `default:"false" desc:"未启用 admin.enablelogin 时是否允许任意来源调用 POST /api/recovery/run；false 时仅允许本地回环请求"`
		} `desc:"录像崩溃恢复配置"`
	} `yaml:"record" desc:"录像配置"`

	// Limits 是录制资源软保护配置（yaml: mp4.limits）。
	Limits struct {
		MaxConcurrentRecordings int `default:"0" desc:"最大并发录制数，0 表示不限制。超限时请求进入有界等待队列（容量=上限×4），排队超过 30s 返回 503；自动录像(onpub/pull_on_start)、事件录像与 GB28181 下载录像共用该名额，自动录像排队超限时记录 Error 并保留参数。"`
		MaxMemoryMB             int `default:"0" desc:"录制内存预算(MB)，0 表示不限制。当前版本仅解析并预留，尚未强制执行。"`
	} `yaml:"limits" desc:"录制资源软保护配置"`

	// recordLimiter 在 Start 时按 Limits 初始化；为 nil 时按不限制处理。
	recordLimiter atomic.Pointer[RecordLimiter]

	// recoveryTask 供 /api/recovery/* 手动触发与状态查询；Start 时存入。
	// 用 atomic 是因为 HTTP handler 可能先于/并发于插件启动被调用。
	recoveryTask atomic.Pointer[IndexRecoveryTask]
	// recoveryState 是恢复 API 的运行时状态（运行历史/最近恢复结果）。
	recoveryState recoveryAPIState
}

const defaultConfig m7s.DefaultYaml = `publish:
  speed: 1`

// DownloadHook 由 cluster 插件 Start 时注入。
// 返回非空 target(scheme+host 前缀,如 "http://10.0.0.5:8080")时,
// /download handler 302 到 target;返回 ("", false) 则走本地下载逻辑。
// 单机部署为 nil。
var DownloadHook func(streamPath string) (target string, ok bool)

// var exceptionChannel = make(chan *Exception)
var _ = m7s.InstallPlugin[MP4Plugin](m7s.PluginMeta{
	DefaultYaml:         defaultConfig,
	ServiceDesc:         &mp4pb.Api_ServiceDesc,
	RegisterGRPCHandler: mp4pb.RegisterApiHandler,
	NewPuller:           mp4pkg.NewPuller,
	NewRecorder:         mp4pkg.NewRecorder,
	NewPullProxy:        m7s.NewHTTPPullPorxy,
})

func init() {
	// Register MP4 control-plane methods that can be routed to the node hosting the stream.
	m7s.RegisterAPIRouteUnary("/mp4.api/List", func() any { return new(pb.RecordResponseList) })
	m7s.RegisterAPIRouteUnary("/mp4.api/Delete", func() any { return new(pb.ResponseDelete) })
	m7s.RegisterAPIRouteUnary("/mp4.api/EventStart", func() any { return new(mp4pb.ResponseEventRecord) })
	m7s.RegisterAPIRouteUnary("/mp4.api/StartRecord", func() any { return new(mp4pb.ResponseStartRecord) })
	m7s.RegisterAPIRouteUnary("/mp4.api/StopRecord", func() any { return new(mp4pb.ResponseStopRecord) })
}

func (p *MP4Plugin) RegisterHandler() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/download/{streamPath...}":           p.download,
		"/extract/compressed/{streamPath...}": p.extractCompressedVideoHandel,
		"/extract/gop/{streamPath...}":        p.extractGopVideoHandel,
		"/snap/{streamPath...}":               p.snapHandel,
		"/api/upload/exhausted":               p.handleListExhaustedUploads,
		"/api/upload/retry":                   p.handleRetryUpload,
		"/api/recovery/list":                  p.handleRecoveryList,
		"/api/recovery/run":                   p.handleRecoveryRun,
		"/api/recovery/status/{id}":           p.handleRecoveryStatus,
	}
}

func (p *MP4Plugin) Start() (err error) {
	// 录制并发软保护：0 表示不限制，保持既有行为（无队列、无拒绝）。
	p.recordLimiter.Store(NewRecordLimiter(p.Limits.MaxConcurrentRecordings))
	if p.Limits.MaxMemoryMB > 0 {
		// 预留配置：当前版本只解析、记录，不执行内存预算判定。
		p.Warn("limits.maxmemorymb is configured but not enforced yet",
			"maxMemoryMB", p.Limits.MaxMemoryMB)
	}

	// 录制器在 pkg 包内，无法直接读取插件配置；在录制开始前注入。
	mp4pkg.SetIndexConfig(mp4pkg.IndexConfig{
		Enable:        p.RecordConfig.Index.Enable,
		FlushInterval: p.RecordConfig.Index.FlushInterval,
	})

	if p.DB != nil {
		err = p.DB.AutoMigrate(&Exception{})
		if err != nil {
			return
		}
		err = p.DB.AutoMigrate(&mp4pkg.TagModel{})
		if err != nil {
			return
		}
		if p.OverwritePercent > 0 {
			var storageTask StorageManagementTask
			storageTask.DB = p.DB
			storageTask.DiskMaxPercent = p.DiskMaxPercent
			storageTask.OverwritePercent = p.OverwritePercent
			storageTask.RecordFileExpireDays = p.RecordFileExpireDays
			storageTask.plugin = p
			p.AddTask(&storageTask)
		}
	}

	// 恢复任务合并了旧 RecordRecoveryTask：启动扫描先补 DB 记录（AutoRecovery），
	// 再定稿 sidecar。索引定稿不依赖 DB，因此即使 DB 未启用也会运行。
	// onstart=false 只关闭索引扫描；AutoRecovery 的 DB 补录仍在任务 Start 中执行。
	//
	// 任务始终注册：/api/recovery/run 需要在关闭启动扫描时仍能手动触发，
	// 同时保留运行状态供 /api/recovery/status 查询。空闲的 Work 无额外开销。
	recoveryTask := &IndexRecoveryTask{
		DB:            p.DB,
		plugin:        p,
		Dirs:          p.RecordConfig.Recovery.Dirs,
		Concurrency:   p.RecordConfig.Recovery.Concurrency,
		MinAge:        p.RecordConfig.Recovery.MinAge,
		RunOnStart:    p.RecordConfig.Recovery.OnStart,
		RunDBBackfill: p.AutoRecovery && p.DB != nil,
	}
	// 按 sidecar 记录的存储类型解析上传后端：用本 server 的 registry 且
	// 绝不回落到当前活跃存储（S3 不可用时也不写 local）。
	if server := p.Server; server != nil {
		recoveryTask.ResolveUploadStorage = server.GetStorageForType
	}
	// observer 必须在任务启动前设置（之后只读）。
	recoveryTask.observer = p.observeRecovery
	p.recoveryTask.Store(recoveryTask)
	p.AddTask(recoveryTask)
	// go func() { //处理所有异常，录像中断异常、录像读取异常、录像导出文件中断、磁盘容量低于阈值异常、磁盘异常
	// 	for exception := range exceptionChannel {
	// 		p.SendToThirdPartyAPI(exception)
	// 	}
	// }()
	_, port, _ := strings.Cut(p.GetCommonConf().HTTP.ListenAddr, ":")
	if port == "80" {
		p.PlayAddr = append(p.PlayAddr, "http://{hostName}/mp4/{streamPath}.mp4")
	} else if port != "" {
		p.PlayAddr = append(p.PlayAddr, fmt.Sprintf("http://{hostName}:%s/mp4/{streamPath}.mp4", port))
	}
	_, port, _ = strings.Cut(p.GetCommonConf().HTTP.ListenAddrTLS, ":")
	if port == "443" {
		p.PlayAddr = append(p.PlayAddr, "https://{hostName}/mp4/{streamPath}.mp4")
	} else if port != "" {
		p.PlayAddr = append(p.PlayAddr, fmt.Sprintf("https://{hostName}:%s/mp4/{streamPath}.mp4", port))
	}
	return
}

func (p *MP4Plugin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	redirectPath := strings.TrimPrefix(r.URL.Path, "/")
	if p.Server != nil && p.Server.RedirectIfNeeded(w, r, "mp4", redirectPath) {
		p.Debug("redirect issued", "protocol", "http", "path", redirectPath)
		return
	}
	streamPath := strings.TrimSuffix(redirectPath, ".mp4")
	if r.URL.RawQuery != "" {
		streamPath += "?" + r.URL.RawQuery
	}
	sub, err := p.Subscribe(r.Context(), streamPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sub.RemoteAddr = r.RemoteAddr
	var ctx util.HTTP_WS_Writer
	ctx.Conn, err = sub.CheckWebSocket(w, r)
	if err != nil {
		return
	}
	ctx.WriteTimeout = p.GetCommonConf().WriteTimeout
	ctx.ContentType = "video/mp4"
	ctx.ServeHTTP(w, r)

	muxer := mp4pkg.NewMuxer(mp4pkg.FLAG_FRAGMENT)
	err = muxer.WriteInitSegment(&ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var audio, video *mp4pkg.Track
	var nextFragmentId uint32
	if sub.Publisher.HasVideoTrack() && sub.SubVideo {
		v := sub.Publisher.VideoTrack.AVTrack
		if err = v.WaitReady(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var codecID box.MP4_CODEC_TYPE
		switch v.ICodecCtx.FourCC() {
		case codec.FourCC_H264:
			codecID = box.MP4_CODEC_H264
		case codec.FourCC_H265:
			codecID = box.MP4_CODEC_H265
		}
		video = muxer.AddTrack(codecID)
		video.Timescale = 1000
		video.Samplelist = []box.Sample{
			{
				KeyFrame: true,
			},
		}
		video.ICodecCtx = v.ICodecCtx
	}

	if sub.Publisher.HasAudioTrack() && sub.SubAudio {
		a := sub.Publisher.AudioTrack.AVTrack
		if err = a.WaitReady(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var codecID box.MP4_CODEC_TYPE
		switch a.ICodecCtx.FourCC() {
		case codec.FourCC_MP4A:
			codecID = box.MP4_CODEC_AAC
		case codec.FourCC_ALAW:
			codecID = box.MP4_CODEC_G711A
		case codec.FourCC_ULAW:
			codecID = box.MP4_CODEC_G711U
		case codec.FourCC_OPUS:
			codecID = box.MP4_CODEC_OPUS
		}
		audio = muxer.AddTrack(codecID)
		audio.Timescale = 1000
		audio.ICodecCtx = a.ICodecCtx
		audio.Samplelist = []box.Sample{
			{
				KeyFrame: true,
			},
		}
	}
	err = muxer.WriteMoov(&ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx.Flush()
	m7s.PlayBlock(sub, func(frame *mp4pkg.AudioFrame) (err error) {
		if audio.Samplelist[0].Buffers != nil {
			audio.Samplelist[0].Duration = sub.AudioReader.AbsTime - audio.Samplelist[0].Timestamp
			nextFragmentId++
			// Create moof box for this track
			moof := audio.MakeMoof(nextFragmentId)
			// Create mdat box for this track
			mdat := box.CreateMemoryBox(box.TypeMDAT, audio.Samplelist[0].Memory)
			box.WriteTo(&ctx, moof, mdat)
			err = ctx.Flush()
		}
		audio.Samplelist[0].Timestamp = sub.AudioReader.AbsTime
		audio.Samplelist[0].Memory = frame.Memory
		return
	}, func(frame *mp4pkg.VideoFrame) (err error) {
		if video.Samplelist[0].Buffers != nil {
			video.Samplelist[0].Duration = sub.VideoReader.AbsTime - video.Samplelist[0].Timestamp
			nextFragmentId++
			// Create moof box for this track
			moof := video.MakeMoof(nextFragmentId)
			// Create mdat box for this track
			mdat := box.CreateMemoryBox(box.TypeMDAT, video.Samplelist[0].Memory)
			box.WriteTo(&ctx, moof, mdat)
			err = ctx.Flush()
		}
		video.Samplelist[0].Memory = frame.Memory
		video.Samplelist[0].Timestamp = sub.VideoReader.AbsTime
		video.Samplelist[0].CTS = uint32(frame.CTS / time.Millisecond)
		video.Samplelist[0].KeyFrame = sub.VideoReader.Value.IDR
		return
	})
}
