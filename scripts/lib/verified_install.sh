#!/bin/bash
# =============================================================================
# Verified installs for Rocky/RHEL 9 nodes (#46). Sourced by the control node
# UserData, control_node_init.sh and build_ami.sh. Every function fails closed:
# a bad signature or an unexpected key fingerprint stops the install.
#
#   KEY_DIR (default /opt/elastic-gcs/keys) holds the pinned public keys from
#   scripts/keys/: amazon-ssm-agent.asc, aws-cli.asc, globus-2024.asc.
#
# Pinned fingerprints (from the AWS and Globus docs; see scripts/keys/README.md):
SSM_AGENT_FPR="1C5B395CBE5E74E2F1CA21592784DBF388D19D46"  # expires 2027-11-25
AWS_CLI_FPR="FB5DB77FD5C118B80511ADA8A6310ACC4672475C"    # expires 2027-07-01
GLOBUS_FPR="FD9528E6EE731D8264C295D7C934C57C39AF3E61"     # RPM-GPG-KEY-Globus-2024
# =============================================================================
KEY_DIR="${KEY_DIR:-/opt/elastic-gcs/keys}"

vi_log() { echo "[verified-install] $*"; }
vi_die() { echo "[verified-install] ERROR: $*" >&2; exit 1; }

# vi_check_key FILE FINGERPRINT: the key file must contain exactly that primary key.
vi_check_key() {
    local got
    got=$(gpg --batch --show-keys --with-colons "$1" 2>/dev/null | awk -F: '$1=="fpr"{print $10; exit}')
    [ "$got" = "$2" ] || vi_die "key $1 has fingerprint '${got:-none}', want $2"
}

# vi_verify_sig KEYFILE FINGERPRINT SIG FILE: detached signature check in a
# throwaway keyring holding only the pinned key.
vi_verify_sig() {
    local home rc
    vi_check_key "$1" "$2"
    home=$(mktemp -d)
    GNUPGHOME="$home" gpg --batch --quiet --import "$1" 2>/dev/null
    GNUPGHOME="$home" gpg --batch --status-fd 1 --verify "$3" "$4" 2>/dev/null | grep -q "^\[GNUPG:\] VALIDSIG $2 "
    rc=$?
    rm -rf "$home"
    [ $rc -eq 0 ] || vi_die "signature check failed for $4"
}

# install_ssm_agent REGION: the agent RPM from the regional bucket, signature verified.
install_ssm_agent() {
    local region=$1 tmp base
    command -v amazon-ssm-agent >/dev/null && { vi_log "SSM agent already installed"; return 0; }
    tmp=$(mktemp -d)
    base="https://s3.${region}.amazonaws.com/amazon-ssm-${region}/latest/linux_amd64"
    curl -sSf --retry 5 -o "$tmp/agent.rpm" "$base/amazon-ssm-agent.rpm" || vi_die "SSM agent download failed"
    curl -sSf --retry 5 -o "$tmp/agent.rpm.sig" "$base/amazon-ssm-agent.rpm.sig" || vi_die "SSM agent signature download failed"
    vi_verify_sig "$KEY_DIR/amazon-ssm-agent.asc" "$SSM_AGENT_FPR" "$tmp/agent.rpm.sig" "$tmp/agent.rpm"
    dnf install -y "$tmp/agent.rpm" || vi_die "SSM agent install failed"
    rm -rf "$tmp"
    systemctl enable --now amazon-ssm-agent
    vi_log "SSM agent installed (signature verified)"
}

# install_aws_cli: AWS CLI v2, signature verified.
install_aws_cli() {
    local tmp url=https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip
    command -v aws >/dev/null && { vi_log "AWS CLI already installed"; return 0; }
    command -v unzip >/dev/null || dnf install -y unzip || vi_die "unzip install failed"
    tmp=$(mktemp -d)
    curl -sSf --retry 5 -o "$tmp/awscliv2.zip" "$url" || vi_die "AWS CLI download failed"
    curl -sSf --retry 5 -o "$tmp/awscliv2.sig" "$url.sig" || vi_die "AWS CLI signature download failed"
    vi_verify_sig "$KEY_DIR/aws-cli.asc" "$AWS_CLI_FPR" "$tmp/awscliv2.sig" "$tmp/awscliv2.zip"
    (cd "$tmp" && unzip -q awscliv2.zip && ./aws/install) || vi_die "AWS CLI install failed"
    ln -sf /usr/local/bin/aws /usr/bin/aws
    rm -rf "$tmp"
    vi_log "AWS CLI installed (signature verified)"
}

# install_epel: from the distribution's own signed extras repo, not a URL.
install_epel() {
    rpm -q epel-release >/dev/null 2>&1 && return 0
    dnf install -y epel-release || vi_die "epel-release install failed"
    vi_log "EPEL enabled (epel-release from the distribution repo)"
}

# setup_globus_repo: the GCS stable repo, defined here rather than by the
# globus-repo RPM (whose repo file would decide which keys dnf trusts), with
# the pinned Globus key. Packages and metadata are GPG-checked.
setup_globus_repo() {
    local key=/etc/pki/rpm-gpg/RPM-GPG-KEY-Globus-2024
    vi_check_key "$KEY_DIR/globus-2024.asc" "$GLOBUS_FPR"
    install -m 0644 "$KEY_DIR/globus-2024.asc" "$key"
    rpm --import "$key" || vi_die "rpm --import of the Globus key failed"
    cat > /etc/yum.repos.d/globus-connect-server-5-stable.repo <<REPO
[Globus-Connect-Server-5-Stable]
name=Globus Connect Server 5 Stable
baseurl=https://downloads.globus.org/globus-connect-server/stable/rpm/el/\$releasever/\$basearch/
enabled=1
priority=98
gpgcheck=1
repo_gpgcheck=1
gpgkey=file://$key
REPO
    vi_log "Globus repo configured with pinned key $GLOBUS_FPR"
}
