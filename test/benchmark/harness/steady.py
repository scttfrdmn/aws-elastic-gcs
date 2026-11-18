"""Steady-state detection from 1 s NIC samples (#151 measurement protocol).

Definition used throughout the study:

1. Per-node cumulative byte counters are turned into a common 1 s grid and
   summed across DTNs (`aggregate_rates`).
2. At each grid time tau, the trailing window (tau - W, tau] (W = 10 s) is the
   rolling W-second mean's support. The level is "stable at tau" when the OLS
   slope of the raw samples in that window is not significantly different
   from 0 (two-sided p > alpha, alpha = 0.05). Testing the raw samples rather
   than the overlapping rolling means keeps the test's independence
   assumption closer to true.
3. Steady state begins at the end of the first run of consecutive stable
   instants lasting `hold_s` (10 s). Two guards reject stable windows that
   are really idle: the rolling mean must exceed `min_level` (absolute; a
   Globus task lists files for tens of seconds before data flows) and be at
   least `min_frac` of the running maximum (a dip between files).
4. The measurement window is the `measure_s` (45 s) after onset. It reports
   mean, sd, CV and a 95% CI corrected for lag-1 autocorrelation, plus the
   window's own trend p-value as a stationarity check.

All functions are pure; `SteadyTracker` wraps them for online use while a
transfer runs.
"""
import math

from hstats import mean_ci, ols

__all__ = ["aggregate_rates", "detect_steady", "SteadyTracker", "window_stats"]


def _interp(series, t):
    """Linear interpolation of cumulative (t, v) samples at t; None outside the range."""
    if not series or t < series[0][0] or t > series[-1][0]:
        return None
    lo, hi = 0, len(series) - 1
    while hi - lo > 1:
        mid = (lo + hi) // 2
        if series[mid][0] <= t:
            lo = mid
        else:
            hi = mid
    (t0, v0), (t1, v1) = series[lo], series[hi]
    if t1 == t0:
        return v1
    return v0 + (v1 - v0) * (t - t0) / (t1 - t0)


def _monotone(series):
    """Sort by time, drop duplicate times, and undo counter resets (re-base after a drop)."""
    out, base, last = [], 0.0, None
    for t, v in sorted(series):
        if out and t == out[-1][0]:
            continue
        if last is not None and v < last:
            base += last
        last = v
        out.append((t, v + base))
    return out


def aggregate_rates(by_node, step=1.0, scale=8.0):
    """Sum per-node rates on a common grid.

    by_node: {node: [(epoch_s, cumulative_bytes), ...]}. Returns (grid_t, rates)
    where rates[i] is the summed rate over (grid_t[i] - step, grid_t[i]] in
    units of `scale` x bytes/s (default bits/s). Only the span covered by
    every node is used, so a node that starts late can't fake a ramp.
    """
    nodes = {k: _monotone(v) for k, v in by_node.items() if len(v) >= 2}
    if not nodes:
        return [], []
    start = max(s[0][0] for s in nodes.values())
    end = min(s[-1][0] for s in nodes.values())
    t = math.ceil(start)
    grid, rates = [], []
    prev = {k: _interp(s, t) for k, s in nodes.items()}
    t += step
    while t <= end + 1e-9:
        cur = {k: _interp(s, t) for k, s in nodes.items()}
        rates.append(sum(scale * (cur[k] - prev[k]) / step for k in nodes))
        grid.append(t)
        prev = cur
        t += step
    return grid, rates


def window_stats(ts, ys):
    s = mean_ci(ys, autocorr=True)
    trend_p = ols(ts, ys)["p"] if len(ys) >= 3 and len(set(ts)) > 1 else None
    return {"n": s["n"], "mean": s["mean"], "sd": s["sd"], "cv": s["cv"], "ci95": s["ci"],
            "n_eff": s["n_eff"], "rho1": s.get("rho1"), "trend_p": trend_p}


def detect_steady(ts, ys, window_s=10, hold_s=10, measure_s=45, alpha=0.05, min_frac=0.5, min_level=0.0):
    """Find steady state in a 1 s rate series.

    Returns a dict: found (bool), complete (bool: a full measurement window is
    available), onset_t (absolute time steady state begins), window [t0, t1],
    stats (window_stats on the window), and stable_mask for diagnostics.
    """
    n = len(ys)
    res = {"found": False, "complete": False, "onset_t": None, "window": None, "stats": None,
           "params": {"window_s": window_s, "hold_s": hold_s, "measure_s": measure_s, "alpha": alpha,
                      "min_frac": min_frac, "min_level": min_level}}
    if n < window_s + hold_s:
        return res
    w = int(round(window_s))
    stable = [False] * n
    running_max = 0.0
    for i in range(n):
        running_max = max(running_max, ys[i])
        if i + 1 < w:
            continue
        seg_t, seg_y = ts[i + 1 - w:i + 1], ys[i + 1 - w:i + 1]
        mean = sum(seg_y) / w
        if mean <= min_level:            # idle (e.g. task listing before data flows) is not steady state
            continue
        if min_frac and running_max > 0 and mean < min_frac * running_max:
            continue
        if len(set(seg_y)) == 1:
            stable[i] = True
            continue
        stable[i] = ols(seg_t, seg_y)["p"] > alpha
    hold = int(round(hold_s))
    run = 0
    onset_i = None
    for i, s in enumerate(stable):
        run = run + 1 if s else 0
        if run >= hold:
            onset_i = i
            break
    res["stable_mask"] = stable
    if onset_i is None:
        return res
    t_on = ts[onset_i]
    t_end = t_on + measure_s
    idx = [j for j in range(onset_i + 1, n) if ts[j] <= t_end + 1e-9]
    res.update(found=True, onset_t=t_on, window=[t_on, t_end])
    if idx:
        res["stats"] = window_stats([ts[j] for j in idx], [ys[j] for j in idx])
    res["complete"] = bool(idx) and ts[idx[-1]] >= t_end - 1e-9
    return res


class SteadyTracker:
    """Online wrapper: feed per-node cumulative samples as they arrive."""

    def __init__(self, **params):
        self.params = params
        self.samples = {}

    def add(self, node, t, cum_bytes):
        s = self.samples.setdefault(node, {})
        s[float(t)] = float(cum_bytes)

    def series(self):
        return aggregate_rates({k: sorted(v.items()) for k, v in self.samples.items()})

    def result(self):
        ts, ys = self.series()
        r = detect_steady(ts, ys, **self.params)
        r.pop("stable_mask", None)
        return r
