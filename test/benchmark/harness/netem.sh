#!/bin/bash
# usage: netem.sh <delay_ms|off> [loss_pct] [on]
# Adds one-way delay (and optional random loss) with tc netem on the running DTN
# slots of deployment <on> (default dst), only for traffic to the other
# deployment's DTN public IPs, so S3 traffic is unaffected. RTT grows by delay_ms.
# For the data direction, pass the sending side as <on> (src for src -> dst
# transfers): the delay and loss then hit data segments, not ACKs (#145, #151).
# "off" clears the qdisc. Prints the netem qdisc per instance as verification.
# shellcheck source=/dev/null
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
H="$(dirname "$0")"; R=$REGION
DELAY=$1; LOSS=${2:-0}; ON=${3:-dst}
case $ON in src) PEER=dst;; dst) PEER=src;; *) echo "bad side $ON"; exit 2;; esac
# Running DTN slots of a deployment: "<instance-id> <public-ip>" per line.
ips() { aws ec2 describe-instances --region "$R" --filters "Name=tag:elastic-gcs:deployment,Values=$R-$1" Name=tag:elastic-gcs:pool,Values=slots Name=instance-state-name,Values=running --query 'Reservations[].Instances[].[InstanceId,PublicIpAddress]' --output text; }
PEER_IPS=$(ips "$PEER" | awk '{print $2}' | tr '\n' ' ')
LOSS_ARG=""; if [ "$LOSS" != 0 ]; then LOSS_ARG="loss ${LOSS}%"; fi
for i in $(ips "$ON" | awk '{print $1}'); do
  # shellcheck disable=SC2016  # $IF and $ip expand on the instance
  if [ "$DELAY" = off ]; then CMD='IF=$(ip route show default | awk "{print \$5; exit}"); tc qdisc del dev $IF root 2>/dev/null; tc qdisc show dev $IF | head -1'
  else CMD='IF=$(ip route show default | awk "{print \$5; exit}"); tc qdisc del dev $IF root 2>/dev/null; tc qdisc add dev $IF root handle 1: prio bands 4 && tc qdisc add dev $IF parent 1:4 handle 40: netem delay '"$DELAY"'ms '"$LOSS_ARG"' limit 100000 && for ip in '"$PEER_IPS"'; do tc filter add dev $IF protocol ip parent 1:0 prio 1 u32 match ip dst $ip/32 flowid 1:4; done; tc qdisc show dev $IF | grep netem'; fi
  echo "$i: $("$H"/ssm_run.sh "$i" "$CMD" 2>/dev/null | head -1)" &
done; wait
