#!/bin/bash
# usage (as root, on a DTN): posix_synth.sh LIST ROOT [JOBS]
# Builds a synthetic POSIX dataset for the envelope study (#151): LIST holds
# "size relpath" lines (synth.py writes it). One random seed per distinct size
# is written with dd, then copied into ROOT with JOBS parallel cp (no reflinks,
# so every file has its own blocks). Files already present at the right size
# are skipped, so a rerun resumes. Seeds live beside ROOT, never inside it.
# Writes ROOT.done when finished; caches are dropped so reads hit the volume.
set -euo pipefail
LIST=$1; ROOT=$2; JOBS=${3:-8}
SEEDS="$(dirname "$ROOT")/.synth-seeds"
mkdir -p "$ROOT" "$SEEDS"
rm -f "$ROOT.done"

awk '{print $1}' "$LIST" | sort -un | while read -r s; do
  f="$SEEDS/$s"
  if [ -f "$f" ] && [ "$(stat -c %s "$f")" = "$s" ]; then continue; fi
  bs=1048576; full=$((s / bs)); rem=$((s % bs))
  : > "$f"
  if [ "$full" -gt 0 ]; then dd if=/dev/urandom of="$f" bs=$bs count=$full iflag=fullblock status=none; fi
  if [ "$rem" -gt 0 ]; then head -c "$rem" /dev/urandom >> "$f"; fi
  echo "seed $s"
done

awk '{print $2}' "$LIST" | sed 's|/[^/]*$||' | sort -u | (cd "$ROOT" && xargs -r mkdir -p)

export SEEDS ROOT
# shellcheck disable=SC2016  # expanded by the inner sh, not here
xargs -n 2 -P "$JOBS" sh -c 'f="$ROOT/$2"; if [ -f "$f" ] && [ "$(stat -c %s "$f")" = "$1" ]; then exit 0; fi; cp --reflink=never "$SEEDS/$1" "$f"' _ < "$LIST"

sync
echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || echo "warning: could not drop caches" >&2
n=$(wc -l < "$LIST"); b=$(awk '{s+=$1} END {printf "%d", s}' "$LIST")
echo "done files=$n bytes=$b root=$ROOT" | tee "$ROOT.done"
