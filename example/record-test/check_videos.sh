#!/bin/bash

# 检查录制文件夹下的视频长度与大小

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

# 使用管道方式，关闭 pipefail 避免管道中非零退出码导致脚本中断
set +e
find "$RECORD_DIR" -name "*.mp4" -type f 2>/dev/null | sort | while IFS= read -r file; do
    # 获取文件大小
    size=$(ls -lh "$file" | awk '{print $5}')
    
    # 使用 ffprobe 获取时长（秒）
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
        printf "%-60s %8s %4d:%02d\n" "$file" "$size" "$mins" "$secs"
        echo "DURATION:$duration_int"
    else
        printf "%-60s %8s   获取失败\n" "$file" "$size"
        echo "FAILED:1"
    fi
done | awk '
    /^DURATION:/ {
        split($0, arr, ":")
        total += arr[2]
    }
    /^FAILED:/ {
        failed++
    }
    !/^(DURATION|FAILED):/ {
        print $0
    }
    END {
        print "----------------------------------------"
        total_mins = int(total / 60)
        total_secs = int(total % 60)
        total_hours = int(total_mins / 60)
        total_mins_remain = total_mins % 60
        print ""
        print "统计信息:"
        printf "  总时长: %d小时%d分%d秒 (%d秒)\n", total_hours, total_mins_remain, total_secs, total
        if (failed > 0) {
            printf "  \033[1;33m警告: %d 个文件获取时长失败\033[0m\n", failed
        }
    }
'
set -e

echo "  总大小: $total_size"
echo "  文件数: $file_count"

echo ""
