"""Small statistics toolkit for the envelope study (#151), stdlib only.

Student t distribution (CDF and quantiles via the regularized incomplete beta
function), OLS with a slope significance test, confidence intervals with an
optional lag-1 autocorrelation correction, and a tiny dense linear solver for
the model fits in analyze.py.
"""
import math

__all__ = ["betai", "t_cdf", "t_sf2", "t_ppf", "ols", "mean_ci", "lag1_autocorr", "solve", "lstsq"]


def _betacf(a, b, x, itmax=300, eps=3e-14):
    """Continued fraction for the incomplete beta function (Lentz's method)."""
    tiny = 1e-300
    qab, qap, qam = a + b, a + 1.0, a - 1.0
    c, d = 1.0, 1.0 - qab * x / qap
    d = 1.0 / (d if abs(d) > tiny else tiny)
    h = d
    for m in range(1, itmax + 1):
        m2 = 2 * m
        aa = m * (b - m) * x / ((qam + m2) * (a + m2))
        d = 1.0 + aa * d
        d = 1.0 / (d if abs(d) > tiny else tiny)
        c = 1.0 + aa / c
        c = c if abs(c) > tiny else tiny
        h *= d * c
        aa = -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2))
        d = 1.0 + aa * d
        d = 1.0 / (d if abs(d) > tiny else tiny)
        c = 1.0 + aa / c
        c = c if abs(c) > tiny else tiny
        de = d * c
        h *= de
        if abs(de - 1.0) < eps:
            break
    return h


def betai(a, b, x):
    """Regularized incomplete beta I_x(a, b)."""
    if x <= 0.0:
        return 0.0
    if x >= 1.0:
        return 1.0
    lbt = math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b) + a * math.log(x) + b * math.log1p(-x)
    bt = math.exp(lbt)
    if x < (a + 1.0) / (a + b + 2.0):
        return bt * _betacf(a, b, x) / a
    return 1.0 - bt * _betacf(b, a, 1.0 - x) / b


def t_sf2(t, df):
    """Two-sided p-value P(|T| >= |t|) for Student's t with df degrees of freedom."""
    if df <= 0:
        return float("nan")
    if math.isinf(t):
        return 0.0
    return betai(df / 2.0, 0.5, df / (df + t * t))


def t_cdf(t, df):
    p2 = t_sf2(t, df)
    return 1.0 - p2 / 2.0 if t >= 0 else p2 / 2.0


def t_ppf(q, df):
    """Quantile of Student's t (bisection on the CDF; df may be fractional)."""
    if not 0.0 < q < 1.0:
        raise ValueError("q must be in (0, 1)")
    if q == 0.5:
        return 0.0
    lo, hi = -1.0, 1.0
    while t_cdf(lo, df) > q:
        lo *= 2.0
    while t_cdf(hi, df) < q:
        hi *= 2.0
    for _ in range(200):
        mid = 0.5 * (lo + hi)
        if t_cdf(mid, df) < q:
            lo = mid
        else:
            hi = mid
        if hi - lo < 1e-12:
            break
    return 0.5 * (lo + hi)


def ols(xs, ys):
    """Simple linear regression y = a + b x.

    Returns dict(a, b, se_b, t, p, r2, n). p is the two-sided p-value for H0: b = 0.
    """
    n = len(xs)
    if n != len(ys) or n < 3:
        raise ValueError("ols needs >= 3 paired points")
    mx, my = sum(xs) / n, sum(ys) / n
    sxx = sum((x - mx) ** 2 for x in xs)
    sxy = sum((x - mx) * (y - my) for x, y in zip(xs, ys))
    syy = sum((y - my) ** 2 for y in ys)
    if sxx == 0:
        raise ValueError("ols: x has no variance")
    b = sxy / sxx
    a = my - b * mx
    sse = max(syy - b * sxy, 0.0)
    df = n - 2
    s2 = sse / df
    se_b = math.sqrt(s2 / sxx)
    if se_b == 0.0:
        t, p = (0.0, 1.0) if b == 0.0 else (math.inf, 0.0)
    else:
        t = b / se_b
        p = t_sf2(t, df)
    r2 = 1.0 - sse / syy if syy > 0 else 1.0
    return {"a": a, "b": b, "se_b": se_b, "t": t, "p": p, "r2": r2, "n": n}


def lag1_autocorr(xs):
    n = len(xs)
    if n < 3:
        return 0.0
    m = sum(xs) / n
    den = sum((x - m) ** 2 for x in xs)
    if den == 0:
        return 0.0
    return sum((xs[i] - m) * (xs[i + 1] - m) for i in range(n - 1)) / den


def mean_ci(xs, conf=0.95, autocorr=False):
    """Mean, sd, CV and a t-based confidence interval.

    With autocorr=True the sample size is replaced by an effective size
    n_eff = n (1 - rho1) / (1 + rho1) (lag-1 autocorrelation, clamped to
    [2, n]), which is appropriate for 1 s throughput samples from one run.
    """
    n = len(xs)
    if n == 0:
        return {"n": 0, "mean": None, "sd": None, "cv": None, "ci": [None, None], "n_eff": 0}
    m = sum(xs) / n
    if n == 1:
        return {"n": 1, "mean": m, "sd": None, "cv": None, "ci": [None, None], "n_eff": 1}
    sd = math.sqrt(sum((x - m) ** 2 for x in xs) / (n - 1))
    n_eff = float(n)
    rho = None
    if autocorr:
        rho = lag1_autocorr(xs)
        if rho > 0:
            n_eff = min(max(n * (1 - rho) / (1 + rho), 2.0), float(n))
    half = t_ppf(0.5 + conf / 2.0, n_eff - 1) * sd / math.sqrt(n_eff)
    out = {"n": n, "mean": m, "sd": sd, "cv": (sd / m if m else None), "ci": [m - half, m + half],
           "n_eff": round(n_eff, 2)}
    if rho is not None:
        out["rho1"] = round(rho, 4)
    return out


def solve(a, b):
    """Solve a dense square system a x = b (Gaussian elimination, partial pivoting)."""
    n = len(a)
    m = [list(map(float, row)) + [float(v)] for row, v in zip(a, b)]
    for col in range(n):
        piv = max(range(col, n), key=lambda r: abs(m[r][col]))
        if abs(m[piv][col]) < 1e-300:
            raise ValueError("singular matrix")
        m[col], m[piv] = m[piv], m[col]
        for r in range(n):
            if r != col:
                f = m[r][col] / m[col][col]
                if f:
                    for c in range(col, n + 1):
                        m[r][c] -= f * m[col][c]
    return [m[i][n] / m[i][i] for i in range(n)]


def lstsq(rows, ys, weights=None):
    """Least squares for y ~ rows @ beta via the normal equations (small problems only)."""
    k = len(rows[0])
    w = weights or [1.0] * len(ys)
    ata = [[sum(wi * r[i] * r[j] for r, wi in zip(rows, w)) for j in range(k)] for i in range(k)]
    aty = [sum(wi * r[i] * y for r, y, wi in zip(rows, ys, w)) for i in range(k)]
    return solve(ata, aty)
