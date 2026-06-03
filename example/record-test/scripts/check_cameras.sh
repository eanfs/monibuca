#!/bin/bash

# 快速检查所有摄像头的连接状态

set -euo pipefail

# 颜色
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo "=========================================="
echo "  摄像头连接状态检查"
echo "=========================================="
echo ""

# 海康威视摄像头
HIKVISION_IPS=(
    "192.168.12.71" "192.168.12.72" "192.168.12.73"
    "192.168.10.111" "192.168.10.112" "192.168.10.113"
    "192.168.12.31" "192.168.12.32" "192.168.12.33"
)

# 大华摄像头
DAHUA_IPS=(
    "192.168.12.11" "192.168.12.12" "192.168.12.13"
    "192.168.12.61" "192.168.12.62" "192.168.12.63"
    "192.168.12.91" "192.168.12.92" "192.168.12.93"
)

online_count=0
offline_count=0
total_count=0

echo "海康威视摄像头 (9 个):"
echo "----------------------------"
for ip in "${HIKVISION_IPS[@]}"; do
    total_count=$((total_count + 1))
    if ping -c 1 -W 1 "$ip" > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC} $ip 在线"
        online_count=$((online_count + 1))
    else
        echo -e "${RED}✗${NC} $ip 离线"
        offline_count=$((offline_count + 1))
    fi
done

echo ""
echo "大华摄像头 (9 个):"
echo "----------------------------"
for ip in "${DAHUA_IPS[@]}"; do
    total_count=$((total_count + 1))
    if ping -c 1 -W 1 "$ip" > /dev/null 2>&1; then
        echo -e "${GREEN}✓${NC} $ip 在线"
        online_count=$((online_count + 1))
    else
        echo -e "${RED}✗${NC} $ip 离线"
        offline_count=$((offline_count + 1))
    fi
done

echo ""
echo "=========================================="
echo "  统计结果"
echo "=========================================="
echo ""
echo "总摄像头数: $total_count"
echo -e "在线: ${GREEN}$online_count${NC}"
echo -e "离线: ${RED}$offline_count${NC}"
echo ""

if [ $online_count -eq $total_count ]; then
    echo -e "${GREEN}✓ 所有摄像头在线！${NC}"
    exit 0
elif [ $online_count -eq 0 ]; then
    echo -e "${RED}✗ 所有摄像头离线！${NC}"
    echo ""
    echo "请检查:"
    echo "  1. 网络连接是否正常"
    echo "  2. 是否在正确的网络环境中"
    echo "  3. 摄像头是否已开机"
    exit 1
else
    echo -e "${YELLOW}⚠ 部分摄像头离线${NC}"
    echo ""
    echo "可以继续测试，但只会录制在线的摄像头"
    exit 0
fi
