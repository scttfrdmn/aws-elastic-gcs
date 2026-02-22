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
#   AWS_REGION      — AWS region (e.g. us-east-1)
#   SECRETS_PREFIX  — Secrets Manager prefix (e.g. elastic-gcs/us-east-1)
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

# --- Tag-based EIP discovery and association ---
# EIPs are tagged with elastic-gcs:role=dtn, elastic-gcs:slot=N, elastic-gcs:region=<region>.
# On first attempt: find unassociated EIP and claim it (atomic via AssociateAddress).
# On retry (service restart on same instance): EIP is already associated — detect and reuse.
# This handles concurrent boot scenarios gracefully (EC2 AssociateAddress is atomic).
echo ">>> Discovering available Elastic IP for slot assignment..."

EIP_ALLOCATION_ID=""
DTN_SLOT_NUMBER=""

# First: check if this instance already has an EIP assigned (handles service restart)
EXISTING=$(aws ec2 describe-addresses \
    --filters \
        "Name=tag:elastic-gcs:region,Values=${AWS_REGION}" \
        "Name=tag:elastic-gcs:role,Values=dtn" \
        "Name=instance-id,Values=${INSTANCE_ID}" \
    --query 'Addresses[*].[AllocationId, Tags[?Key==`elastic-gcs:slot`].Value|[0]]' \
    --output text \
    --region "$AWS_REGION" 2>/dev/null || echo "")

if [ -n "$EXISTING" ]; then
    EIP_ALLOCATION_ID=$(echo "$EXISTING" | head -1 | cut -f1)
    DTN_SLOT_NUMBER=$(echo "$EXISTING" | head -1 | cut -f2)
    echo ">>> Reusing existing EIP $EIP_ALLOCATION_ID (slot $DTN_SLOT_NUMBER)"
else
    # No EIP on this instance yet — find and claim an unassociated one
    AVAILABLE_EIPS=$(aws ec2 describe-addresses \
        --filters \
            "Name=tag:elastic-gcs:region,Values=${AWS_REGION}" \
            "Name=tag:elastic-gcs:role,Values=dtn" \
        --query 'Addresses[?AssociationId==null].[AllocationId, Tags[?Key==`elastic-gcs:slot`].Value|[0]]' \
        --output text \
        --region "$AWS_REGION" 2>/dev/null || echo "")

    while IFS=$'\t' read -r alloc_id slot_num; do
        [ -z "$alloc_id" ] || [ -z "$slot_num" ] && continue
        echo ">>> Trying to associate EIP $alloc_id (slot $slot_num)..."
        if aws ec2 associate-address \
            --instance-id "$INSTANCE_ID" \
            --allocation-id "$alloc_id" \
            --region "$AWS_REGION" 2>/dev/null; then
            EIP_ALLOCATION_ID="$alloc_id"
            DTN_SLOT_NUMBER="$slot_num"
            echo ">>> Associated EIP $EIP_ALLOCATION_ID as slot $DTN_SLOT_NUMBER"
            break
        fi
    done <<< "$AVAILABLE_EIPS"
fi

if [ -z "$EIP_ALLOCATION_ID" ]; then
    echo "ERROR: Could not associate any Elastic IP. Check EIP allocation and tags."
    echo "Expected tags: elastic-gcs:region=${AWS_REGION}, elastic-gcs:role=dtn"
    exit 1
fi

# Read the public IP directly from the EIP allocation record (authoritative).
# Do NOT use IMDS here: IMDS can return the pre-EIP address for several seconds
# after aws ec2 associate-address returns, which would cause node setup to
# register the wrong IP in GCS. describe-addresses is always consistent.
PUBLIC_IP=$(aws ec2 describe-addresses \
    --allocation-ids "$EIP_ALLOCATION_ID" \
    --query 'Addresses[0].PublicIp' \
    --output text \
    --region "$AWS_REGION")
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

    # Use node update --ip-address to both correct the IP (if the EIP's public
    # address has changed since first boot) and enable the node in one step.
    # The node ID is read from the Secrets Manager identity file for this slot —
    # the same JSON that was imported during first boot, so it's always accurate.
    NODE_ID=$(get_secret "${SECRETS_PREFIX}/node-slot-${DTN_SLOT_NUMBER}" | jq -r '.node_id')
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
    --dimensions "Region=$AWS_REGION,Slot=$DTN_SLOT_NUMBER" \
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
