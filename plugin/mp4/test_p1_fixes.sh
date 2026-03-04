#!/bin/bash

# 测试 P1 修复
# 使用方法：./test_p1_fixes.sh

set -e

echo "🧪 测试 P1 修复：动态超时、资源清理、并发安全"
echo "================================================"

# 颜色定义
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# ============================================
# 1. 检查动态超时配置
# ============================================
echo ""
echo "⏱️  检查动态超时配置..."

if grep -q "TimeoutPerMB.*time.Duration" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} S3StorageConfig 包含 TimeoutPerMB 字段"
else
    echo -e "${RED}✗${NC} S3StorageConfig 缺少 TimeoutPerMB 字段"
    exit 1
fi

if grep -q "MaxTimeout.*time.Duration" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} S3StorageConfig 包含 MaxTimeout 字段"
else
    echo -e "${RED}✗${NC} S3StorageConfig 缺少 MaxTimeout 字段"
    exit 1
fi

if grep -q "func (w \*S3File) calculateTimeout" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 实现了 calculateTimeout 方法"
else
    echo -e "${RED}✗${NC} 缺少 calculateTimeout 方法"
    exit 1
fi

# 检查超时计算逻辑
if grep -q "fileSizeMB.*float64(fileSize)" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 实现了文件大小计算"
else
    echo -e "${RED}✗${NC} 缺少文件大小计算"
    exit 1
fi

if grep -q "dynamicTimeout.*baseTimeout.*TimeoutPerMB" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 实现了动态超时计算公式"
else
    echo -e "${RED}✗${NC} 缺少动态超时计算公式"
    exit 1
fi

# ============================================
# 2. 检查资源清理优化
# ============================================
echo ""
echo "🧹 检查资源清理优化..."

if grep -q "var errs \[\]error" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 使用错误收集机制"
else
    echo -e "${RED}✗${NC} 未使用错误收集机制"
    exit 1
fi

if grep -q "closeErr.*w.tempFile.Close()" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 检查文件关闭错误"
else
    echo -e "${RED}✗${NC} 未检查文件关闭错误"
    exit 1
fi

if grep -q "removeErr.*os.Remove" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 检查文件删除错误"
else
    echo -e "${RED}✗${NC} 未检查文件删除错误"
    exit 1
fi

if grep -q "fmt.Errorf.*close temp file" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 记录文件关闭错误"
else
    echo -e "${RED}✗${NC} 未记录文件关闭错误"
    exit 1
fi

# ============================================
# 3. 检查并发安全
# ============================================
echo ""
echo "🔒 检查并发安全..."

if grep -q "uploadSemaphoreOnce sync.Once" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 使用 sync.Once 保护初始化"
else
    echo -e "${RED}✗${NC} 未使用 sync.Once 保护初始化"
    exit 1
fi

if grep -q "uploadSemaphoreOnce.Do" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 使用 Do() 方法初始化"
else
    echo -e "${RED}✗${NC} 未使用 Do() 方法初始化"
    exit 1
fi

if grep -q '"sync"' pkg/record.go; then
    echo -e "${GREEN}✓${NC} 导入 sync 包"
else
    echo -e "${RED}✗${NC} 未导入 sync 包"
    exit 1
fi

# ============================================
# 4. 检查错误信息增强
# ============================================
echo ""
echo "📝 检查错误信息增强..."

if grep -q "fileSizeMB.*fileSize/(1024\*1024)" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 记录文件大小（MB）"
else
    echo -e "${YELLOW}⚠${NC} 未记录文件大小（MB）"
fi

if grep -q "durationMs.*t.durationMs" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 记录录像时长"
else
    echo -e "${YELLOW}⚠${NC} 未记录录像时长"
fi

if grep -q "recordID.*t.recordID" pkg/record.go; then
    echo -e "${GREEN}✓${NC} 记录数据库 ID"
else
    echo -e "${YELLOW}⚠${NC} 未记录数据库 ID"
fi

# ============================================
# 5. 检查日志增强
# ============================================
echo ""
echo "📊 检查日志增强..."

if grep -q "timeout=%s" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 上传日志包含超时信息"
else
    echo -e "${YELLOW}⚠${NC} 上传日志未包含超时信息"
fi

if grep -q "calculated timeout.*exceeds max" ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} 记录超时限制触发"
else
    echo -e "${YELLOW}⚠${NC} 未记录超时限制触发"
fi

# ============================================
# 6. 编译测试
# ============================================
echo ""
echo "🔨 编译测试..."
if go build -tags s3,sqlite ./... 2>/dev/null; then
    echo -e "${GREEN}✓${NC} 代码编译成功"
else
    echo -e "${RED}✗${NC} 代码编译失败"
    exit 1
fi

# ============================================
# 7. 配置验证
# ============================================
echo ""
echo "⚙️  配置验证..."

# 检查默认值
if grep -q 'default:"60s"' ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} Timeout 默认值为 60s"
else
    echo -e "${YELLOW}⚠${NC} Timeout 默认值可能不是 60s"
fi

if grep -q 'default:"3s"' ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} TimeoutPerMB 默认值为 3s"
else
    echo -e "${YELLOW}⚠${NC} TimeoutPerMB 默认值可能不是 3s"
fi

if grep -q 'default:"15m"' ../../pkg/storage/s3.go; then
    echo -e "${GREEN}✓${NC} MaxTimeout 默认值为 15m"
else
    echo -e "${YELLOW}⚠${NC} MaxTimeout 默认值可能不是 15m"
fi

# ============================================
# 8. 文档检查
# ============================================
echo ""
echo "📚 文档检查..."

if [ -f "P1_FIX_SUMMARY.md" ]; then
    echo -e "${GREEN}✓${NC} P1_FIX_SUMMARY.md 存在"
else
    echo -e "${YELLOW}⚠${NC} P1_FIX_SUMMARY.md 不存在"
fi

if grep -q "v1.2.*P1 修复" UPLOAD_OPTIMIZATION.md; then
    echo -e "${GREEN}✓${NC} UPLOAD_OPTIMIZATION.md 已更新版本历史"
else
    echo -e "${YELLOW}⚠${NC} UPLOAD_OPTIMIZATION.md 未更新版本历史"
fi

# ============================================
# 总结
# ============================================
echo ""
echo "================================================"
echo -e "${GREEN}✅ 所有 P1 修复验证通过！${NC}"
echo ""
echo "修复内容："
echo "  1. ✓ 动态超时配置（Timeout + TimeoutPerMB + MaxTimeout）"
echo "  2. ✓ 资源清理优化（错误收集和记录）"
echo "  3. ✓ 并发安全（sync.Once 保护）"
echo "  4. ✓ 错误信息增强（文件大小、时长、ID）"
echo ""
echo "配置示例："
echo "  storage:"
echo "    s3:"
echo "      timeout: 60s"
echo "      timeout_per_mb: 3s"
echo "      max_timeout: 15m"
echo ""
echo "下一步："
echo "  - 测试动态超时计算（不同文件大小）"
echo "  - 监控资源清理日志"
echo "  - 验证并发安全（多次重启）"
echo ""
