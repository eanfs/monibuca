# 发布前测试计划 — monibuca v5.3.x(develop → v5)

- **制定日期**: 2026-07-16
- **被测**: develop `f6a5fc0b`
- **基线**: 镜像 tag `v5.3.1.2607151131`(代码 `2a600639`),已于 2026-07-15 三节点容器版**全量通过**(见 `report-2026-07-15-docker.md`)
- **状态**: 待 review,review 通过后执行

---

## 0. 范围判断(请先看这段)

```
git log --oneline v5.3.1.2607151131..develop
f6a5fc0b chore: 清理废弃 example 测试目录,补充 mp4 录制 API 陷阱文档与 196 事故报告
```

**待发布代码相对已通过全量测试的镜像只多 1 个 chore 提交,功能代码零变更**:

| 改动 | 影响面 |
|---|---|
| CLAUDE.md 补 mp4 API 陷阱 + Known Issues | 文档 |
| 删除 example/cascade*、cluster-dc、cluster-test | 无(废弃目录) |
| `build_docker.sh`: `-o type=docker,dest=-` → `--load` | **唯一影响发布产物的变更** |

**所以本次不是"回归全测"**,而是三件事:

| | 目标 | 为什么 |
|---|---|---|
| **A** | 打包链路首跑验证 | `build_docker.sh` 改完后从未完整跑过,它直接决定发布产物 |
| **B** | 补测从未覆盖的部署形态 | 共享 PG / arm64 运行 / 故障恢复 / 升级路径,都是真实交付形态 |
| **C** | 已知未修缺陷定性 | fileName+fragment 静默丢数据、duration 类型不兼容 —— 发布前必须决定"修 or 文档护栏" |

已通过的 30 路 × 15min 长录制、跨节点录制、三模式完整性等**不重跑全量**,只在批次 C/D 里做低成本抽样与日志扫描。

---

## 1. 历史问题清单 → 本次处置

| # | 问题 | 修复 | 末次验证 | 本次处置 |
|---|---|---|---|---|
| R1 | cluster 同名流 KV acquire **死锁**(RC1 重入 SafeGet / RC2 阻塞 acquire 跑在 Streams 事件循环 / RC3 relay 误抢锁)+ 次生误删 peer 键 | m7s `streamregistry.go` 等 3 文件 | 07-15 场景 7 ✅ | **P0 复跑**(C5)—— 历史最严重故障,成本仅 1 组 curl |
| R2 | gotask `Job.Call` 同 goroutine 重入死锁(R1 的库层根因) | `eanfs/gotask v1.0.5` | 07-14 ✅ | 随 C5;A2 校验 BuildInfo 确含 v1.0.5 |
| R3 | RTSP G.711 → `channels=0` → mp4 音频轨非法、整文件不可解析 | `5c9eb043` | 07-15 ✅ | **P0 抽样**(B3/C6) |
| R4 | sqlite 开 100 连接并发写锁 → 录制重启雪崩 | `dd2b96ec` | 07-15 FINAL3 ✅ | P1 长录制窗口扫 `database is locked` |
| R5 | 机械盘 IOPS 雪崩(每帧一次 write syscall + 退避上限仅 2s) | `ae298dab` | 07-15 FINAL3 ✅ | 同 R4,扫 `reason=discard` |
| R6 | 上传内联在单线程 trailer 队列 → 恒 `active=1/4` 串行 | U1 解耦 | 07-15 `active=2~4/4` ✅ | 同 R4,扫 `active=N/4` |
| R7 | **`duration` 传数字 → 400,录制根本不启动**(升级不兼容) | **不修**(proto string 是正确设计) | 196 实证 | **决策 3** |
| R8 | **指定 `fileName` + fragment>0 → 反复覆盖同一对象键,静默丢数据** | **未修** | FINAL2 批次 30 路全作废 | **决策 1** + B6 量化复现 |
| R9 | `duration` 到点自停后再 stop → 500 not-found | **不修**(语义正确) | 07-15 ✅ | B4 确认行为,写进发布说明 |
| R10 | **`pull_proxy_configs` 无节点归属列 → 共享 DB 时任一节点重启拉全网** | **未修**(已核对 `pull_proxy.go:43` `PullProxyConfig` 确无 node/server 列) | 06-03 实证 | **决策 2** + D1 |
| R11 | build_docker.sh 缺 cluster tag / tar 导出在新版 Docker 失效 | `6409caf3` + `f6a5fc0b` | **未复跑** | **批次 A 首跑** |
| R12 | webrtc 默认 TCP 9000 撞生产 MinIO → panic 整进程 | 部署配置(`enable:false`) | 教训 | 部署前置检查项(§5) |
| R13 | progressive mp4 **录制中崩溃不可恢复**(moov 只在停录时写) | **未修**(结构性) | **未测** | P1 D4 实测量化损失 |

**顺带发现(文档待订正)**:CLAUDE.md Known Issues 里的「`plugin/crypto/pkg/transform.go` 预存在编译错」**已过时** —— 本次实测 `go build ./...` 与 `go build -tags "cluster sqlite s3" ./plugin/crypto/...` 均 exit=0,`go test -tags cluster ./plugin/cluster/...` ok。发布前应删除该条。

---

## 2. 覆盖缺口(未完全覆盖的部署形态)

| # | 形态 / 场景 | 现状 | 计划 |
|---|---|---|---|
| G1 | 三节点 + **共享 PostgreSQL** | **从未跑过**。且 `example/cluster-e2e/docker-compose.yml` 正是此形态(`dsn: postgres://...`),与 07-15 真机验证的「节点本地 sqlite」**不一致**;叠加 R10 未修 → 重启拉全网 | **决策 2** → D1 |
| G2 | 跨节点录制列表聚合 / `/mp4/download` 302 | 本地 sqlite 架构下不可能(元数据各存各的);只有共享 DB 形态才有意义 | 随 G1 |
| G3 | **arm64 镜像真机运行** | 只构建过 + amd64 smoke;**arm64 从未 run 起来过** | **P0 A3**(本机 Mac 即 arm64,成本近零) |
| G4 | 节点故障 failover(KV 过期 / relay 退出 / 重启恢复) | 只在从未跑过的 compose smoke 场景 8 里有 | P1 D2 |
| G5 | Consul 故障 / 会话过期 / 网络分区 | **完全未测** | P1 D3 |
| G6 | 进程崩溃恢复 | 未测(即 R13) | P1 D4 |
| G7 | **升级路径 v5.3.0 → 新版** | **未测**;196 事故本质就是升级不兼容 | **P0 B5** + 决策 3 |
| G8 | its-server(`xde-media-spring-boot-starter`)联调 | 196 事故后其侧 3 处未改、未复测 | 决策 3(跨团队) |
| G9 | xde-installer Consul 集群部署(MR #167) | 待真实环境部署验证 | P2(独立仓库,可发布后) |
| G10 | fmp4 录制类型(`record.go:545`) | 未测 | P2 |
| G11 | `-race` 全绿 | gotask 上游未修,已知 EventLoop 竞争 | 不阻断,记录 |
| G12 | `example/cluster-e2e` docker stack | **从未实跑**;smoke.sh 场景 4/5/6/7/9 仍手动 | P2 E1 |
| **G13** | **上传失败 → pending 暂存 → 自动补传 → 补传耗尽 → 管理 API** | **整个子系统从未测过**(`upload_recover.go` / `upload_retry.go` / `upload_admin.go` / `RaiseUploadAlarm`)。历史所有测试 MinIO 全程健康,**失败路径一次没走过** | **F4/F5/F6** |
| **G14** | **>4GB 单文件(co64 + fast-path 回退全量重写)** | 07-14 报告称"验证 co64/大文件写入路径",但用的是 **5.5 MiB** 文件 —— `record.go:305` 的 4GB 分支**从未触达**;历史最大文件仅 666MB | **F7** |
| G15 | 分片模式 `fragment>0` 的分片连续性 | 唯一一次分片录制(FINAL2)因 R8 的 fileName 覆盖缺陷全批作废,**分片本身是否连续无丢帧从未验证** | F8 |
| G16 | 帧数 / 丢帧率 / A-V 同步 | 长录制只核对了"时长",未核对帧数与音视频漂移(06-03 短录制核对过帧数,之后没再做) | F1/F2 |
| G17 | 源流中断(非崩溃)路径 | 录制中源断开 → trailer 是否正常写、成品是否可播,未系统验证 | F9 |

---

## 3. 测试批次

### 批次 A — 打包与产物自检(P0,≈40min)

前置:develop `f6a5fc0b` 已推;打 tag `v5.3.2.<yyMMddHHmm>`(或续 `v5.3.1.x`,见决策 3)。

| ID | 步骤 | 判据 |
|---|---|---|
| A1 | `./build_docker.sh <tag>` 双架构构建 + 推 SWR + manifest | 脚本 exit=0;**`--load` 路径在本机 Docker 版本上跑通**(R11 复验) |
| A2 | 镜像内二进制自检 | `go version -m` 含 `-tags cluster,postgres,sqlite,s3` 且 `github.com/eanfs/gotask v1.0.5`(R2 守卫) |
| A3 | **arm64 镜像本机 run** | `docker run --platform linux/arm64` 起进程、listen 8080/50051、`ffmpeg -version` 在位(G3 首次) |
| A4 | 三节点 `docker pull` | 131/134/133 均拉取成功(134 历史上无 SWR 凭证) |
| A5 | manifest 校验 | `docker manifest inspect` 含 linux/amd64 + linux/arm64 |

### 批次 B — 单机模式(P0,≈30min)

理由:196 及多数生产为单机形态,且升级兼容性问题只在这里暴露。

| ID | 步骤 | 判据 |
|---|---|---|
| B1 | 单机容器起(`cluster.enable:false`) | API 正常,0 panic |
| B2 | 录制单文件 `{"fragment":"0"}` 5min + 直写 S3 | moov 在文件头 / 时长符合 / 全解码 0 真实错误 |
| B3 | G.711 音频抽样(camera10 或 camera27) | `pcm_alaw ch=1`(R3 守卫) |
| B4 | `{"duration":"300s"}` 到点自停,之后再调 stop | 有 `duration reached` 日志;stop 返回 **500 not-found** —— 确认行为并写入发布说明(R9) |
| B5 | **`{"duration":300}`(数字)** | **复现 400 `invalid value for string type`**,与 196 一致 → 作为升级说明的依据(G7/R7) |
| B6 | **指定 `fileName` 且不传 `fragment`,录 3min** | **复现"只剩最后 1 分钟"**,量化数据丢失 → 决策 1 的依据(R8) |

### 批次 C — cluster 三节点核心回归(P0,≈60min)

复用 133 的 Consul+MinIO 与 131/134/133 现有环境(30 路拉流仍在线,可直接续测)。

| ID | 场景 | 判据 |
|---|---|---|
| C1 | membership | 三节点 `/cluster/api/cluster/nodes` peers=3;Consul `m7s/nodes/` 3 键 |
| C2 | 流注册与属主 | `m7s/streams/live/*` 键数与在线路数一致,属主映射正确 |
| C3 | 302 路由(三方向) | 302 到正确 advertise 地址 |
| C4 | auto-relay 播放(RTSP,三方向) | 持续供流(≥4s 收帧,~20fps) |
| C5 | **同名冲突不死锁 + release-guard** | acquire 失败 → publisher 干净 unpublish;`/api/stream/list` 连探 5 次全 200;**KV 属主仍为原 peer**(R1/R2 核心) |
| C6 | 跨节点 relay 录制 + 完整性(抽 1 路 H.265 或 4K) | moov 前置、全解码 0 错 |
| C7 | lb-suggest | `suggested` 非空且合理 |
| C8 | 日志扫描 | 0 panic / 0 `database is locked` / 0 `reason=discard`(注意先 `sed -E 's/\x1b\[[0-9;]*m//g'` 去色码,否则漏配) |

### 批次 D — 未覆盖形态(P1,≈2-3h)

| ID | 场景 | 判据 / 产出 |
|---|---|---|
| D1 | **共享 PG 形态**(决策 2 = 支持才做):三节点 dsn 指同一 PG | ① 跨节点 `/mp4/api/list` 能否聚合 ② `/mp4/download` 是否 302 到属主 ③ **重启一个节点,观察是否拉全网**(R10 量化) |
| D2 | 节点故障:`docker stop` node-1 | KV `m7s/streams/*` 10-12s 内消失;node-2 上 relay 退出;剩余节点 stream/list 健康;**重启 node-1 后只恢复自己的流**(本地 sqlite 形态下的正确行为) |
| D3 | Consul 故障:停 consul 60s 再起 | 节点不 panic;恢复后重注册;会话 TTL 过期后属主键行为符合预期 |
| D4 | **崩溃恢复**:录制中 `docker kill -s KILL` | 量化 progressive mp4 损失(预期整段不可播,R13);对照验证 **fragment 分段能把损失限制在当前段** → 产出部署建议 |
| D5 | 长录制稳定性:30 路 × 15min `duration:"900s"` 自停(FINAL3 同款) | 30/30 上传成功;0 discard / 0 database-locked;时长 900±3s(R4/R5/R6 一次性覆盖) |

### 批次 E — 可选(P2)

| ID | 内容 |
|---|---|
| E1 | `example/cluster-e2e` docker stack 实跑 `smoke.sh` + `extended.sh`;顺带按决策 2 把 compose 的 dsn 改成与真实形态一致,并把手动场景 4/5/6/7/9 尽量补成断言 |
| E2 | fmp4 录制类型验证(崩溃安全性,可作为 R13 的替代方案证据) |
| E3 | `go build ./...` + `go test -tags cluster ./plugin/cluster/... -count=1`(已预跑通过);顺手删除 CLAUDE.md 里过时的 crypto 编译错条目 |

---

### 批次 F — 录制完整性 × 时长 × 上传 MinIO 专项(追加,≈3-4h)

> 历史报告对这条链的验证一直是「**成功路径**的时长 + moov 位置 + 全解码」。本批次补三类从未覆盖的:
> **① 失败路径**(上传失败 → 补传,整个子系统零测试)、**② 真·边界**(>4GB co64)、**③ 更严的完整性判据**(帧数 / A-V 同步 / multipart ETag,而不只是"时长对得上")。

#### F 组前置:统一校验口径

- ffprobe 用 **134 的标准版**;`mc alias set m7s133 http://172.16.12.133:29000 admin m7sm7sm7s`。
- 「全解码 0 错」的**已知无害噪音**须排除(与历史报告同口径):`non monotonically increasing dts X >= X`(等值 dts);camera27/29 偶发 `decode_slice_header` 为**源流 RTP 丢包坏帧**,非录制问题 —— 判定前先用同源同时段直录对照,别把源侧问题记到录制头上。

| ID | 场景 | 判据 | 优先级 |
|---|---|---|---|
| **F1** | **时长精度矩阵 + 帧数核对**:60s / 300s / 900s(均 `duration` 自停,`fragment:"0"`)各 1 路 | ① ffprobe `format.duration` 偏差 **≤3s**(到点停在下一关键帧)② **帧数核对**:`nb_frames` vs `fps × duration` 偏差 **<1%** —— 长录制此前只核对时长,未核对丢帧 | **P0** |
| **F2** | **音视频同步 + 轨道完整**:取含 G.711 的路(camera10 / camera27)录 300s | ① 音频 `pcm_alaw ch=1`(R3 守卫)② **音视频 duration 差 <1s**、`start_time` 差 **<0.5s**(A-V 漂移)③ 全解码 0 真实错误 | **P0** |
| **F3** | **上传字节一致 + multipart 正确**:对 F1/F2 产物 | ① 录制端日志 `fileSize` **== `mc stat` size**(精确一致)② **>64MB 对象**(`s3.PartSize` 默认 64MB)ETag 形如 `<hash>-<N>`,**N == ceil(size/64MB)** → 证明 multipart 分块数正确、无缺块 ③ 两次 `mc cat` 下载 md5 一致(对象稳定) | **P0** |
| **F4** | **上传失败 → pending → 自动补传**(G13 首测,**本批次最高价值**):录制中 `docker stop` MinIO → 停录触发上传 | ① 上传失败重试(默认 `MaxRetries=3` / `RetryInterval=5s` / 退避上限 5min)② 文件**移入 pending 目录** + DB 登记(`saved failed upload for retry`)③ **此时不写 RecordStream** —— 验证"上传成功才入库"的延迟入库语义 ④ 启回 MinIO → **补传循环自动上传成功** ⑤ DB 补写记录、**pending 目录清空**、本地临时文件不残留 | **P0(强烈建议)** |
| **F5** | **补传耗尽 + 管理 API**:MinIO 持续不可用直到补传次数耗尽 | ① `GET /mp4/api/upload/exhausted` 能列出被放弃的任务 ② 恢复 MinIO 后 `POST /mp4/api/upload/retry?id=N` 重新拉入补传循环并成功 ③ 对象最终完整可播 | P1 |
| **F6** | **pending 目录水位告警**:填充 pending 至接近上限 | `RaiseUploadAlarm(AlarmDiskSpaceFull)` 触发,且**去重生效不刷屏**(`upload_retry.go:42` 巡检) | P2 |
| **F7** | **>4GB 单文件 co64 路径**(G14,从未真正触达) | 用**合成高码率流快速撑过 4GB**(如 `-f lavfi testsrc2` 高码率推 RTMP,或不加 `-re` 全速推高码率源;4GB≈32Gbit,100Mbps 约 5.5min)→ 验证:① `stco→co64` 升级发生 ② fast-path 按 `record.go:305` **回退全量重写**(日志 `insert-range skipped` 或走 temp 全量重写分支)③ **临时盘 2× 空间占用与 trailer 耗时可接受**(直接决定生产磁盘规划)④ 成品 ffprobe 时长/帧数正确、**全解码 0 错**、moov 位置正确 | **P1(重要)** |
| **F8** | **分片模式连续性**(G15,兼作决策 1 的**对照组**):`fragment:"60s"` 且**不指定 fileName**,录 5min | ① 产出 **5 个对象、时间戳命名互不覆盖**(证明"不指定 fileName 时分片工作正常",反衬 R8 只在指定 fileName 时炸)② 各片 ffprobe 可播、moov 正确 ③ **各片时长求和 ≈ 300s,总缺口 <2s**(片间无丢帧断档) | P1 |
| **F9** | **源流中断路径**(G17):录制中停掉 pull-proxy / 断源 | 正常 unpublish → **trailer 正常写** → 成品可播、时长 == 实际收流时长(区别于 F10 的崩溃路径) | P1 |
| **F10** | **进程崩溃**(= D4,合并执行) | 量化 progressive mp4 损失(预期整段不可播,R13);对照验证 `fragment` 分段把损失限制在当前段 | P1 |

**F 组产出**:一张「录制→上传 可靠性矩阵」(成功 / 上传失败可补 / 源断 / 崩溃 / >4GB / 分片 六种路径 × 是否丢数据 × 是否可自愈),直接作为生产部署与运维手册的依据。

---

## 4. 待 review 决策点(3 个)

### 决策 1 —— R8 `fileName + fragment>0` 静默丢数据:发布前修,还是文档护栏?

- **现状**:`plugin/mp4/pkg/record.go:494 CustomFileName` 在 `RecConf.FileName` 非空时,每个分片都返回同一路径;`plugin/mp4/api.go:714` 不传 fragment 默认 **1 分钟** → **默认参数组合即触发**,且全程无告警。
- **代价已实证**:07-15 FINAL2 批次 30 路 15min 录制每路只剩最后 1 分钟,**整批作废**。
- **选项**:
  - **(A) 分片名加时间戳**(`fileName_2006-01-02-15-04-05.mp4`)—— 语义最友好,但改变既有文件命名契约,可能影响 its-server 按名取回。
  - **(B) `StartRecord` 校验该组合直接 400 拒绝** —— 约 4 行 + 1 单测,零命名契约变更,把静默丢数据变成显式报错。
  - **(C) 不修,发布说明写死"指定 fileName 必须同时传 `fragment:"0"`"**。
- **建议:B**。这是唯一"静默丢用户数据"的缺陷,发布前把它变成显式错误成本最低、收益最高。

### 决策 2 —— R10 共享 PostgreSQL 集群形态:支持还是明确不支持?

- **现状**:`PullProxyConfig`(`pull_proxy.go:43`)无节点归属列,共享 DB 时任一节点重启会加载全表 `pull_on_start` → 拉全网 + KV 属主冲突 + 负载激增。
- **矛盾点**:仓库里的 `example/cluster-e2e/docker-compose.yml` **就是共享 PG 形态**,与 07-15 真机验证并推荐的「节点本地 sqlite」形态相反,会误导使用者;而跨节点录制列表/download 302(旧场景 6/7)**只有共享 DB 才有意义**。
- **选项**:
  - **(a) 明确"cluster 部署必须节点本地 sqlite"** —— 改 e2e compose + README + CLAUDE.md,跨节点 list/302 标注为不支持。零代码风险,与已验证的生产形态一致。
  - **(b) 加节点归属列 + 按本节点过滤** —— 改动大(含 migration),需重测。
  - **(c) 维持现状,D1 实测量化风险后再定**。
- **建议:a**(若近期没有跨节点录制列表的产品需求)。

### 决策 3 —— R7 its-server 升级不兼容:发布节奏怎么排?

- **现状**:新版 `duration` 为 proto **string**,its-server(`xde-media-spring-boot-starter` ≤3.2.18)发 **Integer** → **400,录制完全不启动**(196 已实证,且旧版靠 `DiscardUnknown` 静默忽略才"看起来能用")。
- **选项**:
  - **(a) 等 its-server 改完再一起发**(Integer→String + stop 把 not-found 当幂等成功 + 补 `fragment:"0"`)。
  - **(b) 先发 monibuca,发布说明标红"必须同步升级 its-server"**。
  - **(c) monibuca 侧兼容数字**(需 custom unmarshaler,污染 proto 契约,不建议)。
- **建议:取决于 196 演示时间点**。若 196 近期要演示 → (a);否则 (b) + 发布说明。**无论哪个,B5 都要跑,把 400 的复现结论写进发布说明。**

---

## 5. 执行环境与前置检查

- **三节点**:node-1 `172.16.12.131` / node-2 `172.16.12.134` / node-3 `172.16.12.133`,容器 `m7s-node-{1,2,3}`(host 网络)。
- **infra 在 133**:Consul `:8500`、MinIO `:29000`(桶 `m7s-records`,admin/m7sm7sm7s)。
- **生产隔离铁律**:131 与 134 均有生产业务。只碰 `m7s-*` / `cluster-*` 容器与既定高端口;**绝不触碰** `xde-*` / opengauss / 生产 minio / host 上的 `monibuca_linux`(7080/554/50051)。
- **配置必查**:`webrtc` / `webtransport` / `gb28181` 必须 `enable:false`(R12:webrtc 默认 TCP 9000 撞生产 MinIO 会 panic 整进程)。
- **摄像头**:错峰加载 3s/路(IPS 把批量 RTSP 当端口扫描,曾封整个 192.168.12.0/24)。已知 DOWN:camera4/5/6/33。
- **工具**:ffprobe 用 **134 的标准版**(131 是 Sophon 硬解版,`BMVidDecCreate failed`);133 **无 ffmpeg**,测试客户端别放这台。
- **日志扫描**:先去 ANSI 色码再 grep,否则漏配 `reason=discard`。

## 6. 工时估算

| 批次 | 时长 | 阻断发布? |
|---|---|---|
| A 打包与产物自检 | 40min | 是 |
| B 单机模式 | 30min | 是 |
| C cluster 三节点核心回归 | 60min | 是 |
| **F(P0 部分:F1-F4)** | **≈1.5h**(F1 的 900s 录制窗口可与 C 并行) | **是** |
| **P0 小计** | **≈4h(一个工作日上午 + 午后)** | |
| D 未覆盖形态 | 2-3h(D5 含 15min 录制窗口) | 否,但 D1/D4 结论影响文档 |
| F(P1 部分:F5/F7/F8/F9/F10) | 2h(F7 的 4GB 撑量约 6-10min/次) | 否,但 F7 结论影响磁盘规划 |
| E 可选 | 按需 | 否 |

**排期建议**:F1/F2 的长录制窗口与批次 C 的 cluster 回归**并行跑**(录制在后台、C 的 curl 在前台),可省约 30min。F4 需要独占 MinIO(要 stop 掉),**必须与其它所有用 S3 的用例错开**,建议放在 C/D 全部结束之后单独跑。

## 7. 交付物

1. `cluster-e2e-reports/report-2026-07-16-prerelease.md` —— 执行结果报告。
2. **「录制→上传 可靠性矩阵」**(批次 F 产出):六种路径 × 是否丢数据 × 是否可自愈 → 进运维手册。
3. 决策 1/2/3 的结论 → 落到 **发布说明** + CLAUDE.md Known Issues 订正(含删除已过时的 crypto 条目)。
4. 若决策 1 选 A/B:修复 commit + 单测,并在批次 B 复验。
5. 若决策 2 选 (a):`example/cluster-e2e/docker-compose.yml` + README 改为节点本地 sqlite 形态。
6. **若 F7 显示 >4GB 全量重写代价过大**(临时盘 2× 空间 / trailer 耗时过长):产出磁盘规划建议 + 「超长单文件录制建议改用 fragment 分段」的部署约束。
7. **若 F4 失败**(补传子系统不工作):属**发布阻断项** —— 意味着 MinIO 抖动即永久丢录像,须修复后重测。
