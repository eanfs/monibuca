#!/bin/bash

# Monibuca 集群测试启动脚本
# 依次启动5个节点并拉取 RTSP 流

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 节点配置 (格式: config|http_port|rtsp_port|stream_path|rtsp_url|proxy_name)
# 使用 | 分隔 URL，避免 URL 中的 : 和 @ 符号干扰
NODE1="config1.yaml|8081|5541|live/camera91|rtsp://admin:a1234567@192.168.10.91/ch1/main/av_stream|proxy91"
NODE2="config2.yaml|8082|5542|live/camera92|rtsp://admin:a1234567@192.168.10.92/ch1/main/av_stream|proxy92"
NODE3="config3.yaml|8083|5543|live/camera93|rtsp://admin:a1234567@192.168.10.93/ch1/main/av_stream|proxy93"
NODE4="config4.yaml|8084|5544|live/camera94|rtsp://admin:a1234567@192.168.12.91:554/cam/realmonitor?channel=1&subtype=0|proxy94"
NODE5="config5.yaml|8085|5545|live/camera95|rtsp://admin:a1234567@192.168.12.92:554/cam/realmonitor?channel=1&subtype=0|proxy95"

NODES=("$NODE1" "$NODE2" "$NODE3")

# 所有流的路径（用于录制）
STREAM_PATHS=("live/camera91" "live/camera92" "live/camera93")

# 注意：请先手动启动所有节点，然后再运行此脚本
# 启动节点命令示例：
#   ./start_node.sh config1.yaml 1 > logs/node1.log 2>&1 &
#   ./start_node.sh config2.yaml 2 > logs/node2.log 2>&1 &
#   ./start_node.sh config3.yaml 3 > logs/node3.log 2>&1 &
#   ./start_node.sh config4.yaml 4 > logs/node4.log 2>&1 &
#   ./start_node.sh config5.yaml 5 > logs/node5.log 2>&1 &

# 拉取 RTSP 流 (使用代理方式)
pull_stream() {
    local http_port=$1
    local stream_path=$2
    local source_url=$3
    local proxy_name=$4

    echo "添加拉流代理: $stream_path <- $source_url"

    local response=$(curl -s -X POST "http://localhost:$http_port/api/proxy/pull/add" \
        -H "Content-Type: application/json" \
        -d "{
            \"parentID\": 0,
            \"name\": \"$proxy_name\",
            \"type\": \"rtsp\",
            \"streamPath\": \"$stream_path\",
            \"pullOnStart\": true,
            \"pullURL\": \"$source_url\"
        }")

    echo "响应: $response"
}

# 开始 MP4 录制
# 参数: node_id http_port stream_path
start_recording() {
    local node_id=$1
    local http_port=$2
    local stream_path=$3

    # 生成唯一的录制文件名: node{id}-{stream}.mp4
    local record_path="record/node${node_id}"
    local record_name="node${node_id}-${stream_path}"

    echo "节点 $node_id 开始录制流 $stream_path -> $record_path/$record_name.mp4"

    local response=$(curl -s -X POST "http://localhost:$http_port/mp4/api/start/$stream_path" \
        -H "Content-Type: application/json" \
        -d "{
            \"fragment\": \"120s\",
            \"filePath\": \"$record_path-$node_id\",
            \"fileName\": \"$record_name.mp4\"
        }")

    echo "录制响应: $response"
}

# 停止 MP4 录制
# 参数: node_id http_port stream_path
stop_recording() {
    local node_id=$1
    local http_port=$2
    local stream_path=$3

    echo "节点 $node_id 停止录制流 $stream_path"

    local response=$(curl -s --location --request POST "http://localhost:$http_port/mp4/api/stop/$stream_path")

    echo "停止录制响应: $response"
}

# 测试播放流
# 参数: node_id rtsp_port stream_path
test_playback() {
    local node_id=$1
    local rtsp_port=$2
    local stream_path=$3

    local rtsp_url="rtsp://localhost:$rtsp_port/$stream_path"

    echo "节点 $node_id 测试播放流 $stream_path (URL: $rtsp_url)"

    # 使用 ffprobe 探测流信息
    # -rtsp_transport tcp: 使用 TCP 传输
    # -v quiet: 安静模式，不输出日志
    # -print_format json: 输出 JSON 格式
    # -show_streams: 显示流信息
    # -timeout 10000000: 设置超时时间（10秒，单位是微秒）
    # 使用 || exit_code=$? 来捕获退出码，避免触发 set -e
    local exit_code=0
    ffprobe -rtsp_transport tcp -timeout 10000000 -v quiet -print_format json -show_streams -i "$rtsp_url" > /dev/null 2>&1 || exit_code=$?

    if [ $exit_code -eq 0 ]; then
        echo "  ✓ 节点 $node_id 播放流 $stream_path 成功"
    else
        echo "  ✗ 节点 $node_id 播放流 $stream_path 失败 (退出码: $exit_code)"
    fi

    # 总是返回 0，确保测试失败不会导致脚本退出
    return 0
}

# 注意：节点由用户手动管理，此脚本不负责停止节点

# 等待集群同步完成
wait_cluster_sync() {
    echo ""
    echo "=== 等待集群同步 ==="
    sleep 10
}

# ============================================
# 主流程开始
# ============================================

echo ""
echo "=== Monibuca 集群测试脚本 ==="
echo "注意：请确保所有节点已手动启动"
echo ""

# 等待集群同步
wait_cluster_sync

# 重新添加拉流代理（确保集群已同步）
echo ""
echo "=== 添加拉流代理（集群同步后） ==="
for i in 0 1 2; do
    node="${NODES[$i]}"
    id=$((i + 1))
    IFS='|' read -r config http_port rtsp_port stream_path source_url proxy_name <<< "$node"
    echo "节点 $id 拉流: $stream_path <- $source_url"
    pull_stream $http_port $stream_path "$source_url" $proxy_name
    sleep 1
done

# 等待拉流成功
echo ""
echo "=== 等待拉流成功 ==="
sleep 5

# 测试播放 - 每个节点播放所有流
echo ""
echo "=== 测试播放（每个节点播放所有流） ==="
for i in 0 1 2; do
    node="${NODES[$i]}"
    node_id=$((i + 1))
    IFS='|' read -r config http_port rtsp_port stream_path source_url proxy_name <<< "$node"

    echo "节点 $node_id (RTSP端口 $rtsp_port) 测试播放所有流..."

    # 对每个流都进行播放测试
    for stream in "${STREAM_PATHS[@]}"; do
        test_playback $node_id $rtsp_port $stream
        sleep 0.5
    done

    echo "节点 $node_id 已完成所有流的播放测试"
    sleep 1
done

# 开始录制 - 每个节点对所有5个流都发起录制
echo ""
echo "=== 开始录制（每个节点录制所有流） ==="
for i in 0 1 2; do
    node="${NODES[$i]}"
    node_id=$((i + 1))
    IFS='|' read -r config http_port rtsp_port stream_path source_url proxy_name <<< "$node"

    echo "节点 $node_id (端口 $http_port) 开始录制所有流..."

    # 对每个流都发起录制请求
    for stream in "${STREAM_PATHS[@]}"; do
        start_recording $node_id $http_port $stream
        sleep 0.5
    done

    echo "节点 $node_id 已对所有流发起录制请求"
    sleep 1
done

echo ""
echo "=== 所有录制任务已发起 ==="
echo "当前运行状态:"
for i in 0 1 2; do
    node="${NODES[$i]}"
    id=$((i + 1))
    IFS='|' read -r config http_port rtsp_port stream_path source_url proxy_name <<< "$node"
    echo "  节点 $id: HTTP :$http_port, RTSP :$rtsp_port, 拉取流: $stream_path, 录制所有3个流"
done
echo ""
echo "总计: 3个节点 × 3个流 = 9个录制任务"

# 等待录制2分钟
echo ""
echo "=== 等待录制2分钟 ==="
sleep 120

# 停止所有录制
echo ""
echo "=== 停止所有录制 ==="
for i in 0 1 2; do
    node="${NODES[$i]}"
    node_id=$((i + 1))
    IFS='|' read -r config http_port rtsp_port stream_path source_url proxy_name <<< "$node"

    echo "节点 $node_id (端口 $http_port) 停止录制所有流..."

    # 对每个流都发起停止录制请求
    for stream in "${STREAM_PATHS[@]}"; do
        stop_recording $node_id $http_port $stream
        sleep 0.5
    done

    echo "节点 $node_id 已对所有流发起停止录制请求"
    sleep 1
done

# 等待一段时间让录制文件上传到 S3
echo ""
echo "等待录制文件上传到 S3 (30秒)..."
sleep 30

# 检查 S3 (MinIO) 中的录制文件数量
check_s3_files() {
    echo ""
    echo "=== 检查 MinIO 存储中的录制文件 ==="

    local expected_count=9
    local bucket="vidu-media-bucket"
    local path_prefix="recordings"
    local endpoint="storage-dev.xiding.tech"
    local access_key="xidinguser"
    local secret_key="U2FsdGVkX1/7uyvj0trCzSNFsfDZ66dMSAEZjNlvW1c="

    # 使用 MinIO Client (mc) 检查 recordings/ 路径下的 MP4 文件数量
    local count=$(MC_HOST_myminio="https://${access_key}:${secret_key}@${endpoint}" \
        mc ls "myminio/$bucket/$path_prefix/" --recursive 2>/dev/null | grep "\.mp4$" | wc -l)

    echo "MinIO 存储桶 ($bucket/$path_prefix/) 中的 MP4 文件数量: $count"
    echo "预期文件数量: $expected_count"

    if [ "$count" -eq "$expected_count" ]; then
        echo "✓ 文件数量正确! ($count / $expected_count)"
        return 0
    else
        echo "✗ 文件数量不正确! 当前: $count, 预期: $expected_count"
        return 1
    fi
}

check_s3_files

echo ""
echo "=== 测试完成 ==="
echo "注意：节点仍在运行，请手动停止节点进程"