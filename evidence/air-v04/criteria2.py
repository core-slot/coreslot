#!/usr/bin/env python3
"""criterion-2.md's two pre-registered tests, computed literally from a record: criteria2.py RECORD

Written after the second run's data, to compute what criterion-2.md states without changing
analyse.py, which the criteria froze. For each pair it takes the plain, unrounded mean over the
agents of their time to verdict (each agent's `verdict_s`, counted from its own arrival), the
ratio of K's to the other arm's, and whether the ratio is at most 1.05. A test passes in n of the
five pairs; its one-sided sign-test p is the chance of n or more of five by luck, P(X >= n) for
X ~ Binomial(5, 1/2), so five of five is p = 0.031 as the criterion says. A ratio of exactly
1.05 counts as within; the criterion names no tie, and none is dropped. The exploratory sets are
printed for reference and enter no test.
"""
import json
import math
import os
import sys

TESTS = (("1. K against S1, heavy stand", "sem-pair-", "S"),
         ("2. K against F, light mix", "light-pair-", "F"))
MARGIN = 1.05


def mean_verdict(s):
    return sum(a["verdict_s"] for a in s["agents"].values()) / len(s["agents"])


def p_at_least(n, of):
    return sum(math.comb(of, k) for k in range(n, of + 1)) / 2 ** of


def test(sets, prefix, other, pairs=5):
    """[(pair, K mean, other mean, ratio, within)] for pairs 1..5 that have both arms."""
    by = {(s["tag"], s["arm"]): s for s in sets}
    rows = []
    for i in range(1, pairs + 1):
        k, o = by.get((f"{prefix}{i}", "K")), by.get((f"{prefix}{i}", other))
        if k and o:
            mk, mo = mean_verdict(k), mean_verdict(o)
            rows.append((i, mk, mo, mk / mo, mk <= MARGIN * mo))  # the criterion's own form
    return rows


def report(record):
    with open(os.path.join(record, "sets.jsonl")) as f:
        sets = [json.loads(line) for line in f if line.strip()]
    out = []
    for title, prefix, other in TESTS:
        rows = test(sets, prefix, other)
        n = sum(r[4] for r in rows)
        out.append(f"{title}: K <= {MARGIN} x {other} in each of {prefix}1..5")
        for i, mk, mo, ratio, ok in rows:
            out.append(f"  {prefix}{i}: K {mk:.4f} s, {other} {mo:.4f} s, K/{other} {ratio:.4f}"
                       f" {'within' if ok else 'OUTSIDE'}")
        verdict = "passes" if n == 5 and len(rows) == 5 else "fails"
        out.append(f"  within in {n} of {len(rows)} pairs; one-sided sign test p = {p_at_least(n, 5):.4f}; the test {verdict}")
    out.append("exploratory, in no test:")
    for s in sets:
        if s["tag"] == "sem-pilot" or s["tag"].startswith("tune-S"):
            out.append(f"  {s['tag']} {s['arm']}: mean time to verdict {mean_verdict(s):.4f} s,"
                       f" makespan {s['makespan_s']} s")
    return "\n".join(out)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__.splitlines()[0])
    print(report(sys.argv[1]))
