#!/usr/bin/env bash
set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."
source lib/_assert.sh
source lib/verify.sh

tmpdir=$(mktemp -d)
trap "rm -rf $tmpdir; cleanup_mocks" EXIT

# Case 1: 文件不存在 → FAIL
result=$(verify_stream "$tmpdir/nope.mp4" 3840 2160 60)
status=$(echo "$result" | cut -f1)
assert_eq "FAIL" "$status" "缺文件 → FAIL"

# Case 2: 文件大小为 0 → FAIL
touch "$tmpdir/empty.mp4"
result=$(verify_stream "$tmpdir/empty.mp4" 3840 2160 60)
status=$(echo "$result" | cut -f1)
assert_eq "FAIL" "$status" "空文件 → FAIL"

# 接下来需要存在且非空的文件
dd if=/dev/urandom of="$tmpdir/ok.mp4" bs=1024 count=10 >/dev/null 2>&1

# Case 3: 正常 4K 文件 + 时长偏差 1% → PASS
install_mock_bin ffprobe '
cat <<EOF
width=3840
height=2160
duration=59.4
EOF
exit 0
'
result=$(verify_stream "$tmpdir/ok.mp4" 3840 2160 60)
status=$(echo "$result" | cut -f1)
assert_eq "PASS" "$status" "正常 4K + 1% 偏差 → PASS"

# Case 4: 时长偏差 20% → WARN
install_mock_bin ffprobe '
cat <<EOF
width=3840
height=2160
duration=48.0
EOF
exit 0
'
result=$(verify_stream "$tmpdir/ok.mp4" 3840 2160 60)
status=$(echo "$result" | cut -f1)
assert_eq "WARN" "$status" "时长偏差 20% → WARN"

# Case 5: 分辨率不匹配 → FAIL
install_mock_bin ffprobe '
cat <<EOF
width=1920
height=1080
duration=60
EOF
exit 0
'
result=$(verify_stream "$tmpdir/ok.mp4" 3840 2160 60)
status=$(echo "$result" | cut -f1)
assert_eq "FAIL" "$status" "分辨率不匹配 → FAIL"

report_results
