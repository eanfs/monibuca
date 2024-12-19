package plugin_rtsp

import (
	"fmt"
	"net"
	"net/url"
	"time"

	"m7s.live/v5"
	"m7s.live/v5/pkg/util"
	. "m7s.live/v5/plugin/rtsp/pkg"
)

type RTSPPullProxy struct {
	m7s.TCPPullProxy
	conn Stream
}

func (d *RTSPPullProxy) Start() (err error) {
	d.URL, err = url.Parse(d.PullProxy.URL)
	if err != nil {
		return
	}
	if ips, err := net.LookupIP(d.URL.Hostname()); err != nil {
		return err
	} else if len(ips) == 0 {
		return fmt.Errorf("no IP found for host: %s", d.URL.Hostname())
	} else {
		d.TCPAddr, err = net.ResolveTCPAddr("tcp", net.JoinHostPort(ips[0].String(), d.URL.Port()))
		if err != nil {
			return err
		}
		if d.TCPAddr.Port == 0 {
			d.TCPAddr.Port = 554 // Default RTSP port
		}
	}

	d.conn.NetConnection = &NetConnection{
		MemoryAllocator: util.NewScalableMemoryAllocator(1 << 12),
		UserAgent:       "monibuca" + m7s.Version,
	}
	d.conn.Logger = d.Plugin.Logger
	return d.TCPPullProxy.Start()
}

func (d *RTSPPullProxy) GetTickInterval() time.Duration {
	return time.Second * 5
}

func (d *RTSPPullProxy) Tick(any) {
	switch d.PullProxy.Status {
	case m7s.PullProxyStatusOffline:
		err := d.conn.Connect(d.PullProxy.URL)
		if err != nil {
			return
		}
		d.PullProxy.ChangeStatus(m7s.PullProxyStatusOnline)
	case m7s.PullProxyStatusOnline, m7s.PullProxyStatusPulling:
		t := time.Now()
		err := d.conn.Options()
		d.PullProxy.RTT = time.Since(t)
		if err != nil {
			d.PullProxy.ChangeStatus(m7s.PullProxyStatusOffline)
		}
	}
}
