package rtsp

import (
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/langhuihui/gomem"
	"m7s.live/v5"
)

// probeFailureLogEvery 探活失败日志除首条外每 N 次告警一次，其余降为 Debug。
const probeFailureLogEvery = 60

func NewPullProxy() m7s.IPullProxy {
	return &RTSPPullProxy{}
}

type RTSPPullProxy struct {
	m7s.TCPPullProxy
	conn          Stream
	probeFailures int
}

func (d *RTSPPullProxy) Start() (err error) {
	d.URL, err = url.Parse(d.PullProxyConfig.URL)
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
		MemoryAllocator: gomem.NewScalableMemoryAllocator(1 << 12),
		UserAgent:       "monibuca" + m7s.Version,
	}
	d.conn.Logger = d.Logger
	return d.TCPPullProxy.Start()
}

func (d *RTSPPullProxy) Dispose() {
	if nc := d.conn.NetConnection; nc != nil {
		if nc.Conn != nil {
			_ = d.conn.Teardown()
		}
		// Conn 可能已被探活失败逻辑关闭并置空，但 BufReader/MemoryAllocator 仍需回收。
		nc.Dispose()
		d.conn.NetConnection = nil
	}
	d.TCPPullProxy.Dispose()
	d.Info("RTSP pull proxy disposed and all resources cleaned up")
}

func (d *RTSPPullProxy) GetTickInterval() time.Duration {
	return time.Second * 5
}

func (d *RTSPPullProxy) applyReadTimeout() {
	to := d.ReadTimeout
	if to <= 0 {
		to = DefaultReadTimeout
	}
	d.conn.ReadTimeout = to
}

func (d *RTSPPullProxy) Tick(any) {
	// {{ AURA-X: Add - 鉴权失败停重试后不再用错误凭据做 OPTIONS/Connect 探活，避免设备锁定. Confirmed via 寸止. }}
	if d.StopRetryOnAuthFail && d.GetBase().IsAuthFailed() {
		return
	}
	var err error
	d.applyReadTimeout()
	switch d.Status {
	case m7s.PullProxyStatusOffline:
		err = d.conn.Connect(d.Context, d.PullProxyConfig.URL)
		if err != nil {
			return
		}
		d.ChangeStatus(m7s.PullProxyStatusOnline)
	case m7s.PullProxyStatusOnline:
		if d.conn.Conn == nil {
			err = d.conn.Connect(d.Context, d.PullProxyConfig.URL)
		} else {
			t := time.Now()
			err = d.conn.Options()
			d.RTT = time.Since(t)
		}
		if err != nil {
			d.ChangeStatus(m7s.PullProxyStatusOffline)
		}
	case m7s.PullProxyStatusPulling:
		// When Pulling, the active PullJob manages the RTSP connection.
		// Only perform a passive heartbeat; do NOT reset to Offline on
		// failure, as that would corrupt the Pulling→Online lifecycle
		// used by processPullProxyOnDispose/OnStart to track recording state.
		// 取局部变量，避免 Dispose 并发把 d.conn.NetConnection 置空后继续解引用。
		nc := d.conn.NetConnection
		if nc == nil {
			return
		}
		if nc.Conn == nil {
			d.reconnectProbe(nc)
			return
		}
		// 用局部副本发 OPTIONS，避免并发 Dispose 把 d.conn.NetConnection 置空后解引用。
		probe := Stream{NetConnection: nc}
		t := time.Now()
		err = probe.Options()
		d.RTT = time.Since(t)
		if err != nil {
			d.closeProbeConn(nc)
			d.reportProbeFailure(err)
		} else {
			d.probeFailures = 0
		}
	}
}

// reconnectProbe 重建被对端关闭的探活连接。
// 只影响探活，不触碰 PullJob 的媒体连接，也不改变 Pulling 状态。
func (d *RTSPPullProxy) reconnectProbe(nc *NetConnection) {
	if d.IsStopped() {
		return
	}
	if err := nc.Connect(d.Context, d.PullProxyConfig.URL); err != nil {
		d.reportProbeFailure(err)
		return
	}
	if d.probeFailures > 0 {
		d.Info("pull proxy probe connection recovered", "failures", d.probeFailures)
	}
	d.probeFailures = 0
	d.RTT = 0
}

func (d *RTSPPullProxy) closeProbeConn(nc *NetConnection) {
	d.RTT = 0
	if nc.Conn == nil {
		return
	}
	_ = nc.Conn.Close()
	nc.Conn = nil
}

func (d *RTSPPullProxy) reportProbeFailure(err error) {
	d.probeFailures++
	if d.probeFailures == 1 || d.probeFailures%probeFailureLogEvery == 0 {
		d.Warn("pull proxy heartbeat failed, probe connection reset", "error", err, "failures", d.probeFailures)
	} else {
		d.Debug("pull proxy heartbeat failed, probe connection reset", "error", err, "failures", d.probeFailures)
	}
}
