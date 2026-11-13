#!/usr/bin/env python3
"""Build runs.json from raw run logs: one record per run with the final task summary.

usage: summarize.py RESULTS_DIR   (reads RESULTS_DIR/raw/*.jsonl, writes RESULTS_DIR/runs.json)
Cancelled runs are kept with status CANCELLED and their last progress sample.
"""
import glob, json, os, sys

d = sys.argv[1]
runs = {}
for f in sorted(glob.glob(os.path.join(d, "raw", "*.jsonl"))):
    run = os.path.basename(f)[:-6]
    lines = [json.loads(l) for l in open(f) if l.startswith("{")]
    sub = next((l for l in lines if l.get("event") == "submitted"), {})
    done = next((l for l in reversed(lines) if l.get("event") == "done"), None)
    last = next((l for l in reversed(lines) if l.get("event") == "progress"), {})
    r = {"run": run, "task_id": sub.get("task_id")}
    if done:
        r.update({k: done.get(k) for k in ("status", "wall_s", "bytes_transferred", "files_transferred",
                                           "effective_bytes_per_second", "faults")})
        r["wall_gbps"] = round(8 * (done.get("bytes_transferred") or 0) / max(done.get("wall_s") or 1, 1) / 1e9, 3)
    else:
        r.update({"status": "CANCELLED", "elapsed_s": last.get("t"),
                  "bytes_transferred": last.get("bytes_transferred"), "files_transferred": last.get("files_transferred"),
                  "faults": last.get("faults")})
    runs[run] = r
json.dump({"runs": runs}, open(os.path.join(d, "runs.json"), "w"), indent=1, sort_keys=True)
print(f"{len(runs)} runs -> {os.path.join(d, 'runs.json')}")
for r in runs.values():
    print(f"  {r['run']:24} {r['status']:10} wall={r.get('wall_s')} gbps={r.get('wall_gbps')} faults={r.get('faults')}")
