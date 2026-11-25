#!/usr/bin/env python3
"""Parametric synthetic dataset generator for the envelope study (#151 section 5).

A spec (datasets/*.yaml) fixes the size distribution, count or total bytes,
layout (fanout, depth, naming) and target. Planning is deterministic: the
same spec always yields the same keys and sizes, and the spec hash names the
dataset prefix, so a changed spec never collides with old data.

S3 target: one random seed object per distinct size is written in-region over
SSM on a given instance (`head -c N /dev/urandom | aws s3 cp - ...`). The
dataset is then fanned out server-side: CopyObject for objects up to the seed
cap, and multipart UploadPartCopy (ranges of one part-size seed) above it. No
object data passes through this machine. A manifest (keys, sizes, spec hash)
is written next to the dataset prefix, never inside it.

POSIX target: posix_synth.sh runs on a DTN over SSM, writes each seed with dd,
then copies it into the layout.

usage:
  synth.py plan     datasets/F64M.yaml [...]        # = --dry-run: counts, bytes, requests, cost
  synth.py plan     F64M "F1K+fanout=4"             # refs: datasets/<base>.yaml with overrides
  synth.py create   datasets/F64M.yaml --bucket B --instance i-... [--parallel 64]
  synth.py create   datasets/F1M.yaml --target posix --instance i-DTN --root /data/synth
  synth.py delete   datasets/F64M.yaml --bucket B [--seeds]
  synth.py histogram s3://bucket/prefix/ [--unsigned] > hist.json   # for size: {dist: histogram}
"""
import argparse
import concurrent.futures as cf
import datetime as dt
import gzip
import hashlib
import json
import math
import random
import sys
import time
from pathlib import Path

import hlib
from hlib import fmt_bytes, parse_size

MiB, GiB = 1024 ** 2, 1024 ** 3
MAX_COPY = 5 * GiB            # CopyObject limit
MIN_PART = 5 * MiB            # multipart minimum (except the last part)
MAX_PARTS = 10000

# us-west-2 S3 Standard list prices (Nov 2026). Estimates only; check the price list before a run.
PRICE = {"put_per_1k": 0.005, "get_per_1k": 0.0004, "gb_month": 0.023}

DEFAULTS = {
    "root": "synth",
    "naming": "hashed",       # hashed | sequential | dated
    "fanout": 16,             # top-level prefixes (1-256)
    "depth": 1,               # directory levels below the dataset root (1-6)
    "subfanout": 4,           # directories per level below the top level
    "target": "s3",           # s3 | posix
    "seed": 151,              # RNG seed for sizes and names
    "ext": ".bin",
    "content": {"seed_max": "1GiB", "part_size": "512MiB", "quantize_bins": 64},
    "dated": {"start": "2020-01-01T00:00:00", "per_dir": 64},
}


# ---------------------------------------------------------------- spec


def normalize(raw):
    """Fill defaults and parse units. Raises ValueError on an invalid spec."""
    s = json.loads(json.dumps(raw))  # deep copy
    for k, v in DEFAULTS.items():
        if isinstance(v, dict):
            s[k] = {**v, **(s.get(k) or {})}
        else:
            s.setdefault(k, v)
    if "name" not in s:
        raise ValueError("spec needs a name")
    size = s.get("size")
    if size is None:
        raise ValueError(f"{s['name']}: spec needs a size")
    if not isinstance(size, dict):
        size = {"dist": "fixed", "value": size}
    size.setdefault("dist", "fixed")
    for k in ("value", "median", "min", "max"):
        if k in size:
            size[k] = parse_size(size[k])
    if size["dist"] == "fixed" and "value" not in size:
        raise ValueError(f"{s['name']}: fixed size needs value")
    if size["dist"] == "lognormal" and ("median" not in size or "sigma" not in size):
        raise ValueError(f"{s['name']}: lognormal needs median and sigma")
    if size["dist"] == "bimodal":
        modes = size.get("modes") or []
        if len(modes) < 2:
            raise ValueError(f"{s['name']}: bimodal needs >= 2 modes")
        for m in modes:
            m["size"] = parse_size(m["size"])
            m.setdefault("weight", 1.0)
            m.setdefault("sigma", 0.0)
    if size["dist"] == "histogram":
        if "file" in size:
            size["bins"] = json.loads(Path(size.pop("file")).read_text())["bins"]
        if not size.get("bins"):
            raise ValueError(f"{s['name']}: histogram needs bins [[lo, hi, count], ...]")
        size["bins"] = [[parse_size(lo), parse_size(hi), int(c)] for lo, hi, c in size["bins"]]
    if size["dist"] not in ("fixed", "lognormal", "bimodal", "histogram"):
        raise ValueError(f"{s['name']}: unknown size dist {size['dist']!r}")
    s["size"] = size
    if "total_bytes" in s:
        s["total_bytes"] = parse_size(s["total_bytes"])
    if "count" not in s and "total_bytes" not in s and size["dist"] != "histogram":
        raise ValueError(f"{s['name']}: spec needs count or total_bytes")
    c = s["content"]
    c["seed_max"], c["part_size"] = parse_size(c["seed_max"]), parse_size(c["part_size"])
    if not MIN_PART <= c["part_size"] <= MAX_COPY:
        raise ValueError("content.part_size must be within 5 MiB..5 GiB")
    if c["seed_max"] > MAX_COPY:
        raise ValueError("content.seed_max must be <= 5 GiB")
    if not 1 <= int(s["fanout"]) <= 256:
        raise ValueError("fanout must be 1..256")
    if not 1 <= int(s["depth"]) <= 6:
        raise ValueError("depth must be 1..6")
    if s["naming"] not in ("hashed", "sequential", "dated"):
        raise ValueError(f"unknown naming {s['naming']!r}")
    if s["target"] not in ("s3", "posix"):
        raise ValueError(f"unknown target {s['target']!r}")
    return s


def load_spec(path):
    return normalize(hlib.load_yaml_file(path))


def load_ref(ref):
    """A spec path, or a dataset ref: 'F64M' or 'F1K+naming=sequential+fanout=1' (datasets/<base>.yaml
    with top-level overrides; the name gains the overrides so the prefix says what it is)."""
    if Path(ref).is_file():
        return load_spec(ref)
    base, *overrides = ref.split("+")
    raw = hlib.load_yaml_file(hlib.HARNESS / "datasets" / f"{base}.yaml")
    for o in overrides:
        k, _, v = o.partition("=")
        raw[k] = hlib._scalar(v)
    if overrides:
        raw["name"] = base + "-" + "-".join(o.replace("=", "") for o in overrides)
    return normalize(raw)


def spec_hash(spec):
    """Hash of everything that determines the dataset's keys and sizes."""
    keep = {k: v for k, v in spec.items() if k not in ("description", "notes")}
    return hlib.chash(keep, 16)


def dataset_prefix(spec):
    return f"{spec['root'].strip('/')}/{spec['name']}-{spec_hash(spec)[:8]}"


def seed_prefix(spec):
    return f"{spec['root'].strip('/')}/_seeds"


def manifest_key(spec):
    return dataset_prefix(spec) + ".manifest.json.gz"   # a sibling of the dataset, not inside it


# ---------------------------------------------------------------- sizes


def _quantizer(lo, hi, bins):
    """Round sizes to `bins` geometric levels in [lo, hi], so a distribution needs few seeds."""
    if not bins or hi <= lo:
        return lambda x: x
    r = math.log(hi / lo)
    levels = [int(round(lo * math.exp(r * i / (bins - 1)))) for i in range(bins)]

    def q(x):
        i = round((bins - 1) * math.log(max(x, lo) / lo) / r)
        return levels[min(max(i, 0), bins - 1)]
    return q


def sample_sizes(spec):
    size, rng = spec["size"], random.Random(f"{spec['seed']}:sizes")
    dist = size["dist"]
    cap = spec.get("total_bytes")
    count = spec.get("count")
    lo = size.get("min", 1)
    hi = size.get("max", 16 * GiB)

    if dist == "fixed":
        v = size["value"]
        n = count if count is not None else cap // v
        if count is not None and cap is not None:
            n = min(count, cap // v)
        return [v] * int(n)

    if dist == "lognormal":
        mu, sg = math.log(size["median"]), float(size["sigma"])
        draw = lambda: rng.lognormvariate(mu, sg)  # noqa: E731
    elif dist == "bimodal":
        modes = size["modes"]
        weights = [float(m["weight"]) for m in modes]

        def draw():
            m = rng.choices(modes, weights)[0]
            return m["size"] * (math.exp(rng.gauss(0, m["sigma"])) if m["sigma"] else 1.0)
    else:  # histogram replay: pick a bin by count, then log-uniform within it
        bins = size["bins"]
        weights = [b[2] for b in bins]
        lo = min(b[0] for b in bins) if "min" not in size else lo
        hi = max(b[1] for b in bins) if "max" not in size else hi
        if count is None and cap is None:
            count = sum(weights)

        def draw():
            b0, b1, _ = rng.choices(bins, weights)[0]
            b0 = max(b0, 1)
            return math.exp(rng.uniform(math.log(b0), math.log(max(b1, b0 + 1))))

    q = _quantizer(lo, hi, int(spec["content"].get("quantize_bins") or 0))
    out, total = [], 0
    limit = count if count is not None else 10 ** 9
    while len(out) < limit:
        x = q(int(min(max(draw(), lo), hi)))
        if cap is not None and total + x > cap:
            break
        out.append(x)
        total += x
    return out


# ---------------------------------------------------------------- layout


def _name(spec, i):
    return hashlib.sha256(f"{spec['seed']}:{spec['name']}:{i}".encode()).hexdigest()


def layout(spec, n):
    """Relative keys for n objects under the dataset prefix."""
    naming, fan, depth, sub = spec["naming"], int(spec["fanout"]), int(spec["depth"]), int(spec["subfanout"])
    ext = spec["ext"]
    keys = []
    if naming == "hashed":
        for i in range(n):
            h = _name(spec, i)
            hv = int(h[:16], 16)
            parts = [f"{hv % fan:02x}"]
            hv //= fan
            for _ in range(depth - 1):
                parts.append(f"{hv % sub:x}")
                hv //= sub
            keys.append("/".join(parts + [h[16:32] + ext]))
    elif naming == "sequential":
        # Lexicographically adjacent keys: the hot-prefix case. Directories fill in order.
        leaves = fan * sub ** (depth - 1)
        per_dir = max(1, math.ceil(n / leaves))
        for i in range(n):
            d = i // per_dir
            digits = []
            for _ in range(depth - 1):
                digits.append(d % sub)
                d //= sub
            parts = [f"d{d:04d}"] + [f"d{x:04d}" for x in reversed(digits)]
            keys.append("/".join(parts + [f"f{i:09d}{ext}"]))
    else:  # dated: depth 1..4 = year / month / day / hour, `per_dir` objects per leaf
        start = dt.datetime.fromisoformat(str(spec["dated"]["start"]))
        per = int(spec["dated"]["per_dir"])
        step = {1: dt.timedelta(days=366), 2: dt.timedelta(days=31), 3: dt.timedelta(days=1)}.get(
            min(depth, 4), dt.timedelta(hours=1))
        fmts = ["%Y", "%m", "%d", "%H"][:min(depth, 4)]
        for i in range(n):
            t = start + step * (i // per)
            keys.append("/".join([t.strftime(f) for f in fmts] + [f"obs-{t:%Y%m%dT%H}-{i % per:05d}{ext}"]))
    return keys


# ---------------------------------------------------------------- plan


def build_plan(spec):
    sizes = sample_sizes(spec)
    keys = layout(spec, len(sizes))
    c = spec["content"]
    seed_max, part = c["seed_max"], c["part_size"]
    prefix, sp = dataset_prefix(spec), seed_prefix(spec)
    seeds, ops = {}, []
    for k, s in zip(keys, sizes):
        if s <= seed_max:
            seeds[s] = f"{sp}/{s}"
            ops.append({"key": f"{prefix}/{k}", "size": s, "op": "copy", "seed": f"{sp}/{s}"})
        else:
            n_parts = math.ceil(s / part)
            if n_parts > MAX_PARTS:
                raise ValueError(f"{s} B needs {n_parts} parts > {MAX_PARTS}; raise content.part_size")
            seeds[part] = f"{sp}/{part}"
            ops.append({"key": f"{prefix}/{k}", "size": s, "op": "mpu", "seed": f"{sp}/{part}",
                        "parts": n_parts, "part_size": part})
    return {"spec": spec, "spec_hash": spec_hash(spec), "prefix": prefix, "ops": ops,
            "seeds": {str(k): v for k, v in sorted(seeds.items())}}


def _log2_hist(sizes):
    h = {}
    for s in sizes:
        b = 1 << max(0, int(s).bit_length() - 1)
        h[b] = h.get(b, 0) + 1
    return {hlib.size_label(k): v for k, v in sorted(h.items())}


def plan_summary(plan, days=2.0, parallel=64):
    ops = plan["ops"]
    sizes = [o["size"] for o in ops]
    total = sum(sizes)
    seed_bytes = sum(int(k) for k in plan["seeds"])
    copies = sum(1 for o in ops if o["op"] == "copy")
    mpus = [o for o in ops if o["op"] == "mpu"]
    parts = sum(o["parts"] for o in mpus)
    # Seeds go up with the AWS CLI: multipart at its 8 MiB default chunk size.
    seed_reqs = sum(max(1, math.ceil(int(k) / (8 * MiB))) + (2 if int(k) > 8 * MiB else 0) for k in plan["seeds"])
    put_class = seed_reqs + copies + 2 * len(mpus) + parts + math.ceil(len(ops) / 1000)  # + resume LIST
    req_cost = put_class / 1000 * PRICE["put_per_1k"]
    storage_cost = (total + seed_bytes) / 1e9 * PRICE["gb_month"] * days / 30
    # Server-side copy rate is an assumption (about 150 MB/s per in-flight copy, plus 40 ms per request).
    est_s = (total / (150e6 * parallel)) + (copies + len(mpus) * 2 + parts) * 0.04 / parallel
    return {"name": plan["spec"]["name"], "prefix": plan["prefix"], "spec_hash": plan["spec_hash"],
            "target": plan["spec"]["target"], "objects": len(ops), "bytes": total, "human": fmt_bytes(total),
            "distinct_sizes": len(set(sizes)), "seeds": len(plan["seeds"]), "seed_bytes": seed_bytes,
            "copy_object": copies, "multipart_objects": len(mpus), "upload_part_copy": parts,
            "put_class_requests": put_class, "est_request_usd": round(req_cost, 4),
            "est_storage_usd": round(storage_cost, 4), "storage_days": days,
            "est_fanout_minutes": round(est_s / 60, 1), "size_hist_log2": _log2_hist(sizes),
            "prefixes_top": len({o["key"][len(plan["prefix"]) + 1:].split("/")[0] for o in ops})}


def manifest(plan, bucket=None):
    return {"schema": "synth-manifest/1", "name": plan["spec"]["name"], "spec": plan["spec"],
            "spec_hash": plan["spec_hash"], "bucket": bucket, "prefix": plan["prefix"],
            "objects": [[o["key"], o["size"]] for o in plan["ops"]], "seeds": plan["seeds"],
            "created": dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds"),
            "harness_commit": hlib.git_commit()}


def posix_list(plan):
    """'size relpath' lines for posix_synth.sh (relative to the dataset root)."""
    pre = plan["prefix"] + "/"
    return "".join(f"{o['size']} {o['key'][len(pre):]}\n" for o in plan["ops"])


# ---------------------------------------------------------------- S3 execution


def seed_commands(plan, bucket, region, existing):
    """One shell command per missing seed, run in-region on the instance."""
    cmds = []
    for size, key in plan["seeds"].items():
        if existing.get(key) == int(size):
            continue
        cmds.append(f"head -c {int(size)} /dev/urandom | aws s3 cp - s3://{bucket}/{key} "
                    f"--expected-size {int(size)} --region {region} --only-show-errors")
    return cmds


def _with_retries(fn, attempts=6, base=0.5):
    for i in range(attempts):
        try:
            return fn()
        except Exception as e:  # noqa: BLE001 - botocore errors; botocore already retried throttles
            code = getattr(e, "response", {}).get("Error", {}).get("Code", "")
            if i == attempts - 1 or code in ("AccessDenied", "NoSuchBucket", "InvalidRequest"):
                raise
            time.sleep(base * 2 ** i * (0.5 + random.random()))


def copy_one(s3, bucket, op):
    src = {"Bucket": bucket, "Key": op["seed"]}
    if op["op"] == "copy":
        _with_retries(lambda: s3.copy_object(Bucket=bucket, Key=op["key"], CopySource=src,
                                             MetadataDirective="COPY"))
        return op["size"]
    up = _with_retries(lambda: s3.create_multipart_upload(Bucket=bucket, Key=op["key"]))["UploadId"]
    try:
        parts, left = [], op["size"]
        for n in range(1, op["parts"] + 1):
            ln = min(op["part_size"], left)
            r = _with_retries(lambda n=n, ln=ln: s3.upload_part_copy(
                Bucket=bucket, Key=op["key"], UploadId=up, PartNumber=n, CopySource=src,
                CopySourceRange=f"bytes=0-{ln - 1}"))
            parts.append({"PartNumber": n, "ETag": r["CopyPartResult"]["ETag"]})
            left -= ln
        _with_retries(lambda: s3.complete_multipart_upload(Bucket=bucket, Key=op["key"], UploadId=up,
                                                           MultipartUpload={"Parts": parts}))
    except Exception:
        s3.abort_multipart_upload(Bucket=bucket, Key=op["key"], UploadId=up)
        raise
    return op["size"]


def list_sizes(s3, bucket, prefix):
    out = {}
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=prefix):
        for o in page.get("Contents", []):
            out[o["Key"]] = o["Size"]
    return out


def create_s3(plan, bucket, instance, region, parallel, log=print):
    s3 = hlib.boto3_client("s3", region)
    existing = list_sizes(s3, bucket, seed_prefix(plan["spec"]) + "/")
    cmds = seed_commands(plan, bucket, region, existing)
    if cmds:
        log(f"seeds: writing {len(cmds)} on {instance} (in-region)")
        ssm = hlib.SSM(region)
        st, _, err = ssm.run(instance, "set -euo pipefail; " + "; ".join(cmds), timeout=7200)
        if st != "Success":
            raise RuntimeError(f"seed creation failed on {instance}: {err.strip()[-400:]}")
    have = list_sizes(s3, bucket, seed_prefix(plan["spec"]) + "/")
    bad = [k for s, k in plan["seeds"].items() if have.get(k) != int(s)]
    if bad:
        raise RuntimeError(f"seeds missing or wrong size: {bad[:5]}")
    done = list_sizes(s3, bucket, plan["prefix"] + "/")
    todo = [o for o in plan["ops"] if done.get(o["key"]) != o["size"]]
    log(f"fan-out: {len(todo)} of {len(plan['ops'])} objects to copy ({len(plan['ops']) - len(todo)} present)")
    t0, moved, errors = time.time(), 0, []
    with cf.ThreadPoolExecutor(parallel) as ex:
        futs = {ex.submit(copy_one, s3, bucket, o): o for o in todo}
        for i, f in enumerate(cf.as_completed(futs), 1):
            try:
                moved += f.result()
            except Exception as e:  # noqa: BLE001
                errors.append((futs[f]["key"], repr(e)))
            if i % 1000 == 0 or i == len(todo):
                log(f"  {i}/{len(todo)} objects, {fmt_bytes(moved)}, {time.time() - t0:.0f}s, {len(errors)} errors")
    if errors:
        raise RuntimeError(f"{len(errors)} copies failed (rerun to resume): {errors[:3]}")
    m = manifest(plan, bucket)
    s3.put_object(Bucket=bucket, Key=manifest_key(plan["spec"]),
                  Body=gzip.compress(hlib.canonical(m).encode()), ContentType="application/json",
                  ContentEncoding="gzip")
    return m


def delete_prefix(s3, bucket, prefix):
    n = 0
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=prefix):
        objs = [{"Key": o["Key"]} for o in page.get("Contents", [])]
        for i in range(0, len(objs), 1000):
            s3.delete_objects(Bucket=bucket, Delete={"Objects": objs[i:i + 1000], "Quiet": True})
            n += len(objs[i:i + 1000])
    for page in s3.get_paginator("list_multipart_uploads").paginate(Bucket=bucket, Prefix=prefix):
        for u in page.get("Uploads", []):
            s3.abort_multipart_upload(Bucket=bucket, Key=u["Key"], UploadId=u["UploadId"])
    return n


# ---------------------------------------------------------------- POSIX execution


def create_posix(plan, instance, region, root, jobs, log=print):
    ssm = hlib.SSM(region)
    base = f"{root.rstrip('/')}/{plan['prefix'].split('/', 1)[1]}"
    lst = f"{hlib.REMOTE_TMP}/synth-{plan['spec_hash'][:8]}.list"
    ssm.put_file(instance, lst, posix_list(plan).encode())
    ssm.put_file(instance, f"{hlib.REMOTE_TMP}/posix_synth.sh", (hlib.HARNESS / "posix_synth.sh").read_bytes(), "0755")
    ssm.put_file(instance, base + ".manifest.json", json.dumps(manifest(plan)).encode())
    log(f"posix: {len(plan['ops'])} files under {base} on {instance}")
    ssm.run(instance, f"nohup bash {hlib.REMOTE_TMP}/posix_synth.sh {lst} {base} {jobs} "
                      f"> {base}.log 2>&1 &", check=True)
    while True:
        time.sleep(30)
        _, out, _ = ssm.run(instance, f"cat {base}.done 2>/dev/null || tail -1 {base}.log")
        log("  " + out.strip()[-200:])
        if out.startswith("done") or "ERROR" in out:
            return out.strip()


# ---------------------------------------------------------------- histogram


def histogram(url, region, unsigned=False):
    import boto3  # noqa: PLC0415
    from botocore import UNSIGNED
    from botocore.config import Config
    bucket, _, prefix = url[len("s3://"):].partition("/")
    s3 = boto3.client("s3", region_name=region, config=Config(signature_version=UNSIGNED) if unsigned else None)
    h = {}
    n = 0
    for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=prefix):
        for o in page.get("Contents", []):
            b = max(0, int(o["Size"]).bit_length() - 1)
            h[b] = h.get(b, 0) + 1
            n += 1
    return {"source": url, "objects": n, "bins": [[1 << b, (1 << (b + 1)) - 1, c] for b, c in sorted(h.items())]}


# ---------------------------------------------------------------- CLI


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("plan", "create", "delete"):
        p = sub.add_parser(name)
        p.add_argument("specs", nargs="+")
        p.add_argument("--bucket")
        p.add_argument("--region", default="us-west-2")
        p.add_argument("--dry-run", action="store_true")
        p.add_argument("--json", action="store_true", help="print summaries as JSON")
        p.add_argument("--days", type=float, default=2.0, help="storage days for the cost estimate")
        p.add_argument("--parallel", type=int, default=64)
        p.add_argument("--instance", help="instance that writes seeds (S3) or the files (POSIX)")
        p.add_argument("--target", choices=("s3", "posix"))
        p.add_argument("--root", default="/data", help="POSIX root on the DTN")
        p.add_argument("--jobs", type=int, default=8, help="POSIX parallel copies")
        p.add_argument("--seeds", action="store_true", help="delete: also remove the seed objects")
    h = sub.add_parser("histogram")
    h.add_argument("url")
    h.add_argument("--region", default="us-west-2")
    h.add_argument("--unsigned", action="store_true")
    a = ap.parse_args(argv)

    if a.cmd == "histogram":
        print(json.dumps(histogram(a.url, a.region, a.unsigned)))
        return 0

    plans = []
    for path in a.specs:
        spec = load_ref(path)
        if a.target:
            spec["target"] = a.target
        plans.append(build_plan(spec))
    sums = [plan_summary(p, a.days, a.parallel) for p in plans]
    if a.json:
        print(json.dumps(sums, indent=1))
    else:
        tot = {"objects": 0, "bytes": 0, "usd": 0.0}
        print(f"{'dataset':8} {'prefix':26} {'objects':>8} {'bytes':>12} {'sizes':>5} {'seeds':>5} "
              f"{'copy':>7} {'mpu':>5} {'parts':>6} {'PUT-class':>9} {'req $':>7} {'store $':>7} {'min':>5}")
        for s in sums:
            print(f"{s['name']:8} {s['prefix']:26} {s['objects']:8d} {s['human']:>12} {s['distinct_sizes']:5d} "
                  f"{s['seeds']:5d} {s['copy_object']:7d} {s['multipart_objects']:5d} {s['upload_part_copy']:6d} "
                  f"{s['put_class_requests']:9d} {s['est_request_usd']:7.3f} {s['est_storage_usd']:7.2f} "
                  f"{s['est_fanout_minutes']:5.1f}")
            tot["objects"] += s["objects"]
            tot["bytes"] += s["bytes"]
            tot["usd"] += s["est_request_usd"] + s["est_storage_usd"]
        print(f"total: {tot['objects']} objects, {fmt_bytes(tot['bytes'])}, est ${tot['usd']:.2f} "
              f"(requests + {a.days:g} days of storage; server-side copies move no data out of S3)")
    if a.cmd == "plan" or a.dry_run:
        return 0

    for p in plans:
        spec = p["spec"]
        if a.cmd == "delete":
            if spec["target"] == "posix":
                base = f"{a.root.rstrip('/')}/{p['prefix'].split('/', 1)[1]}"
                hlib.SSM(a.region).run(a.instance, f"rm -rf {base} {base}.manifest.json {base}.log {base}.done",
                                       check=True)
                print(f"deleted {base} on {a.instance}")
                continue
            s3 = hlib.boto3_client("s3", a.region)
            n = delete_prefix(s3, a.bucket, p["prefix"] + "/")
            s3.delete_object(Bucket=a.bucket, Key=manifest_key(spec))
            if a.seeds:
                n += delete_prefix(s3, a.bucket, seed_prefix(spec) + "/")
            print(f"deleted {n} objects under s3://{a.bucket}/{p['prefix']}/")
        elif spec["target"] == "posix":
            if not a.instance:
                sys.exit("--instance (a DTN) is required for POSIX targets")
            print(create_posix(p, a.instance, a.region, a.root, a.jobs))
        else:
            if not (a.bucket and a.instance):
                sys.exit("--bucket and --instance are required for S3 targets")
            m = create_s3(p, a.bucket, a.instance, a.region, a.parallel)
            print(f"created s3://{a.bucket}/{p['prefix']}/ ({len(m['objects'])} objects); "
                  f"manifest s3://{a.bucket}/{manifest_key(spec)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
