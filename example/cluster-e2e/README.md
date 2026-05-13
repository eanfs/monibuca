# Cluster v1 e2e

3 节点 m7s + 1 consul + 1 postgres + 1 minio 的端到端验证环境。

## 一次性准备

- Docker + Docker Compose
- 一个 `sample.mp4` 文件(任何 mp4 都行,准备给 ffmpeg 当推流源)
- `jq` 命令行工具(`brew install jq` 或 `apt install jq`)

```bash
cp /path/to/your/sample.mp4 ./sample.mp4
# 或 SAMPLE=/path/to/foo.mp4 ./smoke.sh
```

## 跑

```bash
chmod +x smoke.sh
./smoke.sh
# 调试模式:停在 up 状态不 teardown
KEEP=1 ./smoke.sh
```

退出码 0 = 全部自动场景 PASS。

## 手动启停

```bash
# 启动
docker compose up -d --build

# 查看日志
docker compose logs -f node-1 node-2 node-3

# 停止并清理卷
docker compose down -v
```

## 端口映射

| 节点 | RTMP | RTSP | HTTP | gRPC |
|---|---|---|---|---|
| node-1 | 1935 | 5541 | 8081 | 50051 |
| node-2 | 1936 | 5542 | 8082 | 50052 |
| node-3 | 1937 | 5543 | 8083 | 50053 |

Consul HTTP: 8500; Postgres: 5432; MinIO: 9000 (S3 API) / 9001 (Console).

MinIO Console 访问: http://localhost:9001 (admin / m7sm7sm7s)
Consul UI: http://localhost:8500

## 场景清单(对应 cluster spec §4.4)

| # | 场景 | smoke.sh 自动 | 说明 |
|---|---|---|---|
| 1 | 三节点 membership 同时在 Consul | 是 | `/api/cluster/nodes` peers 数量 = 3 |
| 2 | 推 RTMP 到 node-1 → `m7s/streams/live/foo` = node-1 | 是 | Consul KV 验证 |
| 3 | 从 node-2 订阅 RTMP,500ms 内出图 | 是 | ffmpeg 订阅计时 |
| 4 | 从 node-3 再订阅同流,node-2 上 pull-proxy 不增加(复用) | 手动 | 需要 metrics 计数验证 |
| 5 | 推 record API 到 node-3 → 文件落 node-1 | 部分 | API 调用自动,node_id 校验手动 |
| 6 | 从 node-2 list records → 包含 node-1 的录制 | 部分 | 需目视确认 JSON 中 nodeid=node-1 |
| 7 | 从 node-2 `/download` → 302 到 node-1 | 手动 | `curl -v http://localhost:8082/download/live/foo` |
| 8 | kill node-1 → 10-12s 内 `m7s/streams/live/foo` 消失,relay 退出 | 是 | Consul KV 轮询 |
| 9 | 同 streamPath 同时推 node-1 + node-2 → first-write-wins | 手动 | 需要两个终端并发推流 |
| 10 | `/api/cluster/lb-suggest` 返回 streams 最少节点 | 是 | 验证 suggested 字段非空 |

## 已知限制

- `smoke.sh` 当前自动覆盖场景 1, 2, 3, 8, 10。其余场景(4 / 5 / 6 / 7 / 9)需要手动 + 看日志验证。
- mp4 录制 API 使用 gRPC-gateway 路径 `/mp4.api/StartRecord`(JSON body),具体响应格式以实际部署为准。
- gotask 库内部在 `-race` 下有已知数据竞争(framework lifecycle 内部状态);cluster 代码层面无 race。
- `sample.mp4` 文件不包含在版本库中,需用户自行提供。
- 本环境仅用于功能验证,不做性能压测;生产环境请做好网络隔离与认证配置。
