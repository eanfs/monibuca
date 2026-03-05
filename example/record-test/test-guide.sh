#!/bin/bash

cat << 'EOF'

╔══════════════════════════════════════════════════════════════╗
║         摄像头录制测试 - 完整指南                              ║
╚══════════════════════════════════════════════════════════════╝

📍 测试环境
  位置: /Users/lirichen/Work/XiDingCloud/monibuca-v5/example/record-test
  配置: config.yaml (已配置 18 个摄像头)

🎥 测试摄像头（18 个）
  海康威视 (9 个):
    1-3:   192.168.12.71-73
    4-6:   192.168.10.111-113
    7-9:   192.168.12.31-33

  大华 (9 个):
    10-12: 192.168.12.11-13
    13-15: 192.168.12.61-63
    16-18: 192.168.12.91-93

🚀 快速测试（3 步）

  第 1 步: 启动服务
    cd /Users/lirichen/Work/XiDingCloud/monibuca-v5/example/record-test
    ./start.sh

  第 2 步: 运行测试（新终端）
    cd /Users/lirichen/Work/XiDingCloud/monibuca-v5/example/record-test
    ./test_cameras.sh

  第 3 步: 查看结果
    ls -lh record/live/
    ffplay record/live/*.mp4

📊 测试流程

  ✓ 检查 Monibuca 服务状态
  ✓ 检查摄像头网络连接 (ping)
  ✓ 检查 RTSP 流可用性 (ffprobe)
  ✓ 等待流就绪 (最多 30 秒)
  ✓ 开始录制 (通过 API)
  ✓ 录制 60 秒 (可自定义)
  ✓ 停止录制
  ✓ 验证文件完整性 (ffprobe)
  ✓ 显示测试结果统计

🔧 自定义选项

  # 修改录制时长
  RECORD_DURATION=120 ./test_cameras.sh

  # 修改并发数（加快检查速度）
  CHECK_CONCURRENCY=10 RECORD_CONCURRENCY=18 ./test_cameras.sh

  # 修改服务地址
  NODE_IP=192.168.1.100 HTTP_PORT=8080 ./test_cameras.sh

📁 输出文件

  录制文件: record/live/*.mp4
  日志文件: logs/m7s.log
  数据库: record-test.db

🔍 验证检查点

  1. 文件生成 ✓
     ls -lh record/live/

  2. 文件大小正常 ✓
     du -h record/live/*.mp4

  3. 文件可播放 ✓
     ffplay record/live/*.mp4

  4. 文件完整性 ✓
     ffprobe record/live/*.mp4

  5. MOOV box 位置 ✓
     ffprobe -v trace record/live/*.mp4 2>&1 | grep -i moov

  6. 日志无错误 ✓
     grep -i error logs/m7s.log

📝 预期结果

  成功的测试应该显示:
  - 总摄像头数: 18
  - 可用摄像头: 18 (如果网络正常)
  - 就绪的流: 18
  - 录制的流: 18
  - 成功的录制: 18
  - 所有录制均成功！

  注意: 实际可用摄像头数量取决于网络连接和摄像头在线状态

⚠️  常见问题

  问题: 摄像头网络不可达
  解决: 检查网络连接，确保可以 ping 通摄像头 IP

  问题: RTSP 流连接失败
  解决: 检查用户名密码，确认摄像头 RTSP 服务正常

  问题: 流未就绪
  解决: 等待更长时间，或检查日志 logs/m7s.log

  问题: 录制文件损坏
  解决: 检查磁盘空间，查看日志中的 "write trailer" 信息

🛠️  其他测试工具

  交互式测试:
    ./test_interactive.sh

  自动化测试:
    ./test_record.sh

  快速参考:
    ./quick-ref.sh

📚 详细文档
  cat README.md

EOF
