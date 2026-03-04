#!/bin/bash

# 测试上传重试机制
# 使用方法：./test_upload_retry.sh

set -e

echo "🧪 测试 P0 修复：上传重试机制和数据库记录"
echo "================================================"

# 颜色定义
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# 检查数据库中的上传状态字段
echo ""
echo "📊 检查数据库模型..."
if grep -q "UploadStatus" ../../recoder.go; then
    echo -e "${GREEN}✓${NC} RecordStream 包含 UploadStatus 字段"
else
    echo -e "${RED}✗${NC} RecordStream 缺少 UploadStatus 字段"
    exit 1
fi

if grep -q "UploadError" ../../recoder.go; then
    echo -e "${GREEN}✓${NC} RecordStream 包含 UploadError 字段"
else
    echo -e "${RED}✗${NC} RecordStream 缺少 UploadError 字段"
    exit 1
fi

if grep -q "UploadRetry" ../../recoder.go; then
    echo -e "${GREEN}✓${NC} RecordStream 包含 UploadRetry 字段"
else
    echo -e "${RED}✗${NC} RecordStream 缺少 UploadRetry 字段"
    exit 1
fi

# 检查 S3File 的 Reopen 方法
echo ""
echo "🔄 检查 S3File Reopen 方法..."
if grep -q "func (w \*S3File) Reopen()" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} S3File 实现了 Reopen 方法"
else
    echo -e "${RED}✗${NC} S3File 缺少 Reopen 方法"
    exit 1
fi

# 检查 S3File 的 CleanupTempFile 方法
echo ""
echo "🧹 检查 S3File CleanupTempFile 方法..."
if grep -q "func (w \*S3File) CleanupTempFile()" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} S3File 实现了 CleanupTempFile 方法"
else
    echo -e "${RED}✗${NC} S3File 缺少 CleanupTempFile 方法"
    exit 1
fi

# 检查上传重试逻辑
echo ""
echo "🔁 检查上传重试逻辑..."
if grep -q "const maxRetry = 3" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 配置了最大重试次数（3次）"
else
    echo -e "${YELLOW}⚠${NC} 未找到 maxRetry 配置"
fi

if grep -q "Reopen()" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 重试时调用 Reopen 方法"
else
    echo -e "${RED}✗${NC} 重试逻辑未调用 Reopen"
    exit 1
fi

# 检查数据库状态更新
echo ""
echo "💾 检查数据库状态更新..."
if grep -q "updateUploadStatus" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 实现了 updateUploadStatus 方法"
else
    echo -e "${RED}✗${NC} 缺少 updateUploadStatus 方法"
    exit 1
fi

if grep -q "upload_status" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 更新 upload_status 字段"
else
    echo -e "${RED}✗${NC} 未更新 upload_status 字段"
    exit 1
fi

# 检查信号量释放优化
echo ""
echo "🚦 检查信号量释放优化..."
if grep -q "<-uploadSemaphore" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 实现了信号量释放"

    # 检查是否在上传成功后立即释放
    if grep -A 5 "upload successful" pkg/record.go | grep -q "<-uploadSemaphore"; then
        echo -e "${GREEN}✓${NC} 上传成功后立即释放信号量"
    else
        echo -e "${YELLOW}⚠${NC} 信号量释放时机可能不是最优"
    fi
else
    echo -e "${RED}✗${NC} 未找到信号量释放逻辑"
    exit 1
fi

# 检查临时文件保留逻辑
echo ""
echo "📁 检查临时文件保留逻辑..."
if grep -q "uploadFailed" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} S3File 包含 uploadFailed 标记"
else
    echo -e "${RED}✗${NC} S3File 缺少 uploadFailed 标记"
    exit 1
fi

if grep -q "temp file preserved for retry" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 上传失败时保留临时文件"
else
    echo -e "${RED}✗${NC} 未实现临时文件保留"
    exit 1
fi

# 编译测试
echo ""
echo "🔨 编译测试..."
if go build -tags s3,sqlite ./... 2>/dev/null; then
    echo -e "${GREEN}✓${NC} 代码编译成功"
else
    echo -e "${RED}✗${NC} 代码编译失败"
    exit 1
fi

# 总结
echo ""
echo "================================================"
echo -e "${GREEN}✅ 所有 P0 修复验证通过！${NC}"
echo ""
echo "修复内容："
echo "  1. ✓ 上传重试机制（支持 Reopen）"
echo "  2. ✓ 数据库状态追踪（UploadStatus/Error/Retry）"
echo "  3. ✓ 信号量释放优化"
echo "  4. ✓ 临时文件保留和清理"
echo ""
echo "下一步："
echo "  - 运行集成测试验证实际上传重试"
echo "  - 检查数据库迁移是否正常"
echo "  - 监控日志确认状态更新"
echo ""
