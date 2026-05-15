# 2K/4K 拉流与录制测试 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 `example/record-test/` 上加 "探测 + 分组 + 报告" 三件套，使可以定向跑 2K / 4K 摄像头组的并发录制测试，并出可对比的报告。

**Architecture:** 纯 bash 工具链（无 monibuca 源码改动）。`lib/` 下放纯函数模块（classify / probe / verify / sampler）+ 各自 bash 单测，顶层 `probe_streams.sh` + `test_high_res.sh` 拼装。摄像头分辨率信息固化到 `streams_meta.json` 后入 git。

**Tech Stack:** bash 4+, ffprobe, jq, awk。**不引入 yq / bats / python**。所有单测用 PATH 注入 mock ffprobe 实现，无外部依赖。

**Spec 参考:** `docs/superpowers/specs/2026-05-13-2k4k-record-test-design.md`

---

## 文件结构

```
example/record-test/
├── lib/
│   ├── _assert.sh          [新] 测试断言 helpers（assert_eq / assert_match / mock 安装）
│   ├── classify.sh         [新] 纯函数：W/H → resolution_class
│   ├── probe.sh            [新] ffprobe 包装：URL → W H FPS CODEC BR
│   ├── sampler.sh          [新] 后台采样 monibuca PID 的 CPU/RSS/fd
│   └── verify.sh           [新] 单流校验
├── tests/
│   ├── test_classify.sh    [新]
│   ├── test_probe.sh       [新]
│   ├── test_sampler.sh     [新]
│   └── test_verify.sh      [新]
├── probe_streams.sh        [新] 入口：探测全部并写 streams_meta.json
├── test_high_res.sh        [新] 入口：按分组跑测 + 出报告
├── streams_meta.json       [新] 探测产物，入 git
├── reports/                [新, gitignored except .gitkeep]
│   └── .gitkeep
└── README.md               [改] 加 "2K/4K 高分辨率测试" 一节
```

每个文件单一职责。lib/ 下都是可独立 source 的纯函数（无副作用），顶层入口负责组装+IO。

---

## Task 1: 脚手架与测试 helper

**Files:**
- Create: `example/record-test/lib/_assert.sh`
- Create: `example/record-test/reports/.gitkeep`
- Modify: `example/record-test/.gitignore`（无则建）

- [ ] **Step 1: 建目录结构**

```bash
cd example/record-test
mkdir -p lib tests reports
touch reports/.gitkeep
```

- [ ] **Step 2: 写 .gitignore**

文件 `example/record-test/.gitignore`：

```gitignore
record/
logs/
*.db
*.db-journal
reports/*
!reports/.gitkeep
```

- [ ] **Step 3: 写 lib/_assert.sh**

文件 `example/record-test/lib/_assert.sh`：

```bash
#!/usr/bin/env bash
# Test assertion helpers. Source this in tests/test_*.sh.
# Exposes: assert_eq, assert_match, install_mock_bin, cleanup_mocks.

_ASSERT_PASS=0
_ASSERT_FAIL=0
_MOCK_DIR=""

assert_eq() {
    local expected="$1" actual="$2" label="${3:-}"
    if [ "$expected" = "$actual" ]; then
        _ASSERT_PASS=$((_ASSERT_PASS + 1))
        echo "  ✓ ${label:-assert_eq}"
    else
        _ASSERT_FAIL=$((_ASSERT_FAIL + 1))
        echo "  ✗ ${label:-assert_eq}: expected [$expected], got [$actual]"
    fi
}

assert_match() {
    local pattern="$1" actual="$2" label="${3:-}"
    if echo "$actual" | grep -qE "$pattern"; then
        _ASSERT_PASS=$((_ASSERT_PASS + 1))
        echo "  ✓ ${label:-assert_match}"
    else
        _ASSERT_FAIL=$((_ASSERT_FAIL + 1))
        echo "  ✗ ${label:-assert_match}: pattern [$pattern] not in [$actual]"
    fi
}

# install_mock_bin <name> <script-body>
# Creates an executable at $_MOCK_DIR/<name> with the given body, and prepends to PATH.
install_mock_bin() {
    local name="$1" body="$2"
    if [ -z "$_MOCK_DIR" ]; then
        _MOCK_DIR=$(mktemp -d)
        export PATH="$_MOCK_DIR:$PATH"
    fi
    cat >"$_MOCK_DIR/$name" <<EOF
#!/usr/bin/env bash
$body
EOF
    chmod +x "$_MOCK_DIR/$name"
}

cleanup_mocks() {
    [ -n "$_MOCK_DIR" ] && rm -rf "$_MOCK_DIR"
}

report_results() {
    echo ""
    echo "Pass: $_ASSERT_PASS  Fail: $_ASSERT_FAIL"
    [ "$_ASSERT_FAIL" -eq 0 ]
}
```

- [ ] **Step 4: 验证 helper 自冒烟**

```bash
cd example/record-test
bash -c 'source lib/_assert.sh; assert_eq 1 1 "smoke"; report_results'
```

预期：输出 `✓ smoke` 然后 `Pass: 1  Fail: 0`，退出码 0。

- [ ] **Step 5: 提交**

```bash
git add example/record-test/lib/_assert.sh example/record-test/.gitignore example/record-test/reports/.gitkeep
git commit -m "test(record-test): 加 bash 测试 helper 与目录骨架"
```

---

## Task 2: lib/classify.sh — 分辨率分档（纯函数）

**Files:**
- Create: `example/record-test/lib/classify.sh`
- Create: `example/record-test/tests/test_classify.sh`

- [ ] **Step 1: 写失败测试**

文件 `example/record-test/tests/test_classify.sh`：

```bash
#!/usr/bin/env bash
set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."
source lib/_assert.sh
source lib/classify.sh

assert_eq "unknown" "$(classify_resolution 0 0)"          "0x0 → unknown"
assert_eq "SD"      "$(classify_resolution 720 480)"      "720x480 → SD"
assert_eq "HD"      "$(classify_resolution 1280 720)"     "1280x720 → HD"
assert_eq "FHD"     "$(classify_resolution 1920 1080)"    "1920x1080 → FHD"
assert_eq "FHD"     "$(classify_resolution 1080 1920)"    "1080x1920 (竖屏) → FHD (短边)"
assert_eq "2K"      "$(classify_resolution 2560 1440)"    "2560x1440 → 2K"
assert_eq "2K"      "$(classify_resolution 2688 1520)"    "2688x1520 (5MP) → 2K"
assert_eq "4K"      "$(classify_resolution 3840 2160)"    "3840x2160 → 4K"
assert_eq "4K"      "$(classify_resolution 4096 2160)"    "DCI 4K → 4K"

report_results
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
cd example/record-test
chmod +x tests/test_classify.sh
bash tests/test_classify.sh
```

预期：报 `lib/classify.sh: No such file or directory`，退出码非 0。

- [ ] **Step 3: 写实现**

文件 `example/record-test/lib/classify.sh`：

```bash
#!/usr/bin/env bash
# 按短边像素分档（避免横竖屏混淆）。
# Usage: classify_resolution <width> <height>
# Echoes one of: unknown / SD / HD / FHD / 2K / 4K

classify_resolution() {
    local w="${1:-0}" h="${2:-0}"
    if [ "$w" -le 0 ] || [ "$h" -le 0 ]; then
        echo "unknown"
        return
    fi
    local short="$w"
    [ "$h" -lt "$w" ] && short="$h"
    if   [ "$short" -lt 720 ];  then echo "SD"
    elif [ "$short" -lt 1080 ]; then echo "HD"
    elif [ "$short" -lt 1440 ]; then echo "FHD"
    elif [ "$short" -lt 2000 ]; then echo "2K"
    else                              echo "4K"
    fi
}
```

- [ ] **Step 4: 跑测试，确认通过**

```bash
bash tests/test_classify.sh
```

预期：9 个 `✓`，`Pass: 9  Fail: 0`，退出码 0。

- [ ] **Step 5: 提交**

```bash
git add example/record-test/lib/classify.sh example/record-test/tests/test_classify.sh
git commit -m "feat(record-test): 加分辨率分档纯函数 lib/classify.sh"
```

---

## Task 3: lib/probe.sh — ffprobe 解析

**Files:**
- Create: `example/record-test/lib/probe.sh`
- Create: `example/record-test/tests/test_probe.sh`

ffprobe 命令模板（实际跑时用）：

```bash
ffprobe -v error -select_streams v:0 \
        -show_entries stream=width,height,codec_name,r_frame_rate,bit_rate \
        -of default=noprint_wrappers=1 \
        -rw_timeout 15000000 \
        -timeout 15000000 \
        "$url"
```

输出形如：

```
width=3840
height=2160
codec_name=h264
r_frame_rate=25/1
bit_rate=8388608
```

- [ ] **Step 1: 写失败测试**

文件 `example/record-test/tests/test_probe.sh`：

```bash
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
assert_match "^width=3840 height=2160 fps=25 codec=h264 bitrate_kbps=8192$" "$result" "4K probe"

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
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
chmod +x tests/test_probe.sh
bash tests/test_probe.sh
```

预期：报错或多个 `✗`。

- [ ] **Step 3: 写实现**

文件 `example/record-test/lib/probe.sh`：

```bash
#!/usr/bin/env bash
# ffprobe 包装。解析单路 RTSP URL 的视频流元数据。
# Usage:  probe_stream <url>
# Stdout: "width=<W> height=<H> fps=<F> codec=<C> bitrate_kbps=<B>"
# Exit:   0 成功 / 1 失败（含 timeout / 不通 / 解析空）

probe_stream() {
    local url="$1"
    local out
    out=$(ffprobe -v error -select_streams v:0 \
            -show_entries stream=width,height,codec_name,r_frame_rate,bit_rate \
            -of default=noprint_wrappers=1 \
            -rw_timeout 15000000 \
            "$url" 2>/dev/null) || return 1
    [ -z "$out" ] && return 1

    local width height codec rfr br
    width=$(echo "$out"  | awk -F= '$1=="width"{print $2}')
    height=$(echo "$out" | awk -F= '$1=="height"{print $2}')
    codec=$(echo "$out"  | awk -F= '$1=="codec_name"{print $2}')
    rfr=$(echo "$out"    | awk -F= '$1=="r_frame_rate"{print $2}')
    br=$(echo "$out"     | awk -F= '$1=="bit_rate"{print $2}')

    [ -z "$width" ] || [ -z "$height" ] && return 1

    # r_frame_rate 形如 "25/1" / "25000/1000" / "30000/1001"，整数化
    local fps=0
    if [ -n "$rfr" ] && [ "$rfr" != "0/0" ]; then
        local num="${rfr%/*}" den="${rfr#*/}"
        [ "$den" -gt 0 ] 2>/dev/null && fps=$(( (num + den/2) / den ))
    fi

    # bit_rate "N/A" → 0；单位 bps → kbps
    local kbps=0
    if [ -n "$br" ] && [ "$br" != "N/A" ]; then
        kbps=$(( br / 1000 ))
    fi

    echo "width=$width height=$height fps=$fps codec=$codec bitrate_kbps=$kbps"
}
```

- [ ] **Step 4: 跑测试，确认通过**

```bash
bash tests/test_probe.sh
```

预期：3 个 `✓`，`Pass: 3  Fail: 0`。

- [ ] **Step 5: 提交**

```bash
git add example/record-test/lib/probe.sh example/record-test/tests/test_probe.sh
git commit -m "feat(record-test): 加 ffprobe 解析模块 lib/probe.sh"
```

---

## Task 4: probe_streams.sh — 拼装并产出 streams_meta.json

**Files:**
- Create: `example/record-test/probe_streams.sh`

依赖前面的 `lib/probe.sh` + `lib/classify.sh`。从 `config.yaml` 的 `rtsp.pull:` 段提取所有 `streamPath: url`，逐一探测，写 `streams_meta.json`。

YAML 解析用 awk（不引入 yq）。识别厂商规则：URL 含 `:554/cam/realmonitor` → dahua；URL 含 `/ch1/main/av_stream` → hikvision；其它 → unknown。脱敏：`admin:<password>@` → `admin:***@`。

- [ ] **Step 1: 写脚本**

文件 `example/record-test/probe_streams.sh`：

```bash
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

# 收集流，构造 JSON 数组
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

    # ffprobe 加 timeout 兜底（外层）
    probe_out=$(timeout "$TIMEOUT" bash -c "source lib/probe.sh; probe_stream '$url'" 2>/dev/null)
    rc=$?

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
            --arg cls "$cls" --arg st "ok" --arg err "" \
            '{stream_path:$sp, source_url:$url, vendor:$v,
              width:$w, height:$h, fps:$fps, codec:$c, bitrate_kbps:$br,
              resolution_class:$cls, probe_status:$st, probe_error:(if $err=="" then null else $err end)}')
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
```

- [ ] **Step 2: chmod + 干跑校验 YAML 解析**

```bash
cd example/record-test
chmod +x probe_streams.sh
# 不真探测，先验证 extract_pulls 能从 config.yaml 抽出 35 条
bash -c 'source probe_streams.sh; extract_pulls' 2>/dev/null || true
# 上一行会因 set -u + 主流程跑而走完，预期看到 35 行 "live/cameraN<TAB>rtsp://..."
```

如果主流程也启动了，至少能看到 probe 在尝试连接，那说明 YAML 解析正确。如果 0 条流被发现，则解析逻辑有 bug，回去查 awk 段。

- [ ] **Step 3: 真探测（需能 ping 通摄像头网段）**

```bash
./probe_streams.sh --timeout=10
```

预期：35 行进度，落 `streams_meta.json`，每条含 `width/height/resolution_class`。无网络环境下会全部 `probe_failed`，那也能跑通脚本，只是数据为 0。

- [ ] **Step 4: 检查产物**

```bash
jq '.streams | group_by(.resolution_class) | map({class: .[0].resolution_class, count: length})' streams_meta.json
```

预期：输出每个分档计数表。

- [ ] **Step 5: 提交脚本（不提交 streams_meta.json，留到 Task 8 真跑过再提）**

```bash
git add example/record-test/probe_streams.sh
git commit -m "feat(record-test): 加 probe_streams.sh 探测所有摄像头分辨率"
```

---

## Task 5: lib/sampler.sh — 进程资源采样

**Files:**
- Create: `example/record-test/lib/sampler.sh`
- Create: `example/record-test/tests/test_sampler.sh`

monibuca 是 `go run` 起来的，真正监听 8080 的是 `go run` 编译后的临时二进制（不是 `go run` 进程本身）。最稳的拿 PID 方式：`lsof -i :<port> -t -sTCP:LISTEN`。

- [ ] **Step 1: 写失败测试**

文件 `example/record-test/tests/test_sampler.sh`：

```bash
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
assert_eq "1" "$pass" "sampler 写 ≥4 行 (header + 3 samples)"

header=$(head -1 "$out")
assert_match "ts,cpu_pct,rss_mb" "$header" "header 含字段"

rm -f "$out"
report_results
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
chmod +x tests/test_sampler.sh
bash tests/test_sampler.sh
```

预期：报 `lib/sampler.sh: No such file or directory`。

- [ ] **Step 3: 写实现**

文件 `example/record-test/lib/sampler.sh`：

```bash
#!/usr/bin/env bash
# 进程资源采样工具。
# 提供两个函数：
#   get_m7s_pid <http_port>   — 通过监听端口反查 monibuca PID
#   sample_pid_to_csv <pid> <out_csv> <count> <interval_sec>
#     - 写入 header: ts,cpu_pct,rss_mb,fd_count
#     - 每 interval_sec 采一次，共 count 次
#     - 进程消失则提前结束并在 csv 追加一行 cpu=-1

get_m7s_pid() {
    local port="${1:-8080}"
    lsof -nP -iTCP:"$port" -sTCP:LISTEN -t 2>/dev/null | head -1
}

sample_pid_to_csv() {
    local pid="$1" out="$2" count="$3" interval="$4"
    echo "ts,cpu_pct,rss_mb,fd_count" > "$out"
    local i=0
    while [ "$i" -lt "$count" ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "$(date +%s),-1,-1,-1" >> "$out"
            return
        fi
        # ps -o %cpu,rss : %cpu 总占用%，rss KB
        local ps_out
        ps_out=$(ps -p "$pid" -o %cpu=,rss= 2>/dev/null | awk '{print $1","int($2/1024)}')
        local fd
        fd=$(lsof -p "$pid" 2>/dev/null | wc -l | tr -d ' ')
        [ -z "$ps_out" ] && ps_out="0,0"
        [ -z "$fd" ] && fd=0
        echo "$(date +%s),$ps_out,$fd" >> "$out"
        i=$((i + 1))
        [ "$i" -lt "$count" ] && sleep "$interval"
    done
}
```

- [ ] **Step 4: 跑测试，确认通过**

```bash
bash tests/test_sampler.sh
```

预期：2 个 `✓`，`Pass: 2  Fail: 0`。

- [ ] **Step 5: 提交**

```bash
git add example/record-test/lib/sampler.sh example/record-test/tests/test_sampler.sh
git commit -m "feat(record-test): 加进程采样模块 lib/sampler.sh"
```

---

## Task 6: lib/verify.sh — 单流校验

**Files:**
- Create: `example/record-test/lib/verify.sh`
- Create: `example/record-test/tests/test_verify.sh`

verify 接受 `(file_path, expected_w, expected_h, requested_duration)`，echo 一行 `STATUS<TAB>actual_size<TAB>actual_duration<TAB>actual_wxh<TAB>detail`，其中 STATUS ∈ {PASS, WARN, FAIL}。

- [ ] **Step 1: 写失败测试**

文件 `example/record-test/tests/test_verify.sh`：

```bash
#!/usr/bin/env bash
set -u
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."
source lib/_assert.sh
source lib/verify.sh

tmpdir=$(mktemp -d)
trap "rm -rf $tmpdir" EXIT

# Case 1: 文件不存在 → FAIL
result=$(verify_stream "$tmpdir/nope.mp4" 3840 2160 60)
status=$(echo "$result" | cut -f1)
assert_eq "FAIL" "$status" "缺文件 → FAIL"

# Case 2: 文件大小为 0 → FAIL
touch "$tmpdir/empty.mp4"
result=$(verify_stream "$tmpdir/empty.mp4" 3840 2160 60)
status=$(echo "$result" | cut -f1)
assert_eq "FAIL" "$status" "空文件 → FAIL"

# Case 3: 正常 4K 文件 + 时长偏差 1% → PASS
dd if=/dev/urandom of="$tmpdir/ok.mp4" bs=1024 count=10 >/dev/null 2>&1
install_mock_bin ffprobe '
case "$*" in
  *show_entries*)
    cat <<EOF
width=3840
height=2160
duration=59.4
EOF
    ;;
  *)
    echo "Duration: 00:00:59.40, start: 0" >&2
    ;;
esac
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

cleanup_mocks
report_results
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
chmod +x tests/test_verify.sh
bash tests/test_verify.sh
```

- [ ] **Step 3: 写实现**

文件 `example/record-test/lib/verify.sh`：

```bash
#!/usr/bin/env bash
# 单流录制结果校验。
# Usage: verify_stream <file> <expected_w> <expected_h> <requested_duration_sec>
# Stdout: STATUS<TAB>size_bytes<TAB>duration_sec<TAB>actual_wxh<TAB>detail
#         STATUS ∈ {PASS, WARN, FAIL}

verify_stream() {
    local file="$1" exp_w="$2" exp_h="$3" req_dur="$4"

    if [ ! -f "$file" ]; then
        printf "FAIL\t0\t0\t0x0\tfile_missing\n"
        return
    fi
    local size
    size=$(stat -f%z "$file" 2>/dev/null || stat -c%s "$file" 2>/dev/null || echo 0)
    if [ "$size" -le 0 ]; then
        printf "FAIL\t0\t0\t0x0\tempty_file\n"
        return
    fi

    local out
    out=$(ffprobe -v error -select_streams v:0 \
            -show_entries stream=width,height:format=duration \
            -of default=noprint_wrappers=1 \
            "$file" 2>/dev/null)
    if [ -z "$out" ]; then
        printf "FAIL\t%s\t0\t0x0\tprobe_failed\n" "$size"
        return
    fi
    local w h dur
    w=$(echo "$out"   | awk -F= '$1=="width"{print $2}')
    h=$(echo "$out"   | awk -F= '$1=="height"{print $2}')
    dur=$(echo "$out" | awk -F= '$1=="duration"{print $2}')
    w="${w:-0}"; h="${h:-0}"; dur="${dur:-0}"

    if [ "$w" != "$exp_w" ] || [ "$h" != "$exp_h" ]; then
        printf "FAIL\t%s\t%s\t%sx%s\tresolution_mismatch_expected_%sx%s\n" \
            "$size" "$dur" "$w" "$h" "$exp_w" "$exp_h"
        return
    fi

    # 时长偏差判断（用 awk 算浮点）
    local dev
    dev=$(awk -v d="$dur" -v r="$req_dur" 'BEGIN{
        if (r==0) {print 0; exit}
        x=(d-r)/r; if(x<0)x=-x; printf "%.4f", x
    }')
    local exceed
    exceed=$(awk -v x="$dev" 'BEGIN{print (x>0.05)?1:0}')
    if [ "$exceed" -eq 1 ]; then
        printf "WARN\t%s\t%s\t%sx%s\tduration_drift_%s\n" "$size" "$dur" "$w" "$h" "$dev"
        return
    fi

    printf "PASS\t%s\t%s\t%sx%s\tok\n" "$size" "$dur" "$w" "$h"
}
```

- [ ] **Step 4: 跑测试，确认通过**

```bash
bash tests/test_verify.sh
```

预期：5 个 `✓`，`Pass: 5  Fail: 0`。

- [ ] **Step 5: 提交**

```bash
git add example/record-test/lib/verify.sh example/record-test/tests/test_verify.sh
git commit -m "feat(record-test): 加单流校验模块 lib/verify.sh"
```

---

## Task 7: test_high_res.sh — 主入口

**Files:**
- Create: `example/record-test/test_high_res.sh`

按 `--group=4k|2k|all|2k,4k` 过滤，启停录制，采样，调 verify，渲染 markdown 报告。沿用 `test_minimal.sh` 的 API 调用模板（`POST /mp4/api/start/{stream}` body `{duration, fragment:"0", filePath, fileName}`）。

- [ ] **Step 1: 写脚本**

文件 `example/record-test/test_high_res.sh`：

```bash
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

for arg in "$@"; do
    case "$arg" in
        --group=*)    GROUP="${arg#*=}" ;;
        --duration=*) DURATION="${arg#*=}" ;;
        -h|--help)
            echo "Usage: $0 [--group=4k|2k|all|2k,4k] [--duration=300]"
            exit 0
            ;;
        *) echo "未知参数: $arg" >&2; exit 2 ;;
    esac
done

[ -f streams_meta.json ] || { echo "缺 streams_meta.json，先跑 ./probe_streams.sh" >&2; exit 2; }
command -v jq >/dev/null || { echo "缺 jq" >&2; exit 2; }

source lib/sampler.sh
source lib/verify.sh

# 1. 校验服务
if ! curl -sf "http://$NODE_IP:$HTTP_PORT/api/sysinfo" >/dev/null; then
    echo "monibuca 未运行，请先 ./start.sh" >&2
    exit 1
fi

# 2. 过滤流
filter_jq=""
if [ "$GROUP" = "all" ]; then
    filter_jq='select(.probe_status=="ok")'
else
    # 把 2k,4k 转成 ["2K","4K"]（大写）
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
echo "[1/5] 清理 record/live"
for e in "${entries[@]}"; do
    sp=$(echo "$e" | jq -r '.stream_path')
    curl -s -X POST "http://$NODE_IP:$HTTP_PORT/mp4/api/stop/$sp" >/dev/null 2>&1
done
rm -rf record/live 2>/dev/null || true
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
    file="record/live/${sp##*/}.mp4"
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
```

- [ ] **Step 2: chmod**

```bash
chmod +x test_high_res.sh
```

- [ ] **Step 3: 干跑校验（用空的 streams_meta.json）**

```bash
echo '{"probed_at":"x","probe_timeout_sec":15,"streams":[]}' > streams_meta.json
./test_high_res.sh --group=4k --duration=5
```

预期：报"分组 4K 下无流"并退出 1（不抛 awk/jq 错）。

- [ ] **Step 4: 删掉临时 streams_meta.json**

```bash
rm streams_meta.json
```

- [ ] **Step 5: 提交**

```bash
git add example/record-test/test_high_res.sh
git commit -m "feat(record-test): 加 test_high_res.sh 分组录制测试主入口"
```

---

## Task 8: 真探测 + 端到端冒烟 + README

**Files:**
- Create: `example/record-test/streams_meta.json`
- Modify: `example/record-test/README.md`

需要：能 ping 通摄像头网段的环境 + 已起 monibuca 服务（`./start.sh`）。

- [ ] **Step 1: 起 monibuca 服务（后台）**

```bash
cd example/record-test
./start.sh &
# 等 5s 让服务起来
sleep 5
curl -sf http://localhost:8080/api/sysinfo >/dev/null && echo "服务 ok"
```

- [ ] **Step 2: 真探测**

```bash
./probe_streams.sh --timeout=10
jq '.streams | group_by(.resolution_class) | map({class: .[0].resolution_class, count: length})' streams_meta.json
```

预期：每个分档计数。记下 4K 组数量；若为 0，记下实际分布并在 README 中如实写明（不强行虚构 4K）。

- [ ] **Step 3: 跑一次 60s 4K 测试（或可用的最高档）**

如果 4K 组 > 0：

```bash
./test_high_res.sh --group=4k --duration=60
```

如果 4K 组为 0，找到实际最高档（比如 2K）：

```bash
./test_high_res.sh --group=2k --duration=60
```

预期：录制 60s 后产出 `reports/report-*.md`，逐流校验结果可读，metrics csv 行数 ≥ 60。

- [ ] **Step 4: 检查报告**

```bash
ls -lh reports/
cat reports/report-*.md | tail -40
```

- [ ] **Step 5: 停服务**

```bash
pkill -f 'go run.*main.go' 2>/dev/null
```

- [ ] **Step 6: 更新 README**

在 `example/record-test/README.md` 现有 "## 测试录制" 之后插入一节：

```markdown
## 2K/4K 高分辨率测试

定向跑某个分辨率分组的并发录制，自动出报告。

### 1. 探测所有摄像头分辨率

```bash
./probe_streams.sh
```

产出 `streams_meta.json`（已提交，含每路 W/H/fps/codec/分档）。新增摄像头后重跑，git diff 看变化。

### 2. 跑分组测试

```bash
# 只测 4K 摄像头（默认录 5 分钟）
./test_high_res.sh --group=4k

# 自定义时长
./test_high_res.sh --group=4k --duration=600

# 多档同跑
./test_high_res.sh --group=2k,4k

# 全部探测成功的流
./test_high_res.sh --group=all --duration=120
```

### 3. 查看报告

```bash
ls -lh reports/
cat reports/report-*.md | tail -40
```

每次跑出两个文件：
- `reports/report-YYYYMMDD-HHMMSS.md` — 逐流 PASS/WARN/FAIL + 性能峰值
- `reports/metrics-YYYYMMDD-HHMMSS.csv` — 1Hz 采样的 CPU/RSS/fd

### 4. 跑单测（修改 lib/ 后）

```bash
bash tests/test_classify.sh
bash tests/test_probe.sh
bash tests/test_sampler.sh
bash tests/test_verify.sh
```

### 当前摄像头分辨率分布

(由 `streams_meta.json` 真实数据反映；可通过下面命令查看)

```bash
jq '.streams | group_by(.resolution_class) | map({class: .[0].resolution_class, count: length})' streams_meta.json
```
```

- [ ] **Step 7: 提交**

```bash
git add example/record-test/streams_meta.json example/record-test/README.md
git commit -m "feat(record-test): 提交 streams_meta.json 基线 + README 加 2K/4K 测试章节"
```

---

## Task 9: 验收清单核对

- [ ] **Step 1: 对照 spec §8 逐项核**

```bash
# 1. streams_meta.json 含 35 条
jq '.streams | length' example/record-test/streams_meta.json

# 2. URL 已脱敏（不应出现明文密码）
grep -E 'admin:[^*][^@]+@' example/record-test/streams_meta.json || echo "OK 已脱敏"

# 3. report 内字段齐
grep -c '|' example/record-test/reports/report-*.md | head -1

# 4. 4 个单测全绿
cd example/record-test
for t in tests/test_*.sh; do echo "=== $t ==="; bash "$t"; done
```

逐项确认 spec §8 验收清单全部勾上。

- [ ] **Step 2: 若有遗漏，回到对应 Task 补**
- [ ] **Step 3: 无遗漏则结束**

---

## Self-Review 记录

**Spec coverage**:
- §3.1 分档规则 → Task 2 实现 + 测
- §3.2 streams_meta.json schema → Task 4 拼装
- §3.3 数据流 → Task 7 拼装
- §3.4 verify 检查项 → Task 6 实现（注：moov 位置由 PASS/WARN/FAIL 中的 detail 体现，省略了独立检查项，简化为时长偏差 WARN — 这是相对 spec 的弱化，留在风险中）
- §3.5 report 模板 → Task 7 渲染
- §4 错误处理 → 散在各任务（probe 失败标记 / streams_meta 缺失提示 / sampler 异常自然 csv 写 -1）
- §5 CLI → Task 4 (probe) + Task 7 (test_high_res)
- §6 实现顺序 → Task 2-8 对应
- §8 验收 → Task 9

**与 spec 的微小偏离**：
- spec §3.4 列了 6 项 verify 检查，本计划合并：moov 位置检查未单独实现（实现完整 moov trace 解析需要 `ffprobe -v trace | grep moov` 输出排序判断，工程量较大且 mp4 插件本身就有 moveTrailer 逻辑保证 fast start）。本计划默认 mp4 插件已保证 moov 在前，不重复检测。若需检测，加 Task 9.1（后续可加）。
- spec §3.3 sampler 列了 `goroutine_count`，本计划未采（需要走 monibuca 的 metrics 端点或 pprof，引入复杂度），csv 只采 cpu/rss/fd。

**Type consistency**: 
- `classify_resolution(w, h)` → echo class 字符串，全计划一致
- `probe_stream(url)` → echo `width=X height=Y fps=Z codec=C bitrate_kbps=B`，全计划一致
- `verify_stream(file, w, h, dur)` → echo TSV 5 列 `STATUS\tsize\tduration\tWxH\tdetail`，Task 6/7 一致
- `get_m7s_pid(port)` / `sample_pid_to_csv(pid, out, count, interval)`，Task 5/7 一致

**No placeholders**：已逐 task 走过，每步都给了完整代码块或具体命令。
