package plugin_claster

import (
	"context"
	"time"

	"m7s.live/v5"
	"m7s.live/v5/pkg/task"
	"m7s.live/v5/plugin/cluster/pb"
)

// NodeInfo 节点信息
type NodeInfo struct {
	m7s.Plugin    `json:"-" yaml:"-"`
	task.Task     `json:"-" yaml:"-"`
	ID            string                // 节点ID
	IP            string                // 节点IP地址
	Port          int                   // 节点端口
	Role          string                // 节点角色(manager, worker, edge, transcoder)
	Region        string                // 节点所在区域
	Version       string                // 节点软件版本
	Status        string                // 节点状态(healthy, unhealthy, offline)
	JoinTime      time.Time             // 节点加入时间
	LastHeartbeat time.Time             // 最后一次心跳时间
	Online        bool                  // 节点是否在线
	TotalMemoryGB float64               // 系统总内存(GB)
	Capacity      ResourceCapacity      // 节点资源容量
	CurrentLoad   ResourceUsage         // 节点当前负载
	Streams       map[string]StreamInfo // 节点上的流信息
	StreamCount   int                   // 节点上的流数量
	Tags          map[string]string     // 节点标签
	LocalNode     bool                  `json:"-" yaml:"-"` // 是否是本机节点
	ApiClient     pb.ApiClient          `json:"-" yaml:"-"` // API客户端，用于节点间通信
	Context       context.Context       `json:"-" yaml:"-"` // 上下文对象，用于同步操作
}

// GetKey 获取节点ID作为键
func (n *NodeInfo) GetKey() string {
	return n.ID
}

// Deadline 实现 task.ManagerItem 接口
func (n *NodeInfo) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

// StreamInfo 流信息
type StreamInfo struct {
	m7s.Plugin      `json:"-"`
	task.Task       `json:"-"`
	StreamPath      string            // 流路径
	PublisherNodeID string            // 发布者节点ID
	ReplicatedTo    []string          // 已复制到的节点列表
	SubscriberCount int               // 订阅者数量
	State           string            // 流状态(active, inactive)
	MediaInfo       MediaInfo         // 媒体信息
	ClientInfo      ClientInfo        // 客户端信息
	CreationTime    time.Time         // 创建时间
	LastUpdated     time.Time         // 最后更新时间
	VectorClock     map[string]uint64 // 向量时钟(用于同步)
	Tags            map[string]string // 流标签
	StartTime       time.Time         // 流开始时间
	Context         context.Context   `json:"-" yaml:"-"` // 上下文对象，用于同步操作
}

// NewStreamInfo 创建新的流信息
func NewStreamInfo() *StreamInfo {
	return &StreamInfo{
		ReplicatedTo:    make([]string, 0),
		VectorClock:     make(map[string]uint64),
		Tags:            make(map[string]string),
		CreationTime:    time.Now(),
		LastUpdated:     time.Now(),
		MediaInfo:       MediaInfo{},
		ClientInfo:      ClientInfo{Metadata: make(map[string]string)},
		Context:         context.Background(),
		SubscriberCount: 0,
		State:           "inactive",
	}
}

// EnsureInitialized 确保所有字段都已初始化
func (s *StreamInfo) EnsureInitialized() {
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
	if s.LastUpdated.IsZero() {
		s.LastUpdated = time.Now()
	}
	if s.Context == nil {
		s.Context = context.Background()
	}

	// 确保 ClientInfo 已初始化
	if s.ClientInfo.Metadata == nil {
		s.ClientInfo.Metadata = make(map[string]string)
	}

	// 确保 MediaInfo 已初始化
	if s.MediaInfo.VideoCodec == "" && s.MediaInfo.AudioCodec == "" {
		s.MediaInfo = MediaInfo{}
	}
}

// GetKey 获取流路径作为键
func (s *StreamInfo) GetKey() string {
	return s.StreamPath
}

// GetContext 获取上下文
func (s *StreamInfo) GetContext() context.Context {
	return s.Context
}

// SetContext 设置上下文
func (s *StreamInfo) SetContext(ctx context.Context) {
	s.Context = ctx
}

// Deadline 实现 task.ManagerItem 接口
func (s *StreamInfo) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

// ResourceCapacity 定义节点资源容量
type ResourceCapacity struct {
	MaxConcurrentStreams int     // 最大并发流数
	MaxBandwidthMbps     int     // 最大带宽(Mbps)
	MaxCPUPercent        float64 // 最大CPU使用率(%)
	MaxMemoryGB          float64 // 最大内存使用量(GB)
	TranscodingCapacity  int     // 转码能力等级
	ReserveCPUPercent    float64 // 预留CPU百分比
	ReserveMemoryGB      float64 // 预留内存(GB)
	ReserveBandwidthMbps int     // 预留带宽(Mbps)
}

// ResourceUsage 定义资源使用情况
type ResourceUsage struct {
	ConcurrentStreams int                // 当前并发流数
	BandwidthMbps     int                // 当前带宽使用量(Mbps)
	CPUPercent        float64            // 当前CPU使用率(%)
	MemoryGB          float64            // 当前内存使用量(GB)
	TranscodingLoad   int                // 当前转码负载
	NetworkLatencyMs  map[string]float64 // 与其他节点的网络延迟(ms)
}

// MediaInfo 定义媒体信息结构
type MediaInfo struct {
	VideoCodec    string  // 视频编码
	AudioCodec    string  // 音频编码
	Resolution    string  // 分辨率
	Framerate     float64 // 帧率
	VideoEnabled  bool    // 是否有视频
	AudioEnabled  bool    // 是否有音频
	VideoWidth    int     // 视频宽度
	VideoHeight   int     // 视频高度
	StartTime     int64   // 开始时间戳
	BandwidthMbps float64 // 带宽(Mbps)
}

// ClientInfo 定义客户端信息结构
type ClientInfo struct {
	ClientID    string            // 客户端ID
	ClientIP    string            // 客户端IP
	ConnectTime time.Time         // 连接时间
	UserAgent   string            // 用户代理
	Metadata    map[string]string // 元数据
}

// ClusterStats 集群统计信息
type ClusterStats struct {
	TotalNodes     int       `json:"totalNodes"`     // 总节点数
	ActiveNodes    int       `json:"activeNodes"`    // 活动节点数
	TotalStreams   int       `json:"totalStreams"`   // 总流数
	TotalClients   int       `json:"totalClients"`   // 总客户端数
	TotalBandwidth int       `json:"totalBandwidth"` // 总带宽使用(Mbps)
	UpdatedAt      time.Time `json:"updatedAt"`      // 更新时间
}

// NodeEvent 节点事件
type NodeEvent struct {
	Type      string    `json:"type"`      // 事件类型：join, leave, update, fail
	NodeID    string    `json:"nodeId"`    // 节点ID
	Timestamp time.Time `json:"timestamp"` // 事件时间
	Details   string    `json:"details"`   // 事件详情
}

// StreamEvent 流事件
type StreamEvent struct {
	Type       string    `json:"type"`       // 事件类型：publish, unpublish, play, stop
	StreamPath string    `json:"streamPath"` // 流路径
	NodeID     string    `json:"nodeId"`     // 节点ID
	ClientIP   string    `json:"clientIp"`   // 客户端IP
	Timestamp  time.Time `json:"timestamp"`  // 事件时间
	Details    string    `json:"details"`    // 事件详情
}

// HealthCheck 健康检查结果
type HealthCheck struct {
	NodeID    string    `json:"nodeId"`    // 节点ID
	CheckTime time.Time `json:"checkTime"` // 检查时间
	IsHealthy bool      `json:"isHealthy"` // 是否健康
	Latency   int64     `json:"latency"`   // 延迟(ms)
	ErrorMsg  string    `json:"errorMsg"`  // 错误信息
}

// EtcdConfig etcd 配置
type EtcdConfig struct {
	// 基础配置
	Enabled       bool     // 是否启用 etcd
	Endpoints     []string // etcd 服务器地址列表
	Username      string   // etcd 用户名
	Password      string   // etcd 密码
	CertFile      string   // TLS 证书文件路径
	KeyFile       string   // TLS 密钥文件路径
	TrustedCAFile string   // TLS CA 证书文件路径

	// 连接配置
	DialTimeout      time.Duration `default:"5s"`  // 连接超时时间
	RequestTimeout   time.Duration `default:"3s"`  // 请求超时时间
	AutoSyncInterval time.Duration `default:"30s"` // 自动同步成员列表间隔

	// 键空间配置
	KeyPrefix    string // 键前缀，用于隔离不同集群
	NodeKeyTTL   int64  // 节点注册信息的 TTL (秒)
	StreamKeyTTL int64  // 流信息的 TTL (秒)

	// 重试配置
	MaxRetries    int           // 最大重试次数
	RetryInterval time.Duration `default:"1s"` // 重试间隔

	// 监控配置
	EnableWatcher bool          // 是否启用变更监控
	WatchTimeout  time.Duration `default:"10s"` // 监控超时时间

	// 内嵌 etcd 服务器配置
	Server struct {
		Enabled                 bool     // 是否启用内嵌 etcd 服务器
		DataDir                 string   // 数据目录
		ListenClientUrls        []string // 客户端监听地址
		ListenPeerUrls          []string // 节点间通信监听地址
		AdvertiseClientUrls     []string // 对外公布的客户端地址
		AdvertisePeerUrls       []string // 对外公布的节点间通信地址
		InitialCluster          string   // 初始集群配置
		InitialClusterState     string   // 初始集群状态
		InitialClusterToken     string   // 集群 token
		SnapshotCount           uint64   // 快照计数
		AutoCompactionMode      string   // 自动压缩模式
		AutoCompactionRetention string   // 自动压缩保留时间
		QuotaBackendBytes       int64    // 后端配额大小
	}
}

// LoadBalancer 负载均衡器接口
type LoadBalancer interface {
	Start() error
	Stop(err error)
	OnNodeAdded(node *NodeInfo)
	OnNodeRemoved(node *NodeInfo)
	OnNodeStatusChanged(node *NodeInfo)
	OnStreamAdded(stream *StreamInfo)
	OnStreamRemoved(stream *StreamInfo)
}
