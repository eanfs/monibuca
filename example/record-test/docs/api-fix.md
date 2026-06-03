# API 接口修复说明

## 问题

测试脚本使用了错误的 API 接口：
- ❌ `POST /mp4/api/StartRecord?streamPath={path}`
- ❌ `POST /mp4/api/StopRecord?streamPath={path}`

## 正确的 API

参考 `example/cluster/start_test_cluster.sh`，正确的 API 应该是：

### 1. 开始录制

```bash
POST /mp4/api/start/{streamPath}
Content-Type: application/json

{
  "fragment": "300s",      # 可选：分片时长
  "filePath": "xxx",       # 可选：文件路径
  "fileName": "xxx.mp4"    # 可选：文件名
}
```

**示例：**
```bash
curl -X POST "http://localhost:8080/mp4/api/start/live/camera1" \
  -H "Content-Type: application/json" \
  -d '{}'
```

### 2. 停止录制

```bash
POST /mp4/api/stop/{streamPath}
```

**示例：**
```bash
curl -X POST "http://localhost:8080/mp4/api/stop/live/camera1"
```

### 3. 添加拉流代理

```bash
POST /api/proxy/pull/add
Content-Type: application/json

{
  "parentID": 0,
  "name": "proxy-name",
  "type": "rtsp",
  "streamPath": "live/camera1",
  "pullOnStart": true,
  "pullURL": "rtsp://..."
}
```

**示例：**
```bash
curl -X POST "http://localhost:8080/api/proxy/pull/add" \
  -H "Content-Type: application/json" \
  -d '{
    "parentID": 0,
    "name": "proxy-camera1",
    "type": "rtsp",
    "streamPath": "live/camera1",
    "pullOnStart": true,
    "pullURL": "rtsp://admin:password@192.168.1.100/stream"
  }'
```

## 已修复的文件

✅ `test_minimal.sh` - 极简版测试
✅ `test_simple.sh` - 简化版测试
✅ `test_cameras.sh` - 完整版测试
✅ `test_interactive.sh` - 交互式测试

## 修复内容

### 开始录制

**修复前：**
```bash
curl -X POST "http://localhost:8080/mp4/api/StartRecord?streamPath=live/camera1"
```

**修复后：**
```bash
curl -X POST "http://localhost:8080/mp4/api/start/live/camera1" \
  -H "Content-Type: application/json" \
  -d '{}'
```

### 停止录制

**修复前：**
```bash
curl -X POST "http://localhost:8080/mp4/api/StopRecord?streamPath=live/camera1"
```

**修复后：**
```bash
curl -X POST "http://localhost:8080/mp4/api/stop/live/camera1"
```

## 测试验证

修复后，可以通过以下方式验证：

```bash
# 1. 启动服务
./start.sh

# 2. 等待拉流
sleep 20

# 3. 测试开始录制
curl -X POST "http://localhost:8080/mp4/api/start/live/camera1" \
  -H "Content-Type: application/json" \
  -d '{}'

# 预期响应：
# {"code":0,"msg":"success"}

# 4. 等待录制
sleep 30

# 5. 测试停止录制
curl -X POST "http://localhost:8080/mp4/api/stop/live/camera1"

# 预期响应：
# {"code":0,"msg":"success"}

# 6. 检查录制文件
ls -lh record/live/camera1/
```

## 其他相关 API

### 查询录制列表

```bash
GET /mp4/api/List
```

### 删除录制

```bash
DELETE /mp4/api/Delete?id={record_id}
```

### 查询流状态

```bash
GET /api/stream/{streamPath}
```

**示例：**
```bash
curl "http://localhost:8080/api/stream/live/camera1"
```

## 注意事项

1. **streamPath 参数**：在 URL 路径中，不是查询参数
2. **Content-Type**：开始录制需要 `application/json` 头
3. **请求体**：开始录制需要 JSON body（可以为空 `{}`）
4. **响应格式**：成功返回 `{"code":0,"msg":"success"}`

## 参考

- 集群测试脚本：`example/cluster/start_test_cluster.sh`
- 第 138 行：开始录制 API
- 第 160 行：停止录制 API
- 第 110 行：拉流代理 API
