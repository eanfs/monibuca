package m7s

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/task"
	"m7s.live/v5/pkg/util"
)

const (
	PullProxyStatusOffline byte = iota
	PullProxyStatusOnline
	PullProxyStatusPulling
	PullProxyStatusDisabled
)

type (
	IPullProxy interface {
		Pull()
	}
	PullProxy struct {
		server                         *Server `gorm:"-:all"`
		task.Work                      `gorm:"-:all" yaml:"-"`
		ID                             uint           `gorm:"primarykey"`
		CreatedAt, UpdatedAt           time.Time      `yaml:"-"`
		DeletedAt                      gorm.DeletedAt `yaml:"-"`
		Name                           string
		StreamPath                     string
		PullOnStart, Audio, StopOnIdle bool
		config.Pull                    `gorm:"embedded;embeddedPrefix:pull_"`
		config.Record                  `gorm:"embedded;embeddedPrefix:record_"`
		ParentID                       uint
		Type                           string
		Status                         byte
		Description                    string
		RTT                            time.Duration
		Handler                        IPullProxy `gorm:"-:all" yaml:"-"`
	}
	PullProxyManager struct {
		task.Manager[uint, *PullProxy]
	}
	PullProxyTask struct {
		task.TickTask
		PullProxy *PullProxy
		Plugin    *Plugin
	}
	HTTPPullProxy struct {
		TCPPullProxy
	}
	TCPPullProxy struct {
		PullProxyTask
		TCPAddr *net.TCPAddr
		URL     *url.URL
	}
)

func (d *PullProxy) GetKey() uint {
	return d.ID
}

func (d *PullProxy) GetStreamPath() string {
	if d.StreamPath == "" {
		return fmt.Sprintf("pull/%s/%d", d.Type, d.ID)
	}
	return d.StreamPath
}

func (d *PullProxy) Start() (err error) {
	for plugin := range d.server.Plugins.Range {
		if pullPlugin, ok := plugin.handler.(IPullProxyPlugin); ok && strings.EqualFold(d.Type, plugin.Meta.Name) {
			pullTask := pullPlugin.OnPullProxyAdd(d)
			if pullTask == nil {
				continue
			}
			if pullTask, ok := pullTask.(IPullProxy); ok {
				d.Handler = pullTask
			}
			if t, ok := pullTask.(task.ITask); ok {
				if ticker, ok := t.(task.IChannelTask); ok {
					t.OnStart(func() {
						ticker.Tick(nil)
					})
				}
				d.AddTask(t)
			} else {
				d.ChangeStatus(PullProxyStatusOnline)
			}
		}
	}
	return
}

func (d *PullProxy) ChangeStatus(status byte) {
	if d.Status == status {
		return
	}
	from := d.Status
	d.Info("device status changed", "from", from, "to", status)
	d.Status = status
	d.Update()
	switch status {
	case PullProxyStatusOnline:
		if d.PullOnStart && from == PullProxyStatusOffline {
			d.Handler.Pull()
		}
	}
}

func (d *PullProxy) Update() {
	if d.server.DB != nil {
		d.server.DB.Omit("deleted_at").Save(d)
	}
}

func (d *PullProxyTask) Dispose() {
	d.PullProxy.ChangeStatus(PullProxyStatusOffline)
	d.TickTask.Dispose()
	d.Plugin.Server.Streams.Call(func() error {
		if stream, ok := d.Plugin.Server.Streams.Get(d.PullProxy.GetStreamPath()); ok {
			stream.Stop(task.ErrStopByUser)
		}
		return nil
	})
}

func (d *PullProxyTask) Pull() {
	var pubConf = d.Plugin.config.Publish
	pubConf.PubAudio = d.PullProxy.Audio
	pubConf.DelayCloseTimeout = util.Conditional(d.PullProxy.StopOnIdle, time.Second*5, 0)
	d.Plugin.handler.Pull(d.PullProxy.GetStreamPath(), d.PullProxy.Pull, &pubConf)
}

func (d *HTTPPullProxy) Start() (err error) {
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
			if d.URL.Scheme == "https" || d.URL.Scheme == "wss" {
				d.TCPAddr.Port = 443
			} else {
				d.TCPAddr.Port = 80
			}
		}
	}
	return d.PullProxyTask.Start()
}

func (d *TCPPullProxy) GetTickInterval() time.Duration {
	return time.Second * 10
}

func (d *TCPPullProxy) Tick(any) {
	startTime := time.Now()
	conn, err := net.DialTCP("tcp", nil, d.TCPAddr)
	if err != nil {
		d.PullProxy.ChangeStatus(PullProxyStatusOffline)
		return
	}
	conn.Close()
	d.PullProxy.RTT = time.Since(startTime)
	d.PullProxy.ChangeStatus(PullProxyStatusOnline)
}
