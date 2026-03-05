#!/bin/bash

# 调试单个流的录制

NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
STREAM_PATH="${1:-live/camera1}"

echo "=========================================="
echo "  调试单个流录制"
echo "=========================================="
echo ""
echo "流路径: $STREAM_PATH"
echo "服务地址: http://$NODE_IP:$HTTP_PORT"
echo ""

# 1. 检查服务
echo "[1/5] 检查服务状态..."
if curl -s "http://$NODE_IP:$HTTP_PORT/api/sysinfo" > /dev/null 2>&1; then
    echo "✓ 服务运行正常"
else
    echo "✗ 服务未运行"
    exit 1
fi
echo ""

# 2. 检查流状态
echo "[2/5] 检查流状态..."
stream_response=$(curl -s "http://$NODE_IP:$HTTP_PORT/api/stream/$STREAM_PATH")
echo "流状态响应:"
echo "$stream_response" | jq '.' 2>/dev/null || echo "$stream_response"
echo ""

# 3. 尝试开始录制
echo "[3/5] 开始录制..."
echo "请求: POST http://$NODE_IP:$HTTP_PORT/mp4/api/start/$STREAM_PATH"
echo "请求头: Content-Type: application/json"
echo "请求体: {}"
echo ""

record_response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$STREAM_PATH" \
    -H "Content-Type: application/json" \
    -d '{}')

echo "录制响应:"
echo "$record_response" | jq '.' 2>/dev/null || echo "$record_response"
echo ""

if echo "$record_response" | grep -q '"code":0'; then
    echo "✓ 录制开始成功"

    # 4. 等待 10 秒
    echo ""
    echo "[4/5] 录制中（10秒）..."
    for i in {1..10}; do
        echo -ne "\r  进度: $i/10 秒"
        sleep 1
    done
    echo ""
    echo ""

    # 5. 停止录制
    echo "[5/5] 停止录制..."
    stop_response=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$STREAM_PATH")

    echo "停止响应:"
    echo "$stop_response" | jq '.' 2>/dev/null || echo "$stop_response"
    echo ""

    if echo "$stop_response" | grep -q '"code":0'; then
        echo "✓ 录制停止成功"
    else
        echo "✗ 录制停止失败"
    fi

    # 等待文件写入
    echo ""
    echo "等待文件写入（5秒）..."
    sleep 5

    # 检查文件
    echo ""
    echo "=========================================="
    echo "  检查录制文件"
    echo "=========================================="
    echo ""

    stream_dir="record/${STREAM_PATH%/*}"
    if [ -d "$stream_dir" ]; then
        echo "录制目录: $stream_dir"
        find "$stream_dir" -name "*.mp4" -type f -exec ls -lh {} \;
    else
        echo "录制目录不存在: $stream_dir"
    fi

else
    echo "✗ 录制开始失败"
    echo ""
    echo "可能的原因:"
    echo "1. 流不存在或未就绪"
    echo "2. API 路径错误"
    echo "3. 权限问题"
    echo "4. 配置问题"
    echo ""
    echo "建议:"
    echo "1. 检查流是否存在: curl http://$NODE_IP:$HTTP_PORT/api/stream/$STREAM_PATH"
    echo "2. 查看日志: tail -50 logs/m7s.log"
    echo "3. 检查配置: cat config.yaml"
fi

echo ""
