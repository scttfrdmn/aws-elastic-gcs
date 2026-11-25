#!/usr/bin/env python3
"""Submit one Globus transfer between two collections and poll it to completion.

Runs on the src control node; credentials come from Secrets Manager via the
instance role (GLOBUS_CLIENT_ID / GLOBUS_CLIENT_SECRET env set by the caller).
Prints one JSON line per poll (progress) and a final JSON summary.

usage: bench_transfer.py SRC_COL DST_COL LABEL ITEM [ITEM ...]
  ITEM = "src_path=>dst_path[:r]"   (":r" = recursive directory)

Optional env (all default to the previous behaviour):
  VERIFY_CHECKSUM=0|1     checksum verification (default 1)
  ENCRYPT_DATA=0|1        data-channel encryption; unset = collection default
  CANCEL_FILE=PATH        when PATH appears, cancel the task and keep polling to the end
  POLL_S=N                poll interval in seconds (default 5)
  NEGOTIATED_AFTER_S=N    log the STARTED events once the task is this old (default 30)
The "done" line is followed by an "events" line: STARTED event details
(negotiated concurrency/parallelism), event counts by code, and the first errors.
"""
import json
import os
import re
import sys
import time


def parse_started(details):
    """Negotiated settings from a STARTED event's `details`.

    Transfer has reported these as a JSON object string ({"concurrency": 4,
    "parallelism": 8, ...}) and as free text ("concurrency=4 parallelism=8");
    accept either, plus nested JSON. Returns {} when nothing is found.
    """
    out = {}
    if details is None:
        return out
    obj = details
    if isinstance(details, str):
        try:
            obj = json.loads(details)
        except ValueError:
            obj = None
    if isinstance(obj, dict):
        stack = [obj]
        while stack:
            d = stack.pop()
            for k, v in d.items():
                lk = str(k).lower()
                if isinstance(v, dict):
                    stack.append(v)
                elif lk in ("concurrency", "parallelism", "pipelining", "pipeline_depth") and _int(v) is not None:
                    out.setdefault(lk, _int(v))
                elif lk in ("type", "protocol"):
                    out.setdefault(lk, v)
        if out:
            return out
    text = details if isinstance(details, str) else json.dumps(details)
    for k in ("concurrency", "parallelism", "pipelining"):
        m = re.search(rf"{k}\W{{0,3}}(\d+)", text, re.I)
        if m:
            out[k] = int(m.group(1))
    return out


def _int(v):
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


def task_events(tc, task_id, limit=1000):
    events, off = [], 0
    while True:
        r = tc.task_event_list(task_id, limit=limit, offset=off)
        data = r.get("DATA", []) if hasattr(r, "get") else list(r)
        events.extend(dict(e) for e in data)
        total = r.get("total") if hasattr(r, "get") else None
        off += len(data)
        if not data or total is None or off >= total:
            return events


def events_summary(events):
    started = [{"time": e.get("time"), "details": e.get("details"), "negotiated": parse_started(e.get("details"))}
               for e in events if e.get("code") == "STARTED"]
    counts = {}
    for e in events:
        counts[e.get("code")] = counts.get(e.get("code"), 0) + 1
    errors = [{k: e.get(k) for k in ("time", "code", "description", "details")}
              for e in events if e.get("is_error")][:50]
    return {"started": started, "counts": counts, "errors": errors}


def main(argv):
    import globus_sdk  # noqa: PLC0415 - only on the control node

    src_col, dst_col, label, items = argv[1], argv[2], argv[3], argv[4:]
    cid, secret = os.environ["GLOBUS_CLIENT_ID"], os.environ["GLOBUS_CLIENT_SECRET"]
    poll_s = float(os.environ.get("POLL_S", "5"))
    cancel_file = os.environ.get("CANCEL_FILE")
    neg_after = float(os.environ.get("NEGOTIATED_AFTER_S", "30"))

    # globus-sdk v4: scopes are immutable; GCS mapped collections need a
    # data_access dependency, which service accounts get without interactive consent.
    auth = globus_sdk.ConfidentialAppAuthClient(cid, secret)
    scope = globus_sdk.scopes.TransferScopes.all
    for col in (src_col, dst_col):
        scope = scope.with_dependency(globus_sdk.scopes.GCSCollectionScopes(col).data_access.with_optional(True))
    authorizer = globus_sdk.ClientCredentialsAuthorizer(auth, scope)
    tc = globus_sdk.TransferClient(authorizer=authorizer)

    kw = {}
    if os.environ.get("ENCRYPT_DATA") in ("0", "1"):
        kw["encrypt_data"] = os.environ["ENCRYPT_DATA"] == "1"
    td = globus_sdk.TransferData(src_col, dst_col, label=label, sync_level=None,
                                 verify_checksum=os.environ.get("VERIFY_CHECKSUM", "1") != "0", notify_on_succeeded=False,
                                 notify_on_failed=False, notify_on_inactive=False, **kw)
    for it in items:
        rec = it.endswith(":r")
        it = it[:-2] if rec else it
        s, d = it.split("=>")
        td.add_item(s, d, recursive=rec)

    t0 = time.time()
    task_id = tc.submit_transfer(td)["task_id"]
    print(json.dumps({"event": "submitted", "task_id": task_id, "t": t0, "options": kw,
                      "verify_checksum": td.get("verify_checksum")}), flush=True)

    last, cancelled, neg_logged = None, False, False
    while True:
        t = tc.get_task(task_id)
        row = {k: t.get(k) for k in ("status", "nice_status", "bytes_transferred", "files", "files_transferred",
                                     "directories", "effective_bytes_per_second", "faults", "subtasks_pending",
                                     "subtasks_succeeded", "subtasks_failed", "subtasks_retrying")}
        row.update(event="progress", t=round(time.time() - t0, 1))
        if row != last:
            print(json.dumps({**row, "epoch": round(time.time(), 3)}), flush=True)
            last = dict(row)
        if t["status"] in ("SUCCEEDED", "FAILED"):
            break
        if not neg_logged and time.time() - t0 >= neg_after:
            try:
                ev = events_summary(task_events(tc, task_id))
                if ev["started"]:
                    print(json.dumps({"event": "negotiated", "epoch": round(time.time(), 3),
                                      "started": ev["started"]}), flush=True)
                    neg_logged = True
            except globus_sdk.GlobusAPIError as e:
                print(json.dumps({"event": "warning", "where": "event_list", "error": str(e)[:300]}), flush=True)
        if cancel_file and not cancelled and os.path.exists(cancel_file):
            r = tc.cancel_task(task_id)
            print(json.dumps({"event": "cancel", "epoch": round(time.time(), 3), "code": r.get("code")}), flush=True)
            cancelled = True
        time.sleep(poll_s)

    t = tc.get_task(task_id)
    summary = {k: t.get(k) for k in ("task_id", "status", "nice_status", "request_time", "completion_time",
                                     "bytes_transferred", "files_transferred", "effective_bytes_per_second",
                                     "faults", "subtasks_failed", "concurrency", "parallelism", "encrypt_data",
                                     "verify_checksum")}
    summary.update(event="done", wall_s=round(time.time() - t0, 1), cancelled=cancelled, epoch=round(time.time(), 3),
                   gbps=round(8 * (t.get("bytes_transferred") or 0) / max(time.time() - t0, 1) / 1e9, 3))
    print(json.dumps(summary), flush=True)
    try:
        print(json.dumps({"event": "events", **events_summary(task_events(tc, task_id))}), flush=True)
    except globus_sdk.GlobusAPIError as e:
        print(json.dumps({"event": "warning", "where": "event_list", "error": str(e)[:300]}), flush=True)


if __name__ == "__main__":
    main(sys.argv)
