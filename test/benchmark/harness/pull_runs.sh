#!/bin/bash
# usage: pull_runs.sh OUT_DIR — copy every run log from the src control node into OUT_DIR (gzip + chunked over SSM)
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
OUT=$1; mkdir -p "$OUT"; H="$(dirname "$0")"
"$H"/ssm_run.sh $SRC_CONTROL 'cd /opt/elastic-gcs/bench/runs && tar czf /tmp/runs.tgz *.jsonl && base64 -w0 /tmp/runs.tgz > /tmp/runs.b64' >/dev/null
total=$("$H"/ssm_run.sh $SRC_CONTROL 'wc -c < /tmp/runs.b64' | head -1 | tr -d '[:space:]')
tmp=$(mktemp); off=0
while [ "$off" -lt "$total" ]; do
  "$H"/ssm_run.sh $SRC_CONTROL "tail -c +$((off+1)) /tmp/runs.b64 | head -c 20000" | head -1 | tr -d '\t\n ' >> "$tmp"
  off=$((off+20000))
done
base64 -d < "$tmp" | tar xzf - -C "$OUT" && rm -f "$tmp" && ls "$OUT"/*.jsonl | wc -l
