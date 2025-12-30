#!/bin/bash

# Monibuca 流播放压力测试脚本
# 从服务器获取所有流并使用 ffmpeg 进行并发播放测试

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ============================================
# 默认配置
# ============================================

# 服务器配置
SERVER_IP="${SERVER_IP:-10.24.62.77}"
SERVER_HTTP_PORT="${SERVER_HTTP_PORT:-7080}"
SERVER_RTSP_PORT="${SERVER_RTSP_PORT:-554}"

# 播放配置
PLAY_DURATION="${PLAY_DURATION:-60}"           # 播放时长（秒）
PLAY_CONCURRENCY="${PLAY_CONCURRENCY:-10}"     # 并发播放数量
PLAY_PROTOCOL="${PLAY_PROTOCOL:-rtsp}"         # 播放协议: rtsp, flv, hls
MAX_STREAMS="${MAX_STREAMS:-0}"                # 最大播放流数量（0表示不限制）

# ffmpeg 配置
FFMPEG_LOGLEVEL="${FFMPEG_LOGLEVEL:-quiet}"    # ffmpeg 日志级别: quiet, error, warning, info
RTSP_TRANSPORT="${RTSP_TRANSPORT:-tcp}"        # RTSP 传输协议: tcp, udp

# ============================================
# 辅助函数
# ============================================

# 显示使用说明
show_usage() {
    cat << EOF
用法: $0 [选项]

选项:
  -h, --help                    显示此帮助信息
  -s, --server IP               服务器IP地址（默认: 172.16.12.82）
  -p, --http-port PORT          HTTP端口（默认: 8080）
  -r, --rtsp-port PORT          RTSP端口（默认: 554）
  -d, --duration SECONDS        播放时长（默认: 60秒）
  -c, --concurrency NUM         并发播放数量（默认: 10）
  -m, --max-streams NUM         最大播放流数量（默认: 0，不限制）
  --protocol PROTOCOL           播放协议: rtsp, flv, hls（默认: rtsp）
  --rtsp-transport TRANSPORT    RTSP传输协议: tcp, udp（默认: tcp）
  --loglevel LEVEL              ffmpeg日志级别: quiet, error, warning, info（默认: quiet）

环境变量:
  SERVER_IP                     服务器IP地址
  SERVER_HTTP_PORT              HTTP端口
  SERVER_RTSP_PORT              RTSP端口

示例:
  # 使用默认配置测试
  $0

  # 指定服务器和播放时长
  $0 -s 172.16.12.82 -d 120

  # 高并发测试（20路并发，播放300秒）
  $0 -s 172.16.12.82 -c 20 -d 300

  # 限制最多播放50路流
  $0 -s 172.16.12.82 -m 50

  # 使用FLV协议播放
  $0 -s 172.16.12.82 --protocol flv

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
            -s|--server)
                SERVER_IP="$2"
                shift 2
                ;;
            -p|--http-port)
                SERVER_HTTP_PORT="$2"
                shift 2
                ;;
            -r|--rtsp-port)
                SERVER_RTSP_PORT="$2"
                shift 2
                ;;
            -d|--duration)
                PLAY_DURATION="$2"
                shift 2
                ;;
            -c|--concurrency)
                PLAY_CONCURRENCY="$2"
                shift 2
                ;;
            -m|--max-streams)
                MAX_STREAMS="$2"
                shift 2
                ;;
            --protocol)
                PLAY_PROTOCOL="$2"
                shift 2
                ;;
            --rtsp-transport)
                RTSP_TRANSPORT="$2"
                shift 2
                ;;
            --loglevel)
                FFMPEG_LOGLEVEL="$2"
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

# 并发控制函数 - 基于 PID 数组
limit_jobs() {
    local max_jobs=$1

    # 计算当前运行的进程数
    while true; do
        local running=0
        # 直接使用全局数组 PLAYER_PIDS
        for pid in "${PLAYER_PIDS[@]}"; do
            if [ -n "$pid" ] && [ "$pid" != "failed" ] && kill -0 "$pid" 2>/dev/null; then
                running=$((running + 1))
            fi
        done

        # 如果运行数小于最大并发数，退出循环
        if [ $running -lt $max_jobs ]; then
            break
        fi

        # 等待一小段时间再检查
        sleep 0.2
    done
}

# 测试服务器连接
test_server_connection() {
    local server_ip=$1
    local http_port=$2

    echo -n "测试服务器连接 ($server_ip:$http_port)... "

    if curl -s --connect-timeout 5 "http://$server_ip:$http_port/api/version" > /dev/null 2>&1; then
        echo "✓ 成功"
        return 0
    else
        echo "✗ 失败"
        return 1
    fi
}

# 获取流列表
get_stream_list() {
    local server_ip=$1
    local http_port=$2

    echo "正在获取流列表..."

    # 尝试多个可能的 API 端点
    local api_endpoints=(
        "/rtsp/api/list"         # RTSP 插件 API
    )

    local response=""
    local successful_endpoint=""

    for endpoint in "${api_endpoints[@]}"; do
        echo "尝试 API 端点: $endpoint"
        response=$(curl -s --connect-timeout 10 "http://$server_ip:$http_port$endpoint" 2>&1)

        if [ $? -eq 0 ] && [ -n "$response" ] && [ "$response" != "null" ]; then
            successful_endpoint="$endpoint"
            echo "✓ 成功从 $endpoint 获取响应"
            break
        fi
    done

    if [ -z "$successful_endpoint" ]; then
        echo "错误: 所有 API 端点都无法获取流列表"
        echo "最后响应: $response"
        return 1
    fi

    # 解析 JSON 响应，提取流路径
    local streams=()

    # 方法1: 使用 jq（如果可用）
    if command -v jq &> /dev/null; then
        echo "使用 jq 解析 JSON..."

        # 尝试多种 JSON 结构解析
        # 1. 直接从数组中提取 Path
        streams=($(echo "$response" | jq -r '.[] | .Path // .path // .streamPath // empty' 2>/dev/null | grep -v '^null$' || true))

        # 2. 从嵌套的 data 字段提取
        if [ ${#streams[@]} -eq 0 ]; then
            streams=($(echo "$response" | jq -r '.data[] | .Path // .path // .streamPath // empty' 2>/dev/null | grep -v '^null$' || true))
        fi

        # 3. 从对象的值中提取（用于 map/object 结构）
        if [ ${#streams[@]} -eq 0 ]; then
            streams=($(echo "$response" | jq -r '.[] | .. | .Path? // .path? // .streamPath? // empty' 2>/dev/null | grep -v '^null$' | sort -u || true))
        fi

        # 4. 尝试提取所有看起来像流路径的字符串
        if [ ${#streams[@]} -eq 0 ]; then
            streams=($(echo "$response" | jq -r '.. | strings | select(test("^[a-zA-Z0-9_-]+/[a-zA-Z0-9_/-]+$"))' 2>/dev/null | sort -u || true))
        fi
    fi

    # 方法2: 如果 jq 不可用或没有结果，使用 grep 和 sed
    if [ ${#streams[@]} -eq 0 ]; then
        echo "使用 grep/sed 解析..."
        streams=($(echo "$response" | grep -oE '"(Path|path|streamPath)"\s*:\s*"[^"]+' | sed -E 's/.*"([^"]+)"/\1/' || true))
    fi

    # 方法3: 简单的正则匹配流路径格式
    if [ ${#streams[@]} -eq 0 ]; then
        echo "使用正则表达式匹配流路径..."
        streams=($(echo "$response" | grep -oE '[a-zA-Z0-9_-]+/[a-zA-Z0-9_/-]+' | grep -v '^http' | sort -u || true))
    fi

    if [ ${#streams[@]} -eq 0 ]; then
        echo "警告: 未能从响应中解析出流列表"
        echo "成功的端点: $successful_endpoint"
        echo "原始响应（前500字符）: ${response:0:500}"
        return 1
    fi

    echo "成功解析出 ${#streams[@]} 个流"

    echo "${streams[@]}"
    return 0
}

# 构建播放 URL
build_play_url() {
    local protocol=$1
    local server_ip=$2
    local port=$3
    local stream_path=$4

    case "$protocol" in
        rtsp)
            echo "rtsp://$server_ip:$port/$stream_path"
            ;;
        flv)
            echo "http://$server_ip:$port/$stream_path.flv"
            ;;
        hls)
            echo "http://$server_ip:$port/$stream_path.m3u8"
            ;;
        *)
            echo "rtsp://$server_ip:$port/$stream_path"
            ;;
    esac
}

# 播放单个流
play_stream() {
    local stream_url=$1
    local stream_name=$2
    local log_file=$3

    echo "开始播放: $stream_name ($stream_url)"

    # 将 URL 和命令写入日志文件，方便调试
    {
        echo "========================================"
        echo "流名称: $stream_name"
        echo "播放时间: $(date '+%Y-%m-%d %H:%M:%S')"
        echo "播放协议: $PLAY_PROTOCOL"
        echo "流地址: $stream_url"
        echo "========================================"
        echo ""
    } > "$log_file"

    # ffmpeg 参数说明:
    # -loglevel: 日志级别
    # -rtsp_transport: RTSP 传输协议
    # -timeout: 超时时间（微秒）
    # -analyzeduration: 分析时长（微秒）
    # -probesize: 探测大小（字节）
    # -i: 输入URL
    # -f null: 使用 null muxer (不保存文件)
    # /dev/null: 输出目标

    # 构建 ffmpeg 参数数组（避免 eval 的引号问题）
    local ffmpeg_args=(
        -loglevel error  # 使用 error 级别以捕获更多调试信息
        -analyzeduration 3000000  # 3秒分析时长
        -probesize 1000000        # 1MB 探测大小
    )

    # 根据协议添加特定参数
    if [ "$PLAY_PROTOCOL" = "rtsp" ]; then
        ffmpeg_args+=(
            -rtsp_transport "$RTSP_TRANSPORT"
            -timeout 10000000  # 10秒超时
        )
    fi

    # 添加输入 URL
    ffmpeg_args+=(-i "$stream_url")

    # 添加输出参数 (使用 /dev/null 作为输出目标)
    ffmpeg_args+=(-f null /dev/null)

    # 在后台运行 ffmpeg，将输出重定向到日志文件
    ffmpeg "${ffmpeg_args[@]}" >> "$log_file" 2>&1 &
    local pid=$!

    # 等待一小段时间检查进程是否立即失败
    sleep 0.2

    if ! kill -0 "$pid" 2>/dev/null; then
        echo "  ✗ 播放进程立即退出 (PID: $pid)"
        {
            echo ""
            echo "错误: ffmpeg 进程立即退出"
            echo "退出码: $?"
            echo ""
            echo "完整命令: ffmpeg ${ffmpeg_args[*]}"
        } >> "$log_file"
        echo "failed"
        return 1
    fi

    echo "  ✓ 播放进程已启动 (PID: $pid)"
    echo "$pid"
    return 0
}

# 停止所有播放进程
stop_all_players() {
    local pids=("$@")

    if [ ${#pids[@]} -eq 0 ]; then
        return 0
    fi

    echo ""
    echo "正在停止所有播放进程..."

    local stopped=0
    for pid in "${pids[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            stopped=$((stopped + 1))
        fi
    done

    # 等待进程结束
    sleep 2

    # 强制杀死仍在运行的进程
    for pid in "${pids[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then
            kill -9 "$pid" 2>/dev/null || true
        fi
    done

    echo "已停止 $stopped 个播放进程"
}

# 清理日志文件
cleanup_logs() {
    local log_dir=$1
    if [ -d "$log_dir" ]; then
        rm -rf "$log_dir"
    fi
}

# ============================================
# 主流程
# ============================================

# 解析命令行参数
parse_args "$@"

echo ""
echo "=== Monibuca 流播放压力测试脚本 ==="
echo ""
echo "配置信息:"
echo "  服务器: $SERVER_IP"
echo "  HTTP端口: $SERVER_HTTP_PORT"
echo "  RTSP端口: $SERVER_RTSP_PORT"
echo "  播放协议: $PLAY_PROTOCOL"
echo "  播放时长: ${PLAY_DURATION}秒"
echo "  并发数量: $PLAY_CONCURRENCY"
if [ $MAX_STREAMS -gt 0 ]; then
    echo "  最大流数: $MAX_STREAMS"
else
    echo "  最大流数: 不限制"
fi
echo ""

# 测试服务器连接
if ! test_server_connection "$SERVER_IP" "$SERVER_HTTP_PORT"; then
    echo ""
    echo "错误: 无法连接到服务器，请检查服务器是否已启动"
    exit 1
fi

echo ""

# 获取流列表
stream_list=($(get_stream_list "$SERVER_IP" "$SERVER_HTTP_PORT"))

if [ ${#stream_list[@]} -eq 0 ]; then
    echo ""
    echo "错误: 未找到任何流"
    exit 1
fi

echo ""
echo "找到 ${#stream_list[@]} 个流:"
for stream in "${stream_list[@]}"; do
    echo "  - $stream"
done
echo ""

# 限制流数量
if [ $MAX_STREAMS -gt 0 ] && [ ${#stream_list[@]} -gt $MAX_STREAMS ]; then
    echo "限制播放流数量为 $MAX_STREAMS"
    stream_list=("${stream_list[@]:0:$MAX_STREAMS}")
    echo ""
fi

# 创建日志目录
LOG_DIR="$SCRIPT_DIR/playback_logs"
mkdir -p "$LOG_DIR"

# 确定播放端口
PLAY_PORT=$SERVER_RTSP_PORT
if [ "$PLAY_PROTOCOL" = "flv" ] || [ "$PLAY_PROTOCOL" = "hls" ]; then
    PLAY_PORT=$SERVER_HTTP_PORT
fi

echo "=== 开始播放测试 ==="
echo "开始时间: $(date '+%Y-%m-%d %H:%M:%S')"
echo ""

# 启动播放进程
PLAYER_PIDS=()
FAILED_STREAMS=()
play_count=0
failed_count=0

echo "开始启动播放进程（总共 ${#stream_list[@]} 个流）..."
echo ""

stream_index=0
for stream in "${stream_list[@]}"; do
    stream_index=$((stream_index + 1))

    # 并发控制
    limit_jobs "$PLAY_CONCURRENCY"

    # 显示进度
    echo "[$stream_index/${#stream_list[@]}] 正在处理: $stream"

    # 构建播放 URL
    play_url=$(build_play_url "$PLAY_PROTOCOL" "$SERVER_IP" "$PLAY_PORT" "$stream")

    # 生成日志文件名
    log_file="$LOG_DIR/$(echo "$stream" | tr '/' '_').log"

    # 启动播放
    result=$(play_stream "$play_url" "$stream" "$log_file")

    if [ "$result" = "failed" ]; then
        FAILED_STREAMS+=("$stream")
        failed_count=$((failed_count + 1))
    else
        PLAYER_PIDS+=("$result")
        play_count=$((play_count + 1))
    fi

    sleep 0.1
done

# 等待所有播放进程启动
echo ""
echo "等待所有播放进程启动..."
sleep 3

# 统计实际运行的进程数
running_count=0
stopped_count=0

for pid in "${PLAYER_PIDS[@]}"; do
    if [ "$pid" != "failed" ] && kill -0 "$pid" 2>/dev/null; then
        running_count=$((running_count + 1))
    else
        stopped_count=$((stopped_count + 1))
    fi
done

echo ""
echo "=== 播放状态 ==="
echo "总流数量: ${#stream_list[@]}"
echo "启动成功: $play_count"
echo "启动失败: $failed_count"
echo "当前运行: $running_count"
echo "已停止: $stopped_count"

# 如果有失败的流,显示详细信息
if [ ${#FAILED_STREAMS[@]} -gt 0 ]; then
    echo ""
    echo "=== 启动失败的流 ==="
    for stream in "${FAILED_STREAMS[@]}"; do
        echo "  ✗ $stream"
    done
fi
echo "播放协议: $PLAY_PROTOCOL"
echo "播放时长: ${PLAY_DURATION}秒"
echo ""

# 等待播放
echo "=== 等待播放 ${PLAY_DURATION}秒 ==="

# 显示倒计时和进程状态
remaining=$PLAY_DURATION
while [ $remaining -gt 0 ]; do
    # 每10秒或最后10秒显示一次状态
    if [ $remaining -le 10 ] || [ $((remaining % 10)) -eq 0 ]; then
        # 统计当前运行的进程数
        current_running=0
        for pid in "${PLAYER_PIDS[@]}"; do
            if kill -0 "$pid" 2>/dev/null; then
                current_running=$((current_running + 1))
            fi
        done
        echo "  剩余时间: ${remaining}秒 | 运行进程: $current_running/$play_count"
    fi
    sleep 1
    remaining=$((remaining - 1))
done

echo ""
echo "结束时间: $(date '+%Y-%m-%d %H:%M:%S')"

# 停止所有播放进程
stop_all_players "${PLAYER_PIDS[@]}"

# 统计最终状态
echo ""
echo "=== 测试完成 ==="
echo "总流数量: ${#stream_list[@]}"
echo "播放进程: $play_count"
echo "播放时长: ${PLAY_DURATION}秒"
echo "播放协议: $PLAY_PROTOCOL"
echo ""

# 检查日志文件中的错误
echo "检查播放日志..."
error_count=0
error_streams=()

for log_file in "$LOG_DIR"/*.log; do
    if [ -f "$log_file" ]; then
        if grep -qi "error\|failed\|timeout\|connection refused\|no route to host" "$log_file" 2>/dev/null; then
            error_count=$((error_count + 1))
            stream_name=$(basename "$log_file" .log | tr '_' '/')
            error_streams+=("$stream_name")
        fi
    fi
done

echo ""
echo "=== 最终统计 ==="
echo "总流数量: ${#stream_list[@]}"
echo "启动成功: $play_count"
echo "启动失败: $failed_count"
echo "播放错误: $error_count"
echo ""

if [ $error_count -gt 0 ]; then
    echo "警告: 发现 $error_count 个流播放出现错误"
    echo "详细日志位置: $LOG_DIR/"
    echo ""
    echo "错误流列表:"
    for stream in "${error_streams[@]}"; do
        log_file="$LOG_DIR/$(echo "$stream" | tr '/' '_').log"
        echo "  ✗ $stream"
        # 显示错误日志的前几行
        if [ -f "$log_file" ]; then
            echo "    错误详情:"
            grep -i "error\|failed\|timeout" "$log_file" 2>/dev/null | head -3 | sed 's/^/      /'
        fi
    done
    echo ""
    echo "提示: 查看完整日志请运行: cat $LOG_DIR/<流名称>.log"
else
    echo "✓ 所有流播放正常"
    # 清理日志文件
    cleanup_logs "$LOG_DIR"
fi

echo ""
echo "测试完成！"
echo ""
