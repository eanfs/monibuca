# 集群架构（推拉流 + 录制）

> 本文是 `plugin/cluster` 的设计基线，描述 Monibuca v5 集群方案的目标、关键代码事实、模块划分与分阶段交付。实施在 `feature/cluster` 分支上进行。
>
> 旧版"务实方案 A"见 `doc/cluster.md`（已废弃，仅保留参考）。

## 一、背景与目标

Monibuca v5 当前在 `eanfsv5` 分支上是单机服务。`v5` HEAD 在提交 `6b2c68da` 中已经把 cluster 插件删除；之前 `cluster-v5` 分支上残留的"静态对等体" 97 行版本是基于 apiroute 插件做 HTTP 转发的简化方案，与"按需跨节点拉流 + 集中录制"目标不匹配。本次**在 `eanfsv5` 上从零实现**新的 `plugin/cluster/`，不再沿用旧分支。

集群要解决两件具体的事：

1. **跨节点推拉流**：流发布在节点 A，节点 B 出现订阅者时，节点 B 自动从 A 把流拉过来，作为本地 Publisher 注册，所有 B 上的订阅者复用这一份。Origin 失联时该流终止，不做热备。
2. **跨节点录制**：录制只在持有原始流的节点上跑（避免每个 relay 节点重复录、避免与 origin 解耦后产生孤儿文件），但任意节点上发起的录制 API（开始/停止/列表/下载）都要被路由到正确节点。

经过两轮代码深挖确认：**所有底层机制都已经具备**，cluster 插件只是把它们粘起来——这是设计的关键前提。

## 二、关键代码事实（决定方案形状）

### 2.1 拉流闭环已经现成

完整链路（每一步都已有实现）：

```
EnsurePullProxy(cfg)                    pull_proxy.go:282-319
  └─ 内存中创建/复用 PullProxy（按 streamPath 去重）
  └─ ChangeStatus(Online)               pull_proxy.go:98-114
        └─ 触发 BasePullProxy.Pull()    pull_proxy.go:147-152
              └─ 调用 plugin.handler.Pull(streamPath, pullConf, &pubConf)
                    └─ PullJob.Init/Publish     puller.go:89-176
                          └─ Plugin.PublishWithConfig    plugin.go:548-567
                                └─ Server.Streams.AddTask(publisher).WaitStarted()
                                      └─ Publisher 注册到 Streams  publisher.go:151
                                            └─ WaitManager.WakeUp(streamPath, pub)  wait_stream.go:34
                                                  └─ 等待中的订阅者被 publisher.AddSubscriber 接走  publisher.go:260-288
```

**结论**：cluster 插件不需要写任何拉流细节代码，调一次 `Server.EnsurePullProxy` 即可。多个本地订阅者天然复用同一个 pull-proxy Publisher（一次跨节点拉，本地多客户端共享）。

### 2.2 OnSubscribe 钩子是天然切入点

订阅者到达若本地无对应 Publisher：

```
subscriber.go:117  if publisher, ok := server.Streams.Get(streamPath); ok { ... }
subscriber.go:121  } else {
                       server.Waiting.Wait(s)              // 订阅者入队，不阻塞
subscriber.go:125      server.OnSubscribe(streamPath, args)
                   }
server.go:828-841  Server.OnSubscribe 同步分发到所有 ISubscribeHookPlugin
plugin.go:97-99    ISubscribeHookPlugin interface { OnSubscribe(streamPath, args) }
plugin.go:460-475  分发实现
```

**结论**：cluster 实现 `ISubscribeHookPlugin` 即可被自动调用；订阅者已在等待队列里，钩子可以同步走 etcd 查询和 EnsurePullProxy 调用。

### 2.3 录制必须留在 origin 节点

代码事实：

- `Recorder.Run()` 在 `record.go:229-398` 通过 `recoder.go:95` 的 `SubscribeTypeVod` 订阅本地流。前置条件 `Server.Streams.SafeGet(streamPath)` 拿到的是**本节点的 Publisher**。
- 文件由 `os.Create()` 在 `record.go:200` 就地写入，文件句柄绑死在 Recorder task 内，无法跨进程迁移。
- 录制元数据 `p.DB.Save(&r.stream)` 在 `record.go:218` 写入**本节点 DB**。

如果在 relay 节点上录制（relay 节点的 pull-proxy Publisher 也是合法 Publisher），会出现两个问题：

- 每个 relay 节点都会录一份，拷贝爆炸；
- origin 失联时 relay 的 pull-proxy 退出，导致 trailer 写不完整、文件孤儿。

所以**录制只能跑在 origin**，cluster 必须把任何节点收到的录制 API 路由回 origin。

### 2.4 录制 API 是独立 gRPC 服务

`plugin/mp4/pb/mp4.proto:9-43` 定义了 6 个 unary RPC：`StartRecord`、`StopRecord`、`EventStart`、`List`、`Catalog`、`Delete`。HTTP 端点经 grpc-gateway 暴露在 `plugin/mp4/index.go`，下载（回看）端点 `/download/{streamPath...}` 在 `plugin/mp4/index.go:77-81`。请求里都带 streamPath，可以由 streamPath 决定路由目标。

### 2.5 存储已经为多机准备好了

`pkg/storage/` 抽象支持 S3/COS/OSS（构建 tag `s3`/`cos`/`oss`），`File.Close()` 触发上传，重试集中在 `pkg/storage/retry.go:UploadWithRetry()`。**把录制目标设为共享对象存储 + 录制元数据共享到中心化 PostgreSQL（构建 tag `postgres`，已有支持），跨节点回看就从一项工程降为配置题**。这极大缩减了 cluster 的代码量。本方案统一选 PostgreSQL 作为集群部署推荐 DB。

## 三、总体架构

新增 `plugin/cluster/`，包含五个职责模块：

| 模块 | 任务类型 | 职责 |
|---|---|---|
| 成员管理 `Membership` | `task.Work` + 子 `task.Task` | etcd 注册自身（lease + keepalive），watch `/m7s/nodes/`，维护 `peers map[nodeID]*PeerInfo` |
| 流位置注册 `StreamRegistry` | `task.Work` + 子 `task.Task` | 监听 `OnPublish`/publisher dispose，把 `streamPath → nodeID` 写到 `/m7s/streams/`（同 lease，节点死流自动清）；watch 同前缀维护本地 `streams map[streamPath]nodeID` |
| 拉流 relay `Relay` | `ISubscribeHookPlugin` | `OnSubscribe(streamPath, args)` 时查 streams 找 origin、查 peers 找端口、按协议优先级拼 URL、调 `Server.EnsurePullProxy` |
| 录制路由 `RecordRoute` | gRPC unary interceptor + HTTP 中间件 | 拦截 mp4 录制相关方法，按 streamPath 路由到 origin 节点；下载端点做 302 兜底 |
| 负载上报 `LoadReporter` | `task.TickTask` | 周期更新 etcd 节点条目的 metrics 字段（流数、订阅数、CPU、带宽） |

**配套要求**：

- 推荐部署：所有节点共享一个 etcd（成员/流位置）、一个 PostgreSQL（业务 + 录制元数据）、一份对象存储（录制文件）。构建时带 `postgres` tag。
- 不引入新的节点间媒体传输协议。跨节点拉流就用对端已经在监听的 RTSP/RTMP/HTTP-FLV 端口（直接复用 PullerFactory）。

## 四、详细设计

### 4.1 跨节点拉流

**触发**：`ClusterPlugin.OnSubscribe(streamPath, args)`。

**步骤**：

1. 查本地 `streams[streamPath]`，得到 origin 的 `nodeID`。若空，说明全集群没有这条流，直接返回（订阅者会在等待队列超时）。
2. 校验 `nodeID != self.NodeID`（兜底，正常不会触发，因为本地有流时 `OnSubscribe` 不会被调）。
3. 查 `peers[nodeID]`，拿到对端 advertise 的协议端口表 `{rtmp, rtsp, flv, grpc, ...}`。
4. 按 `ClusterConfig.RelayProtocols`（默认 `["rtmp","rtsp","flv"]`）依次找对端有的第一个协议。
5. 拼 URL，例如 `rtmp://10.0.0.5:1935/<streamPath>`。
6. 构造 `PullProxyConfig`（type、url、streamPath、status=Online、PullOnStart=false、StopOnIdle=true、MaxRetry=3、RetryInterval=1s），并打上 `cluster-relay` 标记。
7. 调 `s.Server.EnsurePullProxy(cfg)`。底层完成 Pull→Publisher 注册→唤醒等待订阅者。

**多订阅者并发**：`EnsurePullProxy` 内部去重（同 streamPath 已存在则返回已有 proxy），所以多个订阅者同一时刻到达只会触发一次跨节点拉。

**自动空闲清理**：所有本地订阅者离开后，`StopOnIdle=true` 让 pull-proxy Publisher 在 `DelayCloseTimeout=5s`（`pull_proxy.go:150`）后停止（`publisher.go:253-256`），自动释放跨节点连接。

**避免环回**：B 从 A 拉到流后会在本地触发 `OnPublish`，`StreamRegistry` 默认会把它写进 etcd——**必须避免**，否则 C 上的订阅者会去 B 拉，B 又从 A 拉，链膨胀。判断方式：cluster 在调用 `EnsurePullProxy` 时给 `PullProxyConfig` 打一个标签字段 `Origin=<originNodeID>`（非持久化、非 DB 字段），`StreamRegistry.OnPublish` 看到带标签的 publisher 直接跳过。

### 4.2 跨节点录制路由

**问题**：用户在节点 B 调 `POST /mp4/api/start/<streamPath>`，但流 origin 在节点 A。`mp4/api.go:374` 调 `p.Server.Streams.SafeGet(...)`，要么本地无（直接报错），要么找到 B 自己的 pull-proxy Publisher（不能在它上面录制——见 §2.3）。

**解决方案**：cluster 注册一个 gRPC unary interceptor + HTTP 路由前置中间件。

**gRPC 路径**：

```
请求到 B    /mp4.Api/StartRecord{StreamPath: live/foo}
   │
   ▼
cluster gRPC interceptor
   │  方法名命中白名单（StartRecord/StopRecord/EventStart/List/Catalog/Delete）
   │  从 req 解析出 stream_path 字段
   │
   ├── 查 streams[live/foo] == self.NodeID  → 交给本地 mp4 处理
   │
   └── 查 streams[live/foo] == nodeID-A     → 用 connPool[A].Invoke 转发整个 RPC
                                              把 reply 原样回传给客户端
```

**HTTP 路径**：mp4 通过 grpc-gateway 暴露 `/mp4/api/*`，cluster 在 plugin 入口注册一个高优先级 handler，做相同的 streamPath 解析与反向代理（最简单：用 `httputil.NewSingleHostReverseProxy` 代理到 origin 的 HTTP 端口）。

**`/download/<streamPath>` 端点的差异**：

- 流仍 active：origin 节点必有该流，按当前 streams 表 302 到 origin 的下载链接。
- 流已结束、查历史录制：录制元数据落在原始节点的本地 DB 上——这是当前的最大缺陷。**强制要求**部署时使用共享 PostgreSQL，录制元数据自然跨节点可见，下载请求查 DB 拿到对象存储 key 直接走 S3/COS/OSS。
- 不愿配共享存储的退路：cluster 在历史录制 `/download` 上做 302→origin（要求 origin 在线），文档化此约束。

**选择 unary interceptor 而非自己起一个新 gRPC server 的原因**：interceptor 只需要在已有 gRPC server 上挂一个 chain，不破坏 mp4 插件的现有路由表，对 mp4 插件零侵入。

**核心改动一处**：`plugin.go` 的 `PluginMeta` 上加 `RegisterGRPCInterceptor` 钩子，`server.go:417-419` 处把所有插件提供的 interceptor 串成 `ChainUnaryInterceptor` 注册到 gRPC server。这是整个方案对核心代码唯一的修改，影响面极小。

### 4.3 节点身份与发现

**配置结构**（`plugin/cluster/config.go`）：

```yaml
cluster:
  nodeid: node-1                     # 必填，全局唯一
  etcd:
    - http://etcd-1:2379
    - http://etcd-2:2379
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

注意 YAML 字段必须全小写（`CLAUDE.md` 「YAML Config Rules」）。

**etcd 键空间**：

```
/m7s/nodes/<nodeID>      = JSON{advertise, metrics, version, started_at}    （TTL 10s lease）
/m7s/streams/<streamPath> = <nodeID>                                          （同 lease）
```

**生命周期**：

- 启动 → 建 lease（TTL 10s，每 3s keepalive）→ 写 `/m7s/nodes/<self>`。
- watch `/m7s/nodes/`：put/delete 实时同步到本地 `peers` map。
- 本地 `OnPublish`（且非 cluster-relay 派生）→ 写 `/m7s/streams/<path>`，绑同一个 lease。
- publisher dispose → 从 etcd delete。
- 节点崩溃 → lease 过期，etcd 自动 delete 本节点的所有键 → 全集群感知到。

### 4.4 健康检查与故障转移

完全依赖 etcd lease，无额外心跳机制。语义如下：

- **origin 死**：lease 过期 → `/m7s/streams/<path>` 自动消失 → relay 节点上 pull-proxy 收 EOF/timeout，`MaxRetry` 用尽后 publisher 退出 → 本地订阅者收到流结束。
- **录制中 origin 死**：录制中止，trailer 写入失败，已写入的 mdat 部分仍可播放。文档化此行为。
- **不做自动续播 / 流复制**。每条流单一 origin，origin 死则该流死。

### 4.5 发布者负载均衡（最小实现）

不在协议层做透明重定向（改 RTSP/RTMP 协议栈复杂度高、协议间不一致）。最小实现：

- `LoadReporter` 周期把 `{streams_count, subscribers_count, goroutines, ingress_bytes_per_sec}` 写到 etcd 节点条目的 metrics 字段。
- 暴露 `/api/cluster/lb-suggest` HTTP 接口，外部 LB（DNS/Nginx）调用得到当前最闲节点。
- v2 增量（不在本期范围）：在 `plugin/rtsp/pkg/connection.go` 的 `ANNOUNCE` 处理处加一个钩子，本节点过载时返回 302 到推荐节点。

## 五、文件清单

### 新建

| 文件 | 作用 |
|---|---|
| `plugin/cluster/index.go` | `ClusterPlugin` 主体；`InstallPlugin[ClusterPlugin]`；实现 `IPlugin` + `IPublishHookPlugin` + `ISubscribeHookPlugin` |
| `plugin/cluster/config.go` | `ClusterConfig` 与默认值（YAML tags） |
| `plugin/cluster/membership.go` | `Membership task.Work` + `MembershipWatcher task.Task`；etcd 注册、keepalive、peer watch |
| `plugin/cluster/streamregistry.go` | `StreamRegistry task.Work` + `StreamWatcher task.Task`；流位置写入与 watch；带 cluster-relay 跳过逻辑 |
| `plugin/cluster/relay.go` | `OnSubscribe` 实现：查询 + URL 拼装 + `EnsurePullProxy` 调用 |
| `plugin/cluster/route.go` | gRPC unary interceptor + HTTP 中间件；按 streamPath 路由 mp4 录制 API；conn 池 |
| `plugin/cluster/metrics.go` | `LoadReporter task.TickTask` |
| `plugin/cluster/lb.go` | `/api/cluster/{nodes,streams,lb-suggest}` HTTP 端点 |
| `plugin/cluster/pb/cluster.proto` | `NodeInfo`、`StreamLoc`、`LBSuggestion`（当前用作 etcd JSON 序列化结构 + 预留控制面 RPC）；用 `sh scripts/protoc.sh cluster` 生成 |
| `plugin/cluster/README.md` | 配置示例 + 多节点部署 + 共享存储建议 |

### 修改核心（最小化）

| 文件 | 改动 |
|---|---|
| `plugin.go` | `PluginMeta` 上加 `GRPCInterceptors []grpc.UnaryServerInterceptor` 字段（或 `RegisterGRPCInterceptor` 函数式钩子） |
| `server.go` | `:417-419` 注册 gRPC server 时拼上所有插件提供的 interceptor（`grpc.ChainUnaryInterceptor(...)`） |
| `pull_proxy.go` | `PullProxyConfig` 加非持久化字段 `Origin string \`gorm:"-"\``，cluster 用它打标 |

### 新建样例

| 文件 | 作用 |
|---|---|
| `example/cluster-fresh/main.go` | 复用 `example/default` 的插件清单 + 加 `_ "m7s.live/v5/plugin/cluster"` |
| `example/cluster-fresh/config-node-{1,2,3}.yaml` | 三节点配置，共享 etcd + PostgreSQL + S3 |
| `example/cluster-fresh/start.sh` | 假定外部 etcd 与 PostgreSQL 已就绪，本机三节点启动 |

## 六、任务系统约束（强制遵守 CLAUDE.md）

- `Membership`、`StreamRegistry` 必须用 `task.Work`（持续存在的队列管理器）。
- watcher、interceptor 状态机用 `task.Task`，由外部调 `Stop(reason)` 停止；**不允许重写 `Stop()`**。
- 周期上报用 `task.TickTask`。
- **绝对禁止裸 goroutine**，所有异步通过 `AddTask` 注册。
- etcd watch / connect 出错由 `task.SetRetry(MaxRetry, RetryInterval)` 自动重连，不在业务代码里写 retry loop。

## 七、阶段化交付（每阶段独立可用、可回滚）

1. **阶段 1 — 成员管理 + 监控接口**。`Membership` 完工，`/api/cluster/nodes` 可读。无业务行为变化，纯观察。
2. **阶段 2 — 流位置注册表**。`StreamRegistry` 完工，`/api/cluster/streams` 可读。仍无跨节点行为。
3. **阶段 3 — 跨节点拉流（最大业务价值）**。`OnSubscribe` 实现 + `EnsurePullProxy` 调用。两节点 RTMP 推流 / 跨节点订阅打通。
4. **阶段 4 — 录制 API 路由**。gRPC interceptor + HTTP 中间件；前提是文档要求共享 DB。
5. **阶段 5 — 共享存储 + 跨节点回看**。文档化 S3 + 共享 DB 配置；`/download` 拦截 + 302 兜底。
6. **阶段 6 — 负载暴露 + LB 建议**。`LoadReporter` + `/api/cluster/lb-suggest`。

每阶段都可独立 PR，互不依赖运行时（仅依赖代码骨架）。

## 八、端到端验证

环境：本机 etcd（`docker run -d -p 2379:2379 quay.io/coreos/etcd ...`）、共享 PostgreSQL、三节点（端口区分：RTMP 1935/1936/1937，RTSP 554/555/556，HTTP 8080/8081/8082，gRPC 50051/50052/50053）。

1. **启动**：`bash example/cluster-fresh/start.sh`。`etcdctl get --prefix /m7s/nodes/` 看到 3 条。
2. **推流到 node-1**：`ffmpeg -re -i sample.mp4 -c copy -f flv rtmp://localhost:1935/live/foo`。
3. **流位置已注册**：`etcdctl get /m7s/streams/live/foo` 显示 `node-1`。
4. **跨节点订阅 node-2**：`ffplay rtmp://localhost:1936/live/foo`，500ms 内出图。
5. **多消费者复用**：同时开 `ffplay` 到 node-2 和 node-3。`curl localhost:8081/api/streams` 显示 node-2 上 `live/foo` 仅一个 publisher（pull-proxy）、订阅数随客户端数量增长。
6. **跨节点录制**：在 node-3 上 `curl -XPOST localhost:8082/mp4/api/start/live/foo`。MP4 文件应落在 node-1 的录制目录；PostgreSQL 的 `mp4_streams` 表新增一行 `node_id=node-1`。
7. **跨节点列表**：`curl localhost:8082/mp4/api/list` 在 node-3 列出包含 node-1 的录制项（共享 DB）。
8. **跨节点回看**：`curl -L localhost:8082/download/live/foo.mp4?id=...` 应被 302 到 node-1，或直接从 S3 取（视配置）。
9. **故障**：`kill` node-1 进程。10s 内 `etcdctl get /m7s/streams/` 不再有 `live/foo`；node-2 的 pull-proxy publisher 在 retry 失败后退出。
10. **单元测试**：`plugin/cluster/membership_test.go`、`streamregistry_test.go` 用嵌入式 etcd（`go.etcd.io/etcd/server/v3/embed`）。`go test ./plugin/cluster/... -count=1`。

每次改动后强制：`gofmt -w <files>` → `go vet ./plugin/cluster/...` → `go test ./plugin/cluster/... -count=1`。最终回归：`go test ./...`。

## 九、关键复用点速查表

| 用途 | 文件:行 |
|---|---|
| 订阅者命中空流的钩子触发 | `subscriber.go:117-127` |
| OnSubscribe 分发 | `server.go:828-841`、`plugin.go:460-475` |
| `ISubscribeHookPlugin` 接口 | `plugin.go:97-99` |
| 内存中拉起 pull-proxy（去重 + 自动 Pull） | `pull_proxy.go:282-319` |
| Pull → Publisher 注册全链路 | `pull_proxy.go:147` → `puller.go:89-176` → `plugin.go:548-567` |
| Publisher 注册到 Streams | `publisher.go:139-177`（`Streams.Set` 在 151） |
| 等待订阅者的唤醒 | `wait_stream.go:34` |
| 多订阅者复用同一 Publisher | `publisher.go:260-288 AddSubscriber` |
| 录制 API（gRPC + HTTP） | `plugin/mp4/pb/mp4.proto:9-43`、`plugin/mp4/api.go:353-386` |
| 录制写盘 | `plugin/mp4/pkg/record.go:200, 229-398` |
| 下载/回看入口 | `plugin/mp4/index.go:77-81` |
| 存储抽象（S3/COS/OSS） | `pkg/storage/storage.go`、`pkg/storage/retry.go` |
| 任务系统约束 | `CLAUDE.md`「Critical Task Constraints」段 |

## 十、不在范围内（明确写出，避免误解）

- Raft / 共识 / Leader 选举。完全依赖 etcd lease。
- 流复制 / 跨节点热备 publisher。每条流单 origin。
- 跨地域集群。单 DC 假设。
- 新的节点间媒体传输协议（QUIC、gRPC streaming media）。沿用对端已有协议端口。
- RTMP / RTSP 协议栈级别的发布者透明重定向。延后到 v2。
- 与 `cluster-v5` 旧分支的兼容。完全重新实现。

## 十一、风险与缓解

| 风险 | 表现 | 缓解 |
|---|---|---|
| etcd 短暂不可用 | 注册不上、watch 中断 | `task.SetRetry` 自动重连；本地缓存 `peers`/`streams` 仍可用做降级 |
| 拉流环回 | A→B→C 链 | 用 `PullProxyConfig.Origin` 标签跳过 cluster-relay 派生的 publisher |
| 录制 API 转发风暴 | List/Catalog 大量请求 | 这两个方法不路由（聚合查共享 DB 即可），仅 Start/Stop/Delete/EventStart 路由 |
| 共享 DB 强依赖 | 没配共享 DB 时跨节点录制列表错乱 | 部署强制使用共享 PostgreSQL（构建带 `postgres` tag）。启动时若检测到 cluster 启用但 DB 是 SQLite，记 ERROR 日志并拒启动；DSN 里默认 sslmode=disable 只在内网，部署文档要求 PG 14+ |
| 节点 advertise 地址错误 | relay 拉不通 | 启动时 cluster 自我探测 advertise 端口可达性，失败拒绝 register |

## 十二、实施进度跟踪

实施在 `feature/cluster` 分支上进行，每个阶段一次提交（必要时拆为多个）：

- [ ] 阶段 1：成员管理 + `/api/cluster/nodes`
- [ ] 阶段 2：流位置注册表 + `/api/cluster/streams`
- [ ] 阶段 3：跨节点拉流 relay
- [ ] 阶段 4：录制 API 路由
- [ ] 阶段 5：跨节点回看 + 共享存储文档
- [ ] 阶段 6：负载上报 + LB 建议
- [ ] 端到端：`example/cluster-fresh/` 三节点验证脚本
