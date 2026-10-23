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
