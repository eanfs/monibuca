# 流播放问题故障排查指南

本文档提供了针对 `test_playback_stress.sh` 脚本的常见问题排查方法。

## 常见问题及解决方案

### 1. ffplay 进程立即退出

**症状:**
```
开始播放: live/camera01 (rtsp://10.24.62.77:554/live/camera01)
  ✗ 播放进程立即退出 (PID: 12345)
```

**可能原因:**
- 流地址不正确或流不存在
- 服务器端口配置错误
- ffplay 未安装或版本不兼容
- 网络连接问题

**排查步骤:**

1. **检查 ffplay 是否安装:**
   ```bash
   which ffplay
   ffplay -version
   ```
   如果未安装,请安装 FFmpeg:
   ```bash
   # macOS
   brew install ffmpeg

   # Ubuntu/Debian
   sudo apt-get install ffmpeg

   # CentOS/RHEL
   sudo yum install ffmpeg
   ```

2. **手动测试单个流:**
   ```bash
   # 测试 RTSP 流
   ffplay -rtsp_transport tcp -timeout 10000000 -i rtsp://10.24.62.77:554/live/camera01

   # 测试 FLV 流
   ffplay -i http://10.24.62.77:7080/live/camera01.flv
   ```

3. **检查服务器 API 端点:**
   ```bash
   # 查看服务器版本
   curl http://10.24.62.77:7080/api/version

   # 查看流列表
   curl http://10.24.62.77:7080/api/streams
   curl http://10.24.62.77:7080/rtsp/api/list
   ```

4. **查看详细日志:**
   脚本会在 `playback_logs/` 目录下生成详细日志:
   ```bash
   # 查看特定流的日志
   cat playback_logs/live_camera01.log

   # 搜索所有错误
   grep -i error playback_logs/*.log
   ```

### 2. 无法获取流列表

**症状:**
```
错误: 所有 API 端点都无法获取流列表
```

**排查步骤:**

1. **检查服务器是否运行:**
   ```bash
   curl -v http://10.24.62.77:7080/api/version
   ```

2. **检查网络连接:**
   ```bash
   ping 10.24.62.77
   telnet 10.24.62.77 7080
   ```

3. **查看 Monibuca 服务器日志:**
   检查服务器端是否有错误信息

4. **尝试不同的 API 端点:**
   脚本已经自动尝试以下端点:
   - `/api/streams` (通用流列表)
   - `/rtsp/api/list` (RTSP 插件)
   - `/api/summary` (摘要信息)

### 3. 流播放卡顿或黑屏

**症状:**
- ffplay 进程运行,但没有画面
- 画面卡顿严重

**可能原因:**
- 网络带宽不足
- 服务器负载过高
- 并发数过大

**解决方案:**

1. **降低并发数:**
   ```bash
   ./test_playback_stress.sh -c 5  # 设置并发为 5
   ```

2. **切换传输协议:**
   ```bash
   # 使用 UDP 传输 (适合局域网)
   ./test_playback_stress.sh --rtsp-transport udp

   # 使用 FLV 协议
   ./test_playback_stress.sh --protocol flv
   ```

3. **增加超时时间:**
   修改脚本中的超时参数(第 306 行):
   ```bash
   -timeout 30000000  # 30秒超时
   ```

### 4. 连接超时

**症状:**
日志中显示 "Connection timed out" 或 "No route to host"

**排查步骤:**

1. **检查防火墙规则:**
   ```bash
   # 检查 RTSP 端口 (554)
   sudo firewall-cmd --list-ports  # CentOS
   sudo ufw status                  # Ubuntu

   # 如需开放端口:
   sudo firewall-cmd --add-port=554/tcp --permanent
   sudo firewall-cmd --reload
   ```

2. **检查服务器监听端口:**
   ```bash
   netstat -tuln | grep 554
   netstat -tuln | grep 7080
   ```

3. **测试网络路由:**
   ```bash
   traceroute 10.24.62.77
   ```

## 使用技巧

### 调试模式运行

启用详细日志以获取更多调试信息:

```bash
# 修改脚本中的日志级别(第 297 行)
-loglevel error  # 改为 info 或 debug
```

或者直接运行:
```bash
FFPLAY_LOGLEVEL=info ./test_playback_stress.sh
```

### 快速测试单个流

```bash
# 测试时间设为 10 秒,只测试前 1 个流
./test_playback_stress.sh -d 10 -m 1
```

### 获取帮助信息

```bash
./test_playback_stress.sh --help
```

## 脚本改进说明

最新版本的脚本包含以下改进:

1. **多 API 端点支持**: 自动尝试多个可能的 API 端点获取流列表
2. **详细错误日志**: 每个流都有独立的日志文件,包含完整的调试信息
3. **进程状态检测**: 启动后立即检测 ffplay 进程是否成功运行
4. **失败统计**: 详细统计启动失败和播放错误的流
5. **改进的命令构建**: 使用数组方式构建 ffplay 命令,避免引号问题

## 联系支持

如果以上方法都无法解决问题,请提供以下信息以便进一步排查:

1. 脚本运行的完整输出
2. `playback_logs/` 目录下的日志文件
3. Monibuca 服务器版本和配置
4. 网络拓扑结构(是否跨网段、是否有 NAT 等)
