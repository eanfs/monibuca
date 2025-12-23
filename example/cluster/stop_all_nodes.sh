#!/bin/bash

# Stop all nodes for example/cluster-dc.
#
# Note: m7s writes/overwrites ./shutdown.sh automatically (pkg/util/linux.go),
# so do NOT rely on ./shutdown.sh for test automation.
#
# Strategy:
# 1) Kill PIDs from runtime/pids (start_all_nodes.sh writes them)
# 2) Best-effort kill any leftover listeners on known ports

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

PID_DIR="${PID_DIR:-runtime/pids}"

echo "Stopping nodes..."

shopt -s nullglob
pids_files=()
if [ -d "$PID_DIR" ]; then
  pids_files=("$PID_DIR"/*.pid)
fi

for pidfile in "${pids_files[@]}"; do
  pid="$(cat "$pidfile" 2>/dev/null || true)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    kill -INT "$pid" 2>/dev/null || true
  fi
done

sleep 1

for pidfile in "${pids_files[@]}"; do
  pid="$(cat "$pidfile" 2>/dev/null || true)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$pidfile" || true
done

# Best-effort: kill any leftover listeners on known ports.
PORTS=(8081 8082 8083 8084 8085 50051 50052 50053 50054 50055 5541 5542 5543 5544 5545)
extra_pids=()
for port in "${PORTS[@]}"; do
  while IFS= read -r pid; do
    [ -n "$pid" ] && extra_pids+=("$pid")
  done < <(ss -lntp 2>/dev/null | rg ":${port}\\b" | rg -o "pid=\\d+" | sed 's/pid=//' | sort -u || true)
done

if [ ${#extra_pids[@]} -gt 0 ]; then
  mapfile -t uniq_pids < <(printf "%s\n" "${extra_pids[@]}" | sort -u)
  echo "Killing leftover listener PIDs: ${uniq_pids[*]}"
  for pid in "${uniq_pids[@]}"; do
    kill -INT "$pid" 2>/dev/null || true
  done
  sleep 1
  for pid in "${uniq_pids[@]}"; do
    kill -KILL "$pid" 2>/dev/null || true
  done
fi

echo "Stopped."

