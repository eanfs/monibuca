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
)

const (
	PushProxyStatusOffline byte = iota
	PushProxyStatusOnline
	PushProxyStatusPushing
	PushProxyStatusDisabled
)

type (
	IPushProxy interface {
		Push()
	}
	PushProxy struct {
		server               *Server `gorm:"-:all"`
		task.Work            `gorm:"-:all" yaml:"-"`
		ID                   uint           `gorm:"primarykey"`
		CreatedAt, UpdatedAt time.Time      `yaml:"-"`
		DeletedAt            gorm.DeletedAt `yaml:"-"`
		Name                 string
		StreamPath           string
		Audio, PushOnStart   bool
		config.Push          `gorm:"embedded;embeddedPrefix:push_"`
		ParentID             uint
		Type                 string
		Status               byte
		Description          string
		RTT                  time.Duration
		Handler              IPushProxy `gorm:"-:all" yaml:"-"`
	}
	PushProxyManager struct {
		task.Manager[uint, *PushProxy]
	}
	PushProxyTask struct {
		task.TickTask
		PushProxy *PushProxy
		Plugin    *Plugin
	}
	TCPPushProxy struct {
		PushProxyTask
		TCPAddr *net.TCPAddr
		URL     *url.URL
	}
)

func (d *PushProxy) GetKey() uint {
	return d.ID
}

func (d *PushProxy) GetStreamPath() string {
	if d.StreamPath == "" {
		return fmt.Sprintf("push/%s/%d", d.Type, d.ID)
	}
	return d.StreamPath
}

func (d *PushProxy) Start() (err error) {
	for plugin := range d.server.Plugins.Range {
		if pushPlugin, ok := plugin.handler.(IPushProxyPlugin); ok && strings.EqualFold(d.Type, plugin.Meta.Name) {
			pushTask := pushPlugin.OnPushProxyAdd(d)
			if pushTask == nil {
				continue
			}
			if pushTask, ok := pushTask.(IPushProxy); ok {
				d.Handler = pushTask
			}
			if t, ok := pushTask.(task.ITask); ok {
				if ticker, ok := t.(task.IChannelTask); ok {
					t.OnStart(func() {
						ticker.Tick(nil)
					})
				}
				d.AddTask(t)
			} else {
				d.ChangeStatus(PushProxyStatusOnline)
			}
		}
	}
	return
}

func (d *PushProxy) ChangeStatus(status byte) {
	if d.Status == status {
		return
	}
	from := d.Status
	d.Info("device status changed", "from", from, "to", status)
	d.Status = status
	d.Update()
	switch status {
	case PushProxyStatusOnline:
		if d.PushOnStart && from == PushProxyStatusOffline {
			d.Handler.Push()
		}
	}
}

func (d *PushProxy) Update() {
	if d.server.DB != nil {
		d.server.DB.Omit("deleted_at").Save(d)
	}
}

func (d *PushProxyTask) Dispose() {
	d.PushProxy.ChangeStatus(PushProxyStatusOffline)
	d.TickTask.Dispose()
}

func (d *PushProxyTask) Push() {
	var subConf = d.Plugin.config.Subscribe
	subConf.SubAudio = d.PushProxy.Audio
	d.Plugin.handler.Push(d.PushProxy.GetStreamPath(), d.PushProxy.Push, &subConf)
}

func (d *TCPPushProxy) GetTickInterval() time.Duration {
	return time.Second * 10
}

func (d *TCPPushProxy) Tick(any) {
	startTime := time.Now()
	conn, err := net.DialTCP("tcp", nil, d.TCPAddr)
	if err != nil {
		d.PushProxy.ChangeStatus(PushProxyStatusOffline)
		return
	}
	conn.Close()
	d.PushProxy.RTT = time.Since(startTime)
	d.PushProxy.ChangeStatus(PushProxyStatusOnline)
}
