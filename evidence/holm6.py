#!/usr/bin/env python3
"""Acceptance run 6's secondary analysis, not pre-registered: a deviation declared on the pull
request before the run ended. It replaces the fixed-sequence chain with Holm's step-down over the
same five hypotheses at family-wise alpha 0.05, on the p-values `run6.py summary` wrote; the
primary analysis stays the pre-registered chain, unchanged.

Each hypothesis enters with one p-value: H1 the largest of its three; H4 the larger of its two; H5,
for each N, six times the smallest of its six p-values, capped at 1, then the largest over N; H3
and H2 their single p. A hypothesis summary.json marks untested (H2 under the light mix's pressure
rule, or a mix short of n) stays out, and Holm runs over the rest.

  python3 evidence/holm6.py OUT    # reads OUT/summary.json; writes OUT/holm.json and OUT/holm.txt
"""
import json
import os
import sys

ALPHA = 0.05
LABEL = "secondary analysis, not pre-registered (declared deviation)"
CHAIN = ("H1", "H4", "H5", "H3", "H2")


def one_p(name, h):
    """The hypothesis's single p-value, as the declared deviation defines it."""
    if name == "H1":
        return max(h[k]["p"] for k in ("t", "warn", "swapout"))
    if name == "H4":
        return max(h[k]["p"] for k in ("warn", "swapout"))
    if name == "H5":
        return max(min(1.0, 6 * min(r["p"] for r in v["tests"])) for v in h["per_n"].values())
    return h["t"]["p"]


def analyse(s):
    """summary.json -> each tested hypothesis's p, its rank, its Holm threshold and decision. With
    p(1) <= ... <= p(k), the hypothesis of rank i is rejected when p(j) < alpha / (k - j + 1)
    for every j <= i."""
    ps, left_out = {}, {}
    for name in CHAIN:
        h = s["tests"][name]
        if "untested" in h:
            left_out[name] = h["untested"]
        else:
            ps[name] = one_p(name, h)
    order = sorted(ps, key=lambda n: ps[n])
    k, going, hyps = len(order), True, {}
    for j, name in enumerate(order, 1):
        threshold = ALPHA / (k - j + 1)
        going = going and ps[name] < threshold
        hyps[name] = {"p": ps[name], "rank": j, "threshold": threshold, "rejected": going,
                      "primary": s["tests"][name]["status"]}
    return {"label": LABEL, "alpha": ALPHA, "k": k,
            "hypotheses": {n: hyps[n] for n in CHAIN if n in hyps}, "left_out": left_out}


def holm_text(r):
    lines = [f"Acceptance run 6: Holm's step-down, {r['label']}", "",
             f"Family-wise alpha {r['alpha']} over k = {r['k']} hypotheses", ""]
    for name, h in sorted(r["hypotheses"].items(), key=lambda kv: kv[1]["rank"]):
        lines.append(f"{name}: rank {h['rank']}, p {h['p']:.4g} against {h['threshold']:.4g}:"
                     f" {'rejected' if h['rejected'] else 'not rejected'}"
                     f" (primary chain: {h['primary']})")
    for name, why in r["left_out"].items():
        lines.append(f"{name}: left out, untested: {why}")
    return "\n".join(lines + [""])


def main(argv):
    if len(argv) != 2:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    out = argv[1]
    with open(os.path.join(out, "summary.json")) as f:
        r = analyse(json.load(f))
    with open(os.path.join(out, "holm.json"), "w") as f:
        json.dump(r, f, indent=1)
    text = holm_text(r)
    with open(os.path.join(out, "holm.txt"), "w") as f:
        f.write(text)
    print(text)


if __name__ == "__main__":
    main(sys.argv)
