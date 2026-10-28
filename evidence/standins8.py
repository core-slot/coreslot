#!/usr/bin/env python3
"""Acceptance run 8's stand-ins: each gate of overload/stand8.json measured alone, and checked
against the gate-class profiles (evidence/run8-profiles.json, "mix").

usage:
  standins8.py measure STAND OUT [--gate NAME ...] [--runs 3] [--cooldown 20]
  standins8.py check STAND RECORDS PROFILES [--h-quiet 9000]

  measure  appends a "quiet" record (uptime, vm.swapusage, the matching pgrep lines), then runs
           each selected gate (default: every gate of "classes", "candidates" and "batch") alone
           under `coreslot run --independent --label <label>` with CORESLOT_STATE_DIR a fresh
           directory under OUT, in its agent A1 checkout with "{n}" = free_n, after a sleep of
           --cooldown s: one warm-up run 0 first (not the batch), then runs 1..--runs. One record
           per run to OUT/standins.jsonl, figures from coreslot's run log record of that run;
           "failed" when the exit is non-zero or the output (OUT/logs/) matches fail_pattern.
           A (gate, run) already recorded is skipped.
  check    the medians of runs 1..3 per gate (a failed run fails the gate) against the class's
           band in PROFILES' mix.targets: wall, cores, max_rss MiB, slowest <= repeat_max_ratio x
           fastest, wall >= 10 s; the batch against mix.long_batch.standin, both memory figures
           under min(cap, H_quiet - behind_agents x behind-class p90). Per class the stand's pick
           if it passes, else the first passing candidate; with none and >= 3 measured gates, the
           one missing the fewest bounds, then nearest the reference p50s (sum of relative
           distances), marked out_of_tolerance. Prints a markdown table; exits 1 when a class or
           the batch has no passing gate.
"""
import argparse
import json
import os
import re
import statistics
import subprocess
import sys
import time


def gates(stand):
    """(class, gate) for the pick of each class, its candidates, then the batch."""
    out = [(c, g) for c, g in stand["classes"].items()]
    out += [(c, g) for c, gs in stand.get("candidates", {}).items() for g in gs]
    return out + [("long_batch", stand["batch"])]


def expand(gate, agent="A1"):
    n = str(gate.get("free_n", ""))
    return (os.path.expanduser(gate["cwd"].replace("{agent}", agent)),
            [a.replace("{n}", n) for a in gate["argv"]])


def sh(*argv):
    return subprocess.run(argv, capture_output=True, text=True).stdout


def record(path, rec):
    with open(path, "a") as f:
        f.write(json.dumps(rec, sort_keys=True) + "\n")


def load(path):
    return [json.loads(l) for l in open(path)] if os.path.exists(path) else []


def measure(a):
    stand = json.load(open(a.stand))
    os.makedirs(os.path.join(a.out, "logs"), exist_ok=True)
    path = os.path.join(a.out, "standins.jsonl")
    done = {(r["gate"], r["run"]) for r in load(path) if "gate" in r}
    record(path, {"kind": "quiet", "time_ns": time.time_ns(), "uptime": sh("uptime"),
                  "swapusage": sh("sysctl", "vm.swapusage"),
                  "pgrep": sh("pgrep", "-lf", "go |pytest|node")})
    state = os.path.join(a.out, f"state-{time.time_ns()}")
    os.makedirs(state, mode=0o700)
    runlog = os.path.join(state, "log", "runs.jsonl")
    coreslot = os.path.expanduser(stand["coreslot"])
    for cls, g in gates(stand):
        if a.gate and g["gate"] not in a.gate:
            continue
        cwd, argv = expand(g)
        for run in ([] if cls == "long_batch" else [0]) + list(range(1, a.runs + 1)):
            if (g["gate"], run) in done:
                continue
            time.sleep(a.cooldown)
            n0, log = len(load(runlog)), os.path.join(a.out, "logs", f"{g['gate']}-{run}.log")
            with open(log, "w") as f:
                p = subprocess.run([coreslot, "run", "--independent", "--label", g["label"], "--",
                                    *argv], cwd=cwd, stdout=f, stderr=subprocess.STDOUT,
                                   env=dict(os.environ, **g.get("env", {}),
                                            CORESLOT_STATE_DIR=state))
            new = load(runlog)[n0:]
            r = new[-1] if new else {}
            cpu = (r.get("cpu_user_s") or 0) + (r.get("cpu_sys_s") or 0)
            pat = g.get("fail_pattern")
            matched = bool(pat and re.search(pat, open(log, errors="replace").read(), re.M))
            record(path, {"gate": g["gate"], "class": cls, "label": g["label"], "run": run,
                          "ran_s": r.get("ran_s"), "cpu_user_s": r.get("cpu_user_s"),
                          "cpu_sys_s": r.get("cpu_sys_s"),
                          "cores": round(cpu / r["ran_s"], 3) if r.get("ran_s") else None,
                          "max_rss_kib": r.get("max_rss_kib"),
                          "mem_peak_kib": r.get("mem_peak_kib"), "exit": p.returncode,
                          "failed": p.returncode != 0 or matched or not new,
                          "time_ns": time.time_ns(), "argv": argv})


def figures(recs, gate):
    """Medians of runs 1..3 of a gate, or None while it has fewer than 3."""
    rs = [r for r in recs if r.get("gate") == gate and 1 <= r["run"] <= 3]
    if len(rs) < 3:
        return None
    if any(r["failed"] for r in rs):
        return {"failed": True}
    med = lambda k: statistics.median(r[k] for r in rs)  # noqa: E731
    walls = [r["ran_s"] for r in rs]
    return {"failed": False, "wall_s": med("ran_s"), "cores": med("cores"),
            "mem_mib": med("max_rss_kib") / 1024, "peak_mib": med("mem_peak_kib") / 1024,
            "ratio": max(walls) / min(walls)}


def bounds(fig, target, repeat):
    if fig["failed"]:
        return {"runs_ok": False}
    inb = lambda k: target[k][0] <= fig[k] <= target[k][1]  # noqa: E731
    return {"wall_s": inb("wall_s"), "cores": inb("cores"), "mem_mib": inb("mem_mib"),
            "repeat": fig["ratio"] <= repeat, "min_10s": fig["wall_s"] >= 10}


def batch_bounds(fig, rule, repeat, h_quiet):
    if fig["failed"]:
        return {"runs_ok": False}
    cap = min(rule["mem_mib_max_share_cap"],
              h_quiet - rule["behind_agents"] * rule["behind_class_mem_p90_mib"])
    return {"wall_s": rule["wall_s_min"] <= fig["wall_s"] <= rule["wall_s_max"],
            "cores": rule["cores"][0] <= fig["cores"] <= rule["cores"][1],
            "mem_mib": fig["mem_mib"] <= cap, "peak_mib": fig["peak_mib"] <= cap,
            "repeat": fig["ratio"] <= repeat}


def distance(fig, p50):
    if fig["failed"]:
        return float("inf")
    return sum(abs(fig[k] - p50[k]) / p50[k] for k in ("wall_s", "cores", "mem_mib"))


def choose(rows, p50):
    """rows: [(gate, fig, bounds)], the pick first. Returns (gate, verdict, missed)."""
    for gate, _, b in rows:
        if all(b.values()):
            return gate, "pass", []
    if len(rows) < 3:
        return None, "none", []
    gate, _, b = min(rows, key=lambda r: (sum(not v for v in r[2].values()), distance(r[1], p50)))
    return gate, "out_of_tolerance", sorted(k for k, v in b.items() if not v)


def check(a):
    stand, recs = json.load(open(a.stand)), load(a.records)
    mix = json.load(open(a.profiles))["mix"]
    repeat, rule = mix["repeat_max_ratio"], mix["long_batch"]["standin"]
    print("| class | gate | wall s | cores | max_rss MiB | mem_peak MiB | slow/fast | missed |")
    print("|---|---|---|---|---|---|---|---|")
    by_class, ok = {}, True
    for cls, g in gates(stand):
        fig = figures(recs, g["gate"])
        if fig is None:
            print(f"| {cls} | {g['gate']} | not measured | | | | | |")
            continue
        b = (batch_bounds(fig, rule, repeat, a.h_quiet) if cls == "long_batch"
             else bounds(fig, mix["targets"][cls], repeat))
        by_class.setdefault(cls, []).append((g["gate"], fig, b))
        nums = ("failed | | | | " if fig["failed"] else " | ".join(
            f"{fig[k]:.3f}" for k in ("wall_s", "cores", "mem_mib", "peak_mib", "ratio")))
        print(f"| {cls} | {g['gate']} | {nums} | "
              f"{', '.join(k for k, v in b.items() if not v) or '-'} |")
    print("\n| class | stand-in | verdict | bounds missed |\n|---|---|---|---|")
    for cls in list(stand["classes"]) + ["long_batch"]:
        p50 = mix["targets"].get(cls, {}).get("p50", {"wall_s": 1, "cores": 1, "mem_mib": 1})
        gate, verdict, missed = choose(by_class.get(cls, []), p50)
        ok &= verdict == "pass"
        print(f"| {cls} | {gate or '-'} | {verdict} | {', '.join(missed) or '-'} |")
    return 0 if ok else 1


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = p.add_subparsers(dest="cmd", required=True)
    m = sub.add_parser("measure")
    m.add_argument("stand"), m.add_argument("out")
    m.add_argument("--gate", action="append", default=[])
    m.add_argument("--runs", type=int, default=3)
    m.add_argument("--cooldown", type=float, default=20)
    c = sub.add_parser("check")
    c.add_argument("stand"), c.add_argument("records"), c.add_argument("profiles")
    c.add_argument("--h-quiet", type=float, default=9000)
    a = p.parse_args(argv)
    return measure(a) if a.cmd == "measure" else check(a)


if __name__ == "__main__":
    sys.exit(main())
