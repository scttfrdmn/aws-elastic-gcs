#!/bin/bash
# usage: scale.sh <src|dst> <N>  — set DTN desired capacity, time ASG InService and GCS active
# Load deployment IDs (control node instances, collection IDs) for the bench pair.
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
D=$1; N=$2; A=elastic-gcs-dtn-$REGION-$D; R=$REGION
t0=$(date +%s)
aws autoscaling set-desired-capacity --region $R --auto-scaling-group-name $A --desired-capacity $N --no-honor-cooldown
while :; do
  c=$(aws autoscaling describe-auto-scaling-groups --region $R --auto-scaling-group-names $A --query 'length(AutoScalingGroups[0].Instances[?LifecycleState==`InService`])' --output text)
  [ "$c" -eq "$N" ] && break; [ $(( $(date +%s) - t0 )) -gt 900 ] && { echo "TIMEOUT inservice=$c"; exit 1; }; sleep 5
done; t1=$(date +%s)
while :; do
  a=$("$(dirname "$0")"/gcs.sh $D 'globus-connect-server node list -F json | jq "[.[0].data[] | select(.status==\"active\")] | length"' | head -1)
  [ "$a" = "$((N+${CTL_ACTIVE:-0}))" ] && break; [ $(( $(date +%s) - t0 )) -gt 1200 ] && { echo "TIMEOUT active=$a"; exit 1; }; sleep 5
done; t2=$(date +%s)
echo "{\"deployment\":\"$D\",\"dtns\":$N,\"to_inservice_s\":$((t1-t0)),\"to_gcs_active_s\":$((t2-t0))}"
