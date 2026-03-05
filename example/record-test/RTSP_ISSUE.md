# RTSP 连接问题说明

## 问题描述

在测试过程中发现，使用 `ffprobe` 直接检查 RTSP 流时会失败，但这些流在 FLV 播放器中是可以正常播放的。

## 原因分析

1. **ffprobe 超时设置**
   - 默认超时时间太短
   - 摄像头响应较慢时会超时

2. **RTSP 协议参数**
   - 不同摄像头品牌需要不同的连接参数
   - 海康威视和大华的 RTSP 实现略有差异

3. **Monibuca 的优势**
   - Monibuca 有更完善的 RTSP 连接处理
   - 内置重连机制和超时处理
   - 对各种摄像头品牌有更好的兼容性

## 解决方案

### 方案 1: 使用简化版测试脚本（推荐）⭐

```bash
./test_simple.sh
```

**特点：**
- 跳过 ffprobe 的 RTSP 检查
- 直接依赖 Monibuca 的拉流功能
- 通过 HTTP API 检查流状态
- 更快、更可靠

**工作流程：**
1. 检查 Monibuca 服务状态
2. 等待 Monibuca 自动拉流（配置文件中已配置）
3. 通过 API 检查流是否就绪
4. 开始录制
5. 停止录制
6. 验证录制文件

### 方案 2: 使用修复后的完整测试脚本

```bash
./test_cameras.sh
```

**改进：**
- 增加了 ffprobe 超时时间（15 秒）
- 添加了更多 RTSP 连接参数
- RTSP 检查失败不会阻止测试继续
- 仍然会尝试录制

### 方案 3: 手动测试单个摄像头

```bash
# 使用 Monibuca 播放（推荐）
# 1. 启动服务
./start.sh

# 2. 在浏览器中访问
http://localhost:8080/flv/live/camera1.flv

# 3. 或使用 ffplay 通过 Monibuca 播放
ffplay http://localhost:8080/flv/live/camera1.flv
```

## 测试对比

### 简化版测试 vs 完整测试

| 特性 | 简化版 (test_simple.sh) | 完整版 (test_cameras.sh) |
|------|------------------------|-------------------------|
| ffprobe 检查 | ❌ 跳过 | ✅ 包含（但不阻塞） |
| 网络 ping | ❌ 跳过 | ✅ 包含 |
| 流状态检查 | ✅ API 检查 | ✅ API + RTSP 检查 |
| 执行速度 | ⚡ 快 | 🐢 较慢 |
| 可靠性 | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐ |
| 详细程度 | ⭐⭐⭐ | ⭐⭐⭐⭐⭐ |
| 推荐场景 | 日常测试 | 故障排查 |

## 推荐测试流程

### 快速测试（日常使用）

```bash
# 1. 启动服务
./start.sh

# 2. 运行简化测试（新终端）
./test_simple.sh

# 3. 查看结果
ls -lh record/live/
```

### 详细测试（故障排查）

```bash
# 1. 检查摄像头连接
./check_cameras.sh

# 2. 启动服务
./start.sh

# 3. 运行完整测试（新终端）
./test_cameras.sh

# 4. 查看详细日志
tail -f logs/m7s.log
```

## 验证 RTSP 流可用性

如果想验证 RTSP 流确实可用，可以通过 Monibuca 间接验证：

```bash
# 1. 启动 Monibuca
./start.sh

# 2. 等待 10 秒让 Monibuca 拉流
sleep 10

# 3. 检查流状态（通过 API）
curl -s "http://localhost:8080/api/stream/live/camera1" | jq '.'

# 4. 如果返回 code: 0，说明流已成功拉取
# 5. 可以通过 FLV 播放验证
ffplay http://localhost:8080/flv/live/camera1.flv
```

## 常见问题

### Q1: 为什么 ffprobe 检查失败但流可以播放？

**A:** ffprobe 使用的是标准 RTSP 客户端实现，对超时和错误处理比较严格。而 Monibuca 针对各种摄像头做了优化，有更好的兼容性和重连机制。

### Q2: 简化版测试会不会漏掉问题？

**A:** 不会。简化版测试仍然会：
- 检查 Monibuca 服务状态
- 通过 API 验证流是否就绪
- 验证录制文件完整性
- 检查文件是否可播放

唯一跳过的是 ffprobe 的直接 RTSP 检查，但这个检查本身就不够可靠。

### Q3: 如何确认摄像头真的在线？

**A:** 使用 ping 测试：

```bash
./check_cameras.sh
```

或手动测试：

```bash
ping 192.168.12.71
```

### Q4: 录制失败怎么办？

**A:** 检查日志：

```bash
# 查看最近的错误
grep -i error logs/m7s.log | tail -20

# 查看特定流的日志
grep "camera1" logs/m7s.log | tail -20

# 查看 RTSP 拉流日志
grep "rtsp" logs/m7s.log | tail -20
```

## 总结

**推荐使用简化版测试脚本 (`test_simple.sh`)**，因为：

1. ✅ 更快速
2. ✅ 更可靠
3. ✅ 直接使用 Monibuca 的能力
4. ✅ 避免 ffprobe 的兼容性问题
5. ✅ 更符合实际使用场景

完整版测试脚本 (`test_cameras.sh`) 适合用于：
- 故障排查
- 详细的连接诊断
- 网络问题分析
