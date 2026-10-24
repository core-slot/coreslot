"""The statistics of acceptance run 7, standard library only: the one-sided Wilcoxon signed-rank
test (exact with ties up to 25 pairs, a corrected normal approximation above), the binomial lower
tail, the intersection-union and Holm decision rules, and the power of the sign and t tests.
"""
import math
from statistics import NormalDist

EXACT_MAX = 25  # the largest n whose rank distribution is enumerated


def _doubled_ranks(a):
    """2 * the average rank of each value of a (sorted ascending): an integer even for ties."""
    out, i, n = [], 0, len(a)
    while i < n:
        j = i
        while j + 1 < n and a[j + 1] == a[i]:
            j += 1
        out.extend([i + j + 2] * (j - i + 1))
        i = j + 1
    return out


def wilcoxon_less(d):
    """The one-sided signed-rank test of H1 median(d) < 0. Zeros drop out, ties take average
    ranks; p = P(W+ <= observed), exact for n <= 25 and the normal approximation (tie-corrected
    variance, continuity 0.5) above. wins counts d < 0, losses d > 0."""
    d = [x for x in d if x != 0]
    n = len(d)
    wins, losses = sum(x < 0 for x in d), sum(x > 0 for x in d)
    if n == 0:
        return {"n": 0, "w_plus": 0, "p": 1.0, "wins": 0, "losses": 0}
    order = sorted(range(n), key=lambda i: abs(d[i]))
    r2 = [0] * n
    for i, r in zip(order, _doubled_ranks([abs(d[i]) for i in order])):
        r2[i] = r
    w2 = sum(r for r, x in zip(r2, d) if x > 0)
    if n <= EXACT_MAX:
        counts = [0] * (sum(r2) + 1)  # counts[s]: sign patterns whose doubled W+ is s
        counts[0] = 1
        for r in r2:
            for s in range(len(counts) - 1, r - 1, -1):
                counts[s] += counts[s - r]
        p = sum(counts[:w2 + 1]) / 2 ** n
    else:
        ties = [sum(abs(x) == v for x in d) for v in {abs(x) for x in d}]
        var = n * (n + 1) * (2 * n + 1) / 24 - sum(t ** 3 - t for t in ties) / 48
        p = NormalDist().cdf((w2 / 2 + 0.5 - n * (n + 1) / 4) / math.sqrt(var))
    return {"n": n, "w_plus": w2 / 2, "p": p, "wins": wins, "losses": losses}


def binom_upper_p(k, n, p0):
    """P(X <= k) for X binomial(n, p0); n = 0 gives 1."""
    if n == 0 or k >= n:
        return 1.0
    return sum(math.comb(n, i) * p0 ** i * (1 - p0) ** (n - i) for i in range(k + 1))


def iut(ps, alpha):
    """The intersection-union test: the joint null is rejected only when every component's p is
    below alpha."""
    comps = {name: {"p": p, "rejected": p < alpha} for name, p in ps.items()}
    return {"rejected": all(c["rejected"] for c in comps.values()), "components": comps}


def holm(ps, alpha):
    """Holm's step-down over ps in ascending p, with thresholds alpha / (m - i). A None p is
    untested: it counts in m as p = 1 and is never rejected. After one failure none is rejected."""
    order = sorted(ps, key=lambda name: 1.0 if ps[name] is None else ps[name])
    rows, going = [], True
    for i, name in enumerate(order):
        threshold = alpha / (len(order) - i)
        tested = ps[name] is not None
        going = going and tested and ps[name] < threshold
        rows.append({"name": name, "p": ps[name], "threshold": threshold, "rejected": going,
                     "tested": tested})
    return rows


def sign_power(m, win_rate, alpha):
    """The power of the exact one-sided sign test of m untied pairs at that win rate: the critical
    count is the smallest c with P(X >= c | m, 0.5) <= alpha."""
    def upper(c, q):
        return sum(math.comb(m, k) * q ** k * (1 - q) ** (m - k) for k in range(c, m + 1))
    c = next(c for c in range(m + 2) if upper(c, 0.5) <= alpha)
    return upper(c, win_rate)


def t_power(n, mean, sd, alpha):
    """The normal-approximation power of a one-sided paired t test of mean > 0."""
    if mean <= 0 or sd == 0:
        return 0.0
    nd = NormalDist()
    return nd.cdf(mean / (sd / math.sqrt(n)) - nd.inv_cdf(1 - alpha))
