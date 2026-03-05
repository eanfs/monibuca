#!/bin/bash

# 停止所有录制

set -euo pipefail

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
NC='\033[0m'

echo "停止所有录制..."
echo ""

stopped=0
for stream in "${STREAMS[@]}"; do
    curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$stream" > /dev/null 2>&1
    stopped=$((stopped + 1))
    echo -ne "\r  已停止: $stopped/${#STREAMS[@]}"
done

echo ""
echo -e "${GREEN}✓${NC} 完成"
echo ""
