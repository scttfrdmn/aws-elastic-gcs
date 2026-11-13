#!/bin/bash
# usage: xfer_bg.sh RUN_ID LABEL ITEM [ITEM...] — start runner detached on the src control node; log /opt/elastic-gcs/bench/runs/RUN_ID.jsonl
# Load deployment IDs (control node instances, collection IDs) for the bench pair.
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
RUN=$1; LABEL=$2; shift 2; ARGS=""; for a in "$@"; do ARGS="$ARGS '$a'"; done
"$(dirname "$0")"/ssm_run.sh $SRC_CONTROL "mkdir -p /opt/elastic-gcs/bench/runs; export GLOBUS_CLIENT_ID=\$(aws secretsmanager get-secret-value --secret-id elastic-gcs/$REGION/src/globus-client-id --query SecretString --output text --region $REGION); export GLOBUS_CLIENT_SECRET=\$(aws secretsmanager get-secret-value --secret-id elastic-gcs/$REGION/src/globus-client-secret --query SecretString --output text --region $REGION); nohup /opt/elastic-gcs/bench/venv/bin/python /opt/elastic-gcs/bench/bench_transfer.py $SRC_COL $DST_COL '$LABEL' $ARGS > /opt/elastic-gcs/bench/runs/$RUN.jsonl 2>&1 & sleep 8; head -1 /opt/elastic-gcs/bench/runs/$RUN.jsonl"
