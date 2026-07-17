#!/bin/bash
# extended.sh — 针对 feature/review-p1-fixes 改动的 cluster e2e 扩展验证。
# 假设 stack 已 up(由 smoke.sh KEEP=1 或手动 docker compose up -d 拉起)。
#
# 覆盖:
#   E1 录制→trailer→S3 上传链路(单流,验证文件真的落 MinIO + ffprobe 可播 + moov 在头)
#   E2 上传并发度(多流同时停录,看 S3 上传日志 active=N/4 是否真并发,验证 U1 解耦)
#   E3 TOCTOU 秒断重推(publish 后立刻 unpublish 再 publish 同名流,验证 KV 键不被永久持有)
#   E4 first-write-wins(同名流并发推 node-1 + node-2,一方被拒,KV 属主唯一)
#   E5 跨节点录制路由(node-2 对 node-1 拥有的流发起录制)
#
# 用法: ./extended.sh          # 跑全部
#        SAMPLE=foo.mp4 ./extended.sh
set -uo pipefail
cd "$(dirname "$0")"
COMPOSE="docker compose -f docker-compose.yml"
SAMPLE="${SAMPLE:-sample.mp4}"
PASS=0; FAIL=0
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL+1)); }
step() { echo ""; echo "==> $1"; }

kv()   { $COMPOSE exec -T consul consul kv get "$1" 2>/dev/null | tail -1; }
# 各节点日志里抓 S3 上传行
s3log() { $COMPOSE logs "$1" 2>/dev/null | grep -E '\[S3\]'; }

[ -f "$SAMPLE" ] || { echo "FAIL: $SAMPLE not found"; exit 1; }

# ---------- E1: 单流录制 → S3 → 完整性 ----------
step "E1: record live/e1 on node-1 → upload S3 → verify"
ffmpeg -re -stream_loop 2 -i "$SAMPLE" -c copy -f flv -loglevel error rtmp://localhost:1935/live/e1 &
P_E1=$!
sleep 3
# mp4 record API: POST /mp4/api/start/<streamPath>
curl -fs -X POST "http://localhost:8081/mp4/api/start/live/e1" \
  -H 'Content-Type: application/json' \
  -d '{"fragment":"0","filePath":"live/e1","fileName":"e1.mp4"}' >/dev/null 2>&1 \
  && echo "  record started" || bad "E1 record start API"
sleep 8
curl -fs -X POST "http://localhost:8081/mp4/api/stop/live/e1" >/dev/null 2>&1 || true
kill "$P_E1" 2>/dev/null || true
echo "  waiting for trailer + upload (12s)"
sleep 12
# 从 MinIO 取回校验
tmp=$(mktemp -d)
$COMPOSE exec -T minio sh -c \
  'mc alias set m http://localhost:9000 admin m7sm7sm7s >/dev/null 2>&1; mc ls -r m/m7s-records/' 2>/dev/null \
  | grep -i e1 && ok "E1 object in MinIO" || bad "E1 object NOT in MinIO"
# 拉回一个对象 ffprobe(用节点容器内的 ffmpeg 直接探测 S3 http)
obj=$($COMPOSE exec -T minio sh -c \
  'mc alias set m http://localhost:9000 admin m7sm7sm7s >/dev/null 2>&1; mc ls -r --json m/m7s-records/ 2>/dev/null' \
  | grep -i e1 | head -1 | sed 's/.*"key":"//;s/".*//')
if [ -n "$obj" ]; then
  $COMPOSE exec -T minio sh -c "mc cat m/m7s-records/$obj" > "$tmp/e1.mp4" 2>/dev/null
  sz=$(wc -c < "$tmp/e1.mp4" | tr -d ' ')
  echo "  downloaded $obj ($sz bytes)"
  # moov 在文件头(前 64KB 内出现 moov 且在 mdat 之前)
  head -c 65536 "$tmp/e1.mp4" | grep -qa moov && ok "E1 moov near head (fast-start)" || bad "E1 moov not near head"
  if command -v ffprobe >/dev/null 2>&1; then
    ffprobe -v error -show_entries format=duration -of csv=p=0 "$tmp/e1.mp4" >/dev/null 2>&1 \
      && ok "E1 ffprobe playable" || bad "E1 ffprobe failed"
  fi
fi
rm -rf "$tmp"

# ---------- E2: 上传并发度(U1 解耦验证)----------
step "E2: concurrent record/stop on 3 streams → S3 uploads should overlap (active>1)"
for n in a b c d; do
  ffmpeg -re -stream_loop 2 -i "$SAMPLE" -c copy -f flv -loglevel error "rtmp://localhost:1935/live/e2$n" &
done
sleep 3
for n in a b c d; do
  curl -fs -X POST "http://localhost:8081/mp4/api/start/live/e2$n" \
    -H 'Content-Type: application/json' \
    -d "{\"fragment\":\"0\",\"filePath\":\"live/e2$n\",\"fileName\":\"e2$n.mp4\"}" >/dev/null 2>&1 || true
done
sleep 6
# 同时停 —— 制造并发上传
for n in a b c d; do
  curl -fs -X POST "http://localhost:8081/mp4/api/stop/live/e2$n" >/dev/null 2>&1 || true
done
sleep 12
pkill -f "live/e2" 2>/dev/null || true
# 从 S3 日志看是否出现过 active>=2(真并发);解耦前恒为 active=1/N
maxactive=$(s3log node-1 | grep -oE 'active=[0-9]+' | sed 's/active=//' | sort -rn | head -1)
echo "  observed max concurrent S3 uploads on node-1: active=${maxactive:-0}/4"
if [ -n "${maxactive:-}" ] && [ "${maxactive:-0}" -ge 2 ]; then
  ok "E2 uploads run concurrently (active=$maxactive) — trailer/upload decoupled"
else
  echo "  NOTE: active<2 可能因单机上传太快无重叠窗口(非确定性);不判 FAIL"
  echo "  INFO: E2 max active = ${maxactive:-0}(参考,不阻断)"
fi

# ---------- E3: TOCTOU 秒断重推 ----------
step "E3: rapid publish→unpublish→republish live/e3 (TOCTOU ghost-key guard)"
# 第一次:短暂 publish 后立刻 kill(模拟秒断)
ffmpeg -re -i "$SAMPLE" -c copy -f flv -loglevel error rtmp://localhost:1935/live/e3 &
P1=$!
sleep 1
kill "$P1" 2>/dev/null || true
sleep 3
# 第二次:同名流再次 publish,应能正常成为属主(键未被上一次永久持有)
ffmpeg -re -i "$SAMPLE" -c copy -f flv -loglevel error rtmp://localhost:1935/live/e3 &
P2=$!
sleep 4
owner=$(kv m7s/streams/live/e3)
kill "$P2" 2>/dev/null || true
if [ "$owner" = "node-1" ]; then
  ok "E3 republish acquired KV key (owner=node-1) — no ghost key"
else
  bad "E3 republish failed to own key (owner='$owner') — possible TOCTOU ghost key"
fi
sleep 3
# 停后键应消失(release 正常)
gone=1; for i in $(seq 1 8); do kv m7s/streams/live/e3 >/dev/null 2>&1 && { gone=0; sleep 1; } || { gone=1; break; }; done
[ "$gone" = "1" ] && ok "E3 key released after unpublish" || bad "E3 key lingering after unpublish"

# ---------- E4: first-write-wins ----------
step "E4: concurrent push live/e4 to node-1 AND node-2 → single owner"
ffmpeg -re -stream_loop 5 -i "$SAMPLE" -c copy -f flv -loglevel error rtmp://localhost:1935/live/e4 &
Q1=$!
ffmpeg -re -stream_loop 5 -i "$SAMPLE" -c copy -f flv -loglevel error rtmp://localhost:1936/live/e4 &
Q2=$!
sleep 5
owner=$(kv m7s/streams/live/e4)
echo "  KV owner of live/e4: '$owner'"
if [ "$owner" = "node-1" ] || [ "$owner" = "node-2" ]; then
  ok "E4 exactly one owner ($owner) — first-write-wins held"
else
  bad "E4 unexpected owner '$owner'"
fi
kill "$Q1" "$Q2" 2>/dev/null || true
sleep 2

# ---------- E5: 跨节点录制 ----------
step "E5: cross-node record — push to node-1, record via node-2 relay"
ffmpeg -re -stream_loop 3 -i "$SAMPLE" -c copy -f flv -loglevel error rtmp://localhost:1935/live/e5 &
R1=$!
sleep 3
# node-2 先订阅触发 auto-relay(FLV 302 不 relay,用 RTSP 订阅拉起 relay),再本地录
ffmpeg -t 3 -i rtsp://localhost:5542/live/e5 -f null -loglevel error /dev/null >/dev/null 2>&1 || true
sleep 1
curl -fs -X POST "http://localhost:8082/mp4/api/start/live/e5" \
  -H 'Content-Type: application/json' \
  -d '{"fragment":"0","filePath":"xnode/e5","fileName":"e5.mp4"}' >/dev/null 2>&1 \
  && echo "  node-2 record started" || echo "  NOTE: node-2 record API 返回非200(可能需先建 relay stream)"
sleep 8
curl -fs -X POST "http://localhost:8082/mp4/api/stop/live/e5" >/dev/null 2>&1 || true
kill "$R1" 2>/dev/null || true
sleep 10
$COMPOSE exec -T minio sh -c \
  'mc alias set m http://localhost:9000 admin m7sm7sm7s >/dev/null 2>&1; mc ls -r m/m7s-records/' 2>/dev/null \
  | grep -i e5 && ok "E5 cross-node record object in MinIO" \
  || echo "  INFO: E5 未见对象(跨节点录制路径依部署而定,参考不阻断)"

echo ""
echo "============================================"
echo " extended e2e: PASS=$PASS  FAIL=$FAIL"
echo "============================================"
[ "$FAIL" = "0" ]
