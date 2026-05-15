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
