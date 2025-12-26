#!/bin/bash

# Monibuca 集群测试脚本（6 节点，每节点拉 3 路）
# 为 6 个指定节点添加多路拉流代理，测试播放与录制

set -euo pipefail

# 并发控制：拉流请求默认 10 并发（6 个节点会同时发起）
PULL_CONCURRENCY="${PULL_CONCURRENCY:-10}"
# 录制请求并发
RECORD_CONCURRENCY="${RECORD_CONCURRENCY:-5}"

limit_jobs() {
    local max_jobs=$1
    while [ "$(jobs -pr | wc -l)" -ge "$max_jobs" ]; do
        local pid
        pid="$(jobs -pr | head -n1)" || true
        [ -n "$pid" ] || break
        wait "$pid" || true
    done
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 每个节点拉流数量
STREAMS_PER_NODE="${STREAMS_PER_NODE:-3}"

# 节点基础配置 (格式: id|ip|http_port|rtsp_port|stream_suffix)
NODE_CONFIGS=(
    "1|172.16.12.132|7080|554|132"
    "2|172.16.12.130|7080|554|130"
    "3|172.16.12.131|7080|554|131"
    "4|172.16.12.51|7080|554|51"
    "5|172.16.12.52|7080|554|52"
    "6|172.16.12.54|7080|554|54"
)

get_source_url() {
    local node_id=$1
    local idx=$2
    local urls=()
    case "$node_id" in
        1) urls=(
            "rtsp://admin:a1234567@192.168.12.71/ch1/main/av_stream"
            "rtsp://admin:a1234567@192.168.12.72/ch1/main/av_stream"
            "rtsp://admin:a1234567@192.168.12.73/ch1/main/av_stream"
        ) ;;
        2) urls=(
            "rtsp://admin:a1234567@192.168.10.111/ch1/main/av_stream"
            "rtsp://admin:a1234567@192.168.10.112/ch1/main/av_stream"
            "rtsp://admin:a1234567@192.168.10.113/ch1/main/av_stream"
        ) ;;
        3) urls=(
            "rtsp://admin:a1234567@192.168.12.31/ch1/main/av_stream"
            "rtsp://admin:a1234567@192.168.12.32/ch1/main/av_stream"
            "rtsp://admin:a1234567@192.168.12.33/ch1/main/av_stream"
        ) ;;
        4) urls=(
            "rtsp://admin:a1234567@192.168.12.11:554/cam/realmonitor?channel=1&subtype=0"
            "rtsp://admin:a1234567@192.168.12.12:554/cam/realmonitor?channel=1&subtype=0"
            "rtsp://admin:a1234567@192.168.12.13:554/cam/realmonitor?channel=1&subtype=0"
        ) ;;
        5) urls=(
            "rtsp://admin:a1234567@192.168.12.61:554/cam/realmonitor?channel=1&subtype=0"
            "rtsp://admin:a1234567@192.168.12.62:554/cam/realmonitor?channel=1&subtype=0"
            "rtsp://admin:a1234567@192.168.12.63:554/cam/realmonitor?channel=1&subtype=0"
        ) ;;
        6) urls=(
            "rtsp://admin:a1234567@192.168.12.91:554/cam/realmonitor?channel=1&subtype=0"
            "rtsp://admin:a1234567@192.168.12.92:554/cam/realmonitor?channel=1&subtype=0"
            "rtsp://admin:a1234567@192.168.12.93:554/cam/realmonitor?channel=1&subtype=0"
        ) ;;
        *) urls=("" "" "");;
    esac
    if [ "$idx" -ge 1 ] && [ "$idx" -le "${#urls[@]}" ]; then
        echo "${urls[$((idx - 1))]}"
    else
        echo ""
    fi
}

# 计算拉流任务与所有流路径
PULL_TASKS=()  # 元素格式: node_id|node_ip|http_port|rtsp_port|stream_path|source_url|proxy_name
STREAM_PATHS=()
NODES=()       # 元素格式: node_id|node_ip|http_port|rtsp_port|stream_suffix
for cfg in "${NODE_CONFIGS[@]}"; do
    IFS='|' read -r node_id node_ip http_port rtsp_port stream_suffix <<< "$cfg"
    NODES+=("$node_id|$node_ip|$http_port|$rtsp_port|$stream_suffix")
    for idx in $(seq 1 "$STREAMS_PER_NODE"); do
        stream_path="live/stream${stream_suffix}-${idx}"
        source_url="$(get_source_url "$node_id" "$idx")"
        proxy_name="proxy${stream_suffix}-${idx}"
        PULL_TASKS+=("$node_id|$node_ip|$http_port|$rtsp_port|$stream_path|$source_url|$proxy_name")
        STREAM_PATHS+=("$stream_path")
    done
done

# 拉取 RTSP 流 (代理方式)
pull_stream() {
    local node_ip=$1
    local http_port=$2
    local stream_path=$3
    local source_url=$4
    local proxy_name=$5

    echo "添加拉流代理: $node_ip:$http_port $stream_path <- $source_url"

    local response
    response=$(curl -s -X POST "http://$node_ip:$http_port/api/proxy/pull/add" \
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
# 参数: node_id node_ip http_port stream_path
start_recording() {
    local node_id=$1
    local node_ip=$2
    local http_port=$3
    local stream_path=$4

    local record_dir="./record/node${node_id}"
    local record_name="node${node_id}-${stream_path}"

    echo "节点 $node_id ($node_ip) 开始录制流 $stream_path -> $record_dir/$record_name.mp4"

    local response
    response=$(curl -s -X POST "http://$node_ip:$http_port/mp4/api/start/$stream_path" \
        -H "Content-Type: application/json" \
        -d "{
            \"fragment\": \"300s\",
            \"filePath\": \"$record_name\",
            \"fileName\": \"$record_name.mp4\"
        }")

    echo "录制响应: $response"
}

# 停止 MP4 录制
# 参数: node_id node_ip http_port stream_path
stop_recording() {
    local node_id=$1
    local node_ip=$2
    local http_port=$3
    local stream_path=$4

    echo "节点 $node_id ($node_ip) 停止录制流 $stream_path"

    local response
    response=$(curl -s --location --request POST "http://$node_ip:$http_port/mp4/api/stop/$stream_path")

    echo "停止录制响应: $response"
}

# 测试播放流
# 参数: node_id node_ip rtsp_port stream_path
test_playback() {
    local node_id=$1
    local node_ip=$2
    local rtsp_port=$3
    local stream_path=$4

    local rtsp_url="rtsp://$node_ip:$rtsp_port/$stream_path"

    echo "节点 $node_id ($node_ip) 测试播放流 $stream_path (URL: $rtsp_url)"

    local exit_code=0
    ffprobe -rtsp_transport tcp -timeout 10000000 -v quiet -print_format json -show_streams -i "$rtsp_url" > /dev/null 2>&1 || exit_code=$?

    if [ $exit_code -eq 0 ]; then
        echo "  ✓ 节点 $node_id 播放流 $stream_path 成功"
    else
        echo "  ✗ 节点 $node_id 播放流 $stream_path 失败 (退出码: $exit_code)"
    fi

    return 0
}

# 等待集群同步完成
wait_cluster_sync() {
    echo ""
    echo "=== 等待集群同步 ==="
    sleep 10
}

# 确认所有拉流 URL 已提供
require_pull_urls() {
    local missing=0
    for task in "${PULL_TASKS[@]}"; do
        IFS='|' read -r node_id _ _ _ _ source_url _ <<< "$task"
        if [ -z "$source_url" ]; then
            echo "ERROR: 节点 $node_id 未配置足够的拉流地址，请在脚本中补全 get_source_url"
            missing=1
        fi
    done
    if [ $missing -ne 0 ]; then
        exit 1
    fi
}

# ============================================
# 主流程开始
# ============================================

echo ""
echo "=== Monibuca 集群测试脚本 ==="
echo "注意：请确保以下 6 个节点已手动启动并可访问 API："
for node in "${NODES[@]}"; do
    IFS='|' read -r node_id node_ip http_port rtsp_port stream_suffix <<< "$node"
    echo "  节点 $node_id @ $node_ip (HTTP:$http_port, RTSP:$rtsp_port) 流前缀: live/stream${stream_suffix}-<1..${STREAMS_PER_NODE}>"
done
echo ""

require_pull_urls

# 等待集群同步
wait_cluster_sync

# 重新添加拉流代理（确保集群已同步）
echo ""
echo "=== 添加拉流代理（集群同步后） ==="
pull_pids=()
for task in "${PULL_TASKS[@]}"; do
    IFS='|' read -r node_id node_ip http_port rtsp_port stream_path source_url proxy_name <<< "$task"
    echo "节点 $node_id 拉流: $stream_path <- $source_url"
    limit_jobs "$PULL_CONCURRENCY"
    pull_stream "$node_ip" "$http_port" "$stream_path" "$source_url" "$proxy_name" &
    pull_pids+=("$!")
done
if [ ${#pull_pids[@]} -gt 0 ]; then
    wait "${pull_pids[@]}"
fi

# 等待拉流成功
echo ""
echo "=== 等待拉流成功 ==="
sleep 5

# 测试播放 - 每个节点播放所有流
echo ""
echo "=== 测试播放（每个节点播放所有流） ==="
play_pids=()
for node in "${NODES[@]}"; do
    IFS='|' read -r node_id node_ip http_port rtsp_port stream_suffix <<< "$node"

    echo "节点 $node_id ($node_ip, RTSP端口 $rtsp_port) 测试播放所有流..."

    for stream in "${STREAM_PATHS[@]}"; do
        limit_jobs "$PULL_CONCURRENCY"
        test_playback "$node_id" "$node_ip" "$rtsp_port" "$stream" &
        play_pids+=("$!")
    done

    echo "节点 $node_id 已完成所有流的播放测试"
done
if [ ${#play_pids[@]} -gt 0 ]; then
    wait "${play_pids[@]}"
fi

# 开始录制 - 每个节点对所有流都发起录制
echo ""
echo "=== 开始录制（每个节点录制所有流） ==="
record_pids=()
for node in "${NODES[@]}"; do
    IFS='|' read -r node_id node_ip http_port rtsp_port stream_suffix <<< "$node"

    echo "节点 $node_id ($node_ip, HTTP端口 $http_port) 开始录制所有流..."

    for stream in "${STREAM_PATHS[@]}"; do
        limit_jobs "$RECORD_CONCURRENCY"
        start_recording "$node_id" "$node_ip" "$http_port" "$stream" &
        record_pids+=("$!")
    done

    echo "节点 $node_id 已对所有流发起录制请求"
done
if [ ${#record_pids[@]} -gt 0 ]; then
    wait "${record_pids[@]}"
fi

echo ""
echo "=== 所有录制任务已发起 ==="
echo "当前运行状态:"
for node in "${NODES[@]}"; do
    IFS='|' read -r node_id node_ip http_port rtsp_port stream_suffix <<< "$node"
    echo "  节点 $node_id ($node_ip): HTTP :$http_port, RTSP :$rtsp_port, 拉取流: live/stream${stream_suffix}-<1..${STREAMS_PER_NODE}>, 录制所有 ${#STREAM_PATHS[@]} 个流"
done
echo ""
echo "总计: ${#NODES[@]} 个节点 × ${#STREAM_PATHS[@]} 个流 = $(( ${#NODES[@]} * ${#STREAM_PATHS[@]} )) 个录制任务"

# 等待录制5分钟
echo ""
echo "=== 等待录制5分钟 ==="
sleep 300

# 停止所有录制
echo ""
echo "=== 停止所有录制 ==="
stop_pids=()
for node in "${NODES[@]}"; do
    IFS='|' read -r node_id node_ip http_port rtsp_port stream_suffix <<< "$node"

    echo "节点 $node_id ($node_ip, 端口 $http_port) 停止录制所有流..."

    for stream in "${STREAM_PATHS[@]}"; do
        limit_jobs "$RECORD_CONCURRENCY"
        stop_recording "$node_id" "$node_ip" "$http_port" "$stream" &
        stop_pids+=("$!")
    done

    echo "节点 $node_id 已对所有流发起停止录制请求"
done
if [ ${#stop_pids[@]} -gt 0 ]; then
    wait "${stop_pids[@]}"
fi

echo ""
echo "等待录制文件上传到 S3 (30秒)..."
sleep 30

# 检查 S3 (MinIO) 中的录制文件数量
check_s3_files() {
    echo ""
    echo "=== 检查 MinIO 存储中的录制文件 ==="

    local expected_count=$(( ${#NODES[@]} * ${#STREAM_PATHS[@]} ))
    local bucket="vidu-media-bucket"
    local path_prefix=""
    local endpoint="storage-uat.xiding.tech"
    local access_key="xidinguser"
    local secret_key="U2FsdGVkX1/7uyvj0trCzSNFsfDZ66dMSAEZjNlvW1c="

    local count
    count=$(MC_HOST_myminio="https://${access_key}:${secret_key}@${endpoint}" \
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
