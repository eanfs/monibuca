#!/bin/bash

# Monibuca cluster-dc 集群测试脚本
#
# 目标：
# 1) 在某个节点拉 RTSP（通过配置 rtsp.pull，不使用 /api/proxy/pull/add）
# 2) 确认其它任意节点都能通过集群重定向“拉到/访问”这条流（RTSP DESCRIBE）
#
# 用法（推荐）：
#   export RTSP_URL='rtsp://admin:***@10.10.10.11:554/Streaming/Channels/101'
#   ./start_test_cluster.sh
#
# 可选环境变量：
#   STREAM_PATH=live/camera101
#   PULL_NODES="1 2 3 4 5"     # 逐个作为拉流节点进行测试
#   SKIP_RESTART=0             # 0: 每轮重启节点（更可靠）；1: 不重启，只做一次检测

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

: "${RTSP_URL:?Please set RTSP_URL (e.g. rtsp://admin:***@10.10.10.11:554/Streaming/Channels/101)}"

STREAM_PATH="${STREAM_PATH:-live/camera101}"
PULL_NODES="${PULL_NODES:-1 2 3 4 5}"
SKIP_RESTART="${SKIP_RESTART:-0}"
CLEANUP_ON_EXIT="${CLEANUP_ON_EXIT:-1}"

cleanup() {
  if [ "$CLEANUP_ON_EXIT" = "1" ] && [ "$SKIP_RESTART" = "0" ]; then
    ./stop_all_nodes.sh >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

HTTP_PORTS=(8081 8082 8083 8084 8085)
RTSP_PORTS=(5541 5542 5543 5544 5545)

wait_ready() {
  local port=$1
  local deadline=$((SECONDS+30))
  while [ $SECONDS -lt $deadline ]; do
    if curl -fsS "http://localhost:${port}/api/sysinfo" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

rtsp_describe() {
  # Print raw RTSP response (headers + maybe SDP)
  local host=$1
  local port=$2
  local stream_path=$3
  printf 'DESCRIBE rtsp://%s:%s/%s RTSP/1.0\r\nCSeq: 1\r\nUser-Agent: m7s-cluster-dc-test\r\nAccept: application/sdp\r\n\r\n' \
    "$host" "$port" "$stream_path" | nc -w 3 "$host" "$port" || true
}

rtsp_describe_retry() {
  local host=$1
  local port=$2
  local stream_path=$3
  local deadline=$((SECONDS+20))
  while [ $SECONDS -lt $deadline ]; do
    resp="$(rtsp_describe "$host" "$port" "$stream_path")"
    # Some environments return empty on early connect/race; retry.
    if [ -n "$resp" ]; then
      printf "%s" "$resp"
      return 0
    fi
    sleep 0.5
  done
  return 1
}

parse_location() {
  # Extract Location header from RTSP response
  rg "^Location:" -m 1 | sed -E 's/^Location:[[:space:]]*//'
}

assert_contains() {
  local haystack=$1
  local needle=$2
  if ! printf "%s" "$haystack" | rg -q "$needle"; then
    echo "ERROR: expected response to contain: $needle" >&2
    echo "--- response ---" >&2
    printf "%s\n" "$haystack" >&2
    return 1
  fi
  return 0
}

run_round() {
  local pull_node_id=$1
  local pull_index=$((pull_node_id - 1))
  local pull_rtsp_port="${RTSP_PORTS[$pull_index]}"

  echo ""
  echo "============================================================"
  echo "Round: PULL_NODE_ID=$pull_node_id (RTSP :$pull_rtsp_port) STREAM_PATH=$STREAM_PATH"
  echo "============================================================"

  if [ "$SKIP_RESTART" = "0" ]; then
    ./stop_all_nodes.sh || true
    PULL_NODE_ID="$pull_node_id" STREAM_PATH="$STREAM_PATH" RTSP_URL="$RTSP_URL" ./start_all_nodes.sh

    echo "等待节点就绪..."
    for p in "${HTTP_PORTS[@]}"; do
      if ! wait_ready "$p"; then
        echo "ERROR: node http :$p not ready" >&2
        exit 2
      fi
    done
  fi

  echo ""
  echo "=== RTSP 验证：任意节点都能访问该流（DESCRIBE） ==="
  for i in 1 2 3 4 5; do
    idx=$((i - 1))
    rtsp_port="${RTSP_PORTS[$idx]}"
    echo "- node$i DESCRIBE rtsp://localhost:$rtsp_port/$STREAM_PATH"

    resp="$(rtsp_describe_retry localhost "$rtsp_port" "$STREAM_PATH" || true)"
    # Normalize line endings for grep/rg.
    resp_lf="$(printf "%s" "$resp" | tr -d $'\r')"
    if [ -z "$resp_lf" ]; then
      echo "ERROR: empty RTSP response (node$i, port=$rtsp_port)" >&2
      return 1
    fi

    if [ "$i" = "$pull_node_id" ]; then
      assert_contains "$resp_lf" "^RTSP/1\\.0 200" || return 1
    else
      assert_contains "$resp_lf" "^RTSP/1\\.0 Found" || return 1
      loc="$(printf "%s" "$resp_lf" | parse_location || true)"
      if [ -z "$loc" ]; then
        echo "ERROR: missing Location header (node$i)" >&2
        printf "%s\n" "$resp_lf" >&2
        return 1
      fi
      # Expect redirect to pull node
      expected="rtsp://localhost:${pull_rtsp_port}/${STREAM_PATH}"
      if [ "$loc" != "$expected" ]; then
        echo "ERROR: unexpected Location (node$i): $loc (expected: $expected)" >&2
        return 1
      fi
      # Follow Location and ensure 200
      follow_resp="$(rtsp_describe localhost "$pull_rtsp_port" "$STREAM_PATH")"
      follow_lf="$(printf "%s" "$follow_resp" | tr -d $'\r')"
      assert_contains "$follow_lf" "^RTSP/1\\.0 200" || return 1
    fi
  done

  echo "✓ Round PASS: PULL_NODE_ID=$pull_node_id"
}

echo "=== Monibuca cluster-dc 测试开始 ==="
echo "RTSP_URL=$RTSP_URL"
echo "STREAM_PATH=$STREAM_PATH"
echo "PULL_NODES=$PULL_NODES"
echo "SKIP_RESTART=$SKIP_RESTART"

for n in $PULL_NODES; do
  run_round "$n"
done

echo ""
echo "=== 测试完成：全部轮次通过 ==="
echo "如需停止节点：./stop_all_nodes.sh"
