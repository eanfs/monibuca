#!/bin/bash

# 启动所有 Monibuca 集群节点（example/cluster-dc）
#
# 用法：
#   RTSP_URL='rtsp://...' ./start_all_nodes.sh
#
# 可选环境变量：
#   STREAM_PATH=live/camera101   # 拉流发布到本机的 streamPath
#   PULL_NODE_ID=1               # 由哪个节点负责拉 RTSP（1..5）
#   RUNTIME_DIR=runtime          # 运行时目录（生成配置/日志/pid/db）
#
# 注意：RTSP_URL 必须通过环境变量传入，避免把账号密码写进仓库。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

: "${RTSP_URL:?Please set RTSP_URL (e.g. rtsp://admin:***@10.10.10.11:554/Streaming/Channels/101)}"

STREAM_PATH="${STREAM_PATH:-live/camera101}"
PULL_NODE_ID="${PULL_NODE_ID:-1}"
M7S_TAGS="${M7S_TAGS:-sqlite}"

RUNTIME_DIR="${RUNTIME_DIR:-runtime}"
LOG_DIR="${LOG_DIR:-$RUNTIME_DIR/logs}"
PID_DIR="${PID_DIR:-$RUNTIME_DIR/pids}"
CFG_DIR="${CFG_DIR:-$RUNTIME_DIR/configs}"

CONFIGS=("config1.yaml" "config2.yaml" "config3.yaml" "config4.yaml" "config5.yaml")
HTTP_PORTS=("8081" "8082" "8083" "8084" "8085")
RTSP_PORTS=("5541" "5542" "5543" "5544" "5545")
GRPC_PORTS=("50051" "50052" "50053" "50054" "50055")

echo "=== 启动所有集群节点 ==="
echo "RTSP_URL=$RTSP_URL"
echo "STREAM_PATH=$STREAM_PATH"
echo "PULL_NODE_ID=$PULL_NODE_ID"
echo "M7S_TAGS=$M7S_TAGS"
echo ""

mkdir -p "$LOG_DIR" "$PID_DIR" "$CFG_DIR"
rm -f "$PID_DIR"/*.pid 2>/dev/null || true

strip_rtsp_pull() {
  # Remove rtsp.pull block in YAML file (best-effort, assumes 2-space indent)
  local file=$1
  awk '
    BEGIN{in_rtsp=0; skip=0}
    /^rtsp:[[:space:]]*$/ {in_rtsp=1}
    in_rtsp && /^  pull:[[:space:]]*$/ {skip=1; next}
    skip {
      if ($0 ~ /^[^[:space:]]/){ skip=0; in_rtsp=0 }
      else { next }
    }
    {print}
  ' "$file" > "$file.tmp" && mv "$file.tmp" "$file"
}

inject_rtsp_pull() {
  local file=$1
  local stream_path=$2
  local rtsp_url=$3
  local rtsp_port=$4
  awk -v sp="$stream_path" -v url="$rtsp_url" -v tcp=":$(printf "%s" "$rtsp_port")" '
    {print}
    /^rtsp:[[:space:]]*$/ {in_rtsp=1; next}
    in_rtsp && $0 ~ "^[[:space:]]+tcp:[[:space:]]*" tcp {
      print "  pull:"
      print "    " sp ":"
      print "      url: " url
      in_rtsp=0
    }
  ' "$file" > "$file.tmp" && mv "$file.tmp" "$file"
}

ensure_mp4_no_cleanup() {
  local file=$1
  if ! rg -q "^mp4:" "$file"; then
    printf "\nmp4:\n  overwritePercent: 0\n" >> "$file"
  fi
}

rewrite_db_dsn() {
  local file=$1
  local node_id=$2
  sed -i "s#dsn: \\\"test${node_id}\\.db\\\"#dsn: \\\"${RUNTIME_DIR}/test${node_id}.db\\\"#g" "$file"
}

rewrite_seedservers() {
  local file=$1
  local self_port=$2
  # Ensure every node can discover every other node for redirect/routing.
  # We rewrite cluster.sync.seedservers to the full set of gRPC peers excluding self.
  awk -v self="localhost:${self_port}" '
    BEGIN{
      # hardcode the full peer list (localhost only) to keep this script dependency-free
      peers[0]="localhost:50051";
      peers[1]="localhost:50052";
      peers[2]="localhost:50053";
      peers[3]="localhost:50054";
      peers[4]="localhost:50055";
      skipping=0
    }
    # match "    seedservers:" (4 spaces)
    /^[[:space:]]{4}seedservers:[[:space:]]*$/ {
      print
      for (i=0; i<5; i++) {
        if (peers[i] != self) {
          print "      - \"" peers[i] "\""
        }
      }
      skipping=1
      next
    }
    skipping==1 {
      # skip old list items under seedservers: (6 spaces + '-')
      if ($0 ~ /^[[:space:]]{6}-[[:space:]]*/) { next }
      skipping=0
    }
    { print }
  ' "$file" > "$file.tmp" && mv "$file.tmp" "$file"
}

echo "准备运行时配置..."
for i in 0 1 2 3 4; do
  node_id=$((i + 1))
  src="${CONFIGS[$i]}"
  dst="$CFG_DIR/config${node_id}.yaml"
  cp "$src" "$dst"
  rewrite_db_dsn "$dst" "$node_id"
  rewrite_seedservers "$dst" "${GRPC_PORTS[$i]}"
  strip_rtsp_pull "$dst"
  ensure_mp4_no_cleanup "$dst"
done

pull_index=$((PULL_NODE_ID - 1))
if [ "$pull_index" -lt 0 ] || [ "$pull_index" -gt 4 ]; then
  echo "ERROR: invalid PULL_NODE_ID=$PULL_NODE_ID (expected 1..5)" >&2
  exit 2
fi

inject_rtsp_pull "$CFG_DIR/config${PULL_NODE_ID}.yaml" "$STREAM_PATH" "$RTSP_URL" "${RTSP_PORTS[$pull_index]}"
echo "✓ 已为 node${PULL_NODE_ID} 注入 rtsp.pull: $STREAM_PATH <- $RTSP_URL"
echo ""

# Build a local binary once so PIDs are stable and shutdown works reliably.
M7S_LOCAL_BIN="${M7S_LOCAL_BIN:-$RUNTIME_DIR/m7s}"
echo "构建本地测试二进制: $M7S_LOCAL_BIN"
go build -tags "$M7S_TAGS" -o "$M7S_LOCAL_BIN" .

PIDS=()
for i in 0 1 2 3 4; do
  node_id=$((i + 1))
  http_port="${HTTP_PORTS[$i]}"
  rtsp_port="${RTSP_PORTS[$i]}"
  config="$CFG_DIR/config${node_id}.yaml"

  echo "启动节点 $node_id (配置: $config, HTTP端口: $http_port, RTSP端口: $rtsp_port)..."
  M7S_BIN="$M7S_LOCAL_BIN" ./start_node.sh "$config" "$node_id" > "$LOG_DIR/node${node_id}.log" 2>&1 &
  pid=$!
  PIDS+=("$pid")
  echo "$pid" > "$PID_DIR/node${node_id}.pid"
  echo "  ✓ 节点 $node_id 已启动, PID: $pid, 日志: $LOG_DIR/node${node_id}.log"
  sleep 2
done

echo ""
echo "=== 所有节点已启动 ==="
echo "节点进程 PIDs: ${PIDS[*]}"
echo ""
echo "查看日志:"
for i in 1 2 3 4 5; do
  echo "  tail -f $LOG_DIR/node${i}.log"
done
echo ""
echo "停止所有节点:"
echo "  ./stop_all_nodes.sh"
echo ""
echo "等待节点就绪后，运行测试脚本:"
echo "  ./start_test_cluster.sh"
