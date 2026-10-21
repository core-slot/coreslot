#!/usr/bin/env python3
"""Acceptance run 6: coreslot against running everything at once and a counting semaphore of 1, 2
and 3 slots, under three levels of background memory held by a ballast.

usage:
  run6.py learn STAND OUT

STAND is run 4's stand file (run4.py's docstring). Every coreslot call goes through run4.cs()
with the stand's state directory.

  learn    every gate of the stand alone: 6 runs in its K form (coreslot, cpu = "140%",
           mem = "60%"), then 3 in its F form (its argv with "free_n"), each recorded to
           OUT/learn.jsonl with its peak footprint (the runner's descendants' resident memory,
           sampled once a second), cores (their CPU seconds over the run time), run time and exit.
           Then `coreslot suggest --write`, whose window of the last 5 runs per label drops the
           first, cold K run, copied to OUT/needs.toml; OUT/needs-hand.toml, one pinned entry per
           gate from its first F-form run (hand_entry); OUT/needs.sha256, both files' SHA-256.
           The first call starts a fresh state directory and records it in OUT/learn-state.json;
           a later call reuses it, never resetting it, and makes only the runs learn.jsonl lacks.
           Refuses with exit 2 once any pilot, main or sensitivity record exists.
"""
import argparse
import glob
import json
import math
import os
import resource
import shutil
import signal
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import overload  # noqa: E402  (load)
import run4  # noqa: E402  (cs, run_one, fresh_state, write_config, unique_gates, ...)

LEARN_SHARE = {"cpu": "140%", "mem": "60%"}
LEARN_RUNS = {"K": 6, "F": 3}
RECORDS = ("pilot-*.jsonl", "main-*.jsonl", "sens-*.jsonl")
NEEDS_FILES = ("needs.toml", "needs-hand.toml")


# --- learn ------------------------------------------------------------------------------------

def hand_entry(peak_kib, cores):
    """A needs entry by hand from one F-form run: the peak footprint rounded up to a whole GiB,
    the cores rounded up, pinned so `suggest --write` keeps it."""
    return {"mem": f"{max(1, math.ceil(peak_kib / 2**20))}G",
            "cpu": str(max(1, math.ceil(cores))), "pinned": True}


def descendants_rss_kib():
    """Resident KiB of every descendant of this process, from one ps listing."""
    r = subprocess.run(["ps", "-A", "-o", "pid=,ppid=,rss="], capture_output=True, text=True)
    kids, rss = {}, {}
    for line in r.stdout.splitlines():
        p = line.split()
        if len(p) == 3:
            kids.setdefault(int(p[1]), []).append(int(p[0]))
            rss[int(p[0])] = int(p[2])
    total, todo = 0, list(kids.get(os.getpid(), []))
    while todo:
        pid = todo.pop()
        total += rss.get(pid, 0)
        todo.extend(kids.get(pid, []))
    return total


def measured(cfg, g, form):
    """run4.run_one with the peak footprint and the cores of what it started."""
    stop, peak = threading.Event(), [0]

    def watch():
        while not stop.is_set():
            peak[0] = max(peak[0], descendants_rss_kib())
            stop.wait(1.0)

    ru0, t0 = resource.getrusage(resource.RUSAGE_CHILDREN), time.time()
    watcher = threading.Thread(target=watch, daemon=True)
    watcher.start()
    try:
        code, out = run4.run_one(cfg, g, form)
    finally:
        stop.set()
        watcher.join()
    wall, ru1 = time.time() - t0, resource.getrusage(resource.RUSAGE_CHILDREN)
    cpu = ru1.ru_utime + ru1.ru_stime - ru0.ru_utime - ru0.ru_stime
    return code, out, peak[0], round(cpu / wall, 2) if wall > 0 else 0.0, round(wall, 1)


def write_learn_state(cfg, out):
    with open(os.path.join(out, "learn-state.json"), "w") as f:
        json.dump({"state_dir": run4.scrub(cfg["state_dir"])}, f)


def seal(out):
    """OUT/needs.sha256: the SHA-256 of each needs file, as sha256sum writes it."""
    with open(os.path.join(out, "needs.sha256"), "w") as f:
        for name in NEEDS_FILES:
            f.write(f"{run4.sha256(os.path.join(out, name))}  {name}\n")


def write_hand(path, entries):
    with open(path, "w") as f:
        f.write("[needs]\n")
        for label, e in entries.items():
            f.write(f'  [needs."{label}"]\n    cpu = "{e["cpu"]}"\n    mem = "{e["mem"]}"\n'
                    f'    pinned = {str(e["pinned"]).lower()}\n')


def learn(cfg, out):
    for pattern in RECORDS:
        if glob.glob(os.path.join(out, pattern)):
            print(f"run6: learn would change the needs {pattern} used", file=sys.stderr)
            sys.exit(2)
    os.makedirs(out, exist_ok=True)
    state = os.path.join(out, "learn-state.json")
    if os.path.exists(state):
        recorded = json.load(open(state))["state_dir"]
        if recorded != run4.scrub(cfg["state_dir"]):
            sys.exit(f"run6: learn started in {recorded}, not in this stand's state directory")
    else:
        run4.fresh_state(cfg)
        write_learn_state(cfg, out)
    run4.write_config(cfg, LEARN_SHARE["cpu"], LEARN_SHARE["mem"])
    path = os.path.join(out, "learn.jsonl")
    done = {(r["gate"], r["form"], r["run"]) for r in run4.read_jsonl(path)}
    for g in run4.unique_gates(cfg):
        for form, runs in LEARN_RUNS.items():
            for n in range(1, runs + 1):
                if (g["gate"], form, n) in done:
                    continue
                started = datetime.now(timezone.utc).isoformat(timespec="seconds")
                code, output, peak, cores, wall = measured(cfg, g, form)
                rec = {"started": started, "gate": g["gate"], "label": g["label"], "form": form,
                       "run": n, "exit": code, "wall_s": wall, "peak_kib": peak, "cores": cores,
                       "failures": run4.fail_lines(g, output) if code else []}
                with open(path, "a") as f:
                    f.write(json.dumps(rec) + "\n")
                print(json.dumps(rec), flush=True)
    r = run4.cs(cfg, "suggest", "--write", capture_output=True, text=True)
    print(run4.scrub(r.stdout + r.stderr), end="", flush=True)
    if r.returncode:
        sys.exit(f"run6: suggest --write exited {r.returncode}")
    shutil.copyfile(os.path.join(run4.checked_state(cfg), "needs.toml"),
                    os.path.join(out, "needs.toml"))
    first = {}
    for r in run4.read_jsonl(path):
        if r["form"] == "F" and r["run"] == 1:
            first[r["label"]] = hand_entry(r["peak_kib"], r["cores"])
    write_hand(os.path.join(out, "needs-hand.toml"),
               {g["label"]: first[g["label"]] for g in run4.unique_gates(cfg)})
    seal(out)


def main():
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("learn")
    p.add_argument("stand")
    p.add_argument("out")
    args = ap.parse_args()
    cfg = overload.load(args.stand)

    def interrupted(*_):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    learn(cfg, args.out)


if __name__ == "__main__":
    main()
