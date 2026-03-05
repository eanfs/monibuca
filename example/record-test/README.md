# Monibuca 录制测试环境

单节点录制测试环境，用于测试 MP4 录制功能。

## 快速开始

### 1. 启动服务

```bash
cd /Users/lirichen/Work/XiDingCloud/monibuca-v5/example/record-test
./start.sh
```

服务启动后：
- HTTP API: `http://localhost:8080`
- RTSP: `rtsp://localhost:554`
- 录制目录: `record/live/`

### 2. 运行摄像头测试

**🥇 推荐：极简版测试（最快、最可靠）**

```bash
# 快速测试（默认 60 秒录制）
./test_minimal.sh

# 快速测试（30 秒录制）
RECORD_DURATION=30 ./test_minimal.sh
```

**极简版特点：**
- ✅ 零检查，直接开始录制
- ✅ 无并发，无变量作用域问题
- ✅ 代码简单，最可靠
- ✅ 执行最快（~90 秒）
- ✅ 输出清晰

---

**🥈 备选：简化版测试（包含流状态检查）**

```bash
# 包含 API 流检查
./test_simple.sh

# 快速测试（30 秒录制）
RECORD_DURATION=30 ./test_simple.sh
```

**简化版特点：**
- ✅ 跳过 ffprobe 的 RTSP 检查
- ✅ 通过 HTTP API 检查流状态
- ✅ 无并发，可靠性高
- ✅ 较快（~100 秒）

---

**🥉 高级：完整版测试（详细诊断）**

仅用于故障排查和详细诊断：

#### 步骤 1: 检查摄像头连接（可选）

```bash
# 快速检查所有摄像头是否在线
./check_cameras.sh
```

#### 步骤 2: 运行完整测试

```bash
# 运行摄像头测试（默认录制 60 秒）
./test_cameras.sh

# 自定义录制时长
RECORD_DURATION=120 ./test_cameras.sh

# 高并发测试（加快检查速度）
CHECK_CONCURRENCY=10 RECORD_CONCURRENCY=18 ./test_cameras.sh
```

**完整版特点：**
- ✅ 包含网络 ping 测试
- ✅ 包含 RTSP 流检查（但不阻塞）
- ✅ 更详细的诊断信息
- ⚠️  执行时间较长

**已配置 18 个摄像头：**

海康威视 (9 个):
- camera1-3: 192.168.12.71-73
- camera4-6: 192.168.10.111-113
- camera7-9: 192.168.12.31-33

大华 (9 个):
- camera10-12: 192.168.12.11-13
- camera13-15: 192.168.12.61-63
- camera16-18: 192.168.12.91-93

详细列表请查看: `cat CAMERAS.md`

### 3. 其他测试方式

**方式 A: 使用 FFmpeg 推流**

```bash
# 从文件推流
ffmpeg -re -i your-video.mp4 -c copy -f rtsp rtsp://localhost:554/live/test1

# 从摄像头推流
ffmpeg -f avfoundation -i "0:0" -c:v libx264 -c:a aac -f rtsp rtsp://localhost:554/live/test1
```

**方式 B: 配置拉流**

编辑 `config.yaml`，取消注释并配置 RTSP 源：

```yaml
rtsp:
  pull:
    live/test1: rtsp://your-rtsp-source-url
```

### 3. 测试录制

**方式 A: 自动录制（推荐）**

配置文件中已启用 `onpub.record`，流发布时自动开始录制。

**方式 B: 手动录制**

```bash
# 开始录制
curl -X POST 'http://localhost:8080/mp4/api/StartRecord?streamPath=live/test1'

# 停止录制
curl -X POST 'http://localhost:8080/mp4/api/StopRecord?streamPath=live/test1'
```

**方式 C: 使用自动化测试脚本**

```bash
# 配置 RTSP 源（可选）
export RTSP_SOURCE_1='rtsp://your-source-1'
export RTSP_SOURCE_2='rtsp://your-source-2'

# 运行测试（默认录制 60 秒）
./test_record.sh

# 自定义录制时长
RECORD_DURATION=120 ./test_record.sh
```

### 4. 查看录制文件

```bash
# 查看文件列表
ls -lh record/live/

# 播放录制文件
ffplay record/live/your-recording.mp4

# 检查文件信息
ffprobe record/live/your-recording.mp4
```

## 目录结构

```
record-test/
├── main.go              # 主程序
├── config.yaml          # 配置文件
├── start.sh             # 快速启动脚本
├── test_record.sh       # 自动化测试脚本
├── record/              # 录制文件目录
│   └── live/
├── logs/                # 日志目录
└── record-test.db       # SQLite 数据库
```

## 配置说明

### 本地存储配置

```yaml
global:
  storage:
    local:
      path: "record"
```

### MP4 录制配置

```yaml
mp4:
  enable: true
  onpub:
    record:
      ^live/.+:
          fragment: 0  # 0 = 不分片，录制完整文件
          filepath: record/$0
          storage:
            local:
              path: ""
```

### RTSP 拉流配置

```yaml
rtsp:
  pull:
    live/test1: rtsp://your-rtsp-source-url
    live/test2: rtsp://your-rtsp-source-url-2
```

## API 接口

### 开始录制

```bash
POST http://localhost:8080/mp4/api/StartRecord?streamPath=live/test1
```

### 停止录制

```bash
POST http://localhost:8080/mp4/api/StopRecord?streamPath=live/test1
```

### 查询录制列表

```bash
GET http://localhost:8080/mp4/api/List
```

### 删除录制

```bash
DELETE http://localhost:8080/mp4/api/Delete?id=<record_id>
```

## 测试检查点

录制完成后，验证以下内容：

1. ✓ **文件生成** - `record/live/` 目录下有 `.mp4` 文件
2. ✓ **文件大小** - 文件大小 > 0，且符合录制时长
3. ✓ **文件可播放** - 使用 VLC 或 ffplay 能正常播放
4. ✓ **MOOV box 位置** - 使用 `ffprobe` 检查文件结构
5. ✓ **日志无错误** - 检查 `logs/m7s.log` 无 error

### 检查 MOOV box 位置

```bash
# 检查文件结构
ffprobe -v trace record/live/your-file.mp4 2>&1 | grep -i moov

# 或使用 mp4info（如果安装了 mp4v2）
mp4info record/live/your-file.mp4
```

MOOV box 应该在文件开头（fast start），这样才能在浏览器中流式播放。

## 故障排查

### 服务无法启动

```bash
# 检查端口占用
lsof -i :8080
lsof -i :554

# 查看日志
tail -f logs/m7s.log
```

### 录制文件为空或损坏

1. 检查磁盘空间: `df -h`
2. 检查日志错误: `grep -i error logs/m7s.log`
3. 检查流是否正常: `ffprobe rtsp://localhost:554/live/test1`

### 无法播放录制文件

1. 检查文件大小: `ls -lh record/live/`
2. 检查文件完整性: `ffprobe record/live/your-file.mp4`
3. 查看 trailer 写入日志: `grep "write trailer" logs/m7s.log`

## 下一步

本地录制测试通过后，可以进行：

1. **S3 上传测试** - 修改配置使用 S3 存储
2. **性能测试** - 多路并发录制
3. **稳定性测试** - 长时间录制

## 停止服务

```bash
# 方式 1: Ctrl+C 停止前台进程

# 方式 2: 杀死进程
pkill -f 'go run.*main.go'
```

## 清理环境

```bash
# 清理录制文件
rm -rf record/live/*.mp4

# 清理日志
rm -rf logs/*

# 清理数据库
rm -f record-test.db
```
