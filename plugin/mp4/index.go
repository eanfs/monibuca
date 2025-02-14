package plugin_mp4

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"m7s.live/v5"
	v5 "m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/plugin/mp4/pb"
	pkg "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
	rtmp "m7s.live/v5/plugin/rtmp/pkg"
)

type MediaContext struct {
	io.Writer
	conn         net.Conn
	wto          time.Duration
	seqNumber    uint32
	muxer        *pkg.Muxer
	audio, video *pkg.Track
	buffer       []byte
	offset       int64
}

func (m *MediaContext) Write(p []byte) (n int, err error) {
	if m.conn != nil {
		m.conn.SetWriteDeadline(time.Now().Add(m.wto))
	}
	return m.Writer.Write(p)
}

func (m *MediaContext) Read(p []byte) (n int, err error) {
	if m.offset >= int64(len(m.buffer)) {
		return 0, io.EOF
	}
	n = copy(p, m.buffer[m.offset:])
	m.offset += int64(n)
	return
}

func (m *MediaContext) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		m.offset = offset
	case io.SeekCurrent:
		m.offset += offset
	case io.SeekEnd:
		m.offset = int64(len(m.buffer)) + offset
	}
	if m.offset < 0 {
		m.offset = 0
	}
	if m.offset > int64(len(m.buffer)) {
		m.offset = int64(len(m.buffer))
	}
	return m.offset, nil
}

type TrackContext struct {
	TrackId  uint32
	fragment *mp4.Fragment
	ts       uint32 // 每个小片段起始时间戳
	abs      uint32 // 绝对起始时间戳
	absSet   bool   // 是否设置过abs
}

func (m *TrackContext) Push(ctx *MediaContext, dt uint32, dur uint32, data []byte, flags uint32) {
	if !m.absSet {
		m.abs = dt
		m.absSet = true
	}
	dt -= m.abs
	if m.fragment != nil && dt-m.ts > 1000 {
		m.fragment.Encode(ctx)
		m.fragment = nil
	}
	if m.fragment == nil {
		ctx.seqNumber++
		m.fragment, _ = mp4.CreateFragment(ctx.seqNumber, m.TrackId)
		m.ts = dt
	}
	m.fragment.AddFullSample(mp4.FullSample{
		Data:       data,
		DecodeTime: uint64(dt),
		Sample: mp4.Sample{
			Flags: flags,
			Dur:   dur,
			Size:  uint32(len(data)),
		},
	})
}

type MP4Plugin struct {
	pb.UnimplementedApiServer
	m7s.Plugin
	BeforeDuration           time.Duration `default:"30s" desc:"事件录像提前时长，不配置则默认30s"`
	AfterDuration            time.Duration `default:"30s" desc:"事件录像结束时长，不配置则默认30s"`
	RecordFileExpireDays     int           `desc:"录像自动删除的天数,0或未设置表示不自动删除"`
	DiskMaxPercent           float64       `default:"90" desc:"硬盘使用百分之上限值，超上限后触发报警，并停止当前所有磁盘写入动作。"`
	AutoOverWriteDiskPercent float64       `default:"80" desc:"自动覆盖功能磁盘占用上限值，超过上限时连续录像自动删除日有录像，事件录像自动删除非重要事件录像，删除规则为删除距离当日最久日期的连续录像或非重要事件录像。"`
	ExceptionPostUrl         string        `desc:"第三方异常上报地址"`
	EventRecordFilePath      string        `desc:"事件录像存放地址"`
}

const defaultConfig m7s.DefaultYaml = `publish:
  speed: 1`

// var exceptionChannel = make(chan *Exception)
var _ = m7s.InstallPlugin[MP4Plugin](defaultConfig, &pb.Api_ServiceDesc, pb.RegisterApiHandler, pkg.NewPuller, pkg.NewRecorder)

func (p *MP4Plugin) RegisterHandler() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/download/{streamPath...}": p.download,
	}
}
func (p *MP4Plugin) OnInit() (err error) {
	if p.DB != nil {
		err = p.DB.AutoMigrate(&Exception{})
		var deleteRecordTask DeleteRecordTask
		deleteRecordTask.DB = p.DB
		deleteRecordTask.DiskMaxPercent = p.DiskMaxPercent
		deleteRecordTask.AutoOverWriteDiskPercent = p.AutoOverWriteDiskPercent
		deleteRecordTask.RecordFileExpireDays = p.RecordFileExpireDays
		p.AddTask(&deleteRecordTask)
	}
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
	streamPath := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".mp4")
	if r.URL.RawQuery != "" {
		streamPath += "?" + r.URL.RawQuery
	}
	sub, err := p.Subscribe(r.Context(), streamPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sub.RemoteAddr = r.RemoteAddr
	var ctx MediaContext
	ctx.conn, err = sub.CheckWebSocket(w, r)
	if err != nil {
		return
	}
	wto := p.GetCommonConf().WriteTimeout
	if ctx.conn == nil {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		if hijacker, ok := w.(http.Hijacker); ok && wto > 0 {
			ctx.conn, _, _ = hijacker.Hijack()
			ctx.conn.SetWriteDeadline(time.Now().Add(wto))
		}
	}

	if ctx.conn != nil {
		ctx.Writer = ctx.conn
	} else {
		ctx.Writer = w
		w.(http.Flusher).Flush()
	}

	ctx.wto = p.GetCommonConf().WriteTimeout
	ctx.muxer = pkg.NewMuxer(pkg.FLAG_FRAGMENT)
	ctx.muxer.WriteInitSegment(ctx.Writer)
	var offsetAudio, offsetVideo = 1, 5

	if sub.Publisher.HasVideoTrack() {
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
		ctx.video = ctx.muxer.AddTrack(codecID)
		ctx.video.Timescale = 1000

		switch v.ICodecCtx.FourCC() {
		case codec.FourCC_H264:
			h264Ctx := v.ICodecCtx.GetBase().(*codec.H264Ctx)
			ctx.video.ExtraData = h264Ctx.Record
			ctx.video.Width = uint32(h264Ctx.Width())
			ctx.video.Height = uint32(h264Ctx.Height())
		case codec.FourCC_H265:
			h265Ctx := v.ICodecCtx.GetBase().(*codec.H265Ctx)
			ctx.video.ExtraData = h265Ctx.Record
			ctx.video.Width = uint32(h265Ctx.Width())
			ctx.video.Height = uint32(h265Ctx.Height())
		}
	}

	if sub.Publisher.HasAudioTrack() {
		a := sub.Publisher.AudioTrack.AVTrack
		if err = a.WaitReady(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var codecID box.MP4_CODEC_TYPE
		switch a.ICodecCtx.FourCC() {
		case codec.FourCC_MP4A:
			codecID = box.MP4_CODEC_AAC
		}
		ctx.audio = ctx.muxer.AddTrack(codecID)
		ctx.audio.Timescale = 1000
		audioCtx := a.ICodecCtx.(v5.IAudioCodecCtx)
		ctx.audio.SampleRate = uint32(audioCtx.GetSampleRate())
		ctx.audio.ChannelCount = uint8(audioCtx.GetChannels())
		ctx.audio.SampleSize = uint16(audioCtx.GetSampleSize())

		switch a.ICodecCtx.FourCC() {
		case codec.FourCC_MP4A:
			offsetAudio = 2
			ctx.audio.ExtraData = a.ICodecCtx.GetBase().(*codec.AACCtx).ConfigBytes
		default:
			offsetAudio = 1
		}
	}

	err = ctx.muxer.WriteInitSegment(&ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	m7s.PlayBlock(sub, func(audio *rtmp.RTMPAudio) error {
		bs := audio.Memory.ToBytes()
		if offsetAudio == 2 && bs[1] == 0 {
			return nil
		}
		sample := box.Sample{
			Offset:    0,
			Data:      bs[offsetAudio:],
			Size:      len(bs) - offsetAudio,
			Timestamp: audio.Timestamp,
			KeyFrame:  true,
		}
		ctx.audio.AddSampleEntry(sample)
		return nil
	}, func(video *rtmp.RTMPVideo) error {
		bs := video.Memory.ToBytes()
		if ctx, ok := sub.VideoReader.Track.ICodecCtx.(*rtmp.H265Ctx); ok && ctx.Enhanced && bs[0]&0b1111 == rtmp.PacketTypeCodedFrames {
			offsetVideo = 8
		} else {
			offsetVideo = 5
		}
		sample := box.Sample{
			Offset:    0,
			Data:      bs[offsetVideo:],
			Size:      len(bs) - offsetVideo,
			Timestamp: video.Timestamp,
			CTS:       video.CTS,
			KeyFrame:  sub.VideoReader.Value.IDR,
		}
		ctx.video.AddSampleEntry(sample)
		return nil
	})
}
