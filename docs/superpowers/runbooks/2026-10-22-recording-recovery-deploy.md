# 录制崩溃恢复上线手册（2026-10-22）

适用对象：现场 `172.16.12.201`，容器 `xde-monibuca`，目录
`/home/project/xde-xd/media-docker-compose`。

目标：换上新镜像后，进程被 SIGKILL/OOM/崩溃时残留的 progressive MP4 能在重启后
**自动恢复成可播放文件并上传 S3**，同时完成日志保留、GB28181 并发崩溃修复与
录制并发软保护。

> 本文所有 `<...>` 均为占位符。不要把真实 AK/SK、JWT、密码写进本文件、命令历史
> 或工单。

---

## 0. 上线前必须知道的三个事实

1. **sidecar 与媒体同目录**：本次版本给每个 progressive MP4 写
   `<media>.m7sidx`（每帧 23 B，1 秒落盘一次）。S3 场景媒体是
   `storage.s3.tempdir` 下的 `s3writer_*.tmp`，因此该目录必须落在持久化 bind
   mount 上（现场是宿主 `./monibuca/tmp:/tmp`）。换镜像不会丢它。
2. **恢复只认 progressive MP4**。`fragment`（fMP4）不走恢复流程；现场必须保持
   `fragment: 0`，并且不要指定 `fileName` + 分片 > 0（会反复覆盖同一对象键）。
3. **S3 必须先可用**。恢复上传只会按 sidecar 里的 `storageType` 解析后端，
   **绝不降级写 local**；S3 不可用时恢复产物会停在 `pending_uploads` 等待补传，
   达不到“自动上传”的验收目标。因此第 2 节的 endpoint 修正是硬性前置条件。

---

## 1. 预检与备份（约 20 分钟）

### 1.1 记录现状

逐条执行并留档（单行）：

```bash
docker inspect -f '{{.Config.Image}} {{.Image}} {{.HostConfig.Memory}} {{.HostConfig.NetworkMode}}' xde-monibuca
```

```bash
docker inspect -f '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}' xde-monibuca
```

```bash
docker inspect -f '{{index .Config.Labels "com.docker.compose.project.config_files"}}' xde-monibuca
```

```bash
df -h /home/project; free -g; nproc
```

确认三件事：

- `./monibuca/tmp` → `/tmp`（或等价目录）确实是 **bind mount**，不是容器临时层；
  `pending_uploads` 与 `m7s.db` 同理。
- 宿主 `tmp/` 里存在现场历史残留：
  ```bash
  ls -1 /home/project/xde-xd/media-docker-compose/monibuca/tmp/s3writer_*.tmp 2>/dev/null | wc -l
  ```
  ```bash
  du -sh /home/project/xde-xd/media-docker-compose/monibuca/tmp
  ```
  2026-10-22 基线约 216 个文件 / 85 GB（含无索引历史 tmp）。
- 宿主磁盘剩余 ≥ 待恢复数据量的 2 倍（恢复是“源 + 新产物”并存，原子 rename 后
  才删源）。

### 1.2 停机备份

建议在维护窗口 **先停容器再备份**，避免 SQLite 在线拷贝不一致：

```bash
docker stop xde-monibuca
```

```bash
BK=/home/project/xde-xd/media-docker-compose/backup-$(date +%Y%m%d-%H%M); mkdir -p "$BK"
```

```bash
cd /home/project/xde-xd/media-docker-compose && cp -a monibuca/config.yaml docker-compose-xde-monibuca.yml "$BK"/
```

```bash
cd /home/project/xde-xd/media-docker-compose/monibuca && cp -a m7s.db "$BK"/m7s.db && tar -czf "$BK/tmp.tar.gz" tmp && tar -czf "$BK/pending_uploads.tar.gz" pending_uploads
```

> 85 GB 的 `tmp` 打包会很久。若磁盘紧张，可改为
> `rsync -a --copy-links tmp/ "$BK/tmp/"`，或至少记录
> `find tmp -type f -printf '%p %s\n' > "$BK/tmp-manifest.txt"`，并单独保留
> `m7s.db` 与 `config.yaml`。**`m7s.db` 必须备份**：恢复成功后会补写
> `record_streams.end_time/duration/storage_type`。

留下旧镜像引用，回滚时要用：

```bash
OLD_IMAGE=$(docker inspect -f '{{.Config.Image}}' xde-monibuca); OLD_IMAGE_ID=$(docker inspect -f '{{.Image}}' xde-monibuca); echo "$OLD_IMAGE $OLD_IMAGE_ID" | tee "$BK/rollback-image.txt"
```

---

## 2. 强制配置修正（换镜像前完成）

现场配置仍指向旧 IP（`192.168.1.5`）。**不修正就换镜像 = 恢复全部进 pending。**

编辑宿主上的 `monibuca/config.yaml`（容器内通常是 `/etc/monibuca/config.yaml`，
以第 1.1 节 mount 为准）：

```yaml
global:
  storage:
    s3:
      endpoint: "127.0.0.1:9000"   # 本机 MinIO；或 "storage.xiding.tech"
      useSSL: false                # storage.xiding.tech 时改为 true
      forcePathStyle: true         # MinIO 必须
      bucket: "<BUCKET>"
      accessKeyID: "<S3_ACCESS_KEY>"
      secretAccessKey: "<S3_SECRET_KEY>"
      pathPrefix: "<PREFIX>"
      tempdir: "/tmp"              # 必须指向持久化 bind mount
      timeout: "600s"

gb28181:
  mediaip: "172.16.12.201"
  sipip: "172.16.12.201"
```

同时确认录制仍为单文件模式（本版本恢复只支持 progressive MP4）：

```yaml
mp4:
  record:
    index:
      enable: true          # 崩溃恢复的前提，禁止关闭
      flushinterval: 1s
    recovery:
      onstart: true         # 启动自动扫描
      concurrency: 2        # 启动恢复并发；先用 2，确认 IO 后再调
      dirs: []              # 空 = tempdir + pending_uploads + 录像输出目录
  limits:
    maxconcurrentrecordings: 160   # 软保护，建议 burn-in 期 160–180；0=不限
    maxmemorymb: 0                 # 本版本仅解析记录，不执行
```

如果现场用的不是默认录像目录（`onpub.record[].filepath` 指向别处），必须把该
目录加进 `recovery.dirs`，否则启动扫描找不到 sidecar。

### 2.1 验证 MinIO 可达 + bucket 可写

在宿主执行（`mc` 未安装时先装；AK/SK 用环境变量或 `mc alias set` 交互输入，
**不要写进命令行参数**）：

```bash
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9000/minio/health/live
```

期望 `200`。

```bash
mc alias set <ALIAS> http://127.0.0.1:9000 <S3_ACCESS_KEY> <S3_SECRET_KEY>
```

```bash
mc ls <ALIAS>/<BUCKET>/ >/dev/null && echo "bucket readable"
```

```bash
echo preflight-$(date +%s) | mc pipe <ALIAS>/<BUCKET>/<PREFIX>/_preflight/check.txt && mc stat <ALIAS>/<BUCKET>/<PREFIX>/_preflight/check.txt && mc rm <ALIAS>/<BUCKET>/<PREFIX>/_preflight/check.txt
```

三条都成功才继续。若使用 `storage.xiding.tech`，把地址与 `useSSL` 配成对应值后
重复以上检查。

---

## 3. 容量与启动洪峰

### 3.1 提高内存限额

现场 OOM 基线：`mem_limit: 14g`，216 路并发录制时 RSS 13.98 GB 被杀。目标
**24–32 GB**，同时给宿主保留余量（`free -g` 至少剩余 8 GB）：

```yaml
services:
  monibuca:
    mem_limit: 28g
    memswap_limit: 28g     # 与 mem_limit 相等，禁止用 swap 掩盖
    restart: always
```

修改 `docker-compose-xde-monibuca.yml` 后先 `docker compose config --quiet` 校验。

### 3.2 `pull_on_start` 分批（现场 3611 条）

一次性拉起 3611 路会同时触发拉流、录制与 GB28181 信令，是本次 OOM 的放大器。
推荐流程：**先全关，换镜像，验证基础功能，再按 300 条/批用 API 打开**。

1）停机状态下先把数据库里的自动拉流全部关掉（务必在 1.2 备份之后）：

```bash
sqlite3 /home/project/xde-xd/media-docker-compose/monibuca/m7s.db "UPDATE pull_proxy_configs SET pull_on_start = 0; SELECT COUNT(*) FROM pull_proxy_configs WHERE pull_on_start = 1;"
```

2）换镜像并启动（第 4 节）。此时不会盲目拉起全部设备；拉流代理仍会创建并用
`checkinterval` 探活，状态变为 Online，但不会自动 Pull。

3）基础验证通过后，分批打开。**单行脚本**（`jq` 取当前仍关闭的 ID，每批 300，
每批间隔 30 秒；API 会立即对 Online 设备发起 Pull）：

```bash
curl -s http://127.0.0.1:8080/api/proxy/pull/list | jq -r '.data[] | select(.pullOnStart == false) | .ID' | head -300 | while read id; do curl -s -X POST -H 'Content-Type: application/json' -d "{\"ID\":$id,\"pullOnStart\":true}" http://127.0.0.1:8080/api/proxy/pull/update >/dev/null; done; sleep 30
```

每批之间观察：

```bash
docker stats --no-stream xde-monibuca; docker logs --since 2m xde-monibuca 2>&1 | grep -c "record started"
```

RSS 接近 `mem_limit` 的 80%、或 `record started` 增速异常时，停止下一批并排查。
若开启 `admin.enablelogin`，所有 API 调用都要加
`-H 'Authorization: Bearer <ADMIN_TOKEN>'`。

4）全部批次完成后确认：

```bash
sqlite3 /home/project/xde-xd/media-docker-compose/monibuca/m7s.db "SELECT COUNT(*) FROM pull_proxy_configs WHERE pull_on_start = 1;"
```

> 回滚时记得把 `pull_on_start` 恢复为 1（第 7 节），否则旧镜像也不会自动拉流。

---

## 4. 切换镜像

```bash
cd /home/project/xde-xd/media-docker-compose && cp -a docker-compose-xde-monibuca.yml docker-compose-xde-monibuca.yml.pre-$(date +%Y%m%d-%H%M)
```

```bash
cd /home/project/xde-xd/media-docker-compose && sed -i 's|intetech/monibuca:.*|intetech/monibuca:<NEW_TAG>|' docker-compose-xde-monibuca.yml && grep -n 'image:.*monibuca' docker-compose-xde-monibuca.yml
```

```bash
cd /home/project/xde-xd/media-docker-compose && docker compose -f docker-compose-xde-monibuca.yml pull && docker compose -f docker-compose-xde-monibuca.yml up -d --force-recreate
```

`<NEW_TAG>` 使用要发版的 tag（格式 `v5.X.Y.YYMMDDHHMM`），不要用 `latest`。

### 4.1 启动日志判定（前 10 分钟）

```bash
docker logs -f --since 10m xde-monibuca 2>&1 | grep -E "create storage failed|S3 unavailable|global storage created|recovery result|index recovery completed|record started|panic|fatal"
```

必须满足：

- **不得出现** `create storage failed` / `S3 unavailable, recording disabled`；
- 应出现 `global storage created type=s3`；
- 出现 `index recovery task started ... runOnStart=true dirs=[...]`；
- 历史残留逐个出现 `recovery result`（`outcome=recovered|skipped|failed`）；
- 结束后一条 `index recovery completed scanned=N recovered=... failed=...`；
- 新录制出现 `record started`，字段含 `streamPath/file/type/storageType/codec`。

存储状态接口必须是 200 且非降级：

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/api/storage/status
```

```bash
curl -s http://127.0.0.1:8080/api/storage/status | jq
```

期望 `desiredType=s3`、`activeType=s3`、`degraded=false`、`fallbackActive=false`。

---

## 5. 验证恢复链路

### 5.1 恢复清单 API

```bash
curl -s 'http://127.0.0.1:8080/mp4/api/recovery/list?page=1&size=50' | jq
```

重点字段：

- `counts.pending`：发现但未处理（正常应随扫描完成而下降）；
- `counts.recovering`：有 `.m7sidx.recovering`，若长期不降说明恢复卡住，看日志；
- `counts.failed`：确定性失败（`.m7sidx.failed`），逐个记录并反馈；
- `data.recent[]`：本进程内最近成功恢复（`file/samples/truncated/at`）。

分页上限 200；`truncated=true` 表示单次扫描超过 1 万条上限，需要分页/分目录查看。

需要手动补扫时（先确认目标在白名单内；默认白名单含 `/tmp`、
`pending_uploads`、录像输出目录）：

```bash
curl -s -X POST -H 'Content-Type: application/json' -d '{"dir":"/tmp"}' http://127.0.0.1:8080/mp4/api/recovery/run | jq
```

返回 `202` + `id`；不同目标同时运行返回 `409`，相同目标重复触发返回 `200` +
同一 `id`（幂等）。查询进度：

```bash
curl -s http://127.0.0.1:8080/mp4/api/recovery/status/<ID> | jq
```

### 5.2 对象抽查（`mc stat`）

先从数据库拿到本次恢复的对象键：

```bash
sqlite3 /home/project/xde-xd/media-docker-compose/monibuca/m7s.db "SELECT id,stream_path,file_path,storage_type,end_time,duration FROM record_streams ORDER BY id DESC LIMIT 20;"
```

再抽查 S3 对象（键 = `pathPrefix` + `file_path`，以后端实际规则为准）：

```bash
mc stat <ALIAS>/<BUCKET>/<PREFIX>/<OBJECT_KEY>
```

必须满足：

- 对象存在，大小与本地恢复产物/日志 `size` 一致（± 少量 trailer 差异不允许；
  应为同一文件）；
- `storage_type` 为 `s3`，`end_time` 非空、`duration > 0`；
- 对应宿主 `tmp` 下的 `s3writer_*.tmp` 与 `.m7sidx` 已被清理。

下载抽样校验（任一对象）：

```bash
mc cp <ALIAS>/<BUCKET>/<PREFIX>/<OBJECT_KEY> /tmp/verify.mp4 && ffprobe -v error -show_entries format=duration:stream=codec_name -of json /tmp/verify.mp4
```

期望 `duration > 0` 且有 h264/h265 轨道。

### 5.3 上传失败兜底

```bash
docker exec xde-monibuca sh -c 'ls -1 /monibuca/pending_uploads 2>/dev/null | wc -l'
```

```bash
curl -s http://127.0.0.1:8080/mp4/api/upload/exhausted | jq
```

`pending_uploads` 增长说明 S3 仍不可用或凭证错误：按日志 `upload failed` 的
`errorClass` / `nextRetryAt` 排查；**不要手工把 pending 文件拷进对象存储后删除
数据库行**，补传调度会在 S3 恢复后自动处理。只有水位告警
（`pending dir near full`）时才需要扩容或限流。

---

## 6. 历史 85 GB 无索引 tmp 抢救（CLI，离线）

这 216 个文件没有 sidecar，只能从裸 mdat 反向重建。**不进入启动自动流程**，
因为音频（AAC raw）通常无法分帧，产物可能只有视频轨；逐文件 best-effort。

### 6.1 准备工具

在与现场同架构的构建机上编译（纯 Go，无 CGO）：

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o m7s-recover ./example/recover
```

```bash
scp m7s-recover root@172.16.12.201:/home/project/xde-xd/media-docker-compose/monibuca/
```

### 6.2 先 dry-run

```bash
cd /home/project/xde-xd/media-docker-compose/monibuca && ./m7s-recover -dir ./tmp -dry-run -concurrency 2 > /tmp/recover-dryrun-$(date +%F).log 2>&1
```

```bash
grep -c '^\[ok\]' /tmp/recover-dryrun-*.log; grep '^\[fail\]' /tmp/recover-dryrun-*.log | head -20; tail -1 /tmp/recover-dryrun-*.log
```

审阅报告：`[fail]` 的确定性原因（无关键帧 / 码流无法识别 / 布局不符）应记录并
反馈；`warn:` 说明音频缺失或时间戳为估算值。**dry-run 不写任何文件。**

### 6.3 分批实跑

不要一次跑 85 GB。按 50 个文件一批，同文件系统 `mv` 是元数据操作，不复制数据：

```bash
cd /home/project/xde-xd/media-docker-compose/monibuca && mkdir -p tmp/salvage-batch-01 && find tmp -maxdepth 1 -name 's3writer_*.tmp' | head -50 | xargs -I{} mv {} tmp/salvage-batch-01/
```

```bash
cd /home/project/xde-xd/media-docker-compose/monibuca && ./m7s-recover -dir ./tmp/salvage-batch-01 -concurrency 2 > /tmp/recover-batch-01.log 2>&1; tail -3 /tmp/recover-batch-01.log
```

每批结束：

```bash
ffprobe -v error -show_entries format=duration:stream=codec_name -of json tmp/salvage-batch-01/*.recovered.mp4 | head -40
```

产物命名 `<源名>.recovered.mp4`，默认不覆盖已存在产物；重跑需 `-force`。磁盘峰值
约为该批源文件大小的 2 倍。

### 6.4 上传与清理

恢复成功的产物先上传并核对，**再**删除源 tmp：

```bash
cd /home/project/xde-xd/media-docker-compose/monibuca && find tmp/salvage-batch-01 -name '*.recovered.mp4' -exec mc cp {} <ALIAS>/<BUCKET>/<PREFIX>/recovered/ \;
```

```bash
mc ls <ALIAS>/<BUCKET>/<PREFIX>/recovered/ | head; mc stat <ALIAS>/<BUCKET>/<PREFIX>/recovered/<SAMPLE>.mp4
```

抽样下载 + `ffprobe` 通过后，删除该批源文件与已上传产物，保留日志与 dry-run 报告：

```bash
cd /home/project/xde-xd/media-docker-compose/monibuca && rm -f tmp/salvage-batch-01/s3writer_*.tmp && ls -1 tmp/salvage-batch-01 | wc -l
```

> 源 tmp 是唯一副本。**任何删除都必须在 `mc stat` + `ffprobe` 双重确认之后**；
> 建议先保留 7 天再清空 `salvage-batch-*` 目录。

---

## 7. 回滚

### 7.1 触发条件

- 启动出现 `create storage failed` 且 5 分钟内未恢复；
- 进程反复 OOM/crash，`fatal/latest.log` 有新的 Go fatal；
- 恢复出现大面积 `failed` 且无法在 30 分钟内定位；
- 新录制无法开始（`record started` 完全消失）。

### 7.2 回滚镜像

```bash
cd /home/project/xde-xd/media-docker-compose && sed -i "s|intetech/monibuca:.*|$(cat backup-*/rollback-image.txt | awk '{print $1}')|" docker-compose-xde-monibuca.yml && docker compose -f docker-compose-xde-monibuca.yml up -d --force-recreate
```

或直接把第 4 节生成的 `docker-compose-xde-monibuca.yml.pre-*` 还原后
`docker compose up -d --force-recreate`。

### 7.3 回滚配置

- **不要**把 `storage.s3.endpoint` 还原成旧 IP `192.168.1.5`：旧地址本身是不可达
  的历史配置，回滚镜像也需要正确的 MinIO endpoint 才能录制与上传；
- `gb28181.mediaip/sipip` 是环境修正，同样保留；
- 若新版本配置项导致旧镜像启动失败（未知字段一般被忽略，但先还原备份更稳）：
  ```bash
  cp backup-*/config.yaml monibuca/config.yaml
  ```
  还原后重新应用第 2 节的 endpoint / gb28181 修正。
- 若已执行第 3.2 节的 `pull_on_start=0`，回滚前恢复：
  ```bash
  sqlite3 /home/project/xde-xd/media-docker-compose/monibuca/m7s.db "UPDATE pull_proxy_configs SET pull_on_start = 1;"
  ```

### 7.4 数据安全边界

- `tmp/`、`pending_uploads/`、`m7s.db` 都是宿主 bind mount，换镜像/回滚镜像不会
  动它们；
- `.m7sidx` / `.m7sidx.recovering` / `.m7sidx.failed` 是新增旁路文件，旧镜像完全
  忽略；回滚后它们会继续留在磁盘（可人工清理，但**不要删除活跃录像的 sidecar**）；
- 恢复产物是标准 progressive MP4（`ftyp|moov|mdat`），旧版本可正常播放/下载；
- 数据库只有新增/既有表写入，无破坏性迁移；回滚前仍建议保留 1.2 的 `m7s.db` 备份；
- 已经上传到 S3 的恢复对象不受回滚影响。

---

## 8. 上线后检查（T+30 分钟 / T+2 小时 / 次日）

### 8.1 日志轮转

新默认值（无需显式配置即生效）：

```yaml
logrotate:
  path: "./logs"
  size: 20971520        # 20 MB
  days: 30              # 按 mtime 清理
  maxfiles: 750         # 磁盘护栏（≈15 GB 上限）
  fileprefix: "m7s-"
  formatter: "2006-01-02T15-04"   # 分钟级，避免同小时轮转互相覆盖
```

检查：

```bash
docker exec xde-monibuca sh -c 'ls -lh /monibuca/logs | head -20; du -sh /monibuca/logs'
```

确认：单文件不超过 ~20 MB + 一条日志；30 天前文件被清理；文件总数不超过 750。
若日志量异常（重试风暴），先降 `loglevel` 到 `info`，不要只靠轮转兜底。

### 8.2 录制结束字段

```bash
docker logs --since 2h xde-monibuca 2>&1 | grep "record stopped" | tail -5
```

每条必须含：`streamPath`、`file`、`durationMs`、`size`、`samples`、`tracks`、
`elapsedMs`。`samples=0` 或 `size` 明显偏小意味着录制链路异常，需结合
`index write failed` / `upload failed` 排查。

### 8.3 无 fatal

```bash
docker exec xde-monibuca sh -c 'ls -la /monibuca/fatal 2>/dev/null; for f in /monibuca/fatal/*.log /monibuca/fatal/latest.log; do [ -s "$f" ] && echo "NON-EMPTY: $f"; done'
```

任何非空文件都是进程级崩溃证据（Go fatal/OOM 前最后栈），必须带回分析。OOM 还需
`dmesg -T | grep -i "killed process"` 与 `docker inspect -f '{{.State.OOMKilled}}' xde-monibuca`。

### 8.4 存储与恢复

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/api/storage/status
```

```bash
curl -s 'http://127.0.0.1:8080/mp4/api/recovery/list?page=1&size=50' | jq '{counts, truncated}'
```

`counts.failed` 必须为 0 或有明确原因；`counts.recovering` 不应长时间不降；
`pending_uploads` 不应持续增长。

### 8.5 验收记录（现场填写）

| 项目 | 期望 | 实际 | 结论 |
|---|---|---|---|
| `global storage created type=s3` | 出现 | | |
| `create storage failed` | 0 次 | | |
| `/api/storage/status` | 200 / degraded=false | | |
| 启动恢复 `index recovery completed` | failed=0 或已解释 | | |
| 恢复对象 `mc stat` | 存在且可 `ffprobe` | | |
| `record started` / `record stopped` | 字段齐全 | | |
| 日志轮转 | 20 MB / 30 天 / 750 | | |
| `fatal/` | 无非空文件 | | |
| RSS（`docker stats`） | < mem_limit 80% | | |

---

## 附：本机集成验收（可重复执行）

仓库内 `test/recovery_integration_test.go` 覆盖同一承诺的确定性版本：

- 子进程启动真实 server + RTMP 推流 + `POST /mp4/api/start` 开录；
- 等 `.m7sidx` 有 ≥60 个样本且含关键帧后 `kill -9`；
- 等待超过 `recovery.minage`（下限 5s）后重启，断言 sidecar 被清理、
  `ffprobe` 能读到 `duration>0` 与音视频轨、日志出现
  `recovery result ... outcome=recovered`。

执行：

```bash
go test ./test -run TestRecoveryAfterKill -count=1 -v
```

`-short` 下跳过；缺 `ffmpeg`/`ffprobe` 时显式 skip。S3 分支不在此测试内（本机无
MinIO），已由 `plugin/mp4/pkg/recovery_upload_test.go` 覆盖。若测试环境被跳过，
请按第 5 节在真实服务器上完成同等验证：制造一次录制中 `kill -9` → 重启 →
`/mp4/api/recovery/list` + `mc stat` + `ffprobe`。
