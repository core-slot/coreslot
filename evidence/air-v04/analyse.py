#!/usr/bin/env python3
"""Reads a run's records (OUT of run.py) and prints the per-pair table, one-sided sign tests of
K against F, the exploratory pilot apart, and the watchdog demo.

  analyse.py OUT

A false failure is a non-zero exit of a gate whose alone runs (learn.jsonl) all passed. Warn and
critical seconds count the 1 s samples at memory pressure level 2 or above and at 4. Swap growth
is the peak of swap used in the set less its value at the start. The sign tests drop tied pairs
and ask how likely K comes out lower in at least that many of the rest by chance. The criterion
is the owner's, written by hand into criterion.md beside this file and committed before the
pairs; nothing here reads it.
"""
import json
import math
import os
import re
import statistics
import sys


CRITERION = os.path.join(os.path.dirname(os.path.abspath(__file__)), "criterion.md")


def load(path):
    try:
        with open(path) as f:
            return [json.loads(line) for line in f if line.strip()]
    except OSError:
        return []


def stable_gates(learn):
    runs = {}
    for r in learn:
        runs.setdefault(r["gate"], []).append(r["exit"])
    return {g for g, exits in runs.items() if exits and not any(exits)}


def metrics(s, stable):
    levels = [x["level"] or 1 for x in s["samples"]]
    k = s.get("runlog", [])
    return {"warn_s": sum(v >= 2 for v in levels), "crit_s": sum(v >= 4 for v in levels),
            "swap_mib": (s["swap_mib"]["peak"] or 0) - (s["swap_mib"]["start"] or 0),
            "makespan_s": s["makespan_s"],
            "verdict_s": round(statistics.mean(a["verdict_s"] for a in s["agents"].values()), 1),
            "ff": sum(1 for g in s["gates"] if g["exit"] and g["gate"] in stable),
            "fail": sum(1 for g in s["gates"] if g["exit"]),
            "waited_s": round(sum(x.get("waited_s", 0) for x in k), 1),
            "stops": sum(1 for x in k if x.get("stops"))}


def sign_test(wins, n):
    """One-sided: the chance of at least `wins` of `n` untied pairs favouring K by luck."""
    return sum(math.comb(n, i) for i in range(wins, n + 1)) / 2 ** n if n else 1.0


def pairs(sets):
    by = {}
    for s in sets:
        m = re.fullmatch(r"pair-(\d+)", s["tag"])
        if m:
            by.setdefault(int(m.group(1)), {})[s["arm"]] = s
    return [(i, p) for i, p in sorted(by.items()) if set(p) == {"F", "K"}]


COLS = [("warn_s", "warn s"), ("crit_s", "crit s"), ("swap_mib", "swap+ MiB"),
        ("makespan_s", "makespan s"), ("verdict_s", "mean verdict s"), ("ff", "false fail")]


def table(rows):
    """rows: [(name, arm, metrics, extra)] -> lines."""
    head = f"{'':10} {'arm':3} " + " ".join(f"{t:>14}" for _, t in COLS) + "  first in pair / K waited s, stops"
    out = [head]
    for name, arm, m, extra in rows:
        k = f"  waited {m['waited_s']}, stops {m['stops']}" if arm == "K" else ""
        out.append(f"{name:10} {arm:3} " + " ".join(f"{m[c]:>14}" for c, _ in COLS) + f"  {extra}{k}")
    return out


def report(out):
    learn, sets = load(os.path.join(out, "learn.jsonl")), load(os.path.join(out, "sets.jsonl"))
    stable = stable_gates(learn)
    lines = [f"alone runs: {len(learn)}; stable gates: {', '.join(sorted(stable)) or 'none'}"]
    for g in sorted({r["gate"] for r in learn}):
        rs = [r for r in learn if r["gate"] == g]
        lines.append(f"  {g}: exits {[r['exit'] for r in rs]}, median {statistics.median(r['dur_s'] for r in rs)} s")
    lines.append(f"criterion: {CRITERION if os.path.exists(CRITERION) else 'none written'} (the owner's; not read here)")
    pilot = [s for s in sets if s["tag"] == "pilot"]
    if pilot:
        lines += ["", "PILOT (exploratory; not part of any test)"]
        lines += table([("pilot", s["arm"], metrics(s, stable), "") for s in pilot])
    ps = pairs(sets)
    lines += ["", f"PAIRS ({len(ps)} complete)"]
    rows = []
    for i, p in ps:
        first = min(p.values(), key=lambda s: s["started"])["arm"]
        rows += [(f"pair-{i}", arm, metrics(p[arm], stable), first if arm == "F" else "") for arm in "FK"]
    lines += table(rows)
    for key, what in (("warn_s", "warn seconds"), ("ff", "false failures"), ("verdict_s", "mean time to verdict")):
        d = [metrics(p["K"], stable)[key] - metrics(p["F"], stable)[key] for _, p in ps]
        wins, n = sum(x < 0 for x in d), sum(x != 0 for x in d)
        lines.append(f"{what}: K lower in {wins} of {n} untied pairs ({len(d) - n} tied);"
                     f" one-sided sign test p = {sign_test(wins, n):.4f}")
    lines += [""] + demo(out)
    return "\n".join(lines)


def demo(out):
    try:
        with open(os.path.join(out, "demo", "demo.json")) as f:
            d = json.load(f)
    except OSError:
        return ["DEMO: no record"]
    hogs = d["hogs"]
    stopped = [h for h in hogs if h["stops"]]
    lines = [f"DEMO (n = 1, a functional check): {len(hogs)} hogs of {d['hog_gib']} GiB, oldest first;"
             f" guard {'stopped it: ' + d['stopped_by'] if d['stopped_by'] else 'did not fire'}"]
    for h in hogs:
        lines.append(f"  hog {h['hog']}: stops {h['stops']}, attempts {h['attempts']}, exit {h['exit']}"
                     + (", gave up" if h["gave_up"] else ""))
    lines.append(f"a stop happened: {'yes, hog ' + ', '.join(str(h['hog']) for h in stopped) if stopped else 'no'}")
    restarted = [h for h in stopped if h["attempts"] > h["stops"] and not h["gave_up"]]
    lines.append(f"stopped hogs restarted: {len(restarted)} of {len(stopped)}")
    done = not d["stopped_by"] and all(h["exit"] == 0 for h in hogs)
    lines.append(f"every hog completed: {'yes' if done else 'no'}")
    return lines


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: analyse.py OUT")
    print(report(sys.argv[1]))
