#!/bin/bash
# usage: netem.sh <delay_ms|off> — add one-way delay on dst DTNs for traffic to src DTN public IPs (RTT += delay); "off" clears it
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
H="$(dirname "$0")"; R=$REGION
ips() { for i in $(aws autoscaling describe-auto-scaling-groups --region $R --auto-scaling-group-names elastic-gcs-dtn-$R-$1 --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId' --output text); do echo "$i $(aws ec2 describe-instances --region $R --instance-ids $i --query 'Reservations[0].Instances[0].PublicIpAddress' --output text)"; done; }
SRC_IPS=$(ips src | awk '{print $2}' | tr '\n' ' ')
for i in $(ips dst | awk '{print $1}'); do
  if [ "$1" = off ]; then CMD='IF=$(ip route show default | awk "{print \$5; exit}"); tc qdisc del dev $IF root 2>/dev/null; tc qdisc show dev $IF | head -1'
  else CMD='IF=$(ip route show default | awk "{print \$5; exit}"); tc qdisc del dev $IF root 2>/dev/null; tc qdisc add dev $IF root handle 1: prio bands 4 && tc qdisc add dev $IF parent 1:4 handle 40: netem delay '"$1"'ms limit 100000 && for ip in '"$SRC_IPS"'; do tc filter add dev $IF protocol ip parent 1:0 prio 1 u32 match ip dst $ip/32 flowid 1:4; done; tc qdisc show dev $IF | grep netem'; fi
  echo "$i: $("$H"/ssm_run.sh $i "$CMD" 2>/dev/null | head -1)" &
done; wait
