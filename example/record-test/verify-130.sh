#!/usr/bin/env bash
# verify-130.sh —— 130 端到端验证一键脚本
#
# 验证两项已合入 develop 的修复:
#   1. Storage upload ctx 解耦   docs/superpowers/plans/2026-05-15-storage-upload-detach-recorder-context.md
#   2. Trailer 重写/限速         docs/superpowers/plans/2026-05-16-trailer-rewrite-io-reduction.md
#
# 做的事: 部署镜像到 130 → API 加 N 路 pull proxy → 启录制 → 录 5min →
#         同时停录制(采集磁盘写峰值) → 等上传 → 收 pending/cancel 指标 → 出报告 → 清理 proxy
#
# 用法:
#   ./verify-130.sh run <image-tag>     部署 + 全套验证 (Run A: 默认配置)
#   ./verify-130.sh deploy <image-tag>  仅部署
#   ./verify-130.sh run-test            跳过部署, 直接对当前 130 服务跑验证
#   ./verify-130.sh remove-proxies      清理本脚本加的 pull proxy
#   子命令: clean-pending | add-proxies | start-record | stop-and-sample | metrics
#
# 前置: 镜像须已 build 并推到 SWR (./build_docker.sh <tag>); 本机能 ssh 到 130; 装了 jq。
#
# Run B (限速验证): 在 130 上把 monibuca config 的 storage.trailerwriteratembps 设为 300,
#   重启容器, 再 `./verify-130.sh run-test` —— 对比报告里的 disk 写峰值。
#
# 所有参数可用环境变量覆盖, 见下方默认值。

set -u

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ---- 可配置项 (环境变量覆盖) ----
SSH_HOST="${SSH_HOST:-root@172.16.12.130}"
COMPOSE_DIR="${COMPOSE_DIR:-/home/project/xde-uat/media-docker-compose}"
COMPOSE_FILE="${COMPOSE_FILE:-docker-compose-xde-monibuca.yml}"
CONTAINER="${CONTAINER:-xde-monibuca}"
IMAGE_REPO="${IMAGE_REPO:-swr.cn-east-3.myhuaweicloud.com/intetech/monibuca}"
API_BASE="${API_BASE:-http://localhost:7080}"   # 130 上 monibuca http listenaddr (network_mode host)
PENDING_DIR="${PENDING_DIR:-/monibuca/pending_uploads}"
STREAM_COUNT="${STREAM_COUNT:-31}"
STREAM_PREFIX="${STREAM_PREFIX:-verify130/cam}"
PROXY_TYPE="${PROXY_TYPE:-rtsp}"
RECORD_SECONDS="${RECORD_SECONDS:-300}"
WARMUP="${WARMUP:-30}"            # 加完 proxy 后等流就绪的秒数
UPLOAD_WAIT="${UPLOAD_WAIT:-180}" # 停录制后等 trailer 重写 + 上传的秒数
SAMPLE_SECS="${SAMPLE_SECS:-45}"  # 停录制窗口磁盘采样次数 (docker stats 每次约 1.5-2s)
CONFIG_YAML="${CONFIG_YAML:-$SCRIPT_DIR/config.yaml}"
CAMERAS_FILE="${CAMERAS_FILE:-}"  # 留空则从 CONFIG_YAML 抓 rtsp:// 地址
TOKEN="${TOKEN:-}"                # 若 130 开了 admin 鉴权, 填 JWT
ASSUME_YES="${ASSUME_YES:-0}"

REPORT_DIR="$SCRIPT_DIR/reports"
TS="$(date +%Y%m%dT%H%M%S)"
REPORT="$REPORT_DIR/verify-130-$TS.md"

# baseline (修复前实测, 见两份 plan)
BASE_PENDING=31
BASE_CANCEL=31
BASE_DISK_MBPS=1126

AUTH_HDR=""
[ -n "$TOKEN" ] && AUTH_HDR="-H 'Authorization: Bearer $TOKEN'"

CAM_URLS=()
STREAMS=()

# ---- 工具函数 ----
# 进度信息一律走 stderr —— cmd_stop_and_sample / cmd_metrics 的返回值靠 stdout 捕获,
# 不能被日志污染。
log()  { printf '\033[36m[%s]\033[0m %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
ok()   { printf '\033[32m  ✓ %s\033[0m\n' "$*" >&2; }
warn() { printf '\033[33m  ! %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[31m[FATAL] %s\033[0m\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少依赖: $1"; }

ssh_do() { ssh -o ConnectTimeout=10 "$SSH_HOST" "$1"; }

# 在 130 上发起 API GET, stdout 即响应体
api_get() {
	ssh_do "curl -s --max-time 20 $AUTH_HDR '$API_BASE$1'"
}

# 在 130 上发起 API POST, body 经 stdin 传入 (避开 & ? \" 等转义地狱)
api_post() {
	local path=$1 body=$2
	printf '%s' "$body" | ssh_do "curl -s --max-time 25 -X POST '$API_BASE$path' -H 'Content-Type: application/json' $AUTH_HDR --data-binary @-"
}

confirm() {
	[ "$ASSUME_YES" = "1" ] && return 0
	printf '%s [y/N] ' "$1"
	local a; read -r a
	[ "$a" = "y" ] || [ "$a" = "Y" ]
}

# 把 CAM_URLS / STREAMS 两个数组填好
load_cameras() {
	local src="$CONFIG_YAML"
	[ -n "$CAMERAS_FILE" ] && src="$CAMERAS_FILE"
	[ -f "$src" ] || die "找不到摄像头来源文件: $src"
	CAM_URLS=()
	local u
	while IFS= read -r u; do
		[ -n "$u" ] && CAM_URLS+=("$u")
	done < <(grep -vE '^[[:space:]]*#' "$src" | grep -oE 'rtsp://[^[:space:]]+' | head -n "$STREAM_COUNT")
	[ "${#CAM_URLS[@]}" -gt 0 ] || die "$src 里没抓到 rtsp:// 地址"
	[ "${#CAM_URLS[@]}" -ge "$STREAM_COUNT" ] || \
		warn "只找到 ${#CAM_URLS[@]} 路摄像头, 少于请求的 $STREAM_COUNT"
	STREAMS=()
	local i
	for i in $(seq 1 "${#CAM_URLS[@]}"); do
		STREAMS+=("${STREAM_PREFIX}${i}")
	done
	log "摄像头来源: $src —— 取 ${#CAM_URLS[@]} 路"
}

# ---- 子命令 ----

cmd_deploy() {
	local tag="${1:-}"
	[ -n "$tag" ] || die "deploy 需要镜像 tag: ./verify-130.sh deploy <tag>"
	log "部署 $IMAGE_REPO:$tag 到 $SSH_HOST"
	ssh_do "cd '$COMPOSE_DIR' && \
		sed -i 's|$IMAGE_REPO:.*|$IMAGE_REPO:$tag|' '$COMPOSE_FILE' && \
		docker compose -f '$COMPOSE_FILE' pull && \
		docker compose -f '$COMPOSE_FILE' up -d --force-recreate" \
		|| die "部署失败"
	ok "容器已重建, 等待 API 就绪..."
	wait_api_ready
}

wait_api_ready() {
	local i
	for i in $(seq 1 30); do
		# 用 /api/sysinfo 判就绪: 返回 {"code":0,...}; 404 页无 "code" 字段, 不会误判
		if api_get "/api/sysinfo" 2>/dev/null | grep -q '"code"'; then
			ok "API 就绪"
			return 0
		fi
		sleep 3
	done
	die "等 90s API 仍未就绪 (检查容器日志: ssh $SSH_HOST docker logs $CONTAINER)"
}

# 返回测前 pending 数, 并清空 pending 目录
cmd_clean_pending() {
	local before
	before=$(ssh_do "docker exec '$CONTAINER' sh -c 'ls $PENDING_DIR/*.mp4 2>/dev/null | wc -l'" | tr -d ' \r')
	log "测前 pending_uploads 残留: ${before:-0} 个 —— 清空以建立干净基线"
	ssh_do "docker exec '$CONTAINER' sh -c 'rm -f $PENDING_DIR/*.mp4 2>/dev/null; true'"
	ok "pending_uploads 已清空"
}

cmd_add_proxies() {
	load_cameras
	log "新增 ${#CAM_URLS[@]} 路 pull proxy (type=$PROXY_TYPE)"
	local i url sp body resp
	for i in $(seq 0 $(( ${#CAM_URLS[@]} - 1 ))); do
		url="${CAM_URLS[$i]}"
		sp="${STREAMS[$i]}"
		body=$(printf '{"name":"verify130-cam%s","type":"%s","pullURL":"%s","pullOnStart":true,"streamPath":"%s","audio":true}' \
			"$((i+1))" "$PROXY_TYPE" "$url" "$sp")
		resp=$(api_post "/api/proxy/pull/add" "$body")
		case "$resp" in
			*'"code":0'*|*'success'*|'') ;;
			*) warn "cam$((i+1)) add 响应: $resp" ;;
		esac
	done
	ok "pull proxy 提交完毕, 等 ${WARMUP}s 让流就绪"
	sleep "$WARMUP"
	local online
	online=$(api_get "/api/proxy/pull/list" | jq -r '[.data[]?|select(.name|startswith("verify130-"))]|length' 2>/dev/null || echo "?")
	log "已登记 verify130 proxy 数: $online"
}

cmd_start_record() {
	load_cameras
	log "对 ${#STREAMS[@]} 路启动录制 (fragment=0 整段不分片, filePath 按 streamPath 区分)"
	local sp resp
	for sp in "${STREAMS[@]}"; do
		# filePath 必须每路唯一 —— StartRecord 默认 filePath="." (api.go:712),
		# 多路共用同一 filePath 会触发 ErrRecordExists. 用 streamPath 当 filePath.
		resp=$(api_post "/mp4/api/start/$sp" "$(printf '{"streamPath":"%s","filePath":"%s","fragment":"0"}' "$sp" "$sp")")
		case "$resp" in
			*'"code":0'*|'') ;;
			*) warn "start $sp 响应: $resp" ;;
		esac
	done
	ok "录制已启动"
}

# 启动磁盘采样 + 同时停所有录制, 把峰值写到 stdout (MB/s)
cmd_stop_and_sample() {
	load_cameras
	local stream_list="${STREAMS[*]}"
	local sfile; sfile=$(mktemp)
	log "启动磁盘写采样 (${SAMPLE_SECS} 次, docker stats BlockIO)"
	# 后台采样: 每次 docker stats --no-stream 约耗 1.5-2s, 循环自然配速
	( ssh_do "for i in \$(seq 1 $SAMPLE_SECS); do echo \"\$(date +%s) \$(docker stats --no-stream --format '{{.BlockIO}}' '$CONTAINER' 2>/dev/null)\"; done" >"$sfile" 2>/dev/null ) &
	local spid=$!
	sleep 3   # 让采样器取到停录前的基线读数
	log "并发停止 ${#STREAMS[@]} 路录制 (触发 trailer 重写 burst)"
	ssh_do "for sp in $stream_list; do curl -s --max-time 25 -X POST '$API_BASE/mp4/api/stop/'\$sp -H 'Content-Type: application/json' $AUTH_HDR --data '{}' >/dev/null 2>&1 & done; wait"
	ok "停止指令已全部下发"
	wait "$spid" 2>/dev/null || true
	# 解析峰值: 行格式 = "epoch read / write"
	local peak_bps
	peak_bps=$(awk '
		function tob(s,  n,u,m){ n=s; sub(/[A-Za-z]+$/,"",n); u=s; sub(/^[0-9.]+/,"",u);
			m=1; if(u=="kB")m=1e3; else if(u=="MB")m=1e6; else if(u=="GB")m=1e9; else if(u=="TB")m=1e12;
			return n*m }
		{ if($4==""||$4=="--")next; b=tob($4);
		  if(pe!=""){dt=$1-pe; if(dt>0){r=(b-pb)/dt; if(r>pk)pk=r}}
		  pe=$1; pb=b }
		END{ printf "%.0f", pk+0 }' "$sfile")
	cp "$sfile" "$REPORT_DIR/.disk-samples-$TS.txt" 2>/dev/null || true
	rm -f "$sfile"
	awk "BEGIN{printf \"%.0f\", ${peak_bps:-0}/1000000}"
}

# stdout 三行: pending / cancel / savedfailed
cmd_metrics() {
	local since="$1"
	local pending cancel saved
	pending=$(ssh_do "docker exec '$CONTAINER' sh -c 'ls $PENDING_DIR/*.mp4 2>/dev/null | wc -l'" | tr -d ' \r')
	cancel=$(ssh_do "docker logs '$CONTAINER' --since '$since' 2>&1 | grep -ciE 'RequestCanceled|context canceled' || true" | tr -d ' \r')
	saved=$(ssh_do "docker logs '$CONTAINER' --since '$since' 2>&1 | grep -c 'saved failed upload for retry' || true" | tr -d ' \r')
	echo "${pending:-0}"
	echo "${cancel:-0}"
	echo "${saved:-0}"
}

cmd_remove_proxies() {
	log "清理 verify130 pull proxy"
	local ids
	ids=$(api_get "/api/proxy/pull/list" | jq -r '.data[]?|select(.name|startswith("verify130-"))|(.ID//.id)' 2>/dev/null)
	if [ -z "$ids" ]; then
		warn "没找到 verify130 proxy (可能已清理, 或鉴权/字段名不符)"
		return 0
	fi
	local id n=0
	for id in $ids; do
		api_post "/api/proxy/pull/remove/$id" "{}" >/dev/null 2>&1 || \
			ssh_do "curl -s -X POST '$API_BASE/api/proxy/pull/remove/$id' $AUTH_HDR >/dev/null" || true
		n=$((n+1))
	done
	ok "已请求移除 $n 个 proxy"
}

# ---- 编排 ----
run_test() {
	need ssh; need jq; need awk
	mkdir -p "$REPORT_DIR"
	load_cameras

	log "=== 验证开始 (流数=${#STREAMS[@]}, 录制 ${RECORD_SECONDS}s) ==="
	if ! confirm "将对 $SSH_HOST 加 ${#STREAMS[@]} 路流并录制 ${RECORD_SECONDS}s, 继续?"; then
		die "已取消"
	fi

	local start_epoch; start_epoch=$(date +%s)
	cmd_clean_pending
	cmd_add_proxies
	cmd_start_record

	log "录制中, 等待 ${RECORD_SECONDS}s ..."
	sleep "$RECORD_SECONDS"

	local disk_peak; disk_peak=$(cmd_stop_and_sample)
	log "停录制窗口磁盘写峰值 (docker stats 2s 平均): ${disk_peak} MB/s"

	log "等待 trailer 重写 + 上传 ${UPLOAD_WAIT}s ..."
	sleep "$UPLOAD_WAIT"

	local m; m=$(cmd_metrics "$start_epoch")
	local pending cancel saved
	pending=$(echo "$m" | sed -n 1p)
	cancel=$(echo "$m" | sed -n 2p)
	saved=$(echo "$m" | sed -n 3p)

	cmd_remove_proxies

	# ---- 判定 ----
	local verdict_ctx="PASS" verdict_overall="PASS"
	[ "${pending:-1}" = "0" ] && [ "${cancel:-1}" = "0" ] || { verdict_ctx="FAIL"; verdict_overall="FAIL"; }

	write_report "$start_epoch" "$pending" "$cancel" "$saved" "$disk_peak" "$verdict_ctx"

	echo
	log "=== 结果 ==="
	printf '  pending_uploads : %s (baseline %s, 期望 0)\n' "$pending" "$BASE_PENDING"
	printf '  cancel 事件      : %s (baseline %s, 期望 0)\n' "$cancel" "$BASE_CANCEL"
	printf '  saved-failed 补传: %s\n' "$saved"
	printf '  磁盘写峰值       : %s MB/s (baseline %s, docker stats 粗采样)\n' "$disk_peak" "$BASE_DISK_MBPS"
	printf '  ctx 修复判定     : %s\n' "$verdict_ctx"
	echo
	log "报告: $REPORT"
	if [ "$verdict_overall" = "FAIL" ]; then
		warn "验证未通过 —— 看报告与容器日志定位"
		return 1
	fi
	ok "ctx 修复验证通过"
}

write_report() {
	local start_epoch=$1 pending=$2 cancel=$3 saved=$4 disk=$5 vctx=$6
	{
		echo "# 130 端到端验证报告 — $TS"
		echo
		echo "- 目标: $SSH_HOST  容器: \`$CONTAINER\`"
		echo "- 流数: ${#STREAMS[@]}   录制时长: ${RECORD_SECONDS}s"
		echo "- 验证: storage upload ctx 解耦 + trailer 重写/限速 (develop)"
		echo
		echo "## 指标"
		echo
		echo "| 指标 | baseline(修复前) | 本次实测 | 期望 | 判定 |"
		echo "|---|---|---|---|---|"
		echo "| pending_uploads | $BASE_PENDING | $pending | 0 | $([ "$pending" = "0" ] && echo PASS || echo FAIL) |"
		echo "| cancel 事件 | $BASE_CANCEL | $cancel | 0 | $([ "$cancel" = "0" ] && echo PASS || echo FAIL) |"
		echo "| 磁盘写峰值(MB/s) | $BASE_DISK_MBPS | $disk | 显著下降 | 见说明 |"
		echo "| saved-failed 补传 | - | $saved | - | 信息项 |"
		echo
		echo "## 判定"
		echo
		echo "- **storage ctx 修复: $vctx** — pending=0 且无 cancel 事件即通过。"
		echo "- 磁盘峰值为 \`docker stats\` 约 2s 平均值, 会抹平亚秒级尖峰;"
		echo "  精确瞬时峰值请在 130 宿主机用 \`iostat -dx 1\` 在停录制窗口交叉验证。"
		echo "- Run B (限速): 把 130 config \`storage.trailerwriteratembps\` 设 300 重启后,"
		echo "  重跑 \`./verify-130.sh run-test\`, 对比本表磁盘峰值应 < 320 MB/s。"
		echo
		echo "## 原始磁盘采样"
		echo
		echo "见 \`reports/.disk-samples-$TS.txt\` (格式: epoch read / write, 累计值)。"
	} > "$REPORT"
}

usage() {
	sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
}

# ---- 入口 ----
main() {
	local cmd="${1:-}"
	case "$cmd" in
		deploy)
			need ssh
			[ -n "${2:-}" ] || die "deploy 需要镜像 tag: ./verify-130.sh deploy <tag>"
			confirm "将强制重建 130 容器 $CONTAINER 为 $IMAGE_REPO:$2 , 继续?" || die "已取消"
			cmd_deploy "$2"
			;;
		run)
			need ssh; need jq; need awk
			[ -n "${2:-}" ] || die "run 需要镜像 tag: ./verify-130.sh run <tag>"
			confirm "将部署 $IMAGE_REPO:$2 到 130(强制重建容器), 并跑 ${STREAM_COUNT} 路 ${RECORD_SECONDS}s 录制验证, 继续?" || die "已取消"
			ASSUME_YES=1   # 已统一确认, run_test 内不再二次询问
			cmd_deploy "$2"
			run_test
			;;
		run-test)      run_test ;;
		clean-pending) need ssh; cmd_clean_pending ;;
		add-proxies)   need ssh; need jq; cmd_add_proxies ;;
		start-record)  need ssh; cmd_start_record ;;
		stop-and-sample) need ssh; mkdir -p "$REPORT_DIR"; cmd_stop_and_sample ;;
		metrics)       need ssh; cmd_metrics "${2:-30m}" ;;
		remove-proxies) need ssh; need jq; cmd_remove_proxies ;;
		-h|--help|help|"") usage ;;
		*) die "未知命令: $cmd (用 --help 看用法)" ;;
	esac
}

main "$@"
