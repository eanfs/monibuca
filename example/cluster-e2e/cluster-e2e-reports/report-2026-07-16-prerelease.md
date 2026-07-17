# 发布前测试执行报告 — monibuca v5.3.x

- **执行日期**: 2026-07-16
- **被测**: develop `f642d51c`(功能代码 = `f6a5fc0b`,二者仅差测试计划文档)
- **镜像**: `v5.3.2.2607161123`(BuildInfo `vcs.revision=f642d51c`, `vcs.modified=false`)
- **环境**: 三节点容器版 node-1 `172.16.12.131:19080` / node-2 `172.16.12.134:19081` / node-3 `172.16.12.133:19082`;Consul + MinIO 在 133
- **对应计划**: `TEST-PLAN-2026-07-16-prerelease.md`

---

## 0. 结论摘要

| | |
|---|---|
| **发布阻断项** | **1 个:F4 上传补传子系统完全不工作** → **已修 + 真机复验通过,阻断解除** |
| 新发现缺陷 | 2 个,**均已修**:F4 注册竞态(连带救活 `CheckSubWaitTimeout`)/ C5 relay 后同名推流漏防(2026-07-17 修,根因勘误见 §3) |
| 历史问题复验 | R1/R2 死锁**确认已修**;R3/R4/R5/R9/R11/R12 全部通过 |
| 已知缺陷量化 | R7(400)、R8(丢 70%)按决策**保留 + 文档护栏** |

**发布建议**:F4 修复已在真机验证补传闭环(见 §6)。**须用含该修复的代码重打镜像**,并复跑 A2(BuildInfo)+ C1/C5 + F4 一轮后发布。其余批次全绿。

**覆盖缺口进展**:本轮**填平 G13(补传子系统,查出并修复阻断)、G14(>4GB,首次真正触达)、G15(分片连续性)、G16(帧数/A-V 同步)、G17(源断)、G6/R13(崩溃量化)**;**G3(arm64 真机)** 与 **D2/D3/D5** 仍缺(后者被 §11 环境事件阻塞)。

---

## 1. 决策落地

| 决策 | 选择 | 落地 |
|---|---|---|
| 决策 1 — R8 `fileName+fragment>0` 静默丢数据 | **C 不修,文档护栏** | B6 已量化(见 §4),发布说明须写死约束 |
| 决策 2 — R10 共享 PG 形态 | **a 明确「必须节点本地 sqlite」** | 跳过 D1;待改 e2e compose + README + CLAUDE.md |
| 决策 3 — R7 its-server 升级不兼容 | **b 先发 monibuca,发布说明标红** | B5 已复现 400(见 §4) |
| F4 阻断处置 | **修代码**(用户解除「不改代码」) | 已修 + 单测,见 §6 |

---

## 2. 批次 A — 打包与产物自检 ✅

计划假设「`build_docker.sh` 改完从未跑过」**已过时**:tag `v5.3.2.2607161123` 今日 11:23 已构建并推送。

| ID | 判据 | 结果 |
|---|---|---|
| A1 | `--load` 路径跑通(R11) | ✅ 镜像已成功构建推送 |
| A2 | BuildInfo tags + gotask | ✅ `-tags=cluster,postgres,sqlite,s3`;**`github.com/eanfs/gotask v1.0.5`**(R2 守卫);`vcs.modified=false` |
| A3 | **arm64 真机 run** | ❌ **未验证** — 计划称「本机 Mac 即 arm64」有误,本机实为 **Intel x86_64**(i7-9750H)。G3 缺口仍在 |
| A4 | 三节点 pull | ✅ 131/134 运行中;node-3 拉 **manifest tag** 成功,得到与 `-amd64` tag **完全相同的 image ID** `sha256:eed5c40f` |
| A5 | manifest 双架构 | ✅ `manifest.list.v2+json` 含 linux/amd64 + linux/arm64,**无 attestation 条目** → R11 解决 |

**附带发现**:
1. `build_docker.sh` **缺 `--provenance=false --sbom=false`**(CLAUDE.md 明确要求)。当前无害:本机 overlay2 image store 下 `--load` 会剥离 attestation。**风险**:Docker Desktop 迁移到 containerd image store 后 attestation 会保留 → SWR 立即拒绝。建议补上作为防御。
2. **tag 与镜像内容不一致**:tag 指向 `f6a5fc0b`,镜像内 `vcs.revision=f642d51c`。差异仅 238 行文档,功能等价,不影响结论;但发布产物 tag 宜指向真实内容。

---

## 3. 批次 C — cluster 三节点核心回归 ✅(1 个新 bug)

前置:node-3 原为 `v5.3.1.2607151131`(混版本),已升级至 RC,三节点同版本干净基线。旧容器保留 `m7s-node-3-old-v531` 供回滚。

| ID | 判据 | 结果 |
|---|---|---|
| C1 | membership | ✅ 三节点 peers=3/3/3;Consul `m7s/nodes` 3 键;`m7s/streams` 30 键(node-3 重启后完全重注册) |
| C2 | 流注册与属主 | ✅ 30 流,属主分布 **node-1:10 / node-2:10 / node-3:10** 完全均衡 |
| C3 | 302 路由 | ⚠️ **本形态下按设计禁用** — 三节点均配 `proxyOnRedirect: true`(代码默认 `false`),故 FLV/RTSP 走 relay 200 而非 302。**CLAUDE.md 未错**,是配置决定;跨节点访问由 C4 覆盖 |
| C4 | auto-relay(三方向) | ✅ 三方向均 relay h264@25fps;4s 窗口收 **77 个视频包**;relay 为**临时性**(订阅者断开即回收)且**不抢锁** |
| C5 | 同名冲突不死锁 + release-guard | ✅ 死锁已修 / ❌ **发现新 bug**(见下) |
| C6 | 跨节点 relay 录制完整性(H.265) | ✅ camera28 hvc1 2592x1944:box 序 `ftyp,moov,free,free,mdat` **moov 前置**;解码 exit=0,20 条错误**全为已知无害等值 dts** → 0 真实错误;40.009s / 1202 帧 / 30.04fps;字节精确 3553352 |
| C7 | lb-suggest | ✅ 三节点 `suggested` 均非空且合理(负载 10/10/10 均衡) |
| C8 | 日志扫描(已去 ANSI 色码) | ✅ 三节点 **0 panic / 0 database-locked / 0 reason=discard / 0 fatal**(~70k 行 / 3h)→ R4/R5/R6 守卫通过 |

**R12 守卫** ✅:三节点 `webrtc` / `webtransport` / `gb28181` 均 `enable:false`。

### C5 详情:R1/R2 核心确认已修

| 判据 | 结果 |
|---|---|
| 不死锁 | ✅ 冲突期间 `/api/stream/list` **8/8 探测全 200**,无 hang |
| KV 属主仍为原 peer | ✅ 全程 `node-1`,从未被抢/误删 |
| 次生误删 peer 键 | ✅ 30 流全程无损 |

### 🆕 新 bug:一次 auto-relay 后,该流在该节点 first-write-wins 永久失效(**已修,2026-07-17,见文末勘误**)

**决定性对照**(同一条流、同一节点,唯一变量是中间做了一次 relay):

| 步骤 | 动作 | ffmpeg 结局 | first-write-wins |
|---|---|---|---|
| 1 | relay **前**推 `camera12` 到 node-2 | **exit=224**(Broken pipe,被停) | ✅ 生效 |
| 2 | 在 node-2 上 relay `camera12` 一次 | — | — |
| 4 | relay **后**推 `camera12` 到 node-2 | **exit=0**(推满 6s) | ❌ **失效** |

三条流独立印证:`camera11`(从未 relay)→ 有保护;`camera1`(早前 relay 过)→ 无保护;`camera12` → before/after 翻转。

- **机制**:`handleLocalPublish`(`streamregistry.go:112`)在 `isClusterRelay` 为真时早返回,跳过 KV acquire 与 first-write-wins。`activeRelays` 条目在 relay 结束后**未被清理**,导致该 streamPath 被永久误判为 relay。
- **讽刺**:`relay.go:120-124` 的注释**一字不差地预言了这个缺陷**,并绑定 `proxy.OnDispose(removeActiveRelay)` 去防它 —— 但实测未生效(relay pull-proxy 已消失,条目仍在)。
- **后果**:节点只要 relay 过流 X,就永久丧失 X 的同名冲突保护(直到重启)。此时重复/误配推流打到该节点会形成**脑裂**(该节点本地服务错误内容,集群仍认为属主权威),**全程无告警**。
- **边界**:不死锁、不丢 KV、不污染属主。要触发需真有重复推流。属**内容完整性/脑裂**风险,非数据丢失。
- **状态**:~~未修~~ → **已修(2026-07-17)**,并勘误根因:
  - **勘误**:上文"`activeRelays` 条目泄漏 / OnDispose 未生效"的判断**不准确**。relay pull-proxy 是**常驻按需路由**(`StopOnIdle` 只关空闲 publisher,proxy 本体不销毁,下次订阅由 `Server.OnSubscribe` 重新 `Pull()`),条目常驻是设计使然;当时用 `/api/proxy/pull/list` 判断"proxy 已消失"也是误读——该 API 只列 **DB 持久化** proxy,内存 relay proxy 永远不在其中。
  - **真缺陷**:`StreamRegistry.OnPublish` 仅按 streamPath 命中 `activeRelays` 即认定 relay 派生 → 入站推流(`Type="server"`)撞上曾 relay 的路径就跳过 first-write-wins。
  - **修复**:分类加 `pub.Type == PublishTypePull` 门槛(relay 派生必为 pull 派生,`PullJob.Init` 强制)。回归测试 `plugin/cluster/streamregistry_c5_test.go` 3 用例(RED→GREEN);全套 cluster 单测 + 发布 tags 构建通过。修复后入站推流会正常走 KV acquire → 属主在 origin → `ErrStreamPathTaken` 停流,与"relay 前"行为一致。

---

## 4. 批次 B — 单机模式 ✅

| ID | 判据 | 结果 |
|---|---|---|
| B1 | 单机容器起(`cluster.enable:false`) | ✅ API 200;**0 panic**;`/cluster/api` 正确 **404** |
| B2 | 录制单文件 `fragment:"0"` + 直写 S3 | ✅ 字节精确 14994069;**moov 前置**;解码 **0 真实错误**;49.889s |
| B3 | G.711 音频(R3 守卫) | ✅ **`pcm_alaw ch=1`** —— `channels=0` 缺陷已修 |
| B4 | duration 自停后再 stop(R9) | ✅ 自停文件 20700ms≈20s;stop 返回 **500 `{"code":2,"message":"not found"}`** —— 确认行为,调用方应当幂等成功 |
| B5 | **duration 传数字(196 事故)** | ✅ **复现 400** |
| B6 | **fileName + 默认 fragment(R8)** | ✅ **复现并量化丢数据** |

### B5 详情 → 决策 3 发布说明依据

| 请求 | 结果 |
|---|---|
| `{"duration":300}` 数字 | **400** `proto: (line 1:13): invalid value for string type: 300` ← 196 根因复现 |
| `{"duration":"300s"}` 字符串 | **200** ✅ |
| **`{"fragment":0}` 数字** | **400** `invalid value for string type: 0` ← **新增发现** |

⚠️ **its-server 补 `fragment` 时必须发字符串 `"0"`,发数字 `0` 同样 400。**

### B6 详情 → 决策 1 护栏依据

`{"filePath":"b6-filename-trap","fileName":"b6test.mp4"}`(不传 fragment → 默认 1 分钟分片),录 ~174s:

| | |
|---|---|
| 实际录制 | ~174s(08:23:29 → 08:26:23) |
| S3 中对象数 | **1 个**(`b6test.mp4`, 9.6MiB) |
| 该对象时长 | **52.24s** |
| DB 记录数 | **4 条,全部指向同一对象键** |
| **静默丢失** | **~122s = 70%,全程零告警** |

日志直证(两次上传同一 key,后者覆盖前者):
```
08:25:31 [S3] uploading: key=b6-filename-trap/b6test.mp4 size=11565902 → upload successful
08:26:23 [S3] uploading: key=b6-filename-trap/b6test.mp4 size=10043427 → upload successful
```

---

## 5. 批次 F — 录制完整性 × 时长 × 上传

### F1 / F2 / F3 ✅ 全绿

| 判据 | 60s 文件 | 300s 文件 | 判定 |
|---|---|---|---|
| 时长精度 | 61.009s(**+1.009s**) | 301.829s(**+1.829s**) | ≤3s ✅ |
| **帧数核对** | 1526 vs 1525 → **0.05%** | 7539 vs 7546 → **0.09%** | <1% ✅ **无丢帧** |
| A-V duration 差 | 0.000s | 0.020s | <1s ✅ |
| A-V start_time 差 | 0.000s | 0.000s | <0.5s ✅ |
| R3 守卫 | `pcm_alaw ch=1` | `pcm_alaw ch=1` | ✅ |
| moov 前置 | ✅ | ✅ | ✅ |
| **上传字节一致** | 2216742 == 2216742 | 8711065 == 8711065 | ✅ 精确 |
| ETag 分块 | 单块(2.1MiB<64MB) | 单块(8.3MiB<64MB) | ✅ 正确 |
| 对象稳定 | — | 两次 `mc cat` md5 **一致** | ✅ |

**F1 遗留疑点**:monibuca 录的 300s(camera13)有 **4 条 `decode_slice_header`/`Frame num change`**;按计划要求做同源直录对照 —— **300s 匹配对照 0 条**(90s 对照亦 0 条)。但对照**非并发窗口**,归因**未定**。鉴于帧偏差仅 0.09%(2 帧级别),属轻微;建议后续做并发对照定论。

### 🔴 F4 — 上传失败 → pending → 自动补传:**失败(发布阻断项)**

| 判据 | 结果 |
|---|---|
| ① 上传失败重试 | ✅ 退避正常:`retry 1/3 after 4.37s` → `2/3 after 9.25s` → `3/3 after 17.59s`,终 `upload failed after 4 attempts (3 retries)` |
| ② 文件移入 pending + DB 登记 | ✅ `saved failed upload for retry` → `/monibuca/pending_uploads/s3writer_622498919.tmp` **33,200,127 字节**(**录像未丢**);DB `upload_tasks` 登记 `status=3, retry_count=0, max_retries=10, next_retry_at=09:07:28` |
| ③ 「上传成功才入库」延迟入库语义 | ⚠️ **部分符合,需澄清**(见下) |
| ④ 恢复后自动补传成功 | ❌ **失败** — 13+ 分钟 `retry_count` 恒 **0**,零重试日志,对象始终未进 MinIO |
| ⑤ DB 补写 / pending 清空 | ❌ **失败** — pending 文件纹丝不动 |

#### 判据③ 澄清:入库其实是「两段式」

读代码 + F9 日志佐证后确认(初读结论有误,此处更正):

| 时机 | 行为 | 代码 |
|---|---|---|
| 录制**开始** | **立即建 RecordStream 占位行**(有 StartTime/FilePath,无 EndTime/duration) | `recoder.go:101-152` `Save(&r.Event.RecordStream)` |
| 文件写完 + **上传成功后** | **deferred 闭包补齐 EndTime/duration** | `recoder.go:218+` `WriteTailDeferred` → `db save RecordStream (deferred)` |

设计意图明确(注释原文):「**保证数据库记录在文件可播放之后才更新 EndTime**」——所以「上传成功才入库」指的是**补齐完成态**,不是不建行。**该语义本身工作正常**(F9 实测 `db save RecordStream (deferred) ok`,duration 正确写为 30810ms)。

**但由此产生一个真实副作用**:上传失败时占位行**永久停留在 `duration=0` / `endTime=0001-01-01`**,在 `/mp4/api/list` 里表现为一条永不结束的「进行中」录制,且会计入 `recordCount`。实测环境即存在 2 条 4 小时前的 `PRERELEASE-*` 僵尸行。修复 F4 后补传成功即会补齐,但**补传彻底耗尽的场景仍会留下僵尸行**,建议后续补一个「孤儿占位行」巡检/展示口径。

**F5 手动兜底也失效**:`POST /mp4/api/upload/retry?id=1` → `{"code":0,"message":"upload task reset for retry"}`,但 `handleRetryUpload` 只调 `ResetUploadForRetry` 改 DB 字段,依赖的正是那个死掉的调度器 → 文件/MinIO/DB 状态**全部不变**。**F4 修复后该 API 恢复有效**(重置后由已复活的调度器接手)。

#### 根因(已定位到行)

```
server.go:447  s.AddTask(&s.Records)        ← Records 在此启动
server.go:457  s.Records.OnStart(func(){    ← 启动之后才注册回调
                 s.Records.AddTask(&UploadRetryScheduler{s: s}) })

gotask task.go:286  OnStart() { append(afterStartListeners, listener) }  ← 不检查是否已启动
gotask task.go:377  afterStartListeners 仅在启动流程中消费一次
```

回调注册晚于启动 → **永不触发** → 调度器从不启动。

**实证**:node-1 / node-2 / node-3 / single **四个实例任务树中 `UploadRetryScheduler` 出现次数全为 0**;`WorkCollection[...RecordJob]` 无该子任务。

**同根因第二处受害者**:`CheckSubWaitTimeout`(`server.go:482`,`s.Streams.OnStart` 晚于 `s.AddTask(&s.Streams)`)—— 实证两节点任务树出现次数亦为 **0**,而 `PulseInterval` 默认 `5s`(>0)本应注册。

#### 严重性:为何比「暂时不传」更重

`pending_uploads` 位于**容器可写层**(`/monibuca/pending_uploads`,这些部署**未挂载为卷**)。而标准升级流程正是 `docker rm` + `docker run`(本次升 node-3 即如此)。

> **MinIO 抖动 → 上传失败 → 文件进 pending → 永不自动补传(手动 API 亦无效)→ 下次容器重建即永久丢失。**

---

## 6. F4 修复

**改动**(`server.go`):把两处 `OnStart` 注册移到对应 `AddTask` **之前**,并注释说明该顺序约束:

```go
// OnStart 必须在 AddTask 之前注册：AddTask 会立即启动任务，而 OnStart 只是把回调
// 追加进 afterStartListeners，不检查任务是否已启动，该切片又只在启动流程里消费一次。
// 注册晚了回调就永远不触发（补传调度器与心跳检查都曾因此静默失效）。
if s.DB != nil {
    s.Records.OnStart(func() { s.Records.AddTask(&UploadRetryScheduler{s: s}) })
}
if s.PulseInterval > 0 {
    s.Streams.OnStart(func() { s.Streams.AddTask(&CheckSubWaitTimeout{s: s}) })
}
s.AddTask(&s.Records)
s.AddTask(&s.Streams)
...
```

**新增单测**(`upload_retry_register_test.go`)锁定 gotask 语义:
- `TestOnStartAfterAddTaskNeverFires` — AddTask 后注册 → 永不触发(**PASS**,实证缺陷成因)
- `TestOnStartBeforeAddTaskFires` — AddTask 前注册 → 必触发(**PASS**,锁定正确顺序)

**静态验证**:`gofmt` ✅ / `go vet` ✅(`plugin.go:243` copylocks 为预存在)/ 构建 ✅(`-tags cluster,postgres,sqlite,s3`)/ `go test .` ✅

### 真机复验 ✅ 阻断解除

把修复后二进制挂载进 RC 镜像(`-v monibuca_fixed:/monibuca/monibuca_linux:ro`)起容器:

| 任务 | 修复前 | 修复后 |
|---|---|---|
| `UploadRetryScheduler` | **0** | **1** ✅(挂在 `WorkCollection[...RecordJob]` 下) |
| `CheckSubWaitTimeout` | **0** | **1** ✅(挂在 `Manager[...Publisher]` 下) |

**F4 补传闭环复测(同款隔离手法:切断/恢复 S3 通路)**:

```
10:01:15  saved failed upload for retry  → pending_uploads/s3writer_4231131128.tmp
10:05:00  found pending uploads server=5 count=1          ← 调度器已活(修复前永不触发)
10:05:00  retrying upload id=1 objectKey=F4-RETEST/... retryCount=0 fileSize=6128722
10:05:00  [S3] upload successful: F4-RETEST/...mp4
10:05:00  retry upload succeeded id=1 retryCount=1
```

| 判据 | 修复前 | 修复后 |
|---|---|---|
| ④ 恢复后自动补传成功 | ❌ 13min `retry_count` 恒 0 | ✅ **5min tick 自动补传成功** |
| ⑤a pending 目录清空 | ❌ 文件纹丝不动 | ✅ **目录已空** |
| ⑤b DB 状态 | ❌ `status=3`(Failed) | ✅ **`status=2`(成功)** |
| ⑤c 对象进 MinIO | ❌ 桶内为空 | ✅ **5.8MiB 已上传** |

> **结论:F4 发布阻断项已解除。** 待打正式镜像后随 A/C 关键项复跑一轮即可发布。

---

### F8 — 分片连续性(不指定 fileName)✅ **决策 1 的决定性对照组**

`{"fragment":"60s","filePath":"F8-fragments"}`(**不指定 fileName**),录 ~266s:

| 分片对象 | 时长 | 时段 |
|---|---|---|
| `2026-07-16-10-10-41_735157817.mp4` | 60370ms | 10:10:40 → 10:11:42 |
| `2026-07-16-10-11-42_095717766.mp4` | 60000ms | 10:11:42 → 10:12:42 |
| `2026-07-16-10-12-42_055893036.mp4` | 60000ms | 10:12:42 → 10:13:42 |
| `2026-07-16-10-13-42_056018527.mp4` | 60000ms | 10:13:42 → 10:14:42 |
| `2026-07-16-10-14-42_055353726.mp4` | 24240ms | 10:14:42 → 10:15:06(stop 截断) |

- ① **5 个对象 / 5 个唯一文件名 → 零覆盖** ✅ 时间戳命名工作正常
- ② 抽 2 片验证:**moov 前置** ✅、解码 **0 真实错误** ✅(59.959s / 24.239s)
- ③ **片间首尾完全相接**(前片 `endTime` == 后片 `startTime`),总时长 264.6s vs 实际窗口 266s,**缺口 ~1.4s < 2s** ✅ 无丢帧断档

**与 B6 的决定性对照 —— 坐实 R8 只在指定 fileName 时发作**:

| | **B6(指定 fileName)** | **F8(不指定 fileName)** |
|---|---|---|
| 对象数 | **1**(反复覆盖同一键) | **5**(时间戳命名) |
| 数据留存 | 仅最后 52.24s / 174s → **丢 70%** | **全部保留,片间无断档** |

### F7 — >4GB 单文件 / fast-path 回退全量重写 ✅ **(G14 缺口首次真正触达)**

历史「验证 co64/大文件路径」用的是 5.5 MiB 文件,`record.go:305` 的 4GB 分支从未跑到。本次用**高熵噪声源**跨过阈值。

**过程要点**:`testsrc2` 是低熵图案,x264 压到 **32.5 Mbps**(远低于请求的 150M),206s 仅 356MiB —— 撑不过 4GB。改用 `color=gray + noise=alls=100:allf=t+u`(不可压缩)达 **545 Mbps**,90s 产出 **4.61 GB**。

| 判据 | 实测 |
|---|---|
| ① 跨过 4GB | ✅ 成品 **4,951,440,794 字节(4.61 GB)**,`mdat` = 4,951,432,576 |
| ② **fast-path 回退全量重写** | ✅ **确认** —— 4.6GB 文件日志**只有** `write trailer start` / `write trailer`,**无** `insert-range fast path done`;对照同批 356MiB 文件**有**该日志 → 与 `record.go:305`(`mdatSize+BasicBoxLen > 0xFFFFFFFF → return false`)预测一致 |
| ③ **全量重写耗时** | **约 118 秒**(`write trailer start` 10:35:46 → `deferred ok` 10:37:44),折合 **~40 MB/s** 重写吞吐 |
| ④ 成品正确性 | ✅ `moov` 前置(`ftyp(32), moov(8186), mdat(4951432576)`);**全解码 exit=0、真实错误 0**;`duration=72.569s`;**帧数 1815 vs 期望 1814 → 无丢帧** |

**关于 `co64`(修正一个初步误判)**:成品 `含 co64: False / 含 stco: True`,初看像 >4GB 未升级 64-bit 的缺陷。**但实测证伪:并非缺陷** —— `stco` 的 `entry_count = 1`,唯一 chunk offset = **8234**,远不溢出 32-bit;样本位置由 `stsz`/`stsc` 累加得出(demuxer 内部 64-bit)。`co64` 只在 **chunk offset 本身** >4GB 时才必需(多 chunk 且靠后 chunk 起始超 4GB)。**全解码 1815/1814 帧、0 真实错误**是最终证据:**该文件完全正确**。

**磁盘规划结论**:4.6GB 单文件的 trailer 全量重写约需 **2 分钟 + 约 2× 临时空间**(本环境容器 `/tmp` 在 overlay 磁盘、61G 可用,非宿主机 tmpfs)。31 路并发录制若同时跨 4GB,重写风暴的 IO 与临时空间需按此估算 —— **超长单文件建议改用 `fragment` 分段**(F8 已证分段路径完好)。

### F9 — 源流中断路径 ✅

录制中杀掉推流端(非崩溃):

```
insert-range fast path done, upload handed off   ← trailer 正常写
[S3] upload successful
db save RecordStream (deferred) ok               ← 延迟入库补齐
```
DB duration **30810ms ≈ 实际收流 30s** ✅ —— 正常 unpublish 路径下成品完整可播,**不丢数据**。

### D4 / F10 — 进程崩溃(R13)❌ 整段不可恢复(确认结构性限制)

录制 40s 后 `docker kill -s KILL`:

| 判据 | 实测 |
|---|---|
| 磁盘半成品 | `/tmp/s3writer_945747027.tmp` **11,534,336 字节**(≈40s 视频,数据其实都在) |
| box 结构 | `ftyp(32), free(8), mdat(8), <乱码>` —— **`mdat` size=8 占位未闭合** |
| 含 moov | **False** → 不可播 |
| ffprobe | `moov atom not found` / `Invalid data found` |
| ffmpeg 抢救 | 同样失败(`moov atom not found`) |
| S3 对象 | **无** |
| DB | 留下**僵尸占位行** `id=8, duration=0, end_time=空` |

**结论**:progressive mp4 崩溃 → **该段 100% 丢失且不可重建**,与 CLAUDE.md 所述结构性限制完全一致(moov 仅在停录写)。

**缓解措施已由 F8 实证**:`fragment` 分段下每片独立 finalize 并即时上传(实测 5 片各 17MiB,每 60s 落一个完整对象)→ **崩溃最多只损失当前未完成的那一片(≤fragment 时长)**,此前分片全部安全。

---

## 7. 录制 → 上传 可靠性矩阵

| 路径 | 是否丢数据 | 是否可自愈 | 证据 |
|---|---|---|---|
| **正常成功**(单文件) | 否 | — | B2/F1/F2/F3:字节精确、moov 前置、0 丢帧、A-V 0 漂移 |
| **跨节点 relay 录制** | 否 | — | C6:H.265 moov 前置、0 真实错误 |
| **duration 自停** | 否 | — | B4/F1:60s→61.009s;300s→301.829s;20s→20.7s |
| **上传失败(MinIO 抖动)** | 修复前:最终会丢<br>**修复后:否** | 修复前:否<br>**修复后:是(≤5min 自愈)** | F4:修复前文件保住在 pending 但永不补传、容器重建即丢;**修复后 5min tick 自动补传成功、pending 清空、DB 转成功、对象落 MinIO** |
| **指定 fileName + fragment>0** | **是,静默丢 70%** | 否 | B6:174s → 仅存 52.24s,4 条 DB 记录同键覆盖,零告警 |
| **分片(不指定 fileName)** | 否 | — | F8:5 片 5 个唯一名、零覆盖、片间无断档(缺口 1.4s)、各片 moov 前置 0 错 |
| **源流中断(非崩溃)** | 否 | — | F9:trailer 正常写 → 上传成功 → deferred 入库,duration 30810ms ≈ 实际 30s |
| **进程崩溃(progressive)** | **是,当前段 100% 丢失** | **否(不可重建)** | D4/F10:mdat size=8 无 moov,ffprobe/ffmpeg 均报 `moov atom not found`;留僵尸 DB 行 |
| **进程崩溃(fragment 分段)** | 仅当前未完成片 | 部分(此前片安全) | F8 实证每 60s 落一个完整对象 → 崩溃损失 ≤ fragment 时长 |
| **>4GB 单文件** | 否 | — | F7:4.61GB 成品 moov 前置、帧数 1815/1814、解码 0 真实错误;fast-path 正确回退全量重写(耗时 ~118s / ~40MB/s) |

---

## 8. 文档订正清单

1. **CLAUDE.md Known Issues「`plugin/crypto/pkg/transform.go` 预存在编译错」已过时** —— 本次 `go build -tags cluster,postgres,sqlite,s3 ./example/cluster` exit=0,全仓 `go test .` ok。**应删除该条**。
2. **CLAUDE.md「FLV 302-redirects, won't relay」需补充前提** —— 该描述对**代码默认值**(`ProxyOnRedirect default:"false"`)正确;但本部署三节点均配 `proxyOnRedirect: true`,FLV/RTSP 会 relay(200)而非 302。建议补注配置开关。
3. **决策 2(a)**:`example/cluster-e2e/docker-compose.yml` 的共享 PG 形态 → 改为节点本地 sqlite;README + CLAUDE.md 标注跨节点录制列表 / `/download` 302 **不支持**。
4. **新增 Known Issue**:C5 `activeRelays` 泄漏(见 §3)。
5. **计划文档订正**:A3「本机 Mac 即 arm64」有误(实为 Intel x86_64);F4 的「MaxRetries=3 / RetryInterval=5s」对应 S3 层重试,DB 层 `UploadTask.MaxRetries` 默认为 **10**,补传 tick 默认 **5 分钟**;G13 所列 `upload_recover.go` 等文件在**仓库根目录**(package `m7s`)而非 `plugin/mp4/`。

---

## 9. 发布说明必含项

1. 🔴 **必须同步升级 its-server**(决策 3):`duration` 需 `Integer`→`String`;**`fragment` 同样必须是字符串**(`"0"`);stop 收到 500 not-found 应视为幂等成功。
2. 🔴 **指定 `fileName` 时必须同时传 `"fragment":"0"`**(决策 1 护栏)—— 否则默认 1 分钟分片会反复覆盖同一对象键,**静默丢失约 70% 录像**。
3. ⚠️ **cluster 部署必须节点本地 sqlite**(决策 2);跨节点录制列表 / `/download` 302 不支持。
4. ⚠️ 建议把 `pending_uploads` **挂载为宿主机卷**,避免容器重建丢失待补传录像(D4 实证容器可写层内容在 `docker rm` 后即失)。
5. ⚠️ **超长单文件录制建议改用 `fragment` 分段**:F7 实证 4.6GB 单文件 trailer 全量重写需 **~118s + ~2× 临时空间**;且 D4 实证 progressive 单文件崩溃 = 整段不可恢复,而分段可把损失限制在当前片(F8 实证)。

---

## 10. 未执行用例

| 用例 | 原因 |
|---|---|
| A3 arm64 真机 run | 本机为 Intel x86_64(i7-9750H),无 arm64 硬件 —— **G3 缺口仍在** |
| D1 共享 PG | 决策 2 选 (a),按计划跳过 |
| **D2 节点故障 failover** | **被 §11 环境事件阻塞** —— 判据需真实流,摄像头网段已封 |
| D3 Consul 故障 | 未执行 |
| D5 30 路 × 15min 长录制 | 未执行(被 §11 阻塞,需真实流) |
| F5 补传耗尽 / F6 pending 水位告警 | 未执行(F5 的手动 API 路径已在 F4 中间接验证:修复前无效、修复后由复活的调度器接手) |
| E1/E2/E3 | P2 可选 |

**已完成的 P1**:**F7(>4GB,G14 缺口已填)**、D4/F10(崩溃量化,R13 已实证)、F8(分片连续性)、F9(源断)—— 见 §5。

---

## 11. ⚠️ 测试期间环境事件:摄像头网段 192.168.12.0/24 被封

**现象**(2026-07-16 约 09:56 起,10:18:20 批量 unpublish):

- 三节点流数 10/10/10 → **0/0/0**;Consul `m7s/streams` 30 键 → **0**。
- `192.168.12.0/24` 从 131/134/133 **全不可达,连 ICMP 都不通**(含网关 `192.168.12.1`);而路由出口 `172.16.12.254` 本身正常。
- m7s 容器全部健康(Up 5h,API 200);生产 `xde-*` 容器 healthy;生产 `monibuca_linux` 进程存活(134 上 streamCount=0/pullCount=0)。

**与历史事故签名一致**(见记忆 `cluster-3node-test-servers`):

> node-2 一次性拉 12 路 RTSP + 超时重试,被网关/安全设备当端口扫描,整个 192.168.12.0/24 从三台全失联(连 ICMP 都不通,网关本身正常)。**对策:用户关 IPS**;恢复后用 `staggered-pull.sh` 逐路错峰(间隔 3s)平缓加载。

**归因(不确定,仅列证据)**:

| 时间 | 事件 |
|---|---|
| 08:45 / 08:52–08:57 | 本次为 F1 归因做的 **2 次直连 camera13(192.168.12.51)直录**(90s + 300s) |
| 07:26 起(全程) | 日志持续出现 `pull proxy heartbeat failed ... broken pipe` —— 30 路拉流重试风暴 |
| **09:56:13** | **首次 `video timeout`** |
| 10:10–10:17 | F8/F9/D4 —— **全部合成流(testsrc2 → 本地 RTMP),未碰摄像头网段** |
| 10:18:20 | 批量 `unpublish reason=publish timeout` |

直录操作距首次异常约 1 小时,**时间上不直接吻合**;30 路重试风暴同样可疑。**无法确定归因**,不排除两者叠加。

**对测试结论的影响:无。** 所有 P0 批次(A / B / C / F1–F4)均在 09:56 前完成取证;F8 / F9 / D4 使用合成流,不依赖摄像头网段。

**恢复需人工介入**:关闭 IPS / 联系网络管理员解封;恢复后按 3s 间隔错峰加载,勿一次性全拉。

### §11.1 后续(2026-07-17):封禁解除后流不自愈 → 发现第三个缺陷(退避无上限)

IPS 解封后(摄像头 554 全部可达)三节点流数仍为 0。排查:

- 任务树 10 个 PullJob 全部存活(`maxRetry:-1` 无限重试),`pullStarted=true` 无误;
- **铁证**:10 个 RTSPPuller 的 description 全部 `retryDelay=11h22m40s, retryCount=14`,与 `5s×2^13=40960s` **精确吻合**;
- 根因:gotask 退避 `RetryInterval×2^(retryCount-1)`,上限判断被 `MaxRetryInterval>0` 门控;`puller.go` 只调 `SetRetry`,从不设上限 → 无界。`retryCount` 仅在 Start **成功后**归零(task.go:386),故短暂抖动自愈正常,**长中断后 puller 沉睡数小时,网络恢复也不再尝试** → 永不自愈,只能重启进程。
- 已核实 `WaitCloseTimeout`/`PublishTimeout`/`IdleTimeout`/`WaitTimeout` 等配置均作用于 Publisher/Subscriber 层,**无法缓解**此问题;`RetryInterval: 5s` 只是退避基数,不是固定间隔。
- 影响面:`ae298dab`(R5)当时只给 recorder 设了 30s 上限,**puller/pusher/transformer/cascade 四处全部漏配** —— 不完整修复。
- **修复**:commit `3a554681`,四处补 `SetMaxRetryInterval(30s)`;语义回归 `retry_backoff_test.go`。
- **连带结论**:昨天 IPS 封禁的最大实际伤害不是封禁本身,而是它触发了这个不自愈状态 —— 生产若遇交换机维护/网段抖动超过约 1 小时,同样会静默丢流直到有人重启。

---

## 12. 二轮测试(2026-07-17,镜像 v5.3.3.2607171320)

三个修复(F4 调度器注册竞态 `1665f6b2` / C5 relay 分类 `1665f6b2` / 退避无上限 `3a554681`)合入后打正式镜像 `v5.3.3.2607171320`(git tag == 镜像 `vcs.revision=3a554681`,一致性问题已修),推 SWR,三节点逐台错峰升级(各保留 `-old-v532` 回滚容器),复跑报告 §0 要求项:

| 项 | 结果 |
|---|---|
| A1/A5 | ✅ 新 `--provenance=false --sbom=false` 生效:manifest 恰 2 条目(amd64+arm64),无 attestation |
| A2 | ✅ `-tags=cluster,postgres,sqlite,s3`;`vcs.revision=3a554681` == git tag;`vcs.modified=false`;gotask v1.0.5 |
| A4 | ✅ 三台 pull manifest tag 成功 |
| C1 | ✅ peers=3/3/3;Consul nodes=3、streams=30;属主 10/10/10 |
| **C5** | ✅ **修复真机闭环**:同一条 camera12,relay 前推 exit=224;relay 一次(日志证实路由建立);**relay 后推 exit=224(一轮时为 exit=0 漏防)**;日志显示 relay 后推流走了 `acquire stream key failed` → `unpublish reason=cluster: streamPath already owned by peer`;死锁探测 3/3=200;属主全程 node-1 |
| **F4** | ✅ **正式镜像(非挂载二进制)闭环**:任务树 `UploadRetryScheduler=1`、`CheckSubWaitTimeout=1`;切断→pending(6,107,971 字节)→DB 登记→恢复→**tick 自动补传成功**(`found pending uploads count=1 → retry upload succeeded`)→pending 清空、DB status=2、5.8MiB 对象落 MinIO |
| **退避上限**(§11.1 修复) | ✅ 升级重启后三节点 30 路**全部即时恢复**(反向坐实退避为不自愈根因);真机探针观察到 `count=2→delay=10s`(退避公式生效;探针拉空流会间歇成功重置 count,未持续到 count≥4 的直接封顶观察);30s 封顶由 `retry_backoff_test.go` 语义测试 + BuildInfo 含 `3a554681` 保证 |

二轮后集群:三节点 api=200、流 10/10/10、Consul 30 键;测试容器/转发器/临时对象已清理。

**发布状态:三个阻断/严重缺陷全部修复并在正式产物上验证,`v5.3.3.2607171320` 为当前发布候选。**

## 13. 三轮测试(2026-07-17,镜像 v5.3.4.2607171508)

develop 合并 `upstream/v5`(官方 14 个修复,merge commit `894e6a2c`,4 文件冲突融合详见提交信息)后重新打包,tag/镜像/代码三者一致(`vcs.revision=894e6a2c`)。三节点逐台错峰升级(保留 `-old-v533` 回滚容器),随后完成端到端单节点与多节点集群测试:

**单节点(134,独立容器 + TCP 转发器隔离 MinIO)**:

| 项 | 结果 |
|---|---|
| B1 启动 | ✅ API 200;`/cluster/api` 正确 404;0 panic;`UploadRetryScheduler`/`CheckSubWaitTimeout` 均在任务树 |
| B2 录制上传完整性 | ✅ 38.9s 录制:S3 字节精确(11,705,172)、moov 前置、解码 0 真实错误、DB 时长一致 |
| B5 API 兼容 | ✅ `{"duration":300}` 数字仍 400(升级说明结论对 v5.3.4 有效) |
| F4 补传闭环 | ✅ 切断→pending(DB status=3)→恢复→tick 自动补传成功→pending 清空、DB=2、5.5MiB 落桶 |

**多节点集群(131/134/133)**:

| 项 | 结果 |
|---|---|
| C1/C2 | ✅ peers 3/3/3;Consul nodes=3、streams=30;属主 10/10/10 |
| C4 relay 三方向 | ✅ 三方向均建立并供流(4s 62 包) |
| C5 同名冲突 | ✅ 修复保持:camera11(未 relay)exit=224;**camera1(activeRelays 命中)exit=224**;4 次 `owned by peer` 停流;死锁探测 3/3=200;属主不变 |
| C6 跨节点 H.265 录制 | ✅ hevc、moov 前置、解码 0 真实错误 |
| C7 lb-suggest | ✅ suggested 合理 |
| C8 日志扫描 | ✅ 三节点 0 panic / 0 database-locked / 0 discard / 0 fatal |

**结论:`v5.3.4.2607171508`(三修复 + upstream 14 修复)单节点与集群端到端全绿,为当前发布候选。**

## 14. 环境状态(收尾)

**集群**:三节点 **api=200 / peers=3 / Consul `m7s/nodes`=3**,进程与集群协调健康。
**流**:因 §11 的网段封禁,摄像头流为 **0**(需人工解封 IPS 后错峰恢复)。

**已清理**:
- 测试容器 `m7s-single` / `m7s-single-fixed` 已删除;TCP 转发器、临时二进制、临时配置与数据目录、临时 mp4 均已清理;无遗留 ffmpeg 进程。
- 测试录制对象(`F4-upload-fail/` `F4-RETEST/` `F7-4GB/` `F8-fragments/` `F9-source-cut/` `D4-crash/` `B2-single/` `b4-autostop/` `b6-filename-trap/` `c6-crossnode/` `F-60s/` `F-300s/`)已从 MinIO 删除(含 4.6GB 大对象)。134 磁盘 `/` 剩余 61G。

**保留**:
- **node-3 回滚容器 `m7s-node-3-old-v531` 在 133**(如需回滚到 v5.3.1)。
- 修复后二进制源码改动在工作树(`server.go` + `upload_retry_register_test.go`),**未提交、未打镜像、未推 SWR**。

**运维注意**:
- ⚠️ 133/134 SSH 曾因高频连接触发 **fail2ban**(CLAUDE.md 已警示)。后续用 `ControlMaster` 复用连接(注意 socket 路径 <104 字节)。
- ⚠️ `pkill -f <pattern>` 若 pattern 出现在自身命令行会**误杀 SSH 会话**,应改用 PID。
