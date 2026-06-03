# Cluster v1 综合设计 spec

| 字段 | 值 |
|---|---|
| Date | 2026-05-12 |
| Branch | `feature/cluser2605` |
| Build tag | `cluster` |
| Phase 1+2 状态 | 已 commit + 8/8 tests 绿(commit `6145fdcf`) |
| Phase 3 状态 | `buildPullURL` 已 GREEN(4 tests),其余待按本 spec 重做 |
| Phase 4-6 + e2e | 待实现 |
| 参考文档 | `doc_CN/arch/cluster.md`(v0 架构总览,本 spec 取代细节决策) |

---

## 1. 架构概览

Cluster v1 把 N 个 Monibuca 节点用 Consul 串成一张轻量集群。**每条流仍有唯一 origin 节点**(物理上接住推流的那个),其余节点对该流变成自动按需 relay。所有节点共享一份业务 DB(PostgreSQL)和一份对象存储(S3/COS/OSS),录制元数据和录制文件全集群可见。

**无 leader、无共识、无流复制、无热备**。所有"集群感知"靠 Consul session lease。

### 1.1 拓扑

```
              ┌─────────────── Consul cluster ───────────────┐
              │  m7s/nodes/<id>      = JSON(PeerInfo)         │
              │  m7s/streams/<path>  = nodeID                 │
              │  session TTL=10s, Behavior=delete, LockDelay=1ms
              └────────────┬───────────────┬──────────────────┘
                           │               │
                  ┌────────┴─────┐  ┌──────┴───────┐
                  │   node-A     │  │   node-B     │
                  │ (origin of   │  │ (origin of   │
                  │  live/foo)   │  │  live/bar)   │
                  └──────┬───────┘  └──────┬───────┘
                         │ rtmp/rtsp/flv 监听对方 Advertise 表
                         └──────── pull on demand ──────┐
                                                       ▼
                                              ┌──────────────┐
                                              │   node-C     │ ← 订阅 live/foo
                                              │ (relay-only  │   到达 C,C 自动
                                              │  for live/foo)│   从 A 拉
                                              └──────────────┘

                  ┌──────── 共享后端 ────────┐
                  │ PostgreSQL: users / proxies / mp4_streams
                  │ S3/COS/OSS: 录制 mp4 文件
                  └─────────────────────────┘
```

### 1.2 范围(Out of scope)

下列项**不在 v1 范围**,本 spec 不予以设计:

- Raft / leader election / 跨集群共识
- 跨地域 / 跨 DC 集群
- 流复制 / 热备 publisher
- 节点间新增传输协议(沿用对端已有 RTMP/RTSP/FLV 端口)
- RTMP/RTSP 协议栈级别的 publisher 透明重定向
- 推流端 cluster-aware SDK / 客户端自动重连到其他节点
- 跨集群 Auth(集群内 relay 走"内网信任面",见 §4.1)

---

## 2. 组件与边界

`plugin/cluster/` 是单一 Go 包,build tag `cluster`,分 6 个子系统。

### 2.1 Membership(`membership.go`,Phase 1,已实现)

**做什么**: 维持本节点 Consul session;在 `m7s/nodes/<self>` 写本节点 advertise 信息;watch `m7s/nodes/` 前缀,本地缓存 `peers map[nodeID]*PeerInfo`。

**公共接口**:
```go
func (m *Membership) Peers() []*PeerInfo
func (m *Membership) Peer(nodeID string) (*PeerInfo, bool)
func (m *Membership) SessionID() string
func (m *Membership) AddOnSessionRebuilt(func(sid string))
```

**依赖**: Consul HTTP API(`github.com/hashicorp/consul/api`)。**是其他 5 个子系统的基础**。

### 2.2 StreamRegistry(`streamregistry.go`,Phase 2,已实现)

**做什么**: 本节点 publisher 上线/下线 → 写/删 `m7s/streams/<path>` 键(用 Membership 当前 session 锁);watch `m7s/streams/` 前缀,维护 `streams map[streamPath]nodeID`。

**公共接口**:
```go
func (sr *StreamRegistry) OnPublish(*m7s.Publisher)           // m7s.IPublishHookPlugin
func (sr *StreamRegistry) Lookup(streamPath string) (nodeID string, ok bool)
func (sr *StreamRegistry) Streams() map[string]string
func (sr *StreamRegistry) AddOnStreamRemoved(func(streamPath string))   // ← 本 spec 新增钩子
```

**依赖**: Membership(读 SessionID + 订阅 OnSessionRebuilt)。

**本 spec 升级点**:
- 新增 `AddOnStreamRemoved` 钩子。`streamWatcher` 检测到 `m7s/streams/<path>` 键消失时回调订阅方(Relay 用)
- `OnPublish` 在 `acquire` 返回 `ok=false` 时,不再只 log Warn,而是 `pub.Stop(ErrStreamPathTaken)`(§4.3)

### 2.3 Relay(`relay.go`,Phase 3)

**做什么**: 本节点收到对一条不在本地的流的订阅 → 通过 StreamRegistry 找 origin → 通过 Membership 找 origin 的 advertise 端口表 → 按 `RelayProtocols` 优先级拼 URL → 调 `Server.EnsurePullProxy` 起一个本地 pull-proxy → 后续本地订阅者天然复用同一个 pull-proxy。

**公共接口**:
```go
func (p *ClusterPlugin) OnSubscribe(streamPath string, args url.Values)  // m7s.ISubscribeHookPlugin
```

**内部分解**:
```go
func buildPullURL(peer *PeerInfo, streamPath string, priority []string) (proto, fullURL string, err error)
func (p *ClusterPlugin) ensureRelay(originNodeID, streamPath, proto, url string) error
```

**关键约束**:
- pull-proxy 创建时 `PullProxyConfig.Description = "cluster-relay:" + originNodeID`(常量 `ClusterRelayDescPrefix`)
- StreamRegistry.OnPublish 看到带这个前缀的 publisher → **直接 return,不写 streamRegistry**(防 C→B→A→B 环回)
- 订阅到 `AddOnStreamRemoved`:origin 失联 → 立刻 Stop 本节点对该 streamPath 的 cluster-relay pull-proxy(§4.2)

**依赖**: StreamRegistry, Membership, `m7s.Server.EnsurePullProxy`。

### 2.4 StreamLocator(`streamlocator.go`,Phase 4)

**做什么**: 把"用 streamPath 找节点"这件事接到 m7s 现成的两条路由链路。

**实现的接口**:
- `m7s.StreamRouter`(给 server.go 现成的 gRPC `RouteInterceptor` 用,核心代码 `grpc_api_route.go:319`,无改动)
- `m7s.RedirectAdvisorV2`(给 flv/rtsp/server_http 现成的 HTTP 重定向链路用,无改动)

**核心代码改动**: **零**。只是把现成的 Router/Advisor 槽位用 Consul 数据源填上(D2=A 决策)。

**依赖**: StreamRegistry(`Lookup`), Membership(`Peer` + `Advertise.GRPC/RTMP/FLV/HTTP`)。

**并行删除**: `plugin/apiroute/` 整目录(功能完全被 StreamLocator 替代),`pkg/config/types.go` 中 `APIRoute` 替换为 `Cluster`。

### 2.5 LoadReporter(`metrics.go`,Phase 6)

**做什么**: `task.TickTask`,每 5s(可配)采集本节点指标 → 用当前 session 重写 `m7s/nodes/<self>` 的 `PeerInfo.Metrics` 字段。

**指标(v1 范围)**:
- `streams`(int): 本节点 publisher 数量
- `subscribers`(int): 本节点 subscriber 数量
- `goroutines`(int): `runtime.NumGoroutine()`

CPU / 带宽指标 v2 再加(避免引入 system metrics 依赖)。

**HTTP**: `/api/cluster/lb-suggest`(挂在 `lb.go`):
- 读所有 peers Metrics,按 `streams` 升序、`goroutines` tie-break、NodeID 字典序兜底
- `?excludeSelf=true`(默认 true)
- 返回 `{suggested, advertise, metrics}` 或 `503 {error:"no peers with metrics"}`

**依赖**: Membership, `m7s.Server`(读 streams 数量等)。

### 2.6 跨节点回看 + 共享存储(Phase 5)

**做什么**: 让任意节点上的 `/download/<streamPath>?id=...` 都能取到对应流的历史录制。

**实现路径**:
- 共享 PostgreSQL: `mp4_streams` 表加 `node_id` 列,**跨节点元数据天然可见**
- 共享对象存储: 录制文件统一上传到 S3/COS/OSS,任何节点的下载 handler 拿到对象存储 key 直接 302 到对象存储预签名 URL
- 极端兜底(没共享存储): `/download/...` 在本节点查 `mp4_streams` 拿到 `node_id`,如果不是本节点,302 到该 node 的 HTTP 端口

**代码改动**:
- `plugin/mp4/index.go` 加 ~20 行下载拦截逻辑(读 cluster.Lookup + 决策 302)
- 新增 `plugin/cluster/download.go`(可选,把拦截逻辑放到 cluster 包内,通过依赖注入挂到 mp4)
- `doc_CN/arch/cluster.md` 大段补充"共享存储部署"说明

**启动期检查**: cluster 启用 + DB 是 SQLite → log Warn "cluster + SQLite 不建议生产用,跨节点录制元数据不会自动共享"(不拒启,P4)。

### 2.7 HTTP 端点(`lb.go`)

- `GET /api/cluster/nodes` → `{self, sessionId, peers}` (Phase 1)
- `GET /api/cluster/streams` → `{self, streams}` (Phase 2)
- `GET /api/cluster/lb-suggest?excludeSelf=true` → `{suggested, advertise, metrics}` 或 503 (Phase 6)

---

## 3. 数据模型

### 3.1 Consul KV namespace

```
m7s/nodes/<nodeID>          JSON(PeerInfo)    session-locked, Behavior=delete
m7s/streams/<streamPath>    nodeID 字符串       session-locked, Behavior=delete
```

`session-locked` 意思:键由当前 sessionID `Acquire` 出来,session 一死,consul 按 Behavior=delete 自动删键。**全集群感知节点 / 流消失无需额外心跳协议**。

### 3.2 PeerInfo(已实现,锁定)

```go
type PeerInfo struct {
    NodeID    string          `json:"nodeId"`
    Advertise AdvertiseConfig `json:"advertise"`
    Metrics   map[string]any  `json:"metrics,omitempty"`   // Phase 6 LoadReporter 填
    Version   string          `json:"version,omitempty"`
    StartedAt int64           `json:"startedAt"`
}

type AdvertiseConfig struct {
    RTMP, RTSP, FLV, GRPC string  // 例: "10.0.0.5:1935", "http://10.0.0.5:8080"
}
```

`Metrics` 选 `map[string]any` 而非固定 struct —— v1 允许灵活加字段,LB suggest 明确读 `streams` / `goroutines`。

### 3.3 Consul session 参数(已修过 bug,锁定)

| 字段 | 取值 | 决策原因 |
|---|---|---|
| Name | `"m7s-cluster-" + NodeID` | NodeID 前缀方便诊断 + 测试清理 |
| TTL | 10s(可配) | Consul 最低值;低于此会被 server 拒 |
| Behavior | `delete` | 节点死,所属键自动删 |
| LockDelay | **1ms** | **不能写 0** —— consul 当默认 15s,会卡 rebuild 15s。1ms 实质 0,绕开默认分支 |
| Renew 间隔 | 3s(可配),测试 200ms | 远小于 TTL,容忍单次网络抖动 |
| **Renew 校验** | 必须检查 `entry == nil`,不只 `err` | consul/api 在 session 已亡时返回 (nil, nil, nil) |

### 3.4 ClusterRelayDescPrefix(已实现,核心约束)

```go
const ClusterRelayDescPrefix = "cluster-relay:"
```

Relay 创建 pull-proxy 时 `PullProxyConfig.Description = "cluster-relay:" + originNodeID`。
StreamRegistry.OnPublish 看到带此前缀 → 直接 return。
**Q2 决策实现**:用 `Description`(非持久化字段)做标记,不改 PullProxyConfig 结构。

### 3.5 Sentinel errors

新增统一在 `plugin/cluster/errors.go`(本 spec 引入):

```go
package plugin_cluster

import "errors"

var (
    // ErrOriginLost: §4.2 中 Relay 主动 Stop 本节点 pull-proxy 时的 reason。
    ErrOriginLost = errors.New("cluster: origin node lost")
    // ErrStreamPathTaken: §4.3 中 first-write-wins 失败时 publisher.Stop 的 reason。
    ErrStreamPathTaken = errors.New("cluster: streamPath already owned by peer")
)
```

被 §4.2 / §4.3 引用。错误判等用 `errors.Is`。

### 3.6 数据库

| 表 | 列 | 改动 |
|---|---|---|
| `mp4_streams` | `node_id string` | **新增**列,记录录制产生节点;Phase 5 回看路由读取 |

GORM `AutoMigrate` 处理,**仅当 cluster build tag 启用时执行**。

### 3.7 ClusterConfig YAML(全小写,符合 CLAUDE.md)

```yaml
cluster:
  nodeid: node-1                       # 必填,全局唯一
  consul:
    addresses: [http://consul-1:8500]
    token: ""
    waittime: 30s
    sessionttl: 10s
    sessionrenewinterval: 3s
  advertise:
    rtmp: 10.0.0.5:1935
    rtsp: 10.0.0.5:554
    flv:  http://10.0.0.5:8080
    grpc: 10.0.0.5:50051
  relayprotocols: [rtmp, rtsp, flv]
  metrics:
    reportinterval: 5s
  loadshed:
    enable: false
    streamthreshold: 500
```

### 3.8 启动期校验

| 校验 | 行为 |
|---|---|
| `cluster.nodeid` 空 | 拒启,error |
| `cluster.consul.addresses` 空 | 拒启,error |
| cluster 启用 + DB 是 SQLite | log Warn,不拒启 |
| `advertise.*` 全空 | log Warn,不拒启 |
| advertise 端口本机不可达 | v1 不做自检,文档明确即可 |

---

## 4. 务实型 v1 决策展开

### 4.1 Auth / 凭证在 relay 边界

**部署约束**(写进 README + 启动期 Info 日志):
> 集群所有 m7s 节点必须部署在**可信内网**(VPC / 同 K8s 集群 / WireGuard mesh)。Advertise 的 RTMP/RTSP/FLV 端口**不对公网暴露**。Cluster 节点间 relay 拉流是**无认证**的。

**实现层面**:
- Relay 拉 origin 时 URL 是裸的(`rtmp://10.0.0.5:1935/live/foo`),不带 token / 签名
- 若 origin 的 m7s `OnAuthSub` 钩子配置成校验 token,这条 relay 拉取会失败 —— 这是用户配置错误,不是 cluster 的责任
- 部署模板:cluster 启用时,m7s 的 auth 配置只校验"用户面"端口,不校验内网 advertise 端口

**用户面 auth 不变**: 订阅者 S 连 B 节点 → B 校验 S → OnSubscribe 触发 relay → B 自己作订阅者去拉 A,走 A 的"内网信任面"。

**spec 要求**: README 加 "Deployment — Security boundary" 一节,1 段话 + 1 个示意图。

### 4.2 Origin 掉线 / 重选举

**触发条件**: `m7s/streams/<path>` 键在 Consul 上消失(原因:origin session 失效 / 主动 publisher.Stop / 节点崩)。

**行为**:
1. 所有 relay 节点的 `streamWatcher` 看到 key 删除事件(blocking query WaitIndex 机制保证 ~ms 级感知)
2. StreamRegistry 调 `AddOnStreamRemoved` 注册的回调
3. Relay 模块订阅该钩子: 找到本节点上对应该 streamPath 的 cluster-relay pull-proxy(用 `Description` 前缀 + `StreamPath` 双重确认),调 `pullProxy.Stop(ErrOriginLost)`
4. pull-proxy 停 → 派生的本地 Publisher 退出 → 现有订阅者收到流结束(EOF)
5. 新订阅者到达: `OnSubscribe` 重新查 `m7s/streams/<path>`。若有新 origin → 正常 relay;若没有 → 订阅者进等待队列直至超时(m7s 现有行为)

**不做"无缝切换"**。订阅者必须重连。这是 cluster.md §4.4 的升级版 —— 升级点在**主动 + 快速终止本地 pull-proxy**,而非等 MaxRetry 耗尽。

### 4.3 Push-side(发布者重复处理,first-write-wins)

1. publisher P 推流到 B 节点,streamPath = X
2. m7s 走原 publish 路径,注册 publisher 后触发 `IPublishHookPlugin.OnPublish`
3. cluster 的 OnPublish → StreamRegistry.OnPublish → `acquire(X)`:
   - 成功 → 维持本地 publisher,正常工作
   - **失败(`ok=false`,X 已被 A 持有)** → **本 spec 升级**: 不再只 log Warn;**主动 Stop 本地 publisher**: `pub.Stop(ErrStreamPathTaken)`
4. RTMP/RTSP/FLV 各 publish handler 在 publisher.Stop 后断开推流端 TCP;推流方收到断流;客户端需要自己重连(SDK 行为,不在 v1 范围)

**重要边界**: cluster-relay 派生的 publisher(Description 前缀)走 OnPublish 跳过路径,**不会**触发 StreamPathTaken。

**Publisher LB 不在 v1**: 外部 LB(DNS / Nginx)调 `/api/cluster/lb-suggest` 自己决策。

### 4.4 多节点 e2e 测试(docker-compose)

**目录**: `example/cluster-e2e/`

```
example/cluster-e2e/
├── docker-compose.yml         # 1 consul + 3 m7s + 1 postgres + 1 minio
├── config-node-{1,2,3}.yaml   # 3 套配置,Advertise 端口分别 1935/1936/1937 等
├── Dockerfile                 # 多阶段构建 m7s (build tag: cluster postgres s3)
├── smoke.sh                   # bash + curl + ffmpeg 跑场景
└── README.md                  # 启停命令 + 场景清单
```

**场景清单(smoke.sh)**:

| # | 场景 | 断言 |
|---|---|---|
| 1 | 三节点起来 | `consul kv get -recurse m7s/nodes/` 有 3 项;`curl :8081/api/cluster/nodes` 有 3 项 |
| 2 | 推 RTMP 到 node-1 | `consul kv get m7s/streams/live/foo` = node-1 |
| 3 | 从 node-2 订阅 RTMP | 500ms 内出图;node-2 上有一个 cluster-relay PullProxy;node-1 上看到一个 type=rtmp 的远端订阅者 |
| 4 | 从 node-3 再订阅 | node-2 上 PullProxy 不增加,本地 subscribers 增加 |
| 5 | 推 mp4 录制 API 到 node-3 | mp4 文件落在 node-1 的录制目录;`mp4_streams` 表新增一行 `node_id=node-1` |
| 6 | 从 node-2 列录制 | 包含 node-1 上的录制项 |
| 7 | 从 node-2 下载 `/download/live/foo.mp4?id=...` | 302 到 node-1,或直接走 minio URL |
| 8 | kill node-1 | 10s 内 `m7s/streams/live/foo` 消失;node-2 上 PullProxy 收到 ErrOriginLost 退出;原订阅者收到 EOF |
| 9 | 同 streamPath 同时推到 node-1 和 node-2 | 谁先抢 Acquire 谁是 origin;另一 publisher 收到 ErrStreamPathTaken 断流 |
| 10 | 调 `/api/cluster/lb-suggest` | 返回 streams 最少的节点 |

**Go 单元 + 集成测试不变**: 走 `go test -tags cluster ./plugin/cluster/...`,docker consul 容器作为 fixture。

**smoke.sh 退出码 0 = 全场景通过**;CI 把 docker-compose up + smoke.sh 串成一个 step。

---

## 5. 横切关注点

### 5.1 Task 系统规范(铁律)

**长期循环子任务必须用 `Go()` 接口,不能用 `Run()`**:
- gotask 父 Job 的事件循环对 `Run()` 是**同步阻塞**调用
- 第一个挂入的 keepalive `Run()` 会霸占事件循环,兄弟任务全饿死
- 已修改:`sessionTask.Go()` / `nodeWatcher.Go()` / `streamWatcher.Go()`
- 即将写:`LoadReporter` 用 `task.TickTask`(单独事件流,不冲突)

**Retry 在 `Go()` 内部循环实现,不用框架 `SetRetry`**:
- 对 `TaskGo`,`SetRetry` 无效;task.run 在 handler 返回后直接 Stop(无重试)
- 模板:

```go
func (t *xxxTask) Go() error {
    for {
        if t.Err() != nil { return task.ErrTaskComplete }
        if err := t.runOnce(); err != nil {
            t.Warn("xxx error, will retry", "error", err)
            select {
            case <-t.Done(): return task.ErrTaskComplete
            case <-time.After(t.backoff):
            }
        }
    }
}
```

退避值: sessionTask 用 `Consul.SessionRenewInterval`(秒级);watcher 用 2s 固定。

**禁止裸 `go` 关键字**(CLAUDE.md 已有,本 spec 强调)。

### 5.2 错误传播 + 日志

结构化日志 —— slog k-v,不用 fmt.Sprintf 拼接:

```go
t.Info("consul session created", "sessionId", sid, "ttl", se.TTL)
t.Warn("acquire stream key failed", "streamPath", path, "error", err)
```

| 错误 | 行为 |
|---|---|
| Consul 完全不可达 | sessionTask 内部 retry 不停,Membership 继续活;本地 peers/streams 缓存仍可读(降级) |
| Renew 失败 / session 不存在 | runOnce 返回 err → Go() 退避 → 重建 session |
| KV.Acquire 返回 `ok=false`(registerNode) | 异常,destroy session 重试 |
| KV.Acquire 返回 `ok=false`(OnPublish) | first-write-wins 正常路径 → `pub.Stop(ErrStreamPathTaken)` |
| EnsurePullProxy 失败 | Log Warn,relay 失败,**订阅者超时**(m7s 现有等待队列行为);不重试 |
| nodeWatcher/streamWatcher 单次 List 失败 | Log Warn,2s 退避后再试,本地缓存不变 |

### 5.3 Build tag 纪律

- `//go:build cluster` 在每个新生产代码文件 + test 文件最上方
- `plugin/cluster/doc.go` **没有** build tag,内容是空 package:**default 构建路径下能 `import _ "m7s.live/v5/plugin/cluster"`**(空包但可 import)
- `example/cluster/main.go` 无条件 import cluster plugin → default 构建必须能编;新增文件继续遵守

### 5.4 文件布局(最终态)

```
plugin/cluster/
├── doc.go                       # 无 build tag,空 package
├── index.go                     # 主 plugin + Start + IPublishHookPlugin/ISubscribeHookPlugin 转发
├── config.go                    # ConsulConfig/AdvertiseConfig/MetricsConfig/LoadShedConfig
├── membership.go                # Phase 1
├── streamregistry.go            # Phase 2(本 spec 加 AddOnStreamRemoved + 强制 Stop publisher)
├── relay.go                     # Phase 3
├── streamlocator.go             # Phase 4
├── metrics.go                   # Phase 6
├── lb.go                        # Phase 1/2 已有,Phase 6 加 /lb-suggest
├── cluster_helper_test.go       # 测试 helper
├── membership_test.go           # Phase 1 tests
├── streamregistry_test.go       # Phase 2 tests(本 spec 加 OnPublishRejects 测试)
├── relay_test.go                # Phase 3 tests
├── streamlocator_test.go        # Phase 4 tests
└── metrics_test.go              # Phase 6 tests

example/cluster-e2e/             # Phase 5 / 9 多节点 e2e
├── docker-compose.yml
├── config-node-{1,2,3}.yaml
├── Dockerfile
├── smoke.sh
└── README.md

doc_CN/arch/cluster.md           # 现有,Phase 5 大段补充"共享存储部署"
docs/superpowers/specs/2026-05-12-cluster-design.md  # 本 spec
docs/superpowers/plans/2026-05-12-cluster-plan.md    # writing-plans 产出
```

**删除**: `plugin/apiroute/` 整目录。

### 5.5 第三方依赖(锁定)

- `github.com/hashicorp/consul/api v1.34.2` —— 已 direct
- 无新增 direct 依赖

---

## 6. 测试策略

### 6.1 三层金字塔

```
                           ┌─────────────────┐
                           │ e2e (smoke.sh)   │  ← 10 个场景
                           │ 3 nodes + consul │     docker-compose
                           │ + pg + minio     │     CI gate
                           └─────────────────┘
                       ┌───────────────────────────┐
                       │  integration tests        │  ← 大头
                       │  go test -tags cluster    │     docker consul,
                       │  单 Go 进程 / 多 Membership │     CI gate
                       └───────────────────────────┘
                  ┌─────────────────────────────────────┐
                  │  pure unit tests                     │  ← 快,无外部依赖
                  │  buildPullURL / handleLocalPublish   │     CI gate
                  │  loadbalancer 选举算法 etc.           │
                  └─────────────────────────────────────┘
```

### 6.2 各 Phase 测试清单

| Phase | 文件 | 状态 | 覆盖 |
|---|---|---|---|
| 1 Membership | `membership_test.go` | ✅ 3 integration 已写并绿 | RegistersSelfAndPeer / WatcherSeesRemotePeer / SessionDestroyTriggersRebuild |
| 2 StreamRegistry | `streamregistry_test.go` | ✅ 2 unit + 3 integration 已绿 | HandleLocalPublish 双分支 + Acquire-Release / WatcherReflectsRemote / RebindAll |
| 2 **(本 spec 新增)** | 同上 | 待加 1 integration | `OnPublishRejectsWhenKeyOwnedByPeer`(§4.3) |
| 3 Relay | `relay_test.go` | 1 unit(✅)+ 4 待写 | buildPullURL(✅)/ Skip-when-local / Skip-when-unknown / CreatesPullProxyWithMarker / `StreamRegistryKeyDisappears_KillsLocalPullProxy` |
| 4 StreamLocator | `streamlocator_test.go` | 4 待写 | gRPC 风格 mock dispatch / HTTP RedirectAdvisor / 本地 streamPath bypass / origin 不在线时 fallback |
| 5 跨节点回看 | mp4 测试或 cluster e2e | 部分 unit + e2e | 一个单元测 mp4 download 拦截逻辑;其余靠 e2e |
| 6 LoadReporter | `metrics_test.go` | 5 待写 | UpdatesMetricsField + LBSuggest 4 个分支:`NoPeersReturns503` / `PicksLeastLoadedPeer` / `TieBreaksByGoroutines` / `ExcludesSelfByDefault` |
| 9 e2e | `example/cluster-e2e/smoke.sh` | 10 场景 | §4.4 表格全部 |

**所有 cluster 测试统一 helper 模式(`cluster_helper_test.go`,已锁定)**:
- `requireConsul(t)` —— 缺 consul 直接 `t.Skip`
- `uniqNodeID(t)` —— 用 `t.Name()` 派生唯一 NodeID,防跨测试撞键
- `startMembershipForTest` / `startStreamRegistryForTest` —— 等到稳定态(`pair.Session == sid`)再返回
- 测试结束 `t.Cleanup` 调 `Stop + WaitStopped` 严防异步残留

**注入点**(可测试性):
- `Relay.EnsurePullProxy` —— `ClusterPlugin.relayHook func(*m7s.PullProxyConfig) (created bool, err error)`,默认走 `p.Server.EnsurePullProxy`,测试 swap 为 recorder fake
- `StreamLocator` —— `routeHook func(streamPath string) (target *PeerInfo, ok bool)`,默认 `Lookup + Peer`
- 注入点用**字段赋值**,不用全局变量,避免并行测试串扰

### 6.3 CI / 本地开发流

**本地最小循环**:

```bash
# 一次性
docker run --rm -d --name m7s-consul-test -p 8500:8500 hashicorp/consul:latest agent -dev -client=0.0.0.0

# 每次改 cluster 代码
gofmt -w plugin/cluster
go vet -tags cluster ./plugin/cluster/...
go test -tags cluster ./plugin/cluster/... -count=1
go build -tags cluster ./plugin/cluster/... ./example/cluster/...
go build ./plugin/cluster/... ./example/cluster/...    # default build 也要过
```

**CI gate**:
1. `gofmt -l` 必须为空
2. `go vet -tags cluster ./...` 通过
3. `go test -race -tags cluster ./plugin/cluster/...` 通过(`-race` 强制)
4. `go build -tags cluster ./...` + `go build ./...` 都通过
5. e2e: `cd example/cluster-e2e && ./smoke.sh` 退出码 0

### 6.4 迁移 + 弃用清单

| 项 | 行为 | 时机 |
|---|---|---|
| `plugin/apiroute/` 整目录 | **删除** | Phase 4 同 PR |
| `pkg/config/types.go` 中 `APIRoute` | 替换为 `Cluster` | Phase 4 同 PR |
| `m7s.PluginMeta.GRPCInterceptors` hook | **不引入**(D2=A 决策) | N/A |
| `m7s.PullProxyConfig.Origin` 字段 | **不引入**(Q2 用 Description 前缀) | N/A |
| 用户 config.yaml 的 `apiroute:` 段 | 静默忽略 + 启动期 Info 提示 | Phase 4 |
| `cluster-v5` 旧分支 cluster 配置 | 不兼容,文档明确 | N/A |
| `mp4_streams` 表 `node_id` 列 | GORM AutoMigrate 加列,旧行 node_id 留空 | Phase 5 |

### 6.5 验收硬指标(cluster v1 算 ready 的条件)

1. ✅ Phase 1+2 已 ready(8/8 tests 绿 + commit `6145fdcf`)
2. Phase 3 Relay 所有单元 + integration 测试绿(7 tests)
3. Phase 4 StreamLocator 所有测试绿;`plugin/apiroute/` 已删
4. Phase 5 `/download/<streamPath>` 跨节点行为有单元测试覆盖;`cluster.md` 共享存储一节 ready
5. Phase 6 LoadReporter 周期上报 + `/api/cluster/lb-suggest` 4 个分支测试绿
6. Phase 9 e2e — smoke.sh 10 场景全过
7. `go test -race -tags cluster ./plugin/cluster/... -count=3`(重复 3 次抓 flake)全绿
8. `cluster.md` 内容与实际实现一致(本 spec 中所有决策都写进 cluster.md)

---

## 7. 已确认的决策回溯(给后来人)

下列决策来自 `/plan-eng-review` 与本 brainstorming session,已锁定:

| 编号 | 决策 | 含义 |
|---|---|---|
| D1 | 删除 `plugin/apiroute/` | cluster 完全替代 |
| D2=A | 复用现有 `RouteInterceptor` + `RedirectAdvisorV2` | 只换数据源,不侵入核心 |
| D3 | 用 Consul,不用 etcd | 服务发现 / KV 后端 |
| D3' | 外部 Consul Server 集群 | 不内嵌 |
| A1 | session 失效后所有本地流位置 re-Acquire | StreamRegistry.rebindAll 实现 |
| Q2 | cluster-relay 标记走 `PullProxyConfig.Description` 前缀 | 不加结构字段 |
| P4 | cluster + SQLite 仅 Warn,不拒启 | 允许"试用" |
| **新** | Origin 失联 → 主动 Stop 本地 pull-proxy(§4.2) | 不等 MaxRetry 耗尽 |
| **新** | first-write-wins 失败 → 强制 Stop publisher(§4.3) | 不再只 Warn |
| **新** | 集群内 relay 无 auth,部署级"内网信任面"约束(§4.1) | 写进 README |
| **新** | e2e 走 docker-compose,10 个 smoke 场景(§4.4) | 不用 testcontainers-go |
| **新** | LoadReporter 指标只采 streams/subscribers/goroutines(§2.5) | CPU/带宽 v2 |

---

## 8. 实现风险

| 风险 | 表现 | 缓解 |
|---|---|---|
| Consul 短暂不可达 | 注册不上 / watch 中断 | task 内部 retry,本地缓存仍可读做降级 |
| 拉流环回 | A→B→C 链 | Description 前缀防护,Q2 实现 |
| 录制 API 转发风暴 | List/Catalog 大量请求 | 这两个方法不路由(聚合查共享 DB),仅 Start/Stop/Delete/EventStart 路由 |
| 共享 DB 强依赖 | SQLite 下跨节点录制元数据看不见 | 部署文档强制 PG;启动期 Warn |
| Advertise 地址错误 | relay 拉不通 | v1 不自检,文档明确 + e2e smoke #3 兜底 |
| consul/api Renew 返回 nil 不报错 | session 假装健康永不重建 | 必须检查 `entry==nil`(已在 §3.3 写死) |
| TaskBlock 阻塞父事件循环 | watcher 饿死 | 所有长跑 task 用 `Go()`(已在 §5.1 写死) |
| LockDelay 默认 15s | session 重建被卡 15s | 显式设 1ms(已在 §3.3 写死) |
