#!/bin/bash
# usage: scale.sh <src|dst> <N>  — set desired-dtns, time N slots serving and GCS active
# The slot controller (#65) converges to desired-dtns; last-active is refreshed so
# demand_monitor keeps the value for scale_in_idle instead of resetting it.
# Load deployment IDs (control node instances, collection IDs) for the bench pair.
# shellcheck source=/dev/null
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
D=$1; N=$2; R=$REGION; P=/elastic-gcs/$R/$D
t0=$(date +%s)
aws ssm put-parameter --region "$R" --name "$P/last-active" --value "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --type String --overwrite >/dev/null
aws ssm put-parameter --region "$R" --name "$P/desired-dtns" --value "$N" --type String --overwrite >/dev/null
while :; do
  # Serving: running with no elastic-gcs:phase tag (the controller clears starting/reviving once healthy).
  c=$(aws ec2 describe-instances --region "$R" \
    --filters "Name=tag:elastic-gcs:deployment,Values=$R-$D" Name=tag:elastic-gcs:pool,Values=slots Name=instance-state-name,Values=running \
    --query 'length(Reservations[].Instances[?!(Tags[?Key==`elastic-gcs:phase`])][])' --output text)
  [ "$c" -eq "$N" ] && break; [ $(( $(date +%s) - t0 )) -gt 900 ] && { echo "TIMEOUT serving=$c"; exit 1; }; sleep 5
done; t1=$(date +%s)
while :; do
  a=$("$(dirname "$0")"/gcs.sh "$D" 'globus-connect-server node list -F json | jq "[.[0].data[] | select(.status==\"active\")] | length"' | head -1)
  [ "$a" = "$((N+${CTL_ACTIVE:-0}))" ] && break; [ $(( $(date +%s) - t0 )) -gt 1200 ] && { echo "TIMEOUT active=$a"; exit 1; }; sleep 5
done; t2=$(date +%s)
echo "{\"deployment\":\"$D\",\"dtns\":$N,\"to_serving_s\":$((t1-t0)),\"to_gcs_active_s\":$((t2-t0))}"
