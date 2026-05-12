# 集群架构（推拉流 + 录制）

> 本文是 `plugin/cluster` 的设计基线，描述 Monibuca v5 集群方案的目标、关键代码事实、模块划分与分阶段交付。实施在 `feature/cluser2605` 分支上进行。
>
> 旧版"务实方案 A"（基于静态 peers + apiroute 插件）见 `doc/cluster.md`，已废弃。本方案保留 core 内的 `RouteInterceptor` 机制，但抽出 stream-locator 接口由 cluster 提供 Consul 后端实现，并删除 `plugin/apiroute/` 插件。

## 一、背景与目标

Monibuca v5 当前在 `eanfsv5` 分支上是单机服务。`v5` HEAD 在提交 `6b2c68da` 中已经把 cluster 插件删除；之前 `cluster-v5` 分支上残留的"静态对等体" 97 行版本是基于 apiroute 插件做 HTTP 转发的简化方案，与"按需跨节点拉流 + 集中录制"目标不匹配。本次**在 `eanfsv5` 上从零实现**新的 `plugin/cluster/`，不再沿用旧分支。

集群要解决两件具体的事：

1. **跨节点推拉流**：流发布在节点 A，节点 B 出现订阅者时，节点 B 自动从 A 把流拉过来，作为本地 Publisher 注册，所有 B 上的订阅者复用这一份。Origin 失联时该流终止，不做热备。
2. **跨节点录制**：录制只在持有原始流的节点上跑（避免每个 relay 节点重复录、避免与 origin 解耦后产生孤儿文件），但任意节点上发起的录制 API（开始/停止/列表/下载）都要被路由到正确节点。

经过两轮代码深挖确认：**所有底层机制都已经具备**，cluster 插件只是把它们粘起来——这是设计的关键前提。

## 二、关键代码事实（决定方案形状）

### 2.1 拉流闭环已经现成

完整链路：

```
EnsurePullProxy(cfg)                    pull_proxy.go (Server.EnsurePullProxy)
  └─ 内存中创建/复用 PullProxy（按 streamPath 去重，注释明确"does not persist anything to DB"）
  └─ ChangeStatus(Online)               pull_proxy.go (BasePullProxy.ChangeStatus)
        └─ 触发 BasePullProxy.Pull()
              └─ 调用 plugin.handler.Pull(streamPath, pullConf, &pubConf)
                    └─ PullJob.Init/Publish     puller.go
                          └─ Plugin.PublishWithConfig    plugin.go
                                └─ Server.Streams.AddTask(publisher).WaitStarted()
                                      └─ Publisher 注册到 Streams       publisher.go
                                            └─ WaitManager.WakeUp(streamPath, pub)  wait_stream.go
                                                  └─ 等待中的订阅者被 publisher.AddSubscriber 接走  publisher.go
```

**结论**：cluster 插件不需要写任何拉流细节代码，调一次 `Server.EnsurePullProxy` 即可。多个本地订阅者天然复用同一个 pull-proxy Publisher（一次跨节点拉，本地多客户端共享）。

> 注：本节及后续凡涉及 file:line 引用，仅给文件名 + 函数名。行号会随 main 漂移，调试时按符号定位即可。

### 2.2 OnSubscribe 钩子是天然切入点

订阅者到达若本地无对应 Publisher：

```
subscriber.go  Subscriber.Run()
  if publisher, ok := server.Streams.Get(streamPath); ok { ... }
  } else {
      server.Waiting.Wait(s)              // 订阅者入队，不阻塞
      server.OnSubscribe(streamPath, args)
  }
server.go      Server.OnSubscribe
plugin.go      ISubscribeHookPlugin interface { OnSubscribe(streamPath, args) }
```

**结论**：cluster 实现 `ISubscribeHookPlugin` 即可被自动调用；订阅者已在等待队列里，钩子可以同步走 Consul 查询和 EnsurePullProxy 调用。

### 2.3 录制必须留在 origin 节点

代码事实：

- `Recorder.Run()` 通过 `SubscribeTypeVod` 订阅本地流。前置条件 `Server.Streams.SafeGet(streamPath)` 拿到的是**本节点的 Publisher**。
- 文件由 `os.Create()` 就地写入，文件句柄绑死在 Recorder task 内，无法跨进程迁移。
- 录制元数据 `p.DB.Save(&r.stream)` 写入**本节点 DB**。

如果在 relay 节点上录制（relay 节点的 pull-proxy Publisher 也是合法 Publisher），会出现两个问题：

- 每个 relay 节点都会录一份，拷贝爆炸；
- origin 失联时 relay 的 pull-proxy 退出，导致 trailer 写不完整、文件孤儿。

所以**录制只能跑在 origin**，cluster 必须把任何节点收到的录制 API 路由回 origin。

### 2.4 录制 API 是独立 gRPC 服务

`plugin/mp4/pb/mp4.proto` 定义了 6 个 unary RPC：`StartRecord`、`StopRecord`、`EventStart`、`List`、`Catalog`、`Delete`。HTTP 端点经 grpc-gateway 暴露。下载（回看）端点 `/download/{streamPath...}` 在 `plugin/mp4/index.go`。请求里都带 streamPath，可以由 streamPath 决定路由目标。

### 2.5 存储已经为多机准备好了

`pkg/storage/` 抽象支持 S3/COS/OSS（构建 tag `s3`/`cos`/`oss`），`File.Close()` 触发上传，重试集中在 `pkg/storage/retry.go:UploadWithRetry()`。**把录制目标设为共享对象存储 + 录制元数据共享到中心化 PostgreSQL（构建 tag `postgres`，已有支持），跨节点回看就从一项工程降为配置题**。这极大缩减了 cluster 的代码量。本方案统一选 PostgreSQL 作为集群部署推荐 DB。

### 2.6 RouteInterceptor 与 RedirectAdvisor 已在核心实现

**新发现，决定整个 Phase 4 形状**：

- `grpc_api_route.go` 的 `Server.RouteInterceptor()` 已经是一个完整的 streamPath-based 路由 gRPC 拦截器：method 白名单、`extractStreamPath(req)`、`apiRouteForwardedMetaKey` forwarding loop 防护、cache、forward——全套。
- `server.go` 在创建 grpcServer 时已经把它接进 `ChainUnaryInterceptor`：

```go
s.grpcServer = grpc.NewServer(grpc.ChainUnaryInterceptor(
    s.AuthInterceptor(),
    s.RouteInterceptor(),
))
```

- `flv`、`rtsp`、`server_http.go` 通过 `RedirectAdvisor` / `RedirectAdvisorV2` 接口拿到"流在哪个节点"，做 HTTP/RTSP 播放重定向。
- 当前实现由 `plugin/apiroute/` 提供 stream-locator + RedirectAdvisor，数据源是**静态 peers + 跨节点 SysInfo 探测**。

**因此 cluster 不需要重写 RouteInterceptor，也不需要给 PluginMeta 加 GRPCInterceptors hook。** 只要：

1. 抽出 `m7s.StreamRouter` 接口，让 RouteInterceptor 通过它做 lookup/forward
2. cluster 插件实现 `StreamRouter` + `RedirectAdvisorV2`，后端为 Consul KV
3. 删除 `plugin/apiroute/`（功能被 cluster 完全覆盖）

## 三、总体架构

新增 `plugin/cluster/`，包含五个职责模块。**全部文件加 `//go:build cluster` 构建标签**，默认构建不引入 Consul 依赖。

| 模块 | 任务类型 | 职责 |
|---|---|---|
| 成员管理 `Membership` | `task.Work` + 子 `task.Task` | Consul 注册自身（session + DeleteOnInvalidate），watch `m7s/nodes/`（KV blocking query），维护 `peers map[nodeID]*PeerInfo`；session 失效时触发 `RebindAllStreams` |
| 流位置注册 `StreamRegistry` | `task.Work` + 子 `task.Task` | 监听 `OnPublish`/publisher dispose，把 `streamPath → nodeID` 写到 `m7s/streams/`（用 `Acquire` 绑定 session，节点死流自动清）；watch 同前缀维护本地 `streams sync.Map` |
| 拉流 relay `Relay` | `ISubscribeHookPlugin` | `OnSubscribe(streamPath, args)` 时查 streams 找 origin、查 peers 找端口、按协议优先级拼 URL、调 `Server.EnsurePullProxy` |
| Stream 路由 `StreamLocator` | 实现 `m7s.StreamRouter` + `RedirectAdvisorV2` | 给 core 的 `RouteInterceptor` 提供 `Resolve(streamPath) → target` 与 `Forward(method, req, target)`；给 flv/rtsp/server_http 提供播放重定向 |
| 负载上报 `LoadReporter` | `task.TickTask` | 周期更新 Consul 节点条目的 metrics 字段（流数、订阅数、CPU、带宽） |

**配套要求**：

- 推荐部署：所有节点共享一个 Consul Server 集群（成员/流位置）、一个 PostgreSQL（业务 + 录制元数据）、一份对象存储（录制文件）。构建 cluster 时带 `-tags "cluster postgres"`（如需 S3 再加 `s3`）。
- 不引入新的节点间媒体传输协议。跨节点拉流就用对端已经在监听的 RTSP/RTMP/HTTP-FLV 端口（直接复用 PullerFactory）。

## 四、详细设计

### 4.1 跨节点拉流

**触发**：`ClusterPlugin.OnSubscribe(streamPath, args)`。

**步骤**：

1. 查本地 `streams[streamPath]`，得到 origin 的 `nodeID`。若空，说明全集群没有这条流，直接返回（订阅者会在等待队列超时）。
2. 校验 `nodeID != self.NodeID`（兜底，正常不会触发，因为本地有流时 `OnSubscribe` 不会被调）。
3. 查 `peers[nodeID]`，拿到对端 advertise 的协议端口表 `{rtmp, rtsp, flv, grpc, ...}`。
4. 按 `ClusterConfig.RelayProtocols`（默认 `["rtmp","rtsp","flv"]`）依次找对端有的第一个协议。
5. 用 `buildPullURL(proto, peer, streamPath)` 构造 URL（规则见 §4.1.1）。
6. 构造 `PullProxyConfig`（type、url、streamPath、status=Online、PullOnStart=false、StopOnIdle=true、MaxRetry=3、RetryInterval=1s）。
7. 调 `s.Server.EnsurePullProxy(cfg)`。底层完成 Pull→Publisher 注册→唤醒等待订阅者。
8. 拿到 `pullProxy` 后立即 `pullProxy.GetBase().SetDescription("origin", originNodeID)`，作为 cluster-relay 标记（避免改 `PullProxyConfig` struct）。

#### 4.1.1 URL 构造规则（A4）

抽 `plugin/cluster/relay.go:buildPullURL(proto, peer *PeerInfo, streamPath string) (string, error)`，按协议拆开：

| 协议 | 模板 | streamPath 处理 |
|---|---|---|
| rtmp | `rtmp://<host:port>/<streamPath>` | path-escape 单段（`url.PathEscape`），保留 `/` 不转义；空 streamPath 拒绝 |
| rtsp | `rtsp://<host:port>/<streamPath>` | 同上 |
| flv | `<scheme>://<host:port>/<streamPath>.flv` | 同上；scheme 由 advertise.flv 给出（http / https） |

**约束**：`streamPath` 中的 `/` 视为路径分隔符不转义；其它特殊字符（中文、空格、`&`、`?`、`#`）必须 `url.PathEscape`。表驱动单元测试（见 §三测试要求）。

**多订阅者并发**：`EnsurePullProxy` 内部去重（同 streamPath 已存在则返回已有 proxy），所以多个订阅者同一时刻到达只会触发一次跨节点拉。

**自动空闲清理**：所有本地订阅者离开后，`StopOnIdle=true` 让 pull-proxy Publisher 在 `DelayCloseTimeout=5s` 后停止，自动释放跨节点连接。

**避免环回（Q2）**：B 从 A 拉到流后会在本地触发 `OnPublish`，`StreamRegistry` 默认会把它写进 Consul——**必须避免**，否则 C 上的订阅者会去 B 拉，B 又从 A 拉，链膨胀。判断方式：
- relay 创建 pull-proxy 后调 `pullProxy.GetBase().SetDescription("origin", originNodeID)`。
- `StreamRegistry.OnPublish(pub)` 检查 `pub.PullProxy != nil && pub.PullProxy.GetBase().GetDescription("origin") != ""`，是的话直接 return，不写 Consul。

> 不在 `PullProxyConfig` struct 上加 `Origin` 字段——`PullProxyConfig` 是 server-level config（`server.go: PullProxy []*PullProxyConfig`），走 yaml/DB 持久化，加非持久化字段同时需要 `gorm:"-"` 和 `yaml:"-"`，污染 schema。`SetDescription`/`GetDescription` 是 `task.Task` 现成的 string-keyed metadata，零结构改动。

### 4.2 跨节点录制路由（D2=A：复用现有 RouteInterceptor）

**问题**：用户在节点 B 调 `POST /mp4/api/start/<streamPath>`，但流 origin 在节点 A。`mp4/api.go:StartRecord` 调 `p.Server.Streams.SafeGet(...)`，要么本地无（直接报错），要么找到 B 自己的 pull-proxy Publisher（不能在它上面录制——见 §2.3）。

**解决方案（轻量改造）**：

```
                               +---------------------------+
                               | grpc_api_route.go         |
                               |  RouteInterceptor()       |  ← 已有，不动
                               |  · methods 白名单          |
                               |  · extractStreamPath      |
                               |  · forwarding loop 防护    |
                               |  · cache                   |
                               +-----------┬---------------+
                                           │ s.streamRouter()
                                           ▼
                               +---------------------------+
                               | m7s.StreamRouter (新接口) |
                               |  Resolve(streamPath)       |
                               |  Forward(method,req,target)|
                               +-----------┬---------------+
                                           ▲
                                           │ 实现
                               +---------------------------+
                               | plugin/cluster.StreamLoc  |
                               |  · Resolve: 查 Consul KV   |
                               |  · Forward: gRPC conn 池   |
                               |  · 同时实现                |
                               |    RedirectAdvisorV2       |
                               +---------------------------+
```

**核心改动**：

1. **`grpc_api_route.go`**：把硬编码调用 `s.apiRouter()`（返回 `*APIRoutePlugin`）改为 `s.streamRouter()` 返回 `m7s.StreamRouter` 接口。
2. **`server.go`**：新增 `Server.SetStreamRouter(r m7s.StreamRouter)`；cluster 插件 Start 时调用注册自己。
3. **`plugin.go`**：新增 `m7s.StreamRouter` 接口定义（`Resolve`、`Forward`）和 `m7s.RedirectAdvisorV2` 接口（已存在的话直接复用）。
4. **删除 `plugin/apiroute/`** 整目录。其 `getConn` 连接池实现搬到 `plugin/cluster/streamlocator.go` 复用。
5. **`pkg/config/types.go`**：删除 `APIRoute` 配置结构；新增 `Cluster` 配置结构（见 §4.3）。

**HTTP 路径**：cluster 实现 `RedirectAdvisorV2`（`flv`、`rtsp`、`server_http.go` 已经会自动调），返回 `(targetHost, 302, true)`。无需新中间件。

**`/download/<streamPath>` 端点**：

- 流仍 active：cluster 的 `RedirectAdvisorV2` 自动 302 到 origin。
- 流已结束、查历史录制：录制元数据落在原始节点的本地 DB——这是**唯一** real 缺陷。**部署强烈建议**使用共享 PostgreSQL，元数据自然跨节点可见，`/download` 直接查 DB 拿对象存储 key 走 S3/COS/OSS。
- 不愿配共享存储的退路：cluster 在 `/download` 上做 302→origin（要求 origin 在线），文档化此约束。**SQLite + cluster 启动只 WARN log 不拒启动**（A3：方便开发/测试场景，部署文档明确写"生产必须共享 PG"）。

### 4.3 节点身份与发现（Consul 后端）

**配置结构**（`plugin/cluster/config.go`）：

```yaml
cluster:
  nodeid: node-1                     # 必填，全局唯一
  consul:
    addresses:                        # Consul Server HTTP 地址，多个用做 client 端 failover
      - http://consul-1:8500
      - http://consul-2:8500
    token: ""                         # 可选，Consul ACL token
    waittime: 30s                     # KV blocking query 长轮询超时（P1）
    sessionttl: 10s                   # Consul session TTL（≥ 10s 是 Consul 硬性下限）
    sessionrenewinterval: 3s          # session 主动续期间隔
  advertise:
    rtmp: 10.0.0.5:1935
    rtsp: 10.0.0.5:554
    flv: http://10.0.0.5:8080
    grpc: 10.0.0.5:50051
  relayprotocols: [rtmp, rtsp, flv]   # 跨节点拉流协议优先级
  loadshed:
    enable: false
    streamthreshold: 500
```

YAML 字段必须全小写（CLAUDE.md「YAML Config Rules」）。

**Consul KV 键空间**：

```
m7s/nodes/<nodeID>      = JSON{advertise, metrics, version, started_at}    (Acquire 到 session)
m7s/streams/<streamPath> = <nodeID>                                          (同 session)
```

**关键 API 选择**：

- 注册键不用 `client.KV().Put(...)` 而用 `client.KV().Acquire(&api.KVPair{..., Session: sid}, nil)`。`Acquire` 才会让 `behavior=delete` 的 session 在失效时自动 delete 关联 keys；普通 `Put` 不会。
- session 创建时 `behavior=delete`、`ttl=10s`、`lockdelay=0`（避免 Consul 默认 15s lock-delay 干扰）。
- watch 用 `client.KV().List("m7s/nodes/", &api.QueryOptions{WaitIndex: lastIndex, WaitTime: 30s, AllowStale: false})`。返回后取 `meta.LastIndex` 作为下次 WaitIndex；blocking query 没事件时会 30s 后超时返回（不变更），看到 LastIndex 没变就直接重新发起。

**生命周期**：

- 启动 → 创建 session（`behavior=delete`，TTL 10s）→ 启动 session 续期 task（每 3s `Renew`）→ `Acquire` 写 `m7s/nodes/<self>`。
- watch `m7s/nodes/`：blocking query 循环（task.Task 内），put/delete 事件实时同步到本地 `peers` map。
- 本地 `OnPublish`（且非 cluster-relay 派生）→ `Acquire` 写 `m7s/streams/<path>`，绑同一个 session。
- publisher dispose → 显式 `Release` + `Delete`。
- 节点崩溃 / 网络中断 > TTL → session invalidate → Consul 自动 delete 本节点所有 Acquire 的 keys → 全集群通过 watch 感知到。

#### 4.3.1 Session 失效后的本地流重注册（A1）

**关键场景**：节点进程没死，但网络抖动 > 10s 让 Consul 撤销了 session。所有 `m7s/streams/*` 键被 Consul 自动删，对集群其它节点本节点的流变得隐形。

**处理流程**：

```
Membership task 监听 session.Renew() 返回的 doneCh
   ↓
doneCh close (session 失效)
   ↓
Membership.RebuildSession():
   1. session = client.Session().Create(...)
   2. Acquire m7s/nodes/<self> 用新 session
   3. 调用 StreamRegistry.RebindAllStreams()
        ↓
        StreamRegistry 遍历 Server.Streams.Range:
          for each pub where pub.PullProxy 没有 origin description:
            client.KV().Acquire({Key: "m7s/streams/" + pub.StreamPath, Value: nodeID, Session: newSid})
```

**测试要求（CRITICAL）**：模拟 5 秒断连，验证恢复后 `m7s/streams/*` 再次出现，且 `peers` map 里 self 也重新出现。

### 4.4 健康检查与故障转移

完全依赖 Consul session 机制，无额外心跳。语义如下：

- **origin 死**：session invalidate → `m7s/streams/<path>` 自动消失 → relay 节点上 pull-proxy 收 EOF/timeout，`MaxRetry` 用尽后 publisher 退出 → 本地订阅者收到流结束。
- **录制中 origin 死**：录制中止，trailer 写入失败，已写入的 mdat 部分仍可播放。文档化此行为。
- **不做自动续播 / 流复制**。每条流单一 origin，origin 死则该流死。

### 4.5 发布者负载均衡（最小实现）

不在协议层做透明重定向（改 RTSP/RTMP 协议栈复杂度高、协议间不一致）。最小实现：

- `LoadReporter` 周期（默认 5s）把 `{streams_count, subscribers_count, goroutines, ingress_bytes_per_sec}` 写到 Consul 节点条目的 metrics 字段。
- 暴露 `/api/cluster/lb-suggest` HTTP 接口，外部 LB（DNS/Nginx）调用得到当前最闲节点。
- v2 增量（不在本期范围）：在 `plugin/rtsp/pkg/connection.go` 的 `ANNOUNCE` 处理处加一个钩子，本节点过载时返回 302 到推荐节点。

> 性能边界（P3）：`LoadReporter` 是 Acquire 写，每次走 Raft commit。100 节点集群 5s 周期 = 20 QPS 写，远低于 Consul Server 单机几千 QPS 写吞吐。如果未来要扩到 1000+ 节点，可以加聚合层。

## 五、文件清单

### 新建

| 文件 | 作用 |
|---|---|
| `plugin/cluster/index.go` | `ClusterPlugin` 主体；`InstallPlugin[ClusterPlugin]`；`//go:build cluster`；实现 `IPlugin` + `IPublishHookPlugin` + `ISubscribeHookPlugin` + `m7s.StreamRouter` + `m7s.RedirectAdvisorV2` |
| `plugin/cluster/config.go` | `ClusterConfig` 与默认值（YAML tags 全小写） |
| `plugin/cluster/membership.go` | `Membership task.Work` + 子 task：session 创建/续期、节点 watcher、`RebindAllStreams` 触发 |
| `plugin/cluster/streamregistry.go` | `StreamRegistry task.Work` + 子 task：流位置 Acquire/Release、watch；带 cluster-relay 跳过逻辑（基于 SetDescription） |
| `plugin/cluster/relay.go` | `OnSubscribe` 实现 + `buildPullURL`（表驱动）+ `EnsurePullProxy` 调用 + origin 标记 |
| `plugin/cluster/streamlocator.go` | `StreamRouter` 接口实现：Resolve（查本地 streams sync.Map） + Forward（gRPC conn 池，复用 apiroute 的 getConn 实现）；`RedirectAdvisorV2` 实现 |
| `plugin/cluster/metrics.go` | `LoadReporter task.TickTask` |
| `plugin/cluster/lb.go` | `/api/cluster/{nodes,streams,lb-suggest}` HTTP 端点 |
| `plugin/cluster/pb/cluster.proto` | `NodeInfo`、`StreamLoc`、`LBSuggestion`（用作 Consul JSON 序列化结构 + 预留控制面 RPC）；用 `sh scripts/protoc.sh cluster` 生成 |
| `plugin/cluster/README.md` | 配置示例 + 多节点部署 + 共享存储建议 + Consul 部署提示 |

### 修改核心（最小化）

| 文件 | 改动 |
|---|---|
| `plugin.go` | 新增 `m7s.StreamRouter` 接口（`Resolve`、`Forward`），如缺失补 `RedirectAdvisorV2` |
| `server.go` | 新增 `Server.SetStreamRouter(r m7s.StreamRouter)` 与 `Server.streamRouter()`；删除/改写老 `apiRouter()` 调用 |
| `grpc_api_route.go` | 把 `s.apiRouter()` 调用改为 `s.streamRouter()`；保留 forwarding loop / method 白名单 / cache 等成熟逻辑；删除 `apiRoutePeers` 相关 helpers |
| `pkg/config/types.go` | 删除 `APIRoute` 结构；新增 `Cluster` 顶级配置结构 |
| `plugin/apiroute/` | **整目录删除** |

> 注意：原方案要在 `PluginMeta` 加 `GRPCInterceptors` hook、`PullProxyConfig` 加 `Origin` 字段——**两个都不要做**，分别由 D2=A 复用现有 chain 和 Q2 用 `SetDescription` 替代。

### 新建样例

| 文件 | 作用 |
|---|---|
| `example/cluster-fresh/main.go` | 复用 `example/default` 的插件清单 + 加 `_ "m7s.live/v5/plugin/cluster"` |
| `example/cluster-fresh/config-node-{1,2,3}.yaml` | 三节点配置，共享 Consul + PostgreSQL + S3 |
| `example/cluster-fresh/start.sh` | 假定外部 Consul + PostgreSQL 已就绪，本机三节点启动；带 `-tags "cluster postgres s3"` |

## 六、任务系统约束（强制遵守 CLAUDE.md）

- `Membership`、`StreamRegistry` 必须用 `task.Work`（持续存在的队列管理器）。
- session 续期、watcher、blocking query 循环用 `task.Task`，由外部调 `Stop(reason)` 停止；**不允许重写 `Stop()`**。
- 周期上报用 `task.TickTask`。
- **绝对禁止裸 goroutine**，所有异步通过 `AddTask` 注册。`plugin/apiroute/index.go:Start()` 现存的 `go func() { warmup }()` 在删除 apiroute 时一并消失。
- Consul connect / KV 操作出错由 `task.SetRetry(MaxRetry, RetryInterval)` 自动重连，不在业务代码里写 retry loop。

## 七、阶段化交付（每阶段独立可用、可回滚）

1. **阶段 1 — 成员管理 + 监控接口**。`Membership` 完工，`/api/cluster/nodes` 可读。无业务行为变化，纯观察。
2. **阶段 2 — 流位置注册表**。`StreamRegistry` 完工，`/api/cluster/streams` 可读。仍无跨节点行为。
3. **阶段 3 — 跨节点拉流（最大业务价值）**。`OnSubscribe` 实现 + `EnsurePullProxy` 调用 + URL 构造 + 环回防护。两节点 RTMP 推流 / 跨节点订阅打通。
4. **阶段 4 — 录制 API 路由 + 删除 apiroute**。`StreamLocator` 实现 `StreamRouter` + `RedirectAdvisorV2`；`grpc_api_route.go` 切换到接口；`plugin/apiroute/` 删除；`pkg/config/types.go` 替换 `APIRoute` → `Cluster`。
5. **阶段 5 — 共享存储 + 跨节点回看**。文档化 S3 + 共享 DB 配置；`/download` 拦截 + 302 兜底；SQLite 启动只 WARN。
6. **阶段 6 — 负载暴露 + LB 建议**。`LoadReporter` + `/api/cluster/lb-suggest`。

每阶段都可独立 PR，互不依赖运行时（仅依赖代码骨架）。

## 八、端到端验证

环境：本机 Consul（`docker run -d -p 8500:8500 hashicorp/consul agent -dev -client=0.0.0.0`）、共享 PostgreSQL、三节点（端口区分：RTMP 1935/1936/1937，RTSP 554/555/556，HTTP 8080/8081/8082，gRPC 50051/50052/50053）。

1. **启动**：`bash example/cluster-fresh/start.sh`。`consul kv get -recurse m7s/nodes/` 看到 3 条。
2. **推流到 node-1**：`ffmpeg -re -i sample.mp4 -c copy -f flv rtmp://localhost:1935/live/foo`。
3. **流位置已注册**：`consul kv get m7s/streams/live/foo` 显示 `node-1`。
4. **跨节点订阅 node-2**：`ffplay rtmp://localhost:1936/live/foo`，500ms 内出图。
5. **多消费者复用**：同时开 `ffplay` 到 node-2 和 node-3。`curl localhost:8081/api/streams` 显示 node-2 上 `live/foo` 仅一个 publisher（pull-proxy）、订阅数随客户端数量增长。
6. **跨节点录制**：在 node-3 上 `curl -XPOST localhost:8082/mp4/api/start/live/foo`。MP4 文件应落在 node-1 的录制目录；PostgreSQL 的 `mp4_streams` 表新增一行 `node_id=node-1`。
7. **跨节点列表**：`curl localhost:8082/mp4/api/list` 在 node-3 列出包含 node-1 的录制项（共享 DB）。
8. **跨节点回看**：`curl -L localhost:8082/download/live/foo.mp4?id=...` 应被 302 到 node-1，或直接从 S3 取（视配置）。
9. **故障 — 进程死**：`kill` node-1 进程。10s 内 `consul kv get -recurse m7s/streams/` 不再有 `live/foo`；node-2 的 pull-proxy publisher 在 retry 失败后退出。
10. **故障 — 网络抖动 + session 失效重注册（A1 CRITICAL）**：在 node-1 用 `iptables -A OUTPUT -p tcp --dport 8500 -j DROP` 阻断 Consul 12 秒，恢复后断言 `consul kv get m7s/streams/live/foo` 仍指 node-1；relay 节点的拉流不中断（或 5s 内恢复）。
11. **单元测试**：
    - `plugin/cluster/membership_test.go` — 用 `consul/sdk/testutil` 起嵌入式 Consul，覆盖 session 失效重注册、watcher 同步
    - `plugin/cluster/relay_test.go` — `buildPullURL` 表驱动 + 环回防护
    - `plugin/cluster/streamregistry_test.go` — OnPublish 写入 + dispose 删除 + cluster-relay 跳过

每次改动后强制：`gofmt -w <files>` → `go vet -tags cluster ./plugin/cluster/...` → `go test -tags cluster ./plugin/cluster/... -count=1`。最终回归：`go test -tags cluster ./...`。

## 九、关键复用点速查表

| 用途 | 文件:符号 |
|---|---|
| 订阅者命中空流的钩子触发 | `subscriber.go:Subscriber.Run` |
| OnSubscribe 分发 | `server.go:Server.OnSubscribe`、`plugin.go: dispatchOnSubscribe`（见 IPublishHookPlugin/ISubscribeHookPlugin 附近） |
| `ISubscribeHookPlugin` 接口 | `plugin.go: ISubscribeHookPlugin` |
| 内存中拉起 pull-proxy（去重 + 自动 Pull） | `pull_proxy.go:Server.EnsurePullProxy`（注释明确 "does not persist anything to DB"） |
| Pull → Publisher 注册全链路 | `pull_proxy.go:BasePullProxy.Pull` → `puller.go:PullJob.Init/Publish` → `plugin.go:Plugin.PublishWithConfig` |
| Publisher 注册到 Streams | `publisher.go: Publisher.Init` 及 `Streams.Set` |
| 等待订阅者的唤醒 | `wait_stream.go:WaitManager.WakeUp` |
| 多订阅者复用同一 Publisher | `publisher.go:AddSubscriber` |
| **gRPC streamPath 路由（复用）** | `grpc_api_route.go:Server.RouteInterceptor`、`extractStreamPath`、`apiRouteForwardedMetaKey` |
| **HTTP/RTSP 播放重定向（复用）** | `m7s.RedirectAdvisorV2` 接口（被 `flv/api.go`、`rtsp/server.go`、`server_http.go` 调） |
| 录制 API（gRPC + HTTP） | `plugin/mp4/pb/mp4.proto`、`plugin/mp4/api.go:StartRecord` |
| 录制写盘 | `plugin/mp4/pkg/record.go:Recorder.Run` |
| 下载/回看入口 | `plugin/mp4/index.go: /download handler` |
| 存储抽象（S3/COS/OSS） | `pkg/storage/storage.go`、`pkg/storage/retry.go` |
| 任务系统约束 | `CLAUDE.md`「Critical Task Constraints」段 |

## 十、不在范围内（明确写出，避免误解）

- Raft / 共识 / Leader 选举。完全依赖 Consul session 机制。
- 流复制 / 跨节点热备 publisher。每条流单 origin。
- 跨地域集群。单 DC 假设。
- 新的节点间媒体传输协议（QUIC、gRPC streaming media）。沿用对端已有协议端口。
- RTMP / RTSP 协议栈级别的发布者透明重定向。延后到 v2。
- 与 `cluster-v5` 旧分支的兼容。完全重新实现。
- Consul ACL token 自动轮转 / Vault 集成。首版假设 token 静态或永不过期。
- Consul agent on every node + service mesh 模式。首版用外部 Server 集群直连。
- `apiroute` 插件的兼容性。该插件被 cluster 完全替代并删除。

## 十一、风险与缓解

| 风险 | 表现 | 缓解 |
|---|---|---|
| Consul 短暂不可用 | 注册不上、watch 中断 | `task.SetRetry` 自动重连；本地缓存 `peers`/`streams` 仍可用做降级；blocking query 失败超过 N 次只记 ERROR 不阻塞主流程 |
| Session 失效但进程未死 | `m7s/streams/*` 自动消失，本地流变隐形 | A1 重注册流程（§4.3.1）；CRITICAL 集成测试覆盖 |
| 拉流环回 | A→B→C 链 | `pullProxy.SetDescription("origin", ...)` + `StreamRegistry.OnPublish` 跳过 cluster-relay 派生的 publisher |
| 录制 API 转发风暴 | List/Catalog 大量请求 | 这两个方法不路由（聚合查共享 DB 即可），仅 Start/Stop/Delete/EventStart 路由 |
| 共享 DB 强依赖 | 没配共享 DB 时跨节点录制列表错乱 | 部署文档强烈推荐 PostgreSQL（构建带 `postgres` tag）。启动时检测到 cluster 启用但 DB 是 SQLite，**WARN 日志提示但不拒启动**（A3：测试/开发环境留口） |
| 节点 advertise 地址错误 | relay 拉不通 | 启动时 cluster 自我探测 advertise 端口可达性，失败拒绝 register |
| streamPath 含特殊字符 | URL 拼装错误 / Consul KV path 异常 | A4 `buildPullURL` 表驱动测试覆盖；Consul KV path 用 streamPath 字面值（Consul 接受 `/`），不做 escape |
| 跨节点 RPC 转发 timeout | 录制 API 卡住 | `ForwardTimeout` 配置（默认 5s）；超时返回 5xx + 上游错误码；客户端可重试 |

## 共享存储部署（Phase 5）

集群模式下，**强烈推荐** 所有节点共享一份业务 DB + 对象存储。否则跨节点
录制元数据和文件回看会不可见。

### PostgreSQL（业务 + 录制元数据）

所有节点的 DSN 指向同一 PostgreSQL 实例，m7s 启动时 GORM AutoMigrate
会建好 `mp4_streams` 等表（其中 `node_id` 列由 Phase 5 引入）。

docker-compose 示例：

```yaml
postgres:
  image: postgres:16-alpine
  environment:
    POSTGRES_PASSWORD: m7s
    POSTGRES_DB: m7s
  ports: ["5432:5432"]
```

m7s config 段：

```yaml
postgres:
  dsn: "postgres://postgres:m7s@postgres:5432/m7s?sslmode=disable"
```

build 时必须带 `postgres` tag。

### S3 / COS / OSS（录制文件）

录制文件统一上传到对象存储。任何节点的 `/download/...` handler 拿到
mp4_streams 行后，如果该行 node_id == 本节点，直接走对象存储；node_id
≠ 本节点 → 由 cluster plugin 注入的 DownloadHook 302 到该节点的 advertise.FLV 端口。

minio docker-compose 示例：

```yaml
minio:
  image: minio/minio:latest
  command: server /data --console-address ":9001"
  environment:
    MINIO_ROOT_USER: admin
    MINIO_ROOT_PASSWORD: m7sm7sm7s
  ports: ["9000:9000", "9001:9001"]
```

m7s config 段（s3 build tag）：

```yaml
storage:
  type: s3
  s3:
    endpoint: http://minio:9000
    accesskey: admin
    secretkey: m7sm7sm7s
    bucket: m7s-records
    region: us-east-1
```

### 部署一致性 checklist

- [ ] 所有节点 postgres.dsn 指向同一 PostgreSQL 实例
- [ ] 所有节点 storage 配置指向同一 bucket
- [ ] cluster.nodeid 全局唯一（不能重复）
- [ ] cluster.advertise.{rtmp,rtsp,flv,grpc} 是其他节点能访问的地址（不要写 127.0.0.1 这种本机回环）
- [ ] cluster.consul.addresses 指向同一 Consul 集群

### SQLite 兼容性

cluster + SQLite **不被推荐生产用** —— SQLite 是本地文件，跨节点不共享。
启动时 m7s 会 log Warn 提醒。"试用"场景可继续用，但 e2e + 录制回看
会受限。

## 十二、实施进度跟踪

实施在 `feature/cluser2605` 分支上进行，每个阶段一次或多次提交：

- [ ] 阶段 1：成员管理 + `/api/cluster/nodes`（Membership + Session 重注册 A1 + build tag A2）
- [ ] 阶段 2：流位置注册表 + `/api/cluster/streams`（StreamRegistry + 环回标记 Q2）
- [ ] 阶段 3：跨节点拉流 relay（OnSubscribe + URL 构造 A4）
- [ ] 阶段 4：StreamRouter 接口 + 删除 apiroute（D2=A 改造 + plugin/apiroute 删除 + APIRoute → Cluster 配置切换）
- [ ] 阶段 5：跨节点回看 + 共享存储文档（A3：SQLite 仅 WARN）
- [ ] 阶段 6：负载上报 + LB 建议（LoadReporter + `/api/cluster/lb-suggest`）
- [ ] 端到端：`example/cluster-fresh/` 三节点验证脚本 + Consul 嵌入式单测
