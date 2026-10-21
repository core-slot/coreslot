#!/usr/bin/env python3
"""Acceptance run 6: coreslot against running everything at once and a counting semaphore of 1, 2
and 3 slots, under three levels of background memory held by a ballast.

usage:
  run6.py learn STAND OUT
  run6.py verdict STAND OUT PHASE MIX FIRST LAST [--as-block B] [--primer]

STAND is run 4's stand file (run4.py's docstring); its optional "light_agents" names the agents of
the light mix (A3 and A4 of the heavy stand). Every coreslot call goes through run4.cs() with the
stand's state directory.

The arms: F runs every gate when its agent reaches it; S1, S2 and S3 hold each heavy gate on one
of 1, 2 or 3 slots of a counting semaphore; K60 and K67 run every gate under coreslot with
cpu = "140%" and mem = "60%" or "67%" and OUT/needs.toml; KH with mem = "60%" and
OUT/needs-hand.toml; KC with mem = "60%" and no needs file, a cold start.

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
  verdict  blocks FIRST..LAST of PHASE (pilot, main or sens) and MIX: heavy (F, S1, S2, S3, K60,
           K67, KH) or light (K60, F, on the light agents) with pilot or main; spread (F, S1, S2,
           S3, K60, arrivals U(0, 30 s)) or cold (K60, KC) with sens. First the needs files must
           match OUT/needs.sha256, else exit 2. Each block runs its arms in the order order_for
           gives it, with a ballast held for the whole block at level_for's level (heavy; 0 MiB
           otherwise) and offsets from the seed run6-<phase>-<mix>-<block>; with --as-block B,
           block B's order, level and offsets. --primer first runs one K60 set of the first block,
           recorded to OUT/primer.jsonl only. Appends one record per set to
           OUT/<phase>-<mix>.jsonl, skipping each (block, arm) already recorded, so the same
           command continues an interrupted session.
"""
import argparse
import glob
import json
import math
import os
import random
import resource
import shutil
import signal
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import acceptance  # noqa: E402  (probe)
import overload  # noqa: E402  (load)
import run4  # noqa: E402  (cs, run_one, run_set, fresh_state, write_config, offsets_for, ...)
from ballast import Ballast  # noqa: E402

LEARN_SHARE = {"cpu": "140%", "mem": "60%"}
LEARN_RUNS = {"K": 6, "F": 3}
RECORDS = ("pilot-*.jsonl", "main-*.jsonl", "sens-*.jsonl")
NEEDS_FILES = ("needs.toml", "needs-hand.toml")
HEAVY_ARMS = ("F", "S1", "S2", "S3", "K60", "K67", "KH")
LIGHT_ARMS = ("K60", "F")
SPREAD_ARMS = ("F", "S1", "S2", "S3", "K60")
COLD_ARMS = ("K60", "KC")
MIXES = {"heavy": HEAVY_ARMS, "light": LIGHT_ARMS, "spread": SPREAD_ARMS, "cold": COLD_ARMS}
LEVELS_MIB = (0, 1536, 3072)
LIGHT_AGENTS = ("A3", "A4")
SPREAD_WINDOW_S = 30
K_MEM = {"K60": "60%", "K67": "67%", "KH": "60%", "KC": "60%"}
K_NEEDS = {"K60": "needs.toml", "K67": "needs.toml", "KH": "needs-hand.toml", "KC": None}


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


# --- verdict ----------------------------------------------------------------------------------

def level_for(phase, block):
    """The ballast MiB of a heavy block: every group of three blocks holds each level once."""
    levels = list(LEVELS_MIB)
    random.Random(f"run6-level-{phase}-{(block - 1) // 3}").shuffle(levels)
    return levels[(block - 1) % 3]


def order_for(phase, mix, block):
    arms = list(MIXES[mix])
    random.Random(f"run6-order-{phase}-{mix}-{block}").shuffle(arms)
    return arms


def arm_kw(out, arm):
    """run4.run_set's keyword arguments for arm."""
    if arm.startswith("S"):
        return {"slots": int(arm[1:])}
    if arm in K_MEM:
        name = K_NEEDS[arm]
        return {"kshare": {"cpu": LEARN_SHARE["cpu"], "mem": K_MEM[arm]},
                "needs": os.path.join(out, name) if name else None}
    return {}


def check_needs(out):
    """Exits 2 unless every file OUT/needs.sha256 names has the SHA-256 it records."""
    path = os.path.join(out, "needs.sha256")
    if not os.path.exists(path):
        print(f"run6: {path} is missing; run learn first", file=sys.stderr)
        sys.exit(2)
    for line in open(path):
        want, name = line.split()
        f = os.path.join(out, name)
        if not os.path.exists(f) or run4.sha256(f) != want:
            print(f"run6: {f} does not match needs.sha256", file=sys.stderr)
            sys.exit(2)


def running_apps():
    """The base names of the running executables under /Applications/."""
    r = subprocess.run(["ps", "-axo", "comm="], capture_output=True, text=True)
    return sorted({os.path.basename(c.strip()) for c in r.stdout.splitlines()
                   if c.startswith("/Applications/")})


def static_sizes(cfg):
    """mem share -> the cpu and mem sizes `coreslot status --json` reports with it, in tokens."""
    os.makedirs(run4.checked_state(cfg), mode=0o700, exist_ok=True)
    sizes = {}
    for mem in sorted(set(K_MEM.values())):
        run4.write_config(cfg, LEARN_SHARE["cpu"], mem)
        r = run4.cs(cfg, "status", "--json", capture_output=True, text=True)
        if r.returncode:
            sys.exit(f"run6: status --json exited {r.returncode}")
        res = json.loads(r.stdout)["resources"]
        sizes[mem] = {"cpu": res["cpu"]["size"], "mem": res["mem"]["size"]}
    return sizes


def mix_cfg(cfg, mix):
    if mix == "light":
        names = cfg.get("light_agents", LIGHT_AGENTS)
        return dict(cfg, agents={n: cfg["agents"][n] for n in names})
    if mix == "spread":
        return dict(cfg, window_s=SPREAD_WINDOW_S)
    return cfg


def verdict(cfg, out, phase, mix, first, last, as_block=None, primer=False):
    if (phase == "sens") != (mix in ("spread", "cold")):
        print(f"run6: mix {mix} does not belong to phase {phase}", file=sys.stderr)
        sys.exit(2)
    check_needs(out)
    path = os.path.join(out, f"{phase}-{mix}.jsonl")
    done = {(r["block"], r["arm"]) for r in run4.read_jsonl(path)}
    blocks = [(b, [a for a in order_for(phase, mix, as_block or b) if (b, a) not in done])
              for b in range(first, last + 1)]
    blocks = [(b, arms) for b, arms in blocks if arms]
    if not blocks:
        return
    scfg = mix_cfg(cfg, mix)
    apps, static, baseline = running_apps(), static_sizes(cfg), acceptance.probe()
    hashes = {name: run4.sha256(os.path.join(out, name)) for name in NEEDS_FILES}
    for i, (b, pending) in enumerate(blocks):
        src = as_block or b
        order = order_for(phase, mix, src)
        level = level_for(phase, src) if mix == "heavy" else 0
        offsets = run4.offsets_for(scfg, f"run6-{phase}-{mix}-{src}")
        sets = [("K60", None, os.path.join(out, "primer.jsonl"))] if primer and i == 0 else []
        sets += [(arm, order.index(arm), path) for arm in pending]
        with Ballast(level) as ballast:
            for arm, pos, dst in sets:
                meta = {"phase": phase, "mix": mix, "block": b, "as_block": as_block,
                        "order": " ".join(order), "config": arm, "level_mib": level,
                        "position": pos, "prev_arm": order[pos - 1] if pos else None,
                        "ballast_rss_mib": ballast.rss_mib(), "apps": apps,
                        "needs_sha256": hashes.get(K_NEEDS.get(arm)),
                        "static": static.get(K_MEM.get(arm))}
                run4.run_set(scfg, dst, arm=arm, offsets=offsets, meta=meta, baseline=baseline,
                             **arm_kw(out, arm))


def main():
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("learn", "verdict"):
        p = sub.add_parser(name)
        p.add_argument("stand")
        p.add_argument("out")
        if name == "verdict":
            p.add_argument("phase", choices=("pilot", "main", "sens"))
            p.add_argument("mix", choices=tuple(MIXES))
            p.add_argument("first", type=int)
            p.add_argument("last", type=int)
            p.add_argument("--as-block", type=int, default=None)
            p.add_argument("--primer", action="store_true")
    args = ap.parse_args()
    cfg = overload.load(args.stand)

    def interrupted(*_):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    if args.cmd == "learn":
        learn(cfg, args.out)
    else:
        verdict(cfg, args.out, args.phase, args.mix, args.first, args.last, args.as_block,
                args.primer)


if __name__ == "__main__":
    main()
