#!/bin/bash

# 最简化的测试脚本 - 直接开始录制，不做任何检查

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
RECORD_DURATION="${RECORD_DURATION:-180}"

# 35 个摄像头流路径
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
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

echo ""
echo "=========================================="
echo "  极简版录制测试"
echo "=========================================="
echo ""
echo "流数量: ${#STREAMS[@]}"
echo "录制时长: ${RECORD_DURATION}秒"
echo ""

# 1. 检查服务
echo -e "${CYAN}[1/6]${NC} 检查服务..."
if curl -s "http://$NODE_IP:$HTTP_PORT/api/sysinfo" > /dev/null 2>&1; then
    echo -e "${GREEN}✓${NC} Monibuca 运行正常"
else
    echo -e "${RED}✗${NC} Monibuca 未运行，请先执行: ./start.sh"
    exit 1
fi
echo ""

# 2. 停止所有录制并清理录制目录
echo -e "${CYAN}[2/6]${NC} 停止所有录制并清理录制目录..."
for stream in "${STREAMS[@]}"; do
    curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$stream" > /dev/null 2>&1
done
# 清理录制目录
rm -rf record/live 2>/dev/null || true
echo -e "${GREEN}✓${NC} 清理完成"
echo ""

# 3. 等待系统释放资源
echo -e "${CYAN}[3/6]${NC} 等待系统释放资源（5秒）..."
for i in {1..5}; do
    echo -ne "\r  等待中... $i/5 秒"
    sleep 1
done
echo ""
echo ""

# 4. 开始录制
echo -e "${CYAN}[4/6]${NC} 开始录制..."
success=0
failed=0
for stream in "${STREAMS[@]}"; do
    # 生成文件名（将 live/camera1 转换为 camera1.mp4）
    camera_name="${stream##*/}"
    filename="${camera_name}.mp4"
    # 使用流路径作为 filePath，确保每个流的录制路径唯一，避免与历史录制任务冲突
    filepath="live/${stream}"

    # 使用正确的 API: POST /mp4/api/start/{streamPath}
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$stream" \
        -H "Content-Type: application/json" \
        -d "{\"fragment\": \"600s\", \"filePath\": \"$filepath\", \"fileName\": \"$filename\"}")

    if echo "$response" | grep -q '"code":0'; then
        success=$((success + 1))
    else
        failed=$((failed + 1))
        # 显示第一个失败的详细信息
        if [ $failed -eq 1 ]; then
            echo ""
            echo "第一个失败的流: $stream"
            echo "响应: $response"
        fi
    fi
    echo -ne "\r  已启动: $success/${#STREAMS[@]} (失败: $failed)"
done
echo ""
echo -e "${GREEN}✓${NC} 成功启动 $success 个录制"
if [ $failed -gt 0 ]; then
    echo -e "${YELLOW}⚠${NC} 失败 $failed 个录制"
fi
echo ""

# 5. 录制中
echo -e "${CYAN}[5/6]${NC} 录制中..."
for i in $(seq 1 "$RECORD_DURATION"); do
    echo -ne "\r  进度: $i/$RECORD_DURATION 秒"
    sleep 1
done
echo ""
echo ""

# 6. 停止录制
echo -e "${CYAN}[6/6]${NC} 停止录制..."
stopped=0
for stream in "${STREAMS[@]}"; do
    # 使用正确的 API: POST /mp4/api/stop/{streamPath}
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$stream")

    if echo "$response" | grep -q '"code":0'; then
        stopped=$((stopped + 1))
    fi
    echo -ne "\r  已停止: $stopped/${#STREAMS[@]}"
done
echo ""
echo -e "${GREEN}✓${NC} 成功停止 $stopped 个录制"
echo ""

# 等待文件写入
echo "等待文件写入（15秒）..."
sleep 15
echo ""

# 检查结果
echo "=========================================="
echo "  结果"
echo "=========================================="
echo ""

if [ -d "record/live" ]; then
    file_count=$(find record/live -name "*.mp4" -type f 2>/dev/null | wc -l | tr -d ' ')
    total_size=$(du -sh record/live 2>/dev/null | cut -f1)

    echo "录制文件数: $file_count"
    echo "总大小: $total_size"
    echo ""

    if [ "$file_count" -gt 0 ]; then
        echo "所有录制文件及时长:"
        echo "----------------------------------------"
        total_duration=0
        failed_count=0
        while IFS= read -r file; do
            # 获取文件大小
            size=$(ls -lh "$file" | awk '{print $5}')
            # 使用 ffprobe 获取时长（秒）- 尝试多种方式
            duration=$(ffprobe -v quiet -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 "$file" 2>/dev/null)
            if [ -z "$duration" ] || [ "$duration" = "N/A" ]; then
                # 尝试从流信息获取
                duration=$(ffprobe -v quiet -show_entries stream=duration -of default=noprint_wrappers=1:nokey=1 "$file" 2>/dev/null | head -1)
            fi
            if [ -n "$duration" ] && [ "$duration" != "N/A" ] && [ "$duration" != "0" ]; then
                duration_int=${duration%.*}
                if [ -n "$duration_int" ] && [ "$duration_int" -gt 0 ] 2>/dev/null; then
                    total_duration=$((total_duration + duration_int))
                    mins=$((duration_int / 60))
                    secs=$((duration_int % 60))
                    printf "%-60s %6s %3d:%02d\n" "$file" "$size" "$mins" "$secs"
                else
                    printf "%-60s %6s 时长异常(%s)\n" "$file" "$size" "$duration"
                    failed_count=$((failed_count + 1))
                fi
            else
                printf "%-60s %6s 获取失败\n" "$file" "$size"
                failed_count=$((failed_count + 1))
            fi
        done < <(find record/live -name "*.mp4" -type f 2>/dev/null | sort)
        echo "----------------------------------------"
        total_mins=$((total_duration / 60))
        total_secs=$((total_duration % 60))
        echo "总时长: ${total_mins}分${total_secs}秒 (${total_duration}秒)"
        if [ $failed_count -gt 0 ]; then
            echo -e "${YELLOW}警告: ${failed_count} 个文件获取时长失败（可能是文件未完整写入或格式异常）${NC}"
        fi
    fi
else
    echo "录制目录不存在"
fi

echo ""
echo "查看详细日志: tail -100 logs/m7s.log"
echo ""
