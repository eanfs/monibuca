#!/bin/bash

# 简化版摄像头录制测试
# 跳过 ffprobe 检查，直接使用 Monibuca 的拉流和录制功能

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 配置
NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
RECORD_DURATION="${RECORD_DURATION:-60}"

# 摄像头列表（简化为流路径）
STREAM_PATHS=(
    "live/camera1" "live/camera2" "live/camera3"
    "live/camera4" "live/camera5" "live/camera6"
    "live/camera7" "live/camera8" "live/camera9"
    "live/camera10" "live/camera11" "live/camera12"
    "live/camera13" "live/camera14" "live/camera15"
    "live/camera16" "live/camera17" "live/camera18"
    "live/camera19" "live/camera20" "live/camera21"
    "live/camera22" "live/camera23" "live/camera24"
    "live/camera25" "live/camera26" "live/camera27"
    "live/camera28" "live/camera29" "live/camera30"
    "live/camera31" "live/camera32" "live/camera33"
    "live/camera34" "live/camera35"
)

# 颜色
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[✓]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[⚠]${NC} $*"; }
log_error() { echo -e "${RED}[✗]${NC} $*"; }
log_step() { echo -e "${CYAN}[STEP]${NC} $*"; }

# 检查服务
check_service() {
    log_step "检查 Monibuca 服务状态"
    if ! curl -s "http://$NODE_IP:$HTTP_PORT/api/sysinfo" > /dev/null 2>&1; then
        log_error "Monibuca 服务未运行"
        exit 1
    fi
    log_success "Monibuca 服务运行正常"
    echo ""
}

# 等待流就绪
wait_for_streams() {
    log_step "等待流拉取成功（配置文件中已配置自动拉流）"
    log_info "等待 15 秒让 Monibuca 拉取所有流..."

    for i in {1..15}; do
        echo -ne "\r等待中... $i / 15 秒"
        sleep 1
    done
    echo ""

    # 检查有多少流已就绪
    local ready_count=0
    for stream_path in "${STREAM_PATHS[@]}"; do
        if curl -s "http://$NODE_IP:$HTTP_PORT/api/stream/$stream_path" 2>/dev/null | grep -q "\"code\":0"; then
            ready_count=$((ready_count + 1))
        fi
    done

    log_success "检测到 $ready_count / ${#STREAM_PATHS[@]} 个流已就绪"
    echo ""
}

# 开始录制
start_all_recordings() {
    log_step "开始录制所有流"

    local success_count=0
    for stream_path in "${STREAM_PATHS[@]}"; do
        local response
        # 使用正确的 API: POST /mp4/api/start/{streamPath}
        response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$stream_path" \
            -H "Content-Type: application/json" \
            -d '{}')

        if echo "$response" | grep -q '"code":0'; then
            success_count=$((success_count + 1))
            echo -ne "\r  已开始录制: $success_count / ${#STREAM_PATHS[@]}"
        fi
    done

    echo ""
    log_success "成功开始 $success_count 个录制任务"
    echo ""
}

# 停止录制
stop_all_recordings() {
    log_step "停止所有录制"

    local success_count=0
    for stream_path in "${STREAM_PATHS[@]}"; do
        local response
        # 使用正确的 API: POST /mp4/api/stop/{streamPath}
        response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$stream_path")

        if echo "$response" | grep -q '"code":0'; then
            success_count=$((success_count + 1))
            echo -ne "\r  已停止录制: $success_count / ${#STREAM_PATHS[@]}"
        fi
    done

    echo ""
    log_success "成功停止 $success_count 个录制任务"
    echo ""
}

# 检查录制文件
check_recordings() {
    log_step "检查录制结果"
    echo ""

    if [ ! -d "record/live" ]; then
        log_error "录制目录不存在"
        return 1
    fi

    local file_count
    file_count=$(find record/live -name "*.mp4" -type f 2>/dev/null | wc -l)

    log_info "找到 $file_count 个录制文件"
    echo ""

    if [ "$file_count" -gt 0 ]; then
        log_success "录制文件列表:"
        find record/live -name "*.mp4" -type f -exec ls -lh {} \; | head -20

        if [ "$file_count" -gt 20 ]; then
            echo "... (还有 $((file_count - 20)) 个文件)"
        fi

        echo ""

        # 检查文件完整性（抽样检查前 5 个）
        log_info "检查文件完整性（抽样）..."
        local check_count=0
        local valid_count=0

        for file in $(find record/live -name "*.mp4" -type f | head -5); do
            check_count=$((check_count + 1))
            if ffprobe -v quiet -print_format json -show_format "$file" > /dev/null 2>&1; then
                valid_count=$((valid_count + 1))
                local size
                size=$(du -h "$file" | cut -f1)
                log_success "✓ $(basename "$file") ($size) - 文件完整"
            else
                log_error "✗ $(basename "$file") - 文件损坏"
            fi
        done

        echo ""
        log_info "抽样检查: $valid_count / $check_count 个文件完整"
    else
        log_warn "未找到录制文件"
    fi

    echo ""
}

# 主流程
main() {
    echo ""
    echo "=========================================="
    echo "  简化版摄像头录制测试"
    echo "=========================================="
    echo ""
    echo "测试流数量: ${#STREAM_PATHS[@]}"
    echo "录制时长: ${RECORD_DURATION}秒"
    echo ""
    echo "注意: 此版本跳过 ffprobe 检查，直接使用 Monibuca 拉流"
    echo ""

    # 检查服务
    check_service

    # 等待流就绪
    wait_for_streams

    # 开始录制
    start_all_recordings

    # 等待录制
    log_step "录制中 (${RECORD_DURATION}秒)"
    echo ""

    for i in $(seq 1 "$RECORD_DURATION"); do
        echo -ne "\r录制进度: $i / $RECORD_DURATION 秒"
        sleep 1
    done
    echo ""
    echo ""

    # 停止录制
    stop_all_recordings

    # 等待文件写入
    log_info "等待文件写入完成 (10秒)..."
    sleep 10
    echo ""

    # 检查结果
    check_recordings

    # 总结
    echo ""
    echo "=========================================="
    echo "  测试完成"
    echo "=========================================="
    echo ""
    log_info "录制文件位置: $SCRIPT_DIR/record/live/"
    log_info "日志文件: $SCRIPT_DIR/logs/m7s.log"
    echo ""
    log_info "查看详细日志:"
    echo "  tail -100 logs/m7s.log"
    echo ""
    log_info "查看所有录制文件:"
    echo "  find record/live -name '*.mp4' -exec ls -lh {} \\;"
    echo ""
}

# 清理
cleanup() {
    echo ""
    log_info "清理中..."
    stop_all_recordings 2>/dev/null || true
}

trap cleanup EXIT

# 运行
main "$@"
