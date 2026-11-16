"""Test helpers: put the harness on sys.path; synthetic series and records."""
import random
import sys
from pathlib import Path

HARNESS = Path(__file__).resolve().parents[1]
if str(HARNESS) not in sys.path:
    sys.path.insert(0, str(HARNESS))


def ramp_plateau(n=150, ramp_s=30, level=9.2, cv=0.03, seed=1, t0=1_700_000_000.0):
    """1 s samples: linear ramp to `level` over ramp_s, then noisy plateau."""
    rng = random.Random(seed)
    ts = [t0 + i for i in range(n)]
    ys = [(level * min(i / ramp_s, 1.0)) * (1 + rng.gauss(0, cv)) for i in range(n)]
    return ts, ys


def cumulative(ts, rates_gbps, start=0):
    """Cumulative byte counters for a rate series (Gbps per 1 s step)."""
    out, acc = [], start
    for t, r in zip(ts, rates_gbps):
        acc += r * 1e9 / 8
        out.append((t, int(acc)))
    return out
