#!/usr/bin/env python3
"""GridFTP per-transfer records: parse, summarize, enable and collect (#151).

Format. globus-gridftp-server logs one NetLogger-style line per file transfer
(GCT GridFTP admin guide: `log_transfer` / `-Z`, or log level TRANSFER, which
puts the same record in the main log):

    DATE=20261116193121.219285 HOST=dtn PROG=globus-gridftp-server NL.EVNT=FTP_INFO
    START=20261116193120.328202 USER=gcs FILE=/bucket/key BUFFER=0 BLOCK=262144
    NBYTES=67108864 VOLUME=/ STREAMS=4 STRIPES=1 DEST=[203.0.113.5] TYPE=RETR CODE=226

DATE is the completion time and START the start time, both UTC, formatted
YYYYMMDDHHMMSS.uuuuuu. TYPE is RETR/ERET (this node sent the file, i.e. read
it from storage) or STOR/ESTO (received and wrote it). STREAMS is the
parallelism used on the data channel. Variants seen in the field add a prefix
(syslog, or "[pid] <date> ... Transfer stats: "), quote FILE, or add fields
(TASKID, SHARE, retrans, ...). The parser keys on `DATE=` and splits on
` KEY=` boundaries, so it tolerates all of these and unquoted FILE values with
spaces; anything it can't use is skipped and counted.

GCS 5.4 logs errors and warnings to /var/log/gridftp.log by default, without
transfer records. `enable` adds /etc/gridftp.d/zz-bench-transfer-log (GCS
leaves files not named globus* alone) with `log_transfer
/var/log/gridftp-transfer.log` and restarts the server; with the #141
KillMode=process drop-in that restart doesn't cut running sessions.

usage:
  gridftp_log.py parse   FILE [...]                 # JSONL, one record per transfer
  gridftp_log.py summary FILE [...] [--t0 E --t1 E] # per-direction rates, in-flight files
  gridftp_log.py enable  <src|dst>                  # live: enable transfer records on running DTNs
  gridftp_log.py fetch   <src|dst> OUT_DIR          # live: copy logs from running DTNs (gzip chunks)
"""
import argparse
import calendar
import json
import re
import sys
import time
from pathlib import Path

LOG_PATHS = ["/var/log/gridftp-transfer.log", "/var/log/gridftp.log"]
CONF_PATH = "/etc/gridftp.d/zz-bench-transfer-log"
SEND_TYPES = {"RETR", "ERET"}
RECV_TYPES = {"STOR", "ESTO", "APPE"}

_KEY = re.compile(r"(?:(?<=\s)|^)([A-Z][A-Za-z0-9_.]*)=")


def parse_time(v):
    """'20261116193121.219285' -> epoch seconds (UTC)."""
    m = re.fullmatch(r"(\d{4})(\d{2})(\d{2})(\d{2})(\d{2})(\d{2})(?:\.(\d+))?", v.strip())
    if not m:
        raise ValueError(v)
    y, mo, d, h, mi, s = (int(x) for x in m.groups()[:6])
    frac = float("0." + m.group(7)) if m.group(7) else 0.0
    return calendar.timegm((y, mo, d, h, mi, s, 0, 0, 0)) + frac


def _fields(s):
    ms = list(_KEY.finditer(s))
    out = {}
    for i, m in enumerate(ms):
        end = ms[i + 1].start() if i + 1 < len(ms) else len(s)
        v = s[m.end():end].strip()
        if len(v) >= 2 and v[0] == v[-1] == '"':
            v = v[1:-1].replace('\\"', '"')
        out[m.group(1)] = v
    return out


def _int(v):
    try:
        return int(v)
    except (TypeError, ValueError):
        return None


def parse_line(line):
    """One transfer record as a dict, or None if the line isn't one."""
    i = line.find("DATE=")
    if i < 0:
        return None
    f = _fields(line[i:].rstrip("\n"))
    if "START" not in f or "NBYTES" not in f:
        return None
    try:
        end, start = parse_time(f["DATE"]), parse_time(f["START"])
    except ValueError:
        return None
    nbytes = _int(f.get("NBYTES"))
    if nbytes is None:
        return None
    typ = f.get("TYPE", "").upper()
    dur = end - start
    dest = f.get("DEST", "").strip("[]")
    rec = {"start": start, "end": end, "dur_s": round(dur, 6), "nbytes": nbytes, "type": typ,
           "direction": "send" if typ in SEND_TYPES else "recv" if typ in RECV_TYPES else "other",
           "streams": _int(f.get("STREAMS")), "stripes": _int(f.get("STRIPES")), "code": f.get("CODE"),
           "file": f.get("FILE"), "dest": dest.split(",") if dest else [], "host": f.get("HOST"),
           "buffer": _int(f.get("BUFFER")), "block": _int(f.get("BLOCK")),
           "rate_bps": (8 * nbytes / dur) if dur > 0 else None}
    for k in ("TASKID", "TASK_ID", "SHARE", "RETRANS", "retrans"):
        if k in f:
            rec[k.lower()] = f[k]
    return rec


def parse(lines):
    """(records, skipped_count). Duplicate records (same line in two logs) are kept once."""
    recs, seen, skipped = [], set(), 0
    for line in lines:
        if "DATE=" not in line:
            continue
        r = parse_line(line)
        if r is None:
            skipped += 1
            continue
        key = (r["host"], r["start"], r["end"], r["file"], r["nbytes"], r["type"])
        if key in seen:
            continue
        seen.add(key)
        recs.append(r)
    return recs, skipped


def _pct(xs, q):
    if not xs:
        return None
    xs = sorted(xs)
    k = (len(xs) - 1) * q
    lo = int(k)
    hi = min(lo + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def summarize(recs, t0=None, t1=None):
    """Per-direction stats. With a window, files are those overlapping [t0, t1];
    'inflight_mean' is the time-averaged number of files in flight and
    'window_gbps' the bytes prorated into the window (assuming a uniform rate per file)."""
    out = {}
    for direction in ("send", "recv"):
        rs = [r for r in recs if r["direction"] == direction and (r["code"] in (None, "226"))]
        if t0 is not None and t1 is not None:
            rs = [r for r in rs if r["end"] > t0 and r["start"] < t1]
        if not rs:
            continue
        rates = [r["rate_bps"] for r in rs if r["rate_bps"]]
        d = {"files": len(rs), "bytes": sum(r["nbytes"] for r in rs),
             "rate_gbps_median": _g(_pct(rates, 0.5)), "rate_gbps_p10": _g(_pct(rates, 0.1)),
             "rate_gbps_p90": _g(_pct(rates, 0.9)),
             "streams": sorted({r["streams"] for r in rs if r["streams"] is not None}),
             "hosts": sorted({r["host"] for r in rs if r["host"]})}
        if t0 is not None and t1 is not None and t1 > t0:
            busy, wbytes = 0.0, 0.0
            for r in rs:
                ov = min(r["end"], t1) - max(r["start"], t0)
                if ov > 0:
                    busy += ov
                    wbytes += r["nbytes"] * (ov / r["dur_s"] if r["dur_s"] > 0 else 1.0)
            d["inflight_mean"] = round(busy / (t1 - t0), 2)
            d["window_gbps"] = round(8 * wbytes / (t1 - t0) / 1e9, 3)
        out[direction] = d
    return out


def _g(bps):
    return None if bps is None else round(bps / 1e9, 4)


def per_size(recs, direction="send"):
    """Median per-file rate by exact size (input to the r(s) fit)."""
    by = {}
    for r in recs:
        if r["direction"] == direction and r["rate_bps"] and r["code"] in (None, "226"):
            by.setdefault(r["nbytes"], []).append(r["rate_bps"])
    return {s: {"n": len(v), "rate_gbps_median": _g(_pct(v, 0.5)), "dur_s_median": s * 8 / _pct(v, 0.5)}
            for s, v in sorted(by.items())}


# ---------------------------------------------------------------- remote


def enable_command(path=LOG_PATHS[0]):
    """Idempotent: write the transfer-log config and restart only if it changed."""
    return (f"if ! grep -qs '^log_transfer {path}$' {CONF_PATH}; then "
            f"printf '%s\\n' '# elastic-gcs envelope study (#151): per-transfer records' 'log_transfer {path}' > {CONF_PATH}; "
            f"touch {path}; systemctl restart globus-gridftp-server; echo enabled; else echo present; fi")


def offsets_command(paths=LOG_PATHS):
    """Print '<path> <size>' for each log, so a point can fetch only its own slice."""
    return "; ".join(f"echo {p} $(stat -c %s {p} 2>/dev/null || echo 0)" for p in paths)


def parse_offsets(text):
    out = {}
    for line in text.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1].isdigit():
            out[parts[0]] = int(parts[1])
    return out


def collect(ssm, instance_id, offsets=None, paths=LOG_PATHS):
    """Fetch each log from its recorded offset; returns {path: text}."""
    offsets = offsets or {}
    return {p: ssm.fetch_file(instance_id, p, offsets.get(p, 0)).decode("utf-8", "replace") for p in paths}


def _running_dtns(region, dep):
    import hlib  # noqa: PLC0415
    ec2 = hlib.boto3_client("ec2", region)
    r = ec2.describe_instances(Filters=[
        {"Name": "tag:elastic-gcs:deployment", "Values": [f"{region}-{dep}"]},
        {"Name": "tag:elastic-gcs:pool", "Values": ["slots"]},
        {"Name": "instance-state-name", "Values": ["running"]}])
    return [i["InstanceId"] for res in r["Reservations"] for i in res["Instances"]]


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for n in ("parse", "summary"):
        p = sub.add_parser(n)
        p.add_argument("files", nargs="+")
        p.add_argument("--t0", type=float)
        p.add_argument("--t1", type=float)
    for n in ("enable", "fetch"):
        p = sub.add_parser(n)
        p.add_argument("dep", choices=("src", "dst"))
        if n == "fetch":
            p.add_argument("out")
    a = ap.parse_args(argv)
    if a.cmd in ("parse", "summary"):
        lines = [ln for f in a.files for ln in Path(f).read_text(errors="replace").splitlines()]
        recs, skipped = parse(lines)
        if a.cmd == "parse":
            for r in recs:
                print(json.dumps(r))
        else:
            print(json.dumps({"records": len(recs), "skipped": skipped, "summary": summarize(recs, a.t0, a.t1),
                              "per_size_send": per_size(recs)}, indent=1))
        print(f"{len(recs)} records, {skipped} unparsed DATE= lines", file=sys.stderr)
        return 0
    import hlib  # noqa: PLC0415
    env = hlib.load_env()
    ssm = hlib.SSM(env["REGION"])
    ids = _running_dtns(env["REGION"], a.dep)
    if a.cmd == "enable":
        for i, (st, out, err) in ssm.run_many(ids, enable_command()).items():
            print(i, st, out.strip(), err.strip()[-200:])
        return 0
    out = Path(a.out)
    out.mkdir(parents=True, exist_ok=True)
    stamp = time.strftime("%Y%m%dT%H%M%S")
    for i in ids:
        for p, text in collect(ssm, i).items():
            dst = out / f"{a.dep}-{i}-{Path(p).name}-{stamp}"
            dst.write_text(text)
            print(dst, len(text))
    return 0


if __name__ == "__main__":
    sys.exit(main())
