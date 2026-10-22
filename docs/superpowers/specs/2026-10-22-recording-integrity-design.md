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

1. 进程被 SIGKILL / OOM / 崩溃后，**重启时自动扫描所有残留 tmp**，恢复成**可播放的普通 MP4**（progressive MP4，不是 fMP4），**自动上传到 S3**，并补数据库记录。范围包括：本次升级前遗留的无索引历史 tmp（现场约 85 GB / 216 个文件）。
2. 上传失败、录制起止、恢复过程都留下可查询的日志与状态。
3. 降低“进程死”的发生概率：GB28181 并发 map 崩溃修复、失败后不要用重启风暴压垮上游（NVR）与自身内存。
4. 交付方式：直接使用最新版本（main 分支）构建的镜像，不为 v5.3.0 打补丁。

## 非目标

- **不做自动续录（接上）**。2026-10-22 已确认：上游 its-server 每次发起录制都用新文件名、且不提供任务 ID，而同一 `streamPath` 允许并发多个录制；在此前提下“哪个残留该被哪个新任务接着写”没有可靠答案，猜错会把不同任务的录像混进同一文件。崩溃恢复的目标改为“每段残留各自恢复成独立可播文件”。
- 不改变“同一 streamPath 可并发多录制（不同 fileName/filePath）”的现有能力。
- 不做 fMP4 录制（用户明确要求 MP4）。
- 不承诺零丢失。异常杀进程是低频事件，接受“最多丢最后一小段”。
- 不为“最后几秒”引入每帧 fsync、双写、预写日志等高频开销。
- 不保证无索引历史 tmp 的恢复质量：这类文件没有 sidecar，只能从裸 mdat 反向重建，**音频轨通常无法恢复**，产物可能只有视频轨。
- 不重做存储启动恢复/降级策略（已有 2026-08-27 设计）。
- 不改造 its-server 的录制调用协议。

## 约束与已定决策

用户已明确：

| 决策 | 内容 |
|---|---|
| 格式 | 必须是普通 MP4（progressive），不是 fMP4 |
| 丢失容忍 | 尽可能少丢；不强制零丢失 |
| 恢复形态 | 前后数据都在、能播即可；不要求时间戳连续 |
| 续录 | **不做**（见非目标；保留同 streamPath 多录制能力） |
| 触发方式 | 重启后自动 + 提供接口手动触发 |
| 交付 | 直接换最新版本镜像，不 backport |

我的判断（可在评审时推翻）：

- 崩溃恢复的**权威来源是录制时落盘的索引**，而不是从裸 mdat 反向猜帧边界。mdat 内是裸帧数据（`plugin/mp4/pkg/muxer.go` `WriteSample`：直接写 sample 数据，长度只记录在内存 stsz 表），反向重建必须解析码流；H.264/H.265 尚可，**AAC raw 帧没有 ADTS 头，基本无法分帧**，音频轨大概率救不回。
- 因此索引采用**伴随文件（sidecar）**，而不是把索引写进文件头预留区。预留区方案每 N 秒要重写整份 moov（216 路会放大成百 MB/s），且长录制 moov 增长会撑爆预留区。
- sidecar 可以精简：只保留 init（ftyp+stsd）与每帧的 `offset/size/dts/keyframe`，不带 CRC，1 秒 flush 一次；25 fps 单轨约 50 KB/分钟。

## 现状盘点（main 分支）

| 能力 | 现状 | 本次动作 |
|---|---|---|
| 同 streamPath 多录制 | 已支持：`StartRecord` 按 `(streamPath, filePath, fileName)` 去重（`plugin/mp4/api.go:713-727`） | 保持不变 |
| GB28181 并发 map 崩溃 | `ensureCollectionMutex()` 已在 `registerhandler.go:216`、`device.go:452`、`device.go:1020` 补上 | 验证插件级 `plugin.channels` 是否同样受保护；补测试 |
| 上传队列/重试/pending | `UploadQueueTask` 与 trailer 队列已拆分；`pkg/storage/upload_manager.go` 有并发与水位；`retry.go` 有永久错误分类；`finalize.go` 有 `TempFileFinalizer` | 复用，不重写 |
| 正常停止时的 moov/trailer | `writeTrailerTask`（`plugin/mp4/pkg/record.go`） | 复用 |
| 崩溃后接管 | **无** | 新增 sidecar + 恢复任务 |
| 无 moov 文件修复 | **无**（只有正常路径 `PrepareFrontMoov`） | 新增（基于 sidecar 重建） |
| 录制起止日志 | 无 | 新增 |
| 上传失败日志 | 零星，未形成状态 | 补齐 + 可查询 |

## 设计

### 1. 伴随索引文件（sidecar）

**位置与命名**：与录制媒体文件同目录，`<media>.m7sidx`。S3 场景媒体是 `tempDir` 下的 `s3writer_*.tmp`，sidecar 与其同目录；local 场景媒体直接落在目标目录。

**格式**：二进制 append-only，精简版。

```
[Header]  magic "M7SIDX1" (7B) | headerLen uint32 | headerJSON (UTF-8)
[Record]  flags uint8 | trackID uint16 | size uint32 | offset uint64 |
          dts uint32 | duration uint32        // 固定 23 字节
[Record]  ...
[Commit]  type=0xFF | endTime | 总时长 | 样本总数
```

headerJSON 至少包含：

```json
{
  "version": 1,
  "streamPath": "19216817/1921682163-...-main",
  "mediaPath": "/tmp/s3writer_94740.tmp",
  "targetName": "rec-1921682163-1792310482792.mp4",
  "storageKey": "<含 prefix 的完整对象键>",
  "filePath": "<record_streams.file_path>",
  "storageType": "s3",
  "startTime": "2026-10-17T08:32:12.5+08:00",
  "tracks": [
    {"id": 1, "fourcc": "avc1", "width": 1920, "height": 1080,
     "sampleRate": 0, "channels": 0, "stsd": "<base64>"}
  ]
}
```

**写入时机**：先写媒体数据、后追加索引记录，保证索引不超前于“已提交给存储层”的数据。用 4 KB `bufio.Writer`，每 1 秒或缓冲满时 flush。

**一致性规则**：恢复以 sidecar 为准，但同时以实际文件长度兜底——逐条读记录，遇到损坏记录或 `offset + size > fileSize` 立即停止，媒体截断到最后一条有效记录末尾。**不延长文件**。

**开销**：每帧 23 B。25 fps 单轨 1 小时约 2 MB，可忽略。

### 2. 崩溃后自动恢复

新增 `RecordIndexRecoveryTask`（独立 `task.Work`，便于限流与观测），在存储初始化完成之后自动运行。**目标：重启后把所有残留 tmp 变成可播 MP4 并上传 S3，不需要人工介入。**

1. 扫描目录：录制 `tempDir`、`pending_uploads`、以及配置的额外目录，查找 `*.m7sidx` 与 `s3writer_*.tmp`（无 sidecar 的也算）。
2. **A 档：有 sidecar 的文件（本次改造后的正常路径）**
   - 读 header → 校验媒体文件存在 → 重放 record 重建 `Muxer` 的 sample 表 → 按上节规则截断 → `PrepareFrontMoov()` 生成 moov-first MP4；
   - 音视频轨完整，时间戳来自索引。
3. **B 档：无 sidecar 的历史 tmp（现场 85 GB）**
   - 从裸 mdat 反向重建：扫描 H.264/H.265 NALU 边界，按关键帧/时序生成最小 stbl；
   - 产物可播，但**音频轨通常缺失**、时间戳为估算值；
   - 逐文件 best-effort，失败不阻塞其它文件，结果写报告。
4. **上传 S3（两档统一）**
   - 用 header 的 `storageType`/`storageKey`（B 档从 DB `record_streams` 或 `s3writer` 命名规则推断）解析目标后端，**不依赖当前 active storage**：`Server.GetStorageForType("s3")`；
   - S3 未就绪时**等待并重试**（复用存储 registry / readiness），**不降级为 local**、不把恢复文件留在本地；
   - 上传成功后才删除本地 tmp 与 sidecar；失败进入 `pending_uploads` 并登记重试（复用现有 `upload_manager` / DB `upload_tasks`）。
5. 更新 `record_streams`：`end_time`、`duration`、`file_path`、`storage_type=s3`；记录不存在则新建。
6. 幂等：处理前把 sidecar 重命名为 `.m7sidx.recovering`；完成后写 `.m7sidx.done` 或删除；下次启动遇到 `.recovering` 重试（恢复必须可重复执行）。
7. 原子性：恢复一律“写新文件 + 原子 rename”，不原地修改。
8. 并发与限速：默认并发 2、可配置；B 档解析是 CPU 密集型，默认更低并发（1）并限速；避免启动时与录制抢 IO/CPU。

**失败处理**：任一文件失败不阻塞其余文件；失败列表写日志并可在 API 查询。

**与既有 `RecordRecoveryTask` 的关系**：`plugin/mp4/index.go:89` 的现有任务负责“扫目录补 DB 记录”，两者合并为一个任务：先补 DB，再做索引定稿，避免两套并行。

### 3. 为什么不做续录

- 上游 its-server 每次发起录制都用新文件名（`rec-<时间戳>.mp4`），不提供任务 ID，且不做续录。
- monibuca 侧同一 `streamPath` 允许并发多个录制（`api.go:713-727` 的去重键是 `(streamPath, filePath, fileName)`），重启后也可能并发拉起多个任务。
- 因此“残留文件 ↔ 新任务”无法可靠配对；按 mtime 或文件名时间戳猜测，会把不同任务的录像混进一个文件，破坏“不丢数据”的初衷。
- 结论：每段残留单独恢复为可播文件；若将来需要真正的续录，前提是先给录制引入任务 ID（需要上游配合）。

**对现有能力的承诺**：本设计不改变“同 streamPath 多录制”的 API 语义与去重逻辑。

### 4. 历史无索引 tmp 的离线抢救

B 档已纳入启动自动流程（见 2.3）。同一套解析实现同时提供 CLI 子命令与 HTTP 入口，用于：单文件重试、dry-run、指定目录批量跑、以及自动流程判定失败后的排查：

- 解析 mdat 内码流（H.264/H.265 NALU 扫描），重建最小可用索引；
- 生成可播 MP4；失败则输出诊断（无关键帧、码流无法识别、音频不可分帧等）；
- 支持目录参数、并发、dry-run；结果写报告文件，供现场逐批核对。

**明确不保证成功率**（音频轨通常无法恢复），也不进入启动自动流程。现场剩余 85 GB 用该工具处理，处理完再决定是否删除。

### 5. 可观测性

**日志（必须）**，统一结构化 key-value：

| 事件 | 级别 | 关键字段 |
|---|---|---|
| 录制开始 | Info | `streamPath`、`file`、`type`、`storageType`、`codec` |
| 录制结束 | Info | `streamPath`、`file`、`durationMs`、`size`、`samples`、`tracks`、`elapsedMs` |
| 索引写入异常 | Error | `streamPath`、`file`、`error`、`recordsWritten` |
| 上传入队/成功/失败 | Info/Error | `file`、`size`、`retryCount`、`errorClass`（permanent/transient）、`nextRetryAt` |
| pending 水位 | Warn | `count`、`bytes`、`limit` |
| 恢复扫描/单个结果 | Info/Error | `scanned`、`recovered`、`failed`、`file`、`elapsedMs` |

**API（必须）**：

- `GET /mp4/api/recovery/list`：未完成索引、恢复中、恢复失败、最近恢复记录（分页）。
- `POST /mp4/api/recovery/run`：手动触发扫描/恢复，可指定目录或单文件，返回任务 id。
- `GET /mp4/api/recovery/status/{id}`：进度与结果。
- 复用/扩展 `upload_admin.go` 的 pending 查询接口。

**指标（可选）**：录制中数量、索引写入失败计数、pending 文件数/字节、恢复成功/失败计数。

### 6. 根因与稳定性

**GB28181 并发 map**：`addOrUpdateChannel` 访问的是 `d.plugin.channels`（插件级集合），而 `ensureCollectionMutex` 只初始化了 `d.channels`/`d.catalogReqs`。需要确认插件级集合的锁初始化路径，并用 `-race` + 并发 Catalog 压测验证；缺失则补齐。

**OOM**：现场 `mem_limit: 14g`，216 路并发录制时 RSS 13.98 GB 被杀。两条腿：

1. 部署侧（必须）：限额提升到 24–32 GB；`pull_on_start` 任务分批拉起（现场有 3611 条）。
2. 代码侧（可配置软保护）：最大并发录制数/内存预算，超限时**入有界等待队列**（默认容量 = 并发上限 × 4），排队超时（默认 30s）才拒绝，API 返回 503、onpub 记 Error 并保留待录参数；不静默停录，也不让内核杀进程。

### 7. 配置项（新增，均有默认值）

```yaml
mp4:
  record:
    index:
      enable: true            # 写伴随索引（崩溃恢复的前提）
      flushinterval: 1s
      dirs: []                # 恢复扫描目录，空则用 tempDir + pending_uploads
    recovery:
      onstart: true
      concurrency: 2
  limits:
    maxconcurrentrecordings: 0  # 0=不限
    maxmemorymb: 0              # 0=不限
storage:
  s3:
    tempdir: /tmp               # 本次补上真实实现（此前配置字段被忽略）
```

### 8. 与既有设计的关系

- 存储启动/降级/readiness：沿用 `2026-08-27-storage-recovery-design.md`；恢复任务在其之后运行，确保上传目标已确定。
- `upload_manager` 的 pending 水位：复用与补日志，不改变语义。
- FLV/HLS 录制的同类问题：本次不覆盖，仅 MP4。

## 验收标准

1. **崩溃恢复 + 自动上传 S3**：录制进行中 `kill -9` 进程 → 重启 → 无人工介入，残留文件全部恢复为 `ffprobe` 可读的 MP4，并**成功上传到 S3 目标对象键**（用 `mc stat` 验证），本地 tmp 与 sidecar 被清理；`record_streams` 的 `end_time`/`duration`/`storage_type` 正确。S3 未就绪时文件留在 pending 并在就绪后自动补传。
2. **同 streamPath 多录制**：同一路流传不同 `fileName` 并发两个录制，两个文件互不干扰、各自恢复、各自上传；`StartRecord` 去重语义不变。
3. **无索引历史 tmp**：用现场 85 GB 中的真实样本验证 B 档反向重建：能产出可播 MP4（允许无音频），自动上传 S3，并输出成功率报告；失败文件给出明确原因。先用 1–2 个样本验证可行性，再全量执行。
4. **可观测性**：录制开始/结束、上传失败、pending 水位、恢复结果均有日志；三个恢复 API 返回真实状态。
5. **稳定性**：GB28181 并发 Catalog 压测 `-race` 无 fatal；录制并发超限时排队/拒绝并记日志，不触发 OOM。
6. **恢复自身的鲁棒性**：恢复过程中再次 `kill -9`，下次启动能重试并最终完成，不产生半修改文件。
7. **回归**：`go test ./plugin/mp4/... ./pkg/storage/... -race` 通过；正常启停、正常上传路径行为不变。
8. **线上验证**：在 201 用最新镜像 + 修正后的配置（旧 IP、限额）跑一轮真实录制，确认无 `create storage failed`、无 fatal、日志齐全。

## 风险与取舍

| 风险 | 说明 | 取舍 |
|---|---|---|
| sidecar 额外 IO | 每帧 23 B，1 秒 flush | 可忽略；崩溃最多丢最后 1 秒索引（对应少量尾部帧） |
| 索引与数据不一致 | 崩溃时媒体多写、索引未落 | 以文件实际长度为准截断，宁可少几帧 |
| 无法续录 | 现场模式下“接上”不可实现 | 用户已确认选 A；每段独立可播 |
| 旧 85 GB 只能部分恢复 | 音频轨通常救不回，可能只有视频轨 | 自动执行 + 报告，不承诺成功率 |
| S3 长时间不可用 | 恢复产物堆积在本地 | 进入 pending 并持续重试；告警 pending 水位 |
| 恢复占用启动 IO | 216 个文件同时恢复 | 默认并发 2、可配置 |
| OOM 仍可能发生 | 代码软保护 + 部署限额 | 双管齐下，不承诺绝对不发生 |
| 恢复期间再次崩溃 | 方案要求幂等 | 写新文件 + 原子 rename，可重跑 |

## 复核补充决定（builder check 后，2026-10-22）

1. **sidecar 删除时机：提交式，而不是正常停止即删。** 正常停止只写 Commit 并保留 sidecar；一直保留到“上传成功”或“已成功登记 pending”之后才删除。`FinalizeFromTemp` 改变媒体路径时追加 `RENAME` 记录（新 `mediaPath`），恢复时以最后一条为准。避免 trailer 重写/上传窗口内再被杀造成二次丢失。
2. **临时目录与换镜像。** 现场 compose 的 `./monibuca/tmp:/tmp` 是 bind mount，换镜像不丢数据；但 S3 目前写 `os.TempDir()`（`pkg/storage/s3.go:519`），示例配置里的 `tempDir` **代码中并不存在**。本次实现 `storage.s3.tempdir`，默认 `/tmp`，sidecar 与媒体同目录；部署说明写明换镜像前确认宿主 tmp 数据。
3. **索引可能超前于媒体（1 MiB 用户态缓冲）。** 恢复以实际文件长度为准，遇 `offset+size > fileSize` 停止并截断；**不**每 1 秒强制 flush 媒体缓冲（216 路代价不可接受），代价是丢弃尾部少量帧。
4. **header 字段。** 同时保存 `targetName`（basename）、`storageKey`（含 prefix 的完整对象键，上传用）、`filePath`（DB 匹配用）。恢复上传用 header 的 `storageType`；若当前配置已不包含该类型，转 pending 并告警，不静默改类型。
5. **恢复原子性。** 写新文件 + 原子 rename；`.recovering` 下次启动重试（幂等）；磁盘峰值需单文件 2 倍空间。
6. **API 安全边界。** 扫描目录必须在白名单（`recovery.dirs`）；HTTP 接口要求管理员权限；手动触发跳过 mtime 小于 `recovery.minage`（默认 5m）的文件与活跃录制；现场优先 CLI。
7. **超限语义：排队而不是直接丢。** 有界等待队列，排队超时才拒绝；不静默停录。
8. **恢复任务与现有 `RecordRecoveryTask` 合并**，先补 DB 再索引定稿。

## 留给实现阶段的决定

1. sidecar 放 `tempDir` 还是独立的 `sidecar/` 子目录（影响清理脚本与扫描逻辑）。
2. 恢复任务的具体挂载点与限流实现（建议独立 `task.Work`）。
3. 离线抢救工具的解析深度：只支持 H.264/H.265 Annex-B，还是也覆盖 AVCC；音频如何处理。
4. OOM 软保护的阈值模型：固定并发上限（建议，简单可预测）还是按 `runtime.ReadMemStats` 动态判断。
