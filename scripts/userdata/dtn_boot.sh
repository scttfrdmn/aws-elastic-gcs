#!/bin/bash
set -euo pipefail

# =============================================================================
# DTN Boot Script (handles both first boot and resume from stopped)
# =============================================================================
# This script runs on every boot of a DTN instance via a systemd unit.
# It detects whether this is a first boot or a resume and acts accordingly.
#
# First boot: claim a slot (SSM lock), node setup --import-node, serialized
#             across the deployment by an SSM setup lock.
# Resume:     local node enable + node update --ip-address (the public IP
#             changes on every start).
#
# Before signalling the ASG launch hook:
#   - going InService: wait until the node's IP is in endpoint DNS;
#   - going into the warm pool (first boot only): disable the node and wait
#     until its IP is out of DNS, so a node about to be stopped never serves
#     transfers (#69).
# Any failure signals ABANDON at once, so the ASG replaces the instance in
# seconds instead of waiting out the 15-minute hook timeout (#34).
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

# --- Helper: wait until the endpoint's DNS name does (present) or doesn't
# (absent) resolve to an IP. Globus updates DNS asynchronously after a node
# changes state; acting before it does leaves dead IPs in DNS (#69).
wait_endpoint_dns() {  # usage: wait_endpoint_dns present|absent IP TIMEOUT_SEC
    local want=$1 ip=$2 deadline=$(( $(date +%s) + $3 )) domain
    domain=$(jq -r '.domain_name // empty' /var/lib/globus-connect-server/info.json 2>/dev/null)
    [ -n "$domain" ] || { echo "WARNING: no endpoint domain in info.json; skipping DNS wait"; return 0; }
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if getent ahostsv4 "$domain" | awk '{print $1}' | grep -qx "$ip"; then
            [ "$want" = present ] && { echo ">>> $ip is in $domain DNS"; return 0; }
        else
            [ "$want" = absent ] && { echo ">>> $ip is out of $domain DNS"; return 0; }
        fi
        sleep 5
    done
    echo "WARNING: $ip still not $want in $domain DNS after ${3}s"
    return 1
}

# --- Helpers: deployment-wide first-boot setup lock (SSM parameter) ---
SETUP_LOCK="${PARAM_PREFIX}/setup-lock"
SETUP_LOCK_STALE_SEC=300
SETUP_LOCK_HELD=false
acquire_setup_lock() {
    local deadline=$(( $(date +%s) + 900 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if aws ssm put-parameter --name "$SETUP_LOCK" --value "$INSTANCE_ID" \
            --type String --region "$AWS_REGION" >/dev/null 2>&1; then
            SETUP_LOCK_HELD=true
            echo ">>> Acquired setup lock"
            return 0
        fi
        local modified age
        modified=$(aws ssm get-parameter --name "$SETUP_LOCK" --region "$AWS_REGION" \
            --query 'Parameter.LastModifiedDate' --output text 2>/dev/null || echo "")
        if [ -n "$modified" ]; then
            age=$(( $(date +%s) - $(date -d "$modified" +%s 2>/dev/null || date +%s) ))
            if [ "$age" -gt "$SETUP_LOCK_STALE_SEC" ]; then
                echo ">>> Setup lock is ${age}s old; treating it as abandoned"
                aws ssm delete-parameter --name "$SETUP_LOCK" --region "$AWS_REGION" >/dev/null 2>&1 || true
                continue
            fi
        fi
        sleep $(( 3 + RANDOM % 5 ))
    done
    echo "WARNING: setup lock not acquired after 15 min; proceeding without it"
}
release_setup_lock() {
    [ "$SETUP_LOCK_HELD" = true ] || return 0
    local owner
    owner=$(aws ssm get-parameter --name "$SETUP_LOCK" --region "$AWS_REGION" \
        --query Parameter.Value --output text 2>/dev/null || echo "")
    if [ "$owner" = "$INSTANCE_ID" ]; then
        aws ssm delete-parameter --name "$SETUP_LOCK" --region "$AWS_REGION" >/dev/null 2>&1 || true
    fi
    SETUP_LOCK_HELD=false
}

INSTANCE_ID=$(get_metadata "instance-id")
echo "Instance ID: $INSTANCE_ID"

# --- ASG launch hook ---
# "Pending:Wait" = launching or resuming into service; "Warmed:Pending:Wait" =
# first boot of an instance headed for the warm pool. Anything else (e.g. a
# service restart on a running DTN) means there is no hook to signal.
ASG_NAME="elastic-gcs-dtn-${AWS_REGION}-${DEPLOYMENT}"
LIFECYCLE_STATE=$(aws autoscaling describe-auto-scaling-instances \
    --instance-ids "$INSTANCE_ID" --region "$AWS_REGION" \
    --query 'AutoScalingInstances[0].LifecycleState' --output text 2>/dev/null || echo unknown)
echo "Lifecycle state: $LIFECYCLE_STATE"
HOOK_DONE=false
complete_hook() {  # CONTINUE|ABANDON
    case "$LIFECYCLE_STATE" in *Pending:Wait) ;; *) return 0;; esac
    [ "$HOOK_DONE" = true ] && return 0
    echo ">>> Completing lifecycle action $1"
    aws autoscaling complete-lifecycle-action \
        --lifecycle-hook-name dtn-ready-hook \
        --auto-scaling-group-name "$ASG_NAME" \
        --instance-id "$INSTANCE_ID" \
        --lifecycle-action-result "$1" \
        --region "$AWS_REGION" \
        && HOOK_DONE=true \
        || echo "WARNING: failed to complete lifecycle action $1"
}

SECRETS_DIR=""
on_exit() {
    local rc=$?
    [ -n "$SECRETS_DIR" ] && rm -rf "$SECRETS_DIR"
    release_setup_lock
    if [ "$rc" -ne 0 ]; then
        echo "ERROR: boot failed (exit $rc); abandoning so the ASG replaces this instance"
        complete_hook ABANDON
    fi
}
trap on_exit EXIT

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

# Public IP from IMDS; it can take a moment to appear after start.
PUBLIC_IP=""
for _ in $(seq 1 30); do
    PUBLIC_IP=$(get_metadata "public-ipv4" || true)
    [[ "$PUBLIC_IP" =~ ^[0-9.]+$ ]] && break
    PUBLIC_IP=""
    sleep 1
done
[ -n "$PUBLIC_IP" ] || { echo "ERROR: no public IPv4 from IMDS"; exit 1; }
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

    # node update requires GCS CLI auth (client credentials).
    # The endpoint ID comes from the local info.json (always accurate).
    ENDPOINT_ID=$(jq -r '.endpoint_id' "$GCS_STATE_DIR/info.json")
    # Assigned separately from export so a failed fetch stops the script (set -e).
    GCS_CLI_CLIENT_ID=$(get_secret "${SECRETS_PREFIX}/globus-client-id")
    GCS_CLI_CLIENT_SECRET=$(get_secret "${SECRETS_PREFIX}/globus-client-secret")
    export GCS_CLI_CLIENT_ID GCS_CLI_CLIENT_SECRET
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

    # Enable locally first: a disabled node's own GCS Manager refuses API calls,
    # and going through the endpoint DNS name fails when no other node is
    # active (e.g. scale-from-zero with the control node out of the data path).
    # Once enabled, update the IP through this node's own manager.
    echo ">>> Enabling node $NODE_ID locally and updating its IP to $PUBLIC_IP"
    globus-connect-server node enable
    chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null || true
    globus-connect-server node update "$NODE_ID" \
        --ip-address "$PUBLIC_IP" \
        --use-explicit-host 127.0.0.1

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

    # Secrets go in a private temp dir that is removed on any exit, so a
    # failed setup can't leave the deployment key on disk.
    # (removed by on_exit).
    SECRETS_DIR=$(umask 077 && mktemp -d /tmp/elastic-gcs.XXXXXX)
    DEPLOY_KEY_FILE="$SECRETS_DIR/deployment-key.json"
    NODE_IDENTITY_FILE="$SECRETS_DIR/node-slot-${DTN_SLOT_NUMBER}.json"

    echo ">>> Retrieving deployment key from Secrets Manager..."
    get_secret "${SECRETS_PREFIX}/deployment-key" > "$DEPLOY_KEY_FILE"

    echo ">>> Retrieving node identity for slot $DTN_SLOT_NUMBER..."
    get_secret "${SECRETS_PREFIX}/node-slot-${DTN_SLOT_NUMBER}" > "$NODE_IDENTITY_FILE"

    # Persist the node_id so the resume path can find it regardless of
    # which slot this instance claims on a subsequent boot (fixes issue #16).
    NODE_ID=$(jq -r '.node_id' "$NODE_IDENTITY_FILE")
    mkdir -p /var/lib/elastic-gcs
    echo "$NODE_ID" > /var/lib/elastic-gcs/node-id

    # node setup updates the endpoint's shared keychain in Globus, and
    # concurrent setups fail with "Unable to save keychain to cloud ... Object
    # KEY_CHAIN has changed". A fresh warm pool boots every instance at once,
    # so serialize setups across the deployment with an SSM lock (setup takes
    # ~7 s). A lock older than SETUP_LOCK_STALE_SEC is treated as abandoned.
    acquire_setup_lock
    MAX_SETUP_ATTEMPTS=4
    for attempt in $(seq 1 "$MAX_SETUP_ATTEMPTS"); do
        echo ">>> Running: globus-connect-server node setup --import-node (attempt $attempt/$MAX_SETUP_ATTEMPTS)"
        if globus-connect-server node setup \
            --deployment-key "$DEPLOY_KEY_FILE" \
            --import-node "$NODE_IDENTITY_FILE" \
            --ip-address "$PUBLIC_IP"; then
            break
        fi
        # Retry as-is: a failed keychain save leaves no info.json, and a plain
        # re-run succeeds. Running `node cleanup` without info.json breaks
        # later attempts (PermissionError on info.json), so only clean up when
        # setup left partial state behind.
        if [ -s "$GCS_STATE_DIR/info.json" ]; then
            globus-connect-server node cleanup || true
        fi
        if [ "$attempt" -eq "$MAX_SETUP_ATTEMPTS" ]; then
            echo "ERROR: node setup failed after $attempt attempts"
            exit 1
        fi
        DELAY=$(( 5 * attempt + RANDOM % 10 ))
        echo ">>> node setup failed; retrying in ${DELAY}s"
        sleep "$DELAY"
    done
    release_setup_lock

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

# --- Ready gate, then signal the launch hook (#34, #69) ---
if [ "$LIFECYCLE_STATE" = "Warmed:Pending:Wait" ]; then
    # Headed for the warm pool: node setup left the node active and in DNS.
    # Take it out before the ASG stops the instance, so it never receives
    # transfer processes it can't finish. The resume path re-enables it.
    echo ">>> Warm-pool first boot: disabling node before it is stopped"
    globus-connect-server node disable
    chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null || true
    wait_endpoint_dns absent "$PUBLIC_IP" 180 || true
else
    # Going InService: transfers can't reach this node until Globus adds it to
    # the endpoint's DNS, which happens asynchronously.
    wait_endpoint_dns present "$PUBLIC_IP" 180 || true
fi
complete_hook CONTINUE

# --- After signalling: diagnostics and metrics (off the scale-out path) ---
NODE_STATUS=$(globus-connect-server node show -F json 2>/dev/null | jq -r '.status' || echo "unknown")
echo ">>> Node status: $NODE_STATUS"
globus-connect-server self-diagnostic 2>&1 | head -20 || true

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

echo ""
echo "=== DTN Boot Complete ==="
echo "Boot type:   $BOOT_TYPE"
echo "Lifecycle:   $LIFECYCLE_STATE"
echo "Node status: $NODE_STATUS"
echo "Public IP:   $PUBLIC_IP"
echo "DTN Slot:    $DTN_SLOT_NUMBER"
echo "Instance:    $INSTANCE_ID"
echo "========================="
