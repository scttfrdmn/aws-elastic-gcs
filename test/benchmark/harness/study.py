#!/usr/bin/env python3
"""Run orchestrator for the envelope study (#151).

A run plan (plans/*.yaml) gives, per phase, a base point and one-factor-at-a-
time sweeps; a sweep's `fixed` lists are crossed with it, which gives the
interaction checks. The orchestrator:

1. expands the plan into points; points that coincide across sweeps are measured once;
2. runs the coarse grid in random order (seeded; the seed and order are recorded),
   with n = 3 at headline points;
3. refines each numeric sweep adaptively: where the relative gain between
   neighbouring points falls below 10% (scaled to the coarse step), it inserts
   intermediate values until the knee lies within one step;
4. repeats the points around each knee to n = 3, and re-measures a random 20% of the rest;
5. per point: sets network_use on both endpoints, pins DTN counts (demand
   monitor paused) and waits until the DNS gate shows exactly the serving DTNs,
   applies netem on the sending side if asked, starts 1 s samplers on every node,
   submits k fresh tasks through bench_transfer.py on the src control node,
   detects steady state, cancels after the 45 s window, collects artifacts
   and deletes what the transfer wrote;
6. appends one JSONL record per point to results/<campaign>/points.jsonl,
   with raw artifacts in results/<campaign>/raw/<point>/. A rerun skips
   points already recorded.

Phase kinds: `globus` (transfers), `iperf3` and `s3probe` (P0 controls).

usage:
  study.py plan    plans/envelope.yaml [--phase P1] [--verbose]   # schedule, time, bytes, cost (offline)
  study.py run     plans/envelope.yaml --phase P1 [--dry-run] [--max-points N] [--keep-data]
  study.py datasets plans/envelope.yaml --phase P1                # dataset refs for synth.py
  study.py install                                                # push bench_transfer.py to the src control node
  study.py reset   [--type c8in.2xlarge]                          # after the instance-size arm
  study.py status  plans/envelope.yaml
"""
import argparse
import base64
import itertools
import json
import math
import random
import re
import shlex
import socket
import subprocess
import sys
import time
from pathlib import Path

import gridftp_log
import hlib
import synth
from steady import SteadyTracker, aggregate_rates, detect_steady, window_stats

SCHEMA = "envelope-point/1"

# Approximate us-west-2 on-demand prices and baseline bandwidth, for --dry-run
# estimates only. Live records take the type's real NetworkInfo from EC2.
INSTANCE = {
    "c8in.xlarge": (0.272, 6.25), "c8in.2xlarge": (0.544, 12.5), "c8in.4xlarge": (1.089, 25.0),
    "c8in.8xlarge": (2.177, 50.0), "c8in.12xlarge": (3.266, 75.0), "c8in.16xlarge": (4.355, 100.0),
    "c8in.24xlarge": (6.532, 150.0), "c8in.32xlarge": (8.709, 200.0), "c8in.48xlarge": (13.064, 300.0),
    "c8in.96xlarge": (26.127, 600.0), "t3a.large": (0.0752, 0.5),
}
TRANSFER_USD_PER_GB = 0.02          # same-region public-IP traffic, both directions
MAX_TASK_CONCURRENCY = 128          # Transfer's per-task cap (L2)

BASE_DEFAULTS = {"route": "s3_to_s3", "c": 32, "p": 4, "n": 1, "k": 1, "rtt_ms": 0, "loss_pct": 0,
                 "verify_checksum": True, "encrypt_data": False, "instance_type": "c8in.2xlarge",
                 "network_use": "custom", "dst_fanout": None}
MEASURE_DEFAULTS = {"mode": "steady", "window_s": 10, "hold_s": 10, "measure_s": 45, "alpha": 0.05,
                    "min_frac": 0.5, "min_level_gbps": 0.05, "poll_s": 15, "max_point_s": 900, "fixed_s": 300,
                    "settle_s": 60}
REPEAT_DEFAULTS = {"headline": 3, "knee": 3, "random_frac": 0.2}
REFINE_DEFAULTS = {"threshold": 0.10, "max_rounds": 6}
PRIOR_DEFAULTS = {"t0_s": 0.3, "b_gbps": 0.35, "node_frac": 0.75, "ramp_s": 40, "noise_cv": 0.04}


# ================================================================ plan


def load_plan(path):
    return prepare_plan(hlib.load_yaml_file(path), path)


def prepare_plan(plan, path="<memory>"):
    """Fill defaults into a parsed plan."""
    plan["_path"] = str(path)
    plan.setdefault("campaign", "test")
    plan.setdefault("seed", 151)
    plan["measure"] = {**MEASURE_DEFAULTS, **(plan.get("measure") or {})}
    plan["repeats"] = {**REPEAT_DEFAULTS, **(plan.get("repeats") or {})}
    plan["refine"] = {**REFINE_DEFAULTS, **(plan.get("refine") or {})}
    plan["prior"] = {**PRIOR_DEFAULTS, **(plan.get("prior") or {})}
    plan.setdefault("routes", {})
    plan.setdefault("datasets", {})
    names = [p["name"] for p in plan.get("phases", [])]
    if len(set(names)) != len(names):
        raise ValueError("phase names must be unique")
    return plan


def get_phase(plan, name):
    for p in plan["phases"]:
        if p["name"] == name:
            return p
    raise KeyError(f"no phase {name!r} in {plan['_path']}")


def phase_plan(plan, phase):
    """The plan as seen by one phase: a phase's `measure:` overrides the plan's."""
    out = dict(plan)
    out["measure"] = {**plan["measure"], **(phase.get("measure") or {})}
    return out


def normalize_factors(f):
    """Fill defaults and expand per-side shorthands (n -> n_src/n_dst, instance_type -> _src/_dst)."""
    f = {**BASE_DEFAULTS, **f}
    for k in ("n", "instance_type"):
        v = f.pop(k)
        f.setdefault(f"{k}_src", v)
        f.setdefault(f"{k}_dst", v)
    return dict(sorted(f.items()))


def point_uid(phase, factors):
    return hlib.chash({"phase": phase, "factors": factors}, 10)


def _matches(factors, sel):
    """Does a normalized point match a (possibly shorthand) selector like {c: 8, n: 4}?"""
    want = {}
    for k, v in sel.items():
        if k in ("n", "instance_type"):
            want[f"{k}_src"] = want[f"{k}_dst"] = v
        else:
            want[k] = v
    return all(factors.get(k) == v for k, v in want.items())


def expand_phase(plan, phase):
    """Points and curves of a phase. Returns (points {uid: point}, curves [curve])."""
    base = dict(phase.get("base") or {})
    points, curves = {}, []

    def add(raw, curve_id=None, headline=False):
        f = normalize_factors(raw)
        uid = point_uid(phase["name"], f)
        p = points.setdefault(uid, {"uid": uid, "phase": phase["name"], "kind": phase.get("kind", "globus"),
                                    "factors": f, "curves": [], "headline": False})
        if curve_id and curve_id not in p["curves"]:
            p["curves"].append(curve_id)
        p["headline"] = p["headline"] or headline
        return uid

    for si, sw in enumerate(phase.get("sweeps") or []):
        fixed = sw.get("fixed") or {}
        keys = sorted(fixed)
        combos = itertools.product(*[(fixed[k] if isinstance(fixed[k], list) else [fixed[k]]) for k in keys])
        for combo in combos:
            fx = dict(zip(keys, combo))
            cid = f"{phase['name']}:{sw['factor']}:{si}:{hlib.chash(fx, 6)}"
            numeric = all(isinstance(v, (int, float)) and not isinstance(v, bool) for v in sw["values"])
            curve = {"id": cid, "phase": phase["name"], "factor": sw["factor"], "fixed": fx,
                     "base": base, "coarse": list(sw["values"]), "numeric": numeric,
                     "refine": bool(sw.get("refine", False)) and numeric,
                     "step": sw.get("step", 1), "integer": all(isinstance(v, int) for v in sw["values"]),
                     "metric": sw.get("metric", phase.get("metric", "steady_gbps")), "points": {}}
            for v in sw["values"]:
                raw = {**base, **fx, **(v if isinstance(v, dict) else {sw["factor"]: v})}
                uid = add(raw, cid, bool(sw.get("headline")))
                curve["points"][_xkey(v)] = uid
            curves.append(curve)
    if not phase.get("sweeps"):
        add(base, None, True)
    for h in phase.get("headline") or []:
        for p in points.values():
            if _matches(p["factors"], h):
                p["headline"] = True
    return points, curves


def _xkey(v):
    return hlib.canonical(v) if isinstance(v, dict) else v


def curve_point(curve, x, phase_name):
    raw = {**curve["base"], **curve["fixed"], curve["factor"]: x}
    f = normalize_factors(raw)
    return {"uid": point_uid(phase_name, f), "phase": phase_name, "kind": None, "factors": f,
            "curves": [curve["id"]], "headline": False}


# ================================================================ randomization


def round_rng(plan, phase, name):
    seed = f"{plan['seed']}:{phase}:{name}"
    return random.Random(seed), seed


def blocked_shuffle(items, rng, block_key=None):
    """Shuffle; with block_key, shuffle the blocks and within each block (blocked randomization)."""
    items = list(items)
    if not block_key:
        rng.shuffle(items)
        return items
    blocks = {}
    for it in items:
        blocks.setdefault(block_key(it), []).append(it)
    order = sorted(blocks, key=str)
    rng.shuffle(order)
    out = []
    for b in order:
        rng.shuffle(blocks[b])
        out += blocks[b]
    return out


# ================================================================ adaptive refinement


def _gaining(xs, ys, i, coarse, thr):
    """Is the step xs[i] -> xs[i+1] still gaining? The threshold is scaled to the
    step's share of its enclosing coarse interval, so refining doesn't by itself
    make a linear region look flat."""
    if ys[i] <= 0:
        return ys[i + 1] > ys[i]
    lo = max([c for c in coarse if c <= xs[i]], default=xs[i])
    hi = min([c for c in coarse if c >= xs[i + 1]], default=xs[i + 1])
    span = (hi - lo) or (xs[i + 1] - xs[i])
    return (ys[i + 1] - ys[i]) / ys[i] >= thr * (xs[i + 1] - xs[i]) / span


def knee_index(xs, ys, coarse, thr=0.10):
    """Index of the first point after which the curve stops gaining, or None."""
    for i in range(len(xs) - 1):
        if not _gaining(xs, ys, i, coarse, thr):
            return i
    return None


def knee_bracket(xs, ys, coarse, thr=0.10):
    """(lo, knee, hi) x values around the knee, or None if the curve gains throughout."""
    i = knee_index(xs, ys, coarse, thr)
    if i is None:
        return None
    return xs[max(i - 1, 0)], xs[i], xs[min(i + 1, len(xs) - 1)]


def refine_proposals(xs, ys, coarse, thr=0.10, step=1, integer=True):
    """New x values to measure so the knee ends up within one step. [] when done."""
    if len(xs) < 2:
        return []
    pts = sorted(zip(xs, ys))
    xs, ys = [p[0] for p in pts], [p[1] for p in pts]
    br = knee_bracket(xs, ys, coarse, thr)
    if br is None:
        return []
    lo, k, hi = br
    if k == xs[0]:
        return []          # flat from the first point: the knee is at or below the grid's start
    out = []
    for a, b in ((lo, k), (k, hi)):
        if b - a > step:
            m = (a + b) / 2
            m = int(math.floor(m + 0.5)) if integer else round(math.floor(m / step + 0.5) * step, 10)
            if a < m < b and m not in xs and m not in out:
                out.append(m)
    return out


def curve_xy(curve, results, metric=None):
    """Mean metric per x over usable repetitions, sorted by x."""
    metric = metric or curve["metric"]
    xs, ys = [], []
    for x, uid in curve["points"].items():
        vals = [r["metrics"].get(metric) for r in results.values()
                if r.get("uid") == uid and r.get("status") in ("ok", "no_steady") and r.get("metrics")
                and r["metrics"].get(metric) is not None]
        if vals:
            xs.append(x)
            ys.append(sum(vals) / len(vals))
    pts = sorted(zip(xs, ys))
    return [p[0] for p in pts], [p[1] for p in pts]


# ================================================================ phase loop


def phase_loop(plan, phase, measure, done=None, max_points=None, log=print):
    """Coarse grid -> refinement -> knee repeats -> random re-measurement.

    measure(point, rep, order_meta) -> record. `done` holds prior records
    ({rid: record}) and makes the loop resumable.
    """
    points, curves = expand_phase(plan, phase)
    for p in points.values():
        p["kind"] = phase.get("kind", "globus")
    results = dict(done or {})
    reps = plan["repeats"]
    block = phase.get("block_by")
    block_key = (lambda it: tuple(points[it[0]]["factors"].get(k) for k in block)) if block else None
    budget = [max_points]

    def run_round(name, todo):
        rng, seed = round_rng(plan, phase["name"], name)
        order = blocked_shuffle(sorted(set(todo)), rng, block_key)
        ran = 0
        for idx, (uid, rep) in enumerate(order):
            rid = f"{uid}-r{rep}"
            if rid in results and results[rid].get("status") != "failed":
                continue
            if budget[0] is not None and budget[0] <= 0:
                return ran
            meta = {"round": name, "order_index": idx, "order_seed": seed, "round_size": len(order),
                    "blocked_by": block}
            results[rid] = measure(points[uid], rep, meta)
            ran += 1
            if budget[0] is not None:
                budget[0] -= 1
        log(f"[{phase['name']}] round {name}: {len(order)} scheduled, {ran} measured")
        return ran

    coarse = [(u, 0) for u in points]
    coarse += [(u, r) for u, p in points.items() if p["headline"] for r in range(1, reps["headline"])]
    run_round("coarse", coarse)

    rcfg = plan["refine"]
    for rnd in range(1, int(rcfg["max_rounds"]) + 1):
        new = []
        for c in curves:
            if not c["refine"]:
                continue
            xs, ys = curve_xy(c, results)
            for x in refine_proposals(xs, ys, c["coarse"], rcfg["threshold"], c["step"], c["integer"]):
                p = curve_point(c, x, phase["name"])
                p["kind"] = phase.get("kind", "globus")
                existing = points.setdefault(p["uid"], p)
                if c["id"] not in existing["curves"]:
                    existing["curves"].append(c["id"])
                c["points"][x] = p["uid"]
                new.append((p["uid"], 0))
        if not new:
            break
        run_round(f"refine{rnd}", new)

    knee_uids = set()
    for c in curves:
        if not c["numeric"] or len(c["points"]) < 3:
            continue
        xs, ys = curve_xy(c, results)
        if len(xs) < 3:
            continue
        br = knee_bracket(xs, ys, c["coarse"], rcfg["threshold"])
        c["knee"] = br
        if br:
            knee_uids |= {c["points"][x] for x in set(br)}
    run_round("knee", [(u, r) for u in knee_uids for r in range(1, reps["knee"])])

    others = sorted(u for u, p in points.items() if not p["headline"] and u not in knee_uids)
    rng, _ = round_rng(plan, phase["name"], "remeasure-pick")
    pick = rng.sample(others, math.ceil(reps["random_frac"] * len(others))) if others else []
    run_round("remeasure", [(u, 1) for u in pick])
    return points, curves, results


# ================================================================ dataset and route resolution


_DS_CACHE = {}


def resolve_dataset(plan, ref):
    """'F64M' or 'F1K+naming=sequential+fanout=1' -> info dict (prefix, files, bytes, top dirs)."""
    key = (id(plan), ref)
    if key not in _DS_CACHE:
        _DS_CACHE[key] = _resolve_dataset(plan, ref)
    return _DS_CACHE[key]


def _resolve_dataset(plan, ref):
    if ref in plan["datasets"]:
        d = dict(plan["datasets"][ref])
        d.setdefault("name", ref)
        d.setdefault("top_dirs", [])
        d.setdefault("mean_size", (d["bytes"] / d["files"]) if d.get("files") else None)
        return d
    spec = synth_spec(ref)
    pl = synth.build_plan(spec)
    sizes = [o["size"] for o in pl["ops"]]
    tops = sorted({o["key"][len(pl["prefix"]) + 1:].split("/")[0] for o in pl["ops"]})
    return {"name": ref, "prefix": pl["prefix"], "dir": pl["prefix"].split("/", 1)[1], "files": len(sizes),
            "bytes": sum(sizes), "mean_size": sum(sizes) / max(len(sizes), 1), "top_dirs": tops,
            "spec_hash": pl["spec_hash"]}


_SPEC_CACHE = {}


def synth_spec(ref):
    if ref not in _SPEC_CACHE:
        _SPEC_CACHE[ref] = synth.load_ref(ref)
    return _SPEC_CACHE[ref]


def resolve_route(plan, f, ds, env, uid):
    """Collections, items and cleanup targets for one point."""
    route = plan["routes"][f["route"]]
    ctx = {**env, "campaign": plan["campaign"], "uid": uid,
           "dataset_prefix": ds.get("prefix", ""), "dataset_dir": ds.get("dir", "")}
    fmt = lambda s: s.format(**ctx) if isinstance(s, str) else s  # noqa: E731
    src_col = env.get(ds.get("src_col") or route["src"], ds.get("src_col") or route["src"])
    dst_col = env.get(route["dst"], route["dst"])
    src = fmt(ds.get("src_path") or route["src_path"]).rstrip("/") + "/"
    dst = fmt(route["dst_path"]).rstrip("/") + "/"
    k = int(f["k"])
    tops = ds.get("top_dirs") or []
    tasks = []
    for j in range(k):
        if k > 1 and len(tops) >= k:
            share = tops[j::k]
            items = [f"{src}{t}/=>{dst}{t}/:r" for t in share]
            split = "prefix"
        else:
            sub = f"t{j}/" if k > 1 else ""
            items = [f"{src}=>{dst}{sub}:r"]
            split = "duplicate" if k > 1 else "none"
        if f.get("dst_fanout") and tops:
            m = int(f["dst_fanout"])
            share = tops[j::k] if (k > 1 and len(tops) >= k) else tops
            items = [f"{src}{t}/=>{dst}{'t%d/' % j if split == 'duplicate' else ''}m{i % m:03d}/{t}/:r"
                     for i, t in enumerate(share)]
        tasks.append({"items": items, "split": split})
    cleanup = []
    for c in route.get("cleanup") or []:
        cleanup.append({k2: fmt(v) for k2, v in c.items()})
    s3p = []
    for role in ("read", "write"):
        x = route.get(f"s3_{role}")
        if x and not (role == "read" and ds.get("src_path")):
            s3p.append({"role": role, "bucket": fmt(x["bucket"]), "prefix": fmt(x["prefix"]).lstrip("/")})
    return {"src_col": src_col, "dst_col": dst_col, "src_path": src, "dst_path": dst, "tasks": tasks,
            "cleanup": cleanup, "s3_prefixes": s3p, "sender": route.get("sender", "src"),
            "drop_caches": route.get("drop_caches")}


# ================================================================ prior model (dry run only)


def prior_gbps(plan, f, ds):
    pr = plan["prior"]
    s = (ds.get("mean_size") or 64 * 2 ** 20) * 8
    r = s / (pr["t0_s"] + s / (pr["b_gbps"] * 1e9)) / 1e9
    c = min(int(f.get("c", 1)), MAX_TASK_CONCURRENCY)
    per_task = c * r
    nic = lambda t: INSTANCE.get(t, (0, 12.5))[1] * pr["node_frac"]  # noqa: E731
    return min(int(f.get("k", 1)) * per_task, int(f["n_src"]) * nic(f["instance_type_src"]),
               int(f["n_dst"]) * nic(f["instance_type_dst"]))


class Estimator:
    """Wall time, bytes and cost of a schedule, tracking topology changes between points."""

    def __init__(self, plan):
        self.plan = plan
        self.state = {}
        self.t = 0.0
        self.bytes = 0.0
        self.inst_usd = 0.0
        self.points = 0

    def add(self, point):
        f, m = point["factors"], self.plan["measure"]
        kind = point["kind"]
        dt = 30.0  # samplers, submit, collect, cleanup
        st = self.state
        for side in ("src", "dst"):
            if st.get(f"type_{side}") not in (None, f[f"instance_type_{side}"]):
                dt += 300           # scale to 0, modify type, revive
            elif st.get(f"n_{side}") != f[f"n_{side}"]:
                dt += 150           # slot controller converges + DNS gate
        if kind == "globus":
            key = (f["network_use"], f["c"], f["p"])
            if st.get("nu") != key:
                dt += 25
            if st.get("netem") != (f["rtt_ms"], f["loss_pct"]):
                dt += 15
            ds = resolve_dataset(self.plan, f["dataset"])
            rate = prior_gbps(self.plan, f, ds)
            if m["mode"] == "fixed":
                run = m["fixed_s"]
            else:
                run = self.plan["prior"]["ramp_s"] + m["hold_s"] + m["window_s"] + m["measure_s"] + m["poll_s"]
            moved = min(rate * 1e9 / 8 * run, ds["bytes"] * int(f["k"]))
            dt += run + 20
            st.update(nu=key, netem=(f["rtt_ms"], f["loss_pct"]))
            self.bytes += moved
        elif kind == "iperf3":
            run = float(f.get("duration_s", 90))
            dt += run + 20
            self.bytes += INSTANCE.get(f["instance_type_src"], (0, 12.5))[1] * 1e9 / 8 * run
        else:  # s3probe: in-region S3, no transfer charge
            dt += 60
        for side in ("src", "dst"):
            st[f"type_{side}"] = f[f"instance_type_{side}"]
            st[f"n_{side}"] = f[f"n_{side}"]
        hourly = sum(int(f[f"n_{s}"]) * INSTANCE.get(f[f"instance_type_{s}"], (0.544, 0))[0] for s in ("src", "dst"))
        hourly += 2 * INSTANCE["t3a.large"][0]
        self.inst_usd += hourly * dt / 3600
        self.t += dt
        self.points += 1

    def summary(self):
        xfer = self.bytes / 1e9 * TRANSFER_USD_PER_GB
        return {"points": self.points, "hours": round(self.t / 3600, 2), "tb_moved": round(self.bytes / 1e12, 2),
                "transfer_usd": round(xfer, 2), "instance_usd": round(self.inst_usd, 2),
                "total_usd": round(xfer + self.inst_usd, 2)}


def simulate(plan, phase, seed=0):
    """Dry run: run the phase loop against the prior model to estimate refinement and repeats."""
    plan = phase_plan(plan, phase)
    est = Estimator(plan)
    rng = random.Random(f"{plan['seed']}:{phase['name']}:sim:{seed}")
    order = []

    def measure(point, rep, meta):
        est.add(point)
        order.append((point, rep, meta))
        f = point["factors"]
        if point["kind"] == "globus":
            ds = resolve_dataset(plan, f["dataset"])
            g = prior_gbps(plan, f, ds) * (1 + rng.gauss(0, plan["prior"]["noise_cv"]))
            fps = g * 1e9 / 8 / max(ds["mean_size"] or 1, 1)
            metrics = {"steady_gbps": g, "wall_gbps": 0.7 * g, "files_per_s": fps}
        else:
            g = INSTANCE.get(f["instance_type_src"], (0, 12.5))[1] * (1 + rng.gauss(0, 0.02))
            metrics = {"steady_gbps": g, "gbps": g}
        return {"uid": point["uid"], "rep": rep, "status": "ok", "metrics": metrics, **meta}

    points, curves, _ = phase_loop(plan, phase, measure, log=lambda *_: None)
    return est, order, points, curves


# ================================================================ record assembly (pure)


def parse_nic(text):
    rows = []
    for ln in (text or "").splitlines():
        p = ln.split()
        if len(p) >= 8:
            try:
                rows.append({"t": float(p[0]), "rx": int(p[1]), "tx": int(p[2]), "busy": int(p[3]),
                             "total": int(p[4]), "mem_kb": int(p[5]), "retrans": int(p[6]),
                             "gridftp_procs": int(p[7])})
            except ValueError:
                continue
    return rows


def parse_proc(text):
    procs, cores = {}, {}
    for ln in (text or "").splitlines():
        p = ln.split()
        try:
            if p[0] == "P" and len(p) == 5:
                procs.setdefault(p[2], []).append((float(p[1]), int(p[3]) + int(p[4])))
            elif p[0] == "C" and len(p) == 5:
                cores.setdefault(p[2], []).append((float(p[1]), int(p[3]), int(p[4])))
        except (ValueError, IndexError):
            continue
    return procs, cores


def _rate(rows, key, t0, t1):
    sel = [r for r in rows if t0 - 1.5 <= r["t"] <= t1 + 1.5]
    if len(sel) < 2:
        return None
    a, b = sel[0], sel[-1]
    return (b[key] - a[key]) / (b["t"] - a["t"]) if b["t"] > a["t"] else None


def node_stats(rows, proc_text, t0, t1, hz=100):
    """USE-method utilization of one node over [t0, t1]."""
    sel = [r for r in rows if t0 - 1.5 <= r["t"] <= t1 + 1.5]
    out = {"samples": len(sel)}
    if len(sel) >= 2:
        a, b = sel[0], sel[-1]
        out["rx_gbps"] = round(8 * _rate(sel, "rx", t0, t1) / 1e9, 3)
        out["tx_gbps"] = round(8 * _rate(sel, "tx", t0, t1) / 1e9, 3)
        out["cpu_pct"] = round(100 * (b["busy"] - a["busy"]) / max(b["total"] - a["total"], 1), 1)
        out["retrans_per_s"] = round(_rate(sel, "retrans", t0, t1) or 0, 1)
        out["mem_avail_min_mb"] = round(min(r["mem_kb"] for r in sel) / 1024)
        out["gridftp_procs_max"] = max(r["gridftp_procs"] for r in sel)
    procs, cores = parse_proc(proc_text)
    best = 0.0
    for samples in procs.values():
        s = [x for x in samples if t0 - 5 <= x[0] <= t1 + 5]
        if len(s) >= 2 and s[-1][0] > s[0][0]:
            best = max(best, 100 * (s[-1][1] - s[0][1]) / hz / (s[-1][0] - s[0][0]))
    if procs:
        out["gridftp_proc_cpu_max_pct"] = round(best, 1)
    core_max = 0.0
    for samples in cores.values():
        s = [x for x in samples if t0 - 5 <= x[0] <= t1 + 5]
        if len(s) >= 2:
            core_max = max(core_max, 100 * (s[-1][1] - s[0][1]) / max(s[-1][2] - s[0][2], 1))
    if cores:
        out["core_busy_max_pct"] = round(core_max, 1)
    return out


_RTT = re.compile(r"\brtt:([0-9.]+)/")


def parse_ss(text):
    rtts = sorted(float(m.group(1)) for m in _RTT.finditer(text or ""))
    cc = {}
    for name in ("bbr", "cubic", "reno", "htcp"):
        n = len(re.findall(rf"\b{name}\b", text or ""))
        if n:
            cc[name] = n
    if not rtts:
        return {"connections": 0, "cc": cc}
    return {"connections": len(rtts), "rtt_ms_min": rtts[0], "rtt_ms_median": rtts[len(rtts) // 2],
            "rtt_ms_max": rtts[-1], "cc": cc}


def runner_summary(lines):
    """Task outcome from one bench_transfer.py log."""
    sub = next((ln for ln in lines if ln.get("event") == "submitted"), {})
    done = next((ln for ln in reversed(lines) if ln.get("event") == "done"), None)
    prog = [ln for ln in lines if ln.get("event") == "progress"]
    ev = next((ln for ln in reversed(lines) if ln.get("event") == "events"), None)
    neg = next((ln for ln in lines if ln.get("event") == "negotiated"), None)
    started = (ev or {}).get("started") or (neg or {}).get("started") or []
    out = {"task_id": sub.get("task_id"), "t_submit": sub.get("t")}
    if done:
        out.update({k: done.get(k) for k in ("status", "nice_status", "bytes_transferred", "files_transferred",
                                             "faults", "wall_s", "cancelled", "request_time", "completion_time")})
        out["wall_gbps"] = round(8 * (done.get("bytes_transferred") or 0) / max(done.get("wall_s") or 1, 1) / 1e9, 3)
    elif prog:
        out.update(status="INCOMPLETE", bytes_transferred=prog[-1].get("bytes_transferred"),
                   files_transferred=prog[-1].get("files_transferred"), faults=prog[-1].get("faults"))
    out["negotiated"] = [s.get("negotiated") for s in started]
    out["started_events"] = len(started)
    if ev:
        out["event_counts"] = ev.get("counts")
    return out


def build_record(point, rep, meta, plan, art, nodes, ctx):
    """Assemble one points.jsonl record from collected artifacts (no I/O)."""
    m = plan["measure"]
    f = point["factors"]
    sender = ctx.get("sender", "src")
    nic = {i: parse_nic(t) for i, t in art.get("nic", {}).items()}
    send_ids = [n["id"] for n in nodes.get(sender, [])]
    recv_ids = [n["id"] for n in nodes.get("dst" if sender == "src" else "src", [])]
    series = {i: [(r["t"], r["tx"]) for r in nic.get(i, [])] for i in send_ids}
    ts, ys = aggregate_rates(series)
    ys_g = [y / 1e9 for y in ys]
    if m["mode"] == "fixed":
        t0 = (ts[0] + m["settle_s"]) if ts else None
        idx = [i for i, t in enumerate(ts) if t0 is not None and t >= t0]
        st = {"found": bool(idx), "complete": bool(idx), "onset_t": t0,
              "window": [t0, ts[idx[-1]]] if idx else None,
              "stats": window_stats([ts[i] for i in idx], [ys_g[i] for i in idx]) if len(idx) >= 3 else None}
    else:
        st = detect_steady(ts, ys_g, m["window_s"], m["hold_s"], m["measure_s"], m["alpha"], m["min_frac"],
                           m["min_level_gbps"])
        st.pop("stable_mask", None)
    if st["window"]:
        w0, w1 = st["window"]
        w1 = min(w1, ts[-1]) if ts else w1
    else:
        w0, w1 = (ts[0], ts[-1]) if ts else (None, None)
    rx_series = {i: [(r["t"], r["rx"]) for r in nic.get(i, [])] for i in recv_ids}
    rts, rys = aggregate_rates(rx_series)
    rx_win = [y / 1e9 for t, y in zip(rts, rys) if w0 is not None and w0 <= t <= w1]
    node_rows = []
    for side, ns in nodes.items():
        for n in ns:
            row = {"side": side, "id": n["id"], "role": n.get("role", "dtn"), "type": n.get("type"),
                   "ip": n.get("ip")}
            if w0 is not None:
                row.update(node_stats(nic.get(n["id"], []), art.get("proc", {}).get(n["id"], ""), w0, w1))
            node_rows.append(row)
    tasks = [runner_summary(lines) for lines in art.get("runner", {}).values()]
    gl_send, gl_recv = [], []
    for side, ns in nodes.items():
        for n in ns:
            text = "\n".join(art.get("gridftp", {}).get(n["id"], {}).values())
            recs, _ = gridftp_log.parse(text.splitlines())
            (gl_send if side == sender else gl_recv).extend(recs)
    gsum = {"send": gridftp_log.summarize(gl_send, w0, w1).get("send"),
            "recv": gridftp_log.summarize(gl_recv, w0, w1).get("recv")}
    files_win = None
    if w0 is not None and w1 and w1 > w0:
        done_in = [r for r in (gl_recv or gl_send) if w0 <= r["end"] <= w1 and r["code"] in (None, "226")]
        files_win = round(len(done_in) / (w1 - w0), 2)
    s = st.get("stats") or {}
    wall = [t["wall_gbps"] for t in tasks if t.get("wall_gbps") is not None]
    metrics = {"steady_gbps": s.get("mean") if st.get("complete") else None,
               "window_gbps": s.get("mean"), "steady_ci95": s.get("ci95"), "steady_cv": s.get("cv"),
               "recv_rx_gbps": round(sum(rx_win) / len(rx_win), 3) if rx_win else None,
               "wall_gbps": round(sum(wall), 3) if wall else None, "files_per_s": files_win,
               "gridftp_window_gbps": (gsum["send"] or {}).get("window_gbps")}
    negotiated = [n for t in tasks for n in t.get("negotiated", [])]
    status = "ok" if st.get("complete") else ("no_steady" if ts else "failed")
    return {"schema": SCHEMA, "campaign": plan["campaign"], "phase": point["phase"], "kind": point["kind"],
            "uid": point["uid"], "rid": f"{point['uid']}-r{rep}", "rep": rep, "factors": f,
            "headline": point["headline"], "curves": point["curves"], **meta, "status": status,
            "metrics": metrics, "steady": st, "tasks": tasks, "negotiated": negotiated, "nodes": node_rows,
            "gridftp": gsum, "ss": {i: parse_ss(t) for i, t in art.get("ss", {}).items()},
            "series": {"t0": ts[0] if ts else None, "gbps": [round(y, 4) for y in ys_g]}, **ctx}


# ================================================================ live backend


class LiveBackend:
    """Everything that touches AWS or Globus. Constructed only for a real run."""

    def __init__(self, env, log=print):
        self.env, self.log = env, log
        self.region = env["REGION"]
        self.ssm = hlib.SSM(self.region)
        self.ec2 = hlib.boto3_client("ec2", self.region)
        self.params = hlib.boto3_client("ssm", self.region)
        self.events = hlib.boto3_client("events", self.region)
        self.s3 = hlib.boto3_client("s3", self.region)
        self.control = {"src": env["SRC_CONTROL"], "dst": env["DST_CONTROL"]}
        self.state = {}
        self._rules = {}
        self._fqdn = {}

    @staticmethod
    def now():
        return time.time()

    @staticmethod
    def sleep(s):
        time.sleep(s)

    # ---------------- topology

    def rule(self, dep):
        return f"elastic-gcs-demand-monitor-{self.region}-{dep}"

    def pause_demand(self):
        for dep in ("src", "dst"):
            r = self.events.describe_rule(Name=self.rule(dep))
            self._rules[dep] = r["State"]
            self.events.disable_rule(Name=self.rule(dep))
        self.log(f"demand monitors disabled (were {self._rules})")

    def resume_demand(self):
        for dep, state in self._rules.items():
            if state == "ENABLED":
                self.events.enable_rule(Name=self.rule(dep))
        self.log("demand monitors restored")

    def slots(self, dep):
        r = self.ec2.describe_instances(Filters=[
            {"Name": "tag:elastic-gcs:deployment", "Values": [f"{self.region}-{dep}"]},
            {"Name": "tag:elastic-gcs:pool", "Values": ["slots"]},
            {"Name": "instance-state-name", "Values": ["pending", "running", "stopping", "stopped"]}])
        out = []
        for res in r["Reservations"]:
            for i in res["Instances"]:
                tags = {t["Key"]: t["Value"] for t in i.get("Tags", [])}
                out.append({"id": i["InstanceId"], "state": i["State"]["Name"], "ip": i.get("PublicIpAddress"),
                            "type": i["InstanceType"], "phase": tags.get("elastic-gcs:phase"),
                            "slot": tags.get("elastic-gcs:slot")})
        return out

    def serving(self, dep):
        return [s for s in self.slots(dep) if s["state"] == "running" and not s["phase"]]

    def set_desired(self, dep, n):
        p = f"/elastic-gcs/{self.region}/{dep}"
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        self.params.put_parameter(Name=f"{p}/last-active", Value=now, Type="String", Overwrite=True)
        self.params.put_parameter(Name=f"{p}/desired-dtns", Value=str(n), Type="String", Overwrite=True)

    def fqdn(self, dep):
        key = f"{dep.upper()}_ENDPOINT_FQDN"
        if self.env.get(key):
            return self.env[key]
        if dep not in self._fqdn:
            _, out, _ = self.ssm.run(self.control[dep], hlib.gcs_command(
                self.region, dep, "globus-connect-server endpoint show -F json"))
            m = re.search(r"https?://([a-z0-9.-]+\.data\.globus\.org)", out)
            if not m:
                raise RuntimeError(f"can't find {dep} endpoint FQDN; set {key} in bench.env")
            self._fqdn[dep] = m.group(1)
        return self._fqdn[dep]

    @staticmethod
    def resolve(name):
        try:
            out = subprocess.run(["dig", "+short", "A", name, "@1.1.1.1"], capture_output=True, text=True,
                                 timeout=10).stdout
            ips = {x for x in out.split() if re.fullmatch(r"\d+\.\d+\.\d+\.\d+", x)}
            if ips:
                return ips
        except (OSError, subprocess.TimeoutExpired):
            pass
        try:
            return set(socket.gethostbyname_ex(name)[2])
        except OSError:
            return set()

    def ensure_dtns(self, dep, n, timeout=1500):
        """Pin desired-dtns and wait for the DNS gate: endpoint DNS == exactly the serving DTNs' EIPs."""
        if self.state.get(f"n_{dep}") == n and len(self.serving(dep)) == n:
            return {"changed": False}
        t0 = time.time()
        self.set_desired(dep, n)
        while time.time() - t0 < timeout:
            srv = self.serving(dep)
            ips = {s["ip"] for s in srv}
            dns = self.resolve(self.fqdn(dep))
            if len(srv) == n and dns == ips:
                self.state[f"n_{dep}"] = n
                return {"changed": True, "ready_s": round(time.time() - t0, 1), "dns": sorted(dns)}
            time.sleep(10)
        raise TimeoutError(f"{dep}: {n} DTNs not ready in DNS after {timeout}s")

    def ensure_instance_type(self, dep, itype):
        """Harness-only override (#151 section 9): stop every slot, change the type of the
        stopped ones, then let ensure_dtns revive them. New launches still use the
        launch template's type, so this needs >= n stopped (warm) slot instances."""
        if self.state.get(f"type_{dep}") == itype:
            return {"changed": False}
        slots = self.slots(dep)
        if slots and all(s["type"] == itype for s in slots):
            self.state[f"type_{dep}"] = itype
            return {"changed": False}
        self.set_desired(dep, 0)
        self.state.pop(f"n_{dep}", None)
        t0 = time.time()
        while any(s["state"] in ("pending", "running", "stopping") for s in self.slots(dep)):
            if time.time() - t0 > 1200:
                raise TimeoutError(f"{dep}: slots did not stop")
            time.sleep(15)
        changed = []
        for s in self.slots(dep):
            if s["state"] == "stopped" and s["type"] != itype:
                self.ec2.modify_instance_attribute(InstanceId=s["id"], InstanceType={"Value": itype})
                changed.append(s["id"])
        self.state[f"type_{dep}"] = itype
        return {"changed": True, "modified": changed}

    def ensure_network_use(self, dep, f):
        key = (f["network_use"], f["c"], f["p"])
        if self.state.get(f"nu_{dep}") == key:
            return self.state[f"nu_show_{dep}"]
        cli = hlib.network_use_cli(f["c"], f["p"], f["network_use"])
        st, out, err = self.ssm.run(self.control[dep], hlib.assert_no_inline_secret(
            hlib.gcs_command(self.region, dep, f"{cli} && globus-connect-server endpoint show -F json")))
        if st != "Success":
            raise RuntimeError(f"{dep} network-use: {err.strip()[-300:]}")
        shown = {}
        try:
            doc = json.loads(out[out.index("{"):])
            doc = doc[0] if isinstance(doc, list) else doc
            doc = doc.get("data", [doc])[0] if isinstance(doc.get("data"), list) else doc
            shown = {k: v for k, v in doc.items() if "concurrency" in k or "parallelism" in k or "network_use" in k}
        except (ValueError, AttributeError, IndexError):
            shown = {"raw": out[-500:]}
        self.state[f"nu_{dep}"] = key
        self.state[f"nu_show_{dep}"] = shown
        return shown

    def ensure_netem(self, rtt_ms, loss_pct, on):
        key = (rtt_ms, loss_pct, on)
        if self.state.get("netem") == key:
            return self.state["netem_out"]
        arg = "off" if not rtt_ms and not loss_pct else str(rtt_ms)
        r = subprocess.run([str(hlib.HARNESS / "netem.sh"), arg, str(loss_pct or 0), on], capture_output=True,
                           text=True, timeout=300)
        out = r.stdout.strip().splitlines()
        if arg != "off" and not any("netem" in x for x in out):
            raise RuntimeError(f"netem not applied: {r.stdout} {r.stderr}")
        self.state["netem"], self.state["netem_out"] = key, out
        return out

    def nodes(self):
        out = {}
        for dep in ("src", "dst"):
            out[dep] = [{"id": s["id"], "ip": s["ip"], "type": s["type"], "role": "dtn"} for s in self.serving(dep)]
        return out

    def controls(self):
        return {dep: [{"id": i, "role": "control"}] for dep, i in self.control.items()}

    def instance_info(self, types):
        r = self.ec2.describe_instance_types(InstanceTypes=sorted(set(types)))
        return {t["InstanceType"]: {"vcpu": t["VCpuInfo"]["DefaultVCpus"], "mem_mib": t["MemoryInfo"]["SizeInMiB"],
                                    "network": t["NetworkInfo"].get("NetworkPerformance"),
                                    "baseline_gbps": [c.get("BaselineBandwidthInGbps")
                                                      for c in t["NetworkInfo"].get("NetworkCards", [])]}
                for t in r["InstanceTypes"]}

    # ---------------- per-point node work

    def prep(self, dtn_ids, control_ids, tag, duration, drop_caches_ids=(), gridftp=True):
        """Ship the sampler, enable GridFTP transfer records, note log offsets, start sampling."""
        b64 = base64.b64encode((hlib.HARNESS / "dtn_sampler.sh").read_bytes()).decode()
        out_prefix = f"{hlib.REMOTE_TMP}/{tag}"
        start = (f"mkdir -p {hlib.REMOTE_TMP}; echo {b64} | base64 -d > {hlib.REMOTE_TMP}/dtn_sampler.sh; "
                 f"nohup bash {hlib.REMOTE_TMP}/dtn_sampler.sh {out_prefix} {int(duration)} >/dev/null 2>&1 & ")
        offsets = {}
        cmd = f"{gridftp_log.enable_command()} >/dev/null; {gridftp_log.offsets_command()}; " if gridftp else ""
        res = self.ssm.run_many(dtn_ids, cmd + start + "echo started")
        for i, (st, out, err) in res.items():
            if "started" not in out:
                raise RuntimeError(f"prep {i}: {st} {err.strip()[-300:]}")
            offsets[i] = gridftp_log.parse_offsets(out)
        if control_ids:
            self.ssm.run_many(control_ids, start + "echo started")
        if drop_caches_ids:
            self.ssm.run_many(drop_caches_ids, "sync; echo 3 > /proc/sys/vm/drop_caches")
        return offsets

    def poll_samples(self, ids, tag, lines=40):
        res = self.ssm.run_many(ids, f"tail -n {lines} {hlib.REMOTE_TMP}/{tag}.nic")
        return {i: parse_nic(out) for i, (_, out, _) in res.items()}

    def snapshot_ss(self, ids):
        q = "'( sport >= :50000 and sport <= :51000 ) or ( dport >= :50000 and dport <= :51000 )'"
        res = self.ssm.run_many(ids, f"ss -tin state established {q} | head -c 20000")
        return {i: out for i, (_, out, _) in res.items()}

    def submit(self, run_id, label, src_col, dst_col, items, env):
        cmd = hlib.assert_no_inline_secret(hlib.runner_command(self.region, run_id, label, src_col, dst_col, items, env))
        st, out, err = self.ssm.run(self.control["src"], cmd)
        line = next((ln for ln in out.splitlines() if ln.startswith("{")), None)
        if st != "Success" or not line:
            raise RuntimeError(f"submit {run_id}: {st} {out.strip()[-300:]} {err.strip()[-300:]}")
        return json.loads(line)

    def runner_state(self, run_ids):
        files = " ".join(f"{hlib.BENCH_DIR}/runs/{shlex.quote(r)}.jsonl" for r in run_ids)
        _, out, _ = self.ssm.run(self.control["src"], f"for f in {files}; do echo \"## $f\"; "
                                                      "grep -h '\"event\": \"done\"' $f || tail -n 1 $f; done")
        state, cur = {}, None
        for ln in out.splitlines():
            if ln.startswith("## "):
                cur = Path(ln[3:]).stem
            elif cur and ln.startswith("{"):
                try:
                    state[cur] = json.loads(ln)
                except ValueError:
                    pass
            elif cur and ("Traceback" in ln or "Error" in ln):
                state[cur] = {"event": "error", "text": ln[:300]}
        return state

    def cancel(self, run_ids):
        self.ssm.run(self.control["src"], " ".join(f"touch {hlib.BENCH_DIR}/runs/{shlex.quote(r)}.cancel;"
                                                    for r in run_ids))

    def collect(self, point_dir, run_ids, dtn_ids, control_ids, tag, offsets, ss, gridftp=True):
        point_dir.mkdir(parents=True, exist_ok=True)
        ids = list(dtn_ids) + list(control_ids)
        self.ssm.run_many(ids, f"touch {hlib.REMOTE_TMP}/{tag}.stop")
        art = {"runner": {}, "nic": {}, "proc": {}, "gridftp": {}, "ss": ss}
        for r in run_ids:
            raw = self.ssm.fetch_file(self.control["src"], f"{hlib.BENCH_DIR}/runs/{r}.jsonl").decode()
            (point_dir / f"{r}.jsonl").write_text(raw)
            art["runner"][r] = [json.loads(x) for x in raw.splitlines() if x.startswith("{")]
        for i in ids:
            for ext in ("nic", "proc"):
                txt = self.ssm.fetch_file(i, f"{hlib.REMOTE_TMP}/{tag}.{ext}").decode()
                (point_dir / f"{i}.{ext}").write_text(txt)
                art[ext][i] = txt
        for i in (dtn_ids if gridftp else []):
            logs = gridftp_log.collect(self.ssm, i, offsets.get(i))
            art["gridftp"][i] = logs
            for p, txt in logs.items():
                if txt:
                    (point_dir / f"{i}.{Path(p).name}").write_text(txt)
        for i, txt in ss.items():
            (point_dir / f"{i}.ss.txt").write_text(txt)
        self.ssm.run_many(ids, f"rm -f {hlib.REMOTE_TMP}/{tag}.*")
        return art

    def cleanup(self, targets):
        out = []
        for c in targets:
            if c.get("kind") == "s3":
                n = synth.delete_prefix(self.s3, c["bucket"], c["prefix"].lstrip("/"))
                out.append({"s3": f"s3://{c['bucket']}/{c['prefix']}", "deleted": n})
            elif c.get("kind") == "posix":
                ids = [n["id"] for n in self.nodes()[c.get("side", "dst")]]
                path = c["path"].rstrip("/")
                if not path.startswith("/") or path.count("/") < 2:
                    raise ValueError(f"refusing to rm -rf {path!r}")
                self.ssm.run_many(ids, f"rm -rf {shlex.quote(path)}")
                out.append({"posix": path, "on": ids})
        return out

    def run_on(self, iid, cmd, timeout=600):
        return self.ssm.run(iid, cmd, timeout)


# ================================================================ point runners


def run_globus_point(be, plan, point, rep, meta, out_dir, keep=False, log=print):
    f, m = point["factors"], plan["measure"]
    rid = f"{point['uid']}-r{rep}"
    pdir = out_dir / "raw" / rid
    ds = resolve_dataset(plan, f["dataset"])
    rt = resolve_route(plan, f, ds, be.env, rid)
    ctx = {"t_start": be.now(), "harness_commit": hlib.git_commit(), "dataset": ds.get("name"),
           "dataset_prefix": ds.get("prefix"), "dataset_spec_hash": ds.get("spec_hash"),
           "dataset_mean_size": ds.get("mean_size"),
           "route": {k: rt[k] for k in ("src_col", "dst_col", "src_path", "dst_path", "sender")},
           "s3_prefixes": rt["s3_prefixes"], "sender": rt["sender"]}
    try:
        topo = {}
        for dep in ("src", "dst"):
            topo[f"type_{dep}"] = be.ensure_instance_type(dep, f[f"instance_type_{dep}"])
        for dep in ("src", "dst"):
            topo[f"n_{dep}"] = be.ensure_dtns(dep, int(f[f"n_{dep}"]))
        ctx["topology"] = topo
        ctx["network_use"] = {dep: be.ensure_network_use(dep, f) for dep in ("src", "dst")}
        ctx["netem"] = be.ensure_netem(f["rtt_ms"], f["loss_pct"], rt["sender"])
        nodes = be.nodes()
        for dep in ("src", "dst"):
            types = {n["type"] for n in nodes[dep]}
            if types != {f[f"instance_type_{dep}"]}:
                raise RuntimeError(f"{dep} DTN types {types} != {f[f'instance_type_{dep}']}: "
                                   "the override needs stopped warm slots")
        ctx["instance_info"] = be.instance_info([n["type"] for ns in nodes.values() for n in ns])
        dtn_ids = [n["id"] for ns in nodes.values() for n in ns]
        ctl = be.controls()
        ctl_ids = [n["id"] for ns in ctl.values() for n in ns]
        drop = [n["id"] for n in nodes[rt["drop_caches"]]] if rt.get("drop_caches") else []
        offsets = be.prep(dtn_ids, ctl_ids, rid, m["max_point_s"] + 300, drop)
        runs = [f"{rid}-t{j}" for j in range(int(f["k"]))]
        env = {"VERIFY_CHECKSUM": int(bool(f["verify_checksum"])), "ENCRYPT_DATA": int(bool(f["encrypt_data"])),
               "POLL_S": 5}
        for j, r in enumerate(runs):
            be.submit(r, f"envelope {point['phase']} {rid} t{j}", rt["src_col"], rt["dst_col"],
                      rt["tasks"][j]["items"], env)
        ctx["t_submit"] = be.now()
        ctx["split"] = rt["tasks"][0]["split"]
        tracker = SteadyTracker(window_s=m["window_s"], hold_s=m["hold_s"], measure_s=m["measure_s"],
                                alpha=m["alpha"], min_frac=m["min_frac"], min_level=m["min_level_gbps"] * 1e9)
        send_ids = [n["id"] for n in nodes[rt["sender"]]]
        ss, why = {}, "timeout"
        while True:
            be.sleep(m["poll_s"])
            for i, rows in be.poll_samples(send_ids, rid).items():
                for r in rows:
                    tracker.add(i, r["t"], r["tx"])
            res = tracker.result()
            el = be.now() - ctx["t_submit"]
            if res["found"] and not ss:
                ss = be.snapshot_ss(dtn_ids)
            st = be.runner_state(runs)
            if any(s.get("event") == "error" for s in st.values()):
                why = "runner_error"
                break
            if st and all(s.get("event") == "done" for s in st.values()) and len(st) == len(runs):
                why = "tasks_finished"
                break
            if m["mode"] == "fixed" and el >= m["fixed_s"]:
                why = "fixed_window"
                break
            if m["mode"] != "fixed" and res["complete"]:
                why = "steady_window"
                break
            if el > m["max_point_s"]:
                break
        ctx["stop_reason"] = why
        if why != "tasks_finished":
            be.cancel(runs)
            t0 = be.now()
            while be.now() - t0 < 300:
                be.sleep(10)
                st = be.runner_state(runs)
                if all(s.get("event") in ("done", "error") for s in st.values()) and len(st) == len(runs):
                    break
        ctx["t_end"] = be.now()
        art = be.collect(pdir, runs, dtn_ids, ctl_ids, rid, offsets, ss)
        all_nodes = {dep: nodes[dep] + ctl[dep] for dep in ("src", "dst")}
        rec = build_record(point, rep, meta, plan, art, all_nodes, ctx)
    except Exception as e:  # noqa: BLE001 - a failed point is recorded, the campaign goes on
        log(f"  point {rid} failed: {e!r}")
        rec = {"schema": SCHEMA, "campaign": plan["campaign"], "phase": point["phase"], "kind": point["kind"],
               "uid": point["uid"], "rid": rid, "rep": rep, "factors": f, "headline": point["headline"],
               "curves": point["curves"], **meta, **ctx, "status": "failed", "error": repr(e)[:1000],
               "metrics": {}}
    finally:
        if not keep:
            try:
                ctx["cleanup"] = be.cleanup(rt["cleanup"])
            except Exception as e:  # noqa: BLE001
                log(f"  cleanup {rid} failed: {e!r} (delete {rt['cleanup']} by hand)")
    rec["cleanup"] = ctx.get("cleanup")
    return rec


def run_iperf3_point(be, plan, point, rep, meta, out_dir, keep=False, log=print):
    """P0 network control: iperf3 from the src DTN to the dst DTN's public IP (the Globus path)."""
    f = point["factors"]
    rid = f"{point['uid']}-r{rep}"
    pdir = out_dir / "raw" / rid
    procs, flows, dur = int(f.get("procs", 4)), int(f.get("flows", 8)), int(f.get("duration_s", 90))
    ctx = {"t_start": time.time(), "harness_commit": hlib.git_commit(), "sender": "src"}
    try:
        for dep in ("src", "dst"):
            be.ensure_instance_type(dep, f[f"instance_type_{dep}"])
        for dep in ("src", "dst"):
            be.ensure_dtns(dep, 1)
        be.ensure_netem(0, 0, "src")
        nodes = be.nodes()
        src, dst = nodes["src"][0], nodes["dst"][0]
        ctx["instance_info"] = be.instance_info([src["type"], dst["type"]])
        ids = [src["id"], dst["id"]]
        be.prep(ids, [], rid, dur + 120, gridftp=False)
        inst = "command -v iperf3 >/dev/null || dnf -y -q install iperf3 >/dev/null 2>&1; "
        be.run_on(dst["id"], inst + f"pkill -x iperf3; for i in $(seq 0 {procs - 1}); do "
                                    f"iperf3 -s -D -1 -p $((51000 - i)); done; sleep 1; pgrep -c iperf3")
        ctx["t_submit"] = time.time()
        _, out, _ = be.run_on(src["id"], inst + f"for i in $(seq 0 {procs - 1}); do iperf3 -c {dst['ip']} "
                                                f"-p $((51000 - i)) -P {flows} -t {dur} -O 3 -J > "
                                                f"{hlib.REMOTE_TMP}/iperf-$i.json & done; wait; "
                                                f"cat {hlib.REMOTE_TMP}/iperf-*.json | gzip -c | base64 -w0",
                              timeout=dur + 300)
        ctx["t_end"] = time.time()
        import gzip as _gz  # noqa: PLC0415
        raw = _gz.decompress(base64.b64decode(out.strip())).decode() if out.strip() else ""
        pdir.mkdir(parents=True, exist_ok=True)
        (pdir / "iperf3.json").write_text(raw)
        sums = []
        for doc in re.split(r"(?<=\n})\s*(?=\{)", raw.strip()):
            try:
                d = json.loads(doc)
                sums.append((d["end"]["sum_sent"]["bits_per_second"], d["end"]["sum_sent"].get("retransmits")))
            except (ValueError, KeyError):
                continue
        art = be.collect(pdir, [], ids, [], rid, {}, {}, gridftp=False)
        rec = build_record(point, rep, meta, plan, art, {"src": [src], "dst": [dst]}, ctx)
        rec["iperf3"] = {"procs": procs, "flows_per_proc": flows, "processes_reported": len(sums),
                         "gbps": round(sum(s[0] for s in sums) / 1e9, 3),
                         "retransmits": sum(s[1] or 0 for s in sums)}
        rec["metrics"]["gbps"] = rec["iperf3"]["gbps"]
        rec["metrics"]["steady_gbps"] = rec["metrics"]["steady_gbps"] or rec["iperf3"]["gbps"]
        rec["status"] = "ok" if sums else "failed"
    except Exception as e:  # noqa: BLE001
        log(f"  point {rid} failed: {e!r}")
        rec = {"schema": SCHEMA, "campaign": plan["campaign"], "phase": point["phase"], "kind": point["kind"],
               "uid": point["uid"], "rid": rid, "rep": rep, "factors": f, **meta, **ctx, "status": "failed",
               "error": repr(e)[:1000], "metrics": {}}
    return rec


def run_s3probe_point(be, plan, point, rep, meta, out_dir, keep=False, log=print):
    """P0 S3 control: CRT client GET/PUT on the src DTN (s3probe.py)."""
    f = point["factors"]
    rid = f"{point['uid']}-r{rep}"
    pdir = out_dir / "raw" / rid
    ds = resolve_dataset(plan, f["dataset"])
    bucket = be.env["SYNTH_BUCKET"]
    size = int(ds["mean_size"])
    inflight = int(f.get("inflight", 1))
    count = int(f.get("count") or max(4 * inflight, 16))
    probe_prefix = f"probe/{plan['campaign']}/{rid}/"
    ctx = {"t_start": time.time(), "harness_commit": hlib.git_commit(), "sender": "src",
           "s3_prefixes": [{"role": "read" if f["op"] == "get" else "write", "bucket": bucket,
                            "prefix": ds["prefix"] + "/" if f["op"] == "get" else probe_prefix}]}
    try:
        be.ensure_instance_type("src", f["instance_type_src"])
        be.ensure_dtns("src", 1)
        node = be.nodes()["src"][0]
        ctx["instance_info"] = be.instance_info([node["type"]])
        be.prep([node["id"]], [], rid, 3600, gridftp=False)
        b64 = base64.b64encode((hlib.HARNESS / "s3probe.py").read_bytes()).decode()
        venv = f"{hlib.REMOTE_TMP}/crt"
        setup = (f"mkdir -p {hlib.REMOTE_TMP}; echo {b64} | base64 -d > {hlib.REMOTE_TMP}/s3probe.py; "
                 f"[ -x {venv}/bin/python ] || (python3 -m venv {venv} && {venv}/bin/pip install -q awscrt); ")
        args = (f"--op {f['op']} --bucket {bucket} --region {be.region} --size {size} --inflight {inflight} "
                f"--count {count} --throughput-gbps {int(f.get('throughput_gbps', 100))} "
                + (f"--prefix {ds['prefix']}/" if f["op"] == "get" else f"--prefix {probe_prefix}"))
        ctx["t_submit"] = time.time()
        st, out, err = be.run_on(node["id"], setup + f"{venv}/bin/python {hlib.REMOTE_TMP}/s3probe.py {args}",
                                 timeout=3000)
        ctx["t_end"] = time.time()
        line = next((ln for ln in out.splitlines() if ln.startswith("{")), None)
        if not line:
            raise RuntimeError(f"s3probe: {st} {err.strip()[-400:]}")
        probe = json.loads(line)
        art = be.collect(pdir, [], [node["id"]], [], rid, {}, {}, gridftp=False)
        rec = build_record(point, rep, meta, plan, art, {"src": [node], "dst": []}, ctx)
        rec["s3probe"] = probe
        rec["metrics"]["gbps"] = probe.get("gbps")
        rec["metrics"]["per_object_gbps"] = probe.get("per_object_gbps_median")
        rec["status"] = "ok"
    except Exception as e:  # noqa: BLE001
        log(f"  point {rid} failed: {e!r}")
        rec = {"schema": SCHEMA, "campaign": plan["campaign"], "phase": point["phase"], "kind": point["kind"],
               "uid": point["uid"], "rid": rid, "rep": rep, "factors": f, **meta, **ctx, "status": "failed",
               "error": repr(e)[:1000], "metrics": {}}
    finally:
        if f.get("op") == "put" and not keep:
            try:
                synth.delete_prefix(be.s3, bucket, probe_prefix)
            except Exception as e:  # noqa: BLE001
                log(f"  cleanup s3://{bucket}/{probe_prefix} failed: {e!r}")
    return rec


RUNNERS = {"globus": run_globus_point, "iperf3": run_iperf3_point, "s3probe": run_s3probe_point}


# ================================================================ results I/O


def results_dir(plan):
    return hlib.RESULTS / plan["campaign"]


def load_records(path):
    out = {}
    if Path(path).exists():
        for ln in Path(path).read_text().splitlines():
            if ln.strip():
                r = json.loads(ln)
                out[r["rid"]] = r          # later lines win (a re-run of a failed point)
    return out


def append_record(path, rec):
    with open(path, "a") as fh:
        fh.write(json.dumps(rec, default=str) + "\n")


# ================================================================ CLI


def cmd_plan(plan, phases, verbose=False):
    grand = {"points": 0, "hours": 0.0, "tb_moved": 0.0, "total_usd": 0.0}
    for ph in phases:
        est, order, points, curves = simulate(plan, ph)
        s = est.summary()
        rounds = {}
        for _, _, meta in order:
            rounds[meta["round"]] = rounds.get(meta["round"], 0) + 1
        coarse = [o for o in order if o[2]["round"] == "coarse"]
        print(f"\n== {ph['name']} ({ph.get('kind', 'globus')}): {ph.get('description', '')}")
        print(f"   {len(points)} distinct points in {len(curves)} curves; measurements by round: {rounds}")
        print(f"   estimate: {s['points']} measurements, {s['hours']} h, {s['tb_moved']} TB moved, "
              f"${s['transfer_usd']} transfer + ${s['instance_usd']} instances = ${s['total_usd']}")
        knees = [(c["id"], c.get("knee")) for c in curves if c.get("knee")]
        if knees:
            print("   prior-model knees: " + "; ".join(f"{c.split(':')[1]}@{k[1]}" for c, k in knees[:8])
                  + (" ..." if len(knees) > 8 else ""))
        print(f"   coarse order seed: {coarse[0][2]['order_seed'] if coarse else None}")
        show = coarse if verbose else coarse[:12]
        for pt, rep, meta in show:
            varying = {k: v for k, v in pt["factors"].items() if k in _varying(points)}
            print(f"   {meta['order_index']:4d} {pt['uid']}-r{rep} {json.dumps(varying)}")
        if not verbose and len(coarse) > 12:
            print(f"   ... {len(coarse) - 12} more (--verbose)")
        for k in grand:
            grand[k] += s[k]
    print(f"\nTOTAL: {grand['points']} measurements, {grand['hours']:.1f} h, {grand['tb_moved']:.1f} TB, "
          f"${grand['total_usd']:.0f} (prior model; refinement and knees depend on the data)")


def _varying(points):
    vals = {}
    for p in points.values():
        for k, v in p["factors"].items():
            vals.setdefault(k, set()).add(hlib.canonical(v))
    return {k for k, v in vals.items() if len(v) > 1} or {"dataset"}


def cmd_run(plan, phase, dry_run, max_points, keep, yes=False, log=print):
    if dry_run:
        cmd_plan(plan, [phase], verbose=True)
        return 0
    plan = phase_plan(plan, phase)
    out = results_dir(plan)
    out.mkdir(parents=True, exist_ok=True)
    (out / "plans").mkdir(exist_ok=True)
    (out / "plans" / Path(plan["_path"]).name).write_text(Path(plan["_path"]).read_text())
    pts_path = out / "points.jsonl"
    done = {k: v for k, v in load_records(pts_path).items() if v.get("phase") == phase["name"]}
    env = hlib.load_env()
    est, *_ = simulate(plan, phase)
    s = est.summary()
    log(f"{phase['name']}: about {s['points']} measurements, {s['hours']} h, {s['tb_moved']} TB, "
        f"${s['transfer_usd']} transfer + ${s['instance_usd']} instances = ${s['total_usd']} (prior model); "
        f"{len(done)} already recorded")
    if not yes and input("proceed? [y/N] ").strip().lower() != "y":
        return 1
    be = LiveBackend(env, log)
    runner = RUNNERS[phase.get("kind", "globus")]
    sched = open(out / f"schedule-{phase['name']}.jsonl", "a")

    def measure(point, rep, meta):
        rid = f"{point['uid']}-r{rep}"
        log(f"[{phase['name']}] {meta['round']} #{meta['order_index'] + 1}/{meta['round_size']} {rid} "
            f"{json.dumps({k: v for k, v in point['factors'].items() if v is not None})}")
        sched.write(json.dumps({"rid": rid, "t": time.time(), **meta}) + "\n")
        sched.flush()
        rec = runner(be, plan, point, rep, meta, out, keep, log)
        append_record(pts_path, rec)
        mt = rec.get("metrics") or {}
        log(f"  -> {rec['status']} steady={mt.get('steady_gbps')} wall={mt.get('wall_gbps')} "
            f"neg={rec.get('negotiated')}")
        return rec

    be.pause_demand()
    try:
        phase_loop(plan, phase, measure, done, max_points, log)
    finally:
        try:
            be.ensure_netem(0, 0, "src")
        finally:
            be.resume_demand()
            sched.close()
    return 0


def cmd_reset(itype, log=print):
    """Put both pools back on one instance type at zero DTNs (after the instance-size arm)."""
    be = LiveBackend(hlib.load_env(), log)
    for dep in ("src", "dst"):
        log(f"{dep}: {be.ensure_instance_type(dep, itype)}")
        be.set_desired(dep, 0)
    log("desired-dtns = 0 on both; stopped slots now use " + itype)
    return 0


def cmd_install(log=print):
    env = hlib.load_env()
    ssm = hlib.SSM(env["REGION"])
    ssm.put_file(env["SRC_CONTROL"], f"{hlib.BENCH_DIR}/bench_transfer.py",
                 (hlib.HARNESS / "bench_transfer.py").read_bytes(), "0755")
    st, out, err = ssm.run(env["SRC_CONTROL"], f"{hlib.BENCH_DIR}/venv/bin/python -c "
                                               "'import globus_sdk; print(globus_sdk.__version__)'")
    log(f"bench_transfer.py installed; globus_sdk {out.strip() or err.strip()[-200:]}")
    return 0 if st == "Success" else 1


def cmd_datasets(plan, phases):
    refs = set()
    for ph in phases:
        pts, _ = expand_phase(plan, ph)
        for p in pts.values():
            d = p["factors"].get("dataset")
            if d and d not in plan["datasets"]:
                refs.add(d)
    for r in sorted(refs):
        info = resolve_dataset(plan, r)
        print(f"{r}\t{info['prefix']}\t{info['files']}\t{hlib.fmt_bytes(info['bytes'])}")


def cmd_status(plan):
    recs = load_records(results_dir(plan) / "points.jsonl")
    by = {}
    for r in recs.values():
        by.setdefault(r["phase"], {}).setdefault(r["status"], 0)
        by[r["phase"]][r["status"]] += 1
    for ph in plan["phases"]:
        print(f"{ph['name']:10} {by.get(ph['name'], {})}")


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for n in ("plan", "run", "datasets", "status"):
        p = sub.add_parser(n)
        p.add_argument("plan")
        p.add_argument("--phase", action="append")
        if n == "plan":
            p.add_argument("--verbose", action="store_true")
        if n == "run":
            p.add_argument("--dry-run", action="store_true")
            p.add_argument("--max-points", type=int)
            p.add_argument("--keep-data", action="store_true", help="don't delete what transfers wrote")
            p.add_argument("--yes", action="store_true", help="skip the cost confirmation")
    sub.add_parser("install")
    r = sub.add_parser("reset", help="after the instance-size arm: stop slots and restore their type")
    r.add_argument("--type", default="c8in.2xlarge")
    a = ap.parse_args(argv)
    if a.cmd == "install":
        return cmd_install()
    if a.cmd == "reset":
        return cmd_reset(a.type)
    plan = load_plan(a.plan)
    phases = [get_phase(plan, n) for n in a.phase] if a.phase else plan["phases"]
    if a.cmd == "plan":
        cmd_plan(plan, phases, a.verbose)
    elif a.cmd == "datasets":
        cmd_datasets(plan, phases)
    elif a.cmd == "status":
        cmd_status(plan)
    else:
        if not a.phase or len(a.phase) != 1:
            sys.exit("run takes exactly one --phase")
        return cmd_run(plan, phases[0], a.dry_run, a.max_points, a.keep_data, a.yes)
    return 0


if __name__ == "__main__":
    sys.exit(main())
