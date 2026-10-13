#!/usr/bin/env python3
"""Product acceptance: the same jobs started at one instant, with and without coreslot, in
alternating pairs.

usage:
  acceptance.py run GATES.json OUTDIR FIRST_PAIR LAST_PAIR
  acceptance.py summary OUTDIR

GATES.json names the coreslot binary, a state directory of its own (CORESLOT_STATE_DIR, so the
run is a separate budget with its own run log), and the jobs. Each job has a working directory,
extra environment, the command without coreslot and the command with it:

  {"coreslot": "/path/to/coreslot", "state_dir": "/path/to/state",
   "gates": {"gate-a": {"cwd": "/path/to/repo", "env": {},
                        "without": ["pytest", "-n", "12"],
                        "with": ["/path/to/coreslot", "run", "--need", "cpu=6", "--",
                                 "pytest", "-n", "12"]}}}

Pair i runs set S(2i-1) without coreslot, then set S(2i) with it, 20 s apart. A set starts every
job at once, samples the load average and (on macOS) the memory pressure level every 5 s, and
appends one JSON record to OUTDIR/results.jsonl with each job's start, end and exit status, the
samples, and the run-log lines the set added. A set refuses to start while the budget shows a
hold or a ticket. `summary` prints the per-set table, both medians, their ratio and the ratio of
each pair.
"""
import json
import os
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


def run_set(cfg, out, label, mode):
    env0 = dict(os.environ, CORESLOT_STATE_DIR=cfg["state_dir"])
    status = sh([cfg["coreslot"], "status"], env0)
    if "holds: none" not in status or "queue: empty" not in status:
        sys.exit(f"{label}: the budget is not empty:\n{status}")
    runlog = os.path.join(cfg["state_dir"], "log", "runs.jsonl")
    nlog0 = len(open(runlog).readlines()) if os.path.exists(runlog) else 0
    setdir = os.path.join(out, "sets", label)
    os.makedirs(setdir)
    rec = {"set": label, "mode": mode,
           "wall_start": datetime.now(timezone.utc).isoformat(timespec="seconds"),
           "loadavg_before": sysctl("vm.loadavg"), "gates": {}}
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
                            "mempress": sysctl("kern.memorystatus_vm_pressure_level")})
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
    rec["runlog_new"] = ([json.loads(x) for x in open(runlog).readlines()[nlog0:]]
                         if os.path.exists(runlog) else [])
    with open(os.path.join(out, "results.jsonl"), "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(json.dumps({"set": label, "set_time_s": rec["set_time_s"],
                      "gates": {n: (g["dur_s"], g["exit"]) for n, g in rec["gates"].items()}}),
          flush=True)


def run(gates_json, out, first, last):
    cfg = json.load(open(gates_json))
    os.makedirs(out, exist_ok=True)
    for i in range(first, last + 1):
        for mode, n in (("without", 2 * i - 1), ("with", 2 * i)):
            run_set(cfg, out, f"S{n}-{mode}", mode)
            time.sleep(20)


def summary(out):
    recs = [json.loads(x) for x in open(os.path.join(out, "results.jsonl"))]
    recs = [r for r in recs if r["set"].startswith("S")]
    names = list(recs[0]["gates"])
    print(" | ".join(["set"] + names + ["last job ends (s)"]))
    for r in recs:
        print(" | ".join([r["set"]] + [f'{r["gates"][n]["dur_s"]:.1f} ({r["gates"][n]["exit"]})'
                                       for n in names] + [f'{r["set_time_s"]:.1f}']))
    w = [r["set_time_s"] for r in recs if r["mode"] == "with"]
    wo = [r["set_time_s"] for r in recs if r["mode"] == "without"]
    mw, mwo = statistics.median(w), statistics.median(wo)
    print(f"\nmedian with = {mw:.1f} s, median without = {mwo:.1f} s, ratio = {mw / mwo:.3f}")
    ratios = [b["set_time_s"] / a["set_time_s"] for a, b in zip(recs[0::2], recs[1::2])]
    print("per pair (with / the preceding without): " + ", ".join(f"{x:.3f}" for x in ratios))
    print(f"pairs favouring coreslot: {sum(x < 1 for x in ratios)} of {len(ratios)}")


if __name__ == "__main__":
    if len(sys.argv) == 6 and sys.argv[1] == "run":
        run(sys.argv[2], sys.argv[3], int(sys.argv[4]), int(sys.argv[5]))
    elif len(sys.argv) == 3 and sys.argv[1] == "summary":
        summary(sys.argv[2])
    else:
        sys.exit(__doc__)
