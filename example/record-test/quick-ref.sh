#!/bin/bash

# 快速参考 - 常用命令

cat << 'EOF'

╔══════════════════════════════════════════════════════════════╗
║           Monibuca 录制测试 - 快速参考                        ║
╚══════════════════════════════════════════════════════════════╝

📍 位置
  cd /Users/lirichen/Work/XiDingCloud/monibuca-v5/example/record-test

🚀 启动服务
  ./start.sh

🧪 测试方式

  方式 1: 极简版测试（最快，零检查）⭐⭐⭐
    ./test_minimal.sh

  方式 2: 简化版测试（推荐，跳过 ffprobe）⭐⭐
    ./test_simple.sh

  方式 3: 完整测试（包含所有检查）⭐
    ./test_cameras.sh

  方式 4: 快速测试（30 秒录制）
    RECORD_DURATION=30 ./test_minimal.sh

  方式 5: 检查摄像头连接
    ./check_cameras.sh

  方式 6: 交互式测试
    ./test_interactive.sh

📊 测试规模
  摄像头总数: 18 个
  海康威视: 9 个
  大华: 9 个
  详细列表: cat CAMERAS.md

📁 重要路径
  录制文件: record/live/
  日志文件: logs/m7s.log
  配置文件: config.yaml

🔍 检查录制结果
  # 查看文件
  ls -lh record/live/

  # 播放文件
  ffplay record/live/your-file.mp4

  # 检查文件信息
  ffprobe record/live/your-file.mp4

  # 查看日志
  tail -f logs/m7s.log

🛑 停止服务
  Ctrl+C 或 pkill -f 'go run.*main.go'

🧹 清理环境
  rm -rf record/live/*.mp4 logs/* *.db

📚 详细文档
  cat README.md

EOF
