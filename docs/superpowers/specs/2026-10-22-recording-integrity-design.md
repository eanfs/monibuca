# 录制完整性与异常恢复设计

日期：2026-10-22
状态：待评审
关联：`2026-08-27-storage-recovery-design.md`（存储启动竞态与历史记录解析，本次不重复处理）

## 背景

2026-10-15 至 10-18，现场 monibuca（v5.3.0.2606031605，容器 `xde-monibuca`）连续发生三次录制异常终止：

| 时间 | 事件 | 后果 |
|---|---|---|
| 10-15 09:45 → 10:07:32 | 48 路录制进行中，容器被手动 stop | 48 个 `/tmp/s3writer_*.tmp`（13.3 GB），DB 中 48 条记录 `end_time` 为空、`duration=0` |
| 10-17 08:32 / 08:57 → 09:12:34 | 72 + 144 路录制，进程被内核 OOM kill（RSS 13.98 GB / `mem_limit: 14g`） | 216 个 tmp（72.8 GB），216 条记录无结束 |
| 10-18 17:05:56 / 17:06:00 | GB28181 Catalog 处理触发 Go runtime fatal（`Collection.Get` 并发 map） | 进程崩溃，录制链路整体中断 |

三次事件的共同点：**progressive MP4 的 moov 与 sample 索引只存在于内存，进程没有机会执行 Stop 就消失**。磁盘上只剩裸 mdat，播放器报 `moov atom not found`，85 GB 数据无法直接使用；DB 记录停在“开始”状态；上传链路没有留下任何失败日志；tmp 文件名是随机数（`s3writer_94740.tmp`），无法与 `record_streams` 对应，只能靠人工抢救（现场 `recovered_mp4/` 39 个文件）。

结论：不是单点 bug，而是“**进程随时可能死**”这个前提在录制链路上没有被设计进去。

## 目标

1. 进程被 SIGKILL / OOM / 崩溃后，重启时自动把残留半成品恢复成**可播放的普通 MP4**（progressive MP4，不是 fMP4），并补传、补数据库记录。
2. 同一路流重新开始录制时，能**接上**之前的半成品继续写，最终形成一个可播文件；前后数据都在，中间断档可接受。
3. 上传失败、录制起止、恢复过程都留下可查询的日志与状态。
4. 降低“进程死”的发生概率：GB28181 并发 map 崩溃修复、失败后不要用重启风暴压垮上游（NVR）与自身内存。
5. 交付方式：直接使用最新版本（main 分支）构建的镜像，不为 v5.3.0 打补丁。

## 非目标

- 不做 fMP4 录制（用户明确要求 MP4）。fMP4 天然崩溃安全，但不在本次范围。
- 不承诺零丢失。异常杀进程是低频事件，接受“最多丢最后一小段”和“中间断档”。
- 不为“最后几秒”引入每帧 fsync、双写、预写日志等高频开销。
- 不自动处理已存在的 85 GB 无索引历史 tmp（提供离线工具，人工触发）。
- 不重做存储启动恢复/降级策略（已有 2026-08-27 设计）。
- 不改造 its-server 的录制调用协议。

## 约束与已定决策

用户已明确：

| 决策 | 内容 |
|---|---|
| 格式 | 必须是普通 MP4（progressive），不是 fMP4 |
| 丢失容忍 | 尽可能少丢；不强制零丢失；断档可接受 |
| 恢复形态 | 前后数据都在、能播即可；不要求时间戳连续 |
| 触发方式 | 重启后自动 + 提供接口手动触发 |
| 交付 | 直接换最新版本镜像，不 backport |

我的判断（可在评审时推翻）：

- 崩溃恢复的**权威来源是录制时落盘的索引**，而不是从裸 mdat 反向猜帧边界。mdat 内是裸帧数据（`plugin/mp4/pkg/muxer.go` `WriteSample`：直接写 sample 数据，长度只记录在内存 stsz 表），反向重建需要解析码流、精度差、无法支持续写。
- 索引采用**伴随文件（sidecar）**，而不是周期性 moov 快照。moov 快照每次都要重算偏移、体积大；sidecar 顺序追加，崩溃后重放即可精确重建。

## 现状盘点（main 分支）

| 能力 | 现状 | 本次动作 |
|---|---|---|
| GB28181 并发 map 崩溃 | `ensureCollectionMutex()` 已在 `registerhandler.go:216`、`device.go:452`、`device.go:1020` 补上 | 验证插件级 `plugin.channels` 是否同样受保护；补测试 |
| 上传队列/重试/pending | `UploadQueueTask` 与 trailer 队列已拆分；`pkg/storage/upload_manager.go` 有并发与水位；`retry.go` 有永久错误分类；`finalize.go` 有 `TempFileFinalizer` | 复用，不重写 |
| 正常停止时的 moov/trailer | `writeTrailerTask`（`plugin/mp4/pkg/record.go`） | 复用 |
| 崩溃后接管 | **无** | 新增 sidecar + 恢复任务 |
| 续录 | **无** | 新增 |
| 无 moov 文件修复 | **无**（只有正常路径 `PrepareFrontMoov`） | 新增（基于 sidecar 重建） |
| 录制起止日志 | 无 | 新增 |
| 上传失败日志 | 零星，未形成状态 | 补齐 + 可查询 |

## 设计

### 1. 伴随索引文件（sidecar）

**位置与命名**：与录制媒体文件同目录，`<media>.m7sidx`。S3 场景媒体是 `tempDir` 下的 `s3writer_*.tmp`，sidecar 与其同目录；local 场景媒体直接落在目标目录。

**格式**：二进制 append-only。

```
[Header]  magic "M7SIDX1" (7B) | headerLen uint32 | headerJSON (UTF-8)
[Record]  type uint8 | flags uint8 | trackID uint16 | size uint32 |
          offset uint64 | dts uint32 | duration uint32 | cts uint32 |
          crc32 uint32        // 共 32 字节，固定长度
[Record]  ...
[Commit]  type=0xFF | 结束时间 | 总时长 | 样本总数 | crc32
```

headerJSON 至少包含：

```json
{
  "version": 1,
  "streamPath": "19216817/1921682163-...-main",
  "mediaPath": "/tmp/s3writer_94740.tmp",
  "targetName": "rec-1921682163-1792310482792.mp4",
  "storageType": "s3",
  "startTime": "2026-10-17T08:32:12.5+08:00",
  "resumeOf": "",
  "tracks": [
    {"id": 1, "fourcc": "avc1", "timescale": 1000, "stsd": "<base64>"}
  ]
}
```

`tracks[].stsd` 存 codec 描述（avcC/hvcC 等），恢复时无需等新码流即可构造 moov。

**写入时机**：每次 `Track.AddSampleEntry` 之后追加一条 Record（媒体数据先写、索引后写，保证索引不超前于数据）。用 4 KB `bufio.Writer`，每 200 ms 或缓冲满时 flush。正常停止时写 Commit、fsync，然后删除 sidecar。

**一致性规则**：恢复时以 sidecar 为准。逐条读记录并校验 crc；遇到第一条损坏/不完整记录即停止。媒体文件截断到 `最后一条有效记录.offset + size`，保证 moov 与 mdat 一致。

**开销**：每条 32 B。按 25 fps 单轨计，1 小时约 2.9 MB；实际流多为 2 轨，仍可忽略。

### 2. 崩溃后自动恢复

新增 `RecordIndexRecoveryTask`（`task.TickTask` 或启动时一次性任务，挂在 MP4 插件），在存储初始化完成之后运行：

1. 扫描目录：录制 `tempDir`、`pending_uploads`、以及配置的额外目录（默认 `/tmp`），查找 `*.m7sidx`。
2. 对每个 sidecar：读 header → 校验媒体文件存在 → 重放 record 重建 `Muxer` 的 sample 表 → 截断媒体尾部 → `PrepareFrontMoov()` 生成 moov-first MP4。
3. 更新 `record_streams`：`end_time`、`duration`（取最后一条 dts+duration）、`file_path`。若记录不存在则新建。
4. 投递到既有 `UploadQueueTask` 上传；成功清理媒体与 sidecar；失败进入 `pending_uploads` 并登记重试（复用现有 `upload_manager`/DB `upload_tasks`）。
5. 幂等：恢复开始即把 sidecar 重命名为 `.m7sidx.recovering`，处理完写 `.m7sidx.done` 或删除；重复启动不会二次处理同一文件。
6. 并发与限速：默认并发 2 个恢复任务、可配置；避免启动时与录制抢 IO。

**失败处理**：任一文件恢复失败不阻塞其余文件；失败列表写入日志并在 API 中可查。

### 3. 续录（接上）

录制开始时（`Recorder.createStream` 之前）做一次“未完成录制”匹配：

- 条件：`mp4.record.resume` 开启；同 `streamPath`；同 storage key；存在未 Commit 的 sidecar；媒体文件存在。
- 命中后：
  1. 重放 sidecar 恢复 `Muxer`（tracks、sample 表、`CurrentOffset`、mdat 占位状态）；
  2. 以 append 方式打开媒体文件，seek 到末尾；
  3. 新 sample 的 `Timestamp` 从“上次最后 dts + wallclock 断档时长”继续累加（断档由播放器表现为跳变/黑屏，可接受）；
  4. 继续写媒体并追加 sidecar；
  5. 停止时统一回填 mdat size（`reWriteMdatSize`）并生成 moov，走原有 trailer/上传链路。
- 未命中则按现状新建文件。
- 竞态：同一 `streamPath` 的续录加进程内互斥，抢不到锁就新建文件；跨进程（多副本）不在本次范围（现场单副本）。

**注意**：若 sidecar 或媒体文件已被恢复任务处理（`.recovering`/`.done`），续录不命中，按新文件处理，避免与恢复任务互相踩踏。

### 4. 历史无索引 tmp 的离线抢救

提供独立入口（CLI 子命令或 `POST /mp4/api/recovery/raw`），对没有 sidecar 的 `s3writer_*.tmp` 做 best-effort 恢复：

- 解析 mdat 内码流（H.264/H.265 NALU 扫描），重建最小可用索引；
- 生成可播 MP4；失败则输出诊断（无关键帧、码流无法识别等）；
- 批量处理支持目录参数、并发、dry-run；
- 结果写入报告文件，供现场逐批核对。

**明确不保证成功率**，也不进入启动自动流程。现场剩余 85 GB 用该工具处理，处理完再决定是否删除。

### 5. 可观测性

**日志（必须）**，统一用结构化 key-value：

| 事件 | 级别 | 关键字段 |
|---|---|---|
| 录制开始 | Info | `streamPath`、`file`、`type`、`storageType`、`codec`、`resume` |
| 录制结束 | Info | `streamPath`、`file`、`durationMs`、`size`、`samples`、`tracks`、`elapsedMs` |
| 索引写入异常 | Error | `streamPath`、`file`、`error`、`recordsWritten` |
| 上传入队/成功/失败 | Info/Error | `file`、`size`、`retryCount`、`errorClass`（permanent/transient）、`nextRetryAt` |
| pending 水位 | Warn | `count`、`bytes`、`limit` |
| 恢复扫描/单个结果 | Info/Error | `scanned`、`recovered`、`failed`、`file`、`elapsedMs` |
| 续录命中/放弃 | Info/Warn | `streamPath`、`file`、`gapMs`、`reason` |

**API（必须）**：

- `GET /mp4/api/recovery/list`：未完成索引、恢复中、恢复失败、最近恢复记录（分页）。
- `POST /mp4/api/recovery/run`：手动触发扫描/恢复，可指定目录或单文件，返回任务 id。
- `GET /mp4/api/recovery/status/{id}`：进度与结果。
- 复用/扩展现有 `upload_admin.go` 的 pending 查询接口，返回待传数量、字节、失败原因分布。

**指标（可选）**：录制中数量、sidecar 写入失败计数、pending 文件数/字节、恢复成功/失败计数。若加，走现有 metrics 通道。

### 6. 根因与稳定性

**GB28181 并发 map**：`addOrUpdateChannel` 里访问的是 `d.plugin.channels`（插件级集合），而 `ensureCollectionMutex` 只初始化了 `d.channels`/`d.catalogReqs`。需要确认插件级集合的锁初始化路径（`MP4/GB28181 插件 Start` 时创建），并用 `-race` + 并发 Catalog 压测验证。若缺失，按同样方式补齐。

**OOM**：现场 `mem_limit: 14g`，216 路并发录制时 RSS 13.98 GB 被杀。两条腿：

1. 部署侧（必须）：建议限额提升到 24–32 GB；`pull_on_start` 任务分批拉起（避免重启后 3611 条同时启动）。
2. 代码侧（可配置软保护）：给录制链路加“最大并发录制数”和“内存预算”，超限时拒绝新录制并记 Error 日志（含 streamPath、当前并发、估算内存），而不是让内核杀进程。默认值保守，允许关闭。

**录制任务拉起**：结合部署侧，限制同一时刻新开录制数（令牌/队列），避免 NVR 503/EOF 与本地内存同时被打爆。

### 7. 配置项（新增，均有默认值）

```yaml
mp4:
  record:
    index:
      enable: true            # 写伴随索引（崩溃恢复的前提）
      flushinterval: 200ms    # 索引落盘节流
      dirs: []                # 恢复扫描目录，空则用 tempDir + pending_uploads + /tmp
    resume:
      enable: true            # 同流续录
    recovery:
      onstart: true           # 启动自动恢复
      concurrency: 2
      maxgap: 0s              # 断档超过该值不续录（0=不限）
  limits:
    maxconcurrentrecordings: 0  # 0=不限
    maxmemorymb: 0              # 0=不限
```

### 8. 与既有设计的关系

- 存储启动/降级/readiness：沿用 `2026-08-27-storage-recovery-design.md`，本次不改；恢复任务在其之后运行，确保上传目标已确定。
- `upload_manager` 的 pending 水位：本次只复用与补日志，不改变语义。
- FLV/HLS 录制的同类问题：本次不覆盖，仅 MP4。

## 验收标准

1. **崩溃恢复**：模拟 216 路录制（可用压测脚本降规模到 20 路 × 循环）进行中 `kill -9` 进程 → 重启 → 全部带 idx 的残留文件自动恢复为 `ffprobe` 可读的 MP4，`record_streams` 的 `end_time`/`duration` 正确，上传成功或进入 pending。
2. **续录**：录制中途 kill → 重启 → 同 `streamPath` 继续录制 → 停止后得到一个可播文件，`ffprobe` 显示前后两段都有数据、中间有断档。
3. **无索引历史文件**：用离线工具处理现场 85 GB 样例，输出成功率报告；失败文件给出明确原因。
4. **可观测性**：录制开始/结束、上传失败、pending 水位、恢复结果均有日志；三个恢复 API 可返回真实状态。
5. **稳定性**：GB28181 并发 Catalog 压测 `-race` 无 fatal；录制并发超限时拒绝并记日志，不触发 OOM。
6. **回归**：`go test ./plugin/mp4/... ./pkg/storage/... -race` 通过；正常启停、正常上传路径行为不变。
7. **线上验证**：在 201 用最新镜像 + 修正后的配置（旧 IP、限额）跑一轮真实录制，确认无 `create storage failed`、无 fatal、日志齐全。

## 风险与取舍

| 风险 | 说明 | 取舍 |
|---|---|---|
| sidecar 额外 IO | 每帧 32 B，buffered | 可忽略；崩溃最多丢最后 200 ms 的索引（对应几帧数据） |
| 索引与数据不一致 | 崩溃时媒体多写、索引未落 | 以索引为准截断媒体尾部，宁可少几帧 |
| 续录时间戳断档 | 播放器会看到跳变 | 用户已接受；不做复杂 gap 修复 |
| sidecar 丢失/损坏 | 磁盘错误、人为删除 | 退化为离线抢救工具，尽力而为 |
| 恢复占用启动 IO | 216 个文件同时恢复 | 默认并发 2、可配置；后续可加时间窗 |
| OOM 仍可能发生 | 代码软保护 + 部署限额 | 双管齐下，不承诺绝对不发生 |

## 留给实现阶段的决定

1. sidecar 放 `tempDir` 还是独立的 `sidecar/` 子目录（影响清理脚本与扫描逻辑）。
2. 恢复任务的挂载点：MP4 插件 Start 后一次性任务，还是独立 `task.Work`（建议后者，便于限流与观测）。
3. 续录的时间戳补偿实现：直接用 wallclock 差值，还是写入 edit list 标记 gap（建议前者，简单）。
4. 离线抢救工具的解析实现深度：只支持 H.264/H.265 Annex-B，还是也覆盖 AVCC。
5. OOM 软保护的具体阈值模型：按“流数 × 分辨率 × 码率”估算，还是按进程 `runtime.ReadMemStats` 动态判断（建议先做固定并发上限，简单可预测）。
