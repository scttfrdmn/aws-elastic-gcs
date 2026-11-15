#!/bin/bash
# =============================================================================
# Control node boot/shutdown hook (#30, #31)
# =============================================================================
# Run by elastic-gcs-control.service on every boot (start) and shutdown (stop).
# cloud-init runs UserData once per instance, so this unit, not UserData, is
# what brings the control node back into the endpoint after a stop/start
# (idle shutdown + waker, scripts/control_node_power.sh, or a manual stop).
#
#   start: enable this GCS node locally and wait (bounded) for its IP to be in
#          endpoint DNS. A booted control node always serves: with the DTN pool
#          at 0 it is the endpoint's only node. Take it out of the data path
#          again with `node disable` once DTNs are active.
#   stop:  disable this GCS node locally and wait (bounded) for its IP to leave
#          endpoint DNS, so a stopped control node never leaves a dead IP behind.
#
# Both are local GCS CLI calls (no Globus credentials) and are idempotent.
# Before control_node_init.sh has set the node up (no info.json) both are no-ops.
# The Elastic IP keeps the node's registered address stable across stop/start.
# =============================================================================
set -uo pipefail

ACTION=${1:?usage: control_node_boot.sh start|stop}
GCS_INFO=/var/lib/globus-connect-server/info.json
LOCK_FILE=/var/lib/globus-connect-server/gcs-manager/locks/subscription_pause
DNS_TIMEOUT=${DNS_TIMEOUT:-180}

log() { echo "[elastic-gcs-control] $*"; }

if [ ! -s "$GCS_INFO" ]; then
    log "GCS not set up yet (no $GCS_INFO); nothing to do"
    exit 0
fi

DOMAIN=$(jq -r '.domain_name // empty' "$GCS_INFO")

public_ip() {
    local token
    token=$(curl -s -X PUT http://169.254.169.254/latest/api/token \
        -H "X-aws-ec2-metadata-token-ttl-seconds: 300")
    curl -s -H "X-aws-ec2-metadata-token: $token" \
        http://169.254.169.254/latest/meta-data/public-ipv4
}

# Root GCS CLI calls leave the subscription_pause lock owned by root, which
# breaks the gcsweb-run manager API until it is handed back.
gcs_local() {
    globus-connect-server "$@" 2>&1 | tail -2
    local rc=${PIPESTATUS[0]}
    chown gcsweb:gcsweb "$LOCK_FILE" 2>/dev/null || true
    return "$rc"
}

wait_dns() {  # present|absent IP TIMEOUT_SEC
    local want=$1 ip=$2 deadline=$(( $(date +%s) + $3 ))
    [ -n "$DOMAIN" ] && [ -n "$ip" ] || { log "no domain or IP; skipping DNS wait"; return 0; }
    while [ "$(date +%s)" -lt "$deadline" ]; do
        if getent ahostsv4 "$DOMAIN" | awk '{print $1}' | grep -qx "$ip"; then
            [ "$want" = present ] && { log "$ip is in $DOMAIN DNS"; return 0; }
        else
            [ "$want" = absent ] && { log "$ip is out of $DOMAIN DNS"; return 0; }
        fi
        sleep 5
    done
    log "WARNING: $ip still not $want in $DOMAIN DNS after ${3}s"
    return 1
}

IP=$(public_ip)
case "$ACTION" in
start)
    log "enabling GCS node ($IP)"
    gcs_local node enable || log "WARNING: node enable failed"
    wait_dns present "$IP" "$DNS_TIMEOUT" || true
    ;;
stop)
    log "disabling GCS node ($IP)"
    gcs_local node disable || log "WARNING: node disable failed"
    wait_dns absent "$IP" "$DNS_TIMEOUT" || true
    ;;
*)
    echo "usage: control_node_boot.sh start|stop" >&2
    exit 2
    ;;
esac
exit 0
