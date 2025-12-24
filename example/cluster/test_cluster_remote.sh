#!/bin/bash

# Monibuca 集群批量摄像头拉流录制脚本
# 自动扫描网段内的摄像头并进行拉流录制

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ============================================
# 默认配置
# ============================================

# 网段配置
NETWORK_PREFIX="${NETWORK_PREFIX:-192.168.10}"  # 网段前缀，如 192.168.10
IP_START="${IP_START:-1}"                        # 起始IP
IP_END="${IP_END:-254}"                          # 结束IP
IP_SUFFIXES=(3 4 5 7 8 9)                        # 摄像头IP结尾

# 摄像头配置
CAMERA_USER="admin"
CAMERA_PASS="a1234567"
CAMERA_RTSP_PATH="/ch1/main/av_stream"

# 节点配置（默认使用localhost）
NODE_IP="${NODE_IP:-localhost}"
NODE_HTTP_PORT="${NODE_HTTP_PORT:-8081}"
NODE_RTSP_PORT="${NODE_RTSP_PORT:-5541}"

# 录制配置
RECORDING_DURATION="${RECORDING_DURATION:-120}"  # 录制时长（秒）
UPLOAD_WAIT_TIME="${UPLOAD_WAIT_TIME:-30}"      # 等待上传时间（秒）
FRAGMENT_DURATION="${FRAGMENT_DURATION:-120s}"   # 分片时长
SKIP_PULL="${SKIP_PULL:-false}"                  # 跳过拉流，只启动录制

# ============================================
# 辅助函数
# ============================================

# 显示使用说明
show_usage() {
    cat << EOF
用法: $0 [选项]

选项:
  -h, --help                显示此帮助信息
  -n, --network PREFIX      网段前缀（默认: 192.168.10）
  -s, --start NUM           起始IP（默认: 1）
  -e, --end NUM             结束IP（默认: 254）
  -d, --duration SECONDS    录制时长（默认: 120秒）
  -w, --wait SECONDS        等待上传时间（默认: 30秒）
  -f, --fragment DURATION   分片时长（默认: 120s）
  --skip-pull               跳过拉流步骤，只启动录制（假设流已存在）
  -i, --node-ip IP          节点IP地址（默认: localhost）
  -p, --node-http-port PORT 节点HTTP端口（默认: 8081）
  -r, --node-rtsp-port PORT 节点RTSP端口（默认: 5541）
  --camera-user USER        摄像头用户名（默认: admin）
  --camera-pass PASS        摄像头密码（默认: a1234567）
  --camera-path PATH        摄像头RTSP路径（默认: /ch1/main/av_stream）

环境变量配置节点:
  NODE_IP                   节点IP地址（默认: localhost）
  NODE_HTTP_PORT            节点HTTP端口（默认: 8081）
  NODE_RTSP_PORT            节点RTSP端口（默认: 5541）

示例:
  # 扫描 192.168.10.x 网段，录制120秒
  $0 -n 192.168.10 -d 120

  # 扫描 192.168.12.x 网段，只扫描 1-100，录制60秒
  $0 -n 192.168.12 -s 1 -e 100 -d 60

  # 使用远程节点（通过命令行参数）
  $0 -n 192.168.10 -d 60 -i 192.168.1.101 -p 8081 -r 5541

  # 使用远程节点（通过环境变量）
  NODE_IP=192.168.1.101 NODE_HTTP_PORT=8081 NODE_RTSP_PORT=5541 \\
  $0 -n 192.168.10 -d 60

摄像头IP规则:
  只选择IP地址最后一位为 3, 4, 5, 7, 8, 9 的摄像头
  例如: 192.168.10.3, 192.168.10.4, 192.168.10.5, 192.168.10.7, 等

EOF
}

# 解析命令行参数
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            -h|--help)
                show_usage
                exit 0
                ;;
            -n|--network)
                NETWORK_PREFIX="$2"
                shift 2
                ;;
            -s|--start)
                IP_START="$2"
                shift 2
                ;;
            -e|--end)
                IP_END="$2"
                shift 2
                ;;
            -d|--duration)
                RECORDING_DURATION="$2"
                shift 2
                ;;
            -w|--wait)
                UPLOAD_WAIT_TIME="$2"
                shift 2
                ;;
            -f|--fragment)
                FRAGMENT_DURATION="$2"
                shift 2
                ;;
            --skip-pull)
                SKIP_PULL="true"
                shift
                ;;
            -i|--node-ip)
                NODE_IP="$2"
                shift 2
                ;;
            -p|--node-http-port)
                NODE_HTTP_PORT="$2"
                shift 2
                ;;
            -r|--node-rtsp-port)
                NODE_RTSP_PORT="$2"
                shift 2
                ;;
            --camera-user)
                CAMERA_USER="$2"
                shift 2
                ;;
            --camera-pass)
                CAMERA_PASS="$2"
                shift 2
                ;;
            --camera-path)
                CAMERA_RTSP_PATH="$2"
                shift 2
                ;;
            *)
                echo "未知选项: $1"
                show_usage
                exit 1
                ;;
        esac
    done
}

# 生成摄像头IP列表
generate_camera_ips() {
    local cameras=()

    for ip_last in $(seq $IP_START $IP_END); do
        # 检查IP最后一位是否在允许的列表中
        for suffix in "${IP_SUFFIXES[@]}"; do
            if [ $((ip_last % 10)) -eq $suffix ]; then
                cameras+=("${NETWORK_PREFIX}.${ip_last}")
                break
            fi
        done
    done

    echo "${cameras[@]}"
}

# 拉取 RTSP 流
pull_stream() {
    local node_ip=$1
    local http_port=$2
    local stream_path=$3
    local source_url=$4
    local proxy_name=$5

    echo "  添加拉流代理: $stream_path <- $source_url"

    local response=$(curl -s -X POST "http://$node_ip:$http_port/api/proxy/pull/add" \
        -H "Content-Type: application/json" \
        -d "{
            \"parentID\": 0,
            \"name\": \"$proxy_name\",
            \"type\": \"rtsp\",
            \"streamPath\": \"$stream_path\",
            \"pullOnStart\": true,
            \"pullURL\": \"$source_url\"
        }" 2>&1)

    if echo "$response" | grep -q "success\|Success\|ok\|OK" 2>/dev/null; then
        echo "    ✓ 成功"
    else
        echo "    响应: $response"
    fi
}

# 开始 MP4 录制
start_recording() {
    local node_id=$1
    local node_ip=$2
    local http_port=$3
    local stream_path=$4

    
    local record_name=$(echo "$stream_path" | tr '/' '-')
    local record_path="record/node${node_id}/$record_name"

    echo "  开始录制: $stream_path -> $record_path/$record_name.mp4"

    local response=$(curl -s -X POST "http://$node_ip:$http_port/mp4/api/start/$stream_path" \
        -H "Content-Type: application/json" \
        -d "{
            \"fragment\": \"$FRAGMENT_DURATION\",
            \"filePath\": \"$record_path\",
            \"fileName\": \"$record_name.mp4\"
        }" 2>&1)

    if echo "$response" | grep -q "success\|Success\|ok\|OK" 2>/dev/null; then
        echo "    ✓ 成功"
    else
        echo "    响应: $response"
    fi
}

# 停止 MP4 录制
stop_recording() {
    local node_id=$1
    local node_ip=$2
    local http_port=$3
    local stream_path=$4

    echo "  停止录制: $stream_path"

    local response=$(curl -s --location --request POST "http://$node_ip:$http_port/mp4/api/stop/$stream_path" 2>&1)

    if echo "$response" | grep -q "success\|Success\|ok\|OK" 2>/dev/null; then
        echo "    ✓ 成功"
    else
        echo "    响应: $response"
    fi
}

# 测试节点连接
test_node_connection() {
    local node_id=$1
    local node_ip=$2
    local http_port=$3

    echo -n "  测试节点 $node_id ($node_ip:$http_port) 连接... "

    if curl -s --connect-timeout 5 "http://$node_ip:$http_port/api/version" > /dev/null 2>&1; then
        echo "✓ 成功"
        return 0
    else
        echo "✗ 失败"
        return 1
    fi
}

# ============================================
# 主流程
# ============================================

# 解析命令行参数
parse_args "$@"

echo ""
echo "=== Monibuca 批量摄像头拉流录制脚本 ==="
echo ""
echo "配置信息:"
echo "  网段: ${NETWORK_PREFIX}.${IP_START}-${IP_END}"
echo "  摄像头IP规则: 结尾为 ${IP_SUFFIXES[*]}"
echo "  录制时长: ${RECORDING_DURATION}秒"
echo "  分片时长: ${FRAGMENT_DURATION}"
echo "  上传等待: ${UPLOAD_WAIT_TIME}秒"
echo ""

echo "节点配置:"
echo "  节点: $NODE_IP, HTTP :$NODE_HTTP_PORT, RTSP :$NODE_RTSP_PORT"
echo ""

# 测试节点连接
echo "测试节点连接:"
if ! test_node_connection 1 "$NODE_IP" $NODE_HTTP_PORT; then
    echo ""
    echo "错误: 节点无法连接，请检查节点是否已启动"
    exit 1
fi

echo ""

# 生成摄像头IP列表
echo "=== 生成摄像头列表 ==="
camera_ips=($(generate_camera_ips))
camera_count=${#camera_ips[@]}

echo "找到 $camera_count 个摄像头:"
for ip in "${camera_ips[@]}"; do
    echo "  - $ip"
done
echo ""

if [ $camera_count -eq 0 ]; then
    echo "错误: 未找到符合条件的摄像头IP"
    exit 1
fi

# 等待准备
echo "=== 等待准备 ==="
sleep 3

# 添加拉流代理（如果未跳过）
if [ "$SKIP_PULL" = "false" ]; then
    echo ""
    echo "=== 添加拉流代理 ==="
    echo "节点 ($NODE_IP:$NODE_HTTP_PORT):"

    for cam_ip in "${camera_ips[@]}"; do
        # 生成流路径和代理名称
        stream_path="live/camera_${cam_ip//./_}"  # 将 . 替换为 _
        proxy_name="proxy_${cam_ip//./_}"
        source_url="rtsp://${CAMERA_USER}:${CAMERA_PASS}@${cam_ip}${CAMERA_RTSP_PATH}"

        pull_stream "$NODE_IP" $NODE_HTTP_PORT "$stream_path" "$source_url" "$proxy_name"
        sleep 0.5
    done

    echo ""

    # 等待拉流成功
    echo "=== 等待拉流成功 ==="
    sleep 5
else
    echo ""
    echo "=== 跳过拉流步骤 ==="
    echo "假设流已存在，直接启动录制"
    echo ""
fi

# 开始录制
echo ""
echo "=== 开始录制 ==="
echo "节点 ($NODE_IP:$NODE_HTTP_PORT):"

total_recordings=0
for cam_ip in "${camera_ips[@]}"; do
    stream_path="live/camera_${cam_ip//./_}"
    start_recording 1 "$NODE_IP" $NODE_HTTP_PORT "$stream_path"
    total_recordings=$((total_recordings + 1))
    sleep 0.5
done

echo ""
echo "=== 所有录制任务已发起 ==="
echo "总计: $total_recordings 个录制任务"
echo ""

# 等待录制
echo "=== 等待录制 ${RECORDING_DURATION}秒 ==="
echo "开始时间: $(date '+%Y-%m-%d %H:%M:%S')"

# 显示倒计时
remaining=$RECORDING_DURATION
while [ $remaining -gt 0 ]; do
    if [ $remaining -le 10 ] || [ $((remaining % 30)) -eq 0 ]; then
        echo "  剩余时间: ${remaining}秒"
    fi
    sleep 1
    remaining=$((remaining - 1))
done

echo "结束时间: $(date '+%Y-%m-%d %H:%M:%S')"
echo ""

# 停止所有录制
echo "=== 停止所有录制 ==="
echo "节点 ($NODE_IP:$NODE_HTTP_PORT):"

for cam_ip in "${camera_ips[@]}"; do
    stream_path="live/camera_${cam_ip//./_}"
    stop_recording 1 "$NODE_IP" $NODE_HTTP_PORT "$stream_path"
    sleep 0.5
done

echo ""

# 等待文件上传
echo "=== 等待录制文件处理和上传 (${UPLOAD_WAIT_TIME}秒) ==="
sleep $UPLOAD_WAIT_TIME

echo ""
echo "=== 测试完成 ==="
echo "摄像头总数: $camera_count"
echo "录制任务数: $total_recordings"
echo "录制时长: ${RECORDING_DURATION}秒"
echo ""
echo "录制文件位置: record/node1/"
echo ""
