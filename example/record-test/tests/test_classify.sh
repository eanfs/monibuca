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
