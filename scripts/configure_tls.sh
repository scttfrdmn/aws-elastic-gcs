#!/bin/bash
set -euo pipefail

# =============================================================================
# TLS Cipher Configuration Script
# =============================================================================
# Applies organization-approved TLS cipher policy to all GCS TLS surfaces.
#
# Why this exists: GCS v5 manages its own Apache VirtualHost-level
# SSLCipherSuite settings and REGENERATES them whenever collections change.
# They are marked "DO NOT EDIT". Direct Apache edits are overwritten.
#
# Solution: Rocky/RHEL 9 system crypto-policy constrains OpenSSL at the OS
# level. Apache (and every other OpenSSL consumer) cannot offer ciphers that
# the policy has removed, regardless of what GCS writes in its VirtualHost.
# GridFTP data channels are configured separately via /etc/grid-security/gsi.conf.
#
# Approved ciphers (IANA → OpenSSL format):
#   TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256  → ECDHE-ECDSA-AES128-GCM-SHA256
#   TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384  → ECDHE-ECDSA-AES256-GCM-SHA384
#   TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256  → ECDHE-ECDSA-AES128-SHA256
#   TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384  → ECDHE-ECDSA-AES256-SHA384
#   TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256    → ECDHE-RSA-AES128-GCM-SHA256
#   TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384    → ECDHE-RSA-AES256-GCM-SHA384
#   TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256    → ECDHE-RSA-AES128-SHA256
#   TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384    → ECDHE-RSA-AES256-SHA384
#
# NOTE on TLS 1.3: the approved list contains only TLS 1.2 cipher names.
# TLS 1.3 mandates its own built-in cipher suites (TLS_AES_128/256_GCM_*)
# which cannot be individually disabled — only TLS 1.3 as a whole can be
# disabled. TLS 1.3 is strictly more secure than 1.2. This script sets the
# minimum to TLS 1.2 but does NOT disable TLS 1.3.
# To restrict to TLS 1.2 only (e.g. compliance requirement), uncomment the
# max_tls_version line in the policy module below.
#
# Usage: run as root on any GCS node (control or DTN)
#   bash scripts/configure_tls.sh
# =============================================================================

LOG=/var/log/elastic-gcs-tls-config.log
exec > >(tee -a "$LOG") 2>&1
echo "=== Elastic GCS TLS Cipher Configuration - $(date -u) ==="

# Approved cipher list in OpenSSL colon-separated format (for gsi.conf / direct use)
APPROVED_CIPHERS="ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-ECDSA-AES128-SHA256:ECDHE-ECDSA-AES256-SHA384:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-RSA-AES128-SHA256:ECDHE-RSA-AES256-SHA384"

# =============================================================================
# 1. RHEL/Rocky Linux 9 system crypto-policy
# =============================================================================
# Removes from the DEFAULT policy:
#   - CHACHA20-POLY1305: not in approved list
#   - FFDHE groups: DHE key exchange not in approved list (approved is ECDHE-only)
# Sets minimum TLS version to 1.2.
#
# The system crypto-policy is a hard floor: OpenSSL will not negotiate any
# cipher removed by the policy, even if Apache's SSLCipherSuite lists it.
# =============================================================================
if command -v update-crypto-policies &>/dev/null; then
    echo ">>> Configuring system crypto-policy (Rocky/RHEL 9)..."

    POLICY_DIR="/etc/crypto-policies/policies/modules"
    POLICY_FILE="${POLICY_DIR}/ELASTIC-GCS.pmod"
    mkdir -p "$POLICY_DIR"

    cat > "$POLICY_FILE" <<'PMOD'
# Elastic GCS approved cipher policy
# Managed by elastic-gcs configure_tls.sh — do not edit manually
#
# Syntax: RHEL 9 crypto-policy module format (.pmod)
#   property = -value       remove this value from the DEFAULT policy list
#   property = -prefix-*    remove all values matching the prefix wildcard
#
# NOTE: `key_exchange = ECDHE ECDHE-PSK` is intentionally omitted.
# Testing confirmed it breaks TLS connectivity to auth.globus.org (server
# responds with close_notify). Removing FFDHE groups via `group = -FFDHE-*`
# is sufficient to eliminate DHE key exchange without affecting ECDHE.

# Remove all finite-field DH groups — disables DHE-RSA-* cipher suites.
# ECDHE groups (X25519, P-256, P-384 etc.) are unaffected.
group = -FFDHE-*

# Remove CHACHA20-POLY1305 (not in approved list).
cipher = -CHACHA20-POLY1305

# The DEFAULT policy already sets minimum TLS version to 1.2.
# To restrict to TLS 1.2 ONLY (disables TLS 1.3), uncomment:
# protocol = TLS1.2
PMOD

    update-crypto-policies --set DEFAULT:ELASTIC-GCS
    echo ">>> Crypto policy applied."
    echo ">>> Active policy: $(update-crypto-policies --show)"
else
    echo "WARNING: update-crypto-policies not found. This script targets Rocky/RHEL 9."
    echo "         For non-RHEL systems, apply Apache SSLCipherSuite and GridFTP gsi.conf manually."
fi

# =============================================================================
# 2. GridFTP data channel TLS (/etc/grid-security/gsi.conf)
# =============================================================================
# The system crypto-policy already constrains OpenSSL for GridFTP, but
# gsi.conf provides an explicit cipher list as a second layer of defense and
# sets the minimum protocol version at the GridFTP layer.
# =============================================================================
GSI_CONF="/etc/grid-security/gsi.conf"

apply_gsi_conf() {
    local conf_file="$1"
    local tmpfile
    tmpfile=$(mktemp)

    # Preserve any lines we're not overriding, update the three we own
    grep -v -E '^(MIN_TLS_PROTOCOL|MAX_TLS_PROTOCOL|CIPHERS|SERVER_CIPHER_ORDER)=' \
        "$conf_file" 2>/dev/null > "$tmpfile" || true

    cat >> "$tmpfile" <<EOF
# Elastic GCS approved ciphers — managed by configure_tls.sh
MIN_TLS_PROTOCOL=TLS1_2_VERSION
CIPHERS=${APPROVED_CIPHERS}
SERVER_CIPHER_ORDER=false
EOF
    mv "$tmpfile" "$conf_file"
}

mkdir -p "$(dirname "$GSI_CONF")"

if [ -f "$GSI_CONF" ]; then
    echo ">>> Updating GridFTP TLS settings in $GSI_CONF..."
    cp "${GSI_CONF}" "${GSI_CONF}.pre-elastic-gcs.bak"
    apply_gsi_conf "$GSI_CONF"
else
    echo ">>> $GSI_CONF not present — writing defaults (GCS node setup will merge these)..."
    touch "$GSI_CONF"
    apply_gsi_conf "$GSI_CONF"
fi

echo ">>> GridFTP TLS configured."

# =============================================================================
# 3. Reload services
# =============================================================================
if systemctl is-active --quiet httpd 2>/dev/null; then
    echo ">>> Reloading Apache httpd to apply crypto-policy..."
    systemctl reload httpd
    echo ">>> httpd reloaded."
fi

if systemctl is-active --quiet globus-gridftp-server 2>/dev/null; then
    echo ">>> Restarting GridFTP server to apply gsi.conf changes..."
    systemctl restart globus-gridftp-server
    echo ">>> GridFTP restarted."
fi

echo ""
echo "=== TLS Configuration Complete ==="
echo "Active policy : $(update-crypto-policies --show 2>/dev/null || echo 'N/A (non-RHEL)')"
echo "GridFTP config: $GSI_CONF"
echo "Log           : $LOG"
echo ""
echo "Removed from DEFAULT: CHACHA20-POLY1305, DHE (FFDHE groups)"
echo "Approved ciphers     : ${APPROVED_CIPHERS//:/ }"
