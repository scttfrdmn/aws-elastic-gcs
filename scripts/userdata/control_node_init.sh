#!/bin/bash
set -euo pipefail

# =============================================================================
# Control Node Initialization Script
# =============================================================================
# This script runs as UserData on first boot of the GCS control node.
# It installs GCS, sets up the endpoint, configures storage gateways
# for Open Data buckets, and pre-creates DTN node identities.
#
# Prerequisites (injected via CDK):
#   - GLOBUS_CLIENT_ID: Globus application client ID
#   - GLOBUS_CLIENT_SECRET: Globus application client secret
#   - GLOBUS_SUBSCRIPTION_ID: Globus subscription ID
#   - GLOBUS_OWNER_IDENTITY: Endpoint owner (e.g. user@globus.org)
#   - ENDPOINT_DISPLAY_NAME: Display name for the endpoint
#   - ORGANIZATION_NAME: Organization name for the endpoint
#   - AWS_REGION: Current AWS region
#   - WARM_POOL_SIZE: Warm pool size (passed but not used directly by script)
#   - TOTAL_SLOTS: Total EIP slots = maxDTNs + warmPoolSize (node identities created)
#   - SECRETS_PREFIX: Secrets Manager prefix (e.g. elastic-gcs/us-west-2/src)
#   - PARAM_PREFIX: SSM Parameter Store prefix (e.g. /elastic-gcs/us-west-2/src)
#   - GCS_VERSION: exact GCS package version to install (e.g. 5.4.101)
# =============================================================================

LOG="/var/log/elastic-gcs-setup.log"
exec > >(tee -a "$LOG") 2>&1
echo "=== Elastic GCS Control Node Setup - $(date -u) ==="

# --- Idempotency guard ---
# cloud-init runs UserData once per instance, so this script normally runs only
# on a new instance. After a stop/start, elastic-gcs-control.service (installed
# by UserData) re-enables the node (#31). If GCS is somehow already set up here,
# do nothing rather than risk a second endpoint or node.
GCS_INFO="/var/lib/globus-connect-server/info.json"
if [ -s "$GCS_INFO" ]; then
    echo "=== GCS already configured on this instance; boot is handled by elastic-gcs-control.service ==="
    exit 0
fi

# Secrets on disk (#32): deployment key and slot identities live only in this
# 0700 directory, which is removed when the script exits, however it exits.
umask 077
SECRETS_DIR=$(mktemp -d /run/elastic-gcs-init.XXXXXX)
trap 'rm -rf "$SECRETS_DIR"' EXIT

# --- 1. Install prerequisites ---
echo ">>> Installing prerequisites..."

# Detect OS
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS_ID="$ID"
fi

case "$OS_ID" in
    rhel|rocky|almalinux)
        # Verified installs (#46): UserData wrote the library and pinned keys and
        # already installed the SSM agent and AWS CLI with signature checks.
        # shellcheck source=/dev/null
        source /opt/elastic-gcs/lib/verified_install.sh
        dnf install -y jq
        install_epel
        setup_globus_repo
        # GCS, pinned so every node in the endpoint runs the same version.
        # Packages and repo metadata are GPG-checked against the pinned key.
        dnf install -y "globus-connect-server54-${GCS_VERSION}"
        ;;
    *)
        # The control stack launches Rocky Linux 9 only.
        echo "ERROR: Unsupported OS: $OS_ID (the control node runs Rocky Linux 9)"
        exit 1
        ;;
esac

# --- 1a. Configure TLS cipher policy ---
# One source for the approved policy, shared with the DTN AMI (#47):
# configure_tls.sh sets the RHEL 9 crypto-policy module and merges (rather
# than overwrites) the GridFTP settings in /etc/grid-security/gsi.conf.
echo ">>> Configuring TLS cipher policy..."
bash /opt/elastic-gcs/scripts/configure_tls.sh

# --- 1b. Keep GridFTP sessions alive across restarts (#141) ---
# Shared with the DTN AMI: GCS restarts GridFTP on every node at each node join
# or leave; KillMode=process lets in-flight sessions finish.
echo ">>> Configuring GridFTP session keep-alive..."
bash /opt/elastic-gcs/scripts/configure_gridftp.sh

# --- 2. Retrieve secrets ---
echo ">>> Retrieving Globus credentials from Secrets Manager..."

get_secret() {
    aws secretsmanager get-secret-value \
        --secret-id "$1" \
        --query SecretString \
        --output text \
        --region "$AWS_REGION"
}

GLOBUS_CLIENT_ID=$(get_secret "${SECRETS_PREFIX}/globus-client-id")
GLOBUS_CLIENT_SECRET=$(get_secret "${SECRETS_PREFIX}/globus-client-secret")

# Export for GCS CLI
export GCS_CLI_CLIENT_ID="$GLOBUS_CLIENT_ID"
export GCS_CLI_CLIENT_SECRET="$GLOBUS_CLIENT_SECRET"

# --- 3. Set up endpoint ---
echo ">>> Setting up GCS endpoint..."

# endpoint setup writes deployment-key.json to its working directory, so it
# runs inside SECRETS_DIR.
DEPLOY_KEY_PATH="$SECRETS_DIR/deployment-key.json"
NEW_ENDPOINT=false

# Idempotency: check Secrets Manager first.
# If an existing deployment key is stored (from a prior deploy), retrieve it
# and skip endpoint setup. This prevents creating a new endpoint on every
# control node replacement.
if aws secretsmanager get-secret-value \
    --secret-id "${SECRETS_PREFIX}/deployment-key" \
    --query SecretString --output text --region "$AWS_REGION" \
    > "$DEPLOY_KEY_PATH" 2>/dev/null && [ -s "$DEPLOY_KEY_PATH" ]; then
    echo ">>> Deployment key retrieved from Secrets Manager — reusing existing endpoint."
else
    rm -f "$DEPLOY_KEY_PATH"
    (cd "$SECRETS_DIR" && globus-connect-server endpoint setup \
        "$ENDPOINT_DISPLAY_NAME" \
        --organization "$ORGANIZATION_NAME" \
        --owner "$GLOBUS_OWNER_IDENTITY" \
        --contact-email "$GLOBUS_OWNER_IDENTITY" \
        --dont-set-advertised-owner \
        --agree-to-letsencrypt-tos)
    NEW_ENDPOINT=true
    echo ">>> Endpoint setup complete."

    # Store the deployment key immediately (#32). If a later step fails, a
    # replacement control node must find it and reuse this endpoint rather than
    # create a second one.
    aws secretsmanager create-secret \
        --name "${SECRETS_PREFIX}/deployment-key" \
        --secret-string "file://${DEPLOY_KEY_PATH}" \
        --region "$AWS_REGION" >/dev/null 2>&1 \
    || aws secretsmanager put-secret-value \
        --secret-id "${SECRETS_PREFIX}/deployment-key" \
        --secret-string "file://${DEPLOY_KEY_PATH}" \
        --region "$AWS_REGION" >/dev/null
    echo ">>> Deployment key stored in Secrets Manager."
fi

# --- 4. Set up this node ---
echo ">>> Setting up control node..."

# Get public IP via IMDSv2
IMDS_TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 3600")
PUBLIC_IP=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
    http://169.254.169.254/latest/meta-data/public-ipv4)

globus-connect-server node setup \
    --deployment-key "$DEPLOY_KEY_PATH" \
    --ip-address "$PUBLIC_IP"

echo ">>> Node setup complete."

# --- 5. Configure endpoint credentials and subscription ---
echo ">>> Configuring endpoint..."

# Read endpoint ID from info.json — avoids a circular dependency where
# `endpoint show` requires GCS_CLI_ENDPOINT_ID but we don't have it yet.
# info.json is written by `node setup` and always has the endpoint_id.
ENDPOINT_ID=$(jq -r '.endpoint_id' /var/lib/globus-connect-server/info.json)
export GCS_CLI_ENDPOINT_ID="$ENDPOINT_ID"

# The service account needs the administrator role on its own endpoint
# (collections setup, demand_monitor's endpoint_manager task list, node cleanup,
# version check). Role creation requires a subscription, and only the endpoint
# owner can attach one, after setup (#66). Try both; if either is refused, print
# the owner's post-deploy steps instead of claiming success.
OWNER_STEPS=false
if [ -n "$GLOBUS_SUBSCRIPTION_ID" ] && \
   globus-connect-server endpoint set-subscription-id "$GLOBUS_SUBSCRIPTION_ID" >/dev/null 2>&1; then
    echo ">>> Endpoint set as managed under subscription $GLOBUS_SUBSCRIPTION_ID."
else
    OWNER_STEPS=true
fi
if globus-connect-server endpoint role create administrator "$GLOBUS_CLIENT_ID" >/dev/null 2>&1; then
    echo ">>> Service account granted the administrator role."
else
    OWNER_STEPS=true
fi
# Network use (#56): Transfer gives each task the lower of the two endpoints'
# max concurrency, and Globus's default ("normal") is 4. Needs the role above.
if [ -n "${NETWORK_USE_ARGS:-}" ]; then
    # shellcheck disable=SC2086  # NETWORK_USE_ARGS is a list of flags
    if [ "$OWNER_STEPS" = false ] && globus-connect-server endpoint update $NETWORK_USE_ARGS >/dev/null 2>&1; then
        echo ">>> Endpoint network use set: $NETWORK_USE_ARGS"
    else
        echo ">>> After the owner steps below, set network use with:"
        echo ">>>   aws-elastic-gcs-ctl endpoint network-use --apply --region $AWS_REGION --deployment <deployment>"
    fi
fi
if [ "$OWNER_STEPS" = true ]; then
    echo ">>> ACTION REQUIRED (endpoint owner, $GLOBUS_OWNER_IDENTITY):"
    echo ">>>   globus login --gcs $ENDPOINT_ID"
    echo ">>>   globus gcs endpoint set-subscription-id $ENDPOINT_ID ${GLOBUS_SUBSCRIPTION_ID:-<subscription-id>}"
    echo ">>>   globus gcs endpoint role create $ENDPOINT_ID administrator $GLOBUS_CLIENT_ID"
    echo ">>> Until then the service account has no role: collections setup and scaling won't work."
fi

# --- 6. Pre-create DTN node identities ---
# (Storage gateway and collection setup is a post-deploy step.
#  Run: aws-elastic-gcs-ctl collections setup --config config/open_data_buckets.yaml)
# TOTAL_SLOTS = maxDTNs + warmPoolSize: every possible slot needs an identity.
# Idempotency: when the endpoint was reused, an existing slot secret with a
# node_id belongs to it and is kept, rather than orphaning a node on every
# control node replacement. When this run created a new endpoint, every existing
# slot secret belongs to an old endpoint and is replaced (#32).
echo ">>> Ensuring $TOTAL_SLOTS DTN node identities (new endpoint: $NEW_ENDPOINT)..."

for i in $(seq 1 "$TOTAL_SLOTS"); do
    NODE_FILE="$SECRETS_DIR/dtn-slot-${i}.json"

    # Check if the slot secret already exists with a valid node_id for this endpoint.
    EXISTING_SECRET=$(aws secretsmanager get-secret-value \
        --secret-id "${SECRETS_PREFIX}/node-slot-${i}" \
        --query SecretString --output text --region "$AWS_REGION" 2>/dev/null || true)

    if [ -n "$EXISTING_SECRET" ] && [ "$NEW_ENDPOINT" = false ]; then
        EXISTING_NODE_ID=$(echo "$EXISTING_SECRET" | jq -r '.node_id // empty' 2>/dev/null || true)
        if [ -n "$EXISTING_NODE_ID" ]; then
            echo ">>> Node identity for slot $i already exists (node_id: $EXISTING_NODE_ID) — skipping."
            continue
        fi
    fi

    echo ">>> Creating node identity for DTN slot $i..."
    globus-connect-server node create \
        --deployment-key "$DEPLOY_KEY_PATH" \
        --export-node "$NODE_FILE"

    # Store in Secrets Manager
    aws secretsmanager create-secret \
        --name "${SECRETS_PREFIX}/node-slot-${i}" \
        --secret-string "file://${NODE_FILE}" \
        --region "$AWS_REGION" \
        2>/dev/null \
    || aws secretsmanager put-secret-value \
        --secret-id "${SECRETS_PREFIX}/node-slot-${i}" \
        --secret-string "file://${NODE_FILE}" \
        --region "$AWS_REGION"

    echo ">>> Node identity for slot $i stored in Secrets Manager."
    rm -f "$NODE_FILE"
done

# --- 7. Store endpoint metadata in SSM ---
echo ">>> Storing endpoint metadata in SSM Parameter Store..."

ENDPOINT_ID=$(globus-connect-server endpoint show -F json | jq -r '.id')

aws ssm put-parameter \
    --name "${PARAM_PREFIX}/endpoint-id" \
    --type String \
    --value "$ENDPOINT_ID" \
    --overwrite \
    --region "$AWS_REGION"

INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
    http://169.254.169.254/latest/meta-data/instance-id)
aws ssm put-parameter \
    --name "${PARAM_PREFIX}/control-node-id" \
    --type String \
    --value "$INSTANCE_ID" \
    --overwrite \
    --region "$AWS_REGION"

# --- 8. Verify ---
echo ">>> Running self-diagnostics..."
globus-connect-server self-diagnostic
globus-connect-server node list

echo ""
echo "=== Control Node Setup Complete ==="
echo "Endpoint ID: $ENDPOINT_ID"
echo "Instance ID: $INSTANCE_ID"
echo "Public IP:   $PUBLIC_IP"
echo "Region:      $AWS_REGION"
echo "DTN slots pre-created: $TOTAL_SLOTS"
echo "=================================="
