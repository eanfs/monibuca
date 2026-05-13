#!/usr/bin/env bash
set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."
source lib/_assert.sh
source lib/sampler.sh

# 起一个 sleep 进程当 mock target
sleep 10 &
pid=$!
trap "kill $pid 2>/dev/null" EXIT

out=$(mktemp)
sample_pid_to_csv "$pid" "$out" 3 1 &
sampler_pid=$!
sleep 4
wait $sampler_pid 2>/dev/null

lines=$(wc -l < "$out" | tr -d ' ')
# header + ≥3 samples
[ "$lines" -ge 4 ] && pass=1 || pass=0
assert_eq "1" "$pass" "sampler 写 ≥4 行 (header + 3 samples), 实际 $lines"

header=$(head -1 "$out")
assert_match "ts,cpu_pct,rss_mb" "$header" "header 含字段"

rm -f "$out"
report_results
