#!/bin/bash
# usage: ssm_run.sh <instance-id> <command...>  — runs as root via SSM, prints stdout/stderr
set -euo pipefail
I=$1; shift
CMD_JSON=$(python3 -I -c 'import json,sys; print(json.dumps({"commands":[sys.argv[1]],"executionTimeout":["300"]}))' "$*")
ID=$(aws ssm send-command --region ${REGION:-us-west-2} --instance-ids "$I" --document-name AWS-RunShellScript --parameters "$CMD_JSON" --query Command.CommandId --output text)
for _ in $(seq 1 100); do
  sleep 3
  S=$(aws ssm get-command-invocation --region ${REGION:-us-west-2} --command-id "$ID" --instance-id "$I" --query Status --output text 2>/dev/null || echo Pending)
  case "$S" in Success|Failed|TimedOut|Cancelled) break;; esac
done
aws ssm get-command-invocation --region ${REGION:-us-west-2} --command-id "$ID" --instance-id "$I" --query '[StandardOutputContent,StandardErrorContent]' --output text
