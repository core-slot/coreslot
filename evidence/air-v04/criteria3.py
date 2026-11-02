#!/usr/bin/env python3
"""criterion-3.md's four criteria, computed literally from a record: criteria3.py RECORD

Written with criterion-3.md and committed with it, before any set of the third run existed.
Warn seconds are analyse.py's own metric (imported, not copied); the mean time to verdict is the
plain, unrounded mean over a set's agents of each agent's `verdict_s` (counted from its own
arrival). A tie counts against the claim. A pair that is missing (a part cut short) counts as a
failure: every claim is judged as k of its planned n, and p = P(X >= k), X ~ Binomial(n, 1/2),
with n the planned n. A claim passes at alpha = 0.05 when its count reaches the pre-registered k
(each of claim 1's two counts); the report also says whether its p (each of claim 1's two p)
would hold under a Bonferroni correction for the three tests, alpha = 0.05 / 3.
"""
import json
import math
import os
import sys

import analyse

ALPHA, TESTS = 0.05, 3
MARGIN = 1.05


def load(record):
    with open(os.path.join(record, "sets.jsonl")) as f:
        return {(s["tag"], s["arm"]): s for s in (json.loads(line) for line in f if line.strip())}


def mean_verdict(s):
    return sum(a["verdict_s"] for a in s["agents"].values()) / len(s["agents"])


def warn_s(s):
    return analyse.metrics(s, set())["warn_s"]


def p_at_least(k, n):
    return sum(math.comb(n, i) for i in range(k, n + 1)) / 2 ** n


def pairs(sets, prefix, other, n):
    """[(i, K set, other set)] of the planned pairs 1..n that have both arms, and the missing ones."""
    have, missing = [], []
    for i in range(1, n + 1):
        k, o = sets.get((f"{prefix}{i}", "K")), sets.get((f"{prefix}{i}", other))
        if k and o:
            have.append((i, k, o))
        else:
            missing.append(f"{prefix}{i}")
    return have, missing


def verdict(name, k, n, need, lines):
    p = p_at_least(k, n)
    lines.append(f"  {name}: {k} of {n} (needs {need}); p = {p:.4f}")
    return k >= need, p


def note(s):
    return f" [cut short: {s['stopped_by']}]" if s.get("stopped_by") else ""


def claim1(sets, lines):
    have, missing = pairs(sets, "pair-", "F", 10)
    lines.append("1. Replication, pair-1..10 (F against K, simultaneous start)")
    kw = kv = 0
    for i, k, f in have:
        wk, wf, vk, vf = warn_s(k), warn_s(f), mean_verdict(k), mean_verdict(f)
        kw, kv = kw + (wk < wf), kv + (vk < vf)
        lines.append(f"  pair-{i}: warn s F {wf} K {wk}; mean verdict F {vf:.4f} K {vk:.4f}{note(k)}{note(f)}")
    a = verdict("K fewer warn seconds than F", kw, 10, 9, lines)
    b = verdict("K lower mean time to verdict than F", kv, 10, 9, lines)
    return a[0] and b[0], max(a[1], b[1]), missing


def ratio_claim(sets, lines, title, prefix, other, n, need, show_order=False):
    have, missing = pairs(sets, prefix, other, n)
    lines.append(title)
    within = 0
    for i, k, o in have:
        mk, mo = mean_verdict(k), mean_verdict(o)
        ok = mk <= MARGIN * mo  # the criterion's own form; a ratio over 1.05 counts against
        within += ok
        order = f" order {','.join(k.get('order') or [])}" if show_order else ""
        lines.append(f"  {prefix}{i}:{order} K {mk:.4f} s, {other} {mo:.4f} s, K/{other} {mk / mo:.4f}"
                     f" {'within' if ok else 'OUTSIDE'}{note(k)}{note(o)}")
    ok, p = verdict(f"K <= {MARGIN} x {other}", within, n, need, lines)
    return ok, p, missing


def demos(record, lines):
    lines.append("4. The watchdog, demos/1..5")
    good = 0
    for n in range(1, 6):
        try:
            with open(os.path.join(record, "demos", str(n), "demo", "demo.json")) as f:
                d = json.load(f)
        except (OSError, ValueError):
            lines.append(f"  demo {n}: missing")
            continue
        hogs = {h["hog"]: h for h in d["hogs"]}
        checks = {"a hog stopped": any(h["stops"] for h in hogs.values()),
                  "the oldest never stopped": 1 in hogs and not hogs[1]["stops"],
                  "every hog exit 0": all(h["exit"] == 0 for h in hogs.values()),
                  "guard did not fire": d.get("stopped_by") is None}
        good += all(checks.values())
        lines.append(f"  demo {n}: " + ", ".join(f"{c} {'yes' if v else 'NO'}" for c, v in checks.items()))
    lines.append(f"  meet all four: {good} of 5")


def report(record):
    sets, lines, claims = load(record), [], []
    claims.append(("1", *claim1(sets, lines)))
    claims.append(("2", *ratio_claim(sets, lines, "2. Against S1, sem-pair-1..24 (K/S1, arrivals 1 s apart)",
                                     "sem-pair-", "S", 24, 17, show_order=True)))
    claims.append(("3", *ratio_claim(sets, lines, "3. With room, light-pair-1..20 (K/F)",
                                     "light-pair-", "F", 20, 15)))
    demos(record, lines)
    lines.append("verdicts")
    for name, ok, p, missing in claims:
        lines.append(f"  claim {name}: {'PASS' if ok else 'FAIL'} at alpha {ALPHA};"
                     f" {'holds' if ok and p <= ALPHA / TESTS else 'does not hold'} under Bonferroni"
                     f" alpha {ALPHA / TESTS:.4f}; missing pairs: {', '.join(missing) or 'none'}")
    return "\n".join(lines)


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__.splitlines()[0])
    print(report(sys.argv[1]))
