#!/bin/bash
# usage: gcs.sh <src|dst> <gcs cli args...> — runs globus-connect-server on that deployment's control node
# Load deployment IDs (control node instances, collection IDs) for the bench pair.
. "${BENCH_ENV:-$(dirname "$0")/bench.env}"
D=$1; shift
case $D in src) I=$SRC_CONTROL;; dst) I=$DST_CONTROL;; *) echo "bad deployment"; exit 2;; esac
P=elastic-gcs/$REGION/$D
exec "$(dirname "$0")"/ssm_run.sh $I "export GCS_CLI_CLIENT_ID=\$(aws secretsmanager get-secret-value --secret-id $P/globus-client-id --query SecretString --output text --region $REGION); export GCS_CLI_CLIENT_SECRET=\$(aws secretsmanager get-secret-value --secret-id $P/globus-client-secret --query SecretString --output text --region $REGION); export GCS_CLI_ENDPOINT_ID=\$(aws ssm get-parameter --name /$P/endpoint-id --query Parameter.Value --output text --region $REGION); $*"
