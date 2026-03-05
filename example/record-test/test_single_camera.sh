#!/bin/bash

# 测试单个摄像头的 RTSP 连接

CAMERA_URL="rtsp://admin:a1234567@192.168.12.71/ch1/main/av_stream"

echo "测试摄像头 RTSP 连接..."
echo "URL: $CAMERA_URL"
echo ""

echo "方法 1: 使用 ffprobe（标准方式）"
if timeout 15 ffprobe \
    -rtsp_transport tcp \
    -v quiet \
    -print_format json \
    -show_streams \
    -i "$CAMERA_URL" > /tmp/ffprobe_output.json 2>&1; then
    echo "✓ 连接成功"
    echo ""
    echo "流信息:"
    cat /tmp/ffprobe_output.json | jq '.streams[0] | {codec_name, width, height, r_frame_rate}'
else
    echo "✗ 连接失败"
    echo "错误信息:"
    cat /tmp/ffprobe_output.json
fi

echo ""
echo "方法 2: 使用 ffprobe（宽松参数）"
if timeout 15 ffprobe \
    -rtsp_transport tcp \
    -stimeout 10000000 \
    -analyzeduration 5000000 \
    -probesize 5000000 \
    -v error \
    -i "$CAMERA_URL" > /dev/null 2>&1; then
    echo "✓ 连接成功"
else
    echo "✗ 连接失败"
fi

echo ""
echo "方法 3: 使用 ffplay 测试（5 秒）"
echo "按 Ctrl+C 可提前退出"
timeout 5 ffplay -rtsp_transport tcp -i "$CAMERA_URL" > /dev/null 2>&1 || true

echo ""
echo "测试完成"
