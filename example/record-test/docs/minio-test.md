# MinIO 上传测试说明

## 更新内容

### 1. config.yaml 配置
已配置 S3/MinIO 存储：
- 端点: `storage-dev.xiding.tech`
- 存储桶: `vidu-media-bucket`
- 路径前缀: `recordings`
- 使用 SSL: `true`

### 2. test_minimal.sh 增强功能

#### 新增 MinIO 文件检查
脚本现在会在录制完成后自动检查 MinIO 上传结果，包括：

1. **文件存在性检查**
   - 验证所有 35 个摄像头的录制文件是否成功上传到 MinIO

2. **元数据提取**
   - `X-Amz-Meta-Video-Duration-Ms`: 视频时长（毫秒）
   - `X-Amz-Meta-Video-Size-Bytes`: 视频大小（字节）

3. **统计信息**
   - 成功上传数量
   - 上传失败数量
   - 总时长（分钟:秒）
   - 总大小（MB）

#### 检查工具支持

**推荐方式：使用 mc (MinIO Client)**
```bash
# macOS 安装
brew install minio/stable/mc

# Linux 安装
wget https://dl.min.io/client/mc/release/linux-amd64/mc
chmod +x mc
sudo mv mc /usr/local/bin/
```

使用 `mc` 可以获取完整的元数据信息，包括视频时长和大小。

**备用方式：使用 curl**
如果未安装 `mc`，脚本会使用 `curl` 进行基本的文件存在性检查（但无法获取元数据）。

## 使用方法

### 运行测试
```bash
cd /Users/lirichen/Work/XiDingCloud/monibuca-v5/example/record-test

# 启动 Monibuca 服务
./start.sh

# 运行录制测试（默认 5 分钟）
./test_minimal.sh

# 自定义录制时长（例如 10 分钟 = 600 秒）
RECORD_DURATION=600 ./test_minimal.sh
```

### 输出示例

```
==========================================
  MinIO 上传结果检查
==========================================

MinIO 配置:
  端点: storage-dev.xiding.tech
  存储桶: vidu-media-bucket
  路径前缀: recordings
  检查工具: mc (MinIO Client)

检查上传文件:
----------------------------------------
摄像头               状态       时长(秒)        大小(字节)
----------------------------------------
camera1              ✓ 已上传   300            45678912
camera2              ✓ 已上传   300            45234567
camera3              ✓ 已上传   300            46123456
...
camera35             ✓ 已上传   300            45987654
----------------------------------------

统计信息:
  成功上传: 35/35
  上传失败: 0
  总时长: 175分0秒 (10500秒)
  总大小: 1567 MB (1643789312 字节)

✓ 所有文件已成功上传到 MinIO
```

## 技术实现

### MP4 插件元数据设置
MP4 插件在录制完成后会自动设置以下元数据：
```go
// plugin/mp4/pkg/record.go
t.file.SetMetadata("video-size-bytes", fmt.Sprintf("%d", fileSize))
t.file.SetMetadata("video-duration-ms", fmt.Sprintf("%d", t.durationMs))
```

### S3 存储元数据上传
S3 存储实现支持用户自定义元数据：
```go
// pkg/storage/s3.go
uploadInput := &s3manager.UploadInput{
    Bucket:      aws.String(w.storage.config.Bucket),
    Key:         aws.String(w.objectKey),
    Body:        w.tempFile,
    ContentType: aws.String("application/octet-stream"),
    Metadata:    aws.StringMap(w.metadata), // 携带元数据
}
```

### MinIO 对象路径结构
```
vidu-media-bucket/
└── recordings/
    └── live/
        ├── camera1/
        │   └── camera1.mp4
        ├── camera2/
        │   └── camera2.mp4
        └── ...
```

## 故障排查

### 1. 上传失败
检查 Monibuca 日志：
```bash
tail -100 logs/m7s.log | grep -i "s3\|upload\|error"
```

### 2. 元数据缺失
- 确保使用的是最新版本的 MP4 插件
- 检查 S3 存储配置是否正确
- 验证 MinIO 服务是否支持用户元数据

### 3. mc 命令失败
```bash
# 测试 mc 连接
mc alias set minio-test https://storage-dev.xiding.tech xidinguser 'U2FsdGVkX1/7uyvj0trCzSNFsfDZ66dMSAEZjNlvW1c='

# 列出存储桶
mc ls minio-test/vidu-media-bucket/recordings/

# 查看单个文件信息
mc stat --json minio-test/vidu-media-bucket/recordings/live/camera1/camera1.mp4
```

## 注意事项

1. **等待时间**: 脚本在停止录制后会等待 30 秒，确保文件完全上传到 MinIO
2. **网络要求**: 需要能够访问 `storage-dev.xiding.tech`
3. **认证信息**: 确保 `config.yaml` 中的 AccessKey 和 SecretKey 正确
4. **存储空间**: 35 个摄像头录制 10 分钟约需 1.5-2GB 存储空间
