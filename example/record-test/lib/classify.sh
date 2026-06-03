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
