# Cluster Plugin（务实方案 A）

本插件是“务实集群方案”的配置入口：消费配置文件中的 `cluster:` 段，提供**静态节点发现**（邻居表），并与 `global.apiRoute` 配合实现：

- 任意节点调用录制等控制面 API → 自动路由到实际拥有该流的源节点执行
- 任意节点播放集群内任意节点的流 → 通过 `plugin/apiroute` 重定向到源节点

注意：当前方案**不做跨节点媒体流同步/复制**（不回源、不做副本），只解决“从任何节点都能控制/播放”的入口问题。

## 配置

### 1) 推荐：`cluster.sync`（兼容示例）

```yaml
cluster:
  sync:
    serverid: "node1"
    address: "localhost:50052"
    seedservers:
      - "localhost:50053"
      - "localhost:50054"
```

### 2) 可选：`cluster.peers`（更直观）

```yaml
cluster:
  peers:
    - "localhost:50052"
    - "localhost:50053"
    - "localhost:50054"
```

## 与 APIRoute 的关系

- 当配置了 `cluster:` 段时，系统会默认启用 `global.apiRoute.enable`（除非显式设置 `global.apiRoute.enable=false`）。
- 当 `global.apiRoute.grpcPeers/nodes` 未配置时，APIRoute 会从 `cluster.peers` 或 `cluster.sync` 推导 peers。

## 需要启用的插件

- `plugin/apiroute`：播放重定向（HTTP/RTSP）
- `plugin/mp4`：录制 API（控制面路由会把请求转发到源节点）
- `plugin/cluster`：消费 `cluster:` 配置（本插件）

示例见 `example/cluster/` 和 `doc/cluster.md`。

## 后续演进（方案 B）

如果需要动态服务发现、故障转移、负载均衡调度等“平台化集群能力”，可以引入 etcd/K8s 作为控制面状态库；这部分不属于当前务实方案。 
