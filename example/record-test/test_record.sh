#!/bin/bash

# 单节点录制测试脚本
# 基于 cluster 测试脚本简化而来

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 配置
NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
RTSP_PORT="${RTSP_PORT:-554}"

# 测试流配置（可通过环境变量覆盖）
STREAM_COUNT="${STREAM_COUNT:-3}"  # 测试流数量
RECORD_DURATION="${RECORD_DURATION:-60}"  # 录制时长（秒）

# RTSP 源地址（需要用户提供）
# 格式: RTSP_SOURCES=("rtsp://source1" "rtsp://source2" "rtsp://source3")
RTSP_SOURCES=()

# 从环境变量或参数读取 RTSP 源
if [ -n "${RTSP_SOURCE_1:-}" ]; then
    RTSP_SOURCES+=("$RTSP_SOURCE_1")
fi
if [ -n "${RTSP_SOURCE_2:-}" ]; then
    RTSP_SOURCES+=("$RTSP_SOURCE_2")
fi
if [ -n "${RTSP_SOURCE_3:-}" ]; then
    RTSP_SOURCES+=("$RTSP_SOURCE_3")
fi

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*"
}

# 检查服务状态
check_service() {
    log_info "检查 Monibuca 服务状态..."

    if ! curl -s "http://$NODE_IP:$HTTP_PORT/api/sysinfo" > /dev/null 2>&1; then
        log_error "Monibuca 服务未运行或不可访问"
        log_info "请先启动服务: go run -tags sqlite main.go"
        exit 1
    fi

    log_success "Monibuca 服务运行正常"
}

# 添加拉流代理
add_pull_proxy() {
    local stream_path=$1
    local source_url=$2
    local proxy_name=$3

    log_info "添加拉流代理: $stream_path <- $source_url"

    local response
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/api/proxy/pull/add" \
        -H "Content-Type: application/json" \
        -d "{
            \"parentID\": 0,
            \"name\": \"$proxy_name\",
            \"type\": \"rtsp\",
            \"streamPath\": \"$stream_path\",
            \"pullOnStart\": true,
            \"pullURL\": \"$source_url\"
        }")

    if echo "$response" | grep -q '"code":0'; then
        log_success "拉流代理添加成功: $stream_path"
    else
        log_warn "拉流代理添加响应: $response"
    fi
}

# 开始录制
start_recording() {
    local stream_path=$1

    log_info "开始录制: $stream_path"

    local response
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$stream_path" \
        -H "Content-Type: application/json" \
        -d '{}')

    if echo "$response" | grep -q '"code":0'; then
        log_success "录制开始成功: $stream_path"
    else
        log_warn "录制开始响应: $response"
    fi
}

# 停止录制
stop_recording() {
    local stream_path=$1

    log_info "停止录制: $stream_path"

    local response
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$stream_path")

    if echo "$response" | grep -q '"code":0'; then
        log_success "录制停止成功: $stream_path"
    else
        log_warn "录制停止响应: $response"
    fi
}

# 测试播放
test_playback() {
    local stream_path=$1

    local rtsp_url="rtsp://$NODE_IP:$RTSP_PORT/$stream_path"

    log_info "测试播放: $rtsp_url"

    if timeout 10 ffprobe -rtsp_transport tcp -v quiet -print_format json -show_streams -i "$rtsp_url" > /dev/null 2>&1; then
        log_success "播放测试成功: $stream_path"
        return 0
    else
        log_error "播放测试失败: $stream_path"
        return 1
    fi
}

# 检查录制文件
check_recording_files() {
    log_info "检查录制文件..."

    if [ ! -d "record/live" ]; then
        log_error "录制目录不存在: record/live"
        return 1
    fi

    local file_count
    file_count=$(find record/live -name "*.mp4" -type f 2>/dev/null | wc -l)

    log_info "找到 $file_count 个录制文件"

    if [ "$file_count" -gt 0 ]; then
        log_success "录制文件列表:"
        find record/live -name "*.mp4" -type f -exec ls -lh {} \;

        # 检查文件是否可播放
        log_info "检查文件完整性..."
        for file in record/live/*.mp4; do
            if [ -f "$file" ]; then
                if ffprobe -v quiet -print_format json -show_format "$file" > /dev/null 2>&1; then
                    local size
                    size=$(du -h "$file" | cut -f1)
                    log_success "✓ $file ($size) - 文件完整"
                else
                    log_error "✗ $file - 文件损坏或不完整"
                fi
            fi
        done
    else
        log_warn "未找到录制文件"
    fi
}

# 清理环境
cleanup() {
    log_info "清理测试环境..."

    # 停止所有录制
    for i in $(seq 1 "$STREAM_COUNT"); do
        stream_path="live/test$i"
        stop_recording "$stream_path" 2>/dev/null || true
    done

    log_success "清理完成"
}

# 主测试流程
main() {
    echo ""
    echo "=========================================="
    echo "  Monibuca 单节点录制测试"
    echo "=========================================="
    echo ""

    # 检查服务
    check_service

    # 检查 RTSP 源
    if [ ${#RTSP_SOURCES[@]} -eq 0 ]; then
        log_warn "未配置 RTSP 源地址"
        log_info "请通过以下方式提供 RTSP 源:"
        log_info "  方式1: 环境变量"
        log_info "    export RTSP_SOURCE_1='rtsp://your-source-1'"
        log_info "    export RTSP_SOURCE_2='rtsp://your-source-2'"
        log_info "    ./test_record.sh"
        log_info ""
        log_info "  方式2: 修改 config.yaml 中的 rtsp.pull 配置"
        log_info ""
        log_info "跳过拉流测试，仅测试手动推流录制..."
        echo ""
    else
        log_info "配置了 ${#RTSP_SOURCES[@]} 个 RTSP 源"

        # 添加拉流代理
        echo ""
        log_info "=== 添加拉流代理 ==="
        for i in "${!RTSP_SOURCES[@]}"; do
            stream_num=$((i + 1))
            stream_path="live/test$stream_num"
            source_url="${RTSP_SOURCES[$i]}"
            proxy_name="proxy-test$stream_num"

            add_pull_proxy "$stream_path" "$source_url" "$proxy_name"
        done

        # 等待拉流成功
        log_info "等待拉流成功 (5秒)..."
        sleep 5

        # 测试播放
        echo ""
        log_info "=== 测试播放 ==="
        for i in $(seq 1 "${#RTSP_SOURCES[@]}"); do
            stream_path="live/test$i"
            test_playback "$stream_path" || true
        done
    fi

    # 开始录制
    echo ""
    log_info "=== 开始录制 ==="

    if [ ${#RTSP_SOURCES[@]} -gt 0 ]; then
        # 录制拉流的流
        for i in $(seq 1 "${#RTSP_SOURCES[@]}"); do
            stream_path="live/test$i"
            start_recording "$stream_path"
        done
    else
        log_info "等待手动推流..."
        log_info "推流地址示例:"
        log_info "  RTSP: rtsp://$NODE_IP:$RTSP_PORT/live/test1"
        log_info "  RTMP: rtmp://$NODE_IP:1935/live/test1"
        log_info ""
        log_info "推流后，录制将自动开始（根据 onpub 配置）"
        log_info ""
        log_info "或者手动开始录制:"
        log_info "  curl -X POST 'http://$NODE_IP:$HTTP_PORT/mp4/api/StartRecord?streamPath=live/test1'"
    fi

    # 等待录制
    echo ""
    log_info "=== 录制中 (${RECORD_DURATION}秒) ==="
    log_info "按 Ctrl+C 可提前停止"

    for i in $(seq 1 "$RECORD_DURATION"); do
        echo -ne "\r录制进度: $i / $RECORD_DURATION 秒"
        sleep 1
    done
    echo ""

    # 停止录制
    echo ""
    log_info "=== 停止录制 ==="

    if [ ${#RTSP_SOURCES[@]} -gt 0 ]; then
        for i in $(seq 1 "${#RTSP_SOURCES[@]}"); do
            stream_path="live/test$i"
            stop_recording "$stream_path"
        done
    fi

    # 等待文件写入完成
    log_info "等待文件写入完成 (10秒)..."
    sleep 10

    # 检查录制文件
    echo ""
    log_info "=== 检查录制结果 ==="
    check_recording_files

    echo ""
    log_success "=== 测试完成 ==="
    echo ""
    log_info "录制文件位置: $SCRIPT_DIR/record/live/"
    log_info "日志文件位置: $SCRIPT_DIR/logs/"
    log_info "数据库文件: $SCRIPT_DIR/record-test.db"
    echo ""
}

# 捕获 Ctrl+C
trap cleanup EXIT

# 运行主流程
main "$@"
