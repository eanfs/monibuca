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
        if [ "$den" -gt 0 ] 2>/dev/null; then
            fps=$(( (num + den/2) / den ))
        fi
    fi

    # bit_rate "N/A" → 0；单位 bps → kbps（按 SI 1000 折算）
    local kbps=0
    if [ -n "$br" ] && [ "$br" != "N/A" ]; then
        kbps=$(( br / 1000 ))
    fi

    echo "width=$width height=$height fps=$fps codec=$codec bitrate_kbps=$kbps"
}
