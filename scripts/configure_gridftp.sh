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
# Second, Globus's interim workaround for the root cause (Globus support,
# 2026-11, a GCS bug they are fixing): on a node-list or endpoint-setting
# change the GCS Manager skips rewriting an unchanged GridFTP config but still
# resets its ownership, which changes its ctime and fires the restarter. An
# ExecCondition on gridftp-server-restarter.service now restarts GridFTP only
# when the config's contents changed. If the file can't be read the restart
# is allowed, so a real change is never missed. Remove both drop-ins once GCS
# ships the fix. ("This workaround ... has not been vetted as thoroughly as
# released GCS code." - Globus)
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

# Globus's script, verbatim apart from this comment block.
cat > /usr/local/sbin/gcs-gridftp-conf-changed <<'SH'
#!/bin/sh
# Exit 0 (allow the restart) only if the GridFTP config contents changed
# since the last restart. Any non-zero exit skips the restart.
conf=/var/lib/globus-connect-server/gridftp.d/globus-connect-server
state=/var/local/gcs-gridftp-conf.sha256

new=$(sha256sum "$conf") || exit 0
[ "$new" = "$(cat "$state" 2>/dev/null)" ] && exit 1
echo "$new" > "$state"
SH
chmod 0755 /usr/local/sbin/gcs-gridftp-conf-changed
RDIR=/etc/systemd/system/gridftp-server-restarter.service.d
mkdir -p "$RDIR"
cat > "$RDIR/override.conf" <<'UNIT'
# Managed by elastic-gcs configure_gridftp.sh (#141); Globus's interim
# workaround: restart GridFTP only when its config contents change.
[Service]
ExecCondition=/usr/local/sbin/gcs-gridftp-conf-changed
UNIT
chmod 0644 "$RDIR/override.conf"
systemctl daemon-reload
# Record the current contents (a node that's already set up), so the next
# metadata-only touch doesn't restart GridFTP. On a fresh AMI the config
# doesn't exist yet; the first real change then seeds the state.
GCONF=/var/lib/globus-connect-server/gridftp.d/globus-connect-server
mkdir -p /var/local
if [ -r "$GCONF" ]; then sha256sum "$GCONF" > /var/local/gcs-gridftp-conf.sha256; fi

mode=$(systemctl show globus-gridftp-server -p KillMode --value)
if [ "$mode" != process ]; then
    echo "ERROR: globus-gridftp-server KillMode is '$mode', expected 'process'" >&2
    exit 1
fi
cond=$(systemctl show gridftp-server-restarter -p ExecCondition --value)
case "$cond" in *gcs-gridftp-conf-changed*) ;; *) echo "ERROR: restarter ExecCondition not set" >&2; exit 1;; esac
echo "globus-gridftp-server: KillMode=process ($CONF); restarter gated on config contents ($RDIR/override.conf)"
