# 摄像头列表

## 总览

- **总数**: 18 个摄像头
- **海康威视**: 9 个
- **大华**: 9 个

## 详细列表

### 海康威视摄像头 (9 个)

#### 192.168.12.x 系列 (3 个)
| ID | 流路径 | IP 地址 | RTSP URL |
|----|--------|---------|----------|
| camera1 | live/camera1 | 192.168.12.71 | rtsp://admin:a1234567@192.168.12.71/ch1/main/av_stream |
| camera2 | live/camera2 | 192.168.12.72 | rtsp://admin:a1234567@192.168.12.72/ch1/main/av_stream |
| camera3 | live/camera3 | 192.168.12.73 | rtsp://admin:a1234567@192.168.12.73/ch1/main/av_stream |

#### 192.168.10.x 系列 (3 个)
| ID | 流路径 | IP 地址 | RTSP URL |
|----|--------|---------|----------|
| camera4 | live/camera4 | 192.168.10.111 | rtsp://admin:a1234567@192.168.10.111/ch1/main/av_stream |
| camera5 | live/camera5 | 192.168.10.112 | rtsp://admin:a1234567@192.168.10.112/ch1/main/av_stream |
| camera6 | live/camera6 | 192.168.10.113 | rtsp://admin:a1234567@192.168.10.113/ch1/main/av_stream |

#### 192.168.12.3x 系列 (3 个)
| ID | 流路径 | IP 地址 | RTSP URL |
|----|--------|---------|----------|
| camera7 | live/camera7 | 192.168.12.31 | rtsp://admin:a1234567@192.168.12.31/ch1/main/av_stream |
| camera8 | live/camera8 | 192.168.12.32 | rtsp://admin:a1234567@192.168.12.32/ch1/main/av_stream |
| camera9 | live/camera9 | 192.168.12.33 | rtsp://admin:a1234567@192.168.12.33/ch1/main/av_stream |

### 大华摄像头 (9 个)

#### 192.168.12.1x 系列 (3 个)
| ID | 流路径 | IP 地址 | RTSP URL |
|----|--------|---------|----------|
| camera10 | live/camera10 | 192.168.12.11 | rtsp://admin:a1234567@192.168.12.11:554/cam/realmonitor?channel=1&subtype=0 |
| camera11 | live/camera11 | 192.168.12.12 | rtsp://admin:a1234567@192.168.12.12:554/cam/realmonitor?channel=1&subtype=0 |
| camera12 | live/camera12 | 192.168.12.13 | rtsp://admin:a1234567@192.168.12.13:554/cam/realmonitor?channel=1&subtype=0 |

#### 192.168.12.6x 系列 (3 个)
| ID | 流路径 | IP 地址 | RTSP URL |
|----|--------|---------|----------|
| camera13 | live/camera13 | 192.168.12.61 | rtsp://admin:a1234567@192.168.12.61:554/cam/realmonitor?channel=1&subtype=0 |
| camera14 | live/camera14 | 192.168.12.62 | rtsp://admin:a1234567@192.168.12.62:554/cam/realmonitor?channel=1&subtype=0 |
| camera15 | live/camera15 | 192.168.12.63 | rtsp://admin:a1234567@192.168.12.63:554/cam/realmonitor?channel=1&subtype=0 |

#### 192.168.12.9x 系列 (3 个)
| ID | 流路径 | IP 地址 | RTSP URL |
|----|--------|---------|----------|
| camera16 | live/camera16 | 192.168.12.91 | rtsp://admin:a1234567@192.168.12.91:554/cam/realmonitor?channel=1&subtype=0 |
| camera17 | live/camera17 | 192.168.12.92 | rtsp://admin:a1234567@192.168.12.92:554/cam/realmonitor?channel=1&subtype=0 |
| camera18 | live/camera18 | 192.168.12.93 | rtsp://admin:a1234567@192.168.12.93:554/cam/realmonitor?channel=1&subtype=0 |

## 网络拓扑

```
测试机器
    │
    ├─── 192.168.12.x 网段
    │    ├─── 192.168.12.71-73 (海康威视)
    │    ├─── 192.168.12.31-33 (海康威视)
    │    ├─── 192.168.12.11-13 (大华)
    │    ├─── 192.168.12.61-63 (大华)
    │    └─── 192.168.12.91-93 (大华)
    │
    └─── 192.168.10.x 网段
         └─── 192.168.10.111-113 (海康威视)
```

## 测试命令

### 测试单个摄像头

```bash
# 测试网络连接
ping 192.168.12.71

# 测试 RTSP 流
ffprobe -rtsp_transport tcp -v quiet -print_format json -show_streams \
  -i rtsp://admin:a1234567@192.168.12.71/ch1/main/av_stream

# 播放流
ffplay -rtsp_transport tcp rtsp://admin:a1234567@192.168.12.71/ch1/main/av_stream
```

### 测试所有摄像头

```bash
# 运行自动化测试
./test_cameras.sh

# 快速测试（30 秒录制）
RECORD_DURATION=30 ./test_cameras.sh

# 高并发测试
CHECK_CONCURRENCY=18 RECORD_CONCURRENCY=18 ./test_cameras.sh
```

## 故障排查

### 检查摄像头在线状态

```bash
# 批量 ping 测试
for ip in 192.168.12.{71..73} 192.168.10.{111..113} 192.168.12.{31..33} \
          192.168.12.{11..13} 192.168.12.{61..63} 192.168.12.{91..93}; do
    if ping -c 1 -W 1 $ip > /dev/null 2>&1; then
        echo "✓ $ip 在线"
    else
        echo "✗ $ip 离线"
    fi
done
```

### 检查 RTSP 服务

```bash
# 检查海康威视摄像头
for ip in 192.168.12.{71..73}; do
    echo "检查 $ip ..."
    timeout 5 ffprobe -rtsp_transport tcp -v quiet \
      -i rtsp://admin:a1234567@$ip/ch1/main/av_stream 2>&1 | head -1
done

# 检查大华摄像头
for ip in 192.168.12.{11..13}; do
    echo "检查 $ip ..."
    timeout 5 ffprobe -rtsp_transport tcp -v quiet \
      -i rtsp://admin:a1234567@$ip:554/cam/realmonitor?channel=1&subtype=0 2>&1 | head -1
done
```

## 性能考虑

### 并发录制

- **18 路并发录制**需要足够的系统资源
- **建议配置**:
  - CPU: 8 核以上
  - 内存: 16GB 以上
  - 磁盘: SSD，至少 100GB 可用空间
  - 网络: 千兆网卡

### 带宽估算

假设每路流 4Mbps（1080p H.264）:
- 18 路 × 4Mbps = 72Mbps
- 建议网络带宽: 100Mbps 以上

### 磁盘空间估算

假设每路流 4Mbps，录制 60 秒:
- 单路: 4Mbps × 60s = 240Mb = 30MB
- 18 路: 30MB × 18 = 540MB

录制 1 小时:
- 18 路: 30MB × 60 × 18 = 32.4GB

## 配置文件

所有摄像头已配置在 `config.yaml` 中的 `rtsp.pull` 部分，服务启动时会自动拉流。

## 录制文件位置

```
record/
└── live/
    ├── camera1/
    ├── camera2/
    ├── ...
    └── camera18/
```

每个摄像头的录制文件会保存在对应的子目录中。
