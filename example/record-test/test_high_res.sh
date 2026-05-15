#!/usr/bin/env bash
# 跑某分辨率分组的并发录制测试 + 出报告。
# Usage:
#   ./test_high_res.sh --group=4k
#   ./test_high_res.sh --group=2k,4k --duration=600
#   ./test_high_res.sh --group=all --duration=120

set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

GROUP="4k"
DURATION="${RECORD_DURATION:-300}"
HTTP_PORT="${HTTP_PORT:-8080}"
NODE_IP="${NODE_IP:-localhost}"
# 录制文件实际位置 = $STORAGE_ROOT/$filePath/$fileName
# 当前 config.yaml 用 S3 配置 + 编译时未带 -tags s3 → fallback 到 local path=. （cwd）
# 如换 config，把 STORAGE_ROOT 改成对应 path（如 record）即可
STORAGE_ROOT="${STORAGE_ROOT:-.}"

for arg in "$@"; do
    case "$arg" in
        --group=*)    GROUP="${arg#*=}" ;;
        --duration=*) DURATION="${arg#*=}" ;;
        -h|--help)
            echo "Usage: $0 [--group=4k|2k|fhd|hd|sd|all|2k,4k] [--duration=300]"
            exit 0
            ;;
        *) echo "未知参数: $arg" >&2; exit 2 ;;
    esac
done

[ -f streams_meta.json ] || { echo "缺 streams_meta.json，先跑 ./probe_streams.sh" >&2; exit 2; }
command -v jq >/dev/null || { echo "缺 jq" >&2; exit 2; }

source lib/sampler.sh
source lib/verify.sh

# 1. 过滤流（先做，避免无流时白起服务）
if [ "$GROUP" = "all" ]; then
    filter_jq='select(.probe_status=="ok")'
else
    classes=$(echo "$GROUP" | tr ',' '\n' | tr '[:lower:]' '[:upper:]' | jq -R . | jq -s .)
    filter_jq="select(.probe_status==\"ok\" and (.resolution_class as \$c | $classes | index(\$c)))"
fi

mapfile -t entries < <(jq -c ".streams[] | $filter_jq" streams_meta.json)
total=${#entries[@]}
if [ "$total" -eq 0 ]; then
    echo "分组 $GROUP 下无流。当前分布："
    jq -r '.streams | group_by(.resolution_class) | map({class: .[0].resolution_class, count: length}) | .[] | "  \(.class): \(.count)"' streams_meta.json
    exit 1
fi

# 2. 校验服务
if ! curl -sf "http://$NODE_IP:$HTTP_PORT/api/sysinfo" >/dev/null; then
    echo "monibuca 未运行，请先 ./start.sh" >&2
    exit 1
fi

ts=$(date +%Y%m%d-%H%M%S)
report="reports/report-$ts.md"
metrics="reports/metrics-$ts.csv"

echo "=========================================="
echo "  高分辨率录制测试"
echo "=========================================="
echo "分组:   $GROUP"
echo "流数:   $total"
echo "时长:   ${DURATION}s"
echo "报告:   $report"
echo "采样:   $metrics"
echo ""

# 3. 启动 sampler
pid=$(get_m7s_pid "$HTTP_PORT")
if [ -z "$pid" ]; then
    echo "无法定位 monibuca PID" >&2; exit 1
fi
echo "monibuca PID: $pid"
sample_count=$((DURATION + 20))
sample_pid_to_csv "$pid" "$metrics" "$sample_count" 1 &
sampler_bgpid=$!
trap 'kill $sampler_bgpid 2>/dev/null' EXIT

# 4. 清旧录制
echo "[1/5] 清理录制目录"
for e in "${entries[@]}"; do
    sp=$(echo "$e" | jq -r '.stream_path')
    curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$sp" >/dev/null 2>&1
done
# 清掉本组流目录（filePath 头一段，例如 "live"）
for e in "${entries[@]}"; do
    sp=$(echo "$e" | jq -r '.stream_path')
    rm -rf "$STORAGE_ROOT/$sp" 2>/dev/null || true
done
sleep 3

# 5. 启动录制
echo "[2/5] 启动 $total 路录制"
started=0
for e in "${entries[@]}"; do
    sp=$(echo "$e" | jq -r '.stream_path')
    name="${sp##*/}.mp4"
    resp=$(curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/start/$sp" \
        -H 'Content-Type: application/json' \
        -d "{\"duration\":\"${DURATION}\",\"fragment\":\"0\",\"filePath\":\"$sp\",\"fileName\":\"$name\"}")
    if echo "$resp" | grep -q '"code":0'; then
        started=$((started + 1))
    else
        echo "  ✗ $sp: $resp"
    fi
done
echo "已启动 $started / $total"

# 6. 录制
echo "[3/5] 录制中 (${DURATION}s)"
for i in $(seq 1 "$DURATION"); do
    echo -ne "\r  $i/${DURATION}s"
    sleep 1
done
echo ""

# 7. 停止
echo "[4/5] 停止录制"
for e in "${entries[@]}"; do
    sp=$(echo "$e" | jq -r '.stream_path')
    curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$sp" >/dev/null
done

echo "  等待 trailer 写入 (15s)..."
sleep 15
kill $sampler_bgpid 2>/dev/null || true

# 8. 校验 + 渲染报告
echo "[5/5] 校验并写报告"
{
    echo "# 录制测试报告"
    echo ""
    echo "- 时间: $(date '+%Y-%m-%d %H:%M:%S')"
    echo "- 分组: $GROUP"
    echo "- 流数: $total"
    echo "- 录制时长: ${DURATION}s"
    echo ""
    echo "## 逐流结果"
    echo ""
    echo "| 流 | 源分辨率 | 编码 | 源 fps | 大小 | 实测时长 | 实际分辨率 | 结果 | detail |"
    echo "|---|---|---|---|---|---|---|---|---|"
} > "$report"

pass=0; warn=0; fail=0
for e in "${entries[@]}"; do
    sp=$(echo "$e" | jq -r '.stream_path')
    w=$(echo "$e"  | jq -r '.width')
    h=$(echo "$e"  | jq -r '.height')
    codec=$(echo "$e" | jq -r '.codec')
    fps=$(echo "$e"   | jq -r '.fps')
    file="$STORAGE_ROOT/$sp/${sp##*/}.mp4"
    v=$(verify_stream "$file" "$w" "$h" "$DURATION")
    status=$(echo "$v" | cut -f1)
    size=$(echo "$v"   | cut -f2)
    dur=$(echo "$v"    | cut -f3)
    awxh=$(echo "$v"   | cut -f4)
    detail=$(echo "$v" | cut -f5)
    case "$status" in
        PASS) pass=$((pass+1)) ;;
        WARN) warn=$((warn+1)) ;;
        FAIL) fail=$((fail+1)) ;;
    esac
    size_h=$(awk -v s="$size" 'BEGIN{
        if (s>=1073741824) printf "%.1fG", s/1073741824;
        else if (s>=1048576) printf "%.0fM", s/1048576;
        else if (s>=1024) printf "%.0fK", s/1024;
        else print s"B"
    }')
    echo "| $sp | ${w}x${h} | $codec | $fps | $size_h | ${dur}s | $awxh | $status | $detail |" >> "$report"
done

# 性能摘要
peak_cpu=$(awk -F, 'NR>1 && $2>=0 {if($2+0>m) m=$2+0} END{print m+0}' "$metrics")
peak_rss=$(awk -F, 'NR>1 && $3>=0 {if($3+0>m) m=$3+0} END{print m+0}' "$metrics")
peak_fd=$(awk  -F, 'NR>1 && $4>=0 {if($4+0>m) m=$4+0} END{print m+0}' "$metrics")
{
    echo ""
    echo "## 汇总"
    echo ""
    echo "- 通过: $pass / $total"
    echo "- 警告: $warn"
    echo "- 失败: $fail"
    echo ""
    echo "## 性能峰值"
    echo ""
    echo "- 峰值 CPU: ${peak_cpu}%"
    echo "- 峰值 RSS: ${peak_rss} MB"
    echo "- 峰值 fd:  ${peak_fd}"
    echo "- 采样原始数据: \`$(basename "$metrics")\`"
} >> "$report"

echo ""
echo "=========================================="
echo "完成"
echo "  PASS=$pass WARN=$warn FAIL=$fail"
echo "  报告: $report"
echo "=========================================="
