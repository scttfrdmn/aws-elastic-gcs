#!/bin/bash
set -euo pipefail

# =============================================================================
# DTN Boot Script (handles both first boot and resume from stopped)
# =============================================================================
# This script runs on every boot of a DTN instance via a systemd unit.
# It detects whether this is a first boot or a resume and acts accordingly.
#
# First boot: full node setup with --import-node (cold path, 2-3 min)
# Resume:     just node enable               (warm path, 30-60s)
#
# EIP assignment uses tag-based discovery — the script finds an unassociated
# EIP tagged for this deployment and claims it atomically. This allows a single
# ASG launch template to work for all DTN slots without per-instance config.
#
# Required environment variables (injected by systemd unit):
#   AWS_REGION      — AWS region (e.g. us-west-2)
#   DEPLOYMENT      — deployment name within the region (e.g. src)
#   SECRETS_PREFIX  — Secrets Manager prefix (e.g. elastic-gcs/us-west-2/src)
#   PARAM_PREFIX    — SSM Parameter Store prefix (e.g. /elastic-gcs/us-west-2/src)
#   TOTAL_SLOTS     — number of pre-created node identity slots
# =============================================================================

LOG="/var/log/elastic-gcs-dtn.log"
exec > >(tee -a "$LOG") 2>&1

TIMESTAMP=$(date -u +"%Y-%m-%d %H:%M:%S UTC")
echo ""
echo "============================================"
echo "=== DTN Boot Script - $TIMESTAMP ==="
echo "============================================"

# --- Helper: get secret from Secrets Manager ---
get_secret() {
    aws secretsmanager get-secret-value \
        --secret-id "$1" \
        --query SecretString \
        --output text \
        --region "$AWS_REGION"
}

# --- Helper: get instance metadata (IMDSv2) ---
get_metadata() {
    TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
    curl -s -H "X-aws-ec2-metadata-token: $TOKEN" \
        "http://169.254.169.254/latest/meta-data/$1"
}

INSTANCE_ID=$(get_metadata "instance-id")
echo "Instance ID: $INSTANCE_ID"

# --- SSM-based slot claim ---
# Claim a DTN slot by atomically writing an SSM parameter. PutParameter without
# --overwrite fails with ParameterAlreadyExists if the slot is taken, making it
# atomic across concurrent boot attempts (same guarantee as EC2 AssociateAddress).
#
# On service restart on the same instance, the lock already has our instance ID —
# detect and reuse it rather than trying to claim a new slot.
#
# Slot number is saved to /var/lib/elastic-gcs/slot for the shutdown script.
echo ">>> Claiming DTN slot via SSM Parameter Store..."

SLOT_LOCK_PREFIX="${PARAM_PREFIX}/slot-lock"
DTN_SLOT_NUMBER=""

# First: check if this instance already owns a slot (handles service restart)
for slot in $(seq 1 "$TOTAL_SLOTS"); do
    OWNER=$(aws ssm get-parameter \
        --name "${SLOT_LOCK_PREFIX}/${slot}" \
        --query 'Parameter.Value' --output text \
        --region "$AWS_REGION" 2>/dev/null || echo "")
    if [ "$OWNER" = "$INSTANCE_ID" ]; then
        DTN_SLOT_NUMBER="$slot"
        echo ">>> Reusing existing slot lock: slot $DTN_SLOT_NUMBER"
        break
    fi
done

# If no existing lock found, claim an available slot
if [ -z "$DTN_SLOT_NUMBER" ]; then
    for slot in $(seq 1 "$TOTAL_SLOTS"); do
        echo ">>> Trying to claim slot $slot..."
        if aws ssm put-parameter \
            --name "${SLOT_LOCK_PREFIX}/${slot}" \
            --value "$INSTANCE_ID" \
            --type String \
            --region "$AWS_REGION" 2>/dev/null; then
            DTN_SLOT_NUMBER="$slot"
            echo ">>> Claimed slot $DTN_SLOT_NUMBER"
            break
        fi
    done
fi

if [ -z "$DTN_SLOT_NUMBER" ]; then
    echo "ERROR: Could not claim any DTN slot (tried 1..$TOTAL_SLOTS). Check slot locks in SSM."
    exit 1
fi

# Persist slot number for the shutdown script
mkdir -p /var/lib/elastic-gcs
echo "$DTN_SLOT_NUMBER" > /var/lib/elastic-gcs/slot

# Tag the instance with its slot number (used by cache eviction Lambda for cleanup)
aws ec2 create-tags \
    --resources "$INSTANCE_ID" \
    --tags "Key=elastic-gcs:slot,Value=${DTN_SLOT_NUMBER}" \
    --region "$AWS_REGION" 2>/dev/null || true

# Get public IP from IMDS. The ephemeral public IP is assigned at instance start
# and is stable within a second or two. A brief sleep ensures it is populated.
sleep 2
PUBLIC_IP=$(get_metadata "public-ipv4")
echo ">>> Public IP: $PUBLIC_IP (slot $DTN_SLOT_NUMBER)"

# --- Detect: first boot vs resume ---
GCS_STATE_DIR="/var/lib/globus-connect-server"

if [ -d "$GCS_STATE_DIR" ] && [ -s "$GCS_STATE_DIR/info.json" ]; then
    # =================================================================
    # RESUME PATH (fast — node already configured, just re-enable)
    # GCS state directory exists from a previous boot. The node identity,
    # certificates, and endpoint membership are on disk. Just re-enable.
    # =================================================================
    echo ">>> RESUME PATH: GCS state exists, re-enabling node..."
    BOOT_TYPE="resume"
    RESUME_START=$(date +%s%N)

    systemctl start globus-connect-server 2>/dev/null || true
    sleep 3

    # node update requires GCS CLI auth (client credentials).
    # The endpoint ID comes from the local info.json (always accurate).
    ENDPOINT_ID=$(jq -r '.endpoint_id' "$GCS_STATE_DIR/info.json")
    export GCS_CLI_CLIENT_ID=$(get_secret "${SECRETS_PREFIX}/globus-client-id")
    export GCS_CLI_CLIENT_SECRET=$(get_secret "${SECRETS_PREFIX}/globus-client-secret")
    export GCS_CLI_ENDPOINT_ID="$ENDPOINT_ID"

    # Use node update --ip-address to both correct the IP (changes every
    # resume since we no longer use EIPs) and enable the node in one step.
    # The node ID is read from the persistent file written at first boot,
    # ensuring we always update the correct node regardless of slot claim order.
    NODE_ID_FILE="/var/lib/elastic-gcs/node-id"
    if [ -s "$NODE_ID_FILE" ]; then
        NODE_ID=$(cat "$NODE_ID_FILE")
    else
        # Fallback: derive node_id from the claimed slot's Secrets Manager entry.
        # This can mismatch if the instance claims a different slot than first boot;
        # see GitHub issue #16. The node-id file prevents this on subsequent resumes.
        NODE_ID=$(get_secret "${SECRETS_PREFIX}/node-slot-${DTN_SLOT_NUMBER}" | jq -r '.node_id')
        echo "WARNING: node-id file missing, falling back to slot $DTN_SLOT_NUMBER node_id"
    fi

    echo ">>> Running: globus-connect-server node update $NODE_ID --ip-address $PUBLIC_IP --enable"
    globus-connect-server node update "$NODE_ID" \
        --ip-address "$PUBLIC_IP" \
        --enable

    # Fix lock file ownership: GCS CLI runs as root, service runs as gcsweb.
    chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null || true

    RESUME_END=$(date +%s%N)
    RESUME_MS=$(( (RESUME_END - RESUME_START) / 1000000 ))
    echo ">>> Node re-enabled in ${RESUME_MS}ms"

else
    # =================================================================
    # FIRST BOOT PATH (slower — need full node setup with --import-node)
    # No GCS state found. Retrieve the pre-created node identity from
    # Secrets Manager and run node setup.
    # =================================================================
    echo ">>> FIRST BOOT PATH: no GCS state found, running node setup..."
    BOOT_TYPE="first_boot"
    SETUP_START=$(date +%s%N)

    DEPLOY_KEY_FILE="/tmp/deployment-key.json"
    NODE_IDENTITY_FILE="/tmp/node-slot-${DTN_SLOT_NUMBER}.json"

    echo ">>> Retrieving deployment key from Secrets Manager..."
    get_secret "${SECRETS_PREFIX}/deployment-key" > "$DEPLOY_KEY_FILE"

    echo ">>> Retrieving node identity for slot $DTN_SLOT_NUMBER..."
    get_secret "${SECRETS_PREFIX}/node-slot-${DTN_SLOT_NUMBER}" > "$NODE_IDENTITY_FILE"

    # Persist the node_id so the resume path can find it regardless of
    # which slot this instance claims on a subsequent boot (fixes issue #16).
    NODE_ID=$(jq -r '.node_id' "$NODE_IDENTITY_FILE")
    mkdir -p /var/lib/elastic-gcs
    echo "$NODE_ID" > /var/lib/elastic-gcs/node-id

    echo ">>> Running: globus-connect-server node setup --import-node (may take several minutes)"
    globus-connect-server node setup \
        --deployment-key "$DEPLOY_KEY_FILE" \
        --import-node "$NODE_IDENTITY_FILE" \
        --ip-address "$PUBLIC_IP"

    rm -f "$DEPLOY_KEY_FILE" "$NODE_IDENTITY_FILE"

    # Fix lock file ownership: GCS CLI runs as root, service runs as gcsweb.
    chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null || true

    SETUP_END=$(date +%s%N)
    SETUP_MS=$(( (SETUP_END - SETUP_START) / 1000000 ))
    echo ">>> Node setup completed in ${SETUP_MS}ms"
fi

# Tag instance with GCS node ID so cache eviction Lambda can clean up stale
# node registrations when this instance is terminated (issue #9).
aws ec2 create-tags \
    --resources "$INSTANCE_ID" \
    --tags "Key=elastic-gcs:gcs-node-id,Value=${NODE_ID}" \
    --region "$AWS_REGION" 2>/dev/null || true

# --- Verify node is healthy ---
echo ">>> Verifying node health..."
sleep 5
globus-connect-server self-diagnostic 2>&1 | head -20 || true
NODE_STATUS=$(globus-connect-server node show -F json 2>/dev/null | jq -r '.status' || echo "unknown")
echo ">>> Node status: $NODE_STATUS"

# --- Publish timing metrics to CloudWatch ---
if [ "$BOOT_TYPE" = "resume" ]; then
    METRIC_VALUE="$RESUME_MS"
    METRIC_NAME="DTNResumeLatencyMs"
else
    METRIC_VALUE="$SETUP_MS"
    METRIC_NAME="DTNFirstBootLatencyMs"
fi

aws cloudwatch put-metric-data \
    --namespace "ElasticGCS" \
    --metric-name "$METRIC_NAME" \
    --value "$METRIC_VALUE" \
    --unit Milliseconds \
    --dimensions "Region=$AWS_REGION,Deployment=$DEPLOYMENT,Slot=$DTN_SLOT_NUMBER" \
    --region "$AWS_REGION" 2>/dev/null \
    || echo "WARNING: Failed to publish CloudWatch metric"

# --- Signal ASG lifecycle hook completion ---
# The boot script signals directly rather than relying on the lifecycle Lambda,
# giving the tightest possible scale-out latency. The Lambda is a fallback.
echo ">>> Checking for ASG lifecycle hook..."
LIFECYCLE_STATE=$(aws autoscaling describe-auto-scaling-instances \
    --instance-ids "$INSTANCE_ID" \
    --region "$AWS_REGION" \
    --query 'AutoScalingInstances[0].LifecycleState' \
    --output text 2>/dev/null || echo "unknown")

if [[ "$LIFECYCLE_STATE" == *"Pending:Wait"* ]] || [[ "$LIFECYCLE_STATE" == *"Warmed:Pending:Wait"* ]]; then
    ASG_NAME=$(aws autoscaling describe-auto-scaling-instances \
        --instance-ids "$INSTANCE_ID" \
        --region "$AWS_REGION" \
        --query 'AutoScalingInstances[0].AutoScalingGroupName' \
        --output text)

    echo ">>> Completing lifecycle action CONTINUE for ASG: $ASG_NAME"
    aws autoscaling complete-lifecycle-action \
        --lifecycle-hook-name "dtn-ready-hook" \
        --auto-scaling-group-name "$ASG_NAME" \
        --instance-id "$INSTANCE_ID" \
        --lifecycle-action-result CONTINUE \
        --region "$AWS_REGION" 2>/dev/null \
        || echo "WARNING: Failed to complete lifecycle action"
fi

echo ""
echo "=== DTN Boot Complete ==="
echo "Boot type:   $BOOT_TYPE"
echo "Node status: $NODE_STATUS"
echo "Public IP:   $PUBLIC_IP"
echo "DTN Slot:    $DTN_SLOT_NUMBER"
echo "Instance:    $INSTANCE_ID"
echo "========================="
