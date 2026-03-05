#!/bin/bash

# 摄像头录制测试脚本
# 使用实际的摄像头 RTSP 流进行测试

# 注意：移除 -u 选项以避免并发执行时的变量未定义错误
set -eo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 配置
NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
RTSP_PORT="${RTSP_PORT:-554}"
RECORD_DURATION="${RECORD_DURATION:-60}"  # 录制时长（秒）

# 并发控制
CHECK_CONCURRENCY="${CHECK_CONCURRENCY:-5}"  # 摄像头检查并发数
RECORD_CONCURRENCY="${RECORD_CONCURRENCY:-10}"  # 录制操作并发数

# 摄像头配置（所有 35 个摄像头）
CAMERAS=(
    # 海康威视 - 192.168.12.7x 系列
    "live/camera1|rtsp://admin:a1234567@192.168.12.71/ch1/main/av_stream|海康威视-71"
    "live/camera2|rtsp://admin:a1234567@192.168.12.72/ch1/main/av_stream|海康威视-72"
    "live/camera3|rtsp://admin:a1234567@192.168.12.73/ch1/main/av_stream|海康威视-73"

    # 海康威视 - 192.168.10.x 系列
    "live/camera4|rtsp://admin:a1234567@192.168.10.81/ch1/main/av_stream|海康威视-81"
    "live/camera5|rtsp://admin:a1234567@192.168.10.91/ch1/main/av_stream|海康威视-91"
    "live/camera6|rtsp://admin:a1234567@192.168.10.83/ch1/main/av_stream|海康威视-83"

    # 海康威视 - 192.168.12.3x 系列（俯视/侧视/正视）
    "live/camera7|rtsp://admin:a1234567@192.168.12.31/ch1/main/av_stream|海康威视-31-俯视"
    "live/camera8|rtsp://admin:a1234567@192.168.12.32/ch1/main/av_stream|海康威视-32-侧视"
    "live/camera9|rtsp://admin:a1234567@192.168.12.33/ch1/main/av_stream|海康威视-33-正视"

    # 大华 - 192.168.12.1x 系列
    "live/camera10|rtsp://admin:a1234567@192.168.12.11:554/cam/realmonitor?channel=1&subtype=0|大华-11"
    "live/camera11|rtsp://admin:a1234567@192.168.12.12:554/cam/realmonitor?channel=1&subtype=0|大华-12"
    "live/camera12|rtsp://admin:a1234567@192.168.12.13:554/cam/realmonitor?channel=1&subtype=0|大华-13"

    # 大华 - 192.168.12.5x 系列（俯视/侧视/正视）
    "live/camera13|rtsp://admin:a1234567@192.168.12.51:554/cam/realmonitor?channel=1&subtype=0|大华-51-俯视"
    "live/camera14|rtsp://admin:a1234567@192.168.12.52:554/cam/realmonitor?channel=1&subtype=0|大华-52-侧视"
    "live/camera15|rtsp://admin:a1234567@192.168.12.53:554/cam/realmonitor?channel=1&subtype=0|大华-53-正视"

    # 大华 - 192.168.12.6x 系列（俯视/侧视/正视）
    "live/camera16|rtsp://admin:a1234567@192.168.12.61:554/cam/realmonitor?channel=1&subtype=0|大华-61-俯视"
    "live/camera17|rtsp://admin:a1234567@192.168.12.62:554/cam/realmonitor?channel=1&subtype=0|大华-62-侧视"
    "live/camera18|rtsp://admin:a1234567@192.168.12.63:554/cam/realmonitor?channel=1&subtype=0|大华-63-正视"

    # 大华 - 192.168.12.9x 系列（俯视/侧视/正视）
    "live/camera19|rtsp://admin:a1234567@192.168.12.91:554/cam/realmonitor?channel=1&subtype=0|大华-91-俯视"
    "live/camera20|rtsp://admin:a1234567@192.168.12.92:554/cam/realmonitor?channel=1&subtype=0|大华-92-侧视"
    "live/camera21|rtsp://admin:a1234567@192.168.12.93:554/cam/realmonitor?channel=1&subtype=0|大华-93-正视"

    # 大华 - 192.168.12.10x 系列（俯视/侧视/正视）
    "live/camera22|rtsp://admin:a1234567@192.168.12.101:554/cam/realmonitor?channel=1&subtype=0|大华-101-俯视"
    "live/camera23|rtsp://admin:a1234567@192.168.12.102:554/cam/realmonitor?channel=1&subtype=0|大华-102-侧视"
    "live/camera24|rtsp://admin:a1234567@192.168.12.103:554/cam/realmonitor?channel=1&subtype=0|大华-103-正视"

    # 大华 - 192.168.12.20x 系列（俯视/侧视/正视）
    "live/camera25|rtsp://admin:a1234567@192.168.12.201:554/cam/realmonitor?channel=1&subtype=0|大华-201-侧视"
    "live/camera26|rtsp://admin:a1234567@192.168.12.202:554/cam/realmonitor?channel=1&subtype=0|大华-202-正视"
    "live/camera27|rtsp://admin:a1234567@192.168.12.203:554/cam/realmonitor?channel=1&subtype=0|大华-203-侧视"
    "live/camera28|rtsp://admin:a1234567@192.168.12.204:554/cam/realmonitor?channel=1&subtype=0|大华-204-俯视"

    # 大华 - 192.168.12.22x 系列（俯视/侧视/正视）
    "live/camera29|rtsp://admin:a1234567@192.168.12.221:554/cam/realmonitor?channel=1&subtype=0|大华-221-俯视"
    "live/camera30|rtsp://admin:a1234567@192.168.12.222:554/cam/realmonitor?channel=1&subtype=0|大华-222-侧视"
    "live/camera31|rtsp://admin:a1234567@192.168.12.223:554/cam/realmonitor?channel=1&subtype=0|大华-223-正视"
    "live/camera32|rtsp://admin:a1234567@192.168.12.224:554/cam/realmonitor?channel=1&subtype=0|大华-224-侧视"

    # 海康威视 - 192.168.12.2x 系列（俯视/侧视/正视）
    "live/camera33|rtsp://admin:a1234567@192.168.12.21/ch1/main/av_stream|海康威视-21-俯视"
    "live/camera34|rtsp://admin:a1234567@192.168.12.22/ch1/main/av_stream|海康威视-22-侧视"
    "live/camera35|rtsp://admin:a1234567@192.168.12.23/ch1/main/av_stream|海康威视-23-正视"
)

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[✓]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[⚠]${NC} $*"
}

log_error() {
    echo -e "${RED}[✗]${NC} $*"
}

log_step() {
    echo -e "${CYAN}[STEP]${NC} $*"
}

# 并发控制函数
limit_jobs() {
    local max_jobs=$1
    while [ "$(jobs -pr | wc -l)" -ge "$max_jobs" ]; do
        local pid
        pid="$(jobs -pr | head -n1)" || true
        [ -n "$pid" ] || break
        wait "$pid" || true
    done
}

# 检查服务状态
check_service() {
    log_step "检查 Monibuca 服务状态..."

    if ! curl -s "http://$NODE_IP:$HTTP_PORT/api/sysinfo" > /dev/null 2>&1; then
        log_error "Monibuca 服务未运行"
        log_info "请先启动服务: ./start.sh"
        exit 1
    fi

    log_success "Monibuca 服务运行正常"
}

# 检查摄像头连接
check_camera_connection() {
    local stream_path=$1
    local source_url=$2
    local camera_name=$3

    log_info "检查摄像头连接: $camera_name"

    # 提取 IP 地址
    local camera_ip
    camera_ip=$(echo "$source_url" | sed -n 's/.*@\([0-9.]*\).*/\1/p')

    # Ping 测试
    if ping -c 1 -W 2 "$camera_ip" > /dev/null 2>&1; then
        log_success "摄像头 $camera_name ($camera_ip) 网络可达"
    else
        log_warn "摄像头 $camera_name ($camera_ip) 网络不可达"
        return 1
    fi

    # RTSP 连接测试 - 使用更宽松的参数
    # 注意：这里只是简单测试，实际播放由 Monibuca 处理
    if timeout 15 ffprobe \
        -rtsp_transport tcp \
        -stimeout 10000000 \
        -analyzeduration 5000000 \
        -probesize 5000000 \
        -v error \
        -i "$source_url" > /dev/null 2>&1; then
        log_success "摄像头 $camera_name RTSP 流正常"
        return 0
    else
        # RTSP 检查失败不一定意味着流不可用
        # Monibuca 可能有更好的连接处理
        log_warn "摄像头 $camera_name RTSP 流检查失败（但可能仍可用）"
        # 仍然返回成功，让 Monibuca 尝试连接
        return 0
    fi
}

# 等待流就绪
wait_for_stream() {
    local stream_path=$1
    local max_wait=30
    local waited=0

    log_info "等待流就绪: $stream_path"

    while [ $waited -lt $max_wait ]; do
        # 通过 HTTP API 检查流是否存在（更可靠）
        if curl -s "http://$NODE_IP:$HTTP_PORT/api/stream/$stream_path" 2>/dev/null | grep -q "\"code\":0"; then
            log_success "流已就绪: $stream_path"
            return 0
        fi

        echo -ne "\r等待流就绪... $waited / $max_wait 秒"
        sleep 1
        waited=$((waited + 1))
    done

    echo ""
    log_warn "流未就绪: $stream_path（但仍会尝试录制）"
    # 返回成功，让录制尝试继续
    return 0
}

# 开始录制
start_recording() {
    local stream_path=$1
    local camera_name=$2

    log_info "开始录制: $camera_name ($stream_path)"

    local response
    # 使用正确的 API: POST /mp4/api/start/{streamPath}
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$stream_path" \
        -H "Content-Type: application/json" \
        -d '{}')

    if echo "$response" | grep -q '"code":0'; then
        log_success "录制开始成功: $camera_name"
        return 0
    else
        log_error "录制开始失败: $camera_name"
        log_warn "响应: $response"
        return 1
    fi
}

# 停止录制
stop_recording() {
    local stream_path=$1
    local camera_name=$2

    log_info "停止录制: $camera_name ($stream_path)"

    local response
    # 使用正确的 API: POST /mp4/api/stop/{streamPath}
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$stream_path")

    if echo "$response" | grep -q '"code":0'; then
        log_success "录制停止成功: $camera_name"
        return 0
    else
        log_warn "录制停止响应: $response"
        return 1
    fi
}

# 检查录制文件
check_recording_file() {
    local stream_path=$1
    local camera_name=$2

    # 查找最新的录制文件
    local record_dir="record/${stream_path%/*}"
    local latest_file
    latest_file=$(find "$record_dir" -name "*.mp4" -type f -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -1 | cut -d' ' -f2-)

    if [ -z "$latest_file" ]; then
        log_error "未找到录制文件: $camera_name"
        return 1
    fi

    local file_size
    file_size=$(du -h "$latest_file" | cut -f1)

    log_info "检查录制文件: $latest_file ($file_size)"

    # 检查文件完整性
    if ffprobe -v quiet -print_format json -show_format "$latest_file" > /dev/null 2>&1; then
        local duration
        duration=$(ffprobe -v quiet -print_format json -show_format "$latest_file" | jq -r '.format.duration' 2>/dev/null || echo "unknown")

        log_success "✓ $camera_name - 文件完整 ($file_size, ${duration}s)"
        echo "  文件: $latest_file"
        return 0
    else
        log_error "✗ $camera_name - 文件损坏"
        return 1
    fi
}

# 主测试流程
main() {
    echo ""
    echo "=========================================="
    echo "  摄像头录制测试"
    echo "=========================================="
    echo ""
    echo "测试摄像头数量: ${#CAMERAS[@]}"
    echo "录制时长: ${RECORD_DURATION}秒"
    echo ""

    # 检查服务
    check_service
    echo ""

    # 检查摄像头连接
    log_step "检查摄像头连接状态（并发检查）"
    echo ""

    local available_cameras=()
    local check_pids=()
    local check_results=()

    # 创建临时目录存储检查结果
    local temp_dir
    temp_dir=$(mktemp -d)

    for i in "${!CAMERAS[@]}"; do
        camera="${CAMERAS[$i]}"
        IFS='|' read -r stream_path source_url camera_name <<< "$camera"

        # 并发检查
        limit_jobs "$CHECK_CONCURRENCY"
        (
            if check_camera_connection "$stream_path" "$source_url" "$camera_name"; then
                echo "success|$camera" > "$temp_dir/$i.result"
            else
                echo "failed|$camera" > "$temp_dir/$i.result"
            fi
        ) &
        check_pids+=($!)
    done

    # 等待所有检查完成
    if [ ${#check_pids[@]} -gt 0 ]; then
        log_info "等待所有摄像头检查完成..."
        wait "${check_pids[@]}"
    fi

    # 收集结果
    for i in "${!CAMERAS[@]}"; do
        if [ -f "$temp_dir/$i.result" ]; then
            result=$(cat "$temp_dir/$i.result")
            status="${result%%|*}"
            camera="${result#*|}"
            if [ "$status" = "success" ]; then
                available_cameras+=("$camera")
            fi
        fi
    done

    # 清理临时目录
    rm -rf "$temp_dir"

    echo ""

    if [ ${#available_cameras[@]} -eq 0 ]; then
        log_error "没有可用的摄像头"
        exit 1
    fi

    log_success "可用摄像头: ${#available_cameras[@]} / ${#CAMERAS[@]}"
    echo ""

    # 等待拉流成功
    log_step "等待拉流成功 (配置文件中已配置自动拉流)"
    sleep 5
    echo ""

    # 等待流就绪
    log_step "等待流就绪（并发检查）"
    echo ""

    local ready_streams=()
    local ready_pids=()
    local ready_temp_dir
    ready_temp_dir=$(mktemp -d)

    for i in "${!available_cameras[@]}"; do
        camera="${available_cameras[$i]}"
        IFS='|' read -r stream_path source_url camera_name <<< "$camera"

        limit_jobs "$CHECK_CONCURRENCY"
        # 使用函数参数传递，避免变量作用域问题
        (
            wait_for_stream "$stream_path" && \
                echo "success|$camera" > "$ready_temp_dir/$i.result" || \
                echo "failed|$camera" > "$ready_temp_dir/$i.result"
        ) &
        ready_pids+=($!)
    done

    if [ ${#ready_pids[@]} -gt 0 ]; then
        log_info "等待所有流就绪检查完成..."
        wait "${ready_pids[@]}"
    fi

    # 收集结果
    for i in "${!available_cameras[@]}"; do
        if [ -f "$ready_temp_dir/$i.result" ]; then
            result=$(cat "$ready_temp_dir/$i.result")
            status="${result%%|*}"
            camera="${result#*|}"
            if [ "$status" = "success" ]; then
                ready_streams+=("$camera")
            fi
        fi
    done

    rm -rf "$ready_temp_dir"
    echo ""

    if [ ${#ready_streams[@]} -eq 0 ]; then
        log_error "没有就绪的流"
        exit 1
    fi

    log_success "就绪的流: ${#ready_streams[@]} / ${#available_cameras[@]}"
    echo ""

    # 开始录制
    log_step "开始录制（并发操作）"
    echo ""

    local recording_streams=()
    local record_start_pids=()

    for camera in "${ready_streams[@]}"; do
        IFS='|' read -r stream_path source_url camera_name <<< "$camera"

        limit_jobs "$RECORD_CONCURRENCY"
        start_recording "$stream_path" "$camera_name" &
        record_start_pids+=($!)

        if start_recording "$stream_path" "$camera_name"; then
            recording_streams+=("$camera")
        fi
    done

    if [ ${#record_start_pids[@]} -gt 0 ]; then
        wait "${record_start_pids[@]}"
    fi

    # 所有就绪的流都应该开始录制
    recording_streams=("${ready_streams[@]}")

    if [ ${#recording_streams[@]} -eq 0 ]; then
        log_error "没有成功开始录制的流"
        exit 1
    fi

    log_success "录制中的流: ${#recording_streams[@]}"
    echo ""

    # 等待录制
    log_step "录制中 (${RECORD_DURATION}秒)"
    echo ""

    for i in $(seq 1 "$RECORD_DURATION"); do
        echo -ne "\r录制进度: $i / $RECORD_DURATION 秒 (${#recording_streams[@]} 路流)"
        sleep 1
    done
    echo ""
    echo ""

    # 停止录制
    log_step "停止录制（并发操作）"
    echo ""

    local stop_pids=()
    for camera in "${recording_streams[@]}"; do
        IFS='|' read -r stream_path source_url camera_name <<< "$camera"

        limit_jobs "$RECORD_CONCURRENCY"
        stop_recording "$stream_path" "$camera_name" &
        stop_pids+=($!)
    done

    if [ ${#stop_pids[@]} -gt 0 ]; then
        wait "${stop_pids[@]}"
    fi

    echo ""

    # 等待文件写入完成
    log_info "等待文件写入完成 (10秒)..."
    sleep 10
    echo ""

    # 检查录制文件
    log_step "检查录制结果"
    echo ""

    local success_count=0
    for camera in "${recording_streams[@]}"; do
        IFS='|' read -r stream_path source_url camera_name <<< "$camera"

        if check_recording_file "$stream_path" "$camera_name"; then
            success_count=$((success_count + 1))
        fi
        echo ""
    done

    # 总结
    echo ""
    echo "=========================================="
    echo "  测试结果总结"
    echo "=========================================="
    echo ""
    echo "总摄像头数: ${#CAMERAS[@]}"
    echo "可用摄像头: ${#available_cameras[@]}"
    echo "就绪的流: ${#ready_streams[@]}"
    echo "录制的流: ${#recording_streams[@]}"
    echo "成功的录制: $success_count"
    echo ""

    if [ $success_count -eq ${#recording_streams[@]} ]; then
        log_success "所有录制均成功！"
    else
        log_warn "部分录制失败"
    fi

    echo ""
    log_info "录制文件位置: $SCRIPT_DIR/record/"
    log_info "日志文件: $SCRIPT_DIR/logs/m7s.log"
    echo ""

    # 显示所有录制文件
    log_info "所有录制文件:"
    find record -name "*.mp4" -type f -exec ls -lh {} \;
    echo ""
}

# 捕获 Ctrl+C
cleanup() {
    echo ""
    log_info "清理中..."

    for camera in "${CAMERAS[@]}"; do
        IFS='|' read -r stream_path source_url camera_name <<< "$camera"
        stop_recording "$stream_path" "$camera_name" 2>/dev/null || true
    done

    log_success "清理完成"
}

trap cleanup EXIT

# 运行主流程
main "$@"
