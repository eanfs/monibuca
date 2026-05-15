#!/usr/bin/env bash
set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."
source lib/_assert.sh
source lib/probe.sh

# Case 1: 4K h264 25fps
install_mock_bin ffprobe '
cat <<EOF
width=3840
height=2160
codec_name=h264
r_frame_rate=25/1
bit_rate=8388608
EOF
exit 0
'
result=$(probe_stream "rtsp://fake/4k")
assert_match "^width=3840 height=2160 fps=25 codec=h264 bitrate_kbps=8388$" "$result" "4K probe"

# Case 2: 25000/1000 形式 fps
install_mock_bin ffprobe '
cat <<EOF
width=1920
height=1080
codec_name=hevc
r_frame_rate=25000/1000
bit_rate=N/A
EOF
exit 0
'
result=$(probe_stream "rtsp://fake/1080")
assert_match "^width=1920 height=1080 fps=25 codec=hevc bitrate_kbps=0$" "$result" "1080p hevc + N/A bitrate"

# Case 3: ffprobe 失败
install_mock_bin ffprobe 'echo "Connection refused" >&2; exit 1'
result=$(probe_stream "rtsp://fake/down" 2>/dev/null)
rc=$?
assert_eq "1" "$rc" "ffprobe 失败 → probe_stream 返回 1"

cleanup_mocks
report_results
