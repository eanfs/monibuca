# Monibuca 集群（务实方案 A）

本文档描述当前仓库中“务实集群方案”的实现：**不做媒体流同步/复制**，只提供“控制面路由 + 播放重定向/RTSP 代理”，以最低成本实现：

- 任何节点都可以对集群内任意节点的流执行录制等控制面操作（实际执行在源节点）
- 任何节点都可以播放集群内任意节点的流（HTTP 302 重定向；RTSP 可选本地拉流代理）

## 核心思路（控制面 vs 媒体面）

- **媒体面**：不做跨节点的“流转发/回源/副本同步”（带宽/复杂度/一致性成本很高）。
- **控制面**：当某节点收到“针对某条流”的 API 请求，但本机没有该流时，通过 gRPC 探测找到源节点并转发执行。
- **播放入口**：当某节点收到播放请求，但本机没有该流时，HTTP 返回重定向到源节点地址；RTSP 可通过本地拉流代理避免客户端重定向。

## 配置与节点发现（静态 peers）

节点发现保持静态：通过 `cluster.sync`（或 `cluster.peers`）维护邻居表，作为 APIRoute 的 peer discovery。

```yaml
cluster:
  # 可选：更直观的静态 peers
  # peers:
  #   - "localhost:50052"
  #   - "localhost:50053"
  #   - "localhost:50054"
  sync:
    serverid: "node1"
    address: "localhost:50052"
    seedservers:
      - "localhost:50053"
      - "localhost:50054"
```

当配置了 `cluster:` 段时，系统会默认开启 `global.apiRoute.enable`（除非显式设置 `global.apiRoute.enable=false`）。

若 RTSP 客户端不支持重定向，可在各节点启用：

```yaml
rtsp:
  proxyOnRedirect: true
```

## 需要的插件

- `plugin/apiroute`：负责播放重定向（HTTP），并为 RTSP 代理提供目标解析。
- `plugin/mp4`：提供录制 API（通过控制面路由在源节点执行）。
- `plugin/cluster`：消费 `cluster:` 配置并补全 APIRoute peers（静态方式），作为“务实集群”的配置入口。

示例工程见 `example/cluster/`。

## 非目标（当前不做）

- 基于 etcd/Redis 的动态服务发现
- 负载均衡调度
- 跨节点媒体回源/副本同步/边缘缓存

这些能力可以作为后续“方案 B（平台化集群）”演进方向，但不属于当前务实方案。 
