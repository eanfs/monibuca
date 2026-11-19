#!/usr/bin/env bash
# RTSP 拉流兼容性验证脚本（WebRTC 2K@24 升级计划 Task 5）。
#
# 思路：本机没有摄像头，也不依赖 Docker/Linux tc，因此用「同一个 Monibuca 实例」
# 完成控制回路验证：
#   1) ffmpeg 经 RTMP 向本实例推 2560x1440@24 的 source2k（复用 Task 0 的 push-2k.sh）；
#   2) 通过 /api/proxy/pull/add 建 RTSP 拉流代理，把 rtsp://127.0.0.1:8554/live/source2k
#      拉到 live/rtsp2k（走真实的 RTSP DESCRIBE/SETUP/PLAY 与 RTP 收流路径）；
#   3) 用 Playwright 打开 /webrtc/test/subscribe 播放 live/rtsp2k 并采集 getStats。
#
# 注意：源和 RTSP 服务是同一个 Monibuca 进程，这是控制回路 sanity check，
# 不等价于独立摄像头/GOP 的真机验收（见 report-rtsp-pull.md 的待真机表）。
#
# 用法：
#   ./rtsp-pull.sh start       # 可选起推流源 + 建拉流代理 + 等拉流就绪
#   ./rtsp-pull.sh status      # 打印 API/源/代理/拉流状态
#   ./rtsp-pull.sh play        # Playwright 播放并断言 avgFps >= 23.5
#   ./rtsp-pull.sh reconnect   # 杀源 → 等 RECONNECT_WAIT 秒 → 重启源，测恢复时间
#   ./rtsp-pull.sh stop        # 删除代理、停掉自己起的源/播放器，不留后台进程
#
# 环境变量（全部可覆盖）：
#   API_BASE         http://127.0.0.1:8080
#   RTSP_URL         rtsp://127.0.0.1:8554/live/source2k
#   STREAM_PATH      live/rtsp2k
#   SOURCE_RTMP_URL  rtmp://127.0.0.1/live/source2k
#   START_SOURCE     1（0 = 只拉外部已存在的源，不启动 ffmpeg）
#   PLAY_SECONDS     60
#   RECONNECT_WAIT   5
#   PLAY_URL_BASE    http://127.0.0.1:8080/webrtc/test/subscribe
#   PRESET           ultrafast（Task 0 实测 veryfast 在本机无法稳定实时 2K@24）
#   WAIT_TIMEOUT     90   等待流/代理就绪的总超时（秒）
#   POLL_INTERVAL    0.5  轮询间隔（秒）
#   MIN_AVG_FPS      23.5 play 子命令的断言阈值
#   PLAYWRIGHT_DIR   /private/tmp/webrtc-2k-run（含 node_modules/playwright 的目录）
#   PLAYWRIGHT_CHANNEL chrome
#   OUT_DIR          /tmp/webrtc-2k-rtsp（状态文件、日志、summary、samples）
#   API_TOKEN        可选，admin 开启登录时填 Bearer token
#   SUMMARY_OUT / SAMPLES_OUT / CONSOLE_LOG  可选，覆盖 play 输出路径
#
# 前置条件：
#   - 服务端已启动且 RTSP 监听 :8554，例如：
#       cd example/default
#       RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 go run -tags sqlite main.go
#   - ffmpeg / curl / python3 / node + Playwright（play 子命令才需要）
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

API_BASE="${API_BASE:-http://127.0.0.1:8080}"
RTSP_URL="${RTSP_URL:-rtsp://127.0.0.1:8554/live/source2k}"
STREAM_PATH="${STREAM_PATH:-live/rtsp2k}"
SOURCE_RTMP_URL="${SOURCE_RTMP_URL:-rtmp://127.0.0.1/live/source2k}"
START_SOURCE="${START_SOURCE:-1}"
PLAY_SECONDS="${PLAY_SECONDS:-60}"
RECONNECT_WAIT="${RECONNECT_WAIT:-5}"
PLAY_URL_BASE="${PLAY_URL_BASE:-http://127.0.0.1:8080/webrtc/test/subscribe}"

PRESET="${PRESET:-ultrafast}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-90}"
POLL_INTERVAL="${POLL_INTERVAL:-0.5}"
MIN_AVG_FPS="${MIN_AVG_FPS:-23.5}"
PLAYWRIGHT_DIR="${PLAYWRIGHT_DIR:-/private/tmp/webrtc-2k-run}"
PLAYWRIGHT_CHANNEL="${PLAYWRIGHT_CHANNEL:-chrome}"
API_TOKEN="${API_TOKEN:-}"
OUT_DIR="${OUT_DIR:-/tmp/webrtc-2k-rtsp}"
STATE_FILE="$OUT_DIR/state.env"
SUMMARY_OUT="${SUMMARY_OUT:-$OUT_DIR/summary.json}"
SAMPLES_OUT="${SAMPLES_OUT:-$OUT_DIR/samples.jsonl}"
CONSOLE_LOG="${CONSOLE_LOG:-$OUT_DIR/console.log}"
PUSH_LOG="$OUT_DIR/push.log"
RUNNER_PATH="$OUT_DIR/play-rtsp-runner.js"

API_BASE="${API_BASE%/}"

log()  { printf '[rtsp-pull] %s\n' "$*"; }
warn() { printf '[rtsp-pull] WARN: %s\n' "$*" >&2; }
die()  { printf '[rtsp-pull] ERROR: %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"
}

now_ms() {
  python3 -c 'import time; print(int(time.time() * 1000))'
}

status_name() {
  case "$1" in
    0) printf 'Offline' ;;
    1) printf 'Online' ;;
    2) printf 'Pulling' ;;
    3) printf 'Disabled' ;;
    *) printf 'Unknown(%s)' "$1" ;;
  esac
}

# ---------- HTTP / JSON 辅助（不依赖 jq） ----------

api_curl() {
  if [ -n "$API_TOKEN" ]; then
    curl -sS --max-time 15 -H "Authorization: Bearer $API_TOKEN" "$@"
  else
    curl -sS --max-time 15 "$@"
  fi
}

api_get() {
  api_curl "$API_BASE$1"
}

api_post_json() {
  local path="$1" body="$2"
  api_curl -X POST -H 'Content-Type: application/json' --data-binary "$body" "$API_BASE$path"
}

# 返回 HTTP 状态码，响应体写入 $3
api_post_json_check() {
  local path="$1" body="$2" out="$3"
  api_curl -o "$out" -w '%{http_code}' \
    -X POST -H 'Content-Type: application/json' --data-binary "$body" "$API_BASE$path"
}

api_ok() {
  local code
  code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 5 "$API_BASE/api/stream/list" 2>/dev/null || true)
  [ "$code" = "200" ]
}

require_api() {
  api_ok || die "无法访问 $API_BASE/api/stream/list。请先启动服务端（见脚本头注释），或用 API_BASE 指定地址。"
}

# 输出 "<id> <status> <pullURL>"；没有匹配（或只有 disabled 记录）时输出 "-1 -1 -"
proxy_state() {
  api_get /api/proxy/pull/list | python3 -c '
import json, sys
path = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    print("-1 -1 -")
    sys.exit(0)
best = None
for item in data.get("data") or []:
    if item.get("streamPath") != path:
        continue
    try:
        status = int(item.get("status", 0))
        # protojson 对字段名 ID 的 json_name 是 "ID"（大写），兼容两种写法
        ident = int(item.get("id", item.get("ID", 0)))
    except (TypeError, ValueError):
        continue
    if status == 3:
        continue
    if best is None or ident > best[0]:
        best = (ident, status, item.get("pullURL", ""))
if best:
    print("%d %d %s" % best)
else:
    print("-1 -1 -")
' "$STREAM_PATH"
}

# 列出该 streamPath 的全部代理 ID（含 disabled），用于 start 前清理/stop 删除
proxy_ids_all() {
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
' "$STREAM_PATH"
}

# 输出 "<present> <subscribers> <WxH> <codec>"
stream_state() {
  api_get /api/stream/list | python3 -c '
import json, sys
path = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    print("0 0 0x0 -")
    sys.exit(0)
for item in data.get("data") or []:
    if item.get("path") != path:
        continue
    vt = item.get("videoTrack") or {}
    print("1", item.get("subscribers", 0),
          "%sx%s" % (vt.get("width", 0), vt.get("height", 0)),
          vt.get("codec", "-"))
    break
else:
    print("0 0 0x0 -")
' "$1"
}

# 输出该流视频轨的 bps 字段（服务端实现为字节/秒），取不到输出 0
stream_bps() {
  api_get /api/stream/list | python3 -c '
import json, sys
path = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    print(0)
    sys.exit(0)
for item in data.get("data") or []:
    if item.get("path") == path:
        vt = item.get("videoTrack") or {}
        try:
            print(int(vt.get("bps", 0)))
        except (TypeError, ValueError):
            print(0)
        break
else:
    print(0)
' "$1"
}

# RTMP URL -> Monibuca streamPath（rtmp://host/app/stream -> app/stream）
rtmp_stream_path() {
  python3 -c 'import sys, urllib.parse; p = urllib.parse.urlparse(sys.argv[1]).path.strip("/"); print(p if p else "")' "$1"
}

# RTSP URL -> host port（用于预检 RTSP 服务是否在听）
rtsp_host_port() {
  python3 -c 'import sys, urllib.parse; u = urllib.parse.urlparse(sys.argv[1]); print(u.hostname or "127.0.0.1", u.port or 554)' "$1"
}

check_rtsp_reachable() {
  read -r host port <<< "$(rtsp_host_port "$RTSP_URL")"
  python3 - "$host" "$port" <<'PY' || die "RTSP 服务不可达 $host:${port}。请确认服务端以 RTSP_TCP_LISTENADDR=:8554 启动。"
import socket, sys
host, port = sys.argv[1], int(sys.argv[2])
try:
    with socket.create_connection((host, port), timeout=3):
        pass
except OSError as error:
    print("connect %s:%d failed: %s" % (host, port, error), file=sys.stderr)
    sys.exit(1)
PY
}

wait_for_source() {
  local source_path="$1" deadline=$((SECONDS + WAIT_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ -n "${SOURCE_PID:-}" ] && ! kill -0 "$SOURCE_PID" 2>/dev/null; then
      warn "推流进程 PID=$SOURCE_PID 已退出，日志尾部："
      tail -n 20 "$PUSH_LOG" >&2 || true
      return 1
    fi
    read -r present _ <<< "$(stream_state "$source_path")"
    if [ "$present" = "1" ]; then
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  return 1
}

# 等代理状态 Online/Pulling + STREAM_PATH 出现
wait_for_pull_ready() {
  local deadline=$((SECONDS + WAIT_TIMEOUT))
  local id status url present subs res codec
  while [ "$SECONDS" -lt "$deadline" ]; do
    read -r id status url <<< "$(proxy_state)"
    read -r present subs res codec <<< "$(stream_state "$STREAM_PATH")"
    if [ "$id" != "-1" ] && { [ "$status" = "1" ] || [ "$status" = "2" ]; } && [ "$present" = "1" ]; then
      log "拉流就绪：proxyId=$id status=$status($(status_name "$status")) stream=$STREAM_PATH subscribers=$subs video=$res codec=$codec"
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done
  read -r id status url <<< "$(proxy_state)"
  read -r present subs res codec <<< "$(stream_state "$STREAM_PATH")"
  warn "等待超时（${WAIT_TIMEOUT}s）：proxyId=$id status=$status($(status_name "$status")) streamPresent=$present video=$res"
  return 1
}

load_state() {
  [ -f "$STATE_FILE" ] || die "未找到状态文件 ${STATE_FILE}，请先执行 ./rtsp-pull.sh start"
  # shellcheck disable=SC1090
  . "$STATE_FILE"
}

pid_matches() {
  local pid="$1" marker="$2" cmd
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  cmd=$(ps -p "$pid" -o command= 2>/dev/null || true)
  case "$cmd" in
    *"$marker"*) return 0 ;;
    *) warn "PID $pid 命令行不匹配标记 ${marker}，跳过（避免误杀）：${cmd:-<empty>}"; return 1 ;;
  esac
}

stop_pid() {
  local pid="$1" waited=0
  kill -TERM "$pid" 2>/dev/null || return 0
  while [ "$waited" -lt 50 ] && kill -0 "$pid" 2>/dev/null; do
    sleep 0.1
    waited=$((waited + 1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    warn "PID $pid 未在 5s 内退出，发送 SIGKILL"
    kill -KILL "$pid" 2>/dev/null || true
  fi
}

# ---------- 子命令 ----------

cmd_start() {
  need_cmd curl
  need_cmd python3
  require_api
  check_rtsp_reachable
  mkdir -p "$OUT_DIR"
  local started_at
  started_at=$(now_ms)

  # 清理同 streamPath 的旧代理（含 disabled 记录），保证每次 start 都是干净状态
  local ids id code
  ids=$(proxy_ids_all)
  if [ -n "$ids" ]; then
    for id in $ids; do
      code=$(api_post_json_check "/api/proxy/pull/remove/$id" '{}' "$OUT_DIR/remove-$id.json")
      if [ "$code" = "200" ]; then
        log "已清理旧拉流代理 id=$id"
      else
        warn "清理旧代理 id=$id 失败：HTTP $code $(cat "$OUT_DIR/remove-$id.json" 2>/dev/null || true)"
      fi
    done
    sleep 0.5
  fi

  local source_path source_pid="" source_started=0
  source_path=$(rtmp_stream_path "$SOURCE_RTMP_URL")
  [ -n "$source_path" ] || die "无法从 SOURCE_RTMP_URL 推导流路径：$SOURCE_RTMP_URL"

  if [ "$START_SOURCE" = "1" ]; then
    # 若上一次 start 启动的推流进程仍在，直接复用并继续追踪其 PID
    local prev_pid="" prev_started="" prev_url=""
    if [ -f "$STATE_FILE" ]; then
      prev_pid=$(awk -F= '/^SOURCE_PID=/{print $2}' "$STATE_FILE")
      prev_started=$(awk -F= '/^SOURCE_STARTED=/{print $2}' "$STATE_FILE")
      prev_url=$(awk -F= '/^SOURCE_RTMP_URL=/{print $2}' "$STATE_FILE")
    fi
    local present
    read -r present _ <<< "$(stream_state "$source_path")"
    if [ "${prev_started:-0}" = "1" ] && pid_matches "${prev_pid:-}" "${prev_url:-$SOURCE_RTMP_URL}"; then
      source_pid="$prev_pid"
      source_started=1
      SOURCE_PID="$source_pid"
      log "复用上一次 start 启动的推流进程 PID=${source_pid}"
      wait_for_source "$source_path" || die "已复用的推流进程存在，但源流 $source_path 未在 ${WAIT_TIMEOUT}s 内就绪"
    elif [ "$present" = "1" ]; then
      log "源流 $source_path 已存在（外部推流），本脚本不启动 ffmpeg"
    else
      [ -x "$SCRIPT_DIR/push-2k.sh" ] || die "未找到可执行脚本 $SCRIPT_DIR/push-2k.sh"
      log "启动 2K 推流：RTMP_URL=$SOURCE_RTMP_URL PRESET=${PRESET}（日志 ${PUSH_LOG}）"
      RTMP_URL="$SOURCE_RTMP_URL" DURATION=0 PRESET="$PRESET" \
        nohup "$SCRIPT_DIR/push-2k.sh" >"$PUSH_LOG" 2>&1 &
      source_pid=$!
      source_started=1
      SOURCE_PID="$source_pid"
      if ! wait_for_source "$source_path"; then
        stop_pid "$source_pid" || true
        die "源流 $source_path 未在 ${WAIT_TIMEOUT}s 内出现，已停止推流进程"
      fi
      log "源流就绪：${source_path}（推流 PID=${source_pid}）"
    fi
  else
    log "START_SOURCE=0：不启动推流，直接建拉流代理（源需已由外部提供）"
  fi

  local body
  body=$(python3 -c 'import json, sys
print(json.dumps({
    "name": "rtsp-2k-pull",
    "pullURL": sys.argv[1],
    "streamPath": sys.argv[2],
    "type": "rtsp",
    "pullOnStart": True,
    "stopOnIdle": False,
    "audio": True,
}))' "$RTSP_URL" "$STREAM_PATH")

  local add_status
  add_status=$(api_post_json_check /api/proxy/pull/add "$body" "$OUT_DIR/add-proxy.json")
  if [ "$add_status" != "200" ]; then
    [ "$source_started" = "1" ] && stop_pid "$source_pid" || true
    die "添加拉流代理失败：HTTP $add_status $(cat "$OUT_DIR/add-proxy.json" 2>/dev/null || true)"
  fi
  local add_code
  add_code=$(python3 -c 'import json, sys
try:
    print(json.load(open(sys.argv[1])).get("code", -1))
except Exception:
    print(-1)' "$OUT_DIR/add-proxy.json")
  if [ "$add_code" != "0" ]; then
    [ "$source_started" = "1" ] && stop_pid "$source_pid" || true
    die "添加拉流代理失败：code=$add_code $(cat "$OUT_DIR/add-proxy.json" 2>/dev/null || true)"
  fi
  log "拉流代理已创建：$RTSP_URL -> $STREAM_PATH"

  local proxy_id
  read -r proxy_id _ _ <<< "$(proxy_state)"
  [ "$proxy_id" != "-1" ] || { [ "$source_started" = "1" ] && stop_pid "$source_pid" || true; die "代理创建后未出现在 /api/proxy/pull/list 中"; }

  if ! wait_for_pull_ready; then
    [ "$source_started" = "1" ] && stop_pid "$source_pid" || true
    api_post_json_check "/api/proxy/pull/remove/$proxy_id" '{}' "$OUT_DIR/remove-$proxy_id.json" >/dev/null || true
    die "拉流未在 ${WAIT_TIMEOUT}s 内就绪（已清理代理 id=${proxy_id}）"
  fi

  cat > "$STATE_FILE" <<EOF
STREAM_PATH=$STREAM_PATH
RTSP_URL=$RTSP_URL
SOURCE_RTMP_URL=$SOURCE_RTMP_URL
SOURCE_PATH=$source_path
SOURCE_PID=$source_pid
SOURCE_STARTED=$source_started
PROXY_ID=$proxy_id
STARTED_AT_MS=$started_at
EOF
  chmod 600 "$STATE_FILE"
  log "start 完成，耗时 $(( $(now_ms) - started_at ))ms；状态文件 $STATE_FILE"
  cmd_status
}

cmd_status() {
  need_cmd curl
  need_cmd python3
  if ! api_ok; then
    warn "API 不可达：$API_BASE/api/stream/list"
    return 1
  fi
  local id status url present subs res codec source_path
  source_path=$(rtmp_stream_path "$SOURCE_RTMP_URL")
  read -r id status url <<< "$(proxy_state)"
  read -r present subs res codec <<< "$(stream_state "$STREAM_PATH")"

  log "API: $API_BASE 可达"
  log "RTSP 源: $RTSP_URL"
  if [ "$id" = "-1" ]; then
    log "拉流代理: 未找到（streamPath=${STREAM_PATH}）"
  else
    log "拉流代理: id=$id status=$status($(status_name "$status")) url=$url"
  fi
  log "推流源流: $source_path -> $(stream_state "$source_path" | awk '{print "present="$1}')"
  log "拉流结果: $STREAM_PATH present=$present subscribers=$subs video=$res codec=$codec"
  if [ -f "$STATE_FILE" ]; then
    # shellcheck disable=SC1090
    . "$STATE_FILE"
    if [ -n "${SOURCE_PID:-}" ] && kill -0 "$SOURCE_PID" 2>/dev/null; then
      log "脚本管理的推流进程: PID=$SOURCE_PID 存活（SOURCE_STARTED=${SOURCE_STARTED}）"
    elif [ -n "${SOURCE_PID:-}" ]; then
      log "脚本管理的推流进程: PID=$SOURCE_PID 已退出"
    fi
  else
    log "状态文件: 不存在（当前 shell 未执行过 start，或已执行 stop）"
  fi
  return 0
}

write_play_runner() {
  cat > "$RUNNER_PATH" <<'JS'
// 由 rtsp-pull.sh 在运行时生成（不提交）。Playwright 注入 play-collect.js，
// 打开播放页采样 WebRTC getStats，输出 summary/samples。
const fs = require('fs');
const { chromium } = require('playwright');

const COLLECTOR = fs.readFileSync(process.env.COLLECTOR_PATH, 'utf8');
const PLAY_URL = process.env.PLAY_URL;
const RUN_SECONDS = Number(process.env.RUN_SECONDS || 60);
const LOG_PATH = process.env.CONSOLE_LOG;
const SAMPLES_PATH = process.env.SAMPLES_PATH;
const SUMMARY_PATH = process.env.SUMMARY_PATH;
const CHANNEL = process.env.PLAYWRIGHT_CHANNEL || 'chrome';

(async () => {
  fs.writeFileSync(LOG_PATH, '');
  const log = (line) => {
    const text = `[runner] ${line}`;
    console.log(text);
    fs.appendFileSync(LOG_PATH, text + '\n');
  };

  const browser = await chromium.launch({
    channel: CHANNEL,
    headless: true,
    args: ['--autoplay-policy=no-user-gesture-required', '--disable-background-timer-throttling'],
  });
  try {
    const context = await browser.newContext();
    await context.addInitScript({ content: COLLECTOR });
    const page = await context.newPage();
    page.on('console', (msg) => fs.appendFileSync(LOG_PATH, msg.text() + '\n'));
    page.on('pageerror', (err) => log(`pageerror: ${err.message}`));

    log(`goto ${PLAY_URL}`);
    await page.goto(PLAY_URL, { waitUntil: 'domcontentloaded', timeout: 20000 });
    await page.waitForFunction(() => {
      const collector = window.__webrtc2kCollector;
      const video = document.querySelector('video');
      return !!collector && collector.state.handles.length > 0 && !!video && video.videoWidth > 0;
    }, null, { timeout: 30000 });

    const info = await page.evaluate(() => {
      const video = document.querySelector('video');
      const pc = window.__webrtc2kCollector.state.handles[0].pc;
      return {
        width: video.videoWidth,
        height: video.videoHeight,
        connectionState: pc.connectionState,
      };
    });
    log(`media ${JSON.stringify(info)}`);
    log(`sampling ${RUN_SECONDS}s`);
    await page.waitForTimeout(RUN_SECONDS * 1000);

    const summary = await page.evaluate(() => window.__webrtc2kCollector.stop('runner finished'));
    const records = await page.evaluate(() => window.__webrtc2kCollector.getRecords());
    fs.writeFileSync(SUMMARY_PATH, JSON.stringify(summary, null, 2));
    fs.writeFileSync(SAMPLES_PATH, records.map((r) => JSON.stringify(r)).join('\n') + '\n');
    log(`summary ${JSON.stringify(summary)}`);
    log(`samples=${records.length} -> ${SAMPLES_PATH}`);
  } finally {
    await browser.close();
  }
})().catch((error) => {
  console.error(`[runner] FAILED: ${error && error.stack ? error.stack : error}`);
  process.exit(1);
});
JS
}

cmd_play() {
  need_cmd curl
  need_cmd python3
  need_cmd node
  require_api

  local present
  read -r present _ <<< "$(stream_state "$STREAM_PATH")"
  if [ "$present" != "1" ]; then
    die "拉流 $STREAM_PATH 不存在。请先执行 ./rtsp-pull.sh start，并等待 status 显示 present=1。"
  fi
  if [ ! -f "$SCRIPT_DIR/play-collect.js" ]; then
    die "缺少采集脚本 $SCRIPT_DIR/play-collect.js"
  fi
  if [ ! -d "$PLAYWRIGHT_DIR/node_modules/playwright" ]; then
    die "Playwright 不可用：未找到 $PLAYWRIGHT_DIR/node_modules/playwright。安装示例：cd $PLAYWRIGHT_DIR && npm init -y && npm i playwright && npx playwright install chrome；也可用 PLAYWRIGHT_DIR 指向已有安装。"
  fi

  mkdir -p "$OUT_DIR"
  write_play_runner
  log "Playwright 播放 ${PLAY_SECONDS}s：${PLAY_URL_BASE}?streamPath=${STREAM_PATH}"
  PLAY_URL="${PLAY_URL_BASE}?streamPath=${STREAM_PATH}" \
  RUN_SECONDS="$PLAY_SECONDS" \
  COLLECTOR_PATH="$SCRIPT_DIR/play-collect.js" \
  SUMMARY_PATH="$SUMMARY_OUT" \
  SAMPLES_PATH="$SAMPLES_OUT" \
  CONSOLE_LOG="$CONSOLE_LOG" \
  PLAYWRIGHT_CHANNEL="$PLAYWRIGHT_CHANNEL" \
  NODE_PATH="$PLAYWRIGHT_DIR/node_modules" \
    node "$RUNNER_PATH" || die "Playwright 播放/采集失败（日志 ${CONSOLE_LOG}）"

  [ -f "$SUMMARY_OUT" ] || die "未生成 summary：$SUMMARY_OUT"

  local metrics avg conn
  metrics=$(python3 - "$SUMMARY_OUT" <<'PY'
import json, sys
summary = json.load(open(sys.argv[1]))
pcs = summary.get("peerConnections") or []
pc = pcs[0] if pcs else {}
for key in ("connectionState", "samples", "spanSec", "decodedFrames", "avgFps",
            "sampleFpsMean", "sampleFpsMin", "framesDropped", "nackCount",
            "pliCount", "jitterBufferDelayAvg", "lastRtt"):
    print("%s=%s" % (key, pc.get(key)))
PY
)
  log "WebRTC 采集指标（${SUMMARY_OUT}）:"
  while IFS= read -r line; do
    log "  $line"
  done <<< "$metrics"

  avg=$(python3 -c 'import sys
for line in sys.stdin:
    if line.startswith("avgFps="):
        print(line.strip().split("=", 1)[1])' <<< "$metrics")
  conn=$(python3 -c 'import sys
for line in sys.stdin:
    if line.startswith("connectionState="):
        print(line.strip().split("=", 1)[1])' <<< "$metrics")

  if [ -z "$avg" ] || [ "$avg" = "None" ]; then
    die "未取得 avgFps（连接未建立或采样不足），完整 summary 见 $SUMMARY_OUT"
  fi
  if [ "$conn" != "connected" ]; then
    log "ASSERT FAIL: connectionState=${conn}（期望 connected）"
    exit 1
  fi
  if python3 -c 'import sys; sys.exit(0 if float(sys.argv[1]) >= float(sys.argv[2]) else 1)' "$avg" "$MIN_AVG_FPS"; then
    log "ASSERT PASS: avgFps=$avg >= $MIN_AVG_FPS"
    return 0
  fi
  log "ASSERT FAIL: avgFps=$avg < ${MIN_AVG_FPS}（指标已如实记录）"
  return 1
}

cmd_reconnect() {
  need_cmd curl
  need_cmd python3
  require_api
  load_state
  [ "${START_SOURCE:-0}" = "1" ] || die "reconnect 要求 START_SOURCE=1（由本脚本管理源进程）"
  [ -n "${SOURCE_STARTED:-0}" ] && [ "${SOURCE_STARTED}" = "1" ] || die "state 记录显示源不是本脚本启动的，无法安全重启；请手工重启源并计时"
  pid_matches "${SOURCE_PID:-}" "${SOURCE_RTMP_URL:-$SOURCE_RTMP_URL}" || die "推流进程 PID=${SOURCE_PID:-} 不存在或命令行不匹配，拒绝继续"

  mkdir -p "$OUT_DIR"
  local source_path kill_ms
  source_path="${SOURCE_PATH:-$(rtmp_stream_path "$SOURCE_RTMP_URL")}"
  kill_ms=$(now_ms)
  log "杀掉源进程 PID=${SOURCE_PID}（${SOURCE_RTMP_URL}）"
  stop_pid "$SOURCE_PID"

  # 观测断流：源流消失耗时 + 拉流是否还有数据（live/rtsp2k 的 publisher 可能不消失）
  local deadline=$((SECONDS + 10)) source_gone=0 present
  while [ "$SECONDS" -lt "$deadline" ]; do
    read -r present _ <<< "$(stream_state "$source_path")"
    if [ "$present" = "0" ]; then
      source_gone=1
      break
    fi
    sleep 0.2
  done
  local source_gone_ms pulled_present pulled_bps
  source_gone_ms=$(( $(now_ms) - kill_ms ))
  read -r pulled_present _ <<< "$(stream_state "$STREAM_PATH")"
  pulled_bps=$(stream_bps "$STREAM_PATH")
  log "断流观测：源流消失=${source_gone}（${source_gone_ms}ms 后）$STREAM_PATH present=$pulled_present videoBps=$pulled_bps"

  # 补足 RECONNECT_WAIT（自 kill 起算）
  local elapsed_ms wait_ms
  elapsed_ms=$(( $(now_ms) - kill_ms ))
  wait_ms=$(python3 -c 'import sys; print(max(0, int(float(sys.argv[1]) * 1000) - int(sys.argv[2])))' "$RECONNECT_WAIT" "$elapsed_ms")
  if [ "$wait_ms" -gt 0 ]; then
    sleep "$(python3 -c 'import sys; print(float(sys.argv[1]) / 1000.0)' "$wait_ms")"
  fi

  RTMP_URL="$SOURCE_RTMP_URL" DURATION=0 PRESET="$PRESET" \
    nohup "$SCRIPT_DIR/push-2k.sh" >"$PUSH_LOG" 2>&1 &
  local new_pid=$!
  local restart_ms
  restart_ms=$(now_ms)
  log "源已重启：PID=${new_pid}，开始计时（断流总时长 $(( restart_ms - kill_ms ))ms）"

  deadline=$((SECONDS + WAIT_TIMEOUT))
  local id status url subs res codec bps source_back_ms="" source_recover_sec="null"
  while [ "$SECONDS" -lt "$deadline" ]; do
    read -r id status url <<< "$(proxy_state)"
    read -r present subs res codec <<< "$(stream_state "$STREAM_PATH")"
    bps=$(stream_bps "$STREAM_PATH")
    if [ "$source_back_ms" = "" ]; then
      local present_src
      read -r present_src _ <<< "$(stream_state "$source_path")"
      if [ "$present_src" = "1" ]; then
        source_back_ms=$(now_ms)
        source_recover_sec=$(python3 -c 'print("%.2f" % ((int(__import__("sys").argv[1]) - int(__import__("sys").argv[2])) / 1000.0))' "$source_back_ms" "$restart_ms")
        log "源流已重新出现：${source_recover_sec}s（自重启起）"
      fi
    fi
    if [ "$present" = "1" ] && [ "$source_back_ms" != "" ] && [ "$id" != "-1" ] && { [ "$status" = "1" ] || [ "$status" = "2" ]; }; then
      local available_ms stream_available_sec
      available_ms=$(( $(now_ms) - restart_ms ))
      stream_available_sec=$(python3 -c 'print("%.2f" % (float(__import__("sys").argv[1]) / 1000.0))' "$available_ms")
      log "拉流可用：耗时 ${stream_available_sec}s（proxyId=$id status=$status($(status_name "$status")) stream=$STREAM_PATH video=$res videoBps=$bps subscribers=${subs}）"
      python3 - "$OUT_DIR/reconnect-result.json" "${SOURCE_PID}" "$new_pid" "$RECONNECT_WAIT" \
        "$source_gone_ms" "$(( restart_ms - kill_ms ))" "${source_recover_sec}" "$stream_available_sec" \
        "$id" "$status" "$STREAM_PATH" "$pulled_present" "$pulled_bps" <<'PY'
import json, sys
(path, old_pid, new_pid, wait_s, source_gone_ms, interrupted_ms, source_recover_s,
 stream_available_s, proxy_id, proxy_status, stream, outage_present, outage_bps) = sys.argv[1:14]
data = {
    "killedPid": int(old_pid),
    "restartedPid": int(new_pid),
    "reconnectWaitSec": float(wait_s),
    "sourceGoneAfterKillMs": int(source_gone_ms),
    "interruptedMs": int(interrupted_ms),
    "sourceRecoverSec": json.loads(source_recover_s),
    "streamAvailableSec": float(stream_available_s),
    "note": "live/rtsp2k publisher 在源断流期间通常不消失；available 以源流恢复+代理在线为准，画面连续性需另行复播验证",
    "proxyId": int(proxy_id),
    "proxyStatus": int(proxy_status),
    "stream": stream,
    "pulledStreamPresentDuringOutage": int(outage_present),
    "pulledVideoBpsObservedDuringOutage": int(outage_bps),
}
json.dump(data, open(path, "w"), ensure_ascii=False, indent=2)
print(json.dumps(data, ensure_ascii=False))
PY
      SOURCE_PID="$new_pid"
      sed -i.bak "s/^SOURCE_PID=.*/SOURCE_PID=$new_pid/" "$STATE_FILE" 2>/dev/null || true
      rm -f "$STATE_FILE.bak"
      return 0
    fi
    sleep "$POLL_INTERVAL"
  done

  read -r id status url <<< "$(proxy_state)"
  read -r present subs res codec <<< "$(stream_state "$STREAM_PATH")"
  warn "恢复超时（${WAIT_TIMEOUT}s）：proxyId=$id status=$status streamPresent=$present videoBps=$(stream_bps "$STREAM_PATH")"
  return 1
}

cmd_stop() {
  need_cmd curl
  need_cmd python3
  local ids id code
  if api_ok; then
    ids=$(proxy_ids_all)
    if [ -n "$ids" ]; then
      for id in $ids; do
        code=$(api_post_json_check "/api/proxy/pull/remove/$id" '{}' "$OUT_DIR/remove-$id.json")
        if [ "$code" = "200" ]; then
          log "已删除拉流代理 id=$id"
        else
          warn "删除拉流代理 id=$id 失败：HTTP $code $(cat "$OUT_DIR/remove-$id.json" 2>/dev/null || true)"
        fi
      done
    else
      log "没有需要删除的拉流代理（streamPath=${STREAM_PATH}）"
    fi
  else
    warn "API 不可达，无法删除拉流代理；服务恢复后可手工 POST /api/proxy/pull/remove/<id>"
  fi

  if [ -f "$STATE_FILE" ]; then
    # shellcheck disable=SC1090
    . "$STATE_FILE"
    if [ "${SOURCE_STARTED:-0}" = "1" ]; then
      if pid_matches "${SOURCE_PID:-}" "${SOURCE_RTMP_URL:-$SOURCE_RTMP_URL}"; then
        log "停止本脚本启动的推流进程 PID=$SOURCE_PID"
        stop_pid "$SOURCE_PID"
      else
        log "推流进程 PID=${SOURCE_PID:-} 已不存在，无需停止"
      fi
    else
      log "推流源不是本脚本启动的，保持运行"
    fi
  else
    log "状态文件不存在：未发现本脚本启动的推流进程"
  fi

  # 清理本脚本可能遗留的 Playwright runner（只按本脚本生成的绝对路径匹配）
  local runner_pids
  runner_pids=$(ps -axo pid=,command= 2>/dev/null | python3 -c '
import os, sys
needle = sys.argv[1]
self_pid = os.getpid()
for line in sys.stdin:
    pid, _, cmd = line.strip().partition(" ")
    if pid and int(pid) != self_pid and needle in cmd:
        print(pid)
' "$RUNNER_PATH" || true)
  if [ -n "$runner_pids" ]; then
    for id in $runner_pids; do
      log "清理遗留 Playwright runner PID=$id"
      stop_pid "$id" || true
    done
  fi

  rm -f "$STATE_FILE" "$RUNNER_PATH"
  log "stop 完成（服务端未由本脚本启动，保持运行）"
}

usage() {
  cat <<'EOF'
用法: ./rtsp-pull.sh <start|status|play|reconnect|stop>

  start      可选启动 2K RTMP 源，创建 RTSP 拉流代理并等待 Online/Pulling
  status     打印 API、源流、代理与拉流状态
  play       Playwright 播放 PLAY_SECONDS 秒，断言 avgFps >= MIN_AVG_FPS
  reconnect  杀掉并重启源，测量拉流恢复耗时
  stop       删除代理并停止本脚本启动的源/播放器

环境变量见脚本头部注释；常用：API_BASE RTSP_URL STREAM_PATH SOURCE_RTMP_URL
START_SOURCE PLAY_SECONDS RECONNECT_WAIT PLAYWRIGHT_DIR OUT_DIR MIN_AVG_FPS
EOF
}

main() {
  case "${1:-}" in
    start)     cmd_start ;;
    status)    cmd_status ;;
    play)      cmd_play ;;
    reconnect) cmd_reconnect ;;
    stop)      cmd_stop ;;
    -h|--help|help|"") usage; [ -n "${1:-}" ] || exit 1 ;;
    *)         usage; exit 1 ;;
  esac
}

main "$@"
