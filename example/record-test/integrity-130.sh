#!/usr/bin/env bash
# integrity-130.sh - 对 130 上最近的 N 路 verify130 录像做完整性验证
# 用法: ./integrity-130.sh [N]   N 默认 3
#
# 做的事:
#   1. 找最近 N 条 verify130/* mp4 record (mp4 list API)
#   2. 在 xde-monibuca 容器内 ffprobe 本地 mp4 (检 duration/frames/codec)
#   3. md5sum + stat -c %s 比对本地文件 与 MinIO 物理桶文件
#   4. 输出表格

set -u
N="${1:-3}"
CONTAINER="${CONTAINER:-xde-monibuca}"
MINIO_CONTAINER="${MINIO_CONTAINER:-xde-minio}"
BUCKET="${BUCKET:-vidu-media-bucket}"
LOCAL_BASE="${LOCAL_BASE:-/monibuca/record}"
MINIO_BASE="${MINIO_BASE:-/data/$BUCKET}"
API_BASE="${API_BASE:-http://localhost:7080}"

ssh_do() { ssh -o ConnectTimeout=10 root@172.16.12.130 "$1"; }

echo "=== 找最近 $N 条 verify130 record ==="
records=$(ssh_do "curl -s '$API_BASE/mp4/api/list?pageNum=1&pageSize=$N' 2>/dev/null" | python3 -c "
import sys, json
d = json.load(sys.stdin)
for r in d.get('data', [])[:$N]:
    fp = r.get('filePath', '')
    if 'verify130' in fp:
        print(f\"{r['id']}|{fp}|{r['startTime']}|{r['endTime']}\")
")
echo "$records"
echo

printf '%-4s %-50s %-12s %-12s %-12s %-10s %s\n' ID FILE_PATH SIZE_LOCAL SIZE_MINIO MD5_OK FFPROBE NOTE
for line in $records; do
    id=$(echo "$line" | cut -d'|' -f1)
    fp=$(echo "$line" | cut -d'|' -f2)

    # 本地 mp4 (容器内 /monibuca/record/<fp>)
    sz_local=$(ssh_do "docker exec $CONTAINER stat -c %s '$LOCAL_BASE/$fp' 2>/dev/null" | tr -d '\r\n ')
    md5_local=$(ssh_do "docker exec $CONTAINER md5sum '$LOCAL_BASE/$fp' 2>/dev/null | awk '{print \$1}'" | tr -d '\r\n ')

    # MinIO 物理文件 (宿主 /data/minio/<bucket>/<fp>)
    sz_minio=$(ssh_do "docker exec $MINIO_CONTAINER sh -c 'stat -c %s $MINIO_BASE/$fp 2>/dev/null'" | tr -d '\r\n ')
    md5_minio=$(ssh_do "docker exec $MINIO_CONTAINER sh -c 'md5sum $MINIO_BASE/$fp 2>/dev/null | awk \"{print \\\$1}\"'" | tr -d '\r\n ')

    # ffprobe 本地
    probe=$(ssh_do "docker exec $CONTAINER ffprobe -v error -show_entries format=duration,size:stream=codec_name,nb_frames,width,height -of default=nw=1 '$LOCAL_BASE/$fp' 2>&1" | tr '\n' ';')
    duration=$(echo "$probe" | grep -oP 'duration=\K[0-9.]+' | head -1)
    nb_frames=$(echo "$probe" | grep -oP 'nb_frames=\K[0-9]+' | head -1)
    ffprobe_err=$(echo "$probe" | grep -iE 'error|invalid|moov' | head -1)

    md5_ok="?"
    [ -n "$md5_local" ] && [ "$md5_local" = "$md5_minio" ] && md5_ok="YES" || md5_ok="NO"
    [ -z "$md5_local" ] && md5_ok="NO-LOCAL"
    [ -z "$md5_minio" ] && md5_ok="NO-MINIO"

    ffprobe_status="OK"
    [ -n "$ffprobe_err" ] && ffprobe_status="ERR"
    [ -z "$duration" ] && ffprobe_status="NO-DUR"

    note="dur=${duration:-?}s frm=${nb_frames:-?}"
    [ -n "$ffprobe_err" ] && note="$note ERR=$ffprobe_err"

    printf '%-4s %-50s %-12s %-12s %-12s %-10s %s\n' \
      "$id" "$fp" "${sz_local:-?}" "${sz_minio:-?}" "$md5_ok" "$ffprobe_status" "$note"
done
