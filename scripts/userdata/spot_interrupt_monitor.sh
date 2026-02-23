#!/bin/bash
set -euo pipefail

# =============================================================================
# Spot Instance Interruption Monitor
# =============================================================================
# Polls the EC2 Instance Metadata Service every 5 seconds for a Spot
# interruption notice. When a notice is detected (action=terminate or stop),
# gracefully disables the GCS node before the 2-minute countdown expires.
#
# On non-Spot instances the IMDS endpoint returns a 404; the script simply
# loops and sleeps, consuming negligible CPU.
#
# Managed by systemd: elastic-gcs-spot-monitor.service
# Starts when elastic-gcs-dtn.service starts; stops when it stops.
# =============================================================================

LOG="/var/log/elastic-gcs-dtn.log"

log() {
    echo "$(date -u '+%Y-%m-%d %H:%M:%S UTC') [spot-monitor] $*" | tee -a "$LOG"
}

log "Spot interruption monitor started."

while true; do
    sleep 5

    # Fetch IMDSv2 token (short TTL — just for this check)
    TOKEN=$(curl -s -X PUT "http://169.254.169.254/latest/api/token" \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 30" 2>/dev/null || true)

    if [ -z "$TOKEN" ]; then
        continue
    fi

    # Check for spot interruption notice. Returns 404 on non-Spot instances
    # or when no interruption is scheduled. Returns JSON like:
    # {"action":"terminate","time":"2026-02-23T01:00:00Z"}
    ACTION=$(curl -s -f \
        -H "X-aws-ec2-metadata-token: $TOKEN" \
        "http://169.254.169.254/latest/meta-data/spot/instance-action" \
        2>/dev/null || true)

    if echo "$ACTION" | grep -qE '"action"\s*:\s*"(terminate|stop)"'; then
        log "Spot interruption notice received: $ACTION"
        log "Disabling GCS node before shutdown (2-minute window)..."

        # Disable the GCS node so it is removed from DNS before the instance stops.
        # Use || true — node may already be disabled or GCS may not be running.
        globus-connect-server node disable 2>&1 | tee -a "$LOG" || true

        # Fix lock file ownership (node disable runs as root, GCS web service runs as gcsweb).
        chown gcsweb:gcsweb \
            /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause \
            2>/dev/null || true

        log "GCS node disabled. Instance will stop/terminate shortly."
        exit 0
    fi
done
