#!/bin/bash

# 启动单个 Monibuca 节点
# 用法: ./start_node.sh <config.yaml> <node_id>

set -euo pipefail

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

# 启动 m7s 服务：
# - 优先使用 M7S_BIN（如果你提供了可执行二进制）
# - 否则尝试使用 ./m7s（如果是本机可执行 ELF）
# - 否则回退到 go run（更通用，适合在不同环境跑客户脚本）

M7S_TAGS="${M7S_TAGS:-sqlite}"

if [ -n "${M7S_BIN:-}" ]; then
    exec "$M7S_BIN" -c "$CONFIG"
fi

if [ -x ./m7s ] && command -v file >/dev/null 2>&1; then
    if file ./m7s | rg -q "ELF"; then
        exec ./m7s -c "$CONFIG"
    fi
fi

exec go run -tags "$M7S_TAGS" ./main.go -c "$CONFIG"
