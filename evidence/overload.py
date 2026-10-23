#!/usr/bin/env python3
"""Acceptance run 3: agents whose gates overload memory and CPU, under three policies.

usage:
  overload.py calibrate STAND.json OUTDIR RUNS
  overload.py run STAND.json OUTDIR FIRST_BLOCK LAST_BLOCK [--seed S] [--cooldown S]
                  [--probe-tolerance F] [--max-cooldown S]
  overload.py summary OUTDIR

STAND.json names the coreslot binary, a state directory of its own, the memory budget and the
agents. Each agent is a chain of gates run one after another; a gate has a working directory,
an argv, whether it is heavy, and, when its argv holds the argument "{n}", the value "{n}" takes
without coreslot (its own default width, such as "auto"):

  {"coreslot": "coreslot", "state_dir": "/path/state", "cpu": 10, "mem_gib": 9, "window_s": 15,
   "agents": {"A1": [{"gate": "tsc", "cwd": "/path/vscode", "heavy": true,
                      "argv": ["npm", "run", "typecheck-client"]}],
              "A2": [{"gate": "lint", "cwd": "/path/django", "heavy": false, "argv": ["flake8"]},
                     {"gate": "tests", "cwd": "/path/django", "heavy": true, "free_n": "auto",
                      "argv": ["python", "tests/runtests.py", "--parallel", "{n}"]}]}}

The policies (arms):
  F  free: every gate starts when its agent reaches it.
  S  semaphore: heavy gates take one of two slots first (a counting semaphore, as the do-it-
     yourself tools of claude-code#101193 do); light gates run free.
  K  coreslot: every gate runs through `coreslot run` with the needs `calibrate` derived.

`calibrate` runs every gate alone RUNS times without coreslot, sampling once a second the
resident memory of the gate's whole process tree, and takes its CPU seconds from rusage. It
writes OUTDIR/calibration.jsonl and OUTDIR/needs.json by a rule fixed here: memory
ceil(1.2 x the peak tree footprint in GiB); cpu ceil(1.2 x the cores used), as a range 2..that
for a gate with "{n}" and a fixed count otherwise. (coreslot's own run log records the largest
single process, not the tree, so it would under-declare a gate of many workers.) A gate that
passes every alone run is stable; a stable gate that fails in a set fails falsely.

`run` runs blocks FIRST..LAST. A block runs F, S and K once each, in an order drawn from the
block's seed; inside a block the agents arrive at offsets drawn once from U(0, window_s), the
same for its three sets. Between sets it waits --cooldown seconds and, with --probe-tolerance,
until the thermal state is nominal and a single-core probe is within tolerance of its first
reading (acceptance.py's cooldown). Each set samples memory pressure, swap and swapouts once a
second and appends one record to OUTDIR/results.jsonl.

`summary` prints per arm the median over blocks of the mean time to verdict (an agent's chain
end minus its arrival), the makespan and the seconds at warn or critical memory pressure, the
per-block ratios of K against F and against S with one-sided sign tests, and false failures.
"""
import argparse
import json
import math
import os
import random
import re
import statistics
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import acceptance  # noqa: E402  (thermal, probe, swap_used_mib, cooldown, sign_test)

ARMS = ("F", "S", "K")


def load(path):
    cfg = json.load(open(path))
    cfg["coreslot"] = os.path.expanduser(cfg["coreslot"])
    cfg["state_dir"] = os.path.expanduser(cfg["state_dir"])
    for chain in cfg["agents"].values():
        for g in chain:
            g["cwd"] = os.path.expanduser(g["cwd"])
    return cfg


def vm_counters(*names):
    """The named vm_stat counters from one vm_stat call, each None when missing."""
    r = subprocess.run(["vm_stat"], capture_output=True, text=True)
    ms = [re.search(rf"^{name}:\s+(\d+)", r.stdout, re.M) for name in names]
    return [int(m.group(1)) if m else None for m in ms]



def available_mib():
    """Memory a new process can take without reclaiming active pages: free, inactive,
    speculative and purgeable pages (macOS vm_stat)."""
    r = subprocess.run(["vm_stat"], capture_output=True, text=True)
    m = re.search(r"page size of (\d+) bytes", r.stdout)
    page = int(m.group(1)) if m else 16384
    pages = sum(int(m) for m in re.findall(
        r"^Pages (?:free|inactive|speculative|purgeable):\s+(\d+)", r.stdout, re.M))
    return pages * page // 2**20


def pressure():
    v = acceptance.sysctl("kern.memorystatus_vm_pressure_level")
    return int(v) if v.isdigit() else None


HEADROOM_PAGES = ("vm.page_free_count", "vm.page_speculative_count", "vm.page_purgeable_count",
                  "vm.page_pageable_external_count")


def headroom_mib():
    """The memory the kernel can hand out without paging anything anonymous out, read as
    coreslot reads it on macOS: free, speculative, purgeable and file-backed pageable pages
    times vm.pagesize; None when a sysctl is missing."""
    r = subprocess.run(["sysctl", "-n", "vm.pagesize", *HEADROOM_PAGES], capture_output=True,
                       text=True)
    vals = r.stdout.split()
    if r.returncode or len(vals) != 1 + len(HEADROOM_PAGES) or not all(v.isdigit() for v in vals):
        return None
    return sum(int(v) for v in vals[1:]) * int(vals[0]) >> 20


def tree_kib(root):
    """pid -> resident KiB of root and each of its descendants, from one ps listing."""
    r = subprocess.run(["ps", "-A", "-o", "pid=,ppid=,rss="], capture_output=True, text=True)
    kids, rss = {}, {}
    for line in r.stdout.splitlines():
        p = line.split()
        if len(p) == 3:
            pid, ppid, kib = int(p[0]), int(p[1]), int(p[2])
            kids.setdefault(ppid, []).append(pid)
            rss[pid] = kib
    tree, todo = {}, [root]
    while todo:
        pid = todo.pop()
        tree[pid] = rss.get(pid, 0)
        todo.extend(kids.get(pid, []))
    return tree


def tree_rss_kib(root):
    return sum(tree_kib(root).values())


class Sampler(threading.Thread):
    """Once a second: memory pressure, swap in use, the swapouts, swapins and pageins counters,
    the 1-min load and the headroom; every fifth second also the thermal state (0 nominal ..
    3 critical)."""

    def __init__(self):
        super().__init__(daemon=True)
        self.samples, self.stop = [], threading.Event()

    def run(self):
        i = 0
        while not self.stop.is_set():
            swapouts, swapins, pageins = vm_counters("Swapouts", "Swapins", "Pageins")
            s = {"t": round(time.time(), 1), "pressure": pressure(),
                 "swap_mib": acceptance.swap_used_mib(), "swapouts": swapouts,
                 "swapins": swapins, "pageins": pageins,
                 "load1": round(os.getloadavg()[0], 2), "headroom_mib": headroom_mib()}
            if i % 5 == 0:
                s["thermal"] = acceptance.thermal()
            self.samples.append(s)
            i += 1
            self.stop.wait(1.0)


def argv_for(cfg, needs, gate, arm, label):
    argv = [gate.get("free_n", "auto") if a == "{n}" and arm != "K" else a for a in gate["argv"]]
    if arm != "K":
        return argv
    nd = needs[gate["gate"]]
    return [cfg["coreslot"], "run", "--label", label, "--need", f"cpu={nd['cpu']}",
            "--need", f"mem={nd['mem']}", "--"] + argv


def calibrate(args):
    cfg = load(args.stand)
    os.makedirs(args.out, exist_ok=True)
    gates = list({g["gate"]: g for chain in cfg["agents"].values() for g in chain}.values())
    available_gib = available_mib() // 1024
    recs = []
    for i in range(args.runs):
        for g in gates:
            argv = argv_for(cfg, {}, g, "F", g["gate"])
            t0 = time.time()
            p = subprocess.Popen(argv, cwd=g["cwd"], env=dict(os.environ, **g.get("env", {})),
                                 stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            peak = 0
            while True:  # wait4 gives this child's own rusage (its waited descendants included)
                pid, status, ru = os.wait4(p.pid, os.WNOHANG)
                if pid == p.pid:
                    break
                peak = max(peak, tree_rss_kib(p.pid))
                time.sleep(1.0)
            p.returncode = os.waitstatus_to_exitcode(status)
            wall = time.time() - t0
            cpu = ru.ru_utime + ru.ru_stime
            # ru_maxrss is in bytes on macOS, KiB on Linux: the largest single process
            maxrss_gib = ru.ru_maxrss / (2**30 if sys.platform == "darwin" else 2**20)
            rec = {"gate": g["gate"], "run": i + 1, "exit": p.returncode, "wall_s": round(wall, 1),
                   "cores_used": round(cpu / wall, 2), "peak_tree_gib": round(peak / 2**20, 2),
                   "max_rss_gib": round(maxrss_gib, 2)}
            recs.append(rec)
            print(json.dumps(rec), flush=True)
            with open(os.path.join(args.out, "calibration.jsonl"), "a") as f:
                f.write(json.dumps(rec) + "\n")
    needs = {"_budget": {"cpu": cfg["cpu"], "mem": available_gib, "rule":
                         "cpu: the logical CPUs; mem: free + inactive + speculative + purgeable"
                         " memory before calibration, floored to GiB"}}
    for g in gates:
        rs = [r for r in recs if r["gate"] == g["gate"]]
        used = max(r["cores_used"] for r in rs)
        # the 1 s tree sample can miss a short burst; the largest process's own peak cannot
        peak = max(max(r["peak_tree_gib"], r["max_rss_gib"]) for r in rs)
        mem = max(1, math.ceil(1.2 * peak))
        top = min(cfg["cpu"], max(1, math.ceil(1.2 * used)))
        cpu = f"{min(2, top)}..{top}" if "{n}" in g["argv"] else str(top)
        needs[g["gate"]] = {"cpu": cpu, "mem": mem,
                            "stable": all(r["exit"] == 0 for r in rs)}
    json.dump(needs, open(os.path.join(args.out, "needs.json"), "w"), indent=1)
    print(json.dumps(needs, indent=1))


def run_set(cfg, needs, out, block, arm, offsets):
    env0 = dict(os.environ, CORESLOT_STATE_DIR=cfg["state_dir"])
    if arm == "K":
        acceptance.check_empty(cfg, env0, f"B{block}-{arm}")
    runlog0 = len(acceptance.runlog_lines(cfg))
    slots = threading.Semaphore(2)
    gates, lock = [], threading.Lock()
    t0 = time.time()

    def agent(name, chain):
        time.sleep(offsets[name])
        for g in chain:
            label = f"B{block}-{arm}-{name}-{g['gate']}"
            argv = argv_for(cfg, needs, g, arm, label)
            held = arm == "S" and g["heavy"]
            if held:
                slots.acquire()
            s = time.time()
            r = subprocess.run(argv, cwd=g["cwd"], env=dict(env0, **g.get("env", {})),
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
            e = time.time()
            if held:
                slots.release()
            with lock:
                gates.append({"agent": name, "gate": g["gate"], "start": round(s - t0, 1),
                              "end": round(e - t0, 1), "exit": r.returncode,
                              "stderr_tail": r.stderr[-400:] if r.returncode else ""})

    sampler = Sampler()
    sampler.start()
    threads = [threading.Thread(target=agent, args=(n, c)) for n, c in cfg["agents"].items()]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    sampler.stop.set()
    sampler.join()
    verdict = {n: round(max(g["end"] for g in gates if g["agent"] == n) - offsets[n], 1)
               for n in cfg["agents"]}
    rec = {"block": block, "arm": arm, "started": datetime.fromtimestamp(t0, timezone.utc).isoformat(),
           "offsets": offsets, "gates": sorted(gates, key=lambda g: (g["agent"], g["start"])),
           "verdict_s": verdict, "mean_verdict_s": round(statistics.mean(verdict.values()), 1),
           "makespan_s": round(max(g["end"] for g in gates), 1), "samples": sampler.samples,
           "runlog": acceptance.runlog_lines(cfg)[runlog0:]}
    with open(os.path.join(out, "results.jsonl"), "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(f"B{block} {arm}: mean verdict {rec['mean_verdict_s']} s, makespan {rec['makespan_s']} s,"
          f" warn+ {sum((s['pressure'] or 1) >= 2 for s in sampler.samples)} s,"
          f" failed {[g['gate'] for g in gates if g['exit']]}", flush=True)


def run(args):
    cfg = load(args.stand)
    needs = json.load(open(os.path.join(args.out, "needs.json")))
    os.makedirs(cfg["state_dir"], exist_ok=True)
    with open(os.path.join(cfg["state_dir"], "config.toml"), "w") as f:
        f.write("[resources]\ncpu = {cpu}\nmem = {mem}\n".format(**needs["_budget"]))
    baseline = acceptance.probe()
    for b in range(args.first, args.last + 1):
        rnd = random.Random(f"{args.seed}-{b}")
        order = list(ARMS)
        rnd.shuffle(order)
        offsets = {n: round(rnd.uniform(0, cfg["window_s"]), 1) for n in cfg["agents"]}
        for arm in order:
            cd = acceptance.cooldown(args, baseline)
            print(f"B{b} {arm}: cooldown {cd}", flush=True)
            run_set(cfg, needs, args.out, b, arm, offsets)


def summary(out):
    recs = [json.loads(l) for l in open(os.path.join(out, "results.jsonl"))]
    needs = json.load(open(os.path.join(out, "needs.json")))
    by = {a: {r["block"]: r for r in recs if r["arm"] == a} for a in ARMS}
    blocks = sorted(set(by["F"]) & set(by["S"]) & set(by["K"]))

    def warn(r):
        return sum((s["pressure"] or 1) >= 2 for s in r["samples"])

    def swapouts(r):
        c = [s["swapouts"] for s in r["samples"] if s["swapouts"] is not None]
        return (c[-1] - c[0]) if len(c) > 1 else 0

    def critical(r):
        return sum((s["pressure"] or 1) >= 4 for s in r["samples"])

    def hottest(r):
        return max((s["thermal"] for s in r["samples"] if s.get("thermal") is not None), default=None)

    print(f"blocks: {blocks}")
    print("medians over blocks; critical(s) and thermal are the maximum over blocks")
    print("arm  mean-verdict(s)  makespan(s)  warn+(s)  critical(s)  swapouts  thermal"
          "  false-failures")
    for a in ARMS:
        rs = [by[a][b] for b in blocks]
        ff = sum(1 for r in rs for g in r["gates"] if g["exit"] and needs[g["gate"]]["stable"])
        print(f"{a}    {statistics.median(r['mean_verdict_s'] for r in rs):>14}"
              f"  {statistics.median(r['makespan_s'] for r in rs):>11}"
              f"  {statistics.median(warn(r) for r in rs):>8}"
              f"  {max(critical(r) for r in rs):>11}"
              f"  {statistics.median(swapouts(r) for r in rs):>8}"
              f"  {max((hottest(r) or 0) for r in rs):>7}  {ff:>14}")
    for other in ("F", "S"):
        for key in ("mean_verdict_s", "makespan_s"):
            ratios = [round(by["K"][b][key] / by[other][b][key], 3) for b in blocks]
            wins = sum(x < 1 for x in ratios)
            print(f"K/{other} {key}: per block {ratios}; K better in {wins} of {len(ratios)};"
                  f" one-sided sign test p = {acceptance.sign_test(wins, len(ratios)):.4f}")


def main():
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    c = sub.add_parser("calibrate")
    c.add_argument("stand")
    c.add_argument("out")
    c.add_argument("runs", type=int)
    r = sub.add_parser("run")
    r.add_argument("stand")
    r.add_argument("out")
    r.add_argument("first", type=int)
    r.add_argument("last", type=int)
    r.add_argument("--seed", default="run3")
    r.add_argument("--cooldown", type=float, default=20)
    r.add_argument("--probe-tolerance", type=float, default=None)
    r.add_argument("--max-cooldown", type=float, default=60)
    s = sub.add_parser("summary")
    s.add_argument("out")
    args = ap.parse_args()
    {"calibrate": calibrate, "run": run, "summary": lambda a: summary(a.out)}[args.cmd](args)


if __name__ == "__main__":
    main()
