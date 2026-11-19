#!/usr/bin/env bash
# 录制兼容性验证脚本（WebRTC 2K@24 升级计划 Task 6）。
#
# 覆盖 A11（RTSP 源录制）、A12（WebRTC/WHIP 推流录制，Opus-in-MP4）、
# A13（分片录制）、录制中停止源、Opus-in-MP4 播放能力探测。
#
# 设计原则：
#   - 只通过 MP4 HTTP API 控制录制，不修改录制器行为；
#   - 所有断言基于真实 ffprobe/API 输出，失败即非零退出；
#   - 无法在本机完成的项（10 分钟长录、真实摄像头、弱网）打印 PENDING，不冒充 PASS；
#   - 只按记录的 PID（并校验命令行）停止进程，不使用 pkill/killall。
#
# 用法：
#   ./record-check.sh rtsp          # A11 视频：live/rtsp2k 录 25s 单文件并 ffprobe
#   ./record-check.sh audio         # A11 音频：起带 AAC 的 2K 源 + RTSP 拉流，录 18s
#   ./record-check.sh fragment      # A13 分片：fragment=1m 录 175s，校验分片数与可播性
#   ./record-check.sh stop-source   # 录制中只停 RTMP 推流源，校验文件收尾
#   ./record-check.sh whip          # A12：Playwright+Chrome 假设备推 H264+Opus，录 15s
#   ./record-check.sh playback [f]  # Opus-in-MP4 播放探测（ffplay/VLC/Chrome）
#   ./record-check.sh self-test     # 分片角色断言合成自测（无需服务端）
#   ./record-check.sh all           # 依次执行以上后 cleanup 并汇总（失败项不影响后续项）
#   ./record-check.sh cleanup       # 停止本脚本启动的源/代理/浏览器，删除临时状态
#
# 环境变量（全部可覆盖）：
#   API_BASE               http://127.0.0.1:8080
#   API_TOKEN              admin.enablelogin 时填 Bearer token
#   STREAM_PATH            live/rtsp2k（RTSP 拉流结果流）
#   AUDIO_STREAM_PATH      live/rtsp2k-audio
#   WHIP_STREAM_PATH       live/whip
#   RTSP_URL               rtsp://127.0.0.1:8554/live/source2k
#   AUDIO_SOURCE_RTMP_URL  rtmp://127.0.0.1/live/source2k-audio
#   AUDIO_RTSP_URL         rtsp://127.0.0.1:8554/live/source2k-audio
#   RECORD_SECONDS         25      rtsp 子命令录制时长
#   AUDIO_RECORD_SECONDS   18      audio 子命令录制时长
#   FRAGMENT               1m      分片大小
#   FRAGMENT_SECONDS       175     分片录制总时长（<180s，恰好 3 个分片文件）
#   STOP_SOURCE_SECONDS    10      停源前录制时长
#   STOP_SOURCE_WAIT       5       停源后等待时长
#   WHIP_RECORD_SECONDS    15      WHIP 录制时长
#   OUT_DIR                /tmp/webrtc-2k-record/check（状态/日志/runner 输出）
#   STORAGE_ROOT           /tmp/webrtc-2k-record/local（服务端 storage.local.path）
#   WAIT_TIMEOUT           90      等待流/文件就绪的总超时（秒）
#   POLL_INTERVAL          0.5
#   RTSP_SCRIPT            <本目录>/rtsp-pull.sh
#   PLAYWRIGHT_DIR         /private/tmp/webrtc-2k-run（含 node_modules/playwright）
#   PLAYWRIGHT_CHANNEL     chrome
#
# 前置条件：
#   - 服务端已启动（HTTP :8080、RTSP :8554、RTMP :1935、WebRTC udp:9000-9100），
#     global.storage.local.path 与 STORAGE_ROOT 一致；不要配置 S3（本任务只测本地录制）。
#     例如：
#       cd example/default && go build -tags sqlite -o /tmp/webrtc-2k-record/m7s-server .
#       mkdir -p /tmp/webrtc-2k-record/run && cd /tmp/webrtc-2k-record/run
#       /tmp/webrtc-2k-record/m7s-server -c /tmp/webrtc-record-config.yaml
#   - ffmpeg/ffprobe 7.x、curl、python3；whip/playback 子命令另需 node + Playwright + Chrome。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

API_BASE="${API_BASE:-http://127.0.0.1:8080}"
API_BASE="${API_BASE%/}"
API_TOKEN="${API_TOKEN:-}"
STREAM_PATH="${STREAM_PATH:-live/rtsp2k}"
AUDIO_STREAM_PATH="${AUDIO_STREAM_PATH:-live/rtsp2k-audio}"
WHIP_STREAM_PATH="${WHIP_STREAM_PATH:-live/whip}"
RTSP_URL="${RTSP_URL:-rtsp://127.0.0.1:8554/live/source2k}"
AUDIO_SOURCE_RTMP_URL="${AUDIO_SOURCE_RTMP_URL:-rtmp://127.0.0.1/live/source2k-audio}"
AUDIO_RTSP_URL="${AUDIO_RTSP_URL:-rtsp://127.0.0.1:8554/live/source2k-audio}"

RECORD_SECONDS="${RECORD_SECONDS:-25}"
AUDIO_RECORD_SECONDS="${AUDIO_RECORD_SECONDS:-18}"
FRAGMENT="${FRAGMENT:-1m}"
# 175s：分片按媒体时间在 60/120/180s 边界轮转，175s（<180s）停止时恰好 3 个文件
# （实测约 60.3s/60.0s/54.8s，最后一片是手动停止的残余）。190s 会跨过 180s 边界
# 得到 4 个文件，不满足“3 个文件”的验收口径；任务简报的“3 分钟”长录由该
# 3 文件/175s sanity run 代表，>3 分钟录制仍属 PENDING（见 report-record.md §9）。
FRAGMENT_SECONDS="${FRAGMENT_SECONDS:-175}"
STOP_SOURCE_SECONDS="${STOP_SOURCE_SECONDS:-10}"
STOP_SOURCE_WAIT="${STOP_SOURCE_WAIT:-5}"
WHIP_RECORD_SECONDS="${WHIP_RECORD_SECONDS:-15}"
PLAYBACK_PROBE_SECONDS="${PLAYBACK_PROBE_SECONDS:-5}"
PLAYBACK_HTTP_PORT="${PLAYBACK_HTTP_PORT:-18765}"

OUT_DIR="${OUT_DIR:-/tmp/webrtc-2k-record/check}"
STORAGE_ROOT="${STORAGE_ROOT:-/tmp/webrtc-2k-record/local}"
RTSP_STATE_DIR="$OUT_DIR/rtsp-pull"
AUDIO_STATE_DIR="$OUT_DIR/rtsp-pull-audio"
# 本脚本新建的拉流代理 ID（cleanup 只删这些，不按 streamPath 批量删除别人的代理）
RTSP_PROXY_IDS_FILE="$OUT_DIR/created-proxy-ids-rtsp.txt"
AUDIO_PROXY_IDS_FILE="$OUT_DIR/created-proxy-ids-audio.txt"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-90}"
POLL_INTERVAL="${POLL_INTERVAL:-0.5}"
RTSP_SCRIPT="${RTSP_SCRIPT:-$SCRIPT_DIR/rtsp-pull.sh}"
PLAYWRIGHT_DIR="${PLAYWRIGHT_DIR:-/private/tmp/webrtc-2k-run}"
PLAYWRIGHT_CHANNEL="${PLAYWRIGHT_CHANNEL:-chrome}"

AUDIO_SOURCE_PID_FILE="$OUT_DIR/audio-source.pid"
WHIP_RUNNER_PID_FILE="$OUT_DIR/whip-runner.pid"
WHIP_READY_FILE="$OUT_DIR/whip-ready.json"
WHIP_STOP_FILE="$OUT_DIR/whip.stop"
WHIP_RUNNER_JS="$OUT_DIR/whip-publish.js"
WHIP_RUNNER_LOG="$OUT_DIR/whip-publish.log"
PLAYBACK_DIR="$OUT_DIR/playback"
PLAYBACK_CHROME_JS="$PLAYBACK_DIR/chrome-probe.js"
PLAYBACK_HTTP_PID_FILE="$OUT_DIR/playback-http.pid"

RESULTS=()
FAILED=0
CLEANED=0
RUN_ID="${RUN_ID:-$(date +%Y%m%d-%H%M%S)}"

log()  { printf '[record-check] %s\n' "$*"; }
warn() { printf '[record-check] WARN: %s\n' "$*" >&2; }
die()  { printf '[record-check] ERROR: %s\n' "$*" >&2; exit 2; }

pass()    { RESULTS+=("PASS|$*");    log "PASS: $*"; }
fail()    { RESULTS+=("FAIL|$*");    FAILED=1; warn "FAIL: $*"; }
pending() { RESULTS+=("PENDING|$*"); log "PENDING: $*"; }
info()    { RESULTS+=("INFO|$*");    log "INFO: $*"; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

# ---------- HTTP ----------

api_curl() {
  if [ -n "$API_TOKEN" ]; then
    curl -sS --max-time 30 -H "Authorization: Bearer $API_TOKEN" "$@"
  else
    curl -sS --max-time 30 "$@"
  fi
}

api_get() { api_curl "$API_BASE$1"; }

# 返回 HTTP 状态码，响应体写入 $3
api_post_json_check() {
  local path="$1" body="$2" out="$3"
  api_curl -o "$out" -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' --data-binary "$body" "$API_BASE$path"
}

api_ok() {
  local code
  # 与其它 API 调用一致，走 API_TOKEN 鉴权（admin.enablelogin 时裸 curl 会 401/403）
  code=$(api_curl -o /dev/null -w '%{http_code}' --max-time 5 "$API_BASE/api/stream/list" 2>/dev/null || true)
  [ "$code" = "200" ]
}

require_api() {
  api_ok || die "无法访问 $API_BASE/api/stream/list。请先启动服务端（见脚本头注释），或用 API_BASE 指定地址。"
}

# 输出 "<present> <audioCodec> <videoCodec> <WxH> <codecMeta>"
stream_state() {
  api_get /api/stream/list | python3 -c '
import json, sys
path = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    print("0 - - 0x0 -")
    sys.exit(0)
for item in data.get("data") or []:
    if item.get("path") != path:
        continue
    at = item.get("audioTrack") or {}
    vt = item.get("videoTrack") or {}
    print("1", at.get("codec") or "-", vt.get("codec") or "-",
          "%sx%s" % (vt.get("width", 0), vt.get("height", 0)),
          vt.get("meta") or "-")
    break
else:
    print("0 - - 0x0 -")
' "$1"
}

stream_present() {
  local present _rest
  read -r present _rest <<< "$(stream_state "$1")"
  [ "$present" = "1" ]
}

wait_stream() {
  local stream="$1" deadline=$((SECONDS + WAIT_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if stream_present "$stream"; then
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  return 1
}

# ---------- 录制 API ----------

build_start_body() {
  local file_path="$1" duration="$2" fragment="$3" record_type="${4:-mp4}"
  python3 -c 'import json, sys
print(json.dumps({
    "duration": sys.argv[1],
    "fragment": sys.argv[2],
    "filePath": sys.argv[3],
    "type": sys.argv[4],
}))' "$duration" "$fragment" "$file_path" "$record_type"
}

start_record() {
  local stream="$1" file_path="$2" duration="$3" fragment="$4" record_type="${5:-mp4}"
  local body code out
  out="$OUT_DIR/start-$(printf '%s' "$stream" | tr '/' '_').json"
  mkdir -p "$OUT_DIR"
  body=$(build_start_body "$file_path" "$duration" "$fragment" "$record_type")
  code=$(api_post_json_check "/mp4/api/start/$stream" "$body" "$out")
  if [ "$code" != "200" ]; then
    warn "start 失败：HTTP $code $(cat "$out" 2>/dev/null || true)"
    return 1
  fi
  if ! python3 -c 'import json,sys
try:
    sys.exit(0 if json.load(open(sys.argv[1])).get("code") == 0 else 1)
except Exception:
    sys.exit(1)' "$out"; then
    warn "start 返回非 0：$(cat "$out" 2>/dev/null || true)"
    return 1
  fi
  log "录制已开始：stream=$stream filePath=$file_path duration=$duration fragment=$fragment type=$record_type"
  return 0
}

# Stop 的 not-found 幂等：200 成功；404（或旧版 500 + not found）视为已停止。
stop_record() {
  local stream="$1"
  local out
  out="$OUT_DIR/stop-$(printf '%s' "$stream" | tr '/' '_').json"
  local code
  code=$(api_post_json_check "/mp4/api/stop/$stream" '{}' "$out")
  case "$code" in
    200)
      log "录制已停止：stream=$stream"
      return 0
      ;;
    404)
      log "录制已不存在（404，按幂等成功处理）：stream=$stream"
      return 0
      ;;
    500)
      if grep -qi "not found" "$out" 2>/dev/null; then
        log "录制已不存在（旧版 500 not found，按幂等成功处理）：stream=$stream"
        return 0
      fi
      warn "stop 失败：HTTP 500 $(cat "$out" 2>/dev/null || true)"
      return 1
      ;;
    *)
      warn "stop 失败：HTTP $code $(cat "$out" 2>/dev/null || true)"
      return 1
      ;;
  esac
}

# 输出该流所有匹配 filePath 前缀的记录：<相对路径>\t<时长ms>\t<videoCodec>\t<audioCodec>
list_records() {
  local stream="$1" prefix="$2"
  api_get "/mp4/api/list/$stream?pageSize=100&pageNum=1" | python3 -c '
import json, sys
prefix = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for item in data.get("data") or []:
    path = item.get("filePath") or ""
    if prefix and not (path == prefix or path.startswith(prefix.rstrip("/") + "/")):
        continue
    print("%s\t%s\t%s\t%s" % (path, item.get("duration", 0),
                              item.get("videoCodec") or "-",
                              item.get("audioCodec") or "-"))
' "$prefix"
}

resolve_local_path() {
  local rel="$1"
  case "$rel" in
    /*) printf '%s\n' "$rel" ;;
    *)  printf '%s\n' "$STORAGE_ROOT/$rel" ;;
  esac
}

# 可移植的文件大小（字节）：GNU stat（Linux）→ BSD/macOS stat → wc -c。
# 脚本在 macOS 开发机与 Linux 真机都要能跑，不能用单一 stat 方言。
file_size() {
  local file="$1"
  stat -c '%s' "$file" 2>/dev/null \
    || stat -f '%z' "$file" 2>/dev/null \
    || wc -c < "$file" | tr -d ' '
}

# 录制文件名由录制器生成，形如 2026-11-18-21-31-13_785563000.mp4（固定宽度，字典序=时间序）。
# /mp4/api/list 的返回顺序是“最新在前”（实测 21:33 → 21:32 → 21:31），不能拿来判断
# 分片先后或“哪一片是手动停止的残余”；统一按 basename 排序成时间顺序后再分配角色。
sort_paths_chronological() {
  python3 -c '
import os, sys
rows = [line.strip() for line in sys.stdin if line.strip()]
rows.sort(key=lambda p: os.path.basename(p))
if rows:
    sys.stdout.write("\n".join(rows) + "\n")
'
}

# 分片角色断言：assert_fragment_roles <expected> <rows-file>。
# rows-file 每行 "<路径>\t<duration>"；调用方须已按文件名时间排序。
# 除时长区间外还**独立**校验时间顺序，避免“列表反转但各时长仍落在容差内”静默通过：
#   1) 文件名时间戳严格升序（未排序/反转列表必须失败）；
#   2) 每个完整分片的 start+duration 与下一片 start 相差 ≤ 5s（分片边界连续）；
#   3) 前 N-1 片（完整分片）duration ∈ [55,65]s；时间上最后一片（残余）∈ [10,80]s。
assert_fragment_roles() {
  local expected="$1" rows_file="$2"
  python3 - "$expected" "$rows_file" <<'PY'
import datetime, os, re, sys

expected = int(sys.argv[1])
# 完整分片按媒体时间 60s 轮转（本次实测 60.300s / 59.957s）。
# 收紧到 55–65s：若把最后残余 54.832s 误当成完整片（列表反转/未排序）会立刻失败。
FULL_MIN, FULL_MAX = 55.0, 65.0
# 时间上最后一片是手动 stop 的残余（175s 运行 → 约 54.8s），允许 10–80s。
LAST_MIN, LAST_MAX = 10.0, 80.0
# 分片边界连续性容差：start[i] + duration[i] ≈ start[i+1]（实测偏差 <0.1s）。
BOUNDARY_TOLERANCE_SECONDS = 5.0

name_re = re.compile(r"(\d{4})-(\d{2})-(\d{2})-(\d{2})-(\d{2})-(\d{2})_(\d+)")
rows = []
with open(sys.argv[2], encoding="utf-8") as rows_file:
    for line in rows_file:
        line = line.rstrip("\n")
        if not line:
            continue
        path, sep, dur = line.partition("\t")
        if not sep:
            print("bad input row (missing tab): %r" % line, file=sys.stderr)
            sys.exit(1)
        rows.append((os.path.basename(path), float(dur)))

errors = []
if len(rows) != expected:
    errors.append("expected %d fragments, got %d" % (expected, len(rows)))

stamps = []
for name, _duration in rows:
    m = name_re.match(name)
    if m is None:
        errors.append("cannot parse timestamp from %s" % name)
        continue
    year, month, day, hour, minute, second, nanos = (int(x) for x in m.groups())
    stamps.append(datetime.datetime(year, month, day, hour, minute, second).timestamp() + nanos / 1e9)

if not errors:
    durations = [duration for _name, duration in rows]
    for i in range(1, len(stamps)):
        if stamps[i] <= stamps[i - 1]:
            errors.append("timestamp order violated at #%d: %s <= %s" % (
                i + 1, rows[i][0], rows[i - 1][0]))
    for i, duration in enumerate(durations[:-1]):
        if not (FULL_MIN <= duration <= FULL_MAX):
            errors.append("fragment #%d duration=%.3fs not in [%.0f,%.0f]" % (
                i + 1, duration, FULL_MIN, FULL_MAX))
        if i + 1 < len(stamps):
            gap = stamps[i + 1] - (stamps[i] + duration)
            if abs(gap) > BOUNDARY_TOLERANCE_SECONDS:
                errors.append("fragment #%d start+duration is %.3fs from next start" % (i + 1, gap))
    last = durations[-1]
    if not (LAST_MIN <= last <= LAST_MAX):
        errors.append("last fragment duration=%.3fs out of [%.0f,%.0f]" % (last, LAST_MIN, LAST_MAX))

if errors:
    for err in errors:
        print(err, file=sys.stderr)
    sys.exit(1)
print("durations=" + ",".join("%.3f" % duration for _name, duration in rows))
PY
}

# 等录制收尾：该前缀的记录都已写入时长（DB duration>0，表示 trailer/入库完成）、
# 文件存在，且大小连续两次采样不变。仅看文件大小会在 trailer 写入前提前返回。
# $3 可选：期望的文件数（分片录制用来避免把尚未出现的分片当成全部）。
wait_record_files() {
  local stream="$1" prefix="$2" expected="${3:-}"
  local deadline=$((SECONDS + WAIT_TIMEOUT))
  local stable="" last="" rows
  while [ "$SECONDS" -lt "$deadline" ]; do
    rows=$(list_records "$stream" "$prefix")
    local count=0
    [ -n "$rows" ] && count=$(printf '%s\n' "$rows" | wc -l | tr -d ' ')
    if [ -n "$expected" ] && [ "$count" != "$expected" ]; then
      rows=""
    fi
    if [ -n "$rows" ]; then
      local all_ready=1 size_sig="" path dur _vc _ac abs sz
      while IFS=$'\t' read -r path dur _vc _ac; do
        abs=$(resolve_local_path "$path")
        if [ ! -f "$abs" ] || [ "${dur:-0}" = "0" ]; then
          all_ready=0
          break
        fi
        sz=$(file_size "$abs" 2>/dev/null || echo 0)
        size_sig="$size_sig$abs:$sz;"
      done <<< "$rows"
      if [ "$all_ready" = "1" ]; then
        if [ "$size_sig" = "$last" ]; then
          stable=$(printf '%s\n' "$rows" | cut -f1)
          break
        fi
        last="$size_sig"
      fi
    fi
    sleep 1
  done
  [ -n "$stable" ] || return 1
  printf '%s\n' "$stable"
}

# ffprobe 输出精简 JSON；文件不可读时返回非零。
probe_json() {
  local file="$1"
  ffprobe -v error \
    -show_entries stream=index,codec_type,codec_name,width,height,avg_frame_rate,sample_rate,channels \
    -show_entries format=duration,size,format_name \
    -of json "$file"
}

probe_summary() {
  python3 -c '
import json, sys
p = json.load(open(sys.argv[1]))
streams = p.get("streams") or []
fmt = p.get("format") or {}
parts = []
for s in streams:
    if s.get("codec_type") == "video":
        parts.append("video=%s %sx%s avg_fps=%s" % (
            s.get("codec_name"), s.get("width"), s.get("height"), s.get("avg_frame_rate")))
    elif s.get("codec_type") == "audio":
        parts.append("audio=%s %sHz %sch" % (
            s.get("codec_name"), s.get("sample_rate"), s.get("channels")))
print("duration=%ss size=%sB %s" % (fmt.get("duration", "?"), fmt.get("size", "?"), " ".join(parts)))
' "$1"
}

# assert_probe <file> <python-expression-file? no> 用法见各子命令中的内联 python。

# ---------- RTSP 源（复用 Task 5 的 rtsp-pull.sh） ----------

ensure_rtsp_ready() {
  if stream_present "$STREAM_PATH"; then
    log "拉流结果已存在：${STREAM_PATH}（$(stream_state "$STREAM_PATH" | cut -d' ' -f1-4)）"
    return 0
  fi
  [ -x "$RTSP_SCRIPT" ] || die "未找到可执行脚本 $RTSP_SCRIPT"
  mkdir -p "$RTSP_STATE_DIR"
  local before_ids
  before_ids=$(list_proxy_ids_for_stream "$STREAM_PATH")
  log "启动 RTSP 拉流链路：rtsp-pull.sh start（OUT_DIR=${RTSP_STATE_DIR}）"
  if ! OUT_DIR="$RTSP_STATE_DIR" API_BASE="$API_BASE" API_TOKEN="$API_TOKEN" \
    "$RTSP_SCRIPT" start >"$OUT_DIR/start.log" 2>&1; then
    warn "rtsp-pull.sh start 失败，日志尾部："
    tail -n 20 "$OUT_DIR/start.log" >&2 || true
    # start 可能已创建代理但随后失败（例如建完代理后等流超时）。
    # 仍做前后差集快照并记入 cleanup 清单，让 EXIT trap 删除孤儿代理。
    record_created_proxies "$STREAM_PATH" "$before_ids" "$RTSP_PROXY_IDS_FILE" || true
    return 1
  fi
  record_created_proxies "$STREAM_PATH" "$before_ids" "$RTSP_PROXY_IDS_FILE"
  wait_stream "$STREAM_PATH"
}

# 只按状态文件里记录的 PID 停止，并校验命令行包含 RTMP URL。
pid_matches() {
  local pid="$1" marker="$2" cmd
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  cmd=$(ps -p "$pid" -o command= 2>/dev/null || true)
  case "$cmd" in
    *"$marker"*) return 0 ;;
    *) warn "PID $pid 命令行不匹配标记 ${marker}，拒绝停止：${cmd:-<empty>}"; return 1 ;;
  esac
}

stop_pid_checked() {
  local pid="$1" marker="$2" waited=0
  pid_matches "$pid" "$marker" || return 1
  kill -TERM "$pid" 2>/dev/null || return 1
  while [ "$waited" -lt 50 ] && kill -0 "$pid" 2>/dev/null; do
    sleep 0.1
    waited=$((waited + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    warn "PID $pid 未在 5s 内退出，发送 SIGKILL"
    kill -KILL "$pid" 2>/dev/null || true
  fi
  return 0
}

# 从 rtsp-pull.sh 的 state.env 读取某个键
state_value() {
  local state_file="$1" key="$2"
  [ -f "$state_file" ] || return 0
  awk -F= -v k="$key" '$1 == k {print $2}' "$state_file" | tail -n 1
}

# 列出某 streamPath 下的全部代理 ID（只读，不删除）
list_proxy_ids_for_stream() {
  local stream="$1"
  api_get /api/proxy/pull/list | python3 -c '
import json, sys
path = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(0)
ids = []
for item in data.get("data") or []:
    if item.get("streamPath") == path:
        try:
            ids.append(int(item.get("id", item.get("ID", 0))))
        except (TypeError, ValueError):
            pass
print(" ".join(str(i) for i in sorted(set(ids)) if i > 0))
' "$stream"
}

# 记录 start 前后的 ID 差集（= 本脚本新建的代理）。cleanup 只删这些 ID，
# 不删除同 streamPath 下早已存在的代理（API 列表顺序不可靠，不能按路径批量删）。
# 与记录文件中已有的 ID 合并：同一次运行内多次 start（含失败重试）不能丢先前记录。
record_created_proxies() {
  local stream="$1" before="$2" out_file="$3"
  local after ids="" id existing_ids
  after=$(list_proxy_ids_for_stream "$stream")
  for id in $after; do
    case " $before " in
      *" $id "*) ;;
      *) ids="$ids $id" ;;
    esac
  done
  if [ -f "$out_file" ]; then
    existing_ids=$(tr ' ' '\n' < "$out_file" | sed '/^$/d')
    for id in $existing_ids; do
      case " $ids " in
        *" $id "*) ;;
        *) ids="$ids $id" ;;
      esac
    done
  fi
  ids="${ids# }"
  if [ -n "$ids" ]; then
    printf '%s\n' $ids > "$out_file"
    log "记录本脚本创建的拉流代理：stream=$stream ids=$ids"
  else
    warn "未识别到本脚本新建的拉流代理（stream=$stream，start 前=[${before}]，start 后=[${after}]）"
  fi
}

# 只删除本脚本记录过的代理 ID。删除失败（非 200）的 ID 写回记录文件保留，
# 不随 cleanup 一起丢弃；服务恢复后再执行 cleanup 会重试删除。
remove_recorded_proxies() {
  local file="$1" stream="$2" ids id code failed=""
  [ -f "$file" ] || return 0
  ids=$(tr ' ' '\n' < "$file" | sed '/^$/d')
  for id in $ids; do
    code=$(api_post_json_check "/api/proxy/pull/remove/$id" '{}' "$OUT_DIR/remove-$id.json")
    if [ "$code" = "200" ]; then
      log "已删除本脚本创建的拉流代理 id=${id}（streamPath=${stream}）"
    else
      warn "删除拉流代理 id=$id 失败：HTTP $code $(cat "$OUT_DIR/remove-$id.json" 2>/dev/null || true)（ID 已保留，稍后可重试 cleanup）"
      failed="$failed $id"
    fi
  done
  failed="${failed# }"
  if [ -n "$failed" ]; then
    printf '%s\n' $failed > "$file"
    warn "仍有拉流代理未删除：streamPath=${stream} ids=${failed}（记录文件 ${file}）"
  else
    rm -f "$file"
  fi
}

start_audio_source() {
  local pid
  pid=$(cat "$AUDIO_SOURCE_PID_FILE" 2>/dev/null || true)
  if pid_matches "${pid:-}" "$AUDIO_SOURCE_RTMP_URL"; then
    log "复用已运行的音频推流 PID=$pid"
    return 0
  fi
  if ! command -v ffmpeg >/dev/null 2>&1; then
    warn "未安装 ffmpeg，无法启动音频推流源"
    return 2
  fi
  mkdir -p "$OUT_DIR"
  log "启动带 AAC 的 2K 推流：${AUDIO_SOURCE_RTMP_URL}（日志 $OUT_DIR/audio-push.log）"
  nohup ffmpeg -hide_banner -loglevel warning -re \
    -f lavfi -i testsrc2=size=2560x1440:rate=24 \
    -f lavfi -i sine=frequency=440:sample_rate=48000 \
    -c:v libx264 -preset ultrafast -b:v 10M -maxrate 12M -bufsize 20M -g 48 -pix_fmt yuv420p \
    -c:a aac -b:a 128k -f flv "$AUDIO_SOURCE_RTMP_URL" \
    >"$OUT_DIR/audio-push.log" 2>&1 &
  pid=$!
  echo "$pid" > "$AUDIO_SOURCE_PID_FILE"
  local source_path deadline=$((SECONDS + WAIT_TIMEOUT))
  source_path=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.urlparse(sys.argv[1]).path.strip("/"))' "$AUDIO_SOURCE_RTMP_URL")
  while [ "$SECONDS" -lt "$deadline" ]; do
    kill -0 "$pid" 2>/dev/null || { warn "音频推流进程已退出，日志尾部："; tail -n 20 "$OUT_DIR/audio-push.log" >&2 || true; return 1; }
    read -r present audio_codec _rest <<< "$(stream_state "$source_path")"
    if [ "$present" = "1" ] && [ "$audio_codec" != "-" ] && [ "$audio_codec" != "None" ]; then
      log "音频源就绪：$source_path audio=$audio_codec"
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  return 1
}

ensure_audio_pull_ready() {
  if stream_present "$AUDIO_STREAM_PATH"; then
    log "音频拉流结果已存在：$AUDIO_STREAM_PATH"
    return 0
  fi
  start_audio_source || return 1
  mkdir -p "$AUDIO_STATE_DIR"
  local before_ids
  before_ids=$(list_proxy_ids_for_stream "$AUDIO_STREAM_PATH")
  log "创建音频 RTSP 拉流代理：$AUDIO_RTSP_URL -> $AUDIO_STREAM_PATH"
  if ! START_SOURCE=0 SOURCE_RTMP_URL="$AUDIO_SOURCE_RTMP_URL" RTSP_URL="$AUDIO_RTSP_URL" \
    STREAM_PATH="$AUDIO_STREAM_PATH" OUT_DIR="$AUDIO_STATE_DIR" API_BASE="$API_BASE" API_TOKEN="$API_TOKEN" \
    "$RTSP_SCRIPT" start >"$AUDIO_STATE_DIR/start.log" 2>&1; then
    warn "音频拉流代理创建失败，日志尾部："
    tail -n 20 "$AUDIO_STATE_DIR/start.log" >&2 || true
    # 与 ensure_rtsp_ready 同理：start 非零退出前可能已建好代理，仍做快照供 cleanup 删除。
    record_created_proxies "$AUDIO_STREAM_PATH" "$before_ids" "$AUDIO_PROXY_IDS_FILE" || true
    return 1
  fi
  record_created_proxies "$AUDIO_STREAM_PATH" "$before_ids" "$AUDIO_PROXY_IDS_FILE"
  wait_stream "$AUDIO_STREAM_PATH"
}

# ---------- 子命令 ----------

cmd_rtsp() {
  need_cmd curl
  need_cmd python3
  need_cmd ffprobe
  require_api
  mkdir -p "$OUT_DIR"
  local file_path="rtsp-$RUN_ID"

  ensure_rtsp_ready || { fail "A11 前置：live/rtsp2k 未就绪"; return 1; }
  read -r _ audio_codec video_codec res _meta <<< "$(stream_state "$STREAM_PATH")"
  info "A11 源流：$STREAM_PATH video=$video_codec audio=$audio_codec $res"
  if [ "$audio_codec" = "-" ] || [ "$audio_codec" = "None" ]; then
    info "A11 默认 2K 源为纯视频（audioTrack 为空），音频另行用 audio 子命令验证"
  fi

  start_record "$STREAM_PATH" "$file_path" "600s" "0" "mp4" || { fail "A11 视频录制启动失败"; return 1; }
  sleep "$RECORD_SECONDS"
  stop_record "$STREAM_PATH" || { fail "A11 视频录制停止失败"; return 1; }
  sleep 2

  local files
  if ! files=$(wait_record_files "$STREAM_PATH" "$file_path" 1); then
    fail "A11 视频：录制停止后未找到完整的文件（filePath=${file_path}）"
    return 1
  fi
  local count
  count=$(printf '%s\n' "$files" | wc -l | tr -d ' ')
  [ "$count" = "1" ] || { fail "A11 视频：期望 1 个文件，实际 $count"; return 1; }

  local abs probe
  abs=$(resolve_local_path "$files")
  probe="$OUT_DIR/probe-rtsp.json"
  probe_json "$abs" > "$probe" || { fail "A11 视频：ffprobe 无法读取 $abs"; return 1; }
  info "A11 ffprobe：$(probe_summary "$probe")"

  if python3 - "$probe" "$RECORD_SECONDS" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
v = next((s for s in p.get("streams", []) if s.get("codec_type") == "video"), None)
fmt = p.get("format") or {}
if v is None:
    print("missing video stream"); sys.exit(1)
if v.get("codec_name") != "h264":
    print("codec=%s != h264" % v.get("codec_name")); sys.exit(1)
if (v.get("width"), v.get("height")) != (2560, 1440):
    print("resolution=%sx%s != 2560x1440" % (v.get("width"), v.get("height"))); sys.exit(1)
num, _, den = (v.get("avg_frame_rate") or "0/1").partition("/")
fps = float(num) / float(den or 1)
if not (23.0 <= fps <= 25.0):
    print("avg_frame_rate=%.3f not within [23,25]" % fps); sys.exit(1)
dur = float(fmt.get("duration") or 0)
if not (float(sys.argv[2]) * 0.6 <= dur <= float(sys.argv[2]) + 15):
    print("duration=%.2fs not around %ss" % (dur, sys.argv[2])); sys.exit(1)
print("avg_frame_rate=%.3f duration=%.3fs" % (fps, dur))
PY
  then
    pass "A11 视频：2560x1440 h264 avg_frame_rate≈24，时长≈录制窗口（${abs}）"
  else
    fail "A11 视频断言未通过（ffprobe JSON：${probe}）"
  fi
  return 0
}

cmd_audio() {
  need_cmd curl
  need_cmd python3
  need_cmd ffprobe
  if ! command -v ffmpeg >/dev/null 2>&1; then
    fail "A11 音频：未安装 ffmpeg，无法启动带 AAC 的推流源"
    return 1
  fi
  require_api
  mkdir -p "$OUT_DIR"
  local file_path="rtsp-audio-$RUN_ID"

  ensure_audio_pull_ready || { fail "A11 音频前置：$AUDIO_STREAM_PATH 未就绪"; return 1; }
  read -r _ audio_codec video_codec res _meta <<< "$(stream_state "$AUDIO_STREAM_PATH")"
  info "A11 音频源流：$AUDIO_STREAM_PATH video=$video_codec audio=$audio_codec $res"
  if [ "$audio_codec" = "-" ] || [ "$audio_codec" = "None" ]; then
    fail "A11 音频：拉流后仍无音轨（期望 AAC）"
    return 1
  fi

  start_record "$AUDIO_STREAM_PATH" "$file_path" "600s" "0" "mp4" || { fail "A11 音频录制启动失败"; return 1; }
  sleep "$AUDIO_RECORD_SECONDS"
  stop_record "$AUDIO_STREAM_PATH" || { fail "A11 音频录制停止失败"; return 1; }
  sleep 2

  local files count abs probe
  if ! files=$(wait_record_files "$AUDIO_STREAM_PATH" "$file_path" 1); then
    fail "A11 音频：录制停止后未找到完整的文件（filePath=${file_path}）"
    return 1
  fi
  count=$(printf '%s\n' "$files" | wc -l | tr -d ' ')
  [ "$count" = "1" ] || { fail "A11 音频：期望 1 个文件，实际 $count"; return 1; }
  abs=$(resolve_local_path "$files")
  probe="$OUT_DIR/probe-rtsp-audio.json"
  probe_json "$abs" > "$probe" || { fail "A11 音频：ffprobe 无法读取 $abs"; return 1; }
  info "A11 音频 ffprobe：$(probe_summary "$probe")"

  if python3 - "$probe" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
a = next((s for s in p.get("streams", []) if s.get("codec_type") == "audio"), None)
fmt = p.get("format") or {}
if a is None:
    print("missing audio stream"); sys.exit(1)
if a.get("codec_name") != "aac":
    print("audio codec=%s != aac" % a.get("codec_name")); sys.exit(1)
if str(a.get("sample_rate")) != "48000":
    print("audio sample_rate=%s != 48000" % a.get("sample_rate")); sys.exit(1)
if int(a.get("channels") or 0) != 1:
    print("audio channels=%s != 1" % a.get("channels")); sys.exit(1)
dur = float(fmt.get("duration") or 0)
if dur <= 0:
    print("duration=%s <= 0" % fmt.get("duration")); sys.exit(1)
print("audio=%s sample_rate=%s channels=%s duration=%.3fs" % (
    a.get("codec_name"), a.get("sample_rate"), a.get("channels"), dur))
PY
  then
    pass "A11 音频：aac 48000Hz 1ch 音轨存在且时长非零（${abs}）"
  else
    fail "A11 音频断言未通过（ffprobe JSON：${probe}）"
  fi
  return 0
}

cmd_fragment() {
  need_cmd curl
  need_cmd python3
  need_cmd ffprobe
  require_api
  mkdir -p "$OUT_DIR"
  local file_path="fragment-$RUN_ID"

  ensure_rtsp_ready || { fail "A13 前置：live/rtsp2k 未就绪"; return 1; }
  start_record "$STREAM_PATH" "$file_path" "600s" "$FRAGMENT" "mp4" || { fail "A13 分片录制启动失败"; return 1; }
  log "分片录制 ${FRAGMENT_SECONDS}s（fragment=${FRAGMENT}），期间请勿中断"
  sleep "$FRAGMENT_SECONDS"
  stop_record "$STREAM_PATH" || { fail "A13 分片录制停止失败"; return 1; }
  sleep 3

  local files count expected
  expected=$(( (FRAGMENT_SECONDS + 59) / 60 ))
  if ! files=$(wait_record_files "$STREAM_PATH" "$file_path" "$expected"); then
    files=$(wait_record_files "$STREAM_PATH" "$file_path" "") || true
  fi
  if [ -z "$files" ]; then
    fail "A13 分片：录制停止后未找到完整的文件（filePath=${file_path}）"
    return 1
  fi
  # 关键：API 列表是最新在前，必须先按文件名（时间戳）排序成时间顺序，
  # 否则 #1..N 角色会整体反转（曾把最后一片 54.8s 误判成“首片”）。
  files=$(printf '%s\n' "$files" | sort_paths_chronological)
  count=$(printf '%s\n' "$files" | wc -l | tr -d ' ')
  info "A13 分片文件数=$count files=$(printf '%s ' $files)"

  if [ "$count" = "$expected" ]; then
    pass "A13 分片数量：${count}（期望 ${expected}）"
  else
    fail "A13 分片数量：实际 ${count}，期望 ${expected}（fragment=${FRAGMENT}，录制 ${FRAGMENT_SECONDS}s）"
  fi

  local path abs probe durations=() rows="" idx=0 bad=0
  for path in $files; do
    idx=$((idx + 1))
    abs=$(resolve_local_path "$path")
    probe="$OUT_DIR/probe-fragment-$idx.json"
    if ! probe_json "$abs" > "$probe" 2>"$OUT_DIR/probe-fragment-$idx.err"; then
      fail "A13 分片 #$idx 无法被 ffprobe 读取：$abs"
      bad=1
      continue
    fi
    local dur
    dur=$(python3 -c 'import json,sys; print((json.load(open(sys.argv[1])).get("format") or {}).get("duration", "0"))' "$probe")
    durations+=("$dur")
    rows+="$path"$'\t'"$dur"$'\n'
    info "A13 分片 #${idx}：duration=${dur}s $(probe_summary "$probe")"
    if python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) > 0 else 1)' "$dur"; then
      :
    else
      fail "A13 分片 #$idx 时长非正：$dur"
      bad=1
    fi
  done

  if [ "$bad" = "0" ]; then
    # 角色断言独立校验时间顺序（详见 assert_fragment_roles）：
    # 反转列表即使时长都落在容差内也会因时间戳降序/边界不连续而失败。
    local assert_out rows_file
    rows_file="$OUT_DIR/fragment-rows.txt"
    printf '%s' "$rows" > "$rows_file"
    if assert_out=$(assert_fragment_roles "$expected" "$rows_file"); then
      pass "A13 分片：$count 个文件均可播，时间顺序与角色正确（${assert_out}）"
    else
      fail "A13 分片断言未通过：durations=$(printf '%s ' "${durations[@]}")"
    fi
  fi
  return 0
}

# 合成自测（无需服务端）：用真实 175s 运行的文件名/时长验证 assert_fragment_roles。
# 期望：时间顺序 + 正确时长 → 通过；反转时长列表 → 失败；
#       反转文件顺序（各时长均在容差内）→ 失败（只能由时间顺序守卫发现）。
run_self_case() {
  local expect="$1" label="$2" rows="$3" err rc=0
  err="$OUT_DIR/self-test.err"
  printf '%s\n' "$rows" > "$OUT_DIR/self-test-rows.txt"
  assert_fragment_roles 3 "$OUT_DIR/self-test-rows.txt" >/dev/null 2>"$err" || rc=$?
  if [ "$rc" = "0" ]; then
    if [ "$expect" = "pass" ]; then
      pass "self-test：${label}（预期通过 → 实际通过）"
    else
      fail "self-test：${label}（预期失败 → 实际通过，守卫失效）"
    fi
  else
    local reason
    reason=$(tr '\n' ';' < "$err" | sed 's/;$//')
    if [ "$expect" = "fail" ]; then
      pass "self-test：${label}（预期失败 → 实际失败：${reason}）"
    else
      fail "self-test：${label}（预期通过 → 实际失败：${reason}）"
    fi
  fi
}

cmd_self_test() {
  need_cmd python3
  mkdir -p "$OUT_DIR"
  local f1="fragment-2026-11-18/2026-11-18-21-31-13_785563000.mp4"
  local f2="fragment-2026-11-18/2026-11-18-21-32-14_151630000.mp4"
  local f3="fragment-2026-11-18/2026-11-18-21-33-14_149407000.mp4"
  local rows_sorted rows_reversed_durations rows_reversed_order

  rows_sorted=$(printf '%s\t%s\n' "$f1" 60.300
                printf '%s\t%s\n' "$f2" 59.957
                printf '%s\t%s\n' "$f3" 54.832)
  rows_reversed_durations=$(printf '%s\t%s\n' "$f1" 54.832
                            printf '%s\t%s\n' "$f2" 59.957
                            printf '%s\t%s\n' "$f3" 60.300)
  rows_reversed_order=$(printf '%s\t%s\n' "$f3" 60.300
                        printf '%s\t%s\n' "$f2" 59.957
                        printf '%s\t%s\n' "$f1" 54.832)

  run_self_case pass "时间顺序 + 正确时长 [60.300,59.957,54.832]" "$rows_sorted"
  run_self_case fail "反转时长列表 [54.832,59.957,60.300]（评审复现）" "$rows_reversed_durations"
  run_self_case fail "反转文件顺序（时长均落在容差内，仅顺序守卫能发现）" "$rows_reversed_order"
}

cmd_stop_source() {
  need_cmd curl
  need_cmd python3
  need_cmd ffprobe
  require_api
  mkdir -p "$OUT_DIR"
  local file_path="stop-source-$RUN_ID"

  ensure_rtsp_ready || { fail "停源测试前置：live/rtsp2k 未就绪"; return 1; }
  local state_file="$RTSP_STATE_DIR/state.env" source_pid source_started source_url
  source_pid=$(state_value "$state_file" SOURCE_PID)
  source_started=$(state_value "$state_file" SOURCE_STARTED)
  source_url=$(state_value "$state_file" SOURCE_RTMP_URL)
  if [ "${source_started:-0}" != "1" ] || [ -z "$source_pid" ]; then
    fail "停源测试：未找到 rtsp-pull.sh 启动的推流 PID（state=${state_file}）；拒绝盲杀进程"
    return 1
  fi
  pid_matches "$source_pid" "${source_url:-rtmp://127.0.0.1/live/source2k}" || {
    fail "停源测试：PID=$source_pid 不存在或命令行不匹配，拒绝继续"
    return 1
  }

  start_record "$STREAM_PATH" "$file_path" "600s" "0" "mp4" || { fail "停源测试录制启动失败"; return 1; }
  sleep "$STOP_SOURCE_SECONDS"
  log "只停止 RTMP 推流源 PID=${source_pid}（${source_url}），拉流代理与服务端保持运行"
  if ! stop_pid_checked "$source_pid" "${source_url:-rtmp://127.0.0.1/live/source2k}"; then
    fail "停源测试：停止推流进程失败"
    stop_record "$STREAM_PATH" || true
    return 1
  fi
  sleep "$STOP_SOURCE_WAIT"
  # 停源后录制可能已自行结束，404 按幂等成功
  stop_record "$STREAM_PATH" || { fail "停源测试录制停止失败"; return 1; }
  sleep 2

  local files count abs probe
  if ! files=$(wait_record_files "$STREAM_PATH" "$file_path" 1); then
    fail "停源测试：录制停止后未找到完整文件（filePath=${file_path}）"
    return 1
  fi
  count=$(printf '%s\n' "$files" | wc -l | tr -d ' ')
  if [ "$count" != "1" ]; then
    fail "停源测试：期望 1 个文件，实际 $count"
    return 1
  fi
  abs=$(resolve_local_path "$files")
  local size
  size=$(file_size "$abs" 2>/dev/null || echo 0)
  probe="$OUT_DIR/probe-stop-source.json"
  if ! probe_json "$abs" > "$probe" 2>"$OUT_DIR/probe-stop-source.err"; then
    fail "停源测试：ffprobe 无法读取 ${abs}（可能是 moov 未收尾）"
    return 1
  fi
  info "停源测试 ffprobe：$(probe_summary "$probe")"
  if python3 - "$probe" "$size" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
size = int(sys.argv[2])
v = next((s for s in p.get("streams", []) if s.get("codec_type") == "video"), None)
fmt = p.get("format") or {}
dur = float(fmt.get("duration") or 0)
if size <= 0:
    print("file size=%d" % size); sys.exit(1)
if v is None:
    print("missing video stream"); sys.exit(1)
if dur <= 0:
    print("duration=%s <= 0" % fmt.get("duration")); sys.exit(1)
print("size=%dB duration=%.3fs" % (size, dur))
PY
  then
    pass "停源收尾：文件非零字节、moov 可读、duration 正常（${abs}）"
  else
    fail "停源收尾断言未通过（ffprobe JSON：${probe}）"
  fi
  return 0
}

write_whip_runner() {
  cat > "$WHIP_RUNNER_JS" <<'JS'
// 由 record-check.sh 运行时生成（不提交）。
// 用 Chrome 假设备向 /webrtc/test/publish 推流；先把 RTCRtpSender.getCapabilities
// 过滤为 video/H264 + audio/opus，避免页面强制选择 PCMA。
// 推流就绪后写 ready 文件，保持推流直到 stop 标记出现，然后关闭浏览器。
const fs = require('fs');
const { chromium } = require('playwright');

const API = process.env.API_BASE;
const STREAM = process.env.STREAM_PATH;
const READY_FILE = process.env.READY_FILE;
const STOP_FILE = process.env.STOP_FILE;
const LOG_PATH = process.env.LOG_PATH;
const WAIT_MS = Number(process.env.WAIT_MS || 30000);

const INIT = `(() => {
  const origGet = RTCRtpSender.getCapabilities.bind(RTCRtpSender);
  RTCRtpSender.getCapabilities = function (kind) {
    const caps = origGet(kind);
    if (!caps || !caps.codecs) return caps;
    if (kind === 'video') {
      caps.codecs = caps.codecs.filter((c) => c.mimeType.toLowerCase() === 'video/h264');
    } else if (kind === 'audio') {
      caps.codecs = caps.codecs.filter((c) => c.mimeType.toLowerCase() === 'audio/opus');
    }
    return caps;
  };
})();`;

const log = (line) => {
  const text = `[whip-runner] ${line}`;
  console.log(text);
  fs.appendFileSync(LOG_PATH, text + '\n');
};

(async () => {
  fs.writeFileSync(LOG_PATH, '');
  const browser = await chromium.launch({
    channel: process.env.PLAYWRIGHT_CHANNEL || 'chrome',
    headless: true,
    args: [
      '--use-fake-device-for-media-stream',
      '--use-fake-ui-for-media-stream',
      '--autoplay-policy=no-user-gesture-required',
      '--disable-background-timer-throttling',
    ],
  });
  try {
    const context = await browser.newContext({ permissions: ['camera', 'microphone'] });
    await context.addInitScript({ content: INIT });
    const page = await context.newPage();
    page.on('console', (msg) => fs.appendFileSync(LOG_PATH, `[page] ${msg.text()}\n`));
    page.on('pageerror', (err) => fs.appendFileSync(LOG_PATH, `[pageerror] ${err.message}\n`));
    const url = `${API}/webrtc/test/publish?streamPath=${encodeURIComponent(STREAM)}`;
    log(`goto ${url}`);
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 20000 });

    const deadline = Date.now() + WAIT_MS;
    let info = null;
    while (Date.now() < deadline) {
      const res = await page.evaluate(async ({ api, stream }) => {
        const r = await fetch(`${api}/api/stream/list`);
        const j = await r.json();
        const s = (j.data || []).find((x) => x.path === stream);
        if (!s || !s.videoTrack || !s.audioTrack) return null;
        return {
          path: s.path,
          videoCodec: s.videoTrack.codec,
          audioCodec: s.audioTrack.codec,
          width: s.videoTrack.width,
          height: s.videoTrack.height,
        };
      }, { api: API, stream: STREAM });
      if (res) { info = res; break; }
      await page.waitForTimeout(300);
    }
    if (!info) {
      log('FAILED: stream with video+audio tracks not observed within wait window');
      process.exitCode = 1;
      return;
    }
    fs.writeFileSync(READY_FILE, JSON.stringify(info));
    log(`ready ${JSON.stringify(info)}`);
    while (!fs.existsSync(STOP_FILE)) {
      await page.waitForTimeout(200);
    }
    log('stop flag seen, closing browser');
  } finally {
    await browser.close();
  }
})().catch((error) => {
  fs.appendFileSync(LOG_PATH, `[whip-runner] FAILED: ${error && error.stack ? error.stack : error}\n`);
  process.exit(1);
});
JS
}

cmd_whip() {
  need_cmd curl
  need_cmd python3
  need_cmd ffprobe
  if ! command -v node >/dev/null 2>&1; then
    fail "A12：未安装 node，无法运行 Playwright 假设备推流"
    return 1
  fi
  require_api
  mkdir -p "$OUT_DIR"
  [ -d "$PLAYWRIGHT_DIR/node_modules/playwright" ] || {
    fail "A12：Playwright 不可用（缺少 $PLAYWRIGHT_DIR/node_modules/playwright）"
    return 1
  }
  local file_path="whip-$RUN_ID"
  rm -f "$WHIP_READY_FILE" "$WHIP_STOP_FILE"

  write_whip_runner
  log "启动 WHIP 假设备推流（Chrome channel=${PLAYWRIGHT_CHANNEL}，日志 ${WHIP_RUNNER_LOG}）"
  API_BASE="$API_BASE" STREAM_PATH="$WHIP_STREAM_PATH" READY_FILE="$WHIP_READY_FILE" \
  STOP_FILE="$WHIP_STOP_FILE" LOG_PATH="$WHIP_RUNNER_LOG" WAIT_MS=$((WAIT_TIMEOUT * 1000)) \
  PLAYWRIGHT_CHANNEL="$PLAYWRIGHT_CHANNEL" NODE_PATH="$PLAYWRIGHT_DIR/node_modules" \
    nohup node "$WHIP_RUNNER_JS" >"$OUT_DIR/whip-runner.stdout" 2>&1 &
  local runner_pid=$!
  echo "$runner_pid" > "$WHIP_RUNNER_PID_FILE"

  local deadline=$((SECONDS + WAIT_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    kill -0 "$runner_pid" 2>/dev/null || break
    [ -f "$WHIP_READY_FILE" ] && break
    sleep "$POLL_INTERVAL"
  done
  if [ ! -f "$WHIP_READY_FILE" ]; then
    warn "WHIP 推流未就绪，runner 输出尾部："
    tail -n 30 "$WHIP_RUNNER_LOG" >&2 || true
    fail "A12：WHIP 假设备推流未产生带 video+audio 的流（${WHIP_STREAM_PATH}）"
    rm -f "$WHIP_STOP_FILE"
    touch "$WHIP_STOP_FILE"
    local fail_waited=0
    while [ "$fail_waited" -lt 100 ] && kill -0 "$runner_pid" 2>/dev/null; do
      sleep 0.1
      fail_waited=$((fail_waited + 1))
    done
    if kill -0 "$runner_pid" 2>/dev/null; then
      stop_pid_checked "$runner_pid" "$WHIP_RUNNER_JS" || true
    fi
    wait "$runner_pid" 2>/dev/null || true
    rm -f "$WHIP_RUNNER_PID_FILE" "$WHIP_STOP_FILE"
    return 1
  fi
  info "A12 WHIP 推流就绪：$(cat "$WHIP_READY_FILE")"
  read -r _ audio_codec video_codec _res _meta <<< "$(stream_state "$WHIP_STREAM_PATH")"
  if [ "$audio_codec" != "-" ] && [ "$audio_codec" != "None" ]; then
    pass "A12 推流侧：video=$video_codec audio=${audio_codec}（期望 h264+opus）"
  else
    fail "A12 推流侧：$WHIP_STREAM_PATH 无音轨（video=$video_codec audio=${audio_codec}）"
  fi

  local started=0
  if start_record "$WHIP_STREAM_PATH" "$file_path" "600s" "0" "mp4"; then
    started=1
    sleep "$WHIP_RECORD_SECONDS"
    stop_record "$WHIP_STREAM_PATH" || fail "A12 录制停止失败"
  else
    fail "A12 WHIP 录制启动失败"
  fi

  touch "$WHIP_STOP_FILE"
  local waited=0
  while [ "$waited" -lt 100 ] && kill -0 "$runner_pid" 2>/dev/null; do
    sleep 0.1
    waited=$((waited + 1))
  done
  if kill -0 "$runner_pid" 2>/dev/null; then
    warn "WHIP runner 未在 10s 内退出，按 PID 停止（${runner_pid}）"
    stop_pid_checked "$runner_pid" "$WHIP_RUNNER_JS" || true
  fi
  wait "$runner_pid" 2>/dev/null || true
  rm -f "$WHIP_RUNNER_PID_FILE" "$WHIP_STOP_FILE"

  [ "$started" = "1" ] || return 1
  sleep 3
  local files count
  if ! files=$(wait_record_files "$WHIP_STREAM_PATH" "$file_path"); then
    fail "A12：停止录制后未找到文件（filePath=${file_path}）"
    return 1
  fi
  count=$(printf '%s\n' "$files" | wc -l | tr -d ' ')
  info "A12 录制文件数=${count}（每文件条目：$(list_records "$WHIP_STREAM_PATH" "$file_path" | tr '\n' ';')）"

  # 取时间上最新的文件做 ffprobe（其余为录制器拒绝 Opus 后每秒重试产生的碎片）。
  # API 列表是“最新在前”，不能 tail -n 1（那会拿到最旧文件）；按文件名排序后取最后一行。
  local newest
  newest=$(printf '%s\n' "$files" | sort_paths_chronological | tail -n 1)
  local abs probe
  abs=$(resolve_local_path "$newest")
  probe="$OUT_DIR/probe-whip.json"
  if ! probe_json "$abs" > "$probe" 2>"$OUT_DIR/probe-whip.err"; then
    fail "A12：ffprobe 无法读取 $abs"
    return 1
  fi
  info "A12 ffprobe：$(probe_summary "$probe")"
  if python3 - "$probe" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
v = next((s for s in p.get("streams", []) if s.get("codec_type") == "video"), None)
if v is None or v.get("codec_name") != "h264":
    print("video codec != h264"); sys.exit(1)
print("video=h264")
PY
  then
    pass "A12 视频：录制文件包含 H264 视频轨（${abs}）"
  else
    fail "A12 视频断言未通过（ffprobe JSON：${probe}）"
  fi
  if python3 - "$probe" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
a = next((s for s in p.get("streams", []) if s.get("codec_type") == "audio"), None)
if a is None:
    print("missing audio stream"); sys.exit(1)
if a.get("codec_name") != "opus":
    print("audio codec=%s != opus" % a.get("codec_name")); sys.exit(1)
print("audio=opus")
PY
  then
    pass "A12 音频：MP4 内含 Opus 音轨（${abs}）"
  else
    fail "A12 音频：MP4 内无 Opus 音轨（progressive mp4 录制器不支持 Opus）"
  fi

  # Opus-in-MP4 播放结论
  cmd_playback || true
  return 0
}

# ---------- Opus-in-MP4 播放探测 ----------

write_playback_helpers() {
  mkdir -p "$PLAYBACK_DIR"
  cat > "$PLAYBACK_DIR/player.html" <<'HTML'
<!DOCTYPE html><html><body>
<video id="v" src="probe.mp4" muted playsinline></video>
<script>
  window.__result = { events: [] };
  const v = document.getElementById('v');
  ['loadedmetadata', 'loadeddata', 'canplay', 'canplaythrough', 'playing', 'error'].forEach((name) => {
    v.addEventListener(name, () => window.__result.events.push(name));
  });
  window.__result.canPlayType = v.canPlayType('video/mp4; codecs="avc1.42E01E, opus"');
  v.play().catch((e) => { window.__result.playError = String(e); });
  setTimeout(() => {
    window.__result.readyState = v.readyState;
    window.__result.videoWidth = v.videoWidth;
    window.__result.duration = v.duration;
    window.__result.currentTime = v.currentTime;
    window.__result.error = v.error ? `${v.error.code}:${v.error.message}` : null;
    window.__result.done = true;
  }, 6000);
</script>
</body></html>
HTML
  cat > "$PLAYBACK_CHROME_JS" <<'JS'
const { chromium } = require('playwright');
(async () => {
  const browser = await chromium.launch({
    channel: process.env.PLAYWRIGHT_CHANNEL || 'chrome',
    headless: true,
    args: ['--autoplay-policy=no-user-gesture-required'],
  });
  try {
    const page = await browser.newPage();
    await page.goto(process.env.URL, { waitUntil: 'domcontentloaded', timeout: 15000 });
    await page.waitForFunction(() => window.__result && window.__result.done, null, { timeout: 20000 });
    console.log(JSON.stringify(await page.evaluate(() => window.__result)));
  } finally {
    await browser.close();
  }
})().catch((e) => { console.error('FAILED', e); process.exit(1); });
JS
}

# cmd_playback [file]：不传 file 时用 ffmpeg 生成 10s 的 Opus-in-MP4 参照文件
# （ffmpeg 合成的参照样本，用于判断播放器/浏览器对容器+编码组合的支持，
#  不是 Monibuca 录制产物；Monibuca 侧的 Opus 录制结论见 whip 子命令）。
cmd_playback() {
  need_cmd ffprobe
  local file="${1:-}"
  mkdir -p "$PLAYBACK_DIR"
  if [ -n "$file" ]; then
    [ -f "$file" ] || { fail "playback：文件不存在：$file"; return 1; }
  fi
  if [ -z "$file" ]; then
    need_cmd ffmpeg
    file="$PLAYBACK_DIR/probe.mp4"
    log "生成 Opus-in-MP4 参照文件（ffmpeg 合成）：$file"
    ffmpeg -hide_banner -loglevel error -y \
      -f lavfi -i testsrc2=size=640x480:rate=25 \
      -f lavfi -i sine=frequency=440:sample_rate=48000 \
      -t 10 -c:v libx264 -preset ultrafast -pix_fmt yuv420p \
      -c:a libopus -b:a 96k -movflags +faststart "$file" || {
        fail "playback：ffmpeg 生成 Opus-in-MP4 参照文件失败"
        return 1
      }
  fi
  local abs_probe="$PLAYBACK_DIR/probe-playback.json"
  if ! probe_json "$file" > "$abs_probe"; then
    fail "playback：ffprobe 无法读取 $file"
    return 1
  fi
  info "playback ffprobe：$(probe_summary "$abs_probe")"
  if ! python3 - "$abs_probe" <<'PY'
import json, sys
p = json.load(open(sys.argv[1]))
a = next((s for s in p.get("streams", []) if s.get("codec_type") == "audio"), None)
sys.exit(0 if a and a.get("codec_name") == "opus" else 1)
PY
  then
    pending "playback：$file 不含 Opus 音轨，跳过 Opus-in-MP4 播放探测"
    return 0
  fi

  # ffplay 解码检查（dummy 音频设备，避免 CI/无音频环境失败）
  if command -v ffplay >/dev/null 2>&1; then
    local ffplay_rc=0
    SDL_AUDIODRIVER=dummy ffplay -autoexit -nodisp -loglevel error -t "$PLAYBACK_PROBE_SECONDS" "$file" \
      >"$PLAYBACK_DIR/ffplay.log" 2>&1 || ffplay_rc=$?
    if [ "$ffplay_rc" = "0" ]; then
      pass "playback ffplay：Opus-in-MP4 解码播放成功（rc=0）"
    else
      fail "playback ffplay：Opus-in-MP4 播放失败（rc=${ffplay_rc}，日志 $PLAYBACK_DIR/ffplay.log）"
    fi
  else
    pending "playback ffplay：未安装 ffplay"
  fi

  # VLC 播放检查（dummy 接口，--stop-time 限制时长）
  local vlc_bin="/Applications/VLC.app/Contents/MacOS/VLC"
  if [ -x "$vlc_bin" ]; then
    local vlc_out
    if vlc_out=$(python3 - "$vlc_bin" "$file" "$PLAYBACK_PROBE_SECONDS" <<'PY'
import subprocess, sys
VLC, FILE, SECONDS = sys.argv[1], sys.argv[2], sys.argv[3]
cmd = [VLC, "-I", "dummy", "--play-and-exit", "--no-video", "--aout=dummy",
       "--stop-time=%s" % SECONDS, FILE]
try:
    p = subprocess.run(cmd, capture_output=True, text=True, timeout=40)
    print("rc=%d" % p.returncode)
    print((p.stderr or "")[-2000:])
except subprocess.TimeoutExpired:
    print("rc=timeout")
    sys.exit(1)
PY
    ); then
      pass "playback VLC：Opus-in-MP4 播放成功（$(printf '%s' "$vlc_out" | head -n 1)）"
    else
      fail "playback VLC：Opus-in-MP4 播放失败（${vlc_out}）"
    fi
  else
    pending "playback VLC：未找到 $vlc_bin"
  fi

  # Chrome <video> 播放检查
  if command -v node >/dev/null 2>&1 && [ -d "$PLAYWRIGHT_DIR/node_modules/playwright" ]; then
    write_playback_helpers
    if [ "$file" != "$PLAYBACK_DIR/probe.mp4" ]; then
      cp "$file" "$PLAYBACK_DIR/probe.mp4"
    fi
    local http_pid="" http_code=""
    ( cd "$PLAYBACK_DIR" && exec python3 -m http.server "$PLAYBACK_HTTP_PORT" --bind 127.0.0.1 \
        >"$PLAYBACK_DIR/http.log" 2>&1 ) &
    http_pid=$!
    echo "$http_pid" > "$PLAYBACK_HTTP_PID_FILE"
    local deadline=$((SECONDS + 10))
    while [ "$SECONDS" -lt "$deadline" ]; do
      http_code=$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PLAYBACK_HTTP_PORT/player.html" 2>/dev/null || true)
      [ "$http_code" = "200" ] && break
      sleep 0.2
    done
    if [ "$http_code" = "200" ]; then
      local chrome_out=""
      if chrome_out=$(URL="http://127.0.0.1:$PLAYBACK_HTTP_PORT/player.html" \
        PLAYWRIGHT_CHANNEL="$PLAYWRIGHT_CHANNEL" NODE_PATH="$PLAYWRIGHT_DIR/node_modules" \
        node "$PLAYBACK_CHROME_JS" 2>"$PLAYBACK_DIR/chrome-probe.err"); then
        info "playback Chrome：$chrome_out"
        if python3 -c 'import json,sys
r = json.loads(sys.argv[1])
ok = r.get("error") is None and r.get("readyState", 0) >= 3 and r.get("videoWidth", 0) > 0
sys.exit(0 if ok else 1)' "$chrome_out"; then
          pass "playback Chrome：Opus-in-MP4 可播放（canPlayType=$(printf '%s' "$chrome_out" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("canPlayType"))'))"
        else
          fail "playback Chrome：Opus-in-MP4 不可播放（${chrome_out}）"
        fi
      else
        fail "playback Chrome：探测脚本失败（见 $PLAYBACK_DIR/chrome-probe.err）"
      fi
    else
      fail "playback Chrome：本地 HTTP 服务未就绪（端口 ${PLAYBACK_HTTP_PORT}）"
    fi
    if [ -n "$http_pid" ] && kill -0 "$http_pid" 2>/dev/null; then
      kill -TERM "$http_pid" 2>/dev/null || true
    fi
    rm -f "$PLAYBACK_HTTP_PID_FILE"
  else
    pending "playback Chrome：node 或 Playwright 不可用"
  fi
  return 0
}

# ---------- 清理 ----------

cmd_cleanup() {
  mkdir -p "$OUT_DIR"
  # 音频推流源
  local pid
  pid=$(cat "$AUDIO_SOURCE_PID_FILE" 2>/dev/null || true)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    if stop_pid_checked "$pid" "$AUDIO_SOURCE_RTMP_URL"; then
      log "已停止音频推流 PID=$pid"
    fi
  fi
  rm -f "$AUDIO_SOURCE_PID_FILE"

  # WHIP runner
  pid=$(cat "$WHIP_RUNNER_PID_FILE" 2>/dev/null || true)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    stop_pid_checked "$pid" "$WHIP_RUNNER_JS" || true
  fi
  rm -f "$WHIP_RUNNER_PID_FILE" "$WHIP_STOP_FILE"

  # playback HTTP 服务
  pid=$(cat "$PLAYBACK_HTTP_PID_FILE" 2>/dev/null || true)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
  fi
  rm -f "$PLAYBACK_HTTP_PID_FILE"

  # RTSP 拉流链路的推流源：rtsp-pull.sh 把 PID 记录在 state.env；
  # 只停该链路自己启动的源（SOURCE_STARTED=1），外部源保持运行。
  local state_file="$RTSP_STATE_DIR/state.env" src_pid src_started src_url
  src_pid=$(state_value "$state_file" SOURCE_PID)
  src_started=$(state_value "$state_file" SOURCE_STARTED)
  src_url=$(state_value "$state_file" SOURCE_RTMP_URL)
  if [ "${src_started:-0}" = "1" ] && pid_matches "${src_pid:-}" "${src_url:-rtmp://127.0.0.1/live/source2k}"; then
    if stop_pid_checked "$src_pid" "${src_url:-rtmp://127.0.0.1/live/source2k}"; then
      log "已停止 RTSP 链路的推流源 PID=${src_pid}"
    fi
  fi
  rm -f "$RTSP_STATE_DIR/state.env" "$AUDIO_STATE_DIR/state.env"

  if api_ok; then
    # 只删本脚本新建并记录过 ID 的代理；不按 streamPath 批量删除（会误伤同名的已有代理）
    remove_recorded_proxies "$AUDIO_PROXY_IDS_FILE" "$AUDIO_STREAM_PATH"
    remove_recorded_proxies "$RTSP_PROXY_IDS_FILE" "$STREAM_PATH"
  else
    warn "API 不可达，跳过已记录代理的清理（服务恢复后可用 /api/proxy/pull/remove/<id> 手工删除）"
  fi

  # 临时 runner/探测文件（保留日志与 ffprobe JSON）
  rm -f "$WHIP_RUNNER_JS" "$WHIP_READY_FILE" "$WHIP_STOP_FILE"
  rm -rf "$PLAYBACK_DIR/probe.mp4" "$PLAYBACK_DIR/player.html" "$PLAYBACK_CHROME_JS"
  CLEANED=1
  log "cleanup 完成（服务端本身不由本脚本启动/停止）"
}

# ---------- 汇总 ----------

print_summary() {
  echo
  echo "==== record-check summary ===="
  if [ "${#RESULTS[@]}" -eq 0 ]; then
    echo "(无结果)"
  else
    local item
    for item in "${RESULTS[@]}"; do
      printf '%s\n' "$item"
    done
  fi
  echo "=============================="
  if [ "$FAILED" != "0" ]; then
    echo "结论：存在 FAIL 项（见上）"
  else
    echo "结论：本次运行的断言全部通过（PENDING 项未执行/待真机）"
  fi
  echo
  echo "PENDING（本机不可完成，Linux 真机命令）："
  echo "  - 10 分钟长录：RECORD_SECONDS=600 ./record-check.sh rtsp（或循环调用 /mp4/api/start|stop）"
  echo "  - 真实摄像头 RTSP 源：把 RTSP_URL 指向摄像头，START_SOURCE=0 由外部提供源"
  echo "  - 弱网/丢包（Linux tc）：tc qdisc add dev eth0 root netem loss 1% delay 20ms，再跑 rtsp/whip"
  echo "  - 10 路并发录制（Linux）：对 live/stream1..10 并发 POST /mp4/api/start（完整循环模板见"
  echo "      example/webrtc-2k-perf/report-record.md §9），观察 CPU/磁盘/DB"
}

usage() {
  cat <<'EOF'
用法: ./record-check.sh <rtsp|audio|fragment|stop-source|whip|playback|self-test|all|cleanup>

  rtsp         A11 视频：live/rtsp2k 单文件录制 + ffprobe 断言（2560x1440/h264/≈24fps）
  audio        A11 音频：起 AAC 2K 源 + RTSP 拉流，录制并断言 AAC + 时长非零
  fragment     A13 分片：fragment=1m 录 FRAGMENT_SECONDS（默认 175s → 3 个文件；
               190s 会跨 180s 边界变 4 个），校验分片数/每片可播/≈60s
  stop-source  录制中只停 RTMP 推流源（按 PID），校验文件可读、非零字节
  whip         A12：Chrome 假设备推 H264+Opus，录制并检查 MP4 音轨 + 播放结论
  playback [f] Opus-in-MP4 播放探测（ffplay/VLC/Chrome；不传文件则用 ffmpeg 生成参照样本）
  self-test    分片角色断言合成自测（无需服务端）：时间顺序 PASS、反转时长/顺序 FAIL
  all          依次执行 rtsp/audio/fragment/stop-source/whip，然后 cleanup 并汇总
  cleanup      停止本脚本启动的源/浏览器/HTTP 服务并删除代理，保留日志与 ffprobe JSON

环境变量见脚本头部注释；常用：API_BASE STREAM_PATH RECORD_SECONDS FRAGMENT
OUT_DIR STORAGE_ROOT PLAYWRIGHT_DIR WHIP_RECORD_SECONDS
EOF
}

main() {
  mkdir -p "$OUT_DIR"
  case "${1:-}" in
    rtsp|audio|fragment|stop-source|whip|playback|all)
      # 任何子命令中途失败/异常退出都清理本脚本启动的后台进程与已记录的代理，
      # 避免留下 ffmpeg/Playwright/拉流代理（cmd_cleanup 幂等，CLEANED 防止重复执行）。
      trap 'if [ "$CLEANED" = "0" ]; then cmd_cleanup || true; fi' EXIT
      ;;
  esac
  case "${1:-}" in
    rtsp)        cmd_rtsp ;;
    audio)       cmd_audio ;;
    fragment)    cmd_fragment ;;
    stop-source) cmd_stop_source ;;
    whip)        cmd_whip ;;
    playback)    shift || true; cmd_playback "${1:-}" ;;
    self-test)   cmd_self_test ;;
    cleanup)     cmd_cleanup ;;
    all)
      need_cmd curl; need_cmd python3
      require_api
      cmd_rtsp || true
      cmd_audio || true
      cmd_fragment || true
      cmd_stop_source || true
      cmd_whip || true
      cmd_cleanup || true
      ;;
    -h|--help|help|"") usage; [ -n "${1:-}" ] || exit 1 ;;
    *) usage; exit 1 ;;
  esac
  print_summary
  [ "$FAILED" = "0" ] || exit 1
}

main "$@"
