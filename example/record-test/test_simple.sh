#!/bin/bash

# 最简化的测试脚本 - 直接开始录制，不做任何检查

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

NODE_IP="${NODE_IP:-localhost}"
HTTP_PORT="${HTTP_PORT:-8080}"
RECORD_DURATION="${RECORD_DURATION:-600}"

# MinIO 配置（从 config.yaml 读取）
MINIO_ENDPOINT="${MINIO_ENDPOINT:-storage-dev.xiding.tech}"
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:-xidinguser}"
MINIO_SECRET_KEY="${MINIO_SECRET_KEY:-U2FsdGVkX1/7uyvj0trCzSNFsfDZ66dMSAEZjNlvW1c=}"
MINIO_BUCKET="${MINIO_BUCKET:-vidu-media-bucket}"
MINIO_PATH_PREFIX="${MINIO_PATH_PREFIX:-recordings}"
MINIO_USE_SSL="${MINIO_USE_SSL:-true}"

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
        -d "{\"fragment\": \"0s\", \"filePath\": \"$filepath\", \"fileName\": \"$filename\"}")

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

# 等待文件上传到 MinIO（等待时间更长，因为需要上传）
echo "等待文件上传到 MinIO（60秒）..."
sleep 60
echo ""

# 检查 MinIO 上传结果
echo "=========================================="
echo "  MinIO 上传结果检查"
echo "=========================================="
echo ""

# 构建 MinIO 协议前缀
if [ "$MINIO_USE_SSL" = "true" ]; then
    MINIO_PROTOCOL="https"
else
    MINIO_PROTOCOL="http"
fi

echo "MinIO 配置:"
echo "  端点: $MINIO_ENDPOINT"
echo "  存储桶: $MINIO_BUCKET"
echo "  路径前缀: $MINIO_PATH_PREFIX"

# 检查 mc (MinIO Client) 是否可用
MC_AVAILABLE=false
if command -v mc &> /dev/null; then
    MC_AVAILABLE=true
    echo "  检查工具: mc (MinIO Client)"
    # 配置 mc alias（如果尚未配置）
    mc alias set minio-test "${MINIO_PROTOCOL}://${MINIO_ENDPOINT}" "${MINIO_ACCESS_KEY}" "${MINIO_SECRET_KEY}" &> /dev/null || true
else
    echo "  检查工具: curl (基本检查)"
    echo -e "  ${YELLOW}提示: 安装 mc 可获取完整元数据信息${NC}"
    echo "        macOS: brew install minio/stable/mc"
    echo "        Linux: wget https://dl.min.io/client/mc/release/linux-amd64/mc && chmod +x mc"
fi
echo ""

# 检查每个摄像头的文件
echo "检查上传文件:"
echo "----------------------------------------"
printf "%-20s %-10s %-15s %-15s\n" "摄像头" "状态" "时长(秒)" "大小(字节)"
echo "----------------------------------------"

uploaded_count=0
failed_upload_count=0
total_duration_ms=0
total_size_bytes=0

# 关闭 pipefail，避免 mc stat 失败导致脚本中断
set +e

for stream in "${STREAMS[@]}"; do
    camera_name="${stream##*/}"
    filename="${camera_name}.mp4"
    # 对象路径: recordings/live/camera1/camera1.mp4
    object_key="${MINIO_PATH_PREFIX}/${stream}/${filename}"

    if [ "$MC_AVAILABLE" = true ]; then
        # 使用 mc stat 获取对象信息和 metadata
        mc_output=$(mc stat --json "minio-test/${MINIO_BUCKET}/${object_key}" 2>/dev/null)

        if [ $? -eq 0 ] && [ -n "$mc_output" ]; then
            # 文件存在，提取 metadata
            # mc stat --json 输出格式:
            # {"status": "success", "metadata": {"X-Amz-Meta-Video-Duration-Ms": "101424", "X-Amz-Meta-Video-Size-Bytes": "6318897"}}
            
            # 提取时长 (毫秒)
            duration_ms=$(echo "$mc_output" | sed 's/.*"X-Amz-Meta-Video-Duration-Ms"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
            
            # 提取文件大小 (字节)
            size_bytes=$(echo "$mc_output" | sed 's/.*"X-Amz-Meta-Video-Size-Bytes"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/')
            
            # 如果 metadata 中没有大小，从 size 字段获取
            if [ -z "$size_bytes" ] || [ "$size_bytes" = "$mc_output" ]; then
                size_bytes=$(echo "$mc_output" | sed 's/.*"size"[[:space:]]*:[[:space:]]*\([0-9]*\).*/\1/')
            fi

            if [ -n "$duration_ms" ] && [ "$duration_ms" -gt 0 ] 2>/dev/null; then
                duration_sec=$((duration_ms / 1000))
                uploaded_count=$((uploaded_count + 1))
                total_duration_ms=$((total_duration_ms + duration_ms))
                
                if [ -n "$size_bytes" ] && [ "$size_bytes" -gt 0 ] 2>/dev/null; then
                    total_size_bytes=$((total_size_bytes + size_bytes))
                    printf "%-20s ${GREEN}%-10s${NC} %-15s %-15s\n" \
                        "$camera_name" "✓ 已上传" "$duration_sec" "$size_bytes"
                else
                    printf "%-20s ${GREEN}%-10s${NC} %-15s %-15s\n" \
                        "$camera_name" "✓ 已上传" "$duration_sec" "N/A"
                fi
            else
                uploaded_count=$((uploaded_count + 1))
                printf "%-20s ${YELLOW}%-10s${NC} %-15s %-15s\n" \
                    "$camera_name" "⚠ 无元数据" "N/A" "$size_bytes"
            fi
        else
            failed_upload_count=$((failed_upload_count + 1))
            printf "%-20s ${RED}%-10s${NC} %-15s %-15s\n" \
                "$camera_name" "✗ 未上传" "-" "-"
        fi
    else
        # mc 不可用，使用 curl + AWS Signature V4（简化版，仅检查存在性）
        # 注意：完整的 AWS Signature V4 实现较复杂，这里仅做基本检查
        response=$(curl -s -o /dev/null -w "%{http_code}" \
            "${MINIO_PROTOCOL}://${MINIO_ENDPOINT}/${MINIO_BUCKET}/${object_key}")

        if [ "$response" = "200" ] || [ "$response" = "403" ]; then
            # 200 = 公开可访问，403 = 存在但需要认证（说明文件存在）
            uploaded_count=$((uploaded_count + 1))
            printf "%-20s ${YELLOW}%-10s${NC} %-15s %-15s\n" \
                "$camera_name" "⚠ 已上传" "需要mc" "需要mc"
        else
            failed_upload_count=$((failed_upload_count + 1))
            printf "%-20s ${RED}%-10s${NC} %-15s %-15s\n" \
                "$camera_name" "✗ 未上传" "-" "-"
        fi
    fi
done

# 重新启用错误检查
set -e

echo "----------------------------------------"
echo ""
echo "统计信息:"
echo "  成功上传: $uploaded_count/${#STREAMS[@]}"
echo "  上传失败: $failed_upload_count"

if [ $total_duration_ms -gt 0 ]; then
    total_duration_sec=$((total_duration_ms / 1000))
    total_mins=$((total_duration_sec / 60))
    total_secs=$((total_duration_sec % 60))
    echo "  总时长: ${total_mins}分${total_secs}秒 (${total_duration_sec}秒)"
fi

if [ $total_size_bytes -gt 0 ]; then
    # 转换为 MB
    total_size_mb=$((total_size_bytes / 1024 / 1024))
    echo "  总大小: ${total_size_mb} MB (${total_size_bytes} 字节)"
fi

echo ""

if [ $failed_upload_count -gt 0 ]; then
    echo -e "${YELLOW}⚠ 警告: 有 $failed_upload_count 个文件未成功上传到 MinIO${NC}"
    echo "  请检查 Monibuca 日志: tail -100 logs/m7s.log"
else
    echo -e "${GREEN}✓ 所有文件已成功上传到 MinIO${NC}"
fi

echo ""
echo "查看详细日志: tail -100 logs/m7s.log"
echo ""
