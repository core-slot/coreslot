#!/usr/bin/env python3
"""Acceptance run 6: coreslot against running everything at once and a counting semaphore of 1, 2
and 3 slots, under three levels of background memory held by a ballast.

usage:
  run6.py learn STAND OUT
  run6.py verdict STAND OUT PHASE MIX FIRST LAST [--as-block B] [--primer]
  run6.py size OUT
  run6.py summary OUT
  run6.py figures OUT

STAND is run 4's stand file (run4.py's docstring); its optional "light_agents" names the agents of
the light mix (A3 and A4 of the heavy stand). Every coreslot call goes through run4.cs() with the
stand's state directory.

The arms: F runs every gate when its agent reaches it; S1, S2 and S3 hold each heavy gate on one
of 1, 2 or 3 slots of a counting semaphore; K60 and K67 run every gate under coreslot with
cpu = "140%" and mem = "60%" or "67%" and OUT/needs.toml; KH with mem = "60%" and
OUT/needs-hand.toml; KC with mem = "60%" and no needs file, a cold start.

  learn    every gate of the stand alone: 6 runs in its K form (coreslot, cpu = "140%",
           mem = "60%"), then 3 in its F form (its argv with "free_n"), each recorded to
           OUT/learn.jsonl with its peak footprint (the summed physical footprint of the runner's
           descendants, coreslot's meter on macOS, VmRSS on Linux, sampled once a second), cores (their CPU seconds over the run time), run time and exit;
           a K run also with its run-log line's ran_s.
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
           block B's order, level and offsets, exit 2 unless block B has an invalid set. The
           running apps' names go to OUT/apps.local.txt only. --primer first runs one K60 set of the first block,
           recorded to OUT/primer.jsonl only. Appends one record per set to
           OUT/<phase>-<mix>.jsonl, skipping each (block, arm) already recorded, so the same
           command continues an interrupted session.
  size     from the first 12 valid pilot blocks of each mix: sigma-hat heavy, the SD of the
           per-block ln(K*/S*) with K* and S* the per-level oracles (S* among S1-S3 and F, four
           slots), and sigma-hat light, the SD of ln(K60/F); each raised to its one-sided 80 %
           upper bound, then the blocks each mix needs (unclamped too; "underpowered" above the
           cap), and their light F sets with a warn-or-worse sample; to OUT/size.json. Exits 2
           while the pilot lacks 12 valid blocks of a mix, or once a main record exists.
  summary  from OUT/main-*.jsonl, OUT/sens-*.jsonl and OUT/size.json: the first n valid blocks of
           each mix by block number (a block counts when every arm has a valid set; an
           `--as-block` re-run replaces one that has not, up to 10 % of n re-runs), the oracles
           chosen on exactly those blocks, the pre-registered chain H1, H4, H5, H3, H2
           (one-sided, alpha 0.05, exploratory after the first not rejected or untested) and the
           reported estimates; to OUT/summary.json and OUT/summary.txt. Exits 2 while a mix lacks
           n valid blocks and has re-runs left; with them spent, its hypotheses are untested.
  figures  from OUT/summary.json and the main sets of the blocks it used: OUT/f1-timeline.svg
           (the block nearest the median ln K*cf/S*), f3-paired.svg (per-block ln K*cf/S* and
           ln K60/F), f4-pareto-<level>.svg (per ballast level), f5-regret.svg, f6-ecdf.svg
           (time to verdict per agent) and f7-light.svg (light K60/F per block).
"""
import argparse
import ctypes
import glob
import hashlib
import json
import math
import os
import random
import resource
import shutil
import signal
import statistics
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import acceptance  # noqa: E402  (probe)
import figures6  # noqa: E402
import overload  # noqa: E402  (load)
import run4  # noqa: E402  (cs, run_one, run_set, fresh_state, write_config, offsets_for, ...)
import stats6  # noqa: E402
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


def footprint_kib(pid):
    """pid's physical footprint in KiB (macOS, proc_pid_rusage's RUSAGE_INFO_V2
    ri_phys_footprint), else its VmRSS; 0 once it has exited."""
    if sys.platform == "darwin":
        info = (ctypes.c_uint64 * 20)()  # rusage_info_v2: a 16-byte uuid, then 18 counters
        if ctypes.CDLL(None).proc_pid_rusage(pid, 2, info):
            return 0
        return info[9] >> 10  # ri_phys_footprint, the eighth counter
    try:
        with open(f"/proc/{pid}/status") as f:
            return next((int(l.split()[1]) for l in f if l.startswith("VmRSS:")), 0)
    except OSError:
        return 0


def descendants_kib():
    """The summed footprint of every descendant of this process; the ps that lists them has
    exited before the first is read, so it adds 0."""
    return sum(footprint_kib(p) for p in overload.tree_kib(os.getpid()) if p != os.getpid())


def measured(cfg, g, form):
    """run4.run_one with the peak footprint and the cores of what it started."""
    stop, peak = threading.Event(), [0]

    def watch():
        while not stop.is_set():
            peak[0] = max(peak[0], descendants_kib())
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
                log0 = len(acceptance.runlog_lines(cfg))
                code, output, peak, cores, wall = measured(cfg, g, form)
                rec = {"started": started, "gate": g["gate"], "label": g["label"], "form": form,
                       "run": n, "exit": code, "wall_s": wall, "peak_kib": peak, "cores": cores,
                       "failures": run4.fail_lines(g, output) if code else []}
                if form == "K":  # coreslot's own run time, the K slowdown's denominator
                    new = acceptance.runlog_lines(cfg)[log0:]
                    rec["ran_s"] = json.loads(new[-1]).get("ran_s") if new else None
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


def apps_meta(out):
    """The running apps' count and SHA-256 for the records; their names only to apps.local.txt."""
    apps = running_apps()
    digest = hashlib.sha256("\n".join(apps).encode()).hexdigest()
    with open(os.path.join(out, "apps.local.txt"), "a") as f:
        f.write(f"{digest}  {json.dumps(apps)}\n")
    return {"apps_count": len(apps), "apps_sha256": digest}


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
    recs = run4.read_jsonl(path)
    if as_block and not any(r["block"] == as_block and not r["valid"] for r in recs):
        print(f"run6: block {as_block} has no invalid set to re-run", file=sys.stderr)
        sys.exit(2)
    done = {(r["block"], r["arm"]) for r in recs}
    blocks = [(b, [a for a in order_for(phase, mix, as_block or b) if (b, a) not in done])
              for b in range(first, last + 1)]
    blocks = [(b, arms) for b, arms in blocks if arms]
    if not blocks:
        return
    scfg = mix_cfg(cfg, mix)
    apps, static, baseline = apps_meta(out), static_sizes(cfg), acceptance.probe()
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
                        "order": " ".join(order), "level_mib": level, "position": pos,
                        "prev_arm": order[pos - 1] if pos else None,
                        "ballast_rss_mib": ballast.rss_mib(), **apps,
                        "needs_sha256": hashes.get(K_NEEDS.get(arm)),
                        "static": static.get(K_MEM.get(arm))}
                run4.run_set(scfg, dst, arm=arm, offsets=offsets, meta=meta, baseline=baseline,
                             **arm_kw(out, arm))


# --- analysis ---------------------------------------------------------------------------------

ALPHA, MARGIN = 0.05, math.log(1.03)
PILOT_BLOCKS, SENS_BLOCKS = 12, 6
N_HEAVY, N_LIGHT = (30, 120, 3), (12, 60, 1)  # blocks_needed's lo, hi and multiple
S_ARMS, K_SHARES = ("S1", "S2", "S3", "F"), ("K60", "K67")  # F: four slots for four agents
CHAIN = ("H1", "H4", "H5", "H3", "H2")
CORESLOT_EXITS = (64, 70, 75, 126, 127)  # coreslot's own: usage, internal, refusal, cannot run


def records(out, phase, mix):
    return run4.read_jsonl(os.path.join(out, f"{phase}-{mix}.jsonl"))


def gate_lines(r):
    """Each gate with its `coreslot run`'s run-log line, the label's nearest in start time."""
    t0, free, pairs = datetime.fromisoformat(r["started"]).timestamp(), list(r["runlog"]), []
    for g in sorted(r["gates"], key=lambda g: g["start"]):
        mine = [l for l in free if l.get("label") == g.get("label")]
        line = min(mine, key=lambda l: abs(l["time_ns"] / 1e9 - t0 - g["start"]), default=None)
        if line is not None:
            free.remove(line)
        pairs.append((g, line))
    return pairs


def coreslot_failed(r):
    """A K set with a gate that left no run-log line, a fail-open one, or a coreslot exit."""
    return r["arm"].startswith("K") and any(
        line is None or line.get("fail_open") or g["exit"] in CORESLOT_EXITS
        for g, line in gate_lines(r))


def valid_blocks(recs, arms):
    """block -> arm -> set, in block order, for the blocks whose every arm has a valid set. A
    block whose own sets are not all valid takes those of its first valid `--as-block` re-run. A
    K set coreslot failed stays, flagged coreslot_failed, with its block's slowest primary."""
    runs = {}
    for r in recs:
        runs.setdefault(r.get("as_block") or r["block"], {}).setdefault(r["block"], {})[
            r["arm"]] = r
    blocks = {}
    for b in sorted(runs):
        for src in sorted(runs[b], key=lambda x: (x != b, x)):
            sets = runs[b][src]
            if all(a in sets and sets[a]["valid"] for a in arms):
                worst = max(sets[a]["mean_verdict_s"] for a in arms)
                blocks[b] = {a: dict(sets[a], coreslot_failed=True, mean_verdict_s=worst)
                             if coreslot_failed(sets[a]) else sets[a] for a in arms}
                break
    return blocks


def first(blocks, n):
    return dict(list(blocks.items())[:n])


def reruns(recs):
    return sorted({r["block"] for r in recs if r.get("as_block")})


def capped(recs, n):
    """The records without the re-runs past 10 % of n, in block order, and those re-runs."""
    extra = reruns(recs)[n // 10:]
    return [r for r in recs if r["block"] not in extra], extra


def swapout_bytes(r):
    return run4.counter(r, "swapouts") * r.get("page_bytes", 16384)


def worse(r, value):
    """value(r), or infinity for a set coreslot failed: in a sign test, never better."""
    return math.inf if r.get("coreslot_failed") else value(r)


def ln_ratio(sets, a, b):
    return math.log(sets[a]["mean_verdict_s"] / sets[b]["mean_verdict_s"])


def by_level(blocks):
    levels = {}
    for b, sets in blocks.items():
        levels.setdefault(next(iter(sets.values()))["level_mib"], []).append(b)
    return dict(sorted(levels.items()))


def lowest(blocks, bs, arms):
    """The arm with the lowest mean primary over the blocks bs; a tie goes to the earlier arm."""
    return min(arms, key=lambda a: (statistics.fmean(blocks[b][a]["mean_verdict_s"] for b in bs),
                                    arms.index(a)))


def cross_fit(blocks, arms):
    """block -> the arm with the lowest mean over the other half of its level's blocks, the
    halves being the level's blocks in block order taken alternately."""
    chosen = {}
    for bs in by_level(blocks).values():
        halves = (bs[0::2], bs[1::2])
        for i, half in enumerate(halves):
            other = halves[1 - i]
            for b in half:
                chosen[b] = lowest(blocks, other, arms) if other else arms[0]
    return chosen


def per_level(blocks, arms):
    return {lvl: lowest(blocks, bs, arms) for lvl, bs in by_level(blocks).items()}


def sign(below, above):
    """The sign test of pairs where the first value is below the second; ties dropped."""
    w = sum(x < y for x, y in zip(below, above))
    lo = sum(x > y for x, y in zip(below, above))
    return {"wins": w, "losses": lo, "p": run4.sign_test(w, w + lo)}


def finite(x):
    return None if x is None or math.isinf(x) or math.isnan(x) else round(x, 4)


def expo(x):
    """exp(x) for a log ratio; None for an infinite or missing bound."""
    return finite(math.exp(x)) if x < 700 else None


def estimate(d):
    """A ratio from log differences: its geometric mean, one-sided 95 % upper bound and BCa 90 %
    interval."""
    if not d:
        return None
    up = stats6.paired_upper(d, 0.0)
    lo, hi = stats6.bca(d)
    return {"n": len(d), "gmr": expo(up["mean"]), "upper": expo(up["upper"]),
            "bca90": [expo(lo), expo(hi)]}


def ci(xs):
    """Per-set values' median and mean, and the BCa 90 % interval of the mean."""
    xs = list(xs)
    lo, hi = stats6.bca(xs)
    return {"median": statistics.median(xs), "mean": finite(statistics.fmean(xs)),
            "bca90": [finite(lo), finite(hi)]}


def size(out):
    if glob.glob(os.path.join(out, "main-*.jsonl")):
        print("run6: size would choose n after main records exist", file=sys.stderr)
        sys.exit(2)
    heavy = first(valid_blocks(records(out, "pilot", "heavy"), HEAVY_ARMS), PILOT_BLOCKS)
    light = first(valid_blocks(records(out, "pilot", "light"), LIGHT_ARMS), PILOT_BLOCKS)
    if len(heavy) < PILOT_BLOCKS or len(light) < PILOT_BLOCKS:
        print(f"run6: the pilot has {len(heavy)} heavy and {len(light)} light valid blocks, not"
              f" {PILOT_BLOCKS} of each", file=sys.stderr)
        sys.exit(2)
    k, s = per_level(heavy, K_SHARES), per_level(heavy, S_ARMS)
    lvl = {b: lv for lv, bs in by_level(heavy).items() for b in bs}
    sd = {"heavy": statistics.stdev(ln_ratio(heavy[b], k[lvl[b]], s[lvl[b]]) for b in heavy),
          "light": statistics.stdev(ln_ratio(light[b], "K60", "F") for b in light)}
    res = {"underpowered": False}
    for mix, (lo, hi, multiple) in (("heavy", N_HEAVY), ("light", N_LIGHT)):
        sigma = stats6.sigma_upper(sd[mix], PILOT_BLOCKS)
        raw = stats6.blocks_needed(sigma, 0, math.inf, multiple)
        res.update({f"sigma_hat_{mix}": sd[mix], f"sigma_{mix}": sigma,
                    f"n_{mix}_unclamped": raw, f"n_{mix}": min(max(raw, lo), hi)})
        res["underpowered"] |= raw > hi
    res["light_pressure_sets"] = sum(run4.warn_s(light[b]["F"]) > 0 for b in light)
    with open(os.path.join(out, "size.json"), "w") as f:
        json.dump(res, f, indent=1)
    print(json.dumps(res, indent=1))


def heavy_tests(heavy, s_star, kcf):
    """H1, H3, H4 and H5 over the heavy blocks."""
    hb = list(heavy)
    lvl = {b: lv for lv, bs in by_level(heavy).items() for b in bs}
    sstar = {b: s_star[lvl[b]] for b in hb}

    def warn(arm_of, bs):
        return [worse(heavy[b][arm_of(b)], run4.warn_s) for b in bs]

    def swap(arm_of, bs):
        return [worse(heavy[b][arm_of(b)], swapout_bytes) for b in bs]

    def arm(a):
        return lambda b: a

    d1 = [ln_ratio(heavy[b], "K60", "F") for b in hb]
    h1 = {"blocks": hb, "t": stats6.paired_upper(d1, 0), "estimate": estimate(d1),
          "warn": sign(warn(arm("K60"), hb), warn(arm("F"), hb)),
          "swapout": sign(swap(arm("K60"), hb), swap(arm("F"), hb))}
    h1["reject"] = all(h1[k]["p"] < ALPHA for k in ("t", "warn", "swapout"))
    d3 = {b: math.log(heavy[b][kcf[b]]["mean_verdict_s"] / heavy[b][sstar[b]]["mean_verdict_s"])
          for b in hb}
    h3 = {"blocks": hb, "t": stats6.paired_upper(list(d3.values()), MARGIN),
          "estimate": estimate(list(d3.values())), "per_level": {}}
    for lv, bs in by_level(heavy).items():
        t = stats6.paired_upper([d3[b] for b in bs], MARGIN)
        h3["per_level"][lv] = {"gmr": expo(t["mean"]), "upper": expo(t["upper"]),
                               "flagged": t["upper"] > MARGIN}
    h3["reject"] = h3["t"]["p"] < ALPHA
    h4 = {"blocks": hb, "warn": sign(warn(kcf.get, hb), warn(sstar.get, hb)),
          "swapout": sign(swap(kcf.get, hb), swap(sstar.get, hb))}
    h4["reject"] = h4["warn"]["p"] < ALPHA and h4["swapout"]["p"] < ALPHA
    h5 = {"blocks": hb, "alpha": ALPHA / 6, "per_n": {}}
    for n in (1, 2, 3):
        rows = []
        for lv, bs in by_level(heavy).items():
            t = stats6.paired_lower([ln_ratio(heavy[b], f"S{n}", "K60") for b in bs], MARGIN)
            rows.append({"level": lv, "test": "time", "gmr": expo(t["mean"]),
                         "lower": expo(t["lower"]), "p": t["p"]})
            rows.append({"level": lv, "test": "warn",
                         **sign(warn(arm("K60"), bs), warn(arm(f"S{n}"), bs))})
        h5["per_n"][n] = {"tests": rows, "any": any(r["p"] < ALPHA / 6 for r in rows)}
    h5["reject"] = all(v["any"] for v in h5["per_n"].values())
    return {"H1": h1, "H3": h3, "H4": h4, "H5": h5}


def tests(heavy, light, s_star, kcf, light_pressure_sets):
    """The pre-registered chain, each test one-sided at ALPHA, a mix given as None untested; it
    stops at the first hypothesis not rejected or untested, and the rest are exploratory."""
    hs = heavy_tests(heavy, s_star, kcf) if heavy else {}
    if light:
        d = [ln_ratio(light[b], "K60", "F") for b in light]
        h2 = hs["H2"] = {"blocks": list(light), "t": stats6.paired_upper(d, MARGIN),
                         "estimate": estimate(d)}
        h2["reject"] = h2["t"]["p"] < ALPHA
        if light_pressure_sets > 1:
            h2["untested"] = "more than one light pilot F set under pressure"
    chain, going = {}, True
    for name in CHAIN:
        h = chain[name] = hs.get(name, {"untested": "its mix is short of n, re-runs spent"})
        if not going:
            h["status"] = "exploratory"
        elif "untested" in h:
            h["status"], going = "untested", False
        else:
            h["status"], going = ("rejected" if h["reject"] else "not rejected"), h["reject"]
        if "t" in h:
            h["t"] = {k: finite(v) for k, v in h["t"].items()}
    return chain


def alone_medians(out):
    """(gate, form) -> the median alone run time: the runner's wall time for F, the run log's
    ran_s for K, which drops its first, cold run."""
    runs = {}
    for r in run4.read_jsonl(os.path.join(out, "learn.jsonl")):
        t = r.get("ran_s") if r["form"] == "K" else r["wall_s"]
        if not (r["form"] == "K" and r["run"] == 1) and t is not None:
            runs.setdefault((r["gate"], r["form"]), []).append(t)
    return {k: statistics.median(v) for k, v in runs.items()}


def counted_gates(out):
    """The gates that passed all three of their F-form alone runs."""
    exits = {}
    for r in run4.read_jsonl(os.path.join(out, "learn.jsonl")):
        if r["form"] == "F":
            exits.setdefault(r["gate"], []).append(r["exit"])
    return {g for g, e in exits.items() if len(e) == LEARN_RUNS["F"] and not any(e)}


def failures(runs):
    k = sum(1 for g in runs if g["exit"])
    return {"k": k, "n": len(runs), "ci95": [finite(x) for x in stats6.clopper_pearson(
        k, len(runs))] if runs else None}


def per_arm(blocks, arms, out):
    """Per arm; a slowdown is a gate's execution, after the semaphore or (ran_s) admission,
    over its form's alone median."""
    alone, counted = alone_medians(out), counted_gates(out)
    res = {}
    for a in arms:
        rs = [blocks[b][a] for b in blocks]
        if not rs:
            continue
        runs = [g for r in rs for g in r["gates"] if g["gate"] in counted]
        slow = {}
        for r in rs:
            mine = {}
            for g, line in gate_lines(r):
                base = alone.get((g["gate"], "K" if a.startswith("K") else "F"))
                ran = (line or {}).get("ran_s") if a.startswith("K") else g["end"] - g["start"]
                if base and ran is not None:
                    mine.setdefault(g["gate"], []).append(ran / base)
            for g, v in mine.items():
                slow.setdefault(g, []).append(statistics.fmean(v))
        res[a] = {"mean_verdict_s": round(statistics.fmean(r["mean_verdict_s"] for r in rs), 1),
                  "makespan_s": statistics.median(r["makespan_s"] for r in rs),
                  "warn_s": statistics.median(run4.warn_s(r) for r in rs),
                  "critical_s": ci(run4.critical_s(r) for r in rs),
                  "swapout_bytes": ci(swapout_bytes(r) for r in rs),
                  "swapins": ci(run4.counter(r, "swapins") for r in rs),
                  "pageins": ci(run4.counter(r, "pageins") for r in rs),
                  "false_failures": dict(failures(runs), by_gate={
                      g: failures([x for x in runs if x["gate"] == g])
                      for g in sorted({x["gate"] for x in runs})}),
                  "slowdown": {g: ci(v) for g, v in slow.items()}}
    return res


def order_effects(blocks):
    """d of a set, ln of its primary over its block's geometric mean, by its position and by the
    arm before it."""
    pos, prev = {}, {}
    for sets in blocks.values():
        gm = statistics.fmean(math.log(r["mean_verdict_s"]) for r in sets.values())
        for r in sets.values():
            d = math.log(r["mean_verdict_s"]) - gm
            pos.setdefault(r["position"], []).append(d)
            prev.setdefault(r["prev_arm"] or "none", []).append(d)
    return {"position": {k: ci(v) for k, v in sorted(pos.items())},
            "prev_arm": {k: ci(v) for k, v in sorted(prev.items())}}


def sensitivity(out):
    spread = first(valid_blocks(records(out, "sens", "spread"), SPREAD_ARMS), SENS_BLOCKS)
    cold = first(valid_blocks(records(out, "sens", "cold"), COLD_ARMS), SENS_BLOCKS)
    means = {a: round(statistics.fmean(spread[b][a]["mean_verdict_s"] for b in spread), 1)
             for a in SPREAD_ARMS} if spread else {}
    return {"spread": {"blocks": list(spread), "means": means,
                       "to_K60": {a: estimate([ln_ratio(spread[b], a, "K60") for b in spread])
                                  for a in SPREAD_ARMS if a != "K60"}},
            "cold": {"blocks": list(cold),
                     "KC/K60": estimate([ln_ratio(cold[b], "KC", "K60") for b in cold])}}


def reported(heavy, light, s_star, kcf, out):
    lvl = {b: lv for lv, bs in by_level(heavy).items() for b in bs}
    sstar = {b: s_star[lvl[b]] for b in heavy}
    k_star = per_level(heavy, K_SHARES)
    scf = cross_fit(heavy, S_ARMS)

    def ratio(blocks, num, den, key="mean_verdict_s"):
        return estimate([math.log(blocks[b][num(b)][key] / blocks[b][den(b)][key])
                         for b in blocks])

    def arm(a):
        return lambda b: a
    return {"per_arm": {"heavy": per_arm(heavy, HEAVY_ARMS, out),
                        "light": per_arm(light, LIGHT_ARMS, out)},
            "makespan": {"K60/F heavy": ratio(heavy, arm("K60"), arm("F"), "makespan_s"),
                         "K60/F light": ratio(light, arm("K60"), arm("F"), "makespan_s"),
                         "K*cf/S*": ratio(heavy, kcf.get, sstar.get, "makespan_s")},
            "K60/S*": ratio(heavy, arm("K60"), sstar.get),
            "KH/K60": ratio(heavy, arm("KH"), arm("K60")),
            "K*/S*": ratio(heavy, lambda b: k_star[lvl[b]], sstar.get),
            "K*cf/Scf": ratio(heavy, kcf.get, scf.get),
            "cross_fit_s": scf,
            "regret": {lv: {a: estimate([ln_ratio(heavy[b], a, s_star[lv]) for b in bs])
                            for a in S_ARMS} for lv, bs in by_level(heavy).items()},
            "order": order_effects(heavy),
            "sensitivity": sensitivity(out)}


def summary_text(s):
    lines = ["Acceptance run 6: summary", "",
             f"Blocks used: heavy {s['blocks']['heavy']}", f"  light {s['blocks']['light']}",
             f"Re-runs: {s['reruns']}; ignored past 10 % of n: {s['reruns_ignored']}",
             f"Underpowered (the formula's n above the cap): {s['size'].get('underpowered')}",
             f"Sets coreslot failed, each primary the block's slowest: {s['coreslot_failed']}",
             f"S* per level (MiB -> arm): {s['oracles']['S']}",
             f"K*cf per block: {s['oracles']['Kcf']}", ""]
    if s["incomplete"]:
        lines += [s["incomplete"], ""]
    for name, h in s["tests"].items():
        ps = {k: h[k]["p"] for k in ("t", "warn", "swapout") if k in h}
        lines.append(f"{name}: {h['status']}; p {ps or h.get('per_n')}")
    lines += ["", "Reported, untested:", json.dumps(s["reported"], indent=1), ""]
    return "\n".join(lines)


def summary(out):
    path = os.path.join(out, "size.json")
    if not os.path.exists(path):
        print(f"run6: {path} is missing; run size first", file=sys.stderr)
        sys.exit(2)
    with open(path) as f:
        sz = json.load(f)
    mixes = {"heavy": (HEAVY_ARMS, sz["n_heavy"]), "light": (LIGHT_ARMS, sz["n_light"])}
    used, spent, ignored, full = {}, {}, {}, {}
    for mix, (arms, n) in mixes.items():
        recs, ignored[mix] = capped(records(out, "main", mix), n)
        blocks = valid_blocks(recs, arms)
        used[mix] = first(blocks, n)
        spent[mix], full[mix] = reruns(recs), len(blocks) >= n
        if not full[mix] and len(spent[mix]) < n // 10:
            print(f"run6: main {mix}: {len(blocks)} of {n} valid blocks, {len(spent[mix])} of"
                  f" {n // 10} re-runs; no summary yet", file=sys.stderr)
            sys.exit(2)
    heavy, light = used["heavy"], used["light"]
    s_star, kcf = per_level(heavy, S_ARMS), cross_fit(heavy, K_SHARES)
    short = [m for m in full if not full[m]]
    s = {"size": sz, "blocks": {m: list(b) for m, b in used.items()}, "reruns": spent,
         "reruns_ignored": ignored,
         "coreslot_failed": {m: [f"{b} {a}" for b, sets in bs.items() for a, r in sets.items()
                                 if r.get("coreslot_failed")] for m, bs in used.items()},
         "oracles": {"S": s_star, "Kcf": kcf},
         "tests": tests(heavy if full["heavy"] else None, light if full["light"] else None,
                        s_star, kcf, sz["light_pressure_sets"]),
         "incomplete": f"incomplete: {' and '.join(short)} short of n with the re-runs spent;"
                       " its hypotheses untested" if short else None,
         "reported": reported(heavy, light, s_star, kcf, out)}
    with open(os.path.join(out, "summary.json"), "w") as f:
        json.dump(s, f, indent=1)
    text = summary_text(s)
    with open(os.path.join(out, "summary.txt"), "w") as f:
        f.write(text)
    print(text)


def figures(out):
    path = os.path.join(out, "summary.json")
    if not os.path.exists(path):
        print(f"run6: {path} is missing; run summary first", file=sys.stderr)
        sys.exit(2)
    with open(path) as f:
        s = json.load(f)
    used = {}
    for mix, arms in (("heavy", HEAVY_ARMS), ("light", LIGHT_ARMS)):
        recs, _ = capped(records(out, "main", mix), s["size"][f"n_{mix}"])
        blocks = valid_blocks(recs, arms)
        used[mix] = {b: blocks[b] for b in s["blocks"][mix]}
    names, mid = figures6.write(out, s, used["heavy"], used["light"])
    print(f"wrote {', '.join(names)} (timeline: block {mid})")


def main():
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("size", "summary", "figures"):
        sub.add_parser(name).add_argument("out")
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
    if args.cmd == "size":
        return size(args.out)
    if args.cmd == "summary":
        return summary(args.out)
    if args.cmd == "figures":
        return figures(args.out)
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
