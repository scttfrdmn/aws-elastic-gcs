#!/bin/bash
# =============================================================================
# Keep in-flight GridFTP sessions alive across GridFTP restarts (#141).
#
# GCS restarts globus-gridftp-server on every node whenever any node joins or
# leaves the endpoint: its manager touches /etc/gridftp.d/globus-connect-server
# and gridftp-server-restarter.path (PathModified) fires, even when the config
# is unchanged. `node disable` triggers it too. GridFTP is a forking daemon
# (one child per session), and the unit's default KillMode=control-group kills
# every session on restart, so each scale event reset every active transfer.
#
# With KillMode=process only the listener restarts: new sessions get the new
# config, existing sessions finish in their own processes. A drain becomes a
# real drain (sessions finish; DNS sends new work elsewhere). Instance stop and
# poweroff still end everything. Measured live: 128/128 sessions survived a
# restart, a disable and an enable with no task faults (#141).
#
# A systemd drop-in, so GCS package upgrades leave it alone; delete the file
# to revert. Idempotent.
#
# usage (as root): bash scripts/configure_gridftp.sh
# =============================================================================
set -euo pipefail

DIR=/etc/systemd/system/globus-gridftp-server.service.d
CONF=$DIR/50-elastic-gcs-keep-sessions.conf

mkdir -p "$DIR"
cat > "$CONF" <<'EOF'
# Managed by elastic-gcs configure_gridftp.sh (#141) — do not edit manually.
# Restart only the GridFTP listener; sessions finish in their own processes.
[Service]
KillMode=process
EOF
chmod 0644 "$CONF"
systemctl daemon-reload

mode=$(systemctl show globus-gridftp-server -p KillMode --value)
if [ "$mode" != process ]; then
    echo "ERROR: globus-gridftp-server KillMode is '$mode', expected 'process'" >&2
    exit 1
fi
echo "globus-gridftp-server: KillMode=process ($CONF)"
