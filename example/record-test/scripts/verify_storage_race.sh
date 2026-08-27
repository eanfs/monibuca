#!/usr/bin/env bash
set -euo pipefail
set -E
umask 077

fail() {
    local round="$1" stage="$2" code="$3" state="$4"
    printf 'FAIL round=%s stage=%s code=%s state=%s\n' "$round" "$stage" "$code" "$state" >&2
    exit 1
}

unexpected_error() {
    local code="$?"
    trap - ERR
    fail 0 unexpected "$code" aborted
}
trap unexpected_error ERR

require_command() {
    local name="$1"
    command -v "$name" >/dev/null 2>&1 || fail 0 "prerequisite-$name" missing unavailable
}

require_command curl
require_command python3
require_command openssl
require_command ffprobe
require_command docker
docker compose version >/dev/null 2>&1 || fail 0 prerequisite-compose missing unavailable

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" 2>/dev/null && pwd)" || fail 0 locate-script invalid unavailable
ROOT_DIR="$(cd "$SCRIPT_DIR/../../.." 2>/dev/null && pwd)" || fail 0 locate-root invalid unavailable
COMPOSE_FILE="$ROOT_DIR/example/record-test/docker-compose.storage-race.yml"
TEMPLATE_FILE="$ROOT_DIR/example/record-test/config.storage-race.yaml.tmpl"

WORK_DIR="$(mktemp -d 2>/dev/null)" || fail 0 create-workdir failed unavailable
chmod 700 "$WORK_DIR" 2>/dev/null || fail 0 secure-workdir failed unavailable

project_token="$(openssl rand -hex 6 2>/dev/null)" || fail 0 generate-project failed unavailable
user_token="$(openssl rand -hex 8 2>/dev/null)" || fail 0 generate-user failed unavailable
password_token="$(openssl rand -hex 24 2>/dev/null)" || fail 0 generate-password failed unavailable
COMPOSE_PROJECT_NAME="storage-race-$project_token"
MINIO_ROOT_USER="race$user_token"
MINIO_ROOT_PASSWORD="$password_token"
STORAGE_RACE_IMAGE="storage-race-runtime:$COMPOSE_PROJECT_NAME"
STORAGE_RACE_CONFIG="$WORK_DIR/config.yaml"
STORAGE_RACE_DATA_DIR="$WORK_DIR/monibuca-data"
STORAGE_RACE_MINIO_DIR="$WORK_DIR/minio-data"
STORAGE_RACE_ARTIFACT_DIR="$WORK_DIR/artifacts"
STORAGE_RACE_UID="$(id -u 2>/dev/null)" || fail 0 detect-uid failed unavailable
STORAGE_RACE_GID="$(id -g 2>/dev/null)" || fail 0 detect-gid failed unavailable
export COMPOSE_PROJECT_NAME MINIO_ROOT_USER MINIO_ROOT_PASSWORD
export STORAGE_RACE_IMAGE STORAGE_RACE_CONFIG STORAGE_RACE_DATA_DIR STORAGE_RACE_MINIO_DIR STORAGE_RACE_ARTIFACT_DIR
export STORAGE_RACE_UID STORAGE_RACE_GID
unset project_token user_token password_token

select_http_port() {
    python3 - <<'PY' 2>/dev/null
import socket

with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
}

STORAGE_RACE_HTTP_PORT="$(select_http_port)" || fail 0 select-http-port failed unavailable
export STORAGE_RACE_HTTP_PORT
[[ "$STORAGE_RACE_HTTP_PORT" =~ ^[0-9]+$ ]] || fail 0 select-http-port invalid unavailable
mkdir -m 700 "$STORAGE_RACE_DATA_DIR" "$STORAGE_RACE_MINIO_DIR" "$STORAGE_RACE_ARTIFACT_DIR" 2>/dev/null || fail 0 create-bind-dirs failed unavailable

compose() {
    docker compose --project-name "$COMPOSE_PROJECT_NAME" --file "$COMPOSE_FILE" "$@"
}

cleanup() {
    trap - ERR
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
    docker image rm --force "$STORAGE_RACE_IMAGE" >/dev/null 2>&1 || true
    rm -rf -- "$WORK_DIR" >/dev/null 2>&1 || true
    unset MINIO_ROOT_USER MINIO_ROOT_PASSWORD
}
trap cleanup EXIT

docker info >/dev/null 2>&1 || fail 0 prerequisite-daemon unavailable stopped

render_config() {
    local round="$1"
    if ! python3 - "$TEMPLATE_FILE" "$STORAGE_RACE_CONFIG" <<'PY' 2>/dev/null
import os
import pathlib
import re
import sys

template_path = pathlib.Path(sys.argv[1])
output_path = pathlib.Path(sys.argv[2])
source = template_path.read_text(encoding="utf-8")
replacements = {
    "__ALLOW_LOCAL_FALLBACK__": os.environ["ALLOW_LOCAL_FALLBACK"],
    "__MINIO_ROOT_USER__": os.environ["MINIO_ROOT_USER"],
    "__MINIO_ROOT_PASSWORD__": os.environ["MINIO_ROOT_PASSWORD"],
}
found = set(re.findall(r"__[A-Z0-9_]+__", source))
if found != set(replacements) or any(source.count(key) != 1 for key in replacements):
    raise SystemExit(1)
rendered = source
for placeholder, value in replacements.items():
    rendered = rendered.replace(placeholder, value)
if re.search(r"__[A-Z0-9_]+__", rendered):
    raise SystemExit(1)
output_path.write_text(rendered, encoding="utf-8")
PY
    then
        fail "$round" render-config failed unavailable
    fi
    chmod 600 "$STORAGE_RACE_CONFIG" 2>/dev/null || fail "$round" secure-config failed unavailable
}

reset_case_volumes() {
    local round="$1"
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
    case "$STORAGE_RACE_DATA_DIR:$STORAGE_RACE_MINIO_DIR" in
        "$WORK_DIR"/*:"$WORK_DIR"/*) ;;
        *) fail "$round" reset-volumes invalid unsafe-path ;;
    esac
    if ! rm -rf -- "$STORAGE_RACE_DATA_DIR" "$STORAGE_RACE_MINIO_DIR" >/dev/null 2>&1; then
        fail "$round" reset-volumes failed unavailable
    fi
    mkdir -m 700 "$STORAGE_RACE_DATA_DIR" "$STORAGE_RACE_MINIO_DIR" 2>/dev/null || fail "$round" reset-volumes failed unavailable
}

storage_status_code() {
    curl --silent --output "$WORK_DIR/storage-status.json" --write-out '%{http_code}' \
        --connect-timeout 2 --max-time 5 \
        "http://127.0.0.1:$STORAGE_RACE_HTTP_PORT/api/storage/status" 2>/dev/null
}

json_field() {
    local field="$1"
    python3 - "$WORK_DIR/storage-status.json" "$field" <<'PY' 2>/dev/null
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    value = json.load(source)[sys.argv[2]]
print(str(value).lower())
PY
}

assert_field() {
    local expected="$1" field="$2" round="$3" stage="$4" actual
    actual="$(json_field "$field" || true)"
    [[ "$actual" == "$expected" ]] || fail "$round" "$stage" mismatch unexpected
}

assert_http_code() {
    local expected="$1" actual="$2" round="$3" stage="$4"
    [[ "$actual" =~ ^[0-9]{3}$ ]] || actual=invalid
    [[ "$actual" == "$expected" ]] || fail "$round" "$stage" "$actual" "expected-$expected"
}

wait_for_status_code() {
    local expected="$1" attempts="$2" attempt code
    for ((attempt = 1; attempt <= attempts; attempt++)); do
        code="$(storage_status_code || true)"
        if [[ "$code" == "$expected" ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

stream_info_code() {
    curl --silent --output /dev/null --write-out '%{http_code}' \
        --connect-timeout 2 --max-time 5 \
        "http://127.0.0.1:$STORAGE_RACE_HTTP_PORT/api/stream/info/live/storage-race" 2>/dev/null
}

wait_for_stream() {
    local attempts="$1" attempt code
    for ((attempt = 1; attempt <= attempts; attempt++)); do
        code="$(stream_info_code || true)"
        if [[ "$code" == 200 ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

record_start_code() {
    local file_name="$1"
    printf '{"duration":"0","fragment":"0","filePath":"live/storage-race","fileName":"%s"}\n' "$file_name" 2>/dev/null >"$WORK_DIR/start-request.json" || return 1
    curl --silent --output "$WORK_DIR/start-response.json" --write-out '%{http_code}' \
        --connect-timeout 2 --max-time 10 --request POST \
        --header 'Content-Type: application/json' --data-binary "@$WORK_DIR/start-request.json" \
        "http://127.0.0.1:$STORAGE_RACE_HTTP_PORT/mp4/api/start/live/storage-race" 2>/dev/null
}

record_stop_code() {
    curl --silent --output "$WORK_DIR/stop-response.json" --write-out '%{http_code}' \
        --connect-timeout 2 --max-time 10 --request POST \
        --header 'Content-Type: application/json' --data '{}' \
        "http://127.0.0.1:$STORAGE_RACE_HTTP_PORT/mp4/api/stop/live/storage-race" 2>/dev/null
}

query_record_count() {
    local file_name="$1"
    python3 - "$STORAGE_RACE_DATA_DIR/storage-race.db" "$file_name" <<'PY' 2>/dev/null
import pathlib
import sqlite3
import sys

path = pathlib.Path(sys.argv[1])
if not path.is_file():
    raise SystemExit(1)
uri = path.resolve().as_uri() + "?mode=ro"
try:
    with sqlite3.connect(uri, uri=True, timeout=0.1) as database:
        row = database.execute(
            """
            SELECT count(*)
              FROM record_streams
             WHERE stream_path = ?
               AND type = ?
               AND file_name = ?
               AND deleted_at IS NULL
            """,
            ("live/storage-race", "mp4", sys.argv[2]),
        ).fetchone()
except (OSError, sqlite3.DatabaseError, sqlite3.OperationalError):
    raise SystemExit(1)
print(row[0])
PY
}

wait_for_record_count() {
    local file_name="$1" expected="$2" attempts="$3" attempt count
    for ((attempt = 1; attempt <= attempts; attempt++)); do
        count="$(query_record_count "$file_name" || true)"
        if [[ "$count" == "$expected" ]]; then
            return 0
        fi
        sleep 1
    done
    return 1
}

query_finalized_record() {
    local file_name="$1" storage_type="$2"
    python3 - "$STORAGE_RACE_DATA_DIR/storage-race.db" "$file_name" "$storage_type" <<'PY' 2>/dev/null
import pathlib
import sqlite3
import sys

path = pathlib.Path(sys.argv[1])
if not path.is_file():
    raise SystemExit(1)
uri = path.resolve().as_uri() + "?mode=ro"
try:
    with sqlite3.connect(uri, uri=True, timeout=0.1) as database:
        rows = database.execute(
            """
            SELECT id, storage_type
              FROM record_streams
             WHERE stream_path = ?
               AND type = ?
               AND file_name = ?
               AND storage_type = ?
               AND deleted_at IS NULL
               AND end_time IS NOT NULL
             ORDER BY id DESC
             LIMIT 2
            """,
            ("live/storage-race", "mp4", sys.argv[2], sys.argv[3]),
        ).fetchall()
except (OSError, sqlite3.DatabaseError, sqlite3.OperationalError):
    raise SystemExit(1)
if len(rows) != 1:
    raise SystemExit(1)
print(f"{rows[0][0]} {rows[0][1]}")
PY
}

wait_for_finalized_record() {
    local file_name="$1" storage_type="$2" attempts="$3" attempt row
    for ((attempt = 1; attempt <= attempts; attempt++)); do
        row="$(query_finalized_record "$file_name" "$storage_type" || true)"
        if [[ "$row" =~ ^[0-9]+\ (local|s3)$ ]]; then
            printf '%s\n' "$row"
            return 0
        fi
        sleep 1
    done
    return 1
}

validate_media() {
    local media_path="$1" probe_path="$WORK_DIR/probe.json"
    ffprobe -v error -select_streams v -show_entries 'format=duration:stream=index' \
        -of json "$media_path" 2>/dev/null >"$probe_path" || return 1
    python3 - "$probe_path" <<'PY' 2>/dev/null
import json
import math
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as source:
        probe = json.load(source)
    duration = float(probe["format"]["duration"])
    valid = math.isfinite(duration) and duration > 0 and len(probe.get("streams", [])) >= 1
except (KeyError, TypeError, ValueError, OSError, json.JSONDecodeError):
    valid = False
raise SystemExit(0 if valid else 1)
PY
}

download_record() {
    local round="$1" stage="$2" record_id="$3" output_name="$4" attempt host_output
    [[ "$record_id" =~ ^[0-9]+$ ]] || fail "$round" "$stage" invalid unsafe-id
    [[ "$output_name" =~ ^[a-z0-9-]+\.mp4$ ]] || fail "$round" "$stage" invalid unsafe-name
    host_output="$STORAGE_RACE_ARTIFACT_DIR/$output_name"
    for ((attempt = 1; attempt <= 30; attempt++)); do
        rm -f -- "$host_output" >/dev/null 2>&1 || fail "$round" "$stage" failed unavailable
        if compose exec --no-TTY monibuca curl --silent --fail --location \
            --connect-timeout 2 --max-time 15 \
            --output "/artifacts/$output_name" \
            "http://127.0.0.1:8080/mp4/download/live/storage-race?id=$record_id" >/dev/null 2>&1 \
            && validate_media "$host_output"; then
            return 0
        fi
        sleep 1
    done
    fail "$round" "$stage" timeout body-unavailable
}

start_case() {
    local round="$1" stage="$2"
    if ! compose up --detach --no-build >/dev/null 2>&1; then
        fail "$round" "$stage" compose start-failed
    fi
}

run_no_fallback_case() {
    local round="$1" file_name start_code stop_code row record_id record_type
    printf 'RUN round=%s scenario=required-s3\n' "$round"
    reset_case_volumes "$round"
    export ALLOW_LOCAL_FALLBACK=false MINIO_DELAY_SECONDS=30
    render_config "$round"
    start_case "$round" required-s3-up

    wait_for_status_code 503 20 || fail "$round" required-s3-degraded timeout not-observed
    assert_field s3 desiredType "$round" required-s3-desired
    assert_field s3 activeType "$round" required-s3-active
    assert_field true degraded "$round" required-s3-degraded-field
    assert_field false fallbackActive "$round" required-s3-fallback-field
    wait_for_stream 45 || fail "$round" publisher-readiness timeout no-stream

    file_name="required-s3-unavailable-$round.mp4"
    start_code="$(record_start_code "$file_name" || true)"
    assert_http_code 503 "$start_code" "$round" required-s3-rejected
    wait_for_record_count "$file_name" 0 30 || fail "$round" required-s3-no-row timeout unexpected-row

    wait_for_status_code 200 120 || fail "$round" required-s3-recovery timeout not-ready
    assert_field s3 activeType "$round" required-s3-recovered-active
    assert_field false degraded "$round" required-s3-recovered-degraded
    assert_field false fallbackActive "$round" required-s3-recovered-fallback

    file_name="required-s3-recovered-$round.mp4"
    start_code="$(record_start_code "$file_name" || true)"
    assert_http_code 200 "$start_code" "$round" required-s3-start
    sleep 6
    stop_code="$(record_stop_code || true)"
    assert_http_code 200 "$stop_code" "$round" required-s3-stop
    row="$(wait_for_finalized_record "$file_name" s3 90)" || fail "$round" required-s3-finalized timeout no-row
    read -r record_id record_type <<<"$row"
    [[ "$record_type" == s3 ]] || fail "$round" required-s3-storage mismatch unexpected
    download_record "$round" required-s3-download "$record_id" "required-s3-$round.mp4"
}

run_explicit_fallback_case() {
    local round="$1" file_name start_code stop_code local_row local_id local_type s3_row s3_id s3_type
    printf 'RUN round=%s scenario=temporary-local-fallback\n' "$round"
    reset_case_volumes "$round"
    export ALLOW_LOCAL_FALLBACK=true MINIO_DELAY_SECONDS=40
    render_config "$round"
    start_case "$round" fallback-up

    wait_for_status_code 503 20 || fail "$round" fallback-degraded timeout not-observed
    assert_field s3 desiredType "$round" fallback-desired
    assert_field local activeType "$round" fallback-active
    assert_field true degraded "$round" fallback-degraded-field
    assert_field true fallbackActive "$round" fallback-flag
    wait_for_stream 45 || fail "$round" publisher-readiness timeout no-stream

    file_name="fallback-local-$round.mp4"
    start_code="$(record_start_code "$file_name" || true)"
    assert_http_code 200 "$start_code" "$round" fallback-local-start
    sleep 6
    stop_code="$(record_stop_code || true)"
    assert_http_code 200 "$stop_code" "$round" fallback-local-stop
    local_row="$(wait_for_finalized_record "$file_name" local 90)" || fail "$round" fallback-local-finalized timeout no-row
    read -r local_id local_type <<<"$local_row"
    [[ "$local_type" == local ]] || fail "$round" fallback-local-storage mismatch unexpected
    download_record "$round" fallback-local-download-before "$local_id" "fallback-$round-local-before.mp4"

    wait_for_status_code 200 150 || fail "$round" fallback-recovery timeout not-ready
    assert_field s3 activeType "$round" fallback-recovered-active
    assert_field false degraded "$round" fallback-recovered-degraded
    assert_field false fallbackActive "$round" fallback-recovered-flag

    file_name="fallback-s3-$round.mp4"
    start_code="$(record_start_code "$file_name" || true)"
    assert_http_code 200 "$start_code" "$round" fallback-s3-start
    sleep 6
    stop_code="$(record_stop_code || true)"
    assert_http_code 200 "$stop_code" "$round" fallback-s3-stop
    s3_row="$(wait_for_finalized_record "$file_name" s3 90)" || fail "$round" fallback-s3-finalized timeout no-row
    read -r s3_id s3_type <<<"$s3_row"
    [[ "$s3_type" == s3 ]] || fail "$round" fallback-s3-storage mismatch unexpected
    download_record "$round" fallback-s3-download "$s3_id" "fallback-$round-s3.mp4"
    download_record "$round" fallback-local-download-after "$local_id" "fallback-$round-local-after.mp4"
}

export ALLOW_LOCAL_FALLBACK=false MINIO_DELAY_SECONDS=30
render_config 0
if ! compose build monibuca >/dev/null 2>&1; then
    fail 0 build-runtime compose build-failed
fi

for round in 1 2 3 4 5; do
    run_no_fallback_case "$round"
    run_explicit_fallback_case "$round"
done

printf 'PASS rounds=5 scenarios=required-s3,temporary-local-fallback\n'
