# Cluster 技术规范

## 1. 系统要求

### 1.1 硬件要求
- **最低要求**：
  - CPU: 4 核心
  - RAM: 8 GB
  - 网络: 千兆以太网
  - 存储: 100 GB SSD
- **推荐配置**:
  - CPU: 8+ 核心
  - RAM: 16+ GB
  - 网络: 10 千兆以太网
  - 存储: 500+ GB NVMe SSD

### 1.2 软件要求
- **操作系统**:
  - Linux (Ubuntu 20.04+, CentOS 8+)
  - 支持 Windows Server 2019+
- **依赖项**:
  - Node.js v16+
  - Redis v6+（用于集群状态）
  - NATS v2.2+（用于消息传递）
- **兼容性**:
  - 与现有的 Cascade 插件架构兼容

### 1.3 网络要求
- **端口**:
  - 8000-8010: API 和 Web 界面
  - 9000-9010: 节点间通信
  - 10000-10100: 媒体流传输
- **防火墙配置**:
  - 允许所有节点之间的双向通信
  - 允许从客户端到所有节点的接入
- **延迟要求**:
  - 集群节点之间 < 50ms（最佳）
  - 跨区域节点可接受 < 200ms

### 1.4 扩展性要求
- 支持至少 20 个节点的集群
- 每个集群支持至少 10,000 个并发流
- 能够处理每秒至少 10,000 个订阅请求

## 2. 通信协议规范

### 2.1 集群控制协议 (CCP)

#### 2.1.1 概述
集群控制协议管理节点之间的通信，处理拓扑结构变化、配置更新和集群管理功能。

#### 2.1.2 消息格式
```json
{
  "messageId": "唯一标识符",
  "messageType": "消息类型",
  "sender": {
    "nodeId": "发送节点 ID",
    "nodeRole": "发送节点角色",
    "timestamp": "发送时间戳"
  },
  "payload": {
    // 特定于消息类型的数据
  },
  "signature": "可选的消息签名用于验证"
}
```

#### 2.1.3 消息类型
- **节点发现**: `NODE_DISCOVERY`, `NODE_ANNOUNCEMENT`, `NODE_HEARTBEAT`
- **集群管理**: `CLUSTER_CONFIG`, `ROLE_ASSIGNMENT`, `LEADER_ELECTION`
- **状态同步**: `STATE_UPDATE`, `METRIC_REPORT`, `CONFIG_SYNC`

#### 2.1.4 安全性
- 使用 TLS 1.3 加密所有通信
- 实现节点证书验证
- 使用 HMAC-SHA256 进行消息完整性验证

### 2.2 流同步协议 (SSP)

#### 2.2.1 概述
流同步协议管理流的元数据和可用性信息在节点间的同步。

#### 2.2.2 消息格式
```json
{
  "syncId": "同步操作 ID",
  "operation": "同步操作类型",
  "streamInfo": {
    "streamId": "流 ID",
    "publisherNodeId": "发布者节点 ID",
    "streamType": "流类型",
    "metadata": {
      // 流的元数据
    },
    "replicaInfo": [
      {
        "nodeId": "副本节点 ID",
        "priority": "副本优先级",
        "health": "副本健康状态"
      }
    ]
  },
  "timestamp": "操作时间戳"
}
```

#### 2.2.3 操作类型
- **流注册**: 新流添加到集群时
- **流注销**: 流不再可用时
- **副本创建**: 在新节点上创建流副本时
- **状态更新**: 更新流的可用性或健康状态时
- **元数据更新**: 更新流的元数据时

### 2.3 负载均衡交换协议 (LBEP)

#### 2.3.1 概述
负载均衡交换协议在节点间共享负载信息，并协调发布者和订阅者重定向。

#### 2.3.2 消息格式
```json
{
  "balanceId": "均衡操作 ID",
  "operationType": "操作类型",
  "sourceNodeId": "源节点 ID",
  "targetNodeId": "目标节点 ID",
  "resourceInfo": {
    "cpuLoad": "CPU 负载百分比",
    "memoryLoad": "内存负载百分比",
    "networkBandwidth": "可用网络带宽",
    "streamCount": "当前流数量"
  },
  "clientRedirectInfo": {
    // 用于重定向的特定客户端信息
  },
  "timestamp": "操作时间戳"
}
```

#### 2.3.3 操作类型
- **负载报告**: 定期节点资源报告
- **发布者重定向**: 请求重定向发布者到不同节点
- **订阅者重定向**: 请求重定向订阅者到不同节点
- **容量查询**: 查询节点当前容量
- **均衡建议**: 负载均衡器对节点操作的建议

## 3. 算法规范

### 3.1 集群管理算法

#### 3.1.1 领导者选举
- 实现基于 Raft 的共识算法
- 选举超时: 500ms（可配置）
- 心跳间隔: 100ms（可配置）
- 领导者选举规则:
  1. 节点等待随机时间后开始选举（150-300ms）
  2. 节点投票给具有最高优先级和最新日志的候选人
  3. 一旦收到大多数票，候选人成为领导者
  4. 领导者故障检测基于心跳超时

#### 3.1.2 节点角色分配
- 角色类型: 管理节点、工作节点、边缘节点
- 分配算法:
  1. 分析每个节点的资源能力和网络位置
  2. 为高容量节点分配管理角色（优先考虑高可用性和低延迟）
  3. 为最靠近用户的节点分配边缘角色
  4. 为剩余节点分配工作角色
  5. 确保角色平衡以保持集群健康

### 3.2 负载均衡算法

#### 3.2.1 发布者负载均衡
- 权重计算:
  ```
  节点权重 = (可用CPU * 0.4) + (可用内存 * 0.3) + (可用带宽 * 0.3)
  ```
- 节点选择算法:
  1. 筛选满足发布者最低要求的节点
  2. 使用 Power of Two Choices 算法:
     - 随机选择两个节点
     - 比较它们的权重
     - 选择权重更高的节点
  3. 应用亲和性规则以优先选择现有连接
  4. 应用地理位置优化以减少延迟

#### 3.2.2 订阅者负载均衡
- 节点评分:
  ```
  订阅者评分 = (流的可用性 * 0.5) + (节点到订阅者的距离 * 0.3) + (节点负载 * 0.2)
  ```
- 节点选择:
  1. 确定可以提供请求流的所有节点
  2. 通过计算网络跳数或延迟估计距离
  3. 为每个候选节点计算评分
  4. 选择评分最高的节点
  5. 在多个相似评分的节点之间应用随机化以避免热点

### 3.3 资源优化算法

#### 3.3.1 流放置
- 优化目标: 最小化资源使用同时满足流质量要求
- 算法:
  1. 对流的资源需求进行分类（高/中/低）
  2. 对节点资源可用性进行排名
  3. 使用改进的 First-Fit-Decreasing 算法
  4. 考虑节点亲和性以最小化数据传输
  5. 对高权重流进行优先处理

#### 3.3.2 自适应副本管理
- 副本数量计算:
  ```
  副本数 = max(2, min(流优先级 * 2, 节点数 / 4))
  ```
- 副本放置:
  1. 确保副本跨多个物理服务器/机架/区域
  2. 为每个副本计算优先级得分
  3. 建立优先级故障转移链
  4. 在资源压力下减少低优先级流的副本
  5. 在资源充足时增加高优先级流的副本

### 3.4 健康监控算法

#### 3.4.1 故障检测
- 多层失败监测:
  1. 直接心跳检查（100ms 间隔）
  2. 间接八卦风格的健康传播
  3. ICMP/TCP 连接监控作为备份
- 故障分类:
  - 暂时性：短暂的网络故障
  - 部分性：节点只能与部分集群通信
  - 持久性：节点长时间脱机
- 故障确认阈值: 至少 3 个其他节点必须报告同一故障

#### 3.4.2 愈合策略
- 自动恢复步骤:
  1. 对暂时性故障使用指数退避重试
  2. 对部分故障尝试重新路由通信
  3. 对持久性故障触发重新均衡
  4. 在恢复后执行同步阶段
  5. 在节点恢复前先验证数据完整性

## 4. 数据结构规范

### 4.1 集群状态

```typescript
interface ClusterState {
  clusterId: string;
  version: number;
  lastUpdated: number; // Unix 时间戳
  leaderNodeId: string;
  nodes: Map<string, NodeInfo>;
  topology: {
    links: Array<{
      sourceId: string;
      targetId: string;
      latency: number; // 毫秒
      bandwidth: number; // Mbps
      quality: number; // 0-1 范围内的连接质量
    }>;
  };
  config: ClusterConfig;
  healthStatus: 'healthy' | 'degraded' | 'critical';
}
```

### 4.2 节点信息

```typescript
interface NodeInfo {
  nodeId: string;
  hostname: string;
  ipAddress: string;
  port: number;
  nodeRole: 'manager' | 'worker' | 'edge';
  capabilities: {
    maxConcurrentStreams: number;
    supportedCodecs: string[];
    transcoding: boolean;
    recording: boolean;
  };
  resources: {
    cpu: {
      total: number; // 核心数
      used: number; // 百分比
    };
    memory: {
      total: number; // MB
      used: number; // MB
    };
    network: {
      uplink: number; // Mbps
      downlink: number; // Mbps
      used: number; // Mbps
    };
    gpu?: {
      model: string;
      memory: number; // MB
      used: number; // 百分比
    };
  };
  stats: {
    publisherCount: number;
    subscriberCount: number;
    activeStreams: number;
    dataTransferred: number; // 字节
    uptime: number; // 秒
  };
  healthChecks: {
    lastHeartbeat: number; // Unix 时间戳
    consecutiveFailures: number;
    status: 'healthy' | 'unhealthy' | 'unknown';
  };
  metadata: Record<string, any>; // 自定义元数据
}
```

### 4.3 流信息

```typescript
interface StreamInfo {
  streamId: string;
  name: string;
  publisherId: string;
  publisherNodeId: string;
  created: number; // Unix 时间戳
  streamType: 'live' | 'recording' | 'ondemand';
  status: 'active' | 'inactive' | 'error';
  media: {
    video?: {
      codec: string;
      bitrate: number; // kbps
      framerate: number;
      resolution: {
        width: number;
        height: number;
      };
    };
    audio?: {
      codec: string;
      bitrate: number; // kbps
      sampleRate: number;
      channels: number;
    };
  };
  statistics: {
    subscriberCount: number;
    bandwidth: number; // 当前带宽使用 (bps)
    packetLoss: number; // 百分比
    latency: number; // 毫秒
  };
  replicas: Array<{
    nodeId: string;
    status: 'active' | 'syncing' | 'error';
    priority: number; // 复制优先级
    lagMs: number; // 与主要源的滞后
  }>;
  metadata: Record<string, any>; // 自定义流元数据
}
```

### 4.4 负载均衡状态

```typescript
interface LoadBalancerState {
  balancerId: string;
  algorithm: 'round-robin' | 'weighted' | 'least-connections' | 'resource-aware';
  lastRebalance: number; // Unix 时间戳
  healthThresholds: {
    cpu: number; // 百分比阈值，超过时触发再平衡
    memory: number;
    bandwidth: number;
  };
  nodeWeights: Map<string, number>; // 节点 ID 到权重的映射
  activeRedirections: Array<{
    clientId: string;
    fromNodeId: string;
    toNodeId: string;
    timestamp: number;
    status: 'pending' | 'active' | 'complete' | 'failed';
  }>;
  streamDistribution: Map<string, string[]>; // 流 ID 到节点 ID 数组的映射
  history: Array<{
    timestamp: number;
    action: string;
    details: any;
    result: 'success' | 'failure';
  }>;
}
```

## 5. API 规范

### 5.1 RESTful API

#### 5.1.1 集群管理 API
- `GET /api/v1/cluster/status` - 获取集群状态
- `GET /api/v1/cluster/nodes` - 列出所有节点
- `GET /api/v1/cluster/nodes/:nodeId` - 获取节点详细信息
- `POST /api/v1/cluster/nodes` - 添加新节点
- `DELETE /api/v1/cluster/nodes/:nodeId` - 移除节点
- `PUT /api/v1/cluster/config` - 更新集群配置

#### 5.1.2 流管理 API
- `GET /api/v1/streams` - 列出所有流
- `GET /api/v1/streams/:streamId` - 获取流详细信息
- `POST /api/v1/streams` - 手动注册流
- `DELETE /api/v1/streams/:streamId` - 手动注销流
- `GET /api/v1/streams/:streamId/replicas` - 列出流的所有副本
- `POST /api/v1/streams/:streamId/replicas` - 创建新副本

#### 5.1.3 负载均衡 API
- `GET /api/v1/loadbalancer/status` - 获取负载均衡器状态
- `PUT /api/v1/loadbalancer/config` - 更新负载均衡配置
- `POST /api/v1/loadbalancer/rebalance` - 手动触发重新平衡
- `GET /api/v1/loadbalancer/metrics` - 获取负载均衡指标
- `GET /api/v1/loadbalancer/redirections` - 列出活动重定向
- `POST /api/v1/loadbalancer/redirections` - 手动创建重定向

#### 5.1.4 监控 API
- `GET /api/v1/metrics` - 获取整体集群指标
- `GET /api/v1/metrics/nodes/:nodeId` - 获取节点指标
- `GET /api/v1/metrics/streams/:streamId` - 获取流指标
- `GET /api/v1/alerts` - 获取当前告警
- `GET /api/v1/events` - 获取系统事件日志

### 5.2 Websocket API

#### 5.2.1 实时指标流
- `ws://server/api/v1/metrics/live` - 实时集群指标
- 推送格式:
  ```json
  {
    "timestamp": 1623456789,
    "metrics": {
      "nodes": {
        "online": 5,
        "total": 6
      },
      "streams": {
        "active": 120,
        "bandwidth": 450000000
      },
      "clients": {
        "publishers": 85,
        "subscribers": 1250
      },
      "resources": {
        "cpuAverage": 45.2,
        "memoryAverage": 62.5,
        "networkUtilization": 38.7
      }
    }
  }
  ```

#### 5.2.2 事件和告警
- `ws://server/api/v1/events/live` - 实时系统事件
- 推送格式:
  ```json
  {
    "eventId": "evt-12345",
    "timestamp": 1623456789,
    "level": "warning",
    "category": "node",
    "source": "node-42",
    "message": "High CPU utilization detected",
    "details": {
      "metricValue": 92.5,
      "threshold": 90
    }
  }
  ```

### 5.3 插件 API

#### 5.3.1 节点通信
```typescript
interface NodeCommunication {
  // 获取集群内所有节点
  getNodes(): Promise<NodeInfo[]>;
  
  // 与特定节点通信
  sendMessage(nodeId: string, message: any): Promise<void>;
  
  // 广播消息给所有节点或特定角色的节点
  broadcast(message: any, targetRole?: string): Promise<void>;
  
  // 订阅来自特定节点的事件
  subscribeToNode(nodeId: string, eventType: string, callback: Function): void;
  
  // 订阅特定类型的集群事件
  subscribeToCluster(eventType: string, callback: Function): void;
}
```

#### 5.3.2 流管理
```typescript
interface StreamManagement {
  // 注册新流
  registerStream(stream: StreamInfo): Promise<string>;
  
  // 更新流信息
  updateStream(streamId: string, updates: Partial<StreamInfo>): Promise<void>;
  
  // 注销流
  unregisterStream(streamId: string): Promise<void>;
  
  // 获取流信息
  getStreamInfo(streamId: string): Promise<StreamInfo>;
  
  // 查询满足条件的流
  queryStreams(filters: any): Promise<StreamInfo[]>;
  
  // 获取流的可用副本
  getStreamReplicas(streamId: string): Promise<StreamReplica[]>;
  
  // 在指定节点上创建流副本
  createReplica(streamId: string, nodeId: string, priority: number): Promise<string>;
}
```

#### 5.3.3 负载均衡
```typescript
interface LoadBalancing {
  // 获取负载均衡器状态
  getStatus(): Promise<LoadBalancerState>;
  
  // 获取节点的当前负载
  getNodeLoad(nodeId: string): Promise<NodeLoadInfo>;
  
  // 为发布者选择最佳节点
  selectNodeForPublisher(publisherId: string, requirements?: any): Promise<string>;
  
  // 为订阅者选择最佳节点
  selectNodeForSubscriber(streamId: string, subscriberId: string, location?: any): Promise<string>;
  
  // 手动触发重新平衡
  triggerRebalance(scope?: 'full' | 'publishers' | 'subscribers'): Promise<void>;
  
  // 重定向客户端到另一个节点
  redirectClient(clientId: string, targetNodeId: string): Promise<boolean>;
}
```

## 6. 安全考虑

### 6.1 认证和授权
- **节点认证**:
  - 使用 X.509 证书进行相互 TLS 认证
  - 实现证书自动续订
  - 支持证书撤销列表（CRL）
  
- **API 认证**:
  - 支持 OAuth 2.0 和 JWT
  - 实现 API 密钥认证
  - 支持 HMAC 请求签名
  
- **授权模型**:
  - 基于角色的访问控制（RBAC）
  - 支持自定义角色和细粒度权限
  - 读/写分离权限

### 6.2 节点间安全
- **传输安全**:
  - 所有节点间通信使用 TLS 1.3
  - 支持强密码套件配置
  - 实现传输层证书锁定
  
- **消息完整性**:
  - 对所有集群消息实现 HMAC 签名
  - 使用每个会话的唯一密钥
  - 实现防重放保护
  
- **节点验证**:
  - 初始节点注册需要预共享密钥或管理员批准
  - 实现节点信誉系统
  - 对可疑行为实现自动隔离

### 6.3 数据保护
- **敏感数据处理**:
  - 加密存储的配置数据和凭证
  - 实现安全日志记录（屏蔽敏感信息）
  - 支持静态数据加密选项
  
- **隐私考虑**:
  - 匿名化用户元数据
  - 可配置的数据保留策略
  - 合规性删除功能
  
- **访问控制**:
  - 流级别访问控制
  - 支持 IP 和地理位置限制
  - 按流和按节点的访问白名单/黑名单

### 6.4 漏洞防护
- **输入验证**:
  - 对所有 API 输入实现严格验证
  - 防止 SQL 注入、XSS 和 CSRF
  - 实现请求速率限制和防暴力攻击
  
- **安全更新**:
  - 支持节点上的无缝安全更新
  - 版本验证以防止降级攻击
  - 自动依赖漏洞扫描
  
- **监控和审计**:
  - 全面的安全日志记录
  - 异常行为检测
  - 安全审计跟踪

## 7. 可扩展性

### 7.1 插件架构
- **扩展点**:
  - 自定义负载均衡算法插件
  - 自定义流处理插件
  - 监控和指标集成插件
  - 安全和认证插件
  
- **插件界面**:
  ```typescript
  interface ClasterPlugin {
    id: string;
    version: string;
    initialize(context: PluginContext): Promise<void>;
    start(): Promise<void>;
    stop(): Promise<void>;
    getCapabilities(): string[];
    getApiExtensions(): ApiExtension[];
  }
  ```
  
- **生命周期钩子**:
  - 节点启动前/后
  - 流注册前/后
  - 负载均衡决策前/后
  - 节点故障前/后

### 7.2 自定义负载均衡
- **自定义策略界面**:
  ```typescript
  interface LoadBalancingStrategy {
    id: string;
    name: string;
    selectNodeForPublisher(publisherId: string, nodes: NodeInfo[], state: ClusterState): Promise<string>;
    selectNodeForSubscriber(streamId: string, subscriberId: string, nodes: NodeInfo[], state: ClusterState): Promise<string>;
    calculateNodeWeights(nodes: NodeInfo[], state: ClusterState): Promise<Map<string, number>>;
    shouldRebalance(state: ClusterState): Promise<boolean>;
  }
  ```
  
- **策略配置**:
  - 通过 JSON 架构进行声明式配置
  - 运行时可调参数
  - A/B 测试支持

### 7.3 事件钩子系统
- **事件类型**:
  - 集群事件（节点加入/离开）
  - 流事件（流创建/删除）
  - 负载均衡事件（重定向/再平衡）
  - 监控事件（阈值超过/警报）
  
- **订阅界面**:
  ```typescript
  interface EventHook {
    id: string;
    eventTypes: string[];
    priority: number; // 执行优先级
    async: boolean; // 同步或异步钩子
    handler(event: ClasterEvent): Promise<void | boolean>;
  }
  ```
  
- **事件过滤**:
  - 支持 JSONPath 风格的事件过滤
  - 正则表达式匹配
  - 阈值触发器

## 8. 监控和调试

### 8.1 指标收集
- **核心指标**:
  - 节点健康和资源使用情况
  - 流统计（活动、带宽、错误率）
  - 客户端连接和分布
  - 负载均衡效率指标
  
- **收集方法**:
  - 基于推送的实时指标
  - 历史指标聚合
  - 分层收集（节点 → 管理器 → 存储）
  
- **存储策略**:
  - 高精度短期存储（1 秒精度，保留 24 小时）
  - 中等精度中期存储（1 分钟精度，保留 30 天）
  - 低精度长期存储（1 小时精度，保留 1 年）

### 8.2 日志记录
- **日志级别**:
  - ERROR: 需要立即关注的关键问题
  - WARN: 潜在问题或异常情况
  - INFO: 常规操作信息
  - DEBUG: 详细调试信息
  - TRACE: 极其详细的调试（仅开发）
  
- **日志格式**:
  ```json
  {
    "timestamp": "ISO-8601 时间戳",
    "level": "INFO",
    "source": {
      "nodeId": "节点-ID",
      "component": "组件名称",
      "location": "文件:行"
    },
    "message": "日志消息",
    "context": {
      // 特定上下文信息
    },
    "correlationId": "请求/操作的相关 ID"
  }
  ```
  
- **分布式追踪**:
  - 实现 OpenTelemetry 兼容性
  - 分布式操作的相关 ID
  - 链路追踪可视化

### 8.3 告警系统
- **告警级别**:
  - CRITICAL: 需要立即干预
  - HIGH: 需要快速响应
  - MEDIUM: 需要注意
  - LOW: 信息性问题
  
- **告警类型**:
  - 资源告警（CPU、内存、网络）
  - 健康告警（节点故障、连接问题）
  - 流告警（流错误、质量问题）
  - 安全告警（认证失败、可疑行为）
  
- **通知集成**:
  - Webhook 支持
  - 电子邮件通知
  - Slack/Teams 集成
  - SMS/推送通知（严重告警）

### 8.4 调试工具
- **实时检查**:
  - 流实时统计和状态
  - 连接信息和质量
  - 当前路由和负载决策
  - 资源使用可视化
  
- **诊断命令**:
  - 集群健康检查
  - 节点互连测试
  - 流可用性探测
  - 配置验证
  
- **故障排除**:
  - 自动诊断流程
  - 可视化问题检测器
  - 排除故障指导
  - 自我修复能力

## 9. 部署考虑

### 9.1 部署拓扑
- **单区域部署**:
  - 最低 3 个节点以实现高可用性
  - 最少 1 个管理节点，2 个工作节点
  - 本地网络互连（<10ms 延迟）
  
- **多区域部署**:
  - 每个区域至少 3 个节点
  - 每个区域至少 1 个管理节点
  - 区域间高速连接（<100ms 延迟）
  - 区域感知路由和数据位置
  
- **边缘/CDN 集成**:
  - Edge 节点部署在靠近用户位置
  - 与 CDN 提供商集成的直接连接
  - 边缘缓存和预缓存能力
  - 最后一公里分发优化

### 9.2 网络配置
- **端口和防火墙**:
  - 集群间通信: UDP/TCP 9000-9010
  - 管理 API: TCP 8000-8010
  - 媒体流端口: UDP/TCP 10000-10100
  - WebRTC ICE: UDP 49152-65535
  
- **网络优化**:
  - DSCP/QoS 标记媒体流量
  - 为低延迟流量实现优先级队列
  - 多路径 TCP 支持
  - 快速故障转移路由
  
- **网络安全**:
  - DDoS 保护配置
  - 流量过滤建议
  - RTCP 包验证
  - 安全信令路径

### 9.3 容器化和编排
- **容器镜像**:
  - 基于 Alpine Linux 的轻量级镜像
  - 多阶段构建最小化镜像大小
  - 非特权容器运行
  - 不变性镜像设计
  
- **Kubernetes 部署**:
  - StatefulSet 用于管理节点
  - DaemonSet 用于特定节点功能
  - 适当的资源限制和请求
  - 自动扩展配置
  - 反亲和性规则以实现高可用性
  
- **Helm 图表配置**:
  - 环境特定的值文件
  - 自动化角色分配
  - 集群发现机制
  - 可插拔的持久化选项

### 9.4 升级策略
- **无缝升级**:
  - 跨集群的滚动更新
  - 自动就绪探针验证
  - 优雅流迁移
  - 自动回滚能力
  
- **版本兼容性**:
  - N-1 版本兼容性保证
  - 渐进式协议升级
  - 数据模式迁移处理
  - 实现长期支持（LTS）版本
  
- **升级测试**:
  - 自动化兼容性测试
  - 升级前和升级后验证
  - 多版本并存测试
  - 渐进式金丝雀更新流程 