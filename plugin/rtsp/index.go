package plugin_rtsp

import (
	"fmt"
	"net"
	"strings"

	"m7s.live/v5/pkg/task"

	"m7s.live/v5"
	. "m7s.live/v5/plugin/rtsp/pkg"
)

const defaultConfig = m7s.DefaultYaml(`tcp:
  listenaddr: :554`)

var _ = m7s.InstallPlugin[RTSPPlugin](defaultConfig, NewPuller, NewPusher)

type RTSPPlugin struct {
	m7s.Plugin
}

func (p *RTSPPlugin) OnTCPConnect(conn *net.TCPConn) task.ITask {
	ret := &RTSPServer{NetConnection: NewNetConnection(conn), conf: p}
	ret.Logger = p.With("remote", conn.RemoteAddr().String())
	return ret
}

func (p *RTSPPlugin) OnPullProxyAdd(pullProxy *m7s.PullProxy) any {
	ret := &RTSPPullProxy{}
	ret.PullProxy = pullProxy
	ret.Plugin = &p.Plugin
	ret.Logger = p.With("pullProxy", pullProxy.Name)
	return ret
}

func (p *RTSPPlugin) OnPushProxyAdd(pushProxy *m7s.PushProxy) any {
	ret := &RTSPPushProxy{}
	ret.PushProxy = pushProxy
	ret.Plugin = &p.Plugin
	ret.Logger = p.With("pushProxy", pushProxy.Name)
	return ret
}

func (p *RTSPPlugin) OnInit() (err error) {
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
	return
}
