#!/bin/bash

# 检查所有流的状态

NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"

STREAMS=(
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

GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

echo "=========================================="
echo "  检查所有流状态"
echo "=========================================="
echo ""

ready_count=0
not_ready_count=0

for stream in "${STREAMS[@]}"; do
    response=$(curl -s "http://$NODE_IP:$HTTP_PORT/api/stream/info/$stream" 2>/dev/null)

    if echo "$response" | grep -q '"code":0'; then
        echo -e "${GREEN}✓${NC} $stream - 就绪"
        ready_count=$((ready_count + 1))
    else
        echo -e "${RED}✗${NC} $stream - 未就绪"
        not_ready_count=$((not_ready_count + 1))
    fi
done

echo ""
echo "=========================================="
echo "  统计"
echo "=========================================="
echo ""
echo "总流数: ${#STREAMS[@]}"
echo -e "就绪: ${GREEN}$ready_count${NC}"
echo -e "未就绪: ${RED}$not_ready_count${NC}"
echo ""

if [ $ready_count -eq 0 ]; then
    echo "⚠️  没有流就绪！"
    echo ""
    echo "可能的原因:"
    echo "1. 配置文件中的 rtsp.pull 未生效"
    echo "2. 摄像头连接失败"
    echo "3. 等待时间不够"
    echo ""
    echo "建议:"
    echo "1. 检查配置: cat config.yaml | grep -A 30 'rtsp:'"
    echo "2. 查看日志: tail -100 logs/m7s.log | grep -i rtsp"
    echo "3. 等待更长时间后重试"
elif [ $ready_count -lt ${#STREAMS[@]} ]; then
    echo "⚠️  部分流未就绪"
    echo ""
    echo "建议:"
    echo "1. 检查未就绪流的摄像头连接"
    echo "2. 查看日志: tail -100 logs/m7s.log"
else
    echo "✓ 所有流都已就绪！"
fi

echo ""
