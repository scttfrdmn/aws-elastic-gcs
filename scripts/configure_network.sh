#!/bin/bash
# =============================================================================
# DTN network tuning (#145): ESnet fasterdata host tuning for 10-25 Gbps hosts
# (https://fasterdata.es.net/host-tuning/linux/), which Globus's DTN guidance
# defers to. DTN AMI only: the control node stays on kernel defaults (it carries
# data only while no DTN serves, #146, and its 8 GB is better kept for GCS).
#
# - Larger TCP buffers, so long paths aren't window-limited (Rocky 9 autotunes
#   to ~6 MB, which caps a stream near 480 Mbps at 100 ms RTT).
# - BBR with fq pacing: holds throughput on lossy long paths, where cubic
#   collapses.
# - MTU probing: 9001 inside the VPC, 1500 (or less) across the internet.
#
# Persistent (/etc/sysctl.d) and idempotent. Fails if BBR can't be enabled.
#
# usage (as root): bash scripts/configure_network.sh
# =============================================================================
set -euo pipefail

CONF=/etc/sysctl.d/90-elastic-gcs-fasterdata.conf

cat > /etc/modules-load.d/elastic-gcs-bbr.conf <<'EOF'
# Managed by elastic-gcs configure_network.sh (#145)
tcp_bbr
EOF
modprobe tcp_bbr

cat > "$CONF" <<'EOF'
# Managed by elastic-gcs configure_network.sh (#145) — do not edit manually.
# ESnet fasterdata host tuning for 10-25 Gbps hosts.
net.core.rmem_max = 67108864
net.core.wmem_max = 67108864
net.ipv4.tcp_rmem = 4096 87380 33554432
net.ipv4.tcp_wmem = 4096 65536 33554432
net.ipv4.tcp_congestion_control = bbr
net.core.default_qdisc = fq
net.ipv4.tcp_mtu_probing = 1
EOF
chmod 0644 "$CONF"
sysctl -q -p "$CONF"

# default_qdisc applies to qdiscs created from now on (every boot). To switch a
# running instance too, swap the root for fq and delete it: the kernel then
# reattaches its default (mq on ENA) with fq children. The default mq root
# itself can't be deleted (handle 0) and its children can't be addressed.
IF=$(ip route show default | awk '{print $5; exit}')
if [ -n "$IF" ]; then
    tc qdisc replace dev "$IF" root handle 1: fq
    tc qdisc del dev "$IF" root
fi

cc=$(sysctl -n net.ipv4.tcp_congestion_control)
qd=$(sysctl -n net.core.default_qdisc)
if [ "$cc" != bbr ] || [ "$qd" != fq ]; then
    echo "ERROR: congestion control '$cc', default qdisc '$qd' (want bbr, fq)" >&2
    exit 1
fi
if [ -n "$IF" ] && tc qdisc show dev "$IF" | grep -q fq_codel; then
    echo "ERROR: $IF still has fq_codel queues" >&2
    exit 1
fi
echo "network tuning: bbr + fq, rmem/wmem max 64 MB, autotune to 32 MB, mtu probing ($CONF)"
