#!/bin/bash
# usage: netsnap.sh <src|dst> — rx/tx bytes + cpu jiffies for control node and InService DTNs, sampled in parallel; each line carries its own epoch
# Load deployment IDs (control node instances, collection IDs) for the bench pair.
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
D=$1; R=$REGION
case $D in src) C=$SRC_CONTROL;; dst) C=$DST_CONTROL;; esac
ids="$C $(aws autoscaling describe-auto-scaling-groups --region $R --auto-scaling-group-names elastic-gcs-dtn-$REGION-$D --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId' --output text)"
for i in $ids; do
  ( r=$("$(dirname "$0")"/ssm_run.sh $i 'IF=$(ip route show default | awk "{print \$5; exit}"); echo "$(cat /sys/class/net/$IF/statistics/rx_bytes) $(cat /sys/class/net/$IF/statistics/tx_bytes) $(awk "/^cpu /{print \$2+\$3+\$4+\$6+\$7+\$8, \$2+\$3+\$4+\$5+\$6+\$7+\$8}" /proc/stat) $(nproc) $(date +%s.%N)"' | head -1); echo "$D $i $r" ) &
done; wait
