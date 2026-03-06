#!/bin/bash

# S3 临时文件清理脚本
# 清理超过 24 小时的 s3writer_*.tmp 文件

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMP_DIR="${SCRIPT_DIR}/record"

echo "[$(date '+%Y-%m-%d %H:%M:%S')] 开始清理 S3 临时文件..."

# 清理 S3 临时文件
if [ -d "$TEMP_DIR" ]; then
    deleted_count=$(find "$TEMP_DIR" -name "s3writer_*.tmp" -delete -print | wc -l | tr -d ' ')
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 已删除 $deleted_count 个临时文件"
else
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 临时目录不存在: $TEMP_DIR"
fi

echo "[$(date '+%Y-%m-%d %H:%M:%S')] 清理完成"
