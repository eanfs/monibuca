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
echo ""
echo "=========================================="
echo "  推流地址"
echo "=========================================="
echo ""
echo "RTSP: rtsp://localhost:554/live/test1"
echo "RTMP: rtmp://localhost:1935/live/test1"
echo ""
echo "=========================================="
echo "  API 示例"
echo "=========================================="
echo ""
echo "开始录制:"
echo "  curl -X POST 'http://localhost:8080/mp4/api/StartRecord?streamPath=live/test1'"
echo ""
echo "停止录制:"
echo "  curl -X POST 'http://localhost:8080/mp4/api/StopRecord?streamPath=live/test1'"
echo ""
echo "查看录制列表:"
echo "  curl 'http://localhost:8080/mp4/api/List'"
echo ""
echo "=========================================="
echo ""

# 启动服务
go run -tags sqlite main.go
