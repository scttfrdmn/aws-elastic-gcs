#!/usr/bin/env python3
"""S3 server access logs: the stock connector's request profile (P1b, #151).

Record format (AWS docs, "Amazon S3 server access log format"): space
delimited, '-' for absent values, with new fields appended over time:

  owner bucket [time] remote_ip requester request_id operation key "request_uri"
  http_status error_code bytes_sent object_size total_time turn_around_time
  "referer" "user_agent" version_id host_id sig_version cipher auth_type host_header
  tls_version access_point_arn acl_required source_region ...

Quoted fields may contain unescaped quotes, so the parser anchors each quoted
field on what follows it. Trailing fields it doesn't know are kept in `extra`.

What the logs can't tell us: there is no Range header field. A ranged GET is
inferred from bytes_sent < object_size, which gives each range's *size* but
not its offset. Time has 1 s resolution (request receipt), so in-flight
overlap uses [time, time + total_time] and is accurate to about a second;
treat per-object concurrency as an estimate. For PUT and UploadPart,
object_size is the size of the request body. Logs are delivered best-effort,
usually within a few hours, so P1b analysis runs after the phase.

usage:
  s3_access_logs.py fetch --log-bucket B --log-prefix logs/bench/ --start 2026-11-20T10:00 --end 2026-11-20T14:00 --out DIR
  s3_access_logs.py profile LOGDIR_OR_FILES... [--prefix synth/F64M-xxxx/] [--t0 E --t1 E] [--objects out.jsonl]
  s3_access_logs.py join  RESULTS_DIR/points.jsonl LOGDIR [--slack 30]   # writes raw/<point>/s3_profile.json
"""
import argparse
import calendar
import json
import re
import sys
import time
from pathlib import Path
from urllib.parse import parse_qs, unquote

_HEAD = re.compile(
    r'^(?P<owner>\S+) (?P<bucket>\S+) \[(?P<time>[^\]]+)\] (?P<remote_ip>\S+) (?P<requester>\S+) '
    r'(?P<request_id>\S+) (?P<operation>\S+) (?P<key>\S+) "(?P<request_uri>.*?)" (?P<http_status>\d{3}|-) '
    r'(?P<error_code>\S+) (?P<bytes_sent>\S+) (?P<object_size>\S+) (?P<total_time>\S+) (?P<turn_around_time>\S+) '
    r'"(?P<referer>.*?)" "(?P<user_agent>.*?)"(?: (?P<rest>\S+ \S+ (?:SigV\d|-)(?: .*)?))?$')
_TAIL = ["version_id", "host_id", "signature_version", "cipher_suite", "authentication_type", "host_header",
         "tls_version", "access_point_arn", "acl_required", "source_region"]
_MON = {m: i for i, m in enumerate(["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov",
                                    "Dec"], 1)}


def _num(v):
    return None if v in (None, "-", "") else int(v)


def parse_time(v):
    """'06/Feb/2019:00:00:38 +0000' -> epoch seconds."""
    m = re.fullmatch(r"(\d{2})/(\w{3})/(\d{4}):(\d{2}):(\d{2}):(\d{2}) ([+-])(\d{2})(\d{2})", v)
    if not m:
        raise ValueError(v)
    d, mon, y, h, mi, s, sign, oh, om = m.groups()
    t = calendar.timegm((int(y), _MON[mon], int(d), int(h), int(mi), int(s), 0, 0, 0))
    off = (int(oh) * 3600 + int(om) * 60) * (1 if sign == "+" else -1)
    return t - off


def parse_line(line):
    m = _HEAD.match(line.rstrip("\n"))
    if not m:
        return None
    g = m.groupdict()
    try:
        t = parse_time(g["time"])
    except ValueError:
        return None
    rec = {"time": t, "bucket": g["bucket"], "remote_ip": g["remote_ip"], "requester": g["requester"],
           "request_id": g["request_id"], "operation": g["operation"],
           "key": None if g["key"] == "-" else unquote(g["key"]),
           "http_status": _num(g["http_status"]), "error_code": None if g["error_code"] == "-" else g["error_code"],
           "bytes_sent": _num(g["bytes_sent"]) or 0, "object_size": _num(g["object_size"]),
           "total_ms": _num(g["total_time"]), "turnaround_ms": _num(g["turn_around_time"]),
           "user_agent": g["user_agent"]}
    rest = (g.get("rest") or "").split(" ") if g.get("rest") else []
    for name, v in zip(_TAIL, rest):
        rec[name] = None if v == "-" else v
    if len(rest) > len(_TAIL):
        rec["extra"] = rest[len(_TAIL):]
    uri = g["request_uri"].split(" ")
    rec["method"] = uri[0] if uri else None
    target = uri[1] if len(uri) > 1 else ""
    q = parse_qs(target.partition("?")[2], keep_blank_values=True)
    rec["query"] = {k: v[0] for k, v in q.items()}
    rec["end"] = t + (rec["total_ms"] or 0) / 1000.0
    return rec


def parse(lines):
    recs, bad = [], 0
    for ln in lines:
        if not ln.strip():
            continue
        r = parse_line(ln)
        if r is None:
            bad += 1
        else:
            recs.append(r)
    return recs, bad


def read_logs(paths):
    lines = []
    for p in paths:
        p = Path(p)
        files = sorted(x for x in p.rglob("*") if x.is_file()) if p.is_dir() else [p]
        for f in files:
            lines.extend(f.read_text(errors="replace").splitlines())
    return parse(lines)


# ---------------------------------------------------------------- analysis


def max_overlap(intervals):
    """Peak number of simultaneously open [start, end] intervals, and the time-weighted mean."""
    ev = []
    for s, e in intervals:
        e = max(e, s + 1e-3)
        ev += [(s, 1), (e, -1)]
    ev.sort(key=lambda x: (x[0], x[1]))
    cur = peak = 0
    area, last = 0.0, None
    for t, d in ev:
        if last is not None:
            area += cur * (t - last)
        cur += d
        peak = max(peak, cur)
        last = t
    span = (ev[-1][0] - ev[0][0]) if ev else 0
    return peak, (area / span if span > 0 else float(peak))


def _stats(xs):
    if not xs:
        return None
    xs = sorted(xs)
    return {"n": len(xs), "min": xs[0], "median": xs[len(xs) // 2], "max": xs[-1], "mean": sum(xs) / len(xs)}


def _is_throttle(r):
    return r["http_status"] == 503 or r["error_code"] in ("SlowDown", "ServiceUnavailable")


def retries(recs):
    """For each failed request, the next attempt at the same operation/key/part: the gap is the backoff."""
    by = {}
    for r in sorted(recs, key=lambda r: r["time"]):
        k = (r["operation"], r["key"], r["query"].get("partNumber"), r["query"].get("uploadId"))
        by.setdefault(k, []).append(r)
    out = []
    for k, rs in by.items():
        for a, b in zip(rs, rs[1:]):
            if a["http_status"] and a["http_status"] >= 500:
                out.append({"operation": k[0], "key": k[1], "part": k[2], "status": a["http_status"],
                            "error": a["error_code"], "backoff_s": round(b["time"] - a["end"], 3),
                            "retry_status": b["http_status"]})
    return out


def profile_objects(recs):
    """Per-object request profile."""
    objs = {}
    for r in recs:
        if r["key"]:
            objs.setdefault(r["key"], []).append(r)
    rows = []
    for key, rs in sorted(objs.items()):
        ops = {}
        for r in rs:
            ops[r["operation"]] = ops.get(r["operation"], 0) + 1
        gets = [r for r in rs if r["operation"] == "REST.GET.OBJECT" and (r["http_status"] or 0) < 300]
        row = {"key": key, "requests": len(rs), "ops": ops,
               "object_size": max((r["object_size"] or 0) for r in rs) or None,
               "throttled_503": sum(1 for r in rs if _is_throttle(r)),
               "errors_5xx": sum(1 for r in rs if (r["http_status"] or 0) >= 500),
               "errors_4xx": sum(1 for r in rs if 400 <= (r["http_status"] or 0) < 500)}
        if gets:
            sizes = [r["bytes_sent"] for r in gets]
            ranged = [r for r in gets if r["object_size"] and r["bytes_sent"] < r["object_size"]]
            peak, mean = max_overlap([(r["time"], r["end"]) for r in gets])
            row["get"] = {"n": len(gets), "ranged": len(ranged), "range_bytes": _stats(sizes),
                          "inflight_peak": peak, "inflight_mean": round(mean, 2),
                          "first_byte_ms": _stats([r["turnaround_ms"] for r in gets if r["turnaround_ms"] is not None]),
                          "span_s": round(max(r["end"] for r in gets) - min(r["time"] for r in gets), 3)}
        uploads = {}
        for r in rs:
            uid = r["query"].get("uploadId")
            if uid:
                uploads.setdefault(uid, []).append(r)
        if uploads:
            row["multipart"] = []
            for uid, us in uploads.items():
                parts = [u for u in us if u["operation"] == "REST.PUT.PART"]
                ok = [u for u in parts if (u["http_status"] or 0) < 300]
                peak, mean = max_overlap([(u["time"], u["end"]) for u in ok]) if ok else (0, 0.0)
                done = [u for u in us if u["operation"] == "REST.POST.UPLOAD"]
                row["multipart"].append({
                    "upload_id": uid[:16], "parts": len({u["query"].get("partNumber") for u in ok}),
                    "part_requests": len(parts), "part_bytes": _stats([u["object_size"] for u in ok if u["object_size"]]),
                    "inflight_peak": peak, "inflight_mean": round(mean, 2), "completed": bool(done),
                    "span_s": round(max(u["end"] for u in us) - min(u["time"] for u in us), 3)})
        puts = [r for r in rs if r["operation"] == "REST.PUT.OBJECT"]
        if puts:
            row["put"] = {"n": len(puts), "bytes": _stats([r["object_size"] for r in puts if r["object_size"]])}
        # Reads of an object after it was written: checksum read-back or verification.
        writes_end = [r["end"] for r in rs if r["operation"] in ("REST.PUT.OBJECT", "REST.POST.UPLOAD")
                      and (r["http_status"] or 0) < 300]
        if writes_end:
            w = min(writes_end)
            row["read_back"] = sum(1 for r in rs if r["operation"] in ("REST.GET.OBJECT", "REST.HEAD.OBJECT")
                                   and r["time"] >= w)
        rows.append(row)
    return rows


def profile(recs):
    """Whole-window profile: request mix, per-object summaries, markers, LISTs, throttling, retries."""
    ops = {}
    for r in recs:
        ops[r["operation"]] = ops.get(r["operation"], 0) + 1
    rows = profile_objects(recs)
    markers = [r for r in recs if r["operation"] == "REST.PUT.OBJECT" and r["key"] and r["key"].endswith("/")
               and (r["object_size"] or 0) == 0]
    lists = [r for r in recs if r["operation"] in ("REST.GET.BUCKET", "REST.GET.BUCKETVERSIONS")]
    heads = [r for r in recs if r["operation"] == "REST.HEAD.OBJECT"]
    thr = [r for r in recs if _is_throttle(r)]
    rt = retries(recs)
    data_objs = [o for o in rows if not o["key"].endswith("/")]
    n = len(data_objs) or 1
    get_objs = [o for o in rows if "get" in o]
    mp = [m for o in rows for m in o.get("multipart", [])]
    out = {"records": len(recs), "objects": len(data_objs), "ops": dict(sorted(ops.items())),
           "requests_per_object": round(sum(o["requests"] for o in data_objs) / n, 2),
           "heads": len(heads), "lists": len(lists), "directory_markers": len(markers),
           "throttled_503": len(thr), "errors_5xx": sum(1 for r in recs if (r["http_status"] or 0) >= 500),
           "retries": len(rt), "backoff_s": _stats([x["backoff_s"] for x in rt]),
           "throttle_first_t": min((r["time"] for r in thr), default=None)}
    if get_objs:
        out["get"] = {"objects": len(get_objs),
                      "gets_per_object": _stats([o["get"]["n"] for o in get_objs]),
                      "range_bytes": _stats([o["get"]["range_bytes"]["median"] for o in get_objs]),
                      "inflight_peak_per_object": _stats([o["get"]["inflight_peak"] for o in get_objs]),
                      "first_byte_ms": _stats([o["get"]["first_byte_ms"]["median"] for o in get_objs
                                               if o["get"]["first_byte_ms"]])}
    if mp:
        out["multipart"] = {"uploads": len(mp), "parts_per_upload": _stats([m["parts"] for m in mp]),
                            "part_bytes": _stats([m["part_bytes"]["median"] for m in mp if m["part_bytes"]]),
                            "inflight_peak_per_upload": _stats([m["inflight_peak"] for m in mp])}
    return out, rows


def select(recs, prefixes=None, t0=None, t1=None, slack=0.0):
    out = []
    for r in recs:
        if t0 is not None and r["time"] < t0 - slack:
            continue
        if t1 is not None and r["time"] > t1 + slack:
            continue
        if prefixes:
            k = r["key"] or r["query"].get("prefix") or ""
            if not any((r["bucket"] == b or b is None) and k.startswith(p) for b, p in prefixes):
                continue
        out.append(r)
    return out


def join(points, recs, slack=30.0):
    """Assign log records to points by time window and key prefix. Returns {point_uid: (profile, rows)}."""
    out = {}
    for pt in points:
        pre = [(x.get("bucket"), x["prefix"]) for x in pt.get("s3_prefixes", []) if x.get("prefix")]
        if not pre or pt.get("t_submit") is None:
            continue
        sel = select(recs, pre, pt["t_submit"], pt.get("t_end") or pt["t_submit"], slack)
        by_role = {}
        for x in pt["s3_prefixes"]:
            sub = select(sel, [(x.get("bucket"), x["prefix"])])
            by_role[x.get("role", "data")] = profile(sub)
        out[pt["uid"]] = by_role
    return out


# ---------------------------------------------------------------- fetch


def fetch(log_bucket, log_prefix, start, end, out, region="us-west-2", lag_h=6, workers=32):
    """Download log objects for [start, end + lag].

    Handles both key formats S3 can write: simple (<prefix>YYYY-mm-dd-HH-MM-SS-<id>)
    and partitioned (<prefix><account>/<region>/<bucket>/YYYY/MM/DD/YYYY-mm-dd-HH-MM-SS-<id>).
    The object's file name starts with its timestamp in both, so filter on that.
    """
    import concurrent.futures as cf  # noqa: PLC0415
    import hlib  # noqa: PLC0415
    s3 = hlib.boto3_client("s3", region)
    t0 = time.strftime("%Y-%m-%d-%H-%M-%S", time.gmtime(start))
    t1 = time.strftime("%Y-%m-%d-%H-%M-%S", time.gmtime(end + lag_h * 3600))
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    keys = []
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=log_bucket, Prefix=log_prefix):
        for o in page.get("Contents", []):
            name = o["Key"].rsplit("/", 1)[-1]
            if t0 <= name[:19] <= t1:
                keys.append(o["Key"])

    def get(key):
        dest = out / key[len(log_prefix):].replace("/", "_")
        if not dest.exists():
            s3.download_file(log_bucket, key, str(dest))

    with cf.ThreadPoolExecutor(max_workers=workers) as ex:
        list(ex.map(get, keys))
    return len(keys)


def _epoch(v):
    if v is None:
        return None
    try:
        return float(v)
    except ValueError:
        return calendar.timegm(time.strptime(v[:16], "%Y-%m-%dT%H:%M"))


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    f = sub.add_parser("fetch")
    f.add_argument("--log-bucket", required=True)
    f.add_argument("--log-prefix", required=True)
    f.add_argument("--start", required=True)
    f.add_argument("--end", required=True)
    f.add_argument("--out", required=True)
    f.add_argument("--region", default="us-west-2")
    p = sub.add_parser("profile")
    p.add_argument("paths", nargs="+")
    p.add_argument("--prefix", action="append")
    p.add_argument("--t0")
    p.add_argument("--t1")
    p.add_argument("--objects", help="write per-object rows (JSONL) here")
    j = sub.add_parser("join")
    j.add_argument("points")
    j.add_argument("logs")
    j.add_argument("--slack", type=float, default=30.0)
    a = ap.parse_args(argv)
    if a.cmd == "fetch":
        print(fetch(a.log_bucket, a.log_prefix, _epoch(a.start), _epoch(a.end), a.out, a.region), "log objects")
        return 0
    if a.cmd == "profile":
        recs, bad = read_logs(a.paths)
        recs = select(recs, [(None, x) for x in a.prefix] if a.prefix else None, _epoch(a.t0), _epoch(a.t1))
        summary, rows = profile(recs)
        summary["unparsed_lines"] = bad
        print(json.dumps(summary, indent=1))
        if a.objects:
            Path(a.objects).write_text("".join(json.dumps(r) + "\n" for r in rows))
        return 0
    recs, bad = read_logs([a.logs])
    pts = [json.loads(ln) for ln in Path(a.points).read_text().splitlines() if ln.strip()]
    res = join(pts, recs, a.slack)
    base = Path(a.points).parent
    for uid, by_role in res.items():
        d = base / "raw" / uid
        d.mkdir(parents=True, exist_ok=True)
        (d / "s3_profile.json").write_text(json.dumps({r: s for r, (s, _) in by_role.items()}, indent=1))
        with open(d / "s3_objects.jsonl", "w") as fh:
            for role, (_, rows) in by_role.items():
                for row in rows:
                    fh.write(json.dumps({"role": role, **row}) + "\n")
    print(f"{len(recs)} records ({bad} unparsed); profiles for {len(res)} points under {base / 'raw'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
