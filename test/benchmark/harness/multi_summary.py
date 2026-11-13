# usage: multi_summary.py RUN_DIR RUN_PREFIX — aggregate concurrent-task runs (first request -> last completion)
import json, glob, os, sys, datetime as dt
d, pre = sys.argv[1], sys.argv[2]
rows = []
for f in sorted(glob.glob(os.path.join(d, pre + "-*.jsonl"))):
    done = [json.loads(l) for l in open(f) if '"event": "done"' in l]
    if done: rows.append(done[-1])
p = lambda s: dt.datetime.fromisoformat(s)
t0 = min(p(r["request_time"]) for r in rows); t1 = max(p(r["completion_time"]) for r in rows)
b = sum(r["bytes_transferred"] for r in rows); w = (t1 - t0).total_seconds()
print(json.dumps({"tasks": len(rows), "bytes": b, "wall_s": w, "aggregate_gbps": round(8 * b / w / 1e9, 3),
                  "per_task_gbps": [round(8 * r["bytes_transferred"] / max(r["wall_s"], 1) / 1e9, 2) for r in rows],
                  "faults": sum(r.get("faults") or 0 for r in rows)}))
