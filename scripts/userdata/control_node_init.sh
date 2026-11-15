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
    OS_VERSION_ID="${VERSION_ID%%.*}"
fi

case "$OS_ID" in
    amzn|rhel|rocky|almalinux)
        dnf install -y jq
        # UserData installs AWS CLI v2 before downloading this script.
        command -v aws >/dev/null || dnf install -y awscli
        # Install EPEL (required for GCS dependencies)
        dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-${OS_VERSION_ID}.noarch.rpm || true
        # Install GCS repo and import its GPG key.
        # The globus-repo RPM installs the key file but does not rpm --import it
        # on Rocky/RHEL 9, causing GPG check failures. Import explicitly.
        dnf install -y https://downloads.globus.org/globus-connect-server/stable/installers/repo/rpm/globus-repo-latest.noarch.rpm
        rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-Globus-2024 || true
        # Install GCS, pinned so every node in the endpoint runs the same version
        dnf install -y "globus-connect-server54-${GCS_VERSION}"
        ;;
    ubuntu|debian)
        apt-get update
        apt-get install -y jq curl
        command -v aws >/dev/null || apt-get install -y awscli
        # Install GCS repo
        curl -LOs https://downloads.globus.org/globus-connect-server/stable/installers/repo/deb/globus-repo_latest_all.deb
        dpkg -i globus-repo_latest_all.deb
        apt-get update
        apt-get install -y "globus-connect-server54=${GCS_VERSION}-*"
        ;;
    *)
        echo "ERROR: Unsupported OS: $OS_ID"
        exit 1
        ;;
esac

# --- 1a. Configure TLS cipher policy ---
# GCS regenerates its Apache VirtualHost-level SSLCipherSuite on every
# collection change and marks those files "DO NOT EDIT". To enforce the
# approved cipher list regardless of GCS regeneration, we use the RHEL 9
# system crypto-policy (a hard OpenSSL constraint) plus explicit gsi.conf
# settings for GridFTP data channels.
echo ">>> Configuring TLS cipher policy..."
APPROVED_CIPHERS="ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-ECDSA-AES128-SHA256:ECDHE-ECDSA-AES256-SHA384:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-RSA-AES128-SHA256:ECDHE-RSA-AES256-SHA384"

case "$OS_ID" in
    amzn|rhel|rocky|almalinux)
        # RHEL 9 crypto-policy: removes CHACHA20-POLY1305 and DHE (FFDHE)
        # key exchange groups from the DEFAULT policy. Apache and GridFTP
        # inherit this constraint from OpenSSL; GCS config regeneration cannot
        # re-enable ciphers that the policy has removed.
        mkdir -p /etc/crypto-policies/policies/modules
        cat > /etc/crypto-policies/policies/modules/ELASTIC-GCS.pmod <<'PMOD'
# Elastic GCS approved cipher policy — do not edit manually
# key_exchange = ECDHE ECDHE-PSK is omitted: breaks auth.globus.org (close_notify)
# group = -FFDHE-* achieves DHE removal without affecting Globus connectivity
group = -FFDHE-*
cipher = -CHACHA20-POLY1305
# To restrict to TLS 1.2 only, uncomment: protocol = TLS1.2
PMOD
        update-crypto-policies --set DEFAULT:ELASTIC-GCS
        echo ">>> Crypto policy applied: $(update-crypto-policies --show)"
        ;;
    ubuntu|debian)
        # No update-crypto-policies on Debian/Ubuntu.
        # Write an Apache drop-in as best-effort; note GCS may override it.
        mkdir -p /etc/apache2/conf-available
        cat > /etc/apache2/conf-available/elastic-gcs-tls.conf <<EOF
# Elastic GCS approved cipher list (best-effort — GCS may regenerate VirtualHost settings)
SSLProtocol -all +TLSv1.2 +TLSv1.3
SSLCipherSuite ${APPROVED_CIPHERS}
SSLHonorCipherOrder off
EOF
        a2enconf elastic-gcs-tls 2>/dev/null || true
        ;;
esac

# GridFTP data channel TLS (/etc/grid-security/gsi.conf)
# Explicit cipher list for ports 50000-51000; system crypto-policy also applies.
mkdir -p /etc/grid-security
cat > /etc/grid-security/gsi.conf <<EOF
MIN_TLS_PROTOCOL=TLS1_2_VERSION
CIPHERS=${APPROVED_CIPHERS}
SERVER_CIPHER_ORDER=false
EOF
echo ">>> GridFTP TLS configured."

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
