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
#   - GCS 5.4 (globus-connect-server54)
#   - SSM Agent (amazon-ssm-agent)
#   - jq, awscli
#
# Usage:
#   AWS_PROFILE=aws ./scripts/build_ami.sh [region]
#   AWS_PROFILE=aws ./scripts/build_ami.sh us-east-1
# =============================================================================

REGION="${1:-us-east-1}"
AWS_PROFILE="${AWS_PROFILE:-default}"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
AMI_NAME="elastic-gcs-dtn-${TIMESTAMP}"

echo "=== Building DTN AMI ==="
echo "Region:   $REGION"
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
    --name "/elastic-gcs/${REGION}/control-node-id" \
    --query Parameter.Value --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

read -r SUBNET_ID SG_ID IAM_PROFILE_ARN < <(aws ec2 describe-instances \
    --instance-ids "$CONTROL_INSTANCE" \
    --query 'Reservations[0].Instances[0].[SubnetId,SecurityGroups[0].GroupId,IamInstanceProfile.Arn]' \
    --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

echo ">>> Subnet: $SUBNET_ID  SG: $SG_ID"
echo ">>> IAM Profile: $IAM_PROFILE_ARN"

# Extract profile name from ARN
IAM_PROFILE_NAME="${IAM_PROFILE_ARN##*/}"

# --- UserData: ONLY install SSM Agent so we can manage the rest via SSM ---
# Doing package installs in UserData causes RPM lock races with cloud-init.
# Instead we launch a bare instance, wait for SSM, then run installs via SSM.
USERDATA=$(cat <<'USERDATA_EOF'
#!/bin/bash
# Minimal UserData — just get SSM agent running so we can take over via SSM
dnf install -y https://s3.amazonaws.com/ec2-downloads-windows/SSMAgent/latest/linux_amd64/amazon-ssm-agent.rpm 2>/dev/null || true
systemctl enable amazon-ssm-agent
systemctl start amazon-ssm-agent
USERDATA_EOF
)

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

echo "Installing tools..."
dnf install -y jq unzip

echo "Installing AWS CLI v2..."
curl -s https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip -o /tmp/awscliv2.zip
cd /tmp && unzip -q awscliv2.zip
/tmp/aws/install
ln -sf /usr/local/bin/aws /usr/bin/aws
aws --version
cd /

echo "Installing EPEL..."
. /etc/os-release
OS_VER="${VERSION_ID%%.*}"
dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-${OS_VER}.noarch.rpm || true
rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-EPEL-9 || true

echo "Installing GCS repo..."
dnf install -y https://downloads.globus.org/globus-connect-server/stable/installers/repo/rpm/globus-repo-latest.noarch.rpm
rpm --import /etc/pki/rpm-gpg/RPM-GPG-KEY-Globus-2024 || true

echo "Installing GCS..."
dnf install -y globus-connect-server54

echo "Verifying..."
globus-connect-server --version

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

# --- Configure TLS cipher policy ---
echo ">>> Applying TLS cipher policy..."
TLS_SCRIPT=$(cat "$(dirname "$0")/configure_tls.sh" | base64 | tr -d '\n')
TLS_CMD=$(aws ssm send-command \
    --instance-ids "$BUILDER_ID" \
    --document-name AWS-RunShellScript \
    --timeout-seconds 120 \
    --parameters "commands=[\"python3 -c \\\"import base64; open('/tmp/configure-tls.sh','wb').write(base64.b64decode('${TLS_SCRIPT}'))\\\"\", \"bash /tmp/configure-tls.sh\"]" \
    --query 'Command.CommandId' --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

for i in $(seq 1 12); do
    sleep 10
    STATUS=$(aws ssm get-command-invocation \
        --command-id "$TLS_CMD" \
        --instance-id "$BUILDER_ID" \
        --query 'Status' --output text \
        --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null || echo "Pending")
    if [ "$STATUS" = "Success" ] || [ "$STATUS" = "Failed" ]; then
        echo ">>> TLS config status: $STATUS"
        break
    fi
    echo "    ($i/12) Status: $STATUS — waiting..."
done
aws ssm get-command-invocation \
    --command-id "$TLS_CMD" \
    --instance-id "$BUILDER_ID" \
    --query 'StandardOutputContent' --output text \
    --region "$REGION" --profile "$AWS_PROFILE" 2>/dev/null | tail -10 || true

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
    --description "Elastic GCS DTN - GCS v5.4 pre-installed on Rocky Linux 9 x86_64" \
    --no-reboot \
    --tag-specifications \
        "ResourceType=image,Tags=[{Key=Name,Value=${AMI_NAME}},{Key=elastic-gcs:purpose,Value=dtn},{Key=elastic-gcs:region,Value=${REGION}}]" \
    --query 'ImageId' --output text \
    --region "$REGION" --profile "$AWS_PROFILE")

echo ">>> AMI ID: $AMI_ID (building — this takes 2-5 minutes)"

# --- Store AMI ID in SSM ---
aws ssm put-parameter \
    --name "/elastic-gcs/${REGION}/dtn-ami-id" \
    --type String \
    --value "$AMI_ID" \
    --overwrite \
    --region "$REGION" --profile "$AWS_PROFILE"
echo ">>> Stored in SSM: /elastic-gcs/${REGION}/dtn-ami-id"

# --- Wait for AMI to be available ---
echo ">>> Waiting for AMI to become available..."
aws ec2 wait image-available \
    --image-ids "$AMI_ID" \
    --region "$REGION" --profile "$AWS_PROFILE"
echo ">>> AMI is available."

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
echo "SSM:         /elastic-gcs/${REGION}/dtn-ami-id"
echo ""
echo "Next: update the DTN launch template to use this AMI, then deploy:"
echo "  CDK_DEFAULT_ACCOUNT=\$(aws sts get-caller-identity --query Account --output text)"
echo "  AWS_PROFILE=$AWS_PROFILE cdk deploy ElasticGCS-DTNPool-$REGION"
echo "=============================="
