"""The statistics of acceptance run 8, standard library only: the power of the one-sided paired t
test against a bound, the block count and plan that power asks for under a time cap, the pilot's
futility rule, and the union and intersection p-values. It reuses stats6's t tests by import.
"""
import math
from statistics import NormalDist

import stats6

DESCRIPTIVE_BELOW = 0.5  # a joint power under this makes the primary result descriptive


def t_power_below(n, mean, sigma, bound, alpha):
    """The normal-approximation power of the one-sided paired t test of H1 mean(d) < bound at n
    pairs, for the true mean and SD sigma. Superiority "a < b" on d = ln(a / b) is bound 0,
    non-inferiority at 3 % is bound ln 1.03. A mean at or above the bound has power 0; sigma 0
    gives 1 below the bound."""
    if mean >= bound:
        return 0.0
    if sigma == 0:
        return 1.0
    nd = NormalDist()
    return nd.cdf((bound - mean) / (sigma / math.sqrt(n)) - nd.inv_cdf(1 - alpha))


def n_for(power_target, comps, lo, hi):
    """The smallest n in [lo, hi] at which every component (mean, sigma, bound, alpha) reaches
    power_target; hi when none does below hi."""
    return next((n for n in range(lo, hi + 1)
                 if all(t_power_below(n, *c) >= power_target for c in comps)), hi)


def power_plan(cells, *, primary, batch, drop_order, cap_h, power_min, lo, hi, alpha,
               descriptive_below=DESCRIPTIVE_BELOW):
    """The block counts of every cell under a cap of cap_h hours. cells maps a name to
    {"comps": [(mean, sigma, bound)], "set_s": seconds one block of the cell takes, all arms}.
    Each kept primary cell takes the smallest n whose power reaches power_min ** (1 / m), m the
    primary cells kept; a secondary cell takes the largest primary n; the batch cell (never
    dropped) takes the n at which each of its components reaches power_min ** (1 / its count).
    While the hours exceed cap_h the next cell of drop_order is dropped and m recomputed; if the
    hours still exceed it, every non-batch n is scaled by one common factor (floor, never below
    lo). Returns n, dropped, hours (and hours_by_cell), scale, per-component power, the joint
    power (the product over the kept primary cells' components) and descriptive (joint power below
    descriptive_below)."""
    dropped = []

    def sized():
        kept = [c for c in cells if c not in dropped]
        prim = [c for c in kept if c in primary]
        n = {}
        for c in prim:
            comps = [(m, s, b, alpha) for m, s, b in cells[c]["comps"]]
            n[c] = n_for(power_min ** (1 / len(prim)), comps, lo, hi)
        top = max(n.values(), default=lo)
        for c in kept:
            if c == batch:
                comps = [(m, s, b, alpha) for m, s, b in cells[c]["comps"]]
                n[c] = n_for(power_min ** (1 / len(comps)), comps, lo, hi)
            elif c not in primary:
                n[c] = top
        return n

    def hours(n):
        return sum(k * cells[c]["set_s"] for c, k in n.items()) / 3600

    n = sized()
    for c in drop_order:
        if hours(n) <= cap_h:
            break
        if c in cells and c != batch and c not in dropped:
            dropped.append(c)
            n = sized()
    scale = 1.0
    if hours(n) > cap_h:
        fixed = cells[batch]["set_s"] * n[batch] if batch in n else 0
        flex = sum(k * cells[c]["set_s"] for c, k in n.items() if c != batch)
        scale = max(0.0, (cap_h * 3600 - fixed) / flex)
        n = {c: k if c == batch else max(lo, math.floor(k * scale)) for c, k in n.items()}
    power = {c: [t_power_below(n[c], m, s, b, alpha) for m, s, b in cells[c]["comps"]] for c in n}
    joint = math.prod(p for c in n if c in primary for p in power[c])
    return {"n": n, "dropped": dropped, "hours": hours(n),
            "hours_by_cell": {c: k * cells[c]["set_s"] / 3600 for c, k in n.items()},
            "scale": scale, "power": power, "joint": joint,
            "descriptive": joint < descriptive_below}


def futile(d, bound, alpha=0.10):
    """Whether the one-sided 1 - alpha lower bound of mean(d) is at or above bound, so the claim
    "mean(d) < bound" is excluded. Fewer than two values have no finite bound: not futile."""
    return stats6.paired_lower(d, bound, alpha)["lower"] >= bound


def union_p(ps):
    """The p of a union of legs by Bonferroni: min(1, tested legs x the smallest p). None legs
    are skipped; with no tested leg the result is None."""
    tested = [p for p in ps if p is not None]
    return min(1.0, len(tested) * min(tested)) if tested else None


def iut_p(ps):
    """The p of an intersection-union test: the largest leg p. Any None leg (or no leg) gives
    None."""
    return None if not ps or any(p is None for p in ps) else max(ps)
