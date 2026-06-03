#!/usr/bin/env bash
# 探测 config.yaml 中 rtsp.pull 列表的所有摄像头，输出 streams_meta.json。
# Usage:
#   ./probe_streams.sh
#   ./probe_streams.sh --timeout=30
#   ./probe_streams.sh --config=config.yaml --out=streams_meta.json

set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

CONFIG="config.yaml"
OUT="streams_meta.json"
TIMEOUT=15

for arg in "$@"; do
    case "$arg" in
        --config=*) CONFIG="${arg#*=}" ;;
        --out=*)    OUT="${arg#*=}" ;;
        --timeout=*) TIMEOUT="${arg#*=}" ;;
        -h|--help)
            echo "Usage: $0 [--config=config.yaml] [--out=streams_meta.json] [--timeout=15]"
            exit 0
            ;;
        *) echo "未知参数: $arg" >&2; exit 2 ;;
    esac
done

[ -f "$CONFIG" ] || { echo "找不到 $CONFIG" >&2; exit 2; }
command -v ffprobe >/dev/null || { echo "缺 ffprobe" >&2; exit 2; }
command -v jq      >/dev/null || { echo "缺 jq" >&2; exit 2; }

source lib/probe.sh
source lib/classify.sh

# 从 config.yaml 抽 rtsp.pull 下的 "  key: value" 对（4 空格缩进）。
extract_pulls() {
    awk '
        /^rtsp:/   { in_rtsp=1; next }
        in_rtsp && /^  pull:/ { in_pull=1; next }
        in_pull && /^    [^ #]/ {
            sub(/^    /, "")
            n = index($0, ":")
            if (n == 0) next
            key = substr($0, 1, n-1)
            val = substr($0, n+2)
            sub(/^[ \t]+/, "", val)
            sub(/[ \t\r]+$/, "", val)
            print key "\t" val
            next
        }
        in_pull && /^[^ ]/ { in_pull=0; in_rtsp=0 }
        in_rtsp && /^[^ ]/ && !/^rtsp:/ { in_rtsp=0 }
    ' "$CONFIG"
}

mask_url() {
    echo "$1" | sed -E 's#(rtsp://[^:]+:)[^@]+@#\1***@#'
}

detect_vendor() {
    case "$1" in
        *"/cam/realmonitor"*) echo "dahua" ;;
        *"/ch1/main/av_stream"*) echo "hikvision" ;;
        *) echo "unknown" ;;
    esac
}

# 主循环：累计 JSON 数组
streams_json="[]"
total=0
ok=0
fail=0

while IFS=$'\t' read -r key url; do
    [ -z "$key" ] && continue
    total=$((total + 1))
    masked=$(mask_url "$url")
    vendor=$(detect_vendor "$url")

    echo "[$total] probing $key ($vendor) ..."

    # 用 timeout 命令兜底 ffprobe 卡住（macOS gtimeout 或 coreutils timeout）
    if command -v timeout >/dev/null 2>&1; then
        probe_out=$(timeout "$TIMEOUT" bash -c "source lib/probe.sh; probe_stream '$url'" 2>/dev/null)
        rc=$?
    elif command -v gtimeout >/dev/null 2>&1; then
        probe_out=$(gtimeout "$TIMEOUT" bash -c "source lib/probe.sh; probe_stream '$url'" 2>/dev/null)
        rc=$?
    else
        # 没 timeout 命令，依赖 ffprobe -rw_timeout 自带超时
        probe_out=$(probe_stream "$url" 2>/dev/null)
        rc=$?
    fi

    if [ $rc -eq 0 ] && [ -n "$probe_out" ]; then
        w=$(echo "$probe_out" | sed -n 's/.*width=\([0-9]*\).*/\1/p')
        h=$(echo "$probe_out" | sed -n 's/.*height=\([0-9]*\).*/\1/p')
        fps=$(echo "$probe_out" | sed -n 's/.*fps=\([0-9]*\).*/\1/p')
        codec=$(echo "$probe_out" | sed -n 's/.*codec=\([^ ]*\).*/\1/p')
        kbps=$(echo "$probe_out" | sed -n 's/.*bitrate_kbps=\([0-9]*\).*/\1/p')
        cls=$(classify_resolution "$w" "$h")
        entry=$(jq -n \
            --arg sp "$key" --arg url "$masked" --arg v "$vendor" \
            --argjson w "$w" --argjson h "$h" --argjson fps "$fps" \
            --arg c "$codec" --argjson br "$kbps" \
            --arg cls "$cls" --arg st "ok" \
            '{stream_path:$sp, source_url:$url, vendor:$v,
              width:$w, height:$h, fps:$fps, codec:$c, bitrate_kbps:$br,
              resolution_class:$cls, probe_status:$st, probe_error:null}')
        ok=$((ok + 1))
        echo "    → ${w}x${h} ${codec} ${fps}fps (${cls})"
    else
        reason="probe_failed"
        [ $rc -eq 124 ] && reason="probe_timeout"
        entry=$(jq -n \
            --arg sp "$key" --arg url "$masked" --arg v "$vendor" \
            --arg cls "unknown" --arg st "failed" --arg err "$reason" \
            '{stream_path:$sp, source_url:$url, vendor:$v,
              width:0, height:0, fps:0, codec:"", bitrate_kbps:0,
              resolution_class:$cls, probe_status:$st, probe_error:$err}')
        fail=$((fail + 1))
        echo "    → $reason"
    fi
    streams_json=$(echo "$streams_json" | jq ". + [$entry]")
done < <(extract_pulls)

probed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
jq -n --arg t "$probed_at" --argjson to "$TIMEOUT" --argjson s "$streams_json" \
    '{probed_at:$t, probe_timeout_sec:$to, streams:$s}' > "$OUT"

echo ""
echo "完成：总 $total  成功 $ok  失败 $fail"
echo "写入：$OUT"
