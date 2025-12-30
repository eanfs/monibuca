#!/bin/bash

# 测试 API 响应解析
SERVER_IP="${1:-10.24.62.77}"
SERVER_HTTP_PORT="${2:-7080}"

echo "测试服务器: $SERVER_IP:$SERVER_HTTP_PORT"
echo ""

# 测试连接
echo "1. 测试服务器连接..."
if curl -s --connect-timeout 5 "http://$SERVER_IP:$SERVER_HTTP_PORT/api/version" > /dev/null 2>&1; then
    echo "   ✓ 服务器连接成功"
else
    echo "   ✗ 服务器连接失败"
    exit 1
fi

echo ""
echo "2. 测试 API 端点..."

# 测试 /rtsp/api/list
echo "   尝试: /rtsp/api/list"
response=$(curl -s --connect-timeout 10 "http://$SERVER_IP:$SERVER_HTTP_PORT/rtsp/api/list" 2>&1)

if [ $? -eq 0 ] && [ -n "$response" ]; then
    echo "   ✓ API 响应成功"
    echo ""
    echo "3. 解析 JSON..."
    
    if command -v jq &> /dev/null; then
        echo "   使用 jq 解析..."
        streams=($(echo "$response" | jq -r '.[] | .Path' 2>/dev/null))
        
        if [ ${#streams[@]} -gt 0 ]; then
            echo "   ✓ 成功解析 ${#streams[@]} 个流"
            echo ""
            echo "流列表:"
            for stream in "${streams[@]}"; do
                echo "   - $stream"
            done
            
            echo ""
            echo "4. 测试播放 URL 构建..."
            for stream in "${streams[@]:0:2}"; do
                rtsp_url="rtsp://$SERVER_IP:554/$stream"
                echo "   RTSP: $rtsp_url"
                
                # 使用 ffprobe 测试流是否可访问（如果安装了）
                if command -v ffprobe &> /dev/null; then
                    echo -n "   检查流可用性... "
                    if timeout 5 ffprobe -v quiet -print_format json -show_streams -rtsp_transport tcp "$rtsp_url" > /dev/null 2>&1; then
                        echo "✓ 可用"
                    else
                        echo "✗ 不可用或超时"
                    fi
                fi
            done
        else
            echo "   ✗ 未能解析出流"
            echo "   响应: ${response:0:500}"
        fi
    else
        echo "   ✗ jq 未安装，无法解析 JSON"
    fi
else
    echo "   ✗ API 请求失败"
fi
