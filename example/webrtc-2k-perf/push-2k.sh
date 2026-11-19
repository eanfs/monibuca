#!/usr/bin/env bash
# 向 Monibuca 推送 2560x1440@24 H264 测试流（testsrc2 合成源，无需素材文件）。
#
# 用法：
#   ./push-2k.sh                          # 推 rtmp://127.0.0.1/live/2k
#   RTMP_URL=rtmp://127.0.0.1:19350/live/2k ./push-2k.sh
#   DURATION=90 ./push-2k.sh              # 推 90 秒后自动退出（便于脚本化）
#
# 所有参数都可用环境变量覆盖；脚本会先打印实际执行的完整命令再 exec ffmpeg。
set -euo pipefail

RTMP_URL="${RTMP_URL:-rtmp://127.0.0.1/live/2k}"
SIZE="${SIZE:-2560x1440}"
FPS="${FPS:-24}"
BITRATE="${BITRATE:-10M}"
MAXRATE="${MAXRATE:-12M}"
BUFSIZE="${BUFSIZE:-20M}"
GOP="${GOP:-48}"
PRESET="${PRESET:-veryfast}"
PIX_FMT="${PIX_FMT:-yuv420p}"
# DURATION=0 表示一直推流直到 Ctrl-C
DURATION="${DURATION:-0}"

if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "错误：未找到 ffmpeg，请先安装（macOS: brew install ffmpeg）" >&2
  exit 1
fi

args=(
  ffmpeg -hide_banner -loglevel info
  -re -f lavfi -i "testsrc2=size=${SIZE}:rate=${FPS}"
  -c:v libx264 -preset "${PRESET}" -b:v "${BITRATE}" -maxrate "${MAXRATE}" -bufsize "${BUFSIZE}"
  -g "${GOP}" -pix_fmt "${PIX_FMT}"
)

if [ "${DURATION}" != "0" ]; then
  args+=(-t "${DURATION}")
fi

args+=(-f flv "${RTMP_URL}")

# 单行打印，方便复制复现
printf 'Running:'
printf ' %q' "${args[@]}"
printf '\n'

exec "${args[@]}"
