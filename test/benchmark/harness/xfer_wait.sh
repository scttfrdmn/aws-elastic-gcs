#!/bin/bash
# usage: xfer_wait.sh RUN_ID — block until the run logs its "done" line; print it
# Load deployment IDs (control node instances, collection IDs) for the bench pair.
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
RUN=$1
while :; do
  l=$("$(dirname "$0")"/ssm_run.sh $SRC_CONTROL "tail -1 /opt/elastic-gcs/bench/runs/$RUN.jsonl" | head -1)
  case "$l" in *'"event": "done"'*|*Traceback*|*Error*) echo "$l"; break;; esac
  sleep 20
done
