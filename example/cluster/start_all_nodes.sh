#!/bin/bash

# 启动所有 Monibuca 集群节点
# 用法: ./start_all_nodes.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 节点配置
CONFIGS=("config1.yaml" "config2.yaml" "config3.yaml" "config4.yaml" "config5.yaml")
HTTP_PORTS=("8081" "8082" "8083" "8084" "8085")
PIDS=()

echo "=== 启动所有集群节点 ==="
echo ""

# 创建日志目录
mkdir -p logs

# 启动所有节点
for i in 0 1 2 3 4; do
    config="${CONFIGS[$i]}"
    http_port="${HTTP_PORTS[$i]}"
    node_id=$((i + 1))

    echo "启动节点 $node_id (配置: $config, HTTP端口: $http_port)..."
    ./start_node.sh "$config" "$node_id" > "logs/node${node_id}.log" 2>&1 &
    pid=$!
    PIDS+=($pid)
    echo "  ✓ 节点 $node_id 已启动, PID: $pid, 日志: logs/node${node_id}.log"
    sleep 2
done

echo ""
echo "=== 所有节点已启动 ==="
echo "节点进程 PIDs: ${PIDS[@]}"
echo ""
echo "查看日志:"
for i in 1 2 3 4 5; do
    echo "  tail -f logs/node${i}.log"
done
echo ""
echo "停止所有节点:"
echo "  pkill -f 'm7s -c config'"
echo "  或者: kill ${PIDS[@]}"
echo ""
echo "等待节点就绪后，运行测试脚本:"
echo "  ./start_test_cluster.sh"
