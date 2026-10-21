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
  size     from OUT/pilot-*.jsonl only: sigma-hat heavy, the SD of the per-block ln(K*/S*) with K*
           and S* the per-level oracles over the pilot's valid blocks, and sigma-hat light, the SD
           of ln(K60/F); each raised to its one-sided 80 % upper bound, then the blocks each mix
           needs, and the light F sets with a warn-or-worse sample; to OUT/size.json. Exits 2 while
           the pilot has fewer than 12 valid blocks of either mix.
  summary  from OUT/main-*.jsonl, OUT/sens-*.jsonl and OUT/size.json: the first n valid blocks of
           each mix by block number (a block counts when every arm has a valid set; an
           `--as-block` re-run replaces its block), the oracles chosen on exactly those blocks,
           the pre-registered chain H1-H5 (one-sided, alpha 0.05, exploratory after the first
           non-rejection) and the reported estimates; to OUT/summary.json and OUT/summary.txt.
           Exits 2 while a mix lacks n valid blocks and has re-runs left (10 % of n); with them
           spent, the summary is incomplete: descriptive numbers and no test.
  figures  from OUT/summary.json and the main sets of the blocks it used: OUT/f1-timeline.svg
           (the block nearest the median ln K*cf/S*), f3-paired.svg (per-block ln K*cf/S* and
           ln K60/F), f4-pareto-<level>.svg (per ballast level), f5-regret.svg, f6-ecdf.svg
           (time to verdict per agent) and f7-light.svg (light K60/F per block).
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


# --- analysis ---------------------------------------------------------------------------------

ALPHA, MARGIN = 0.05, math.log(1.03)
PILOT_BLOCKS = 12
N_HEAVY, N_LIGHT = (30, 120, 3), (12, 60, 1)  # blocks_needed's lo, hi and multiple
S_ARMS, K_SHARES = ("S1", "S2", "S3"), ("K60", "K67")


def records(out, phase, mix):
    return run4.read_jsonl(os.path.join(out, f"{phase}-{mix}.jsonl"))


def valid_blocks(recs, arms):
    """block -> arm -> set, in block order, for the blocks whose every arm has a valid set. An
    `--as-block B` re-run counts as block B and replaces its sets: a block's sets are those of its
    latest valid re-run, else its own when valid."""
    runs = {}
    for r in recs:
        runs.setdefault(r.get("as_block") or r["block"], {}).setdefault(r["block"], {})[
            r["arm"]] = r
    blocks = {}
    for b in sorted(runs):
        for src in sorted(runs[b], key=lambda x: (x != b, x), reverse=True):
            sets = runs[b][src]
            if all(a in sets and sets[a]["valid"] for a in arms):
                blocks[b] = sets
                break
    return blocks


def reruns(recs):
    return sorted({r["block"] for r in recs if r.get("as_block")})


def counter(r, key):
    """The rise of a sampled counter over the set."""
    c = [s[key] for s in r["samples"] if s.get(key) is not None]
    return c[-1] - c[0] if len(c) > 1 else 0


def swapout_bytes(r):
    return counter(r, "swapouts") * r.get("page_bytes", 16384)


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
    return {"wins": w, "losses": lo, "p": stats6.sign_test(w, lo)}


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


def size(out):
    heavy = valid_blocks(records(out, "pilot", "heavy"), HEAVY_ARMS)
    light = valid_blocks(records(out, "pilot", "light"), LIGHT_ARMS)
    if len(heavy) < PILOT_BLOCKS or len(light) < PILOT_BLOCKS:
        print(f"run6: the pilot has {len(heavy)} heavy and {len(light)} light valid blocks, not"
              f" {PILOT_BLOCKS} of each", file=sys.stderr)
        sys.exit(2)
    k, s = per_level(heavy, K_SHARES), per_level(heavy, S_ARMS)
    lvl = {b: lv for lv, bs in by_level(heavy).items() for b in bs}
    sd_heavy = statistics.stdev(ln_ratio(heavy[b], k[lvl[b]], s[lvl[b]]) for b in heavy)
    sd_light = statistics.stdev(ln_ratio(light[b], "K60", "F") for b in light)
    sig_heavy = stats6.sigma_upper(sd_heavy, len(heavy))
    sig_light = stats6.sigma_upper(sd_light, len(light))
    res = {"sigma_hat_heavy": sd_heavy, "sigma_heavy": sig_heavy,
           "n_heavy": stats6.blocks_needed(sig_heavy, *N_HEAVY),
           "sigma_hat_light": sd_light, "sigma_light": sig_light,
           "n_light": stats6.blocks_needed(sig_light, *N_LIGHT),
           "light_pressure_sets": sum(1 for r in records(out, "pilot", "light")
                                      if r["arm"] == "F" and run4.warn_s(r) > 0)}
    with open(os.path.join(out, "size.json"), "w") as f:
        json.dump(res, f, indent=1)
    print(json.dumps(res, indent=1))


def tests(heavy, light, s_star, kcf, light_pressure_sets):
    """The pre-registered chain, each test one-sided at ALPHA; after the first non-rejection the
    rest are exploratory."""
    hb, lb = list(heavy), list(light)
    lvl = {b: lv for lv, bs in by_level(heavy).items() for b in bs}
    sstar = {b: f"S{s_star[lvl[b]]}" for b in hb}

    def warn(arm_of, bs, blocks):
        return [run4.warn_s(blocks[b][arm_of(b)]) for b in bs]

    def swap(arm_of, bs, blocks):
        return [swapout_bytes(blocks[b][arm_of(b)]) for b in bs]

    def arm(a):
        return lambda b: a

    h1 = {"blocks": hb, "t": stats6.paired_upper([ln_ratio(heavy[b], "K60", "F") for b in hb], 0),
          "warn": sign(warn(arm("K60"), hb, heavy), warn(arm("F"), hb, heavy)),
          "swapout": sign(swap(arm("K60"), hb, heavy), swap(arm("F"), hb, heavy))}
    h1["reject"] = all(h1[k]["p"] < ALPHA for k in ("t", "warn", "swapout"))
    h2 = {"blocks": lb,
          "t": stats6.paired_upper([ln_ratio(light[b], "K60", "F") for b in lb], MARGIN),
          "exploratory": light_pressure_sets > 1}
    h2["reject"] = h2["t"]["p"] < ALPHA
    d3 = {b: math.log(heavy[b][kcf[b]]["mean_verdict_s"] / heavy[b][sstar[b]]["mean_verdict_s"])
          for b in hb}
    h3 = {"blocks": hb, "t": stats6.paired_upper(list(d3.values()), MARGIN), "per_level": {}}
    for lv, bs in by_level(heavy).items():
        t = stats6.paired_upper([d3[b] for b in bs], MARGIN)
        h3["per_level"][lv] = {"gmr": expo(t["mean"]), "upper": expo(t["upper"]),
                               "flagged": t["upper"] > MARGIN}
    h3["reject"] = h3["t"]["p"] < ALPHA
    h4 = {"blocks": hb, "warn": sign(warn(kcf.get, hb, heavy), warn(sstar.get, hb, heavy)),
          "swapout": sign(swap(kcf.get, hb, heavy), swap(sstar.get, hb, heavy))}
    h4["reject"] = h4["warn"]["p"] < ALPHA and h4["swapout"]["p"] < ALPHA
    h5 = {"blocks": hb, "alpha": ALPHA / 6, "per_n": {}}
    for n in (1, 2, 3):
        ps = []
        for lv, bs in by_level(heavy).items():
            ps.append(stats6.paired_lower([ln_ratio(heavy[b], f"S{n}", "K60") for b in bs],
                                          MARGIN)["p"])
            ps.append(sign(warn(arm("K60"), bs, heavy), warn(arm(f"S{n}"), bs, heavy))["p"])
        h5["per_n"][n] = {"p": ps, "any": any(p < ALPHA / 6 for p in ps)}
    h5["reject"] = all(v["any"] for v in h5["per_n"].values())
    chain, going = {"H1": h1, "H2": h2, "H3": h3, "H4": h4, "H5": h5}, True
    for h in chain.values():
        if not going or h.pop("exploratory", False):
            h["status"], going = "exploratory", False
        else:
            h["status"], going = ("rejected" if h["reject"] else "not rejected"), h["reject"]
    for h in chain.values():
        if "t" in h:
            h["t"] = {k: finite(v) for k, v in h["t"].items()}
    return chain


def alone_medians(out):
    """(gate, form) -> the median alone run time; the K form drops its first, cold run."""
    runs = {}
    for r in run4.read_jsonl(os.path.join(out, "learn.jsonl")):
        if not (r["form"] == "K" and r["run"] == 1):
            runs.setdefault((r["gate"], r["form"]), []).append(r["wall_s"])
    return {k: statistics.median(v) for k, v in runs.items()}


def counted_gates(out):
    """The gates that passed all three of their F-form alone runs."""
    exits = {}
    for r in run4.read_jsonl(os.path.join(out, "learn.jsonl")):
        if r["form"] == "F":
            exits.setdefault(r["gate"], []).append(r["exit"])
    return {g for g, e in exits.items() if len(e) == LEARN_RUNS["F"] and not any(e)}


def per_arm(blocks, arms, out):
    alone, counted = alone_medians(out), counted_gates(out)
    res = {}
    for a in arms:
        rs = [blocks[b][a] for b in blocks]
        if not rs:
            continue
        runs = [g for r in rs for g in r["gates"] if g["gate"] in counted]
        fails = sum(1 for g in runs if g["exit"])
        slow = {}
        for r in rs:
            for g in r["gates"]:
                base = alone.get((g["gate"], "K" if a.startswith("K") else "F"))
                if base:
                    slow.setdefault(g["gate"], []).append((g["end"] - g["start"]) / base)
        res[a] = {"mean_verdict_s": round(statistics.fmean(r["mean_verdict_s"] for r in rs), 1),
                  "makespan_s": statistics.median(r["makespan_s"] for r in rs),
                  "warn_s": statistics.median(run4.warn_s(r) for r in rs),
                  "critical_s": statistics.median(run4.critical_s(r) for r in rs),
                  "critical_s_total": sum(run4.critical_s(r) for r in rs),
                  "swapout_bytes": statistics.median(swapout_bytes(r) for r in rs),
                  "swapins": statistics.median(counter(r, "swapins") for r in rs),
                  "pageins": statistics.median(counter(r, "pageins") for r in rs),
                  "false_failures": {"k": fails, "n": len(runs), "ci95": [
                      finite(x) for x in stats6.clopper_pearson(fails, len(runs))]
                      if runs else None},
                  "slowdown": {g: round(statistics.median(v), 3) for g, v in slow.items()}}
    return res


def order_effects(blocks):
    """The mean d of a set, ln of its primary over its block's geometric mean, by its position
    and by the arm before it."""
    pos, prev = {}, {}
    for sets in blocks.values():
        gm = statistics.fmean(math.log(r["mean_verdict_s"]) for r in sets.values())
        for r in sets.values():
            d = math.log(r["mean_verdict_s"]) - gm
            pos.setdefault(r["position"], []).append(d)
            prev.setdefault(r["prev_arm"] or "none", []).append(d)
    return {"position": {k: round(statistics.fmean(v), 4) for k, v in sorted(pos.items())},
            "prev_arm": {k: round(statistics.fmean(v), 4) for k, v in sorted(prev.items())}}


def sensitivity(out):
    spread = valid_blocks(records(out, "sens", "spread"), SPREAD_ARMS)
    cold = valid_blocks(records(out, "sens", "cold"), COLD_ARMS)
    means = {a: round(statistics.fmean(spread[b][a]["mean_verdict_s"] for b in spread), 1)
             for a in SPREAD_ARMS} if spread else {}
    return {"spread": {"blocks": list(spread), "means": means,
                       "to_K60": {a: estimate([ln_ratio(spread[b], a, "K60") for b in spread])
                                  for a in SPREAD_ARMS if a != "K60"}},
            "cold": {"blocks": list(cold),
                     "KC/K60": estimate([ln_ratio(cold[b], "KC", "K60") for b in cold])}}


def reported(heavy, light, s_star, kcf, out):
    lvl = {b: lv for lv, bs in by_level(heavy).items() for b in bs}
    sstar = {b: f"S{s_star[lvl[b]]}" for b in heavy}
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
            "regret": {lv: {n: estimate([ln_ratio(heavy[b], f"S{n}", f"S{s_star[lv]}")
                                         for b in bs]) for n in (1, 2, 3)}
                       for lv, bs in by_level(heavy).items()},
            "order": order_effects(heavy),
            "sensitivity": sensitivity(out)}


def summary_text(s):
    lines = ["Acceptance run 6: summary", "",
             f"Blocks used: heavy {s['blocks']['heavy']}", f"  light {s['blocks']['light']}",
             f"Re-runs: {s['reruns']}", f"S* per level (MiB -> N): {s['oracles']['S']}",
             f"K*cf per block: {s['oracles']['Kcf']}", ""]
    if s["incomplete"]:
        lines += [s["incomplete"], ""]
    else:
        for name, h in s["tests"].items():
            ps = {k: h[k]["p"] for k in ("t", "warn", "swapout") if k in h}
            lines.append(f"{name}: {h['status']}; p {ps or h.get('per_n')}")
        lines.append("")
    lines += ["Reported, untested:", json.dumps(s["reported"], indent=1), ""]
    return "\n".join(lines)


def summary(out):
    path = os.path.join(out, "size.json")
    if not os.path.exists(path):
        print(f"run6: {path} is missing; run size first", file=sys.stderr)
        sys.exit(2)
    with open(path) as f:
        sz = json.load(f)
    mixes = {"heavy": (HEAVY_ARMS, sz["n_heavy"]), "light": (LIGHT_ARMS, sz["n_light"])}
    used, spent, full = {}, {}, {}
    for mix, (arms, n) in mixes.items():
        recs = records(out, "main", mix)
        blocks = valid_blocks(recs, arms)
        used[mix] = {b: blocks[b] for b in list(blocks)[:n]}
        spent[mix], full[mix] = reruns(recs), len(blocks) >= n
        if not full[mix] and len(spent[mix]) < n // 10:
            print(f"run6: main {mix}: {len(blocks)} of {n} valid blocks, {len(spent[mix])} of"
                  f" {n // 10} re-runs; no summary yet", file=sys.stderr)
            sys.exit(2)
    heavy, light = used["heavy"], used["light"]
    s_star = {lv: int(a[1:]) for lv, a in per_level(heavy, S_ARMS).items()}
    kcf = cross_fit(heavy, K_SHARES)
    s = {"size": sz, "blocks": {m: list(b) for m, b in used.items()}, "reruns": spent,
         "oracles": {"S": s_star, "Kcf": kcf}, "tests": None, "incomplete": None,
         "reported": reported(heavy, light, s_star, kcf, out)}
    if all(full.values()):
        s["tests"] = tests(heavy, light, s_star, kcf, sz["light_pressure_sets"])
    else:
        s["incomplete"] = (f"incomplete: {len(heavy)} heavy and {len(light)} light valid blocks"
                           " with the re-runs spent; no test")
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
        blocks = valid_blocks(records(out, "main", mix), arms)
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
