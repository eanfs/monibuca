# cluster-dc 客户测试例子：脚本化集群拉流验证

本目录是客户提供的 5 节点集群测试用例（`config1.yaml` ~ `config5.yaml` + shell 脚本）。

## 测试目标

1. **指定某个节点拉 RTSP**（通过配置 `rtsp.pull`），并发布为 `STREAM_PATH`（默认 `live/camera101`）。
2. **集群内其它节点都能访问该流**：从任意节点对本地 RTSP 端口发起 `DESCRIBE`，应返回：
   - 拉流节点：`RTSP/1.0 200 OK`
   - 非拉流节点：`RTSP/1.0 200 OK`（由节点在本地自动拉流代理，不依赖客户端重定向）
3. **WSS 拉流可用**（WebSocket FLV）：从任意节点对 HTTPS 端口发起 WebSocket 握手：
   - 拉流节点：`101 Switching Protocols`
   - 非拉流节点：`302` 重定向到拉流节点，随后可 `101` 成功

> 说明：RTSP 代理能力需开启 `rtsp.proxyOnRedirect: true`（本目录配置已包含）。
> 说明：浏览器的 WebSocket 不会自动跟随 302，因此需要开启 `flv.proxyOnRedirect: true`（本目录配置已包含，适用于 ws-flv 与 ws-raw/jessica）。

> 说明：本方案不使用 `POST /api/proxy/pull/add`，也不依赖外部“代理服务器”。拉流完全通过运行时生成配置里的 `rtsp.pull` 实现。

---

## 运行方式

### 0) 准备 RTSP_URL（必填）

为避免把账号密码写进仓库，脚本要求你通过环境变量传入：

```bash
export RTSP_URL='rtsp://admin:huicheng123@10.10.10.11:554/Streaming/Channels/101'
```

可选：

```bash
export STREAM_PATH='live/camera101'   # 默认就是它
```

### 1) 启动所有节点（选择哪个节点拉流）

例如让 node1 拉流：

```bash
PULL_NODE_ID=1 ./start_all_nodes.sh
```

你也可以让其它节点拉流（1..5）：

```bash
PULL_NODE_ID=3 ./start_all_nodes.sh
```

脚本会在 `runtime/` 下生成运行时配置与日志：

- `runtime/configs/configN.yaml`：运行时配置（已注入/移除 `rtsp.pull`）
- `runtime/logs/nodeN.log`：节点日志
- `runtime/pids/nodeN.pid`：节点 PID
- `runtime/testN.db`：sqlite DB

### 2) 等待节点就绪后运行测试脚本

默认测试当前已启动的 5 个节点（不会自动切换拉流节点）：

```bash
./start_test_cluster.sh
```

如果你想切换拉流节点，请先停止后重启（例如切到 node3 拉流）：

```bash
./stop_all_nodes.sh
PULL_NODE_ID=3 ./start_all_nodes.sh
./start_test_cluster.sh
```

脚本默认会额外检查 WSS（WebSocket FLV）握手；如需跳过：

```bash
WSS_TEST=0 ./start_test_cluster.sh
```

---

## 停止节点

```bash
./stop_all_nodes.sh
```

---

## 配置检查说明（当前目录）

- `cascadeserver:` 配置在当前 `main.go` 未引入 cascade 插件的情况下不会生效；保留不会影响本测试，但如未来引入 cascade，可能出现端口冲突（需要再评估）。
- `global.storage.s3:` 如果你的二进制没有带 `-tags s3` 编译，S3 存储实现不会生效，程序会回落到本地存储；这与“集群拉流互访”测试无关。

---

## WSS 验证说明（脚本已自动测试）

脚本使用 `curl` 对 `https://localhost:<HTTPS端口>/flv/<streamPath>` 发起 WebSocket 握手（携带 Upgrade 头），期望返回：

- 源节点：`101 Switching Protocols`
- 非源节点：`302` 并带 `Location: wss://...`，随后对该地址握手得到 `101`

因使用自签名证书，脚本会带 `-k` 忽略证书验证。

> 注意：脚本会先测 WSS、后测 RTSP，避免 RTSP 代理提前让所有节点都持有流，从而引发 WSS 302 互相重定向的循环。
