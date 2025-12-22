# Cluster 插件设计文档

## 概述

Cluster 插件扩展了现有 cascade 插件的功能，提供更加健壮、可扩展和高效的媒体服务器集群解决方案。虽然 cascade 插件实现了基本的级联功能，但 Cluster 插件提供了完整的集群管理解决方案，包括负载均衡、服务器同步、资源优化和高可用性等功能。

## 架构

![Cluster 架构图](../../architecture.png)

### 核心组件

1. **集群管理器（Cluster Manager）**
   - 负责维护集群的整体状态
   - 跟踪服务器健康状况、容量和性能指标
   - 做出负载均衡和资源分配决策
   - 提供管理 API 进行集群管理

2. **节点代理（Node Agent）**
   - 在集群中的每个服务器上运行
   - 收集并报告本地服务器指标（CPU、内存、网络、流）
   - 执行集群管理器发出的命令
   - 管理本地资源和流路由

3. **流同步服务（Stream Synchronization Service）**
   - 确保所有节点上的流信息一致
   - 维护活跃流的分布式注册表
   - 提供快速查找流可用性的功能
   - 支持在节点之间无缝迁移流

4. **负载均衡器（Load Balancer）**
   - 实现上行和下行的各种负载均衡策略
   - 适应不断变化的服务器条件和工作负载
   - 提供连接重定向和流路由
   - 优化以实现最小延迟和最大吞吐量

5. **资源优化器（Resource Optimizer）**
   - 分析资源使用模式
   - 建议或自动实施资源优化策略
   - 平衡性能和资源效率
   - 支持节点间流转码卸载

6. **健康监视器（Health Monitor）**
   - 持续监控所有节点的健康状况
   - 检测节点故障并启动故障转移程序
   - 跟踪性能指标随时间的变化
   - 提供关键问题的告警功能

## 主要功能

### 1. 高级负载均衡

#### 上行负载均衡
- **动态发布者路由**：根据当前负载、地理位置和网络条件，智能地将新发布者路由到最合适的节点。
- **自适应容量管理**：根据观察到的性能持续调整节点容量阈值。
- **发布者迁移**：在需要进行负载分配时，无中断地在节点之间移动发布者。

#### 下行负载均衡
- **边缘优化交付**：将订阅者路由到最佳边缘节点。
- **多层分发**：实现层次化分发模型，最小化主干网络流量。
- **自适应比特率分发**：根据网络条件和节点负载动态调整流质量。

### 2. 流同步

- **实时流注册表**：维护集群中所有活跃流的分布式注册表。
- **元数据同步**：确保流元数据在所有节点上保持一致。
- **低延迟查找**：优化流位置和状态检查的快速查找。
- **流状态恢复**：支持节点故障后的流状态恢复。

### 3. 资源优化

- **智能资源分配**：根据流需求和优先级分配服务器资源。
- **转码分发**：根据可用容量在多个节点之间分配转码工作负载。
- **流合并**：合并低观看量的流以优化资源使用。
- **预测性扩展**：使用历史模式预测资源需求并预先分配容量。

### 4. 高可用性

- **自动故障转移**：将流从故障节点重定向到健康节点。
- **会话持久性**：在故障转移事件期间维护观看者会话。
- **地理冗余**：支持多区域部署以进行灾难恢复。
- **分裂脑预防**：实施共识算法以防止脑裂情况。

### 5. 监控和管理

- **集中式仪表板**：提供整个集群的全面视图。
- **详细指标**：收集并显示流健康状况、服务器性能和观看者体验的指标。
- **告警系统**：通知管理员关键问题。
- **API驱动管理**：提供自动化集群管理的完整API。

## 技术设计

### 通信协议

Cluster 插件扩展了 cascade 插件中基于 QUIC 的通信协议，增加了：

1. **集群控制协议（CCP）**：用于节点管理、健康检查和管理命令。
2. **流同步协议（SSP）**：用于节点之间高效同步流信息。
3. **负载均衡交换协议（LBEP）**：用于交换负载指标和做出平衡决策。

### 数据结构

#### 节点信息
```go
type NodeInfo struct {
    ID             string
    IP             string
    Port           int
    Role           string // "manager", "edge", "transcoder" 等
    Region         string
    Capacity       ResourceCapacity
    CurrentLoad    ResourceUsage
    Status         string // "healthy", "degraded", "offline"
    LastHeartbeat  time.Time
    Streams        map[string]StreamInfo
    Version        string
}

type ResourceCapacity struct {
    MaxConcurrentStreams  int
    MaxBandwidthMbps      int
    MaxCPUPercent         float64
    MaxMemoryGB           float64
    TranscodingCapacity   int // 相对单位
}

type ResourceUsage struct {
    ConcurrentStreams   int
    BandwidthMbps       float64
    CPUPercent          float64
    MemoryGB            float64
    TranscodingLoad     float64 // 0.0-1.0
    DiskIOPS            int
    NetworkLatencyMs    map[string]float64 // 与其他节点之间的延迟
}
```

#### 流信息
```go
type StreamInfo struct {
    StreamPath       string
    PublisherNodeID  string
    StartTime        time.Time
    MediaInfo        MediaDetails
    ViewerCount      int
    BandwidthMbps    float64
    Priority         int // 用于资源分配决策
    ReplicatedTo     []string // 节点ID列表
    State            string // "active", "inactive", "migrating"
}

type MediaDetails struct {
    VideoCodec       string
    VideoResolution  string
    VideoFPS         float64
    VideoBitrateMbps float64
    AudioCodec       string
    AudioBitrateMbps float64
    KeyframeInterval int
}
```

### 算法

#### 负载均衡算法

负载均衡算法考虑多个因素：

1. **当前资源利用率**：CPU、内存、网络和流数量
2. **节点能力**：不同节点可能有不同的硬件能力
3. **网络拓扑**：优先考虑靠近发布者/订阅者的节点
4. **流特性**：高比特率流有不同的资源需求

算法为每个节点计算综合得分：

```
score = w1*cpuFactor + w2*memoryFactor + w3*networkFactor + w4*streamCountFactor + w5*topologyFactor
```

其中：
- `w1` 到 `w5` 是可配置的权重
- 每个因子都被归一化到 0-1 范围

#### 流同步算法

流同步使用经过修改的 gossip 协议：

1. 每个节点维护一个本地流注册表
2. 节点定期与其对等节点交换增量更新
3. 完全同步按可配置的间隔进行
4. 使用布隆过滤器高效识别差异
5. 冲突解决使用最后写入获胜策略和向量时钟

## 开发路线图

### 阶段 1：基础（2 周）

1. 设置基本插件结构
2. 实现带有基本指标收集的节点代理
3. 创建节点和流信息的数据结构
4. 开发通信协议（CCP、SSP、LBEP）
5. 实现基本集群形成和节点发现

### 阶段 2：核心功能（3 周）

1. 实现集群管理器
2. 开发流同步服务
3. 创建上行和下行的基本负载均衡
4. 实现节点健康监控和基本故障转移
5. 开发管理 API

### 阶段 3：高级功能（4 周）

1. 增强负载均衡的高级算法
2. 实现资源优化策略
3. 添加流迁移功能
4. 开发多区域支持
5. 创建监控仪表板

### 阶段 4：优化和测试（3 周）

1. 性能优化
2. 压力测试和可扩展性测试
3. 故障场景测试
4. 文档编写
5. 安全强化

## 与现有系统集成

Cluster 插件将通过以下方式与现有系统集成：

1. 基于 cascade 插件的框架进行构建
2. 扩展核心系统中定义的插件接口
3. 利用现有的流处理管道
4. 利用现有的配置管理系统

## 配置

```yaml
cluster:
  enabled: true
  role: "manager" # 或 "edge", "worker" 等
  manager_address: "192.168.1.100:7001" # 仅对非管理节点
  manager_fallback: "192.168.1.101:7001" # 可选的备份管理器
  cluster_secret: "shared-secret-key"
  region: "us-east"
  
  # 资源配置
  resources:
    max_streams: 1000
    max_bandwidth_mbps: 10000
    reserve_cpu_percent: 20
    reserve_memory_percent: 15
    transcoding_priority: 0.8 # 0.0-1.0
  
  # 负载均衡配置
  load_balancing:
    strategy: "resource-aware" # 或 "round-robin", "network-optimized" 等
    check_interval_ms: 1000
    rebalance_threshold: 0.2 # 触发重新平衡的差异
    weights:
      cpu: 0.3
      memory: 0.2
      network: 0.3
      stream_count: 0.2
  
  # 同步配置
  sync:
    gossip_interval_ms: 500
    full_sync_interval_ms: 60000
    sync_batch_size: 1000
  
  # 高可用性配置
  high_availability:
    failover_timeout_ms: 5000
    min_manager_nodes: 3
    heartbeat_interval_ms: 1000
    consensus_protocol: "raft" # 或 "paxos"
  
  # 监控配置
  monitoring:
    metrics_interval_ms: 5000
    history_retention_hours: 24
    alert_thresholds:
      cpu_percent: 90
      memory_percent: 85
      stream_failure_rate: 0.01
```

## 结论

Cluster 插件为管理分布式媒体服务器网络提供了全面的解决方案。它解决了负载均衡、资源优化、高可用性和监控方面的关键挑战。通过构建在现有 cascade 插件的基础上并扩展其功能，Cluster 实现了高度可扩展和可靠的媒体流基础设施。 