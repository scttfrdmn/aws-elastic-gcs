#!/usr/bin/env python3
"""Submit one Globus transfer between two collections and poll it to completion.

Runs on the src control node; credentials come from Secrets Manager via the
instance role (GLOBUS_CLIENT_ID / GLOBUS_CLIENT_SECRET env set by the caller).
Prints one JSON line per poll (progress) and a final JSON summary.

usage: bench_transfer.py SRC_COL DST_COL LABEL ITEM [ITEM ...]
  ITEM = "src_path=>dst_path[:r]"   (":r" = recursive directory)
"""
import json, os, sys, time
import globus_sdk

src_col, dst_col, label, items = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4:]
cid, secret = os.environ["GLOBUS_CLIENT_ID"], os.environ["GLOBUS_CLIENT_SECRET"]

# globus-sdk v4: scopes are immutable; GCS mapped collections need a
# data_access dependency, which service accounts get without interactive consent.
auth = globus_sdk.ConfidentialAppAuthClient(cid, secret)
scope = globus_sdk.scopes.TransferScopes.all
for col in (src_col, dst_col):
    scope = scope.with_dependency(globus_sdk.scopes.GCSCollectionScopes(col).data_access.with_optional(True))
authorizer = globus_sdk.ClientCredentialsAuthorizer(auth, scope)
tc = globus_sdk.TransferClient(authorizer=authorizer)

td = globus_sdk.TransferData(src_col, dst_col, label=label, sync_level=None,
                             verify_checksum=True, notify_on_succeeded=False,
                             notify_on_failed=False, notify_on_inactive=False)
for it in items:
    rec = it.endswith(":r")
    it = it[:-2] if rec else it
    s, d = it.split("=>")
    td.add_item(s, d, recursive=rec)

t0 = time.time()
task_id = tc.submit_transfer(td)["task_id"]
print(json.dumps({"event": "submitted", "task_id": task_id, "t": t0}), flush=True)

last = None
while True:
    t = tc.get_task(task_id)
    row = {k: t.get(k) for k in ("status", "nice_status", "bytes_transferred", "files",
                                 "files_transferred", "directories", "effective_bytes_per_second",
                                 "faults", "subtasks_pending", "subtasks_succeeded",
                                 "subtasks_failed", "subtasks_retrying")}
    row.update(event="progress", t=round(time.time() - t0, 1))
    if row != last:
        print(json.dumps(row), flush=True)
        last = dict(row)
    if t["status"] in ("SUCCEEDED", "FAILED"):
        break
    time.sleep(5)

t = tc.get_task(task_id)
summary = {k: t.get(k) for k in ("task_id", "status", "nice_status", "request_time", "completion_time",
                                 "bytes_transferred", "files_transferred", "effective_bytes_per_second",
                                 "faults", "subtasks_failed", "concurrency" , "parallelism")}
summary.update(event="done", wall_s=round(time.time() - t0, 1),
               gbps=round(8 * (t.get("bytes_transferred") or 0) / max(time.time() - t0, 1) / 1e9, 3))
print(json.dumps(summary), flush=True)
