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

# Don't let the instance stop until Globus has removed this node from the
# endpoint's DNS; stopping first leaves a dead IP in DNS (#69). Bounded so
# the stop finishes inside TimeoutStopSec and the lifecycle hook.
TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 60")
PUBLIC_IP=$(curl -s -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/public-ipv4)
[ -n "$PUBLIC_IP" ] && wait_endpoint_dns absent "$PUBLIC_IP" 180 || true

# Note: The SSM slot lock (${PARAM_PREFIX}/slot-lock/{N}) is released by
# the lifecycle Lambda (lambda/lifecycle/main.go) when the instance is permanently
# terminated (Destination=="EC2"). The shutdown script intentionally does NOT
# release the lock here — doing so would allow a new instance to claim the slot
# while this instance is merely entering the warm pool (Warmed:Stopped), causing
# two instances to share the same GCS node identity.

echo "=== DTN Shutdown Complete ==="
