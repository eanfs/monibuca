package m7s

import (
	"context"
	"net/url"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"
	"gorm.io/gorm"
	"m7s.live/v5/pb"
)

type AliasStream struct {
	*Publisher `gorm:"-:all"`
	AutoRemove bool
	StreamPath string
	Alias      string     `gorm:"primarykey"`
	Args       url.Values `gorm:"-"`
}

func (a *AliasStream) GetKey() string {
	return a.Alias
}

// StreamAliasDB 用于存储流别名的数据库模型
type StreamAliasDB struct {
	AliasStream
	ArgsString string    `gorm:"column:args;type:text"`
	CreatedAt  time.Time `yaml:"-"`
	UpdatedAt  time.Time `yaml:"-"`
}

func (StreamAliasDB) TableName() string {
	return "stream_alias"
}

// BeforeSave 保存前序列化查询参数
func (db *StreamAliasDB) BeforeSave(tx *gorm.DB) error {
	if len(db.Args) > 0 {
		db.ArgsString = db.Args.Encode()
	} else {
		db.ArgsString = ""
	}
	return nil
}

// BeforeCreate 创建前序列化查询参数
func (db *StreamAliasDB) BeforeCreate(tx *gorm.DB) error {
	return db.BeforeSave(tx)
}

// BeforeUpdate 更新前序列化查询参数
func (db *StreamAliasDB) BeforeUpdate(tx *gorm.DB) error {
	return db.BeforeSave(tx)
}

// AfterFind 查询后反序列化查询参数
func (db *StreamAliasDB) AfterFind(tx *gorm.DB) error {
	if db.ArgsString != "" {
		var err error
		db.Args, err = url.ParseQuery(db.ArgsString)
		if err != nil {
			db.Args = nil
		}
	} else {
		db.Args = nil
	}
	return nil
}

func (s *Server) initStreamAlias() {
	if s.DB == nil {
		return
	}
	var aliases []StreamAliasDB
	s.DB.Find(&aliases)
	for _, alias := range aliases {
		s.AliasStreams.Add(&alias.AliasStream)
		if publisher, ok := s.Streams.Get(alias.StreamPath); ok {
			alias.Publisher = publisher
		}
	}
}

func (s *Server) GetStreamAlias(ctx context.Context, req *emptypb.Empty) (res *pb.StreamAliasListResponse, err error) {
	res = &pb.StreamAliasListResponse{}
	s.CallOnStreamTask(func() {
		for alias := range s.AliasStreams.Range {
			info := &pb.StreamAlias{
				StreamPath: alias.StreamPath,
				Alias:      alias.Alias,
				AutoRemove: alias.AutoRemove,
			}
			if s.Streams.Has(alias.Alias) {
				info.Status = 2
			} else if alias.Publisher != nil {
				info.Status = 1
			}
			res.Data = append(res.Data, info)
		}
	})
	return
}

func (s *Server) SetStreamAlias(ctx context.Context, req *pb.SetStreamAliasRequest) (res *pb.SuccessResponse, err error) {
	res = &pb.SuccessResponse{}
	s.CallOnStreamTask(func() {
		if req.StreamPath != "" {
			u, err := url.Parse(req.StreamPath)
			if err != nil {
				return
			}
			req.StreamPath = strings.TrimPrefix(u.Path, "/")
			queryParams := u.Query()
			publisher, canReplace := s.Streams.Get(req.StreamPath)
			if !canReplace {
				defer s.OnSubscribe(req.StreamPath, queryParams)
			}
			if aliasInfo, ok := s.AliasStreams.Get(req.Alias); ok { //modify alias
				oldStreamPath := aliasInfo.StreamPath
				aliasInfo.AutoRemove = req.AutoRemove
				aliasInfo.Args = queryParams
				if aliasInfo.StreamPath != req.StreamPath {
					aliasInfo.StreamPath = req.StreamPath
					if canReplace {
						if aliasInfo.Publisher != nil {
							aliasInfo.TransferSubscribers(publisher) // replace stream
							aliasInfo.Publisher = publisher
						} else {
							aliasInfo.Publisher = publisher
							s.Waiting.WakeUp(req.Alias, publisher)
						}
					}
				}
				// 更新数据库中的别名
				if s.DB != nil {
					dbAlias := &StreamAliasDB{
						AliasStream: *aliasInfo,
					}
					s.DB.Where("alias = ?", req.Alias).Save(dbAlias)
				}
				s.Info("modify alias", "alias", req.Alias, "oldStreamPath", oldStreamPath, "streamPath", req.StreamPath, "replace", ok && canReplace)
			} else { // create alias
				aliasInfo := AliasStream{
					AutoRemove: req.AutoRemove,
					StreamPath: req.StreamPath,
					Alias:      req.Alias,
					Args:       queryParams,
				}
				var pubId uint32
				s.AliasStreams.Add(&aliasInfo)
				aliasStream, ok := s.Streams.Get(aliasInfo.Alias)
				if canReplace {
					aliasInfo.Publisher = publisher
					if ok {
						aliasStream.TransferSubscribers(publisher) // replace stream
					} else {
						s.Waiting.WakeUp(req.Alias, publisher)
					}
				} else if ok {
					aliasInfo.Publisher = aliasStream
				}
				if aliasInfo.Publisher != nil {
					pubId = aliasInfo.Publisher.ID
				}
				// 保存到数据库
				if s.DB != nil {
					dbAlias := &StreamAliasDB{
						AliasStream: aliasInfo,
					}
					s.DB.Create(dbAlias)
				}
				s.Info("add alias", "alias", req.Alias, "streamPath", req.StreamPath, "replace", ok && canReplace, "pub", pubId)
			}
		} else {
			s.Info("remove alias", "alias", req.Alias)
			if aliasStream, ok := s.AliasStreams.Get(req.Alias); ok {
				s.AliasStreams.Remove(aliasStream)
				// 从数据库中删除
				if s.DB != nil {
					s.DB.Where("alias = ?", req.Alias).Delete(&StreamAliasDB{})
				}
				if aliasStream.Publisher != nil {
					if publisher, hasTarget := s.Streams.Get(req.Alias); hasTarget { // restore stream
						aliasStream.TransferSubscribers(publisher)
					} else {
						// 优先使用别名保存的查询参数
						args := aliasStream.Args
						if len(args) == 0 {
							// 如果没有保存的查询参数，则从订阅者中获取
							for sub := range aliasStream.Publisher.SubscriberRange {
								if sub.StreamPath == req.Alias {
									aliasStream.Publisher.RemoveSubscriber(sub)
									s.Waiting.Wait(sub)
									args = sub.Args
									break
								}
							}
						}
						if len(args) > 0 {
							s.OnSubscribe(req.Alias, args)
						}
					}
				}
			}
		}
	})
	return
}

func (p *Publisher) processAliasOnStart() {
	s := p.Plugin.Server
	for alias := range s.AliasStreams.Range {
		if alias.StreamPath != p.StreamPath {
			continue
		}
		if alias.Publisher == nil {
			alias.Publisher = p
			s.Waiting.WakeUp(alias.Alias, p)
		} else if alias.Publisher.StreamPath != alias.StreamPath {
			alias.Publisher.TransferSubscribers(p)
			alias.Publisher = p
		}
	}
}

func (p *Publisher) processAliasOnDispose() {
	s := p.Plugin.Server
	var relatedAlias []*AliasStream
	for alias := range s.AliasStreams.Range {
		if alias.StreamPath == p.StreamPath {
			if alias.AutoRemove {
				defer s.AliasStreams.Remove(alias)
				if s.DB != nil {
					defer s.DB.Where("alias = ?", alias.Alias).Delete(&StreamAliasDB{})
				}
			}
			alias.Publisher = nil
			relatedAlias = append(relatedAlias, alias)
		}
	}
	if p.Subscribers.Length > 0 {
	SUBSCRIBER:
		for subscriber := range p.SubscriberRange {
			for _, alias := range relatedAlias {
				if subscriber.StreamPath == alias.Alias {
					if originStream, ok := s.Streams.Get(alias.Alias); ok {
						originStream.AddSubscriber(subscriber)
						continue SUBSCRIBER
					}
				}
			}
			s.Waiting.Wait(subscriber)
		}
		p.Subscribers.Clear()
	}
}

func (s *Subscriber) processAliasOnStart() (hasInvited bool, done bool) {
	server := s.Plugin.Server
	if alias, ok := server.AliasStreams.Get(s.StreamPath); ok {
		if alias.Publisher != nil {
			alias.Publisher.AddSubscriber(s)
			done = true
			return
		} else {
			// 合并参数：先使用别名保存的参数，然后用订阅者传入的参数覆盖同名参数
			args := make(url.Values)
			// 先复制别名保存的参数
			if alias.Args != nil {
				for k, v := range alias.Args {
					args[k] = append([]string(nil), v...)
				}
			}
			// 用订阅者传入的参数覆盖同名参数
			if s.Args != nil {
				for k, v := range s.Args {
					args[k] = append([]string(nil), v...)
				}
			}
			server.OnSubscribe(alias.StreamPath, args)
			hasInvited = true
		}
	} else {
		for reg, alias := range server.StreamAlias {
			if streamPath := reg.Replace(s.StreamPath, alias); streamPath != "" {
				as := AliasStream{
					StreamPath: streamPath,
					Alias:      s.StreamPath,
					Args:       s.Args,
				}
				server.AliasStreams.Set(&as)
				if server.DB != nil {
					dbAlias := &StreamAliasDB{
						AliasStream: as,
					}
					server.DB.Where("alias = ?", s.StreamPath).Save(dbAlias)
				}
				if publisher, ok := server.Streams.Get(streamPath); ok {
					publisher.AddSubscriber(s)
					done = true
					return
				} else {
					server.OnSubscribe(streamPath, s.Args)
					hasInvited = true
				}
				break
			}
		}
	}
	return
}
