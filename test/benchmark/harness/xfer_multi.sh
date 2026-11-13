#!/bin/bash
# usage: xfer_multi.sh RUN_PREFIX N SRC_BASE DST_BASE ITEM_SUFFIX... — start N concurrent tasks, task i moves SRC_BASE/<suffix i>/ to DST_BASE/<suffix i>/
RUN=$1; N=$2; SRC=$3; DST=$4; shift 4
H="$(dirname "$0")"; i=0
for s in "$@"; do
  i=$((i+1)); [ $i -gt $N ] && break
  "$H"/xfer_bg.sh "$RUN-$i" "$RUN-$i" "$SRC/$s/=>$DST/$s/:r" 2>/dev/null | cut -c1-90 &
done; wait
