package plugin_claster

import (
	"context"
	"fmt"
	"path"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"gopkg.in/yaml.v2"
	"m7s.live/v5/pkg/task"
)

// EtcdStreamSync 实现基于 etcd 的流同步
type EtcdStreamSync struct {
	task.Work
	plugin    *ClusterPlugin
	client    *clientv3.Client
	keyPrefix string
	watchChan clientv3.WatchChan
}

// StreamSyncTask 定期同步任务
type StreamSyncTask struct {
	task.TickTask
	sync *EtcdStreamSync
}

// StreamWatchTask 监控任务
type StreamWatchTask struct {
	task.ChannelTask
	sync *EtcdStreamSync
}

// GetTickInterval 获取同步间隔
func (t *StreamSyncTask) GetTickInterval() time.Duration {
	return t.sync.plugin.Sync.FullSyncInterval
}

// GetSignal 获取信号通道
func (t *StreamWatchTask) GetSignal() any {
	return t.sync.watchChan
}

// Tick 执行同步
func (t *StreamSyncTask) Tick(any) {
	// 更新流信息
	t.sync.Info("Updating stream info")
	if err := t.sync.syncStreamsToEtcd(); err != nil {
		t.sync.Error("Failed to update stream info", "error", err)
	}
}

// Tick 处理监控事件
func (t *StreamWatchTask) Tick(resp any) {
	watchResp := resp.(clientv3.WatchResponse)
	if watchResp.Err() != nil {
		t.sync.Error("Watch", "error", watchResp.Err())
		return
	}
	t.sync.handleWatchEvents(watchResp.Events)
}

// NewEtcdStreamSync 创建新的 etcd 流同步服务
func NewEtcdStreamSync(plugin *ClusterPlugin, client *clientv3.Client) *EtcdStreamSync {
	return &EtcdStreamSync{
		plugin:    plugin,
		client:    client,
		keyPrefix: path.Join(plugin.Etcd.KeyPrefix, "streams"),
	}
}

// Start 启动流同步服务
func (s *EtcdStreamSync) Start() error {
	s.Info("Starting etcd stream sync", "keyPrefix", s.keyPrefix)

	// 如果启用了监控，开始监控流变化
	if s.plugin.Etcd.EnableWatcher {
		s.startWatch()
		// 启动监控任务
		watchTask := &StreamWatchTask{sync: s}
		s.AddTask(watchTask)
	}

	// 启动定期同步任务
	s.Info("Starting periodic sync task")
	syncTask := &StreamSyncTask{sync: s}
	s.AddTask(syncTask)

	// 初始同步
	if err := s.syncStreamsFromEtcd(); err != nil {
		s.Error("Failed to sync initial streams", "error", err)
	}

	return nil
}

// Dispose 清理资源
func (s *EtcdStreamSync) Dispose() {
	// 关闭监控通道
	s.Info("Disposing etcd stream sync")
}

// startWatch 开始监控流变化
func (s *EtcdStreamSync) startWatch() {
	s.Info("Starting stream watcher", "path", s.keyPrefix)

	// 监控流目录
	watchPath := s.keyPrefix
	s.watchChan = s.client.Watch(s.Context, watchPath, clientv3.WithPrefix())
	s.Info("Stream watcher started successfully")
}

// syncStreamsToEtcd 将本地流信息同步到 etcd
func (s *EtcdStreamSync) syncStreamsToEtcd() error {
	s.Info("Syncing streams to etcd")

	// 只有管理节点需要同步流信息到 etcd
	if s.plugin.Role != "manager" {
		s.Info("Not a manager node, skipping stream sync to etcd")
		return nil
	}

	// 遍历所有流
	s.plugin.streams.Range(func(stream *StreamInfo) bool {
		// 只同步基本信息，不包括动态信息
		streamBasicInfo := NewStreamBasicInfo()
		streamBasicInfo.StreamPath = stream.StreamPath
		streamBasicInfo.PublisherNodeID = stream.PublisherNodeID
		streamBasicInfo.ReplicatedTo = stream.ReplicatedTo
		streamBasicInfo.State = stream.State
		streamBasicInfo.CreationTime = stream.CreationTime

		// 复制向量时钟和标签
		if stream.VectorClock != nil {
			for k, v := range stream.VectorClock {
				streamBasicInfo.VectorClock[k] = v
			}
		}

		if stream.Tags != nil {
			for k, v := range stream.Tags {
				streamBasicInfo.Tags[k] = v
			}
		}

		// 确保所有字段都已初始化
		streamBasicInfo.EnsureInitialized()

		// 序列化流信息
		data, err := yaml.Marshal(streamBasicInfo)
		if err != nil {
			s.Error("Failed to marshal stream info", "error", err, "streamPath", stream.StreamPath)
			return true
		}

		// 写入 etcd
		ctx, cancel := context.WithTimeout(context.Background(), s.plugin.Etcd.RequestTimeout)
		defer cancel()

		streamPath := path.Join(s.keyPrefix, stream.StreamPath)
		_, err = s.client.Put(ctx, streamPath, string(data))
		if err != nil {
			s.Error("Failed to put stream info", "error", err, "streamPath", stream.StreamPath)
			return true
		}

		s.Debug("Stream info synced to etcd", "streamPath", stream.StreamPath)
		return true
	})

	s.Info("Streams synced to etcd successfully")
	return nil
}

// syncStreamsFromEtcd 从 etcd 同步流信息
func (s *EtcdStreamSync) syncStreamsFromEtcd() error {
	s.Info("Syncing streams from etcd")

	// 获取所有流信息
	ctx, cancel := context.WithTimeout(context.Background(), s.plugin.Etcd.RequestTimeout)
	defer cancel()

	// 查询流目录
	resp, err := s.client.Get(ctx, s.keyPrefix, clientv3.WithPrefix())
	if err != nil {
		return fmt.Errorf("failed to get streams: %v", err)
	}

	s.Info("Found streams in etcd", "count", len(resp.Kvs))

	// 处理流信息
	for _, kv := range resp.Kvs {
		streamPath := path.Base(string(kv.Key))
		s.Debug("Processing stream", "streamPath", streamPath)

		var streamBasicInfo StreamBasicInfo
		if err := yaml.Unmarshal(kv.Value, &streamBasicInfo); err != nil {
			s.Error("Failed to unmarshal stream info", "error", err, "streamPath", streamPath)
			continue
		}

		// 确保所有字段都已初始化
		streamBasicInfo.EnsureInitialized()

		// 跳过本地发布的流
		existingStream, exists := s.plugin.streams.Get(streamBasicInfo.StreamPath)
		if exists && existingStream.PublisherNodeID == s.plugin.NodeID {
			s.Debug("Skipping local stream", "streamPath", streamBasicInfo.StreamPath)
			continue
		}

		// 创建或更新流信息
		streamInfo := NewStreamInfo()
		streamInfo.StreamPath = streamBasicInfo.StreamPath
		streamInfo.PublisherNodeID = streamBasicInfo.PublisherNodeID
		streamInfo.ReplicatedTo = streamBasicInfo.ReplicatedTo
		streamInfo.State = streamBasicInfo.State
		streamInfo.CreationTime = streamBasicInfo.CreationTime

		// 复制向量时钟和标签
		if streamBasicInfo.VectorClock != nil {
			for k, v := range streamBasicInfo.VectorClock {
				streamInfo.VectorClock[k] = v
			}
		}

		if streamBasicInfo.Tags != nil {
			for k, v := range streamBasicInfo.Tags {
				streamInfo.Tags[k] = v
			}
		}

		// 确保所有字段都已初始化
		streamInfo.EnsureInitialized()

		// 如果已存在，保留动态信息
		if exists {
			streamInfo.SubscriberCount = existingStream.SubscriberCount
			// 如果存在媒体信息，则复制
			if existingStream.MediaInfo.VideoCodec != "" || existingStream.MediaInfo.AudioCodec != "" {
				streamInfo.MediaInfo = existingStream.MediaInfo
			}
			// 如果存在上下文，则复制
			if existingStream.Context != nil {
				streamInfo.Context = existingStream.Context
			}
		}

		// 确保上下文已初始化
		if streamInfo.Context == nil {
			streamInfo.Context = context.Background()
		}

		// 添加到流集合
		s.plugin.streams.Add(streamInfo, s.plugin.Logger, streamInfo.Context)
		s.Debug("Stream added/updated from etcd", "streamPath", streamInfo.StreamPath)
	}

	s.Info("Streams synced from etcd successfully")
	return nil
}

// handleWatchEvents 处理监控事件
func (s *EtcdStreamSync) handleWatchEvents(events []*clientv3.Event) {
	s.Info("Handling stream watch events", "count", len(events))

	for _, event := range events {
		streamPath := path.Base(string(event.Kv.Key))

		switch event.Type {
		case clientv3.EventTypePut:
			// 流添加或更新
			var streamBasicInfo StreamBasicInfo
			if err := yaml.Unmarshal(event.Kv.Value, &streamBasicInfo); err != nil {
				s.Error("Failed to unmarshal stream info", "error", err, "streamPath", streamPath)
				continue
			}

			// 确保所有字段都已初始化
			streamBasicInfo.EnsureInitialized()

			// 跳过本地发布的流
			existingStream, exists := s.plugin.streams.Get(streamBasicInfo.StreamPath)
			if exists && existingStream.PublisherNodeID == s.plugin.NodeID {
				s.Debug("Skipping local stream update", "streamPath", streamBasicInfo.StreamPath)
				continue
			}

			// 创建或更新流信息
			streamInfo := NewStreamInfo()
			streamInfo.StreamPath = streamBasicInfo.StreamPath
			streamInfo.PublisherNodeID = streamBasicInfo.PublisherNodeID
			streamInfo.ReplicatedTo = streamBasicInfo.ReplicatedTo
			streamInfo.State = streamBasicInfo.State
			streamInfo.CreationTime = streamBasicInfo.CreationTime

			// 复制向量时钟和标签
			if streamBasicInfo.VectorClock != nil {
				for k, v := range streamBasicInfo.VectorClock {
					streamInfo.VectorClock[k] = v
				}
			}

			if streamBasicInfo.Tags != nil {
				for k, v := range streamBasicInfo.Tags {
					streamInfo.Tags[k] = v
				}
			}

			// 确保所有字段都已初始化
			streamInfo.EnsureInitialized()

			// 如果已存在，保留动态信息
			if exists {
				streamInfo.SubscriberCount = existingStream.SubscriberCount
				// 如果存在媒体信息，则复制
				if existingStream.MediaInfo.VideoCodec != "" || existingStream.MediaInfo.AudioCodec != "" {
					streamInfo.MediaInfo = existingStream.MediaInfo
				}
				// 如果存在上下文，则复制
				if existingStream.Context != nil {
					streamInfo.Context = existingStream.Context
				}
			}

			// 确保上下文已初始化
			if streamInfo.Context == nil {
				streamInfo.Context = context.Background()
			}

			// 添加到流集合
			s.plugin.streams.Add(streamInfo, s.plugin.Logger, streamInfo.Context)
			s.Info("Stream added/updated from etcd", "streamPath", streamInfo.StreamPath)

		case clientv3.EventTypeDelete:
			// 流删除
			s.Info("Stream deleted in etcd", "streamPath", streamPath)

			// 检查是否是本地发布的流
			existingStream, exists := s.plugin.streams.Get(streamPath)
			if exists && existingStream.PublisherNodeID != s.plugin.NodeID {
				// 如果不是本地发布的流，则删除
				s.plugin.streams.RemoveByKey(streamPath)
				s.Info("Stream removed locally", "streamPath", streamPath)
			}
		}
	}
}

// RegisterStream 注册流到 etcd
func (s *EtcdStreamSync) RegisterStream(stream *StreamInfo) error {
	// 只有管理节点需要注册流到 etcd
	if s.plugin.Role != "manager" {
		return nil
	}

	s.Info("Registering stream to etcd", "streamPath", stream.StreamPath)

	// 只同步基本信息，不包括动态信息
	streamBasicInfo := NewStreamBasicInfo()
	streamBasicInfo.StreamPath = stream.StreamPath
	streamBasicInfo.PublisherNodeID = stream.PublisherNodeID
	streamBasicInfo.ReplicatedTo = stream.ReplicatedTo
	streamBasicInfo.State = stream.State
	streamBasicInfo.CreationTime = stream.CreationTime

	// 复制向量时钟和标签
	if stream.VectorClock != nil {
		for k, v := range stream.VectorClock {
			streamBasicInfo.VectorClock[k] = v
		}
	}

	if stream.Tags != nil {
		for k, v := range stream.Tags {
			streamBasicInfo.Tags[k] = v
		}
	}

	// 确保所有字段都已初始化
	streamBasicInfo.EnsureInitialized()

	// 序列化流信息
	data, err := yaml.Marshal(streamBasicInfo)
	if err != nil {
		return fmt.Errorf("failed to marshal stream info: %v", err)
	}

	// 写入 etcd
	ctx, cancel := context.WithTimeout(context.Background(), s.plugin.Etcd.RequestTimeout)
	defer cancel()

	streamPath := path.Join(s.keyPrefix, stream.StreamPath)
	_, err = s.client.Put(ctx, streamPath, string(data))
	if err != nil {
		return fmt.Errorf("failed to put stream info: %v", err)
	}

	s.Info("Stream registered to etcd successfully", "streamPath", stream.StreamPath)
	return nil
}

// UnregisterStream 从 etcd 注销流
func (s *EtcdStreamSync) UnregisterStream(streamPath string) error {
	// 只有管理节点需要注销流
	if s.plugin.Role != "manager" {
		return nil
	}

	s.Info("Unregistering stream from etcd", "streamPath", streamPath)

	// 从 etcd 删除
	ctx, cancel := context.WithTimeout(context.Background(), s.plugin.Etcd.RequestTimeout)
	defer cancel()

	etcdPath := path.Join(s.keyPrefix, streamPath)
	_, err := s.client.Delete(ctx, etcdPath)
	if err != nil {
		return fmt.Errorf("failed to delete stream: %v", err)
	}

	s.Info("Stream unregistered from etcd successfully", "streamPath", streamPath)
	return nil
}

// UpdateStream 更新 etcd 中的流信息
func (s *EtcdStreamSync) UpdateStream(stream *StreamInfo) error {
	// 只有管理节点需要更新流
	if s.plugin.Role != "manager" {
		return nil
	}

	s.Info("Updating stream in etcd", "streamPath", stream.StreamPath)

	// 只更新基本信息，不包括动态信息
	streamBasicInfo := NewStreamBasicInfo()
	streamBasicInfo.StreamPath = stream.StreamPath
	streamBasicInfo.PublisherNodeID = stream.PublisherNodeID
	streamBasicInfo.ReplicatedTo = stream.ReplicatedTo
	streamBasicInfo.State = stream.State
	streamBasicInfo.CreationTime = stream.CreationTime

	// 复制向量时钟和标签
	if stream.VectorClock != nil {
		for k, v := range stream.VectorClock {
			streamBasicInfo.VectorClock[k] = v
		}
	}

	if stream.Tags != nil {
		for k, v := range stream.Tags {
			streamBasicInfo.Tags[k] = v
		}
	}

	// 确保所有字段都已初始化
	streamBasicInfo.EnsureInitialized()

	// 序列化流信息
	data, err := yaml.Marshal(streamBasicInfo)
	if err != nil {
		return fmt.Errorf("failed to marshal stream info: %v", err)
	}

	// 写入 etcd
	ctx, cancel := context.WithTimeout(context.Background(), s.plugin.Etcd.RequestTimeout)
	defer cancel()

	streamPath := path.Join(s.keyPrefix, stream.StreamPath)
	_, err = s.client.Put(ctx, streamPath, string(data))
	if err != nil {
		return fmt.Errorf("failed to put stream info: %v", err)
	}

	s.Info("Stream updated in etcd successfully", "streamPath", stream.StreamPath)
	return nil
}

// StreamBasicInfo 流基本信息（存储在 etcd 中）
type StreamBasicInfo struct {
	StreamPath      string            // 流路径
	PublisherNodeID string            // 发布者节点ID
	ReplicatedTo    []string          // 已复制到的节点列表
	State           string            // 流状态(active, inactive)
	CreationTime    time.Time         // 创建时间
	VectorClock     map[string]uint64 // 向量时钟(用于同步)
	Tags            map[string]string // 流标签
}

// NewStreamBasicInfo 创建新的流基本信息
func NewStreamBasicInfo() *StreamBasicInfo {
	return &StreamBasicInfo{
		ReplicatedTo: make([]string, 0),
		VectorClock:  make(map[string]uint64),
		Tags:         make(map[string]string),
		CreationTime: time.Now(),
		State:        "inactive",
	}
}

// EnsureInitialized 确保所有字段都已初始化
func (s *StreamBasicInfo) EnsureInitialized() {
	if s.ReplicatedTo == nil {
		s.ReplicatedTo = make([]string, 0)
	}
	if s.VectorClock == nil {
		s.VectorClock = make(map[string]uint64)
	}
	if s.Tags == nil {
		s.Tags = make(map[string]string)
	}
	if s.State == "" {
		s.State = "inactive"
	}
	if s.CreationTime.IsZero() {
		s.CreationTime = time.Now()
	}
}
