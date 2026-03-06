#!/bin/bash

# 检查录制文件夹下的视频长度与大小

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

RECORD_DIR="${1:-record/live}"

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

echo ""
echo "=========================================="
echo "  视频文件检查"
echo "=========================================="
echo ""
echo "检查目录: $RECORD_DIR"
echo ""

if [ ! -d "$RECORD_DIR" ]; then
    echo -e "${RED}错误: 目录不存在${NC}"
    exit 1
fi

file_count=$(find "$RECORD_DIR" -name "*.mp4" -type f 2>/dev/null | wc -l | tr -d ' ')

if [ "$file_count" -eq 0 ]; then
    echo "未找到 MP4 文件"
    exit 0
fi

total_size=$(du -sh "$RECORD_DIR" 2>/dev/null | cut -f1)

echo "文件数量: $file_count"
echo "总大小: $total_size"
echo ""
echo "----------------------------------------"
printf "%-60s %8s %8s\n" "文件路径" "大小" "时长"
echo "----------------------------------------"

total_duration=0
failed_count=0

while IFS= read -r file; do
    # 获取文件大小
    size=$(ls -lh "$file" | awk '{print $5}')
    
    # 使用 ffprobe 获取时长（秒），忽略错误退出码
    duration=$(ffprobe -v quiet -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 "$file" 2>/dev/null) || true
    
    if [ -z "$duration" ] || [ "$duration" = "N/A" ]; then
        # 尝试从流信息获取
        duration=$(ffprobe -v quiet -show_entries stream=duration -of default=noprint_wrappers=1:nokey=1 "$file" 2>/dev/null | head -1) || true
    fi
    
    if [ -n "$duration" ] && [ "$duration" != "N/A" ] && [ "$duration" != "0" ]; then
        duration_int=${duration%.*}
        if [ -n "$duration_int" ] && [ "$duration_int" -gt 0 ] 2>/dev/null; then
            total_duration=$((total_duration + duration_int))
            mins=$((duration_int / 60))
            secs=$((duration_int % 60))
            printf "%-60s %8s %4d:%02d\n" "$file" "$size" "$mins" "$secs"
        else
            printf "%-60s %8s   异常(%s)\n" "$file" "$size" "$duration"
            failed_count=$((failed_count + 1))
        fi
    else
        printf "%-60s %8s   获取失败\n" "$file" "$size"
        failed_count=$((failed_count + 1))
    fi
done < <(find "$RECORD_DIR" -name "*.mp4" -type f 2>/dev/null | sort)

echo "----------------------------------------"
total_mins=$((total_duration / 60))
total_secs=$((total_duration % 60))
total_hours=$((total_mins / 60))
total_mins_remain=$((total_mins % 60))

echo ""
echo "统计信息:"
echo "  总时长: ${total_hours}小时${total_mins_remain}分${total_secs}秒 (${total_duration}秒)"
echo "  总大小: $total_size"
echo "  文件数: $file_count"

if [ $failed_count -gt 0 ]; then
    echo ""
    echo -e "${YELLOW}警告: ${failed_count} 个文件获取时长失败（可能是文件未完整写入或格式异常）${NC}"
fi

echo ""
