# Monibuca 集群架构设计

本文档描述了 Monibuca 的集群架构设计，包括推流负载均衡和拉流负载均衡的实现方案。

## 整体架构

```mermaid
graph TB
    subgraph 负载均衡层
    LB[负载均衡器/API网关]
    end
    
    subgraph 集群节点
    N1[节点1]
    N2[节点2]
    N3[节点3]
    end
    
    subgraph 服务发现
    Redis[(Redis/etcd)]
    end
    
    Client1[推流客户端] --> LB
    Client2[拉流客户端] --> LB
    
    LB --> N1
    LB --> N2
    LB --> N3
    
    N1 <--> Redis
    N2 <--> Redis
    N3 <--> Redis
    
    %% 节点间互通连接
    N1 <-.流媒体同步.-> N2
    N2 <-.流媒体同步.-> N3
    N1 <-.流媒体同步.-> N3
```

## 节点间流媒体同步

```mermaid
sequenceDiagram
    participant C as 拉流客户端
    participant N2 as 节点2
    participant R as Redis/etcd
    participant N1 as 节点1(源流所在)
    
    C->>N2: 请求拉流(Stream1)
    N2->>R: 查询Stream1位置
    R-->>N2: 返回Stream1在节点1
    N2->>N1: 请求Stream1
    N1-->>N2: 建立节点间流传输
    Note over N1,N2: 使用高效的节点间传输协议
    N2->>R: 注册Stream1副本信息
    N2-->>C: 向客户端推送流
```

## 推流负载均衡

```mermaid
sequenceDiagram
    participant P as 推流客户端
    participant LB as 负载均衡器
    participant R as Redis/etcd
    participant N1 as 节点1
    participant N2 as 节点2
    
    P->>LB: 发起推流请求
    LB->>R: 获取可用节点列表
    R-->>LB: 返回节点信息
    LB->>LB: 根据负载算法选择节点
    LB-->>P: 返回推流节点地址
    P->>N1: 建立推流连接
    N1->>R: 注册流信息
```

## 拉流负载均衡

```mermaid
sequenceDiagram
    participant C as 拉流客户端
    participant LB as 负载均衡器
    participant R as Redis/etcd
    participant N1 as 源节点
    participant N2 as 边缘节点
    
    C->>LB: 发起拉流请求
    LB->>R: 查询流信息
    R-->>LB: 返回流所在节点
    alt 就近节点已有流
        LB-->>C: 返回就近节点地址
        C->>N2: 建立拉流连接
    else 需要回源
        LB-->>C: 返回边缘节点地址
        C->>N2: 建立拉流连接
        N2->>N1: 回源拉流
        N2->>R: 注册流信息
    end
```

## 关键特性

1. **高可用性**
   - 节点故障自动切换
   - 无单点故障设计
   - 服务自动发现
   - 多节点流媒体冗余备份

2. **负载均衡策略**
   - 基于节点负载的动态调度
   - 就近接入原则
   - 带宽占用均衡
   - 考虑节点间流量成本

3. **扩展性**
   - 支持水平扩展
   - 动态添加删除节点
   - 平滑扩容/缩容
   - 节点间按需同步流

4. **监控和管理**
   - 集群状态实时监控
   - 流量统计和分析
   - 节点健康检查
   - 跨节点流媒体质量监控

## 实现考虑

1. **服务发现**
   - 使用 Redis 或 etcd 存储集群节点信息
   - 定期更新节点状态和负载信息
   - 支持节点心跳检测
   - 维护流媒体在各节点的分布信息

2. **负载均衡算法**
   - 考虑 CPU 使用率
   - 考虑内存使用情况
   - 考虑带宽使用情况
   - 考虑地理位置因素
   - 考虑节点间网络质量

3. **容错机制**
   - 节点故障自动摘除
   - 流媒体自动切换
   - 会话保持机制
   - 节点间流媒体备份策略

4. **节点间通信**
   - 高效的流媒体转发协议
   - 节点间带宽优化
   - 流媒体缓存策略
   - 按需拉流和预加载策略
   - QoS保证机制 