#!/bin/bash

# 交互式录制测试工具

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
RTSP_PORT="${RTSP_PORT:-554}"

# 颜色
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

show_menu() {
    clear
    echo "=========================================="
    echo "  Monibuca 录制测试工具"
    echo "=========================================="
    echo ""
    echo "服务地址: http://$NODE_IP:$HTTP_PORT"
    echo "RTSP 地址: rtsp://$NODE_IP:$RTSP_PORT"
    echo ""
    echo "1. 检查服务状态"
    echo "2. 添加拉流代理"
    echo "3. 开始录制"
    echo "4. 停止录制"
    echo "5. 查看录制列表"
    echo "6. 查看录制文件"
    echo "7. 测试播放流"
    echo "8. 查看实时日志"
    echo "9. 清理录制文件"
    echo "0. 退出"
    echo ""
    echo -n "请选择操作: "
}

check_service() {
    echo ""
    echo "检查服务状态..."
    echo ""

    if curl -s "http://$NODE_IP:$HTTP_PORT/api/sysinfo" > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC} HTTP 服务运行正常"
    else
        echo -e "${RED}✗${NC} HTTP 服务不可访问"
    fi

    if nc -z "$NODE_IP" "$RTSP_PORT" 2>/dev/null; then
        echo -e "${GREEN}✓${NC} RTSP 服务运行正常"
    else
        echo -e "${RED}✗${NC} RTSP 服务不可访问"
    fi

    if [ -d "record/live" ]; then
        local file_count
        file_count=$(find record/live -name "*.mp4" -type f 2>/dev/null | wc -l)
        echo -e "${GREEN}✓${NC} 录制目录存在 (已有 $file_count 个文件)"
    else
        echo -e "${YELLOW}⚠${NC} 录制目录不存在"
    fi

    echo ""
}

add_pull_proxy() {
    echo ""
    echo "添加拉流代理"
    echo ""
    echo -n "流路径 (例: live/test1): "
    read -r stream_path

    echo -n "RTSP 源地址: "
    read -r source_url

    echo -n "代理名称 (例: proxy-test1): "
    read -r proxy_name

    echo ""
    echo "添加拉流代理..."

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

    echo ""
    echo "响应: $response"
    echo ""
}

start_recording() {
    echo ""
    echo "开始录制"
    echo ""
    echo -n "流路径 (例: live/test1): "
    read -r stream_path

    echo ""
    echo "开始录制 $stream_path ..."

    local response
    # 使用正确的 API: POST /mp4/api/start/{streamPath}
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$stream_path" \
        -H "Content-Type: application/json" \
        -d '{}')

    echo ""
    echo "响应: $response"
    echo ""
}

stop_recording() {
    echo ""
    echo "停止录制"
    echo ""
    echo -n "流路径 (例: live/test1): "
    read -r stream_path

    echo ""
    echo "停止录制 $stream_path ..."

    local response
    # 使用正确的 API: POST /mp4/api/stop/{streamPath}
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$stream_path")

    echo ""
    echo "响应: $response"
    echo ""
}

list_recordings() {
    echo ""
    echo "录制列表:"
    echo ""

    curl -s "http://$NODE_IP:$HTTP_PORT/mp4/api/List" | jq '.' 2>/dev/null || \
        curl -s "http://$NODE_IP:$HTTP_PORT/mp4/api/List"

    echo ""
}

show_files() {
    echo ""
    echo "录制文件:"
    echo ""

    if [ -d "record/live" ]; then
        find record/live -name "*.mp4" -type f -exec ls -lh {} \; 2>/dev/null || \
            echo "暂无录制文件"
    else
        echo "录制目录不存在"
    fi

    echo ""
}

test_playback() {
    echo ""
    echo "测试播放流"
    echo ""
    echo -n "流路径 (例: live/test1): "
    read -r stream_path

    local rtsp_url="rtsp://$NODE_IP:$RTSP_PORT/$stream_path"

    echo ""
    echo "测试播放: $rtsp_url"
    echo ""

    if timeout 10 ffprobe -rtsp_transport tcp -v quiet -print_format json -show_streams -i "$rtsp_url" 2>&1; then
        echo ""
        echo -e "${GREEN}✓${NC} 播放测试成功"
    else
        echo ""
        echo -e "${RED}✗${NC} 播放测试失败"
    fi

    echo ""
}

show_logs() {
    echo ""
    echo "实时日志 (Ctrl+C 退出):"
    echo ""

    if [ -f "logs/m7s.log" ]; then
        tail -f logs/m7s.log
    else
        echo "日志文件不存在"
    fi
}

cleanup_files() {
    echo ""
    echo "清理录制文件"
    echo ""
    echo -n "确认删除所有录制文件? (y/N): "
    read -r confirm

    if [ "$confirm" = "y" ] || [ "$confirm" = "Y" ]; then
        rm -rf record/live/*.mp4
        echo ""
        echo -e "${GREEN}✓${NC} 录制文件已清理"
    else
        echo ""
        echo "取消清理"
    fi

    echo ""
}

# 主循环
while true; do
    show_menu
    read -r choice

    case $choice in
        1) check_service ;;
        2) add_pull_proxy ;;
        3) start_recording ;;
        4) stop_recording ;;
        5) list_recordings ;;
        6) show_files ;;
        7) test_playback ;;
        8) show_logs ;;
        9) cleanup_files ;;
        0) echo "退出"; exit 0 ;;
        *) echo "无效选择" ;;
    esac

    echo ""
    echo -n "按 Enter 继续..."
    read -r
done
