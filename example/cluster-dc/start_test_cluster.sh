#!/bin/bash

# Monibuca 集群测试脚本（example/cluster-dc）
# 依赖 start_all_nodes.sh 启动节点后再执行。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

STREAM_PATH="${STREAM_PATH:-live/camera101}"
RUNTIME_DIR="${RUNTIME_DIR:-runtime}"
WSS_TEST="${WSS_TEST:-1}"

HTTP_PORTS=("8081" "8082" "8083" "8084" "8085")
RTSP_PORTS=("5541" "5542" "5543" "5544" "5545")
HTTPS_PORTS=("18551" "18552" "18553" "18558" "18555")
NODE_IDS=("1" "2" "3" "4" "5")

start_recording() {
  local node_id=$1
  local http_port=$2
  local stream_path=$3
  local record_dir="${RUNTIME_DIR}/recordings/node${node_id}"
  local record_name="node${node_id}-${stream_path//\//_}.mp4"

  mkdir -p "$record_dir"

  echo "节点 $node_id 开始录制流 $stream_path -> $record_dir/$record_name"
  curl -s -X POST "http://localhost:$http_port/mp4/api/start/$stream_path" \
    -H "Content-Type: application/json" \
    -d "{
      \"fragment\": \"3s\",
      \"filePath\": \"$record_dir\",
      \"fileName\": \"$record_name\"
    }" >/dev/null
}

# 停止 MP4 录制
# 参数: node_id http_port stream_path
stop_recording() {
  local node_id=$1
  local http_port=$2
  local stream_path=$3

  echo "节点 $node_id 停止录制流 $stream_path"
  curl -s --location --request POST "http://localhost:$http_port/mp4/api/stop/$stream_path" >/dev/null
}

# 测试播放流
# 参数: node_id rtsp_port stream_path
test_playback() {
  local node_id=$1
  local rtsp_port=$2
  local stream_path=$3
  local rtsp_url="rtsp://localhost:$rtsp_port/$stream_path"
  local attempt=1
  local max_attempts=10

  echo "节点 $node_id 测试播放流 $stream_path (URL: $rtsp_url)"

  while [ $attempt -le $max_attempts ]; do
    if ffprobe -rtsp_transport tcp -timeout 10000000 -v quiet -print_format json -show_streams -i "$rtsp_url" >/dev/null 2>&1; then
      echo "  ✓ 节点 $node_id 播放流 $stream_path 成功"
      return 0
    fi
    sleep 1
    attempt=$((attempt + 1))
  done

  echo "  ✗ 节点 $node_id 播放流 $stream_path 失败"
  return 0
}

request_wss_headers() {
  local url=$1
  curl -k -s -D - -o /dev/null --max-time 3 --http1.1 \
    -H "Connection: Upgrade" \
    -H "Upgrade: websocket" \
    -H "Sec-WebSocket-Version: 13" \
    -H "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==" \
    "$url" || true
}

normalize_ws_url() {
  local url=$1
  printf "%s" "$url" | sed -e 's#^wss://#https://#' -e 's#^ws://#http://#'
}

# 测试 WSS 播放
# 参数: node_id https_port stream_path
test_wss() {
  local node_id=$1
  local https_port=$2
  local stream_path=$3
  local https_url="https://localhost:$https_port/flv/$stream_path"
  local current_url="$https_url"
  local hop=0
  local max_hops=5

  echo "节点 $node_id 测试 WSS 拉流 $stream_path (URL: wss://localhost:$https_port/flv/$stream_path)"

  while [ $hop -le $max_hops ]; do
    local headers
    headers="$(request_wss_headers "$current_url")"
    local status
    status="$(printf "%s" "$headers" | head -n1 | tr -d '\r' | awk '{print $2}')"

    if [ -z "$status" ]; then
      echo "  ✗ 节点 $node_id WSS 无响应"
      return 0
    fi

    if [ "$status" = "101" ]; then
      if [ $hop -eq 0 ]; then
        echo "  ✓ 节点 $node_id WSS 直连成功"
      else
        echo "  ✓ 节点 $node_id WSS 重定向后成功"
      fi
      return 0
    fi

    if [ "$status" = "302" ] || [ "$status" = "301" ] || [ "$status" = "307" ] || [ "$status" = "308" ]; then
      local location
      location="$(printf "%s" "$headers" | awk -F': ' 'tolower($1)=="location" {print $2}' | head -n1 | tr -d '\r')"
      if [ -z "$location" ]; then
        echo "  ✗ 节点 $node_id WSS 重定向缺少 Location"
        return 0
      fi
      echo "  -> 重定向到 $location"
      current_url="$(normalize_ws_url "$location")"
      hop=$((hop + 1))
      continue
    fi

    echo "  ✗ 节点 $node_id WSS 失败 (HTTP $status)"
    return 0
  done

  echo "  ✗ 节点 $node_id WSS 重定向次数过多"
  return 0
}

wait_node_ready() {
  local http_port=$1
  local deadline=$((SECONDS + 30))
  while [ $SECONDS -lt $deadline ]; do
    if curl -fsS "http://localhost:${http_port}/api/sysinfo" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

# 注意：节点由用户手动管理，此脚本不负责停止节点

# ============================================
# 主流程开始
# ============================================

echo ""
echo "=== Monibuca 集群测试脚本 ==="
echo "STREAM_PATH=$STREAM_PATH"
echo "RUNTIME_DIR=$RUNTIME_DIR"
echo "WSS_TEST=$WSS_TEST"
echo ""

echo "=== 等待节点就绪 ==="
for i in 0 1 2 3 4; do
  http_port="${HTTP_PORTS[$i]}"
  if ! wait_node_ready "$http_port"; then
    echo "✗ 节点 $((i + 1)) 未就绪 (HTTP :$http_port)"
    exit 1
  fi
done

if [ "$WSS_TEST" != "0" ]; then
  echo "=== 测试 WSS（WebSocket FLV） ==="
  for i in 0 1 2 3 4; do
    node_id="${NODE_IDS[$i]}"
    https_port="${HTTPS_PORTS[$i]}"
    test_wss "$node_id" "$https_port" "$STREAM_PATH"
  done
else
  echo "=== 跳过 WSS 测试 (WSS_TEST=0) ==="
fi

echo ""
echo "=== 测试播放（每个节点播放该流） ==="
for i in 0 1 2 3 4; do
  node_id="${NODE_IDS[$i]}"
  rtsp_port="${RTSP_PORTS[$i]}"
  test_playback "$node_id" "$rtsp_port" "$STREAM_PATH"
done

echo ""
echo "=== 开始录制（每个节点录制该流） ==="
for i in 0 1 2 3 4; do
  node_id="${NODE_IDS[$i]}"
  http_port="${HTTP_PORTS[$i]}"
  start_recording "$node_id" "$http_port" "$STREAM_PATH"
done

echo ""
echo "=== 等待录制片段完成 ==="
sleep 6

echo ""
echo "=== 停止录制 ==="
for i in 0 1 2 3 4; do
  node_id="${NODE_IDS[$i]}"
  http_port="${HTTP_PORTS[$i]}"
  stop_recording "$node_id" "$http_port" "$STREAM_PATH"
done

echo ""
echo "=== 测试完成 ==="
echo "可在 ${RUNTIME_DIR}/recordings/ 下检查 mp4 文件"
