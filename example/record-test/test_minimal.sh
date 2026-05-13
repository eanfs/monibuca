#!/bin/bash

# 最简化的测试脚本 - 直接开始录制，不做任何检查

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
RECORD_DURATION="${RECORD_DURATION:-100}"

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
    filepath="${stream}"

    # 使用正确的 API: POST /mp4/api/start/{streamPath}
    response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$stream" \
        -H "Content-Type: application/json" \
        -d "{\"duration\": \"60\", \"fragment\": \"0\", \"filePath\": \"$filepath\", \"fileName\": \"$filename\"}")

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
        # 关闭 pipefail 避免管道中非零退出码导致脚本中断
        set +e
        find record/live -name "*.mp4" -type f 2>/dev/null | sort | while IFS= read -r file; do
            # 获取文件大小
            size=$(ls -lh "$file" | awk '{print $5}')
            # 使用 ffprobe 获取时长
            # 由于某些视频音频流有问题，直接从 Duration 行解析更可靠
            duration_str=$(ffprobe "$file" 2>&1 | grep Duration | awk '{print $2}' | tr -d ',')
            if [ -n "$duration_str" ] && [ "$duration_str" != "N/A" ]; then
                # 将 HH:MM:SS.ms 转换为秒
                hours=$(echo "$duration_str" | cut -d: -f1)
                mins=$(echo "$duration_str" | cut -d: -f2)
                secs=$(echo "$duration_str" | cut -d: -f3)
                # 移除小数部分
                secs_int=${secs%%.*}
                duration_int=$((10#$hours * 3600 + 10#$mins * 60 + 10#$secs_int))
            else
                duration_int=0
            fi
            if [ -n "$duration_int" ] && [ "$duration_int" -gt 0 ] 2>/dev/null; then
                mins=$((duration_int / 60))
                secs=$((duration_int % 60))
                printf "%-60s %6s %3d:%02d\n" "$file" "$size" "$mins" "$secs"
                echo "DURATION:$duration_int"
            else
                printf "%-60s %6s 获取失败\n" "$file" "$size"
                echo "FAILED:1"
            fi
        done | awk '
            /^DURATION:/ { split($0, arr, ":"); total += arr[2] }
            /^FAILED:/ { failed++ }
            !/^(DURATION|FAILED):/ { print $0 }
            END {
                print "----------------------------------------"
                total_mins = int(total / 60)
                total_secs = int(total % 60)
                printf "总时长: %d分%d秒 (%d秒)\n", total_mins, total_secs, total
                if (failed > 0) {
                    printf "\033[1;33m警告: %d 个文件获取时长失败\033[0m\n", failed
                }
            }
        '
        set -e
    fi
else
    echo "录制目录不存在"
fi

echo ""
echo "查看详细日志: tail -100 logs/m7s.log"
echo ""
