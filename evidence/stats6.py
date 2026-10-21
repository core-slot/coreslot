"""The statistics of acceptance run 6, standard library only: Student's t through the regularized
incomplete beta, one-sided paired t tests against a bound, a BCa bootstrap interval of the mean,
the Clopper-Pearson interval, and the block count the pilot's sigma asks for.
"""
import math
import random
from statistics import NormalDist, fmean, stdev


def _betacf(a, b, x):
    """The continued fraction of the incomplete beta (modified Lentz)."""
    tiny, qab, qap, qam = 1e-300, a + b, a + 1, a - 1
    c, d = 1.0, 1 - qab * x / qap
    d = 1 / (d if abs(d) > tiny else tiny)
    h = d
    for m in range(1, 1000):
        m2 = 2 * m
        for aa in (m * (b - m) * x / ((qam + m2) * (a + m2)),
                   -(a + m) * (qab + m) * x / ((a + m2) * (qap + m2))):
            d = 1 + aa * d
            d = 1 / (d if abs(d) > tiny else tiny)
            c = 1 + aa / c
            c = c if abs(c) > tiny else tiny
            h *= d * c
        if abs(d * c - 1) < 1e-15:
            break
    return h


def betainc(a, b, x):
    """The regularized incomplete beta I_x(a, b)."""
    if x <= 0:
        return 0.0
    if x >= 1:
        return 1.0
    front = math.exp(math.lgamma(a + b) - math.lgamma(a) - math.lgamma(b)
                     + a * math.log(x) + b * math.log1p(-x))
    if x < (a + 1) / (a + b + 2):
        return front * _betacf(a, b, x) / a
    return 1 - front * _betacf(b, a, 1 - x) / b


def _bisect(f, lo, hi, iters=200):
    """The root of an increasing f between lo and hi."""
    for _ in range(iters):
        mid = (lo + hi) / 2
        if f(mid) < 0:
            lo = mid
        else:
            hi = mid
    return (lo + hi) / 2


def t_sf(t, df):
    """P(T > t) for Student's t with df degrees of freedom."""
    tail = 0.5 * betainc(df / 2, 0.5, df / (df + t * t))
    return tail if t >= 0 else 1 - tail


def t_ppf(q, df):
    """The t whose CDF is q."""
    hi = 1.0
    while 1 - t_sf(hi, df) < q:
        hi *= 2
    lo = -1.0
    while 1 - t_sf(lo, df) > q:
        lo *= 2
    return _bisect(lambda t: (1 - t_sf(t, df)) - q, lo, hi)


def paired_upper(d, bound, alpha=0.05):
    """H0 mean(d) >= bound against H1 mean(d) < bound; upper is the one-sided 1 - alpha bound.
    Fewer than two values test nothing: p 1, no finite bound."""
    n, mean = len(d), fmean(d) if d else math.nan
    if n < 2:
        return {"mean": mean, "se": None, "t": None, "p": 1.0, "upper": math.inf}
    se = stdev(d) / math.sqrt(n)
    if se == 0:  # identical differences: the bound is the mean itself
        return {"mean": mean, "se": 0.0, "t": None, "p": 0.0 if mean < bound else 1.0,
                "upper": mean}
    t = (mean - bound) / se
    return {"mean": mean, "se": se, "t": t, "p": t_sf(-t, n - 1),
            "upper": mean + t_ppf(1 - alpha, n - 1) * se}


def paired_lower(d, bound, alpha=0.05):
    """The mirror of paired_upper: H1 mean(d) > bound; lower is the one-sided 1 - alpha bound."""
    r = paired_upper([-x for x in d], -bound, alpha)
    return {"mean": -r["mean"], "se": r["se"], "t": None if r["t"] is None else -r["t"],
            "p": r["p"], "lower": -r["upper"]}


def _quantile(xs, q):
    """The q quantile of the sorted xs, interpolated linearly."""
    pos = q * (len(xs) - 1)
    i = min(int(pos), len(xs) - 2)
    return xs[i] + (xs[i + 1] - xs[i]) * (pos - i)


def bca(d, alpha=0.10, reps=10000, seed=0):
    """The two-sided 1 - alpha BCa bootstrap interval of the mean, with jackknife acceleration."""
    n, mean = len(d), fmean(d) if d else math.nan
    if n < 2 or min(d) == max(d):
        return (mean, mean)
    rnd = random.Random(seed)
    boot = sorted(fmean(rnd.choices(d, k=n)) for _ in range(reps))
    below = sum(b < mean for b in boot) + 0.5 * sum(b == mean for b in boot)
    nd = NormalDist()
    z0 = nd.inv_cdf(min(max(below / reps, 1 / reps), 1 - 1 / reps))
    total = sum(d)
    jack = [(total - x) / (n - 1) for x in d]
    jm = fmean(jack)
    num = sum((jm - j) ** 3 for j in jack)
    den = 6 * sum((jm - j) ** 2 for j in jack) ** 1.5
    a = num / den if den else 0.0

    def adjusted(q):
        z = nd.inv_cdf(q)
        return nd.cdf(z0 + (z0 + z) / (1 - a * (z0 + z)))
    return (_quantile(boot, adjusted(alpha / 2)), _quantile(boot, adjusted(1 - alpha / 2)))


def clopper_pearson(k, n, alpha=0.05):
    """The two-sided 1 - alpha exact interval of a proportion k of n."""
    lo = 0.0 if k == 0 else _bisect(lambda p: betainc(k, n - k + 1, p) - alpha / 2, 0.0, 1.0)
    hi = 1.0 if k == n else _bisect(lambda p: betainc(k + 1, n - k, p) - (1 - alpha / 2),
                                    0.0, 1.0)
    return (lo, hi)


def chi2_ppf(q, df):
    """The chi-square quantile by Wilson-Hilferty."""
    c = 2 / (9 * df)
    return df * (1 - c + NormalDist().inv_cdf(q) * math.sqrt(c)) ** 3


def sigma_upper(sd, k, conf=0.80):
    """The one-sided conf upper bound of sigma from a sample SD of k values."""
    return sd * math.sqrt((k - 1) / chi2_ppf(1 - conf, k - 1))


def blocks_needed(sigma, lo, hi, multiple, margin=0.03, alpha=0.05, power=0.80):
    """Blocks for a one-sided paired t test against ln(1 + margin): the normal approximation
    plus 2, rounded up to a multiple, clamped to [lo, hi]."""
    nd = NormalDist()
    z = nd.inv_cdf(1 - alpha) + nd.inv_cdf(power)
    n = math.ceil(z * z * sigma * sigma / math.log1p(margin) ** 2) + 2
    n = -(-n // multiple) * multiple
    return min(max(n, lo), hi)
