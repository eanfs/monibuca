#!/bin/bash

# 启动单个 Monibuca 节点
# 用法: ./start_node.sh <config.yaml> <node_id>

set -e

if [ $# -lt 2 ]; then
    echo "用法: $0 <config.yaml> <node_id>"
    exit 1
fi

CONFIG=$1
NODE_ID=$2

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "[节点 $NODE_ID] 启动时间: $(date '+%Y-%m-%d %H:%M:%S')"
echo "[节点 $NODE_ID] 配置文件: $CONFIG"
echo "[节点 $NODE_ID] 工作目录: $SCRIPT_DIR"
echo "[节点 $NODE_ID] ================================"

# 启动 m7s 服务
./m7s -c "$CONFIG"
