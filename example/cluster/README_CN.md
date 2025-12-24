# cluster 示例（3 节点）运行与验证：多点访问 + 跨节点录制

本目录提供一个最小的 3 节点集群示例：

- `config1.yaml`：node1（HTTP `:8080`，gRPC `:50052`，RTSP `:8554`）
- `config2.yaml`：node2（HTTP `:8081`，gRPC `:50053`，RTSP `:8555`，并在此节点配置拉流）
- `config3.yaml`：node3（HTTP `:8082`，gRPC `:50054`，RTSP `:8556`）

该示例采用“务实方案（A）”：

- **媒体流不做全量同步**（不复制媒体数据）。
- **播放/拉取**：
  - HTTP/WS 类协议（MP4/FLV/HLS 等）仍采用 **302 重定向** 到真正持有该流的节点。
  - RTSP 在启用 `rtsp.proxyOnRedirect: true` 时，会在本地创建拉流代理，**不依赖客户端重定向**。
- **录制等控制面 API**：可从任意节点调用，但会自动 **路由到持有该流的节点执行**（更省资源）。

> 注意：`config2.yaml` 里的 RTSP URL 是占位符（RFC 5737 的 `192.0.2.0/24`），你需要在本机运行时替换为真实摄像机 URL；不要把真实账号密码提交到仓库。

---

## 1. 前置条件

- Go：以 `go.mod` 的 toolchain 为准（项目内已声明）。
- 本机端口空闲：`8080/8081/8082`、`50052/50053/50054`、`8554/8555/8556`。
- 如需拉摄像机：确保能访问摄像机的 `RTSP` 端口（示例里是 `10.10.10.11:554`）。

---

## 2. 启动 3 个节点（手动方式）

在 3 个终端分别执行：

```bash
go run -tags sqlite ./example/cluster/main.go -c example/cluster/config1.yaml
go run -tags sqlite ./example/cluster/main.go -c example/cluster/config2.yaml
go run -tags sqlite ./example/cluster/main.go -c example/cluster/config3.yaml
```

检查节点是否就绪：

```bash
curl -fsS http://localhost:8080/api/sysinfo >/dev/null && echo node1 OK
curl -fsS http://localhost:8081/api/sysinfo >/dev/null && echo node2 OK
curl -fsS http://localhost:8082/api/sysinfo >/dev/null && echo node3 OK
```

---

## 3. 配置 node2 拉 RTSP 到集群

`example/cluster/config2.yaml` 已配置了：

```yaml
rtsp:
  pull:
    live/camera101:
      url: rtsp://user:pass@192.0.2.10:554/Streaming/Channels/101
```

你需要把 `url:` 改成真实摄像机地址，例如：

```text
rtsp://admin:huicheng123@10.10.10.11:554/Streaming/Channels/101
```

推荐做法：**复制一份临时配置再改**（避免误提交账号密码）：

```bash
cp example/cluster/config2.yaml /tmp/config2.local.yaml
sed -i 's|rtsp://user:pass@192.0.2.10:554/Streaming/Channels/101|rtsp://admin:huicheng123@10.10.10.11:554/Streaming/Channels/101|g' /tmp/config2.local.yaml
go run -tags sqlite ./example/cluster/main.go -c /tmp/config2.local.yaml
```

---

## 4. 验证“任意节点都能访问到集群内某节点的流”

当 node2 成功拉到 `live/camera101` 后，通过 HTTP 播放入口（MP4/FLV 等）从 node1 / node3 访问同一个流，应该被 302 重定向到 node2。

### 4.1 MP4（从 node1 访问）

```bash
curl -sS -D- -o /dev/null http://localhost:8080/mp4/live/camera101.mp4
```

期望看到类似：

```text
HTTP/1.1 302 Found
Location: http://localhost:8081/mp4/live/camera101.mp4
```

### 4.2 FLV（从 node3 访问）

```bash
curl -sS -D- -o /dev/null http://localhost:8082/flv/live/camera101.flv
```

期望看到类似：

```text
HTTP/1.1 302 Found
Location: http://localhost:8081/flv/live/camera101.flv
```

> 如果你直接访问源节点（node2），通常是 `200`（并开始输出流数据）。

### 4.3 RTSP（从 node1 / node3 访问）

当 `rtsp.proxyOnRedirect: true` 时，RTSP 会在本地自动拉流代理，因此无需客户端支持重定向。

```bash
ffprobe -rtsp_transport tcp -timeout 10000000 -v quiet -show_streams -i rtsp://localhost:8554/live/camera101
ffprobe -rtsp_transport tcp -timeout 10000000 -v quiet -show_streams -i rtsp://localhost:8556/live/camera101
```

期望：两条命令都能成功返回（无 302 重定向）。

### 4.4 WSS（WebSocket FLV）

WSS 通过 HTTPS 端口进行 WebSocket 握手（自签名证书需 `-k` 跳过校验）。
浏览器的 WebSocket 不会自动跟随 302 重定向，如需从任意节点直接播放，可开启 `flv.proxyOnRedirect: true`。

从非源节点（node1）访问时，会 302 重定向到源节点：

```bash
curl -k -sS -D- -o /dev/null --http1.1 \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  https://localhost:18555/flv/live/camera101
```

期望看到类似：

```text
HTTP/1.1 302 Found
Location: wss://localhost:18556/flv/live/camera101
```

再对源节点（node2）发起握手，应返回 `101 Switching Protocols`：

```bash
curl -k -sS -D- -o /dev/null --http1.1 \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  https://localhost:18556/flv/live/camera101
```

---

## 5. 验证“从任意节点都能录制这条流”

录制使用 MP4 插件的 HTTP API（gRPC-Gateway）：

- `POST /mp4/api/start/{streamPath=**}`：开始录制
- `POST /mp4/api/stop/{streamPath=**}`：停止录制

### 5.1 重要注意（避免录制文件被自动清理）

MP4 插件默认会按磁盘占用率做清理（例如阈值 `80%`）。如果你运行环境磁盘占用很高，可能刚录完就触发清理。

建议在“本地临时配置”里加：

```yaml
mp4:
  overwritePercent: 0
```

### 5.2 从非源节点发起录制（例如从 node3 发起，实际在 node2 执行）

建议 `filePath` 使用**相对路径**（避免不同工作目录/存储 base path 造成混淆）：

```bash
curl -sS -X POST \
  http://localhost:8082/mp4/api/start/live/camera101 \
  -H 'Content-Type: application/json' \
  -d '{"filePath":"./recordings","fragment":"3s"}'
```

等待几秒后停止（也可以从任意节点发 stop）：

```bash
curl -sS -X POST \
  http://localhost:8080/mp4/api/stop/live/camera101 \
  -H 'Content-Type: application/json' \
  -d '{}'
```

检查是否生成文件（在**执行录制的节点**的工作目录下）：

```bash
find ./recordings -maxdepth 1 -type f -name '*.mp4' -ls
```

---

## 6. 一键脚本（推荐：不改动仓库配置，自动拉起 3 节点并验证）

下面脚本会：

- 创建 `example/cluster/run-logs-<timestamp>/` 临时目录
- 复制三份配置到临时目录（避免污染 git 工作区）
- 给 node2 注入真实 RTSP URL
- 为避免误删，给三节点临时配置追加 `mp4.overwritePercent: 0`
- 编译并启动 3 节点
- 验证 302 重定向与跨节点录制，并检查是否生成 mp4 文件

```bash
set -euo pipefail

RTSP_URL=${RTSP_URL:-'rtsp://admin:huicheng123@10.10.10.11:554/Streaming/Channels/101'}
RUN_DIR="example/cluster/run-logs-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RUN_DIR"

cp example/cluster/config1.yaml "$RUN_DIR/config1.yaml"
cp example/cluster/config2.yaml "$RUN_DIR/config2.yaml"
cp example/cluster/config3.yaml "$RUN_DIR/config3.yaml"

# 独立 DB，避免多次运行互相干扰
sed -i "s#dsn: \"test1.db\"#dsn: \"$RUN_DIR/test1.db\"#" "$RUN_DIR/config1.yaml"
sed -i "s#dsn: \"test2.db\"#dsn: \"$RUN_DIR/test2.db\"#" "$RUN_DIR/config2.yaml"
sed -i "s#dsn: \"test3.db\"#dsn: \"$RUN_DIR/test3.db\"#" "$RUN_DIR/config3.yaml"

# 注入真实 RTSP（占位符 -> 真实）
sed -i "s|rtsp://user:pass@192.0.2.10:554/Streaming/Channels/101|${RTSP_URL}|g" "$RUN_DIR/config2.yaml"

# 禁用按磁盘占用率清理（避免测试时刚录完就被删）
for f in "$RUN_DIR"/config{1,2,3}.yaml; do
  if ! rg -q "^mp4:" "$f"; then
    printf "\nmp4:\n  overwritePercent: 0\n" >> "$f"
  fi
done

BIN="$RUN_DIR/m7s-cluster"
go build -tags sqlite -o "$BIN" ./example/cluster

start_node() { local name=$1 cfg=$2; "$BIN" -c "$cfg" >"$RUN_DIR/$name.log" 2>&1 & echo $! >"$RUN_DIR/$name.pid"; }
start_node node1 "$RUN_DIR/config1.yaml"
start_node node2 "$RUN_DIR/config2.yaml"
start_node node3 "$RUN_DIR/config3.yaml"

wait_http() {
  local port=$1 deadline=$((SECONDS+30))
  while [ $SECONDS -lt $deadline ]; do
    curl -fsS "http://localhost:${port}/api/sysinfo" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}
wait_http 8080; wait_http 8081; wait_http 8082

# MP4 HTTP 302 验证（RTSP 不再重定向）
curl -sS -D- -o /dev/null http://localhost:8080/mp4/live/camera101.mp4 | rg -n "HTTP/|Location:"

# WSS 握手验证（node1 重定向，node2 返回 101）
curl -k -sS -D- -o /dev/null --http1.1 \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  https://localhost:18555/flv/live/camera101 | rg -n "HTTP/|Location:"
curl -k -sS -D- -o /dev/null --http1.1 \
  -H "Connection: Upgrade" -H "Upgrade: websocket" \
  -H "Sec-WebSocket-Version: 13" -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
  https://localhost:18556/flv/live/camera101 | rg -n "HTTP/"

# 录制：从 node3 发起，实际由源节点执行
mkdir -p "$RUN_DIR/recordings"
curl -sS -X POST http://localhost:8082/mp4/api/start/live/camera101 \
  -H 'Content-Type: application/json' \
  -d "{\"filePath\":\"$RUN_DIR/recordings\",\"fragment\":\"3s\"}" >/dev/null
sleep 6
curl -sS -X POST http://localhost:8080/mp4/api/stop/live/camera101 \
  -H 'Content-Type: application/json' \
  -d '{}' >/dev/null
sleep 2

find "$RUN_DIR/recordings" -maxdepth 1 -type f -name '*.mp4' -ls
echo "Logs: $RUN_DIR"
echo "PIDs: node1=$(cat "$RUN_DIR/node1.pid") node2=$(cat "$RUN_DIR/node2.pid") node3=$(cat "$RUN_DIR/node3.pid")"
```

停止节点：

```bash
kill -INT $(cat example/cluster/run-logs-*/node*.pid)
```

---

## 7. 常见问题（Troubleshooting）

- **访问不重定向 / 仍然 404**：通常是 node2 尚未成功拉到流，或集群发现尚未同步完成；先查看 node2 日志中是否出现 `publish streamPath=live/camera101`。
- **录制 API 返回成功但没文件**：优先检查是否触发了磁盘清理（建议临时配置 `mp4.overwritePercent: 0`），以及 `filePath` 是否使用相对路径/指向你有权限写入的位置。
- **RTSP 仍然返回重定向/拉流失败**：确认 `rtsp.proxyOnRedirect: true`，并确保 `global.apiRoute.enable` 已开启（cluster 插件会自动开启）。
- **端口被占用**：`ss -lntp | rg ":(8080|8081|8082|50052|50053|50054|8554|8555|8556)\\b"` 查占用进程并停止。
