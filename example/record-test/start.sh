#!/bin/bash

# 快速启动脚本

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "=========================================="
echo "  Monibuca 录制测试 - 快速启动"
echo "=========================================="
echo ""

# 创建必要的目录
mkdir -p record/live
mkdir -p logs

# 检查是否已经在运行
if pgrep -f "go run.*main.go" > /dev/null; then
    echo "⚠️  检测到 Monibuca 已在运行"
    echo ""
    echo "如需重启，请先停止:"
    echo "  pkill -f 'go run.*main.go'"
    echo ""
    exit 1
fi

echo "✅ 环境准备完成"
echo ""
echo "启动 Monibuca 服务..."
echo ""
echo "=========================================="
echo "  服务信息"
echo "=========================================="
echo ""
echo "HTTP API:  http://localhost:8080"
echo "RTSP:      rtsp://localhost:554"
echo "配置文件:  config.yaml"
echo "录制目录:  record/live/"
echo "日志目录:  logs/"


# 启动服务（-tags sqlite,s3 启用 S3/MinIO 上传, 不带 s3 会 fallback 到本地）
TAGS="${BUILD_TAGS:-sqlite,s3}"
echo "构建标签: $TAGS"
go run -tags "$TAGS" main.go
