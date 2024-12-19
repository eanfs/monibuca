package plugin_rtmp

import (
	"fmt"
	"net"
	"net/url"

	"m7s.live/v5"
)

type RTMPPushProxy struct {
	m7s.TCPPushProxy
}

func (d *RTMPPushProxy) Start() (err error) {
	d.URL, err = url.Parse(d.PushProxy.URL)
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
			if d.URL.Scheme == "rtmps" {
				d.TCPAddr.Port = 443
			} else {
				d.TCPAddr.Port = 1935
			}
		}
	}
	return d.TCPPushProxy.Start()
}
