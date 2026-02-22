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
#   - OPEN_DATA_BUCKETS_JSON: JSON array of bucket configs
#   - SECRETS_PREFIX: Secrets Manager prefix (e.g. elastic-gcs/us-east-1)
# =============================================================================

LOG="/var/log/elastic-gcs-setup.log"
exec > >(tee -a "$LOG") 2>&1
echo "=== Elastic GCS Control Node Setup - $(date -u) ==="

# --- Idempotency guard ---
# cloud-init re-runs UserData on every stop/start (e.g. after a CDK deploy that
# changes UserData). If GCS is already configured, skip the full setup and just
# update the node's registered IP to the current public IP. This prevents
# duplicate endpoint/node creation and avoids destroying existing state.
GCS_INFO="/var/lib/globus-connect-server/info.json"
if [ -s "$GCS_INFO" ]; then
    echo "=== GCS already configured — skipping full setup (resume after stop/start) ==="

    get_secret_resume() {
        aws secretsmanager get-secret-value \
            --secret-id "$1" --query SecretString --output text --region "$AWS_REGION"
    }

    # Get current public IP via IMDSv2
    IMDS_TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
    PUBLIC_IP=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
        http://169.254.169.254/latest/meta-data/public-ipv4)
    INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
        http://169.254.169.254/latest/meta-data/instance-id)
    DOMAIN_NAME=$(jq -r '.domain_name' "$GCS_INFO")
    ENDPOINT_ID=$(jq -r '.endpoint_id' "$GCS_INFO")

    echo ">>> Public IP: $PUBLIC_IP  Instance: $INSTANCE_ID  Domain: $DOMAIN_NAME"

    # GCS CLI contacts the GCS Manager via the endpoint domain, which still
    # resolves to the OLD IP. Route it through localhost so node update succeeds.
    echo "127.0.0.1 ${DOMAIN_NAME}" >> /etc/hosts

    export GCS_CLI_CLIENT_ID=$(get_secret_resume "${SECRETS_PREFIX}/globus-client-id")
    export GCS_CLI_CLIENT_SECRET=$(get_secret_resume "${SECRETS_PREFIX}/globus-client-secret")
    export GCS_CLI_ENDPOINT_ID="$ENDPOINT_ID"

    # node show returns this node's details without needing to know the node ID up front.
    CONTROL_NODE_ID=$(globus-connect-server node show -F json 2>/dev/null | jq -r '.id' || echo "")
    if [ -n "$CONTROL_NODE_ID" ] && [ "$CONTROL_NODE_ID" != "null" ]; then
        echo ">>> Updating control node $CONTROL_NODE_ID IP → $PUBLIC_IP"
        globus-connect-server node update "$CONTROL_NODE_ID" \
            --ip-address "$PUBLIC_IP" \
            --enable
        chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null || true
        echo ">>> Node IP updated and enabled."
    else
        echo "WARNING: Could not determine control node ID — IP not updated."
    fi

    # Remove the temporary hosts entry
    sed -i "/${DOMAIN_NAME}/d" /etc/hosts

    # Update SSM with current instance ID (in case instance was replaced)
    aws ssm put-parameter \
        --name "/elastic-gcs/${AWS_REGION}/control-node-id" \
        --type String --value "$INSTANCE_ID" \
        --overwrite --region "$AWS_REGION" \
        && echo ">>> SSM control-node-id updated." || true

    echo "=== Resume complete: IP=$PUBLIC_IP Endpoint=$ENDPOINT_ID ==="
    exit 0
fi

# --- 1. Install prerequisites ---
echo ">>> Installing prerequisites..."

# Detect OS
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS_ID="$ID"
    OS_VERSION_ID="${VERSION_ID%%.*}"
fi

case "$OS_ID" in
    amzn|rhel|rocky|almalinux)
        dnf install -y jq awscli
        # Install EPEL (required for GCS dependencies)
        dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-${OS_VERSION_ID}.noarch.rpm || true
        # Install GCS repo and import its GPG key.
        # The globus-repo RPM installs the key file but does not rpm --import it
        # on Rocky/RHEL 9, causing GPG check failures. Import explicitly.
        dnf install -y https://downloads.globus.org/globus-connect-server/stable/installers/repo/rpm/globus-repo-latest.noarch.rpm
        rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-Globus-2024 || true
        # Install GCS
        dnf install -y globus-connect-server54
        ;;
    ubuntu|debian)
        apt-get update
        apt-get install -y jq awscli curl
        # Install GCS repo
        curl -LOs https://downloads.globus.org/globus-connect-server/stable/installers/repo/deb/globus-repo_latest_all.deb
        dpkg -i globus-repo_latest_all.deb
        apt-get update
        apt-get install -y globus-connect-server54
        ;;
    *)
        echo "ERROR: Unsupported OS: $OS_ID"
        exit 1
        ;;
esac

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

# deployment-key.json is created in CWD by endpoint setup.
# UserData CWD is /, so the file lands at /deployment-key.json.
DEPLOY_KEY_PATH="/deployment-key.json"

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
    globus-connect-server endpoint setup \
        "$ENDPOINT_DISPLAY_NAME" \
        --organization "$ORGANIZATION_NAME" \
        --owner "$GLOBUS_OWNER_IDENTITY" \
        --contact-email "$GLOBUS_OWNER_IDENTITY" \
        --dont-set-advertised-owner \
        --agree-to-letsencrypt-tos
    # deployment-key.json is created in CWD (/)
    echo ">>> Endpoint setup complete."
fi

# --- 4. Set up this node ---
echo ">>> Setting up control node..."

# Get public IP via IMDSv2
IMDS_TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
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

# Grant the client app admin rights so it can manage storage gateways and
# collections. endpoint setup automatically grants admin to the caller, but
# we do this explicitly as a safeguard.
globus-connect-server endpoint role create administrator "$GLOBUS_CLIENT_ID" || true
echo ">>> Client app admin role ensured."

# Subscription association requires the endpoint owner's personal identity —
# the client app cannot set it. Attempt it here in case a future GCS version
# allows it, but don't fail the script if it's denied.
if [ -n "$GLOBUS_SUBSCRIPTION_ID" ]; then
    globus-connect-server endpoint set-subscription-id "$GLOBUS_SUBSCRIPTION_ID" 2>/dev/null \
        && echo ">>> Endpoint set as managed under subscription." \
        || echo ">>> NOTE: Subscription must be set manually by the endpoint owner:"
    echo ">>>   globus login --gcs $ENDPOINT_ID"
    echo ">>>   globus gcs endpoint set-subscription-id $ENDPOINT_ID $GLOBUS_SUBSCRIPTION_ID"
fi

# --- 6. Configure S3 storage gateways for Open Data buckets ---
echo ">>> Configuring S3 storage gateways..."

# OPEN_DATA_BUCKETS_JSON is a JSON array like:
# [{"name":"1000 Genomes","bucket":"1000genomes","requester_pays":false}]

echo "$OPEN_DATA_BUCKETS_JSON" | jq -c '.[]' | while read -r bucket_config; do
    BUCKET_NAME=$(echo "$bucket_config" | jq -r '.name')
    BUCKET_S3=$(echo "$bucket_config" | jq -r '.bucket')
    REQUESTER_PAYS=$(echo "$bucket_config" | jq -r '.requester_pays')

    echo ">>> Creating storage gateway for: $BUCKET_NAME ($BUCKET_S3)"

    # Public buckets use --s3-unauthenticated (no user S3 credentials needed).
    # Requester-pays buckets require the user to supply S3 credentials.
    if [ "$REQUESTER_PAYS" = "true" ]; then
        AUTH_FLAG="--s3-user-credential"
    else
        AUTH_FLAG="--s3-unauthenticated"
    fi

    GW_ID=$(globus-connect-server storage-gateway create s3 \
        "$BUCKET_NAME" \
        --domain globusid.org \
        --s3-endpoint "https://s3.${AWS_REGION}.amazonaws.com" \
        --bucket "$BUCKET_S3" \
        $AUTH_FLAG \
        2>&1 | grep -oP 'Storage Gateway ID: \K[a-f0-9-]+' || true)

    if [ -n "$GW_ID" ]; then
        echo ">>> Storage gateway created: $GW_ID"

        # Create a mapped collection
        globus-connect-server collection create \
            "$GW_ID" / \
            "Open Data: $BUCKET_NAME" \
            --allow-guest-collections

        echo ">>> Collection created for: $BUCKET_NAME"
    else
        echo "WARNING: Failed to create storage gateway for $BUCKET_NAME"
    fi
done

# --- 7. Pre-create DTN node identities ---
# TOTAL_SLOTS = maxDTNs + warmPoolSize: every possible EIP slot needs an identity.
echo ">>> Pre-creating $TOTAL_SLOTS DTN node identities..."

for i in $(seq 1 "$TOTAL_SLOTS"); do
    NODE_FILE="/tmp/dtn-slot-${i}.json"

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

# Also store the deployment key in Secrets Manager (upsert)
aws secretsmanager create-secret \
    --name "${SECRETS_PREFIX}/deployment-key" \
    --secret-string "file://${DEPLOY_KEY_PATH}" \
    --region "$AWS_REGION" \
    2>/dev/null \
|| aws secretsmanager put-secret-value \
    --secret-id "${SECRETS_PREFIX}/deployment-key" \
    --secret-string "file://${DEPLOY_KEY_PATH}" \
    --region "$AWS_REGION"

# --- 8. Store endpoint metadata in SSM ---
echo ">>> Storing endpoint metadata in SSM Parameter Store..."

ENDPOINT_ID=$(globus-connect-server endpoint show -F json | jq -r '.id')

aws ssm put-parameter \
    --name "/elastic-gcs/${AWS_REGION}/endpoint-id" \
    --type String \
    --value "$ENDPOINT_ID" \
    --overwrite \
    --region "$AWS_REGION"

INSTANCE_ID=$(curl -s -H "X-aws-ec2-metadata-token: $IMDS_TOKEN" \
    http://169.254.169.254/latest/meta-data/instance-id)
aws ssm put-parameter \
    --name "/elastic-gcs/${AWS_REGION}/control-node-id" \
    --type String \
    --value "$INSTANCE_ID" \
    --overwrite \
    --region "$AWS_REGION"

# --- 9. Verify ---
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
