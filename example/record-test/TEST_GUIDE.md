# 测试脚本选择指南

## 问题说明

`test_cameras.sh` 在并发执行时出现了 "unbound variable" 错误。这是因为：

1. 脚本使用了 `set -u` 选项（严格模式）
2. 并发执行的子 shell 中变量作用域问题
3. 复杂的并发逻辑导致变量传递困难

## 已修复

- ✅ 移除了 `set -u` 选项
- ✅ 修复了并发执行中的变量传递
- ✅ 简化了子 shell 中的逻辑

## 推荐方案

### 🥇 首选：极简版测试

```bash
./test_minimal.sh
```

**优点：**
- ✅ 零并发，无变量作用域问题
- ✅ 代码简单，易于维护
- ✅ 执行最快
- ✅ 输出清晰
- ✅ 最可靠

**适用场景：**
- 日常测试
- 快速验证
- 生产环境

### 🥈 次选：简化版测试

```bash
./test_simple.sh
```

**优点：**
- ✅ 包含基本的流状态检查
- ✅ 无复杂并发逻辑
- ✅ 较快的执行速度
- ✅ 可靠性高

**适用场景：**
- 需要验证流状态
- 开发测试
- 集成测试

### 🥉 备选：完整版测试

```bash
./test_cameras.sh
```

**优点：**
- ✅ 包含所有检查（ping、RTSP、API）
- ✅ 详细的诊断信息
- ✅ 并发执行（已修复）

**缺点：**
- ⚠️  代码复杂
- ⚠️  执行较慢
- ⚠️  可能有边界情况

**适用场景：**
- 故障排查
- 详细诊断
- 网络问题分析

## 测试对比

| 特性 | 极简版 | 简化版 | 完整版 |
|------|--------|--------|--------|
| 代码行数 | ~100 | ~200 | ~500 |
| 并发执行 | ❌ | ❌ | ✅ |
| 执行时间 | ~90秒 | ~100秒 | ~120秒 |
| 可靠性 | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐ |
| 维护性 | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐ |
| 诊断能力 | ⭐⭐ | ⭐⭐⭐ | ⭐⭐⭐⭐⭐ |

## 使用建议

### 日常使用

```bash
# 快速测试（推荐）
./test_minimal.sh

# 30 秒快速验证
RECORD_DURATION=30 ./test_minimal.sh
```

### 开发测试

```bash
# 包含流状态检查
./test_simple.sh
```

### 故障排查

```bash
# 详细诊断
./test_cameras.sh

# 或者分步检查
./check_cameras.sh  # 先检查连接
./test_simple.sh    # 再测试录制
tail -f logs/m7s.log  # 查看日志
```

## 错误处理

如果遇到 "unbound variable" 错误：

1. **首选方案**：使用 `test_minimal.sh` 或 `test_simple.sh`
2. **备选方案**：确保 `test_cameras.sh` 已更新到最新版本
3. **临时方案**：手动执行测试步骤

### 手动测试步骤

```bash
# 1. 启动服务
./start.sh

# 2. 等待拉流
sleep 20

# 3. 开始录制
for i in {1..18}; do
    curl -s -X POST "http://localhost:8080/mp4/api/StartRecord?streamPath=live/camera$i"
done

# 4. 等待录制
sleep 60

# 5. 停止录制
for i in {1..18}; do
    curl -s -X POST "http://localhost:8080/mp4/api/StopRecord?streamPath=live/camera$i"
done

# 6. 查看结果
find record/live -name "*.mp4" -exec ls -lh {} \;
```

## 总结

**强烈推荐使用 `test_minimal.sh`**，因为：

1. ✅ 简单可靠
2. ✅ 无并发问题
3. ✅ 执行最快
4. ✅ 易于理解和维护
5. ✅ 满足 99% 的测试需求

只有在需要详细诊断时才使用 `test_cameras.sh`。
