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
