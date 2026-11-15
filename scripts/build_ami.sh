#!/bin/bash
set -euo pipefail

# =============================================================================
# DTN AMI Build Script
# =============================================================================
# Builds an AMI with GCS pre-installed for use by DTN warm pool instances.
# The AMI contains NO node identity or endpoint config — those are injected
# at boot time from Secrets Manager by the UserData / systemd service.
#
# AMI contents:
#   - GCS 5.4 (globus-connect-server54, pinned to $GCS_VERSION)
#   - SSM Agent (amazon-ssm-agent)
#   - jq, awscli
#   - TLS cipher policy (configure_tls.sh) and the GridFTP KillMode=process
#     drop-in (configure_gridftp.sh, #141)
#
# Usage:
#   AWS_PROFILE=aws ./scripts/build_ami.sh [region] [deployment]
#   AWS_PROFILE=aws ./scripts/build_ami.sh us-west-2 src
#   GCS_VERSION=5.4.101 ./scripts/build_ami.sh us-west-2 src
#
# The builder borrows the networking of <deployment>'s control node. The AMI
# itself is shared by every deployment in the region running that GCS version.
#
# GCS_VERSION defaults to the globus:gcsVersion value in cdk.json, which the
# DTN pool stack uses to look up this AMI by name.
# =============================================================================

REGION="${1:-us-east-1}"
DEPLOYMENT="${2:-default}"
PARAM_PREFIX="/elastic-gcs/${REGION}/${DEPLOYMENT}"
AWS_PROFILE="${AWS_PROFILE:-default}"
GCS_VERSION="${GCS_VERSION:-$(jq -r '.context["globus:gcsVersion"]' "$(dirname "$0")/../cdk.json")}"
if ! [[ "$GCS_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "ERROR: invalid GCS_VERSION '$GCS_VERSION' (set globus:gcsVersion in cdk.json)"
    exit 1
fi
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
AMI_NAME="elastic-gcs-dtn-gcs${GCS_VERSION}-${TIMESTAMP}"

echo "=== Building DTN AMI ==="
echo "Region:   $REGION"
echo "GCS:      $GCS_VERSION"
echo "AMI Name: $AMI_NAME"
echo ""

# --- Resolve Rocky Linux 9 base AMI (RESF official) ---
echo ">>> Resolving Rocky Linux 9 x86_64 base AMI..."
BASE_AMI=$(aws ec2 describe-images \
    --owners 792107900819 \
    --filters \
        "Name=name,Values=Rocky-9-EC2-Base-*.x86_64" \
        "Name=state,Values=available" \
    --query 'sort_by(Images, &CreationDate)[-1].ImageId' \
    --output text \
    --region "$REGION" \
    --profile "$AWS_PROFILE")
echo ">>> Base AMI: $BASE_AMI"

# --- Get networking from control node (reuse same VPC/subnet/sg) ---
CONTROL_INSTANCE=$(aws ssm get-parameter \
    --name "${PARAM_PREFIX}/control-node-id" \
    --query Parameter.Value --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

SUBNET_ID=$(aws ec2 describe-instances \
    --instance-ids "$CONTROL_INSTANCE" \
    --query 'Reservations[0].Instances[0].SubnetId' --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

# The builder has its own profile (SSM agent only, no secrets) and an
# egress-only security group, both from the control stack (#45).
BUILDER_NAME="elastic-gcs-ami-builder-${REGION}-${DEPLOYMENT}"
IAM_PROFILE_NAME="$BUILDER_NAME"
SG_ID=$(aws ec2 describe-security-groups \
    --filters "Name=group-name,Values=${BUILDER_NAME}" \
    --query 'SecurityGroups[0].GroupId' --output text \
    --region "$REGION" --profile "$AWS_PROFILE")
if [ -z "$SG_ID" ] || [ "$SG_ID" = "None" ]; then
    echo "ERROR: security group ${BUILDER_NAME} not found; deploy the control stack (0.13+) first"
    exit 1
fi

echo ">>> Subnet: $SUBNET_ID  SG: $SG_ID  Profile: $IAM_PROFILE_NAME"

# --- UserData: ONLY install SSM Agent so we can manage the rest via SSM ---
# Doing package installs in UserData causes RPM lock races with cloud-init.
# Instead we launch a bare instance, wait for SSM, then run installs via SSM.
# The SSM agent is signature-checked against the pinned key (#46): the
# verified-install library and keys ride along gzip+base64 encoded.
HERE=$(cd "$(dirname "$0")" && pwd)
embed() { printf "echo '%s' | base64 -d | gunzip > %s\n" "$(gzip -c "$1" | base64 | tr -d '\n')" "$2"; }
USERDATA="#!/bin/bash
set -euo pipefail
mkdir -p /opt/elastic-gcs/lib /opt/elastic-gcs/keys
$(embed "$HERE/lib/verified_install.sh" /opt/elastic-gcs/lib/verified_install.sh)
$(embed "$HERE/keys/amazon-ssm-agent.asc" /opt/elastic-gcs/keys/amazon-ssm-agent.asc)
$(embed "$HERE/keys/aws-cli.asc" /opt/elastic-gcs/keys/aws-cli.asc)
$(embed "$HERE/keys/globus-2024.asc" /opt/elastic-gcs/keys/globus-2024.asc)
source /opt/elastic-gcs/lib/verified_install.sh
install_ssm_agent $REGION
"

# --- Launch builder instance ---
echo ">>> Launching builder instance (t3.large, Rocky Linux 9)..."
BUILDER_ID=$(aws ec2 run-instances \
    --image-id "$BASE_AMI" \
    --instance-type t3.large \
    --subnet-id "$SUBNET_ID" \
    --security-group-ids "$SG_ID" \
    --iam-instance-profile "Name=${IAM_PROFILE_NAME}" \
    --metadata-options "HttpTokens=required,HttpEndpoint=enabled" \
    --tag-specifications \
        "ResourceType=instance,Tags=[{Key=Name,Value=elastic-gcs-ami-builder},{Key=elastic-gcs:purpose,Value=ami-build}]" \
    --user-data "$USERDATA" \
    --query 'Instances[0].InstanceId' \
    --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

echo ">>> Builder instance: $BUILDER_ID"

# --- Wait for instance running ---
echo ">>> Waiting for instance to reach running state..."
aws ec2 wait instance-running \
    --instance-ids "$BUILDER_ID" \
    --region "$REGION" --profile "$AWS_PROFILE"
echo ">>> Instance is running."

# --- Wait for SSM to come online ---
echo ">>> Waiting for SSM Agent to come online (up to 5 minutes)..."
for i in $(seq 1 30); do
    STATUS=$(aws ssm describe-instance-information \
        --filters "Key=InstanceIds,Values=${BUILDER_ID}" \
        --query 'InstanceInformationList[0].PingStatus' \
        --output text \
        --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null || echo "None")
    if [ "$STATUS" = "Online" ]; then
        echo ">>> SSM online."
        break
    fi
    echo "    ($i/30) SSM status: $STATUS — waiting 10s..."
    sleep 10
done

# Give cloud-init a moment to fully settle before we start installing packages
echo ">>> Waiting 30s for cloud-init to settle..."
sleep 30

# --- Install GCS via SSM (avoids cloud-init RPM lock races) ---
# Write the install script via Python base64 to avoid shell quoting/redirection issues
echo ">>> Running GCS installation via SSM..."

GCS_INSTALL_SCRIPT='#!/bin/bash
set -euo pipefail
exec > /var/log/ami-build.log 2>&1

echo "=== DTN AMI Build via SSM - $(date -u) ==="

# Verified installs (#46): library and pinned keys were written by UserData.
source /opt/elastic-gcs/lib/verified_install.sh

echo "Installing tools..."
dnf install -y jq unzip

echo "Installing AWS CLI v2 (signature verified)..."
install_aws_cli
aws --version

echo "Installing EPEL..."
install_epel

echo "Configuring the GCS repo with the pinned Globus key..."
setup_globus_repo

echo "Installing GCS '"$GCS_VERSION"'..."
dnf install -y globus-connect-server54-'"$GCS_VERSION"'

echo "Verifying..."
globus-connect-server --version | grep -F '"$GCS_VERSION"'

echo "Cleaning up..."
dnf clean all
rm -rf /var/cache/dnf /tmp/* /var/tmp/*
truncate -s 0 /etc/machine-id

echo "INSTALL_COMPLETE"
'

SCRIPT_B64=$(echo "$GCS_INSTALL_SCRIPT" | base64 | tr -d '\n')

INSTALL_CMD=$(aws ssm send-command \
    --instance-ids "$BUILDER_ID" \
    --document-name AWS-RunShellScript \
    --timeout-seconds 600 \
    --parameters "commands=[\"python3 -c \\\"import base64; open('/tmp/gcs-install.sh','wb').write(base64.b64decode('${SCRIPT_B64}'))\\\"\", \"bash /tmp/gcs-install.sh\"]" \
    --query 'Command.CommandId' --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

echo ">>> Install command ID: $INSTALL_CMD"
echo ">>> Waiting for GCS installation to complete (up to 10 minutes)..."

for i in $(seq 1 60); do
    sleep 10
    STATUS=$(aws ssm get-command-invocation \
        --command-id "$INSTALL_CMD" \
        --instance-id "$BUILDER_ID" \
        --query 'Status' --output text \
        --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null || echo "Pending")
    if [ "$STATUS" = "Success" ] || [ "$STATUS" = "Failed" ]; then
        echo ">>> Installation status: $STATUS"
        break
    fi
    echo "    ($i/60) Status: $STATUS — waiting 10s..."
done

# Get output for logging
aws ssm get-command-invocation \
    --command-id "$INSTALL_CMD" \
    --instance-id "$BUILDER_ID" \
    --query 'StandardOutputContent' --output text \
    --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null | tail -20 || true

# Verify GCS installed correctly — fail fast if not present
echo ">>> Verifying GCS installation..."
VERIFY_CMD=$(aws ssm send-command \
    --instance-ids "$BUILDER_ID" \
    --document-name AWS-RunShellScript \
    --parameters 'commands=["globus-connect-server --version 2>&1 || echo GCS_NOT_FOUND"]' \
    --query 'Command.CommandId' --output text \
    --region "$REGION" --profile "$AWS_PROFILE")
sleep 8
GCS_VER=$(aws ssm get-command-invocation \
    --command-id "$VERIFY_CMD" \
    --instance-id "$BUILDER_ID" \
    --query 'StandardOutputContent' --output text \
    --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null | tr -d '[:space:]')
if echo "$GCS_VER" | grep -q "GCS_NOT_FOUND"; then
    echo "ERROR: GCS was not installed. Check /var/log/ami-build.log on $BUILDER_ID"
    echo "Terminating builder without creating AMI."
    aws ec2 terminate-instances --instance-ids "$BUILDER_ID" \
        --region "$REGION" --profile "$AWS_PROFILE" > /dev/null
    exit 1
fi
echo ">>> GCS verified: $GCS_VER"

# --- Node configuration that must be in every DTN AMI ---
# run_config NAME SCRIPT: run a repo script as root on the builder over SSM. An
# AMI missing any of these must not be published (#54), so a failure
# terminates the builder and exits.
run_config() {
    local name=$1 script=$2 b64 cmd status=""
    echo ">>> Applying $name..."
    b64=$(base64 < "$(dirname "$0")/$script" | tr -d '\n')
    cmd=$(aws ssm send-command \
        --instance-ids "$BUILDER_ID" \
        --document-name AWS-RunShellScript \
        --timeout-seconds 120 \
        --parameters "commands=[\"python3 -c \\\"import base64; open('/tmp/$script','wb').write(base64.b64decode('${b64}'))\\\"\", \"bash /tmp/$script\"]" \
        --query 'Command.CommandId' --output text \
        --region "$REGION" --profile "$AWS_PROFILE")
    for i in $(seq 1 12); do
        sleep 10
        status=$(aws ssm get-command-invocation \
            --command-id "$cmd" --instance-id "$BUILDER_ID" \
            --query 'Status' --output text \
            --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null || echo "Pending")
        if [ "$status" = "Success" ] || [ "$status" = "Failed" ]; then
            echo ">>> $name status: $status"
            break
        fi
        echo "    ($i/12) Status: $status — waiting..."
    done
    aws ssm get-command-invocation \
        --command-id "$cmd" --instance-id "$BUILDER_ID" \
        --query 'StandardOutputContent' --output text \
        --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null | tail -10 || true
    if [ "$status" != "Success" ]; then
        echo "ERROR: $name did not succeed (status: ${status:-unknown})."
        echo "Terminating builder without creating AMI."
        aws ec2 terminate-instances --instance-ids "$BUILDER_ID" \
            --region "$REGION" --profile "$AWS_PROFILE" > /dev/null
        exit 1
    fi
}
run_config "TLS cipher policy" configure_tls.sh
# Sessions survive GridFTP restarts on node joins/leaves and drains (#141).
run_config "GridFTP session keep-alive" configure_gridftp.sh

# --- Stop the instance before imaging (ensures filesystem consistency) ---
echo ">>> Stopping builder instance for AMI creation..."
aws ec2 stop-instances \
    --instance-ids "$BUILDER_ID" \
    --region "$REGION" --profile "$AWS_PROFILE" > /dev/null
aws ec2 wait instance-stopped \
    --instance-ids "$BUILDER_ID" \
    --region "$REGION" --profile "$AWS_PROFILE"
echo ">>> Instance stopped."

# --- Create AMI ---
echo ">>> Creating AMI: $AMI_NAME ..."
AMI_ID=$(aws ec2 create-image \
    --instance-id "$BUILDER_ID" \
    --name "$AMI_NAME" \
    --description "Elastic GCS DTN - GCS ${GCS_VERSION} pre-installed on Rocky Linux 9 x86_64" \
    --no-reboot \
    --tag-specifications \
        "ResourceType=image,Tags=[{Key=Name,Value=${AMI_NAME}},{Key=elastic-gcs:purpose,Value=dtn},{Key=elastic-gcs:region,Value=${REGION}},{Key=elastic-gcs:gcs-version,Value=${GCS_VERSION}}]" \
    --query 'ImageId' --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

echo ">>> AMI ID: $AMI_ID (building — this takes 2-5 minutes)"

# --- Wait for AMI to be available ---
echo ">>> Waiting for AMI to become available..."
aws ec2 wait image-available \
    --image-ids "$AMI_ID" \
    --region "$REGION" --profile "$AWS_PROFILE"
echo ">>> AMI is available."

# --- Publish the AMI for this GCS version ---
# Region-wide and version-keyed: every deployment in the region pinned to this
# GCS version resolves its DTN AMI from here at deploy time (stacks/dtn_pool.go).
# Written only after the AMI is available, so a deploy never picks up a pending
# image.
AMI_PARAM="/elastic-gcs/${REGION}/dtn-ami/gcs-${GCS_VERSION}"
aws ssm put-parameter \
    --name "$AMI_PARAM" \
    --type String \
    --data-type aws:ec2:image \
    --value "$AMI_ID" \
    --overwrite \
    --region "$REGION" --profile "$AWS_PROFILE" > /dev/null
echo ">>> Stored in SSM: $AMI_PARAM"

# --- Terminate builder instance ---
echo ">>> Terminating builder instance..."
aws ec2 terminate-instances \
    --instance-ids "$BUILDER_ID" \
    --region "$REGION" --profile "$AWS_PROFILE" > /dev/null
echo ">>> Builder terminated."

echo ""
echo "=== DTN AMI Build Complete ==="
echo "AMI ID:      $AMI_ID"
echo "AMI Name:    $AMI_NAME"
echo "Region:      $REGION"
echo "SSM:         $AMI_PARAM"
echo ""
echo "Next: update the DTN launch template to use this AMI, then deploy:"
echo "  CDK_DEFAULT_ACCOUNT=\$(aws sts get-caller-identity --query Account --output text)"
echo "  AWS_PROFILE=$AWS_PROFILE cdk deploy ElasticGCS-DTNPool-$REGION-$DEPLOYMENT"
echo "=============================="
