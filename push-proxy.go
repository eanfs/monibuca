package m7s

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/mcuadros/go-defaults"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
	"m7s.live/v5/pb"
	"m7s.live/v5/pkg"
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
		if from == PushProxyStatusOffline {
			if d.PushOnStart {
				d.Handler.Push()
			} else {
				d.server.Streams.Call(func() error {
					if d.server.Streams.Has(d.GetStreamPath()) {
						d.Handler.Push()
					}
					return nil
				})
			}
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
	if d.PushProxy.Status == PushProxyStatusOffline {
		d.PushProxy.ChangeStatus(PushProxyStatusOnline)
	}
}

func (d *PushProxy) InitializeWithServer(s *Server) {
	d.server = s
	d.Logger = s.Logger.With("pushProxy", d.ID, "type", d.Type, "name", d.Name)
	if d.Type == "" {
		u, err := url.Parse(d.URL)
		if err != nil {
			d.Logger.Error("parse push url failed", "error", err)
			return
		}
		switch u.Scheme {
		case "srt", "rtsp", "rtmp":
			d.Type = u.Scheme
		default:
			ext := filepath.Ext(u.Path)
			switch ext {
			case ".m3u8":
				d.Type = "hls"
			case ".flv":
				d.Type = "flv"
			case ".mp4":
				d.Type = "mp4"
			}
		}
	}
}

func (s *Server) GetPushProxyList(ctx context.Context, req *emptypb.Empty) (res *pb.PushProxyListResponse, err error) {
	res = &pb.PushProxyListResponse{}
	s.PushProxies.Call(func() error {
		for device := range s.PushProxies.Range {
			res.Data = append(res.Data, &pb.PushProxyInfo{
				Name:        device.Name,
				CreateTime:  timestamppb.New(device.CreatedAt),
				UpdateTime:  timestamppb.New(device.UpdatedAt),
				Type:        device.Type,
				PushURL:     device.URL,
				ParentID:    uint32(device.ParentID),
				Status:      uint32(device.Status),
				ID:          uint32(device.ID),
				PushOnStart: device.PushOnStart,
				Audio:       device.Audio,
				Description: device.Description,
				Rtt:         uint32(device.RTT.Milliseconds()),
				StreamPath:  device.GetStreamPath(),
			})
		}
		return nil
	})
	return
}

func (s *Server) AddPushProxy(ctx context.Context, req *pb.PushProxyInfo) (res *pb.SuccessResponse, err error) {
	device := &PushProxy{
		server:      s,
		Name:        req.Name,
		Type:        req.Type,
		ParentID:    uint(req.ParentID),
		PushOnStart: req.PushOnStart,
		Description: req.Description,
		StreamPath:  req.StreamPath,
	}

	if device.Type == "" {
		var u *url.URL
		u, err = url.Parse(req.PushURL)
		if err != nil {
			s.Error("parse pull url failed", "error", err)
			return
		}
		switch u.Scheme {
		case "srt", "rtsp", "rtmp":
			device.Type = u.Scheme
		default:
			ext := filepath.Ext(u.Path)
			switch ext {
			case ".m3u8":
				device.Type = "hls"
			case ".flv":
				device.Type = "flv"
			case ".mp4":
				device.Type = "mp4"
			}
		}
	}

	defaults.SetDefaults(&device.Push)
	device.URL = req.PushURL
	device.Audio = req.Audio
	if s.DB == nil {
		err = pkg.ErrNoDB
		return
	}
	s.DB.Create(device)
	s.PushProxies.Add(device)
	res = &pb.SuccessResponse{}
	return
}

func (s *Server) UpdatePushProxy(ctx context.Context, req *pb.PushProxyInfo) (res *pb.SuccessResponse, err error) {
	if s.DB == nil {
		err = pkg.ErrNoDB
		return
	}
	target := &PushProxy{
		server: s,
	}
	err = s.DB.First(target, req.ID).Error
	if err != nil {
		return
	}
	target.Name = req.Name
	target.URL = req.PushURL
	target.ParentID = uint(req.ParentID)
	target.Type = req.Type
	if target.Type == "" {
		var u *url.URL
		u, err = url.Parse(req.PushURL)
		if err != nil {
			s.Error("parse pull url failed", "error", err)
			return
		}
		switch u.Scheme {
		case "srt", "rtsp", "rtmp":
			target.Type = u.Scheme
		default:
			ext := filepath.Ext(u.Path)
			switch ext {
			case ".m3u8":
				target.Type = "hls"
			case ".flv":
				target.Type = "flv"
			case ".mp4":
				target.Type = "mp4"
			}
		}
	}
	target.PushOnStart = req.PushOnStart
	target.Audio = req.Audio
	target.Description = req.Description
	target.RTT = time.Duration(int(req.Rtt)) * time.Millisecond
	target.StreamPath = req.StreamPath
	s.DB.Save(target)
	var needStopOld *PushProxy
	s.PushProxies.Call(func() error {
		if device, ok := s.PushProxies.Get(uint(req.ID)); ok {
			if target.URL != device.URL || device.Audio != target.Audio || device.StreamPath != target.StreamPath {
				device.Stop(task.ErrStopByUser)
				needStopOld = device
				return nil
			}
			if device.PushOnStart != target.PushOnStart && target.PushOnStart && device.Handler != nil && device.Status == PushProxyStatusOnline {
				device.Handler.Push()
			}
			device.Name = target.Name
			device.PushOnStart = target.PushOnStart
			device.Description = target.Description
		}
		return nil
	})
	if needStopOld != nil {
		needStopOld.WaitStopped()
		s.PushProxies.Add(target)
	}
	res = &pb.SuccessResponse{}
	return
}

func (s *Server) RemovePushProxy(ctx context.Context, req *pb.RequestWithId) (res *pb.SuccessResponse, err error) {
	if s.DB == nil {
		err = pkg.ErrNoDB
		return
	}
	res = &pb.SuccessResponse{}
	if req.Id > 0 {
		tx := s.DB.Delete(&PushProxy{
			ID: uint(req.Id),
		})
		err = tx.Error
		s.PushProxies.Call(func() error {
			if device, ok := s.PushProxies.Get(uint(req.Id)); ok {
				device.Stop(task.ErrStopByUser)
			}
			return nil
		})
		return
	} else if req.StreamPath != "" {
		var deviceList []*PushProxy
		s.DB.Find(&deviceList, "stream_path=?", req.StreamPath)
		if len(deviceList) > 0 {
			for _, device := range deviceList {
				tx := s.DB.Delete(device)
				err = tx.Error
				s.PushProxies.Call(func() error {
					if device, ok := s.PushProxies.Get(uint(device.ID)); ok {
						device.Stop(task.ErrStopByUser)
					}
					return nil
				})
			}
		}
		return
	} else {
		res.Message = "parameter wrong"
		return
	}
}
