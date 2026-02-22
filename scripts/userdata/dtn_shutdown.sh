#!/bin/bash
set -euo pipefail

# =============================================================================
# DTN Shutdown Script
# =============================================================================
# Runs before instance stop/terminate to gracefully remove the node from
# the GCS endpoint's DNS. This prevents the "endpoint unreliable" state
# documented in the GCS docs.
#
# Called by: systemd unit (ExecStop) or ASG lifecycle hook Lambda
# =============================================================================

LOG="/var/log/elastic-gcs-dtn.log"
exec > >(tee -a "$LOG") 2>&1

echo "=== DTN Shutdown - $(date -u) ==="

# Check if GCS is configured
if [ ! -d "/var/lib/globus-connect-server" ]; then
    echo ">>> No GCS state directory, nothing to disable."
    exit 0
fi

# Disable the node (remove from DNS, stop serving)
echo ">>> Disabling GCS node..."
DISABLE_START=$(date +%s%N)

sudo globus-connect-server node disable 2>&1 || true

# Fix lock file ownership: node disable runs as root, but the GCS web service
# runs as gcsweb. If root creates the subscription_pause lock, the service will
# get Permission Denied on the next node operation. Ensure correct ownership.
chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause 2>/dev/null || true

DISABLE_END=$(date +%s%N)
DISABLE_MS=$(( (DISABLE_END - DISABLE_START) / 1000000 ))
echo ">>> Node disabled in ${DISABLE_MS}ms"

# Note: DNS propagation takes up to 2 minutes per Globus docs.
# The ASG lifecycle hook should allow for this delay.
echo ">>> Node disabled. DNS propagation may take up to 2 minutes."

# Release the SSM slot lock so the slot can be claimed by the next instance.
SLOT_FILE="/var/lib/elastic-gcs/slot"
if [ -f "$SLOT_FILE" ]; then
    DTN_SLOT_NUMBER=$(cat "$SLOT_FILE")
    echo ">>> Releasing SSM slot lock for slot $DTN_SLOT_NUMBER..."
    aws ssm delete-parameter \
        --name "/elastic-gcs/${AWS_REGION}/slot-lock/${DTN_SLOT_NUMBER}" \
        --region "$AWS_REGION" 2>/dev/null \
        && echo ">>> Slot $DTN_SLOT_NUMBER released." \
        || echo "WARNING: Failed to release slot lock (may already be gone)."
    rm -f "$SLOT_FILE"
else
    echo ">>> No slot file found — slot lock not released (first boot or already cleaned up)."
fi

echo "=== DTN Shutdown Complete ==="
