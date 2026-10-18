#!/usr/bin/env python3
"""Product acceptance: the same jobs started at one instant, with and without coreslot, in
alternating pairs.

usage:
  acceptance.py run GATES.json OUTDIR FIRST_PAIR LAST_PAIR [--order wc|abba] [--cooldown S]
                [--probe-tolerance F] [--max-cooldown S]
  acceptance.py alone GATES.json OUTDIR RUNS [--cooldown S] [--probe-tolerance F]
                [--max-cooldown S]
  acceptance.py summary OUTDIR

GATES.json names the coreslot binary, a state directory of its own (CORESLOT_STATE_DIR, so the
run is a separate budget with its own run log), and the jobs. Each job has a working directory,
extra environment, the command without coreslot and the command with it:

  {"coreslot": "/path/to/coreslot", "state_dir": "/path/to/state",
   "gates": {"gate-a": {"cwd": "/path/to/repo", "env": {},
                        "without": ["pytest", "-n", "12"],
                        "with": ["/path/to/coreslot", "run", "--need", "cpu=6", "--",
                                 "pytest", "-n", "12"]}}}

Pair i runs two sets, S(2i-1) and S(2i). With --order wc (the default) the first is without
coreslot and the second with it; with --order abba, even pairs run with coreslot first, so that
neither arm always starts on a machine the other arm has just heated. A set starts every job at
once, samples the load average, the swap in use and, on macOS, the memory pressure level and the
thermal state every 5 s, and appends one JSON record to OUTDIR/results.jsonl with each job's
start, end and exit status, the samples, the cooldown before it, and the run-log lines the set
added. A set refuses to start while the budget shows a hold or a ticket.

Between sets the runner pauses --cooldown seconds (20). With --probe-tolerance F it then also
waits until the thermal state is nominal and a single-core speed probe (a fixed pure-Python
loop, best of three) takes at most (1 + F) times the probe taken before the first set, checking
every 15 s and giving up after --max-cooldown seconds (900); a set that started after giving up
says so in its record.

`alone` runs each job's command without coreslot, one job at a time, RUNS times each in
rotation, inside `coreslot run --need cpu=1..256`, so that the job has the whole budget to
itself and the run log records the cores it used; it appends to OUTDIR/alone.jsonl. A job that
passes every one of its alone runs is stable, and a non-zero exit of a stable job in a set is a
false failure.

`summary` prints the per-set table, both medians of the time the last job ends, their ratio and
the ratio of each pair; then the same for mean completion (the mean of the jobs' end times) with
a one-sided sign test over the pairs; then the failures of each arm and, where OUTDIR/alone.jsonl
gives a baseline, the false failures of each arm with a one-sided Fisher exact test.
"""
import argparse
import json
import math
import os
import re
import statistics
import subprocess
import sys
import time
from datetime import datetime, timezone


def sh(cmd, env):
    r = subprocess.run(cmd, capture_output=True, text=True, env=env)
    return (r.stdout + r.stderr).strip()


def sysctl(name):
    r = subprocess.run(["sysctl", "-n", name], capture_output=True, text=True)
    return r.stdout.strip() if r.returncode == 0 else ""


def thermal():
    """The macOS thermal state (0 nominal, 1 fair, 2 serious, 3 critical), or None."""
    if sys.platform != "darwin":
        return None
    r = subprocess.run(["osascript", "-l", "JavaScript", "-e",
                        'ObjC.import("Foundation"); $.NSProcessInfo.processInfo.thermalState'],
                       capture_output=True, text=True)
    try:
        return int(r.stdout.strip())
    except ValueError:
        return None


def swap_used_mib():
    if sys.platform == "darwin":
        m = re.search(r"used = ([0-9.]+)M", sysctl("vm.swapusage"))
        return float(m.group(1)) if m else None
    try:
        info = dict(l.split(":", 1) for l in open("/proc/meminfo"))
        return (int(info["SwapTotal"].split()[0]) - int(info["SwapFree"].split()[0])) / 1024
    except (OSError, KeyError, ValueError):
        return None


def probe():
    """Seconds a fixed single-core loop takes, best of three: it rises when the CPU throttles."""
    best = None
    for _ in range(3):
        t = time.perf_counter()
        x = 0
        for i in range(3_000_000):
            x += i * i
        d = time.perf_counter() - t
        best = d if best is None else min(best, d)
    return round(best, 4)


def cooldown(args, baseline):
    t0 = time.monotonic()
    time.sleep(args.cooldown)
    p, th = probe(), thermal()

    def hot():
        return th not in (None, 0) or p > baseline * (1 + args.probe_tolerance)

    if args.probe_tolerance is not None:
        while hot() and time.monotonic() - t0 < args.max_cooldown:
            time.sleep(min(15.0, max(0.0, args.max_cooldown - (time.monotonic() - t0))))
            p, th = probe(), thermal()
    return {"cooldown_s": round(time.monotonic() - t0, 1), "probe_s": p, "thermal": th,
            "cooldown_gave_up": args.probe_tolerance is not None and hot()}


def check_empty(cfg, env, label):
    status = sh([cfg["coreslot"], "status"], env)
    if "holds: none" not in status or "queue: empty" not in status:
        sys.exit(f"{label}: the budget is not empty:\n{status}")


def runlog_lines(cfg):
    runlog = os.path.join(cfg["state_dir"], "log", "runs.jsonl")
    return open(runlog).readlines() if os.path.exists(runlog) else []


def run_set(cfg, out, label, mode, extra):
    env0 = dict(os.environ, CORESLOT_STATE_DIR=cfg["state_dir"])
    check_empty(cfg, env0, label)
    nlog0 = len(runlog_lines(cfg))
    setdir = os.path.join(out, "sets", label)
    os.makedirs(setdir)
    rec = {"set": label, "mode": mode,
           "wall_start": datetime.now(timezone.utc).isoformat(timespec="seconds"),
           "loadavg_before": sysctl("vm.loadavg"), **extra, "gates": {}}
    procs = {}
    t0 = time.monotonic()
    for name, g in cfg["gates"].items():
        fo = open(os.path.join(setdir, name + ".out"), "w")
        fe = open(os.path.join(setdir, name + ".err"), "w")
        p = subprocess.Popen(g[mode], cwd=g["cwd"], env=dict(env0, **g.get("env", {})),
                             stdout=fo, stderr=fe, stdin=subprocess.DEVNULL)
        procs[name] = {"p": p, "start": time.monotonic() - t0, "fo": fo, "fe": fe}
    samples, next_sample = [], 0.0
    while True:
        alive = 0
        for d in procs.values():
            if "end" in d:
                continue
            rc = d["p"].poll()
            if rc is None:
                alive += 1
            else:
                d["end"], d["rc"] = time.monotonic() - t0, rc
        el = time.monotonic() - t0
        if alive == 0:
            break
        if el >= next_sample:
            samples.append({"t": round(el, 1), "loadavg": sysctl("vm.loadavg"),
                            "mempress": sysctl("kern.memorystatus_vm_pressure_level"),
                            "swap_used_mib": swap_used_mib(), "thermal": thermal()})
            next_sample += 5.0
        time.sleep(0.1)
    for name, d in procs.items():
        d["fo"].close()
        d["fe"].close()
        rec["gates"][name] = {"argv": cfg["gates"][name][mode], "start_s": round(d["start"], 3),
                              "end_s": round(d["end"], 2), "dur_s": round(d["end"] - d["start"], 2),
                              "exit": d["rc"]}
    rec["set_time_s"] = round(max(g["end_s"] for g in rec["gates"].values()), 2)
    rec["samples"] = samples
    rec["thermal_after"] = thermal()
    rec["runlog_new"] = [json.loads(x) for x in runlog_lines(cfg)[nlog0:]]
    with open(os.path.join(out, "results.jsonl"), "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(json.dumps({"set": label, "set_time_s": rec["set_time_s"],
                      "gates": {n: (g["dur_s"], g["exit"]) for n, g in rec["gates"].items()}}),
          flush=True)


def run(args):
    cfg = json.load(open(args.gates))
    os.makedirs(args.out, exist_ok=True)
    baseline = probe()
    extra = {"probe_baseline_s": baseline, "probe_s": baseline, "thermal": thermal(),
             "cooldown_s": 0.0, "cooldown_gave_up": False}
    for i in range(args.first, args.last + 1):
        first = ("with", "without") if args.order == "abba" and i % 2 == 0 else ("without", "with")
        for n, mode in zip((2 * i - 1, 2 * i), first):
            if n > 2 * args.first - 1:
                extra = {"probe_baseline_s": baseline, **cooldown(args, baseline)}
            run_set(cfg, args.out, f"S{n}-{mode}", mode,
                    {"pair": i, "order": args.order, **extra})


def alone(args):
    cfg = json.load(open(args.gates))
    os.makedirs(os.path.join(args.out, "alone"), exist_ok=True)
    env0 = dict(os.environ, CORESLOT_STATE_DIR=cfg["state_dir"])
    baseline = probe()
    extra = {"probe_baseline_s": baseline, "probe_s": baseline, "thermal": thermal(),
             "cooldown_s": 0.0, "cooldown_gave_up": False}
    for r in range(1, args.runs + 1):
        for k, (name, g) in enumerate(cfg["gates"].items()):
            if r > 1 or k > 0:
                extra = {"probe_baseline_s": baseline, **cooldown(args, baseline)}
            label = f"{name}-{r}"
            check_empty(cfg, env0, label)
            nlog0 = len(runlog_lines(cfg))
            argv = [cfg["coreslot"], "run", "--need", "cpu=1..256", "--label", f"alone:{name}",
                    "--"] + g["without"]
            base = os.path.join(args.out, "alone", label)
            t0 = time.monotonic()
            with open(base + ".out", "w") as fo, open(base + ".err", "w") as fe:
                rc = subprocess.run(argv, cwd=g["cwd"], env=dict(env0, **g.get("env", {})),
                                    stdout=fo, stderr=fe, stdin=subprocess.DEVNULL).returncode
            rec = {"gate": name, "run": r, **extra,
                   "wall_start": datetime.now(timezone.utc).isoformat(timespec="seconds"),
                   "dur_s": round(time.monotonic() - t0, 2), "exit": rc,
                   "runlog_new": [json.loads(x) for x in runlog_lines(cfg)[nlog0:]]}
            with open(os.path.join(args.out, "alone.jsonl"), "a") as f:
                f.write(json.dumps(rec) + "\n")
            print(json.dumps({"alone": label, "dur_s": rec["dur_s"], "exit": rc}), flush=True)


def set_number(label):
    return int(re.match(r"S(\d+)-", label).group(1))


def sign_test(wins, n):
    """One-sided: the chance of at least `wins` of `n` pairs favoring one arm by luck."""
    return sum(math.comb(n, k) for k in range(wins, n + 1)) / 2 ** n


def fisher_greater(x_with, y_without, n_with, n_without):
    """One-sided Fisher exact: the chance of at least `y_without` of the failures falling in the
    without arm, given the total, if both arms failed alike."""
    m = x_with + y_without
    if m == 0:
        return 1.0
    total = math.comb(n_with + n_without, m)
    return sum(math.comb(n_without, k) * math.comb(n_with, m - k)
               for k in range(y_without, min(n_without, m) + 1)) / total


def compare(recs, pairs, value):
    """Medians of both arms and the with / without ratio of each pair, under `value`."""
    w = [value(r) for r in recs if r["mode"] == "with"]
    wo = [value(r) for r in recs if r["mode"] == "without"]
    ratios = [value(p["with"]) / value(p["without"]) for p in pairs]
    return statistics.median(w), statistics.median(wo), ratios


def summary(out):
    recs = [json.loads(x) for x in open(os.path.join(out, "results.jsonl"))]
    recs = [r for r in recs if r["set"].startswith("S")]
    names = list(recs[0]["gates"])
    print(" | ".join(["set"] + names + ["last job ends (s)"]))
    for r in recs:
        print(" | ".join([r["set"]] + [f'{r["gates"][n]["dur_s"]:.1f} ({r["gates"][n]["exit"]})'
                                       for n in names] + [f'{r["set_time_s"]:.1f}']))
    by_pair = {}
    for r in recs:
        by_pair.setdefault((set_number(r["set"]) + 1) // 2, {})[r["mode"]] = r
    pairs = [p for _, p in sorted(by_pair.items()) if len(p) == 2]
    wc = all(set_number(p["without"]["set"]) < set_number(p["with"]["set"]) for p in pairs)
    mw, mwo, ratios = compare(recs, pairs, lambda r: r["set_time_s"])
    print(f"\nmedian with = {mw:.1f} s, median without = {mwo:.1f} s, ratio = {mw / mwo:.3f}")
    which = "with / the preceding without" if wc else "with / without, within the pair"
    print(f"per pair ({which}): " + ", ".join(f"{x:.3f}" for x in ratios))
    print(f"pairs favouring coreslot: {sum(x < 1 for x in ratios)} of {len(ratios)}")

    def mean_end(r):
        return statistics.mean(g["end_s"] for g in r["gates"].values())

    mw, mwo, ratios = compare(recs, pairs, mean_end)
    wins = sum(x < 1 for x in ratios)
    print("\nmean completion (the mean of the jobs' end times):")
    print(f"median with = {mw:.1f} s, median without = {mwo:.1f} s, ratio = {mw / mwo:.3f}")
    print(f"per pair ({which}): " + ", ".join(f"{x:.3f}" for x in ratios))
    print(f"pairs favouring coreslot: {wins} of {len(ratios)}; "
          f"one-sided sign test p = {sign_test(wins, len(ratios)):.4f}")

    runs = {m: [p[m] for p in pairs] for m in ("with", "without")}
    fails = {m: sum(g["exit"] != 0 for r in rs for g in r["gates"].values())
             for m, rs in runs.items()}
    njobs = {m: sum(len(r["gates"]) for r in rs) for m, rs in runs.items()}
    print(f"\nfailures (non-zero exit): with {fails['with']} of {njobs['with']} job runs, "
          f"without {fails['without']} of {njobs['without']}")
    alone_path = os.path.join(out, "alone.jsonl")
    if not os.path.exists(alone_path):
        print("false failures: no alone baseline (alone.jsonl)")
        return
    arecs = [json.loads(x) for x in open(alone_path)]
    print("\nalone | runs | failures | median s | median cores used")
    stable = []
    for n in names:
        a = [x for x in arecs if x["gate"] == n]
        cores = [(l["cpu_user_s"] + l["cpu_sys_s"]) / l["ran_s"] for x in a
                 for l in x["runlog_new"] if l.get("ran_s") and "cpu_user_s" in l]
        nf = sum(x["exit"] != 0 for x in a)
        if a and nf == 0:
            stable.append(n)
        print(f"{n} | {len(a)} | {nf} | "
              + (f"{statistics.median(x['dur_s'] for x in a):.1f}" if a else "-") + " | "
              + (f"{statistics.median(cores):.2f}" if cores else "-"))
    ff = {m: sum(r["gates"][n]["exit"] != 0 for r in rs for n in stable) for m, rs in runs.items()}
    nst = {m: len(rs) * len(stable) for m, rs in runs.items()}
    pf = fisher_greater(ff["with"], ff["without"], nst["with"], nst["without"])
    print(f"false failures (a non-zero exit of a job that passed all its alone runs; stable: "
          f"{', '.join(stable) or 'none'}): with {ff['with']} of {nst['with']}, "
          f"without {ff['without']} of {nst['without']}; one-sided Fisher exact p = {pf:.4f}")


def main():
    ap = argparse.ArgumentParser(usage=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)
    pacing = argparse.ArgumentParser(add_help=False)
    pacing.add_argument("--cooldown", type=float, default=20.0)
    pacing.add_argument("--probe-tolerance", type=float, default=None)
    pacing.add_argument("--max-cooldown", type=float, default=900.0)
    r = sub.add_parser("run", parents=[pacing])
    r.add_argument("gates")
    r.add_argument("out")
    r.add_argument("first", type=int)
    r.add_argument("last", type=int)
    r.add_argument("--order", choices=("wc", "abba"), default="wc")
    a = sub.add_parser("alone", parents=[pacing])
    a.add_argument("gates")
    a.add_argument("out")
    a.add_argument("runs", type=int)
    s = sub.add_parser("summary")
    s.add_argument("out")
    args = ap.parse_args()
    if args.cmd == "run":
        run(args)
    elif args.cmd == "alone":
        alone(args)
    else:
        summary(args.out)


if __name__ == "__main__":
    main()
