#!/bin/bash
# =============================================================================
# Stop or start a deployment's control node to cut idle cost (~$55 -> ~$6/month).
#
# usage: AWS_PROFILE=aws scripts/control_node_power.sh <region> <deployment> stop|start|status|install
#
# stop:  refuses unless the DTN pool is at 0 (desired 0, nothing InService);
#        disables the control node's GCS node locally, waits for its IP to leave
#        the endpoint's DNS, then stops the instance. The endpoint is offline
#        (zero active nodes) until a DTN or the control node comes back.
# start: starts the instance, waits for SSM, enables the GCS node locally and
#        waits for its Elastic IP to reappear in endpoint DNS.
#
# install: put elastic-gcs-control.service (node enable on boot, disable on
#        shutdown; #30, #31) on a control node created before 0.12. Newer
#        control nodes get it from UserData. Safe to re-run.
#
# DTNs can still scale out while the control node is stopped: they enable
# themselves locally on resume (#72). Lambdas that target the control node
# (cache eviction, version check) fail until it is started again.
# =============================================================================
set -euo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
REGION=${1:?region}; DEP=${2:?deployment}; ACTION=${3:?stop|start|status|install}
P="/elastic-gcs/$REGION/$DEP"; ASG="elastic-gcs-dtn-$REGION-$DEP"
aws_() { aws --region "$REGION" "$@"; }

ID=$(aws_ ssm get-parameter --name "$P/control-node-id" --query Parameter.Value --output text)
EIP=$(aws_ ec2 describe-addresses --filters "Name=instance-id,Values=$ID" --query 'Addresses[0].PublicIp' --output text)
EP=$(aws_ ssm get-parameter --name "$P/endpoint-id" --query Parameter.Value --output text)
state() { aws_ ec2 describe-instances --instance-ids "$ID" --query 'Reservations[0].Instances[0].State.Name' --output text; }

# Run a root shell command on the control node via SSM; print stdout.
run() {
  local cid s
  cid=$(aws_ ssm send-command --instance-ids "$ID" --document-name AWS-RunShellScript \
        --parameters "$(python3 -I -c 'import json,sys; print(json.dumps({"commands":[sys.argv[1]],"executionTimeout":["300"]}))' "$1")" \
        --query Command.CommandId --output text)
  for _ in $(seq 1 100); do
    sleep 3
    s=$(aws_ ssm get-command-invocation --command-id "$cid" --instance-id "$ID" --query Status --output text 2>/dev/null || echo Pending)
    case "$s" in Success|Failed|TimedOut|Cancelled) break;; esac
  done
  aws_ ssm get-command-invocation --command-id "$cid" --instance-id "$ID" --query StandardOutputContent --output text
  [ "$s" = Success ]
}

# Endpoint DNS name from the node's info.json (cached locally once seen).
domain() { run "jq -r .domain_name /var/lib/globus-connect-server/info.json" | head -1 | tr -d '[:space:]'; }

wait_dns() {  # present|absent DOMAIN IP TIMEOUT
  local deadline=$(( $(date +%s) + $4 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if dig +short "$2" | grep -qx "$3"; then [ "$1" = present ] && return 0; else [ "$1" = absent ] && return 0; fi
    sleep 10
  done
  echo "WARNING: $3 still not $1 in $2 DNS after $4 s" >&2; return 1
}

case "$ACTION" in
status)
  echo "control node $ID ($EIP): $(state); endpoint $EP"
  aws_ autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$ASG" \
    --query 'AutoScalingGroups[0].{desired:DesiredCapacity,in_service:length(Instances[?LifecycleState==`InService`])}' --output text \
    | awk '{print "DTN pool: desired=" $1 " in_service=" $2}'
  ;;
stop)
  [ "$(state)" = running ] || { echo "control node is $(state); nothing to do"; exit 0; }
  read -r desired inservice < <(aws_ autoscaling describe-auto-scaling-groups --auto-scaling-group-names "$ASG" \
    --query 'AutoScalingGroups[0].[DesiredCapacity,length(Instances[?LifecycleState==`InService`])]' --output text)
  if [ "$desired" != 0 ] || [ "$inservice" != 0 ]; then
    echo "refusing: DTN pool not idle (desired=$desired in_service=$inservice)"; exit 1
  fi
  D=$(domain); echo "disabling control node GCS node ($EIP) on $D"
  run "globus-connect-server node disable 2>&1 | tail -1; chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null; true"
  wait_dns absent "$D" "$EIP" 300 || true
  aws_ ec2 stop-instances --instance-ids "$ID" --query 'StoppingInstances[0].CurrentState.Name' --output text
  aws_ ec2 wait instance-stopped --instance-ids "$ID" && echo "stopped $ID"
  ;;
start)
  if [ "$(state)" != running ]; then
    aws_ ec2 start-instances --instance-ids "$ID" --query 'StartingInstances[0].CurrentState.Name' --output text
    aws_ ec2 wait instance-running --instance-ids "$ID"
  fi
  for _ in $(seq 1 60); do
    [ "$(aws_ ssm describe-instance-information --filters "Key=InstanceIds,Values=$ID" --query 'InstanceInformationList[0].PingStatus' --output text)" = Online ] && break
    sleep 5
  done
  D=$(domain); echo "enabling control node GCS node ($EIP) on $D"
  run "globus-connect-server node enable 2>&1 | tail -1; chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null; true"
  wait_dns present "$D" "$EIP" 300 && echo "endpoint $EP online via $EIP"
  ;;
install)
  [ "$(state)" = running ] || { echo "control node is $(state); start it first"; exit 1; }
  B=$(base64 < "$HERE/userdata/control_node_boot.sh" | tr -d '\n')
  U=$(base64 < "$HERE/userdata/elastic-gcs-control.service" | tr -d '\n')
  run "set -e; mkdir -p /opt/elastic-gcs/scripts
echo $B | base64 -d > /opt/elastic-gcs/scripts/control_node_boot.sh; chmod 0755 /opt/elastic-gcs/scripts/control_node_boot.sh
echo $U | base64 -d > /etc/systemd/system/elastic-gcs-control.service; chmod 0644 /etc/systemd/system/elastic-gcs-control.service
systemctl daemon-reload; systemctl enable elastic-gcs-control 2>&1 | tail -1
systemctl start elastic-gcs-control; systemctl is-active elastic-gcs-control"
  ;;
*) echo "unknown action $ACTION"; exit 2;;
esac
