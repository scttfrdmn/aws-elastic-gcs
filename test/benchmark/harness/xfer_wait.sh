#!/bin/bash
# usage: xfer_wait.sh RUN_ID — block until the run logs its "done" line; print it
# Load deployment IDs (control node instances, collection IDs) for the bench pair.
# shellcheck source=/dev/null
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
RUN=$1
while :; do
  # The runner prints an "events" line after "done", so look for "done" rather than the last line.
  l=$("$(dirname "$0")"/ssm_run.sh "$SRC_CONTROL" "F=/opt/elastic-gcs/bench/runs/$RUN.jsonl; grep -m1 '\"event\": \"done\"' \$F || tail -1 \$F" | head -1)
  case "$l" in *'"event": "done"'*|*Traceback*|*Error*) echo "$l"; break;; esac
  sleep 20
done
