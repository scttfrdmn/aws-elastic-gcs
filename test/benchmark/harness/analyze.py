#!/usr/bin/env python3
"""Analysis for the envelope study (#151): tables, model fits, figures, attribution.

Reads results/<campaign>/points.jsonl and writes results/<campaign>/analysis/:
  points_agg.csv      one row per point: factors, n, mean, sd, CV, 95% CI
  tables.md           one table per curve (sweep x fixed combination), with CIs
  fits.json           fitted models with held-out validation
  attribution.md      bottleneck-attribution template, one row per knee
  fig-*.png/.pdf/.csv every figure, with the exact data it plots

Models (all fitted in closed form, stdlib only):
  per-file rate     r(s) = s / (t0 + s / b), i.e. time per file = t0 + s / b  (OLS)
  concurrency       T(c) = min(c * r, N_cap)                                  (breakpoint search)
  scalability (USL) T(n) = lambda n / (1 + sigma (n - 1) + kappa n (n - 1))
                    fitted as n / T = a + b (n - 1) + c n (n - 1)             (linear LS)
Validation: leave-one-out over the curve's points, and a seeded 20% hold-out.

usage: analyze.py RESULTS_DIR [--metric steady_gbps] [--no-figures] [--seed 151]
"""
import argparse
import csv
import json
import math
import random
import sys
from pathlib import Path

from hstats import lstsq, mean_ci

LAYER_RELIEF = {
    "L1-nic": "larger NIC (next instance size) or more DTNs; check the knee moves with NIC",
    "L1-cpu": "more vCPUs at equal NIC (larger size), or encrypt_data/verify_checksum off",
    "L2": "more concurrent tasks k (each gets its own 128-file cap)",
    "L3": "more DTNs at equal total cores; encrypt_data off; compare per-process CPU",
    "L4": "compare with P0 single-object rate at the same size; larger objects; connector change",
    "L5": "spread destination prefixes (dst_fanout); fewer requests per file",
}


# ================================================================ loading and aggregation


def load(results_dir):
    path = Path(results_dir) / "points.jsonl"
    recs = {}
    for ln in path.read_text().splitlines():
        if ln.strip():
            r = json.loads(ln)
            recs[r["rid"]] = r
    return list(recs.values())


def usable(r, metric):
    return r.get("status") in ("ok", "no_steady") and (r.get("metrics") or {}).get(metric) is not None


def aggregate(recs, metric="steady_gbps"):
    """{uid: row} with the repetition mean and a t-based 95% CI across repetitions.

    With one repetition the CI is the within-run (autocorrelation-corrected)
    interval of the measurement window, flagged ci_kind = "within"."""
    by = {}
    for r in recs:
        if usable(r, metric):
            by.setdefault(r["uid"], []).append(r)
    rows = {}
    for uid, rs in by.items():
        vals = [r["metrics"][metric] for r in rs]
        s = mean_ci(vals)
        row = {"uid": uid, "phase": rs[0]["phase"], "factors": rs[0]["factors"], "curves": rs[0].get("curves", []),
               "metric": metric, "n": s["n"], "mean": s["mean"], "sd": s["sd"], "cv": s["cv"], "ci95": s["ci"],
               "ci_kind": "reps", "t_start": [r.get("t_start") for r in rs], "rids": [r["rid"] for r in rs],
               "rounds": sorted({r.get("round") for r in rs if r.get("round")})}
        if s["n"] == 1 and metric == "steady_gbps":
            ci = (rs[0].get("metrics") or {}).get("steady_ci95")
            if ci:
                row.update(ci95=ci, ci_kind="within")
        rows[uid] = row
    return rows


def curves_from(rows):
    """Reconstruct curves: {curve_id: {factor, fixed key, points [(x, row)]}} from records."""
    out = {}
    for row in rows.values():
        for cid in row["curves"]:
            phase, factor = cid.split(":")[:2]
            c = out.setdefault(cid, {"id": cid, "phase": phase, "factor": factor, "points": []})
            x = row["factors"].get(factor)
            c["points"].append((x if x is not None else _label(row["factors"]), row))
    for c in out.values():
        c["numeric"] = all(isinstance(x, (int, float)) and not isinstance(x, bool) for x, _ in c["points"])
        if c["numeric"]:
            c["points"].sort(key=lambda p: p[0])
        c["fixed"] = _fixed(c)
        c["coarse"] = [x for x, r in c["points"] if "coarse" in r.get("rounds", [])] or None
    return out


def _label(f):
    keys = ("instance_type_src", "n_src", "dataset", "op", "procs")
    return "/".join(str(f[k]) for k in keys if k in f and f[k] is not None)


def _fixed(curve):
    """Factors constant across the curve (what the curve holds fixed)."""
    pts = [r["factors"] for _, r in curve["points"]]
    if not pts:
        return {}
    keys = set(pts[0])
    return {k: pts[0][k] for k in sorted(keys) if k != curve["factor"] and all(p.get(k) == pts[0][k] for p in pts)}


# ================================================================ models


def fit_rs(sizes_bytes, rates_gbps):
    """r(s) = s / (t0 + s / b): least squares of time per file on size (Gb).

    Weighted by 1/t (i.e. relative error), so a 16 GiB file's ~200 s doesn't
    swamp a 1 MiB file's ~0.1 s and leave t0 unconstrained; t0 is constrained
    to >= 0 (if the unconstrained fit is negative, t0 = 0 and b is refit).
    Returns t0 (s), b (Gbps), r2 (on time, unweighted).
    """
    xs = [8 * s / 1e9 for s in sizes_bytes]
    ts = [x / r for x, r in zip(xs, rates_gbps)]
    w = [1.0 / t for t in ts]
    beta = lstsq([[wi, wi * x] for wi, x in zip(w, xs)], [wi * t for wi, t in zip(w, ts)])
    t0, inv_b = beta
    if t0 < 0:
        t0 = 0.0
        inv_b = sum((wi * x) * (wi * t) for wi, x, t in zip(w, xs, ts)) / sum((wi * x) ** 2 for wi, x in zip(w, xs))
    pred = [t0 + x * inv_b for x in xs]
    mt = sum(ts) / len(ts)
    sst = sum((t - mt) ** 2 for t in ts)
    r2 = 1 - sum((t - p) ** 2 for t, p in zip(ts, pred)) / sst if sst else 1.0
    return {"model": "r(s)=s/(t0+s/b)", "t0_s": t0, "b_gbps": (1 / inv_b) if inv_b > 0 else math.inf, "r2": r2}


def predict_rs(fit, size_bytes):
    x = 8 * size_bytes / 1e9
    return x / (fit["t0_s"] + x / fit["b_gbps"])


def fit_min(cs, ys):
    """T(c) = min(c r, cap): try every breakpoint; LS slope through the origin below it, mean above."""
    pts = sorted(zip(cs, ys))
    best = None
    for j in range(len(pts), 0, -1):            # ties go to the simpler model (no or a later cap)
        lo, hi = pts[:j], pts[j:]
        r = sum(c * y for c, y in lo) / sum(c * c for c, _ in lo)
        cap = sum(y for _, y in hi) / len(hi) if hi else math.inf
        sse = sum((y - min(c * r, cap)) ** 2 for c, y in pts)
        if best is None or sse < best[0] - 1e-12:
            best = (sse, r, cap)
    sse, r, cap = best
    return {"model": "T(c)=min(c*r,cap)", "r_gbps": r, "cap_gbps": None if math.isinf(cap) else cap,
            "c_star": None if math.isinf(cap) else cap / r, "sse": sse}


def predict_min(fit, c):
    cap = fit["cap_gbps"] if fit["cap_gbps"] is not None else math.inf
    return min(c * fit["r_gbps"], cap)


def fit_usl(ns, ys):
    """Universal Scalability Law via n/T = a + b(n-1) + c n(n-1)."""
    rows = [[1.0, n - 1.0, n * (n - 1.0)] for n in ns]
    a, b, c = lstsq(rows, [n / y for n, y in zip(ns, ys)])
    lam, sigma, kappa = 1 / a, b / a, c / a
    peak = math.sqrt((1 - sigma) / kappa) if kappa > 0 and sigma < 1 else None
    return {"model": "USL", "lambda": lam, "sigma": sigma, "kappa": kappa, "n_peak": peak}


def predict_usl(fit, n):
    return fit["lambda"] * n / (1 + fit["sigma"] * (n - 1) + fit["kappa"] * n * (n - 1))


MODELS = {"rs": (fit_rs, predict_rs, 2), "min": (fit_min, predict_min, 2), "usl": (fit_usl, predict_usl, 3)}


def validate(kind, xs, ys, holdout_frac=0.2, seed=151):
    """Leave-one-out and a seeded hold-out: MAPE and RMSE of held-out predictions."""
    fit, pred, k = MODELS[kind]
    n = len(xs)
    out = {"n": n}
    if n < k + 2:
        out["note"] = "too few points to validate"
        return out
    errs = []
    for i in range(n):
        xi, yi = xs[:i] + xs[i + 1:], ys[:i] + ys[i + 1:]
        try:
            f = fit(xi, yi)
            errs.append((pred(f, xs[i]), ys[i]))
        except (ValueError, ZeroDivisionError):
            continue
    out["loo"] = _err(errs)
    rng = random.Random(f"{seed}:{kind}:{n}")
    m = max(1, round(holdout_frac * n))
    test = set(rng.sample(range(n), m))
    tr = [i for i in range(n) if i not in test]
    if len(tr) >= k + 1:
        try:
            f = fit([xs[i] for i in tr], [ys[i] for i in tr])
            out["holdout"] = {**_err([(pred(f, xs[i]), ys[i]) for i in sorted(test)]),
                              "test_x": [xs[i] for i in sorted(test)]}
        except (ValueError, ZeroDivisionError):
            pass
    return out


def _err(pairs):
    pairs = [(p, y) for p, y in pairs if y]
    if not pairs:
        return None
    mape = 100 * sum(abs(p - y) / abs(y) for p, y in pairs) / len(pairs)
    rmse = math.sqrt(sum((p - y) ** 2 for p, y in pairs) / len(pairs))
    return {"mape_pct": round(mape, 2), "rmse": round(rmse, 4), "points": len(pairs)}


# ================================================================ knees and attribution


def knee(xs, ys, coarse=None, thr=0.10):
    """The run-time rule (study.knee_index): first step whose gain, scaled to its coarse step, is < thr."""
    import study  # noqa: PLC0415
    return study.knee_index(xs, ys, coarse or xs, thr)


def evidence(recs_by_uid, uid, s3_profiles=None):
    """Utilization, saturation and errors at one point (USE method), from its records."""
    rs = recs_by_uid.get(uid, [])
    if not rs:
        return {}
    r = rs[0]
    sender = r.get("sender", "src")
    dtns = [n for n in r.get("nodes", []) if n.get("role") == "dtn" and n.get("side") == sender and "tx_gbps" in n]
    info = r.get("instance_info") or {}
    util = []
    for n in dtns:
        base = (info.get(n.get("type")) or {}).get("baseline_gbps") or []
        b = sum(x for x in base if x) or None
        if b:
            util.append(n["tx_gbps"] / b)
    ev = {"nic_util_max": round(max(util), 3) if util else None,
          "cpu_pct_max": max((n.get("cpu_pct") or 0) for n in dtns) if dtns else None,
          "core_busy_max_pct": max((n.get("core_busy_max_pct") or 0) for n in dtns) if dtns else None,
          "gridftp_proc_cpu_max_pct": max((n.get("gridftp_proc_cpu_max_pct") or 0) for n in dtns) if dtns else None,
          "retrans_per_s": sum((n.get("retrans_per_s") or 0) for n in dtns) if dtns else None,
          "negotiated": r.get("negotiated"),
          "files_inflight": ((r.get("gridftp") or {}).get("send") or {}).get("inflight_mean"),
          "per_file_gbps_median": ((r.get("gridftp") or {}).get("send") or {}).get("rate_gbps_median"),
          "faults": sum((t.get("faults") or 0) for t in r.get("tasks", []))}
    prof = (s3_profiles or {}).get(r["rid"]) or {}
    ev["s3_503"] = sum((p or {}).get("throttled_503", 0) for p in prof.values()) if prof else None
    return ev


def suggest_layer(ev, factors):
    if ev.get("nic_util_max") is not None and ev["nic_util_max"] >= 0.85:
        return "L1-nic"
    if (ev.get("cpu_pct_max") or 0) >= 85:
        return "L1-cpu"
    if (ev.get("gridftp_proc_cpu_max_pct") or 0) >= 90:
        return "L3"
    if (ev.get("s3_503") or 0) > 0:
        return "L5"
    if int(factors.get("c") or 0) >= 128:
        return "L2"
    return "L4"


def attribution(curves, recs, metric, s3_profiles=None):
    by = {}
    for r in recs:
        by.setdefault(r["uid"], []).append(r)
    rows = []
    for c in curves.values():
        if not c["numeric"] or len(c["points"]) < 3:
            continue
        xs = [x for x, _ in c["points"]]
        ys = [row["mean"] for _, row in c["points"]]
        i = knee(xs, ys, c.get("coarse"))
        if i is None:
            continue
        row = c["points"][i][1]
        ev = evidence(by, row["uid"], s3_profiles)
        layer = suggest_layer(ev, row["factors"])
        rows.append({"curve": c["id"], "factor": c["factor"], "fixed": c["fixed"], "knee_x": xs[i],
                     "value": ys[i], "ci95": row["ci95"], "evidence": ev, "suggested_layer": layer,
                     "relief": LAYER_RELIEF[layer]})
    return rows


# ================================================================ output


def _fmt(v, d=3):
    if v is None:
        return "-"
    if isinstance(v, float):
        return f"{v:.{d}f}"
    return str(v)


def write_tables(out, rows, curves, metric):
    with open(out / "points_agg.csv", "w", newline="") as fh:
        keys = sorted({k for r in rows.values() for k in r["factors"]})
        w = csv.writer(fh)
        w.writerow(["uid", "phase", *keys, "metric", "n", "mean", "sd", "cv", "ci95_lo", "ci95_hi", "ci_kind"])
        for r in sorted(rows.values(), key=lambda r: (r["phase"], r["uid"])):
            w.writerow([r["uid"], r["phase"], *[r["factors"].get(k) for k in keys], metric, r["n"], r["mean"],
                        r["sd"], r["cv"], *(r["ci95"] or [None, None]), r["ci_kind"]])
    lines = [f"# Envelope study tables ({metric})", "",
             "Mean over repetitions with a 95% t-interval (ci_kind=reps); single runs show the "
             "within-run interval of the measurement window (within).", ""]
    for cid, c in sorted(curves.items()):
        lines += [f"## {cid}", "", f"Factor `{c['factor']}`; fixed: `{json.dumps(c['fixed'])}`", "",
                  f"| {c['factor']} | n | mean | 95% CI | CV | CI kind |", "|---|---|---|---|---|---|"]
        for x, r in c["points"]:
            ci = r["ci95"] or [None, None]
            lines.append(f"| {x} | {r['n']} | {_fmt(r['mean'])} | [{_fmt(ci[0])}, {_fmt(ci[1])}] | "
                         f"{_fmt(r['cv'])} | {r['ci_kind']} |")
        lines.append("")
    (out / "tables.md").write_text("\n".join(lines))


def write_attribution(out, rows):
    lines = ["# Bottleneck attribution", "",
             "One row per knee. `suggested` is a rule-of-thumb from utilization at the knee; a layer is "
             "credited only after its relief experiment moves the knee (#151 section 4). Fill in the last "
             "two columns from the relief runs.", "",
             "| Curve | Knee | Value | 95% CI | NIC util | CPU % | max GridFTP proc % | 503s | files in flight "
             "| negotiated | Suggested | Relief experiment | Relief result | Credited layer |",
             "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        ev, ci = r["evidence"], r["ci95"] or [None, None]
        lines.append(f"| {r['curve']} | {r['factor']}={r['knee_x']} | {_fmt(r['value'])} | "
                     f"[{_fmt(ci[0])}, {_fmt(ci[1])}] | {_fmt(ev.get('nic_util_max'))} | "
                     f"{_fmt(ev.get('cpu_pct_max'), 1)} | {_fmt(ev.get('gridftp_proc_cpu_max_pct'), 1)} | "
                     f"{_fmt(ev.get('s3_503'))} | {_fmt(ev.get('files_inflight'))} | "
                     f"{json.dumps(ev.get('negotiated'))} | {r['suggested_layer']} | {r['relief']} |  |  |")
    if not rows:
        lines.append("| (no knees yet) | | | | | | | | | | | | | |")
    (out / "attribution.md").write_text("\n".join(lines) + "\n")


def fits(curves, recs, seed):
    """Fit per curve by factor: c -> min model; n, k -> USL. Plus r(s) from per-file GridFTP rates."""
    out = {"curves": {}, "rs": None}
    for cid, c in curves.items():
        if not c["numeric"] or len(c["points"]) < 3:
            continue
        xs = [float(x) for x, _ in c["points"]]
        ys = [row["mean"] for _, row in c["points"]]
        if any(y is None or y <= 0 for y in ys):
            continue
        kind = {"c": "min", "n": "usl", "k": "usl", "inflight": "usl"}.get(c["factor"])
        if not kind:
            continue
        try:
            f = MODELS[kind][0](xs, ys)
        except (ValueError, ZeroDivisionError) as e:
            out["curves"][cid] = {"error": str(e)}
            continue
        out["curves"][cid] = {"kind": kind, "factor": c["factor"], "fixed": c["fixed"], "fit": f,
                              "validation": validate(kind, xs, ys, seed=seed), "x": xs, "y": ys}
    # r(s): one point per (route, size) from the c = 1 runs' per-file medians, else steady / c.
    pts = {}
    for r in recs:
        if r.get("phase") != "P1" or r.get("status") != "ok":
            continue
        f = r["factors"]
        # The slower side binds: for writes the sender only reads local disk.
        gg = [((r.get("gridftp") or {}).get(side) or {}).get("rate_gbps_median") for side in ("send", "recv")]
        gg = [x for x in gg if x]
        g = min(gg) if gg else None
        if g is None and f.get("c") == 1:
            g = (r.get("metrics") or {}).get("steady_gbps")
        size = r.get("dataset_mean_size") or _size_of(f.get("dataset"))
        if g and size:
            pts.setdefault(f.get("route"), {}).setdefault(size, []).append(g)
    rs = {}
    for route, by in pts.items():
        sizes = sorted(by)
        rates = [sorted(by[s])[len(by[s]) // 2] for s in sizes]
        if len(sizes) >= 2:
            f = fit_rs(sizes, rates)
            rs[route] = {"fit": f, "validation": validate("rs", sizes, rates, seed=seed), "sizes": sizes,
                         "rates_gbps": rates}
    out["rs"] = rs
    return out


def _size_of(ref):
    if not ref:
        return None
    try:
        import study  # noqa: PLC0415
        return study.synth_spec(ref)["size"].get("value")
    except (OSError, KeyError, ValueError):
        return None


def figures(out, curves, fitres, metric):
    try:
        import matplotlib  # noqa: PLC0415
        matplotlib.use("Agg")
        import matplotlib.pyplot as plt  # noqa: PLC0415
        import matplotlib.ticker  # noqa: F401,PLC0415
    except ImportError:
        print("matplotlib not installed: skipping figures (tables and fits are written)", file=sys.stderr)
        return []
    ink, muted, grid = "#0b0b0b", "#52514e", "#e4e3df"
    series = ["#2a78d6", "#eb6834", "#1baf7a"]          # categorical slots 1-3 (validated all-pairs)
    markers = ["o", "s", "^"]
    plt.rcParams.update({"font.size": 9, "axes.edgecolor": muted, "axes.labelcolor": ink, "xtick.color": muted,
                         "ytick.color": muted, "axes.spines.top": False, "axes.spines.right": False,
                         "axes.grid": True, "grid.color": grid, "grid.linewidth": 0.6, "lines.linewidth": 2,
                         "legend.frameon": False})
    made = []
    groups = {}
    for cid, c in curves.items():
        if c["numeric"] and len(c["points"]) >= 2:
            groups.setdefault((c["phase"], c["factor"]), []).append(c)
    for (phase, factor), cs in sorted(groups.items()):
        for start in range(0, len(cs), 3):          # at most three series per figure
            chunk = cs[start:start + 3]
            fig, ax = plt.subplots(figsize=(4.8, 3.2), dpi=150)
            rows_csv = []
            for i, c in enumerate(chunk):
                xs = [x for x, _ in c["points"]]
                ys = [r["mean"] for _, r in c["points"]]
                lo = [r["mean"] - (r["ci95"][0] if r["ci95"] and r["ci95"][0] is not None else r["mean"])
                      for _, r in c["points"]]
                hi = [(r["ci95"][1] if r["ci95"] and r["ci95"][1] is not None else r["mean"]) - r["mean"]
                      for _, r in c["points"]]
                label = ", ".join(f"{k}={v}" for k, v in c["fixed"].items() if k in _vary_keys(chunk)) or c["id"]
                ax.errorbar(xs, ys, yerr=[lo, hi], color=series[i], marker=markers[i], markersize=5,
                            capsize=2, elinewidth=1, label=label)
                fr = (fitres["curves"].get(c["id"]) or {}).get("fit")
                if fr and "error" not in fr:
                    kind = fitres["curves"][c["id"]]["kind"]
                    grid_x = _grid(min(xs), max(xs))
                    ax.plot(grid_x, [MODELS[kind][1](fr, x) for x in grid_x], color=series[i], linewidth=1,
                            linestyle="--", alpha=0.8)
                for x, (_, r) in zip(xs, c["points"]):
                    rows_csv.append([c["id"], label, x, r["n"], r["mean"], *(r["ci95"] or [None, None])])
            if max(x for c in chunk for x, _ in c["points"]) / max(min(x for c in chunk for x, _ in c["points"]), 1e-9) >= 8:
                ax.set_xscale("log", base=2)
                ax.xaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: f"{v:g}"))
            ax.set_xlabel(factor)
            ax.set_ylabel(metric.replace("_", " "))
            ax.set_ylim(bottom=0)
            ax.set_title(f"{phase}: {metric.replace('_', ' ')} vs {factor} (95% CI; dashed: fit)", fontsize=9,
                         color=ink, loc="left")
            if len(chunk) > 1:
                ax.legend(fontsize=7)
            name = f"fig-{phase}-{factor}-{start // 3}"
            fig.tight_layout()
            for ext in ("png", "pdf"):
                fig.savefig(out / f"{name}.{ext}")
            plt.close(fig)
            with open(out / f"{name}.csv", "w", newline="") as fh:
                w = csv.writer(fh)
                w.writerow(["curve", "series", factor, "n", "mean", "ci95_lo", "ci95_hi"])
                w.writerows(rows_csv)
            made.append(name)
    rs = fitres.get("rs") or {}
    if rs:
        fig, ax = plt.subplots(figsize=(4.8, 3.2), dpi=150)
        rows_csv = []
        for i, (route, d) in enumerate(sorted(rs.items())[:3]):
            sz = [s / 2 ** 20 for s in d["sizes"]]
            ax.plot(sz, d["rates_gbps"], color=series[i], marker=markers[i], linestyle="none", markersize=5,
                    label=f"{route} (t0={d['fit']['t0_s']:.2f} s, b={d['fit']['b_gbps']:.2f} Gbps)")
            gx = _grid(min(d["sizes"]), max(d["sizes"]))
            ax.plot([g / 2 ** 20 for g in gx], [predict_rs(d["fit"], g) for g in gx], color=series[i], linewidth=1,
                    linestyle="--")
            rows_csv += [[route, s, r] for s, r in zip(d["sizes"], d["rates_gbps"])]
        ax.set_xscale("log", base=2)
        ax.xaxis.set_major_formatter(matplotlib.ticker.FuncFormatter(lambda v, _: f"{v:g}"))
        ax.set_xlabel("object size (MiB)")
        ax.set_ylabel("per-file rate (Gbps)")
        ax.set_ylim(bottom=0)
        ax.set_title("P1: per-file rate r(s), median per size; dashed: s/(t0+s/b)", fontsize=9, loc="left")
        ax.legend(fontsize=7)
        fig.tight_layout()
        for ext in ("png", "pdf"):
            fig.savefig(out / f"fig-P1-rs.{ext}")
        plt.close(fig)
        with open(out / "fig-P1-rs.csv", "w", newline="") as fh:
            w = csv.writer(fh)
            w.writerow(["route", "size_bytes", "rate_gbps"])
            w.writerows(rows_csv)
        made.append("fig-P1-rs")
    return made


def _vary_keys(chunk):
    keys = set()
    for c in chunk:
        keys |= set(c["fixed"])
    return {k for k in keys if len({json.dumps(c["fixed"].get(k)) for c in chunk}) > 1}


def _grid(lo, hi, n=60):
    if lo <= 0:
        return [lo + (hi - lo) * i / (n - 1) for i in range(n)]
    return [lo * (hi / lo) ** (i / (n - 1)) for i in range(n)]


def load_s3_profiles(results_dir):
    out = {}
    for p in (Path(results_dir) / "raw").glob("*/s3_profile.json"):
        out[p.parent.name] = json.loads(p.read_text())
    return out


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("results")
    ap.add_argument("--metric", default="steady_gbps")
    ap.add_argument("--phase", action="append")
    ap.add_argument("--no-figures", action="store_true")
    ap.add_argument("--seed", type=int, default=151)
    a = ap.parse_args(argv)
    recs = load(a.results)
    if a.phase:
        recs = [r for r in recs if r["phase"] in a.phase]
    out = Path(a.results) / "analysis"
    out.mkdir(parents=True, exist_ok=True)
    rows = aggregate(recs, a.metric)
    curves = curves_from(rows)
    write_tables(out, rows, curves, a.metric)
    fr = fits(curves, recs, a.seed)
    (out / "fits.json").write_text(json.dumps(fr, indent=1, default=str))
    write_attribution(out, attribution(curves, recs, a.metric, load_s3_profiles(a.results)))
    made = [] if a.no_figures else figures(out, curves, fr, a.metric)
    print(f"{len(recs)} records, {len(rows)} points, {len(curves)} curves, {len(made)} figures -> {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
