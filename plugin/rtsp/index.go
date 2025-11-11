package plugin_rtsp

import (
	"fmt"
	"net"
	"strings"
	"sync"

	task "github.com/langhuihui/gotask"
	"m7s.live/v5/pkg/util"

	"m7s.live/v5"
	. "m7s.live/v5/plugin/rtsp/pkg"
)

var _ = m7s.InstallPlugin[RTSPPlugin](m7s.PluginMeta{
	DefaultYaml: `tcp:
  listenaddr: :554`,
	NewPuller:    NewPuller,
	NewPusher:    NewPusher,
	NewPullProxy: NewPullProxy,
	NewPushProxy: NewPushProxy,
})

type RTSPPlugin struct {
	m7s.Plugin
	UserName        string             `desc:"用户名"`
	Password        string             `desc:"密码"`
	UdpPort         util.Range[uint16] `default:"20001-30000" desc:"媒体端口范围"` //媒体端口范围
	udpPorts        chan uint16
	advisorOnce     sync.Once
	redirectAdvisor rtspRedirectAdvisor
}

type rtspRedirectAdvisor interface {
	ShouldRedirectRTSP(streamPath, currentHost string) (string, bool)
}

func (p *RTSPPlugin) OnTCPConnect(conn *net.TCPConn) task.ITask {
	ret := &RTSPServer{NetConnection: NewNetConnection(conn), conf: p}
	ret.Logger = p.Logger.With("remote", conn.RemoteAddr().String())
	return ret
}

func (p *RTSPPlugin) Start() (err error) {
	if tcpAddr := p.GetCommonConf().TCP.ListenAddr; tcpAddr != "" {
		_, port, _ := strings.Cut(tcpAddr, ":")
		if port == "554" {
			p.PlayAddr = append(p.PlayAddr, "rtsp://{hostName}/{streamPath}")
			p.PushAddr = append(p.PushAddr, "rtsp://{hostName}/{streamPath}")
		} else {
			p.PlayAddr = append(p.PlayAddr, fmt.Sprintf("rtsp://{hostName}:%s/{streamPath}", port))
			p.PushAddr = append(p.PushAddr, fmt.Sprintf("rtsp://{hostName}:%s/{streamPath}", port))
		}
	}

	// 初始化UDP端口池
	p.initUDPPortPool()
	return
}

func (p *RTSPPlugin) findRedirectAdvisor() rtspRedirectAdvisor {
	p.advisorOnce.Do(func() {
		for plugin := range p.Server.Plugins.Range {
			if advisor, ok := plugin.GetHandler().(rtspRedirectAdvisor); ok {
				p.redirectAdvisor = advisor
				break
			}
		}
	})
	return p.redirectAdvisor
}

// 初始化UDP端口池
func (p *RTSPPlugin) initUDPPortPool() {
	if p.UdpPort.Valid() {
		p.SetDescription("tcp", fmt.Sprintf("%d-%d", p.UdpPort[0], p.UdpPort[1]))
		p.udpPorts = make(chan uint16, p.UdpPort.Size())
		for i := range p.UdpPort.Size() {
			p.udpPorts <- p.UdpPort[0] + i
		}
	} else {
		p.Error("udp ports cannot init")
		//p.SetDescription("tcp", fmt.Sprintf("%d", p.UdpPort[0]))
		//tcpConfig := &p.GetCommonConf().TCP
		//tcpConfig.ListenAddr = fmt.Sprintf(":%d", p.UdpPort[0])
	}
}

// 获取一个可用的UDP端口对(RTP端口和RTCP端口)
func (p *RTSPPlugin) GetUDPPort() (udpPort uint16, err error) {
	if p.UdpPort.Valid() {
		select {
		case udpPort = <-p.udpPorts:
			defer func() {
				p.udpPorts <- udpPort
			}()
		default:
			err = fmt.Errorf("no available tcp port")
		}
	} else {
		udpPort = p.UdpPort[0]
	}
	return
}
