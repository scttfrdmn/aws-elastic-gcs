#!/bin/bash
# usage (as root, on a DTN or control node): dtn_sampler.sh OUT_PREFIX DURATION_S
# 1 s utilization samples for the envelope study (#151), the USE method's raw data.
#   OUT_PREFIX.nic   every 1 s: epoch rx_bytes tx_bytes cpu_busy cpu_total mem_avail_kb tcp_retrans gridftp_procs
#   OUT_PREFIX.proc  every 5 s: "P epoch pid utime stime" per GridFTP process, and
#                    "C epoch cpuN busy total" per core (per-process and per-core saturation)
# Stops after DURATION_S or when OUT_PREFIX.stop exists. Counters are cumulative;
# rates are computed offline (steady.py), so a missed sample loses nothing.
set -u
OUT=$1; DUR=$2
IF=$(ip route show default | awk '{print $5; exit}')
NIC=/sys/class/net/$IF/statistics
end=$(( $(date +%s) + DUR ))
: > "$OUT.nic"; : > "$OUT.proc"; rm -f "$OUT.stop"
i=0
while [ "$(date +%s)" -lt "$end" ] && [ ! -e "$OUT.stop" ]; do
  now=$(date +%s.%N)
  read -r rx < "$NIC/rx_bytes"; read -r tx < "$NIC/tx_bytes"
  cpu=$(awk '/^cpu /{print $2+$3+$4+$7+$8+$9, $2+$3+$4+$5+$6+$7+$8+$9; exit}' /proc/stat)
  mem=$(awk '/^MemAvailable:/{print $2; exit}' /proc/meminfo)
  ret=$(awk '/^Tcp:/{if (h) {for (k = 1; k <= NF; k++) if (n[k] == "RetransSegs") print $k; exit} else {for (k = 1; k <= NF; k++) n[k] = $k; h = 1}}' /proc/net/snmp)
  pids=$(pgrep -f '^[^ ]*globus-gridftp-server' || true)   # the server binary, not shells mentioning it
  np=$(printf '%s\n' "$pids" | grep -c . || true)
  echo "$now $rx $tx $cpu $mem ${ret:-0} $np" >> "$OUT.nic"
  if [ $((i % 5)) -eq 0 ]; then
    for p in $pids; do
      [ -r "/proc/$p/stat" ] && awk -v t="$now" -v p="$p" '{sub(/^.*\) /, ""); print "P", t, p, $12, $13}' "/proc/$p/stat"
    done >> "$OUT.proc"
    awk -v t="$now" '/^cpu[0-9]/{print "C", t, $1, $2+$3+$4+$7+$8+$9, $2+$3+$4+$5+$6+$7+$8+$9}' /proc/stat >> "$OUT.proc"
  fi
  i=$((i + 1))
  sleep 1
done
