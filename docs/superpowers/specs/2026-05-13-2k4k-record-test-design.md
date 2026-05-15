# 2K/4K 拉流与视频录制测试 — 设计文档

- **日期**: 2026-05-13
- **状态**: Draft（等用户 review）
- **范围**: `example/record-test/` 增强，新增"探测 + 分组 + 报告"三件套
- **不改**: monibuca 内核、`plugin/mp4`、`plugin/rtsp`、新增独立 example

---

## 1. 目标

在并发拉取并录制 **真正的 2K (1440p±) / 4K (2160p±)** 高分辨率 RTSP 主码流时，验证 monibuca v5 的：

1. **流稳定性**：拉流成功率、录制启停 API 成功率、断流重连行为
2. **录制文件完整性**：文件存在、大小>0、`ffprobe` 可解析、moov box 位置、分辨率与源匹配
3. **性能开销**：服务进程 CPU / RSS / fd 数量随高分辨率流并发增长的曲线
4. **可重复性**：把"哪几路是 4K"这种环境状态固化成文件，重跑结果可 diff

**非目标**：

- 改 monibuca 任何源码
- 推流 / 转码 / cluster / WebRTC 拉流
- S3 上传性能基准测试（保持现有 config 不动；S3 开关默认沿用 record-test 的 config.yaml 设定）
- Web 报表 / 看板

---

## 2. 背景

`example/record-test/` 已存在并落地：

- 配置：35 路 RTSP 摄像头主码流（海康 `ch1/main/av_stream`、大华 `subtype=0`），sqlite + S3(MinIO `storage-dev.xiding.tech`) 在 `config.yaml` 中已开
- 入口：`main.go` 起 monibuca，`start.sh` 拉起服务（`go run -tags sqlite main.go`）
- 测试脚本：`test_minimal.sh`（全 35 路批量启录 / 停录 / `ffprobe` 时长校验）
- 录制 API：`POST /mp4/api/start/{streamPath}`、`POST /mp4/api/stop/{streamPath}`

**当前缺口**：35 路里到底哪几路是 2K、哪几路是 4K，散落在摄像头型号 / 部署文档里，脚本无法定向跑"只测 4K 这一档"。所有摄像头被一锅煮，无法判断分辨率档位对系统的影响。

---

## 3. 架构

```
example/record-test/
├── streams_meta.json          [新] 探测产物：每路 camera 的 W/H/fps/codec/bitrate/resolution_class
├── probe_streams.sh           [新] 入口：ffprobe 探测所有源 → 写 streams_meta.json
├── test_high_res.sh           [新] 入口：按 class 过滤跑录制+采样+校验+出报告
├── lib/
│   ├── probe.sh               [新] ffprobe 解析（W/H/fps/codec/bitrate）
│   ├── classify.sh            [新] 分辨率分档逻辑
│   ├── sampler.sh             [新] 后台采样进程 CPU/RSS/fd（1 Hz → metrics.csv）
│   └── verify.sh              [新] 单流校验（存在/大小/时长偏差/分辨率匹配）
├── reports/                   [新] 每次跑落 report-YYYYMMDD-HHMMSS.md + metrics-*.csv
└── (其他文件保持不变)
```

### 3.1 分辨率分档

依据**短边像素**分档（避免横竖屏摄像头混淆）：

| 档位 | 短边像素 | 备注 |
|---|---|---|
| `SD` | < 720 | 4CIF/D1 等 |
| `HD` | 720 ≤ x < 1080 | 720p |
| `FHD` | 1080 ≤ x < 1440 | 1080p |
| `2K` | 1440 ≤ x < 2000 | 含 2.5K / 1520p 等 |
| `4K` | x ≥ 2000 | 2160p / 4K UHD |
| `unknown` | ffprobe 失败 | 不参与高分组测试，但记入 meta 便于排查 |

### 3.2 streams_meta.json schema

```json
{
  "probed_at": "2026-05-13T11:20:00Z",
  "probe_timeout_sec": 15,
  "streams": [
    {
      "stream_path": "live/camera1",
      "source_url": "rtsp://admin:***@192.168.12.71/ch1/main/av_stream",
      "vendor": "hikvision",
      "width": 3840,
      "height": 2160,
      "fps": 25,
      "codec": "h264",
      "bitrate_kbps": 8192,
      "resolution_class": "4K",
      "probe_status": "ok",
      "probe_error": null
    }
  ]
}
```

口令密码在 source_url 里需脱敏写入（密码替换为 `***`），源 URL 仍可从 `config.yaml` 反查。

### 3.3 数据流（test_high_res.sh）

```
test_high_res.sh --group=4k --duration=300 [--no-s3]
  │
  ├─[1] 加载 streams_meta.json → 过滤 resolution_class ∈ {4K} → STREAMS[]
  ├─[2] 校验 monibuca 服务起着（GET /api/sysinfo）
  ├─[3] 启动 sampler.sh & （写 reports/metrics-*.csv，列：ts,cpu_pct,rss_mb,fd_count,goroutine_count）
  ├─[4] for s in STREAMS：POST /mp4/api/start/{s}，body 同 test_minimal.sh
  ├─[5] sleep $duration
  ├─[6] for s in STREAMS：POST /mp4/api/stop/{s}
  ├─[7] sleep 15s（等待 mp4 trailer 重写）
  ├─[8] kill sampler
  ├─[9] for s in STREAMS：lib/verify.sh → PASS/WARN/FAIL
  └─[10] 渲染 reports/report-*.md
```

### 3.4 verify.sh 判定

| 检查项 | 通过条件 | 失败级别 |
|---|---|---|
| 文件存在 | `record/live/{camera_name}.mp4` 存在 | FAIL |
| 文件大小 | > 0 字节 | FAIL |
| ffprobe 可解析 | `ffprobe` 退出码 0 且能拿到 duration | FAIL |
| 分辨率匹配源 | 录制文件 W/H == streams_meta.json 中的 W/H | FAIL |
| 时长偏差 | abs(录制时长 - 请求时长) / 请求时长 ≤ 5% | WARN |
| moov 位置 | `ffprobe -v trace` 中 moov 在 mdat 之前（fast start） | WARN |

FAIL 抓 `logs/m7s.log` 中该 streamPath 的最后 50 行附在报告底部。

### 3.5 report.md 模板

```markdown
# 录制测试报告

- 时间: 2026-05-13 11:30:00
- 分组: 4K
- 流数: 6
- 录制时长: 300s

## 汇总
- 通过: 5 / 6
- 警告: 1
- 失败: 0

## CPU / 内存
- 峰值 CPU: 142%
- 峰值 RSS: 1.2 GB
- (附 metrics-*.csv)

## 逐流结果

| 流 | 源分辨率 | 编码 | fps | 文件大小 | 实测时长 | 偏差 | 结果 |
|---|---|---|---|---|---|---|---|
| live/camera1 | 3840x2160 | h264 | 25 | 287MB | 298s | -0.7% | PASS |
| ...
```

---

## 4. 错误处理

| 场景 | 处理 |
|---|---|
| `probe_streams.sh` 中 ffprobe 失败 | 标记 `probe_status: failed`，写 `probe_error`，不阻塞，继续探测其它流 |
| `test_high_res.sh` 启动前服务未起 | 退出码 1，提示 `./start.sh` |
| `start/{stream}` 返回非 0 | 累计 fail 计数，在 report 中记 `FAIL: api_start`，继续其它流 |
| 录制文件缺失 | report 中 FAIL + 截取 `logs/m7s.log` 最后 50 行该 streamPath |
| sampler 异常退出 | report 中标记"性能数据不完整"，不影响测试结果 |
| streams_meta.json 缺失 | `test_high_res.sh` 提示先跑 `probe_streams.sh`，退出码 2 |

---

## 5. 命令行接口

```bash
# 探测全部摄像头分辨率，生成 streams_meta.json
./probe_streams.sh
./probe_streams.sh --timeout=30          # 默认 15s

# 跑 4K 组录制测试（5 分钟）
./test_high_res.sh --group=4k

# 自定义时长
./test_high_res.sh --group=4k --duration=600

# 跑 2K + 4K
./test_high_res.sh --group=2k,4k --duration=300

# 跑所有探测成功的流
./test_high_res.sh --group=all --duration=120
```

环境变量沿用现有：`NODE_IP`、`HTTP_PORT`、`RECORD_DURATION`（被 `--duration` 覆盖）。

---

## 6. 实现顺序（供 writing-plans 参考）

1. `lib/probe.sh`：单流 ffprobe 解析函数 + 单测（mock 一段 ffprobe 输出）
2. `lib/classify.sh`：短边分档纯函数 + 单测
3. `probe_streams.sh`：拼装 1+2，遍历 `config.yaml` 中的 `rtsp.pull`，输出 `streams_meta.json`
4. `lib/sampler.sh`：pgrep monibuca → `ps -p PID -o %cpu,rss` 循环写 csv
5. `lib/verify.sh`：单流校验函数 + 单测（mock ffprobe）
6. `test_high_res.sh`：拼装 3+4+5 + render markdown
7. 跑一次 `probe_streams.sh` 提交 `streams_meta.json`
8. 跑一次 `test_high_res.sh --group=4k --duration=60` 验证端到端

---

## 7. 风险与开放问题

- **风险 A：摄像头主码流不真 4K**。如果 35 路里没有 4K 分辨率源，`--group=4k` 会拉空。预案：probe 完看到 4K 组为 0 时，脚本给明确提示并列出实际分布。
- **风险 B：S3 上传在长录制场景拖慢主流程**。本测试默认不关 S3（沿用现有 config），但 `--no-s3` 暂留为未来扩展（本期不实现，留 issue）。
- **风险 C：ffprobe 探测 RTSP 时常 hang**。用 `-rw_timeout` + `timeout` 命令双重保护。
- **开放问题（可后续迭代）**：
  - 卡顿率 / 丢帧率：需要 ffprobe `-show_frames` + pts 抽样，本期不做
  - 录制完整性深度校验（每个 GOP 是否齐全）：本期不做
  - 实时大屏 / web 报表：本期不做

---

## 8. 验收

- [ ] `probe_streams.sh` 跑通，产出 `streams_meta.json`，包含 35 路全部摄像头条目
- [ ] `streams_meta.json` git 提交（密码已脱敏）
- [ ] `test_high_res.sh --group=4k --duration=60` 端到端跑通，产出 `reports/report-*.md` + `metrics-*.csv`
- [ ] 报告内分辨率、编码、时长、CPU/RSS 字段都填上
- [ ] `lib/probe.sh`、`lib/classify.sh`、`lib/verify.sh` 各自有 bash 单测（`bats` 或裸 `test ... && echo OK`）
- [ ] README 更新加一节"2K/4K 高分辨率测试"
