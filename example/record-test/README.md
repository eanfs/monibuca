# Monibuca 录制测试环境

单节点 RTSP 拉流 + MP4 录制测试。已配置 35 路摄像头（海康 / 大华混合），支持本地录制 + S3/MinIO 上传。

## 目录结构

```
record-test/
├── main.go              # 主程序入口
├── config.yaml          # 服务配置（HTTP/RTSP/MP4/HLS/WebRTC + S3）
├── start.sh             # 启动 monibuca 服务
├── streams_meta.json    # 35 路摄像头探测基线（W/H/fps/codec/分档）
│
├── probe_streams.sh     # 探测所有摄像头分辨率 → streams_meta.json
├── test_minimal.sh      # 录制冒烟（全 35 路，60s 默认，无诊断）
├── test_cameras.sh      # 录制完整测（含网络 ping / RTSP 检查 / 并发）
├── test_high_res.sh     # 按分辨率分组录制 + 性能采样 + 自动出报告
│
├── lib/                 # 纯函数模块（classify/probe/sampler/verify）
├── tests/               # lib/ 单测
├── scripts/             # 辅助脚本（check / debug / cleanup）
├── docs/                # 历史 issue 与扩展说明
├── reports/             # test_high_res.sh 产物：report-*.md + metrics-*.csv
├── record/              # 本地录制输出（被 .gitignore）
└── logs/                # m7s 日志（被 .gitignore）
```

## 快速开始

### 1. 启动服务

```bash
./start.sh
```

- HTTP API：<http://localhost:8080>
- RTSP：rtsp://localhost:554
- 录制目录：`record/live/`（实际可能落在 `./live/` 见 [关于录制文件路径](#关于录制文件路径)）
- 日志：`logs/m7s.log`

### 2. 选一个测试脚本跑

| 脚本 | 场景 | 默认时长 |
|---|---|---|
| **`test_minimal.sh`** | 日常冒烟：全 35 路开录，停录，校验文件 | 100s |
| **`test_cameras.sh`** | 故障诊断：含 ping、RTSP 检查、并发可调 | 60s |
| **`test_high_res.sh`** | 性能聚焦：按分辨率分组（2K/4K/all），出报告 | 300s |

```bash
# 冒烟
RECORD_DURATION=30 ./test_minimal.sh

# 诊断
CHECK_CONCURRENCY=10 RECORD_CONCURRENCY=18 ./test_cameras.sh

# 分组测试（先跑过 probe_streams.sh）
./test_high_res.sh --group=2k --duration=60
```

### 3. 看结果

```bash
# 录制文件
find . -name "*.mp4" -not -path './.git/*'

# 测试报告（仅 test_high_res.sh 产出）
ls -lh reports/
cat reports/report-*.md | tail -40
```

## 2K/4K 高分辨率测试

定向跑某个分辨率分组的并发录制，自动出报告。基于 `lib/` 下的纯函数模块（`classify`/`probe`/`sampler`/`verify`），每个模块都有 bash 单测。

### 1. 探测所有摄像头分辨率

```bash
./probe_streams.sh
./probe_streams.sh --timeout=30  # 自定义超时
```

产出 `streams_meta.json`（已提交，含每路 W/H/fps/codec/分档），URL 中密码已脱敏。分档规则按短边像素：

| 档位 | 短边像素 |
|---|---|
| SD | < 720 |
| HD | 720 - 1079 |
| FHD | 1080 - 1439 |
| 2K | 1440 - 1999 |
| 4K | ≥ 2000 |

```bash
# 看当前分布
jq '.streams | group_by(.resolution_class) | map({class: .[0].resolution_class, count: length})' streams_meta.json
```

### 2. 跑分组测试

```bash
./test_high_res.sh --group=4k                    # 默认 5 分钟
./test_high_res.sh --group=4k --duration=600
./test_high_res.sh --group=2k,4k
./test_high_res.sh --group=all --duration=120
```

每次跑出两个文件：

- `reports/report-YYYYMMDD-HHMMSS.md` — 逐流 PASS/WARN/FAIL + 性能峰值
- `reports/metrics-YYYYMMDD-HHMMSS.csv` — 1Hz 采样 `ts,cpu_pct,rss_mb,fd_count`

### 3. 跑 lib/ 单测

```bash
for t in tests/test_*.sh; do echo "=== $t ==="; bash "$t"; done
```

预期 19 个 ✓ 全绿（classify 9 + probe 3 + sampler 2 + verify 5）。

### 关于录制文件路径

当前 `config.yaml` 只配置 S3 storage 但未带 `-tags s3` 编译。运行时 S3 init 失败并 fallback 到 local，path 默认是 `.`（cwd）。所以录制文件实际落在：

```
./live/<camera>/<camera>.mp4    (即 example/record-test/live/...)
```

而非 `record/live/...`。`test_high_res.sh` 有环境变量 `STORAGE_ROOT` 可调（默认 `.`）。如改 config 加 `local: { path: record }` 则用 `STORAGE_ROOT=record` 跑。

## 辅助工具

### scripts/ 下

```bash
scripts/check_cameras.sh     # ping 所有摄像头 IP，看在线状态
scripts/check_streams.sh     # 通过 API 看活跃流列表
scripts/check_videos.sh      # ffprobe 检查录制 mp4 时长/大小
scripts/debug_single.sh      # 调试单路流：起 → 录 → 停 → 校验
scripts/cleanup_temp.sh      # 清掉超 24h 的 S3 临时文件 (record/**/s3writer_*.tmp)
```

### 手动 API 调用

```bash
# 开始录制
curl -X POST 'http://localhost:8080/mp4/api/start/live/camera1' \
  -H 'Content-Type: application/json' \
  -d '{"fragment":"0","filePath":"live/camera1","fileName":"camera1.mp4"}'

# 停止录制
curl -X POST 'http://localhost:8080/mp4/api/stop/live/camera1'

# 查询录制列表
curl 'http://localhost:8080/mp4/api/list'
```

注：早期文档里有 `StartRecord` / `StopRecord` 大驼峰接口，已废弃。

## 配置说明

`config.yaml` 关键字段：

- `global.http.listenaddr`：HTTP API 端口（默认 `:8080`）
- `global.db.dsn`：sqlite 文件路径
- `global.storage.s3`：S3/MinIO 配置（不带 `-tags s3` 编译会 fallback 到 local，path=cwd）
- `rtsp.tcp.listenaddr`：RTSP 监听（默认 `:554`）
- `rtsp.pull`：摄像头列表（`<streamPath>: <rtsp-url>`），共 35 路
- `mp4.publish.delayclosetimeout`：录制停止后等待时间

详见 `config.yaml` 内联注释。

## 录制完成校验点

1. ✓ 文件生成：录制目录下有 `.mp4` 文件
2. ✓ 文件大小 > 0 且符合录制时长
3. ✓ `ffprobe` 可解析、可播放
4. ✓ MOOV box 在文件开头（fast start）— `mp4` 插件已默认保证
5. ✓ `logs/m7s.log` 无 error 级别

`test_high_res.sh` 的校验逻辑（`lib/verify.sh`）已覆盖前 3 点 + 分辨率匹配 + 时长偏差。

## 故障排查

### 服务起不来

```bash
lsof -i :8080 -i :554   # 端口被占
tail -100 logs/m7s.log  # 看启动日志
```

### 录制文件缺失或为空

```bash
df -h                                   # 磁盘空间
grep -i error logs/m7s.log              # m7s 错误
scripts/check_streams.sh                # 看活跃流
ffprobe rtsp://localhost:554/live/camera1   # 探测单路
```

### RTSP 拉流 timeout

`docs/rtsp-issue.md` 有详细分析。常见原因：摄像头响应慢 / 海康大华参数差异 / 海康主码流可能用 hevc。

## 停止 / 清理

```bash
pkill -f 'go run.*main.go'    # 停服务

# 清录制
rm -rf record/* live/*

# 清日志
rm -rf logs/*

# 清 sqlite
rm -f record-test.db
```

## 更多文档

- `docs/api-fix.md`：MP4 录制 API 历史变更（StartRecord → start/{path}）
- `docs/rtsp-issue.md`：RTSP 探测 vs FLV 行为差异分析
- `docs/minio-test.md`：MinIO/S3 上传测试细节
