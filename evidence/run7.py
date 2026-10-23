#!/usr/bin/env python3
"""Acceptance run 7: coreslot against fixed slot counts (a machine-wide flock and `sem -j2`) and
GNU parallel's --memsuspend, at a quiet and a loaded level of memory headroom.

usage:
  run7.py learn STAND OUT
  run7.py verdict STAND OUT PHASE MIX FIRST LAST [--as-block B] [--primer]
  run7.py pilot-check OUT
  run7.py summary OUT

STAND is overload/stand7.json: run 4's stand with FastAPI's tests under `--dist loadgroup`, and
"wide_agents", the agents of the wide mix. Every coreslot call goes through run4.cs() with the
stand's state directory; GNU parallel runs with PARALLEL_HOME=<state_dir>/../parallel-home.

The arms: K60 runs every gate under coreslot with cpu = "140%", mem = "60%" and OUT/needs.toml;
S1 and S2 hold each heavy gate on one of 1 or 2 slots of a counting semaphore, S1 a machine-wide
flock; P runs each heavy gate through GNU sem with --memsuspend 9G and -j 4; F runs every gate
when its agent reaches it. The mixes: heavy (K60, S1, S2, P on the stand's agents, at the quiet
and the loaded level), light (K60, F on A3 and A4), wide (K60, S2, F on the wide agents) and
spread (K60, S2, arrivals U(0, 30 s)), the last three at the quiet level only.

  learn    every gate of the agents and the wide agents alone, each run after a cooldown of at
           least 20 s: 6 runs in its K form (coreslot, cpu = "140%", mem = "60%"), then 3 in its
           F form (its argv with "free_n"); then `coreslot suggest --write`, copied to
           OUT/needs-pass1.toml. Then the relearn pass: 5 K-form runs per gate under that entry,
           so the window of the last 5 runs per label holds only runs at the granted widths, and
           suggest --write again, copied to OUT/needs.toml. Each run is recorded to
           OUT/learn.jsonl as run 6's learn records it, with "pass": "learn" or "relearn";
           OUT/needs.sha256 holds the SHA-256 of needs.toml and learn.jsonl.
           The first call starts a fresh state directory and records it in OUT/learn-state.json;
           a later call reuses it, never resetting it, and makes only the runs learn.jsonl lacks
           by (gate, form, pass, run). Once a relearn record exists, pass 1 and its suggest are
           never run again. Refuses with exit 2 once any pilot, main or sensitivity record exists.
  verdict  blocks FIRST..LAST of PHASE (pilot or main for the heavy, light and wide mixes; sens
           for spread) and MIX, each block's arms in the order order_for gives it, offsets from
           the seed run7-<phase>-<mix>-<block>. A heavy block's level comes from level_for
           (blocks 1-2, 3-4, ... each hold both levels); the other mixes run at quiet. Each set:
           a cooldown, with the other processes' CPU over it (bg_cpu_cores); the level, quiet
           waiting up to QUIET_WAIT_S for QUIET_HEADROOM_MIB of headroom, loaded holding a
           ballast that brings the headroom down to LOADED_HEADROOM_MIB; the headroom at t0;
           then the agents, under a timeout of 600 s (a dry stand may lower it with
           RUN7_SET_TIMEOUT_S), past which every gate is stopped, P's sem calls are killed down
           to their jobs' process groups and the set is invalid with set_timeout. A missed
           level is flagged, never fatal. P leaves the heavy mix once OUT/pilot_check.json says
           it is infeasible. Exit 2 when P is to run and sem is not on PATH, when the evidence
           tree has uncommitted changes (pilot, main), when main lacks a pilot_check.json whose
           "go" is true, or when the files OUT/needs.sha256 names do not match it. --as-block
           and --primer as in run 6. Each record carries the coreslot binary's SHA-256, the
           evidence tree's HEAD and tree_dirty, the first line of `parallel --version` and, for
           main, pilot_check.json's SHA-256, all read once per command. Appends one record per
           set to OUT/<phase>-<mix>.jsonl, skipping each (block, arm) already recorded.
  pilot-check
           from the pilot's first valid blocks: the futility check of P1's three components and
           whether P is feasible, to OUT/pilot_check.json; main runs only when its "go" is true.
  summary  from OUT/main-*.jsonl, OUT/sens-*.jsonl and OUT/pilot_check.json: P1 by
           intersection-union, then the secondary family by Holm at 0.05, and the reported
           estimates; to OUT/summary.json and OUT/summary.txt.
"""
import argparse
import contextlib
import glob
import json
import math
import os
import random
import shutil
import signal
import statistics
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import acceptance  # noqa: E402  (probe, cooldown, runlog_lines)
import ballast  # noqa: E402  (Ballast)
import overload  # noqa: E402  (load, Sampler, headroom_mib)
import run4  # noqa: E402  (cs, fresh_state, write_config, checked_state, pacing, ...)
import run6  # noqa: E402  (measured, write_learn_state, check_needs, apps_meta, mix_cfg, ...)

HEAVY_ARMS = ("K60", "S1", "S2", "P")
P1_ARMS = ("K60", "S1", "S2")
LIGHT_ARMS = ("K60", "F")
WIDE_ARMS = ("K60", "S2", "F")
SPREAD_ARMS = ("K60", "S2")
LEVELS = ("quiet", "loaded")
QUIET_HEADROOM_MIB = 10500  # (probe)
LOADED_HEADROOM_MIB = 8000  # (probe)
LEVEL_TOLERANCE_MIB = 512
BALLAST_CAP_MIB = 6144
QUIET_WAIT_S = 120
RESIDENCY_SHARE = 0.95
RESIDENCY_WAIT_S = 60
SET_TIMEOUT_S = 600
P_MEMSUSPEND = "9G"
P_JOBS = 4
BG_CPU_FLAG_CORES = 1.5
K_SHARE = {"cpu": "140%", "mem": "60%"}
LEARN_RUNS = {"K": 6, "F": 3}
RELEARN_RUNS = 5
LEARN_COOLDOWN_S = 20
PILOT_BLOCKS = {"heavy": 12, "light": 12, "wide": 6}
N_LEVEL = 72  # heavy blocks per level
N_LIGHT = 40
N_WIDE = 30
SPREAD_BLOCKS = 12
SPREAD_WINDOW_S = 30
ALPHA = 0.05
MARGIN = math.log(1.03)
SWAP_EVENT_MIB = 64
SWAP_EVENT_P0 = 0.03
FUTILITY_CP = 0.5
P_INFEASIBLE_TIMEOUTS = 2  # P is infeasible when more than 2 of the 12 pilot heavy P sets time out


# --- learn ------------------------------------------------------------------------------------

def load(path):
    """overload.load, with the wide agents' cwd expanded as well."""
    cfg = overload.load(path)
    for chain in cfg.setdefault("wide_agents", {}).values():
        for g in chain:
            g["cwd"] = os.path.expanduser(g["cwd"])
    return cfg


def learn_gates(cfg):
    """Every gate of the agents and the wide agents, once each, in the stand's order."""
    return list({g["gate"]: g for agents in (cfg["agents"], cfg.get("wide_agents", {}))
                 for chain in agents.values() for g in chain}.values())


def seal(out):
    """OUT/needs.sha256: the SHA-256 of needs.toml and learn.jsonl, as sha256sum writes it."""
    with open(os.path.join(out, "needs.sha256"), "w") as f:
        for name in ("needs.toml", "learn.jsonl"):
            f.write(f"{run4.sha256(os.path.join(out, name))}  {name}\n")


def suggest(cfg, out, name):
    """`coreslot suggest --write`, its needs file copied to OUT/name."""
    r = run4.cs(cfg, "suggest", "--write", capture_output=True, text=True)
    print(run4.scrub(r.stdout + r.stderr), end="", flush=True)
    if r.returncode:
        sys.exit(f"run7: suggest --write exited {r.returncode}")
    shutil.copyfile(os.path.join(run4.checked_state(cfg), "needs.toml"), os.path.join(out, name))


def learn(cfg, out):
    for pattern in run6.RECORDS:
        if glob.glob(os.path.join(out, pattern)):
            print(f"run7: learn would change the needs {pattern} used", file=sys.stderr)
            sys.exit(2)
    os.makedirs(out, exist_ok=True)
    state = os.path.join(out, "learn-state.json")
    if os.path.exists(state):
        with open(state) as f:
            recorded = json.load(f)["state_dir"]
        if recorded != run4.scrub(cfg["state_dir"]):
            sys.exit(f"run7: learn started in {recorded}, not in this stand's state directory")
    else:
        run4.fresh_state(cfg)
        run6.write_learn_state(cfg, out)
    run4.write_config(cfg, K_SHARE["cpu"], K_SHARE["mem"])
    path = os.path.join(out, "learn.jsonl")
    recs = run4.read_jsonl(path)
    done = {(r["gate"], r["form"], r["pass"], r["run"]) for r in recs}
    pacing = run4.pacing(cfg)
    pacing.cooldown = max(pacing.cooldown, LEARN_COOLDOWN_S)
    baseline = []

    def runs(todo):
        for g, form, pass_, n in todo:
            if (g["gate"], form, pass_, n) in done:
                continue
            if not baseline:
                baseline.append(acceptance.probe())
            acceptance.cooldown(pacing, baseline[0])
            started = datetime.now(timezone.utc).isoformat(timespec="seconds")
            log0 = len(acceptance.runlog_lines(cfg))
            code, output, peak, cores, wall = run6.measured(cfg, g, form)
            rec = {"started": started, "gate": g["gate"], "label": g["label"], "form": form,
                   "pass": pass_, "run": n, "exit": code, "wall_s": wall, "peak_kib": peak,
                   "cores": cores, "failures": run4.fail_lines(g, output) if code else []}
            if form == "K":  # coreslot's own run time, the K slowdown's denominator
                new = acceptance.runlog_lines(cfg)[log0:]
                rec["ran_s"] = json.loads(new[-1]).get("ran_s") if new else None
            with open(path, "a") as f:
                f.write(json.dumps(rec) + "\n")
            print(json.dumps(rec), flush=True)

    gates = learn_gates(cfg)
    # suggest #1 rewrites the entries relearn runs under, so it never runs once relearn began
    if not any(r["pass"] == "relearn" for r in recs):
        runs((g, form, "learn", n) for g in gates for form, k in LEARN_RUNS.items()
             for n in range(1, k + 1))
        suggest(cfg, out, "needs-pass1.toml")
    runs((g, "K", "relearn", n) for g in gates for n in range(1, RELEARN_RUNS + 1))
    suggest(cfg, out, "needs.toml")
    seal(out)


# --- sets -------------------------------------------------------------------------------------

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
_p_live, _p_lock = set(), threading.Lock()  # P's sem calls, for kill_tree on a timeout


@contextlib.contextmanager
def level(name):
    """The set's memory level; yields its record and releases the ballast on leaving. Quiet waits
    up to QUIET_WAIT_S for the headroom to reach its floor; loaded holds a ballast that brings the
    headroom down to its target and waits up to RESIDENCY_WAIT_S for the ballast to be resident."""
    before = overload.headroom_mib()
    rec = {"level": name, "headroom_before_mib": before, "ballast_mib": 0,
           "ballast_rss_mib": None, "flags": []}
    if name == "quiet":
        t0, h = time.monotonic(), before
        while h is None or h < QUIET_HEADROOM_MIB:
            if time.monotonic() - t0 >= QUIET_WAIT_S:
                rec["flags"].append("quiet_short")
                break
            time.sleep(1.0)
            h = overload.headroom_mib()
        yield rec
        return
    size = rec["ballast_mib"] = min(max((before or 0) - LOADED_HEADROOM_MIB, 0), BALLAST_CAP_MIB)
    with ballast.Ballast(size) as b:
        t0 = time.monotonic()
        while size:
            rec["ballast_rss_mib"] = b.rss_mib()
            if (rec["ballast_rss_mib"] or 0) >= RESIDENCY_SHARE * size:
                break
            if time.monotonic() - t0 >= RESIDENCY_WAIT_S:
                rec["flags"].append("ballast_short")
                break
            time.sleep(1.0)
        yield rec


def level_off(headroom_t0, name):
    """Whether the headroom at t0 missed the level: quiet's target is a floor, loaded's a point
    with LEVEL_TOLERANCE_MIB either side."""
    if headroom_t0 is None:
        return True
    if name == "quiet":
        return headroom_t0 < QUIET_HEADROOM_MIB
    return abs(headroom_t0 - LOADED_HEADROOM_MIB) > LEVEL_TOLERANCE_MIB


def parse_ps_time(s):
    """Seconds from ps's time column: macOS's M:SS.ss with unbounded minutes, or H:MM:SS."""
    secs = 0.0
    for part in s.strip().split(":"):
        secs = secs * 60 + float(part)
    return secs


def ps_snapshot():
    """pid -> (ppid, CPU seconds) of every process."""
    r = subprocess.run(["ps", "-A", "-o", "pid=,ppid=,time="], capture_output=True, text=True)
    snap = {}
    for line in r.stdout.splitlines():
        p = line.split()
        if len(p) == 3:
            snap[int(p[0])] = (int(p[1]), parse_ps_time(p[2]))
    return snap


def descendants(snap, pid):
    kids = {}
    for p, (ppid, _) in snap.items():
        kids.setdefault(ppid, []).append(p)
    found, todo = [], list(kids.get(pid, []))
    while todo:
        p = todo.pop()
        found.append(p)
        todo.extend(kids.get(p, []))
    return found


def background_cpu_cores(before, after, own_pids, seconds):
    """The cores other processes used between two snapshots: a pid new in after counts all its
    time, and a reused pid's negative delta counts 0."""
    used = sum(max(0.0, t - before.get(p, (None, 0.0))[1])
               for p, (_, t) in after.items() if p not in own_pids)
    return round(used / seconds, 2) if seconds > 0 else 0.0


def _killpg(group, sig):
    try:
        os.killpg(group, sig)
        return True
    except (ProcessLookupError, PermissionError):
        return False


def kill_tree(pid):
    """SIGTERM, then SIGKILL after 5 s, to the process group of every descendant of pid, then to
    pid's own: sem starts its job in a group of its own. Never the runner's own group."""
    groups = []
    for p in descendants(ps_snapshot(), pid) + [pid]:
        try:
            g = os.getpgid(p)
        except ProcessLookupError:
            continue
        if g != os.getpgrp() and g not in groups:
            groups.append(g)
    groups = [g for g in groups if _killpg(g, signal.SIGTERM)]
    deadline = time.monotonic() + 5
    while groups and time.monotonic() < deadline:
        time.sleep(0.2)
        groups = [g for g in groups if _killpg(g, 0)]
    for g in groups:
        _killpg(g, signal.SIGKILL)


def run_p(argv, **kw):
    """run4.run_gate for P's sem call, registered while it runs so the set can kill_tree it."""
    p = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                         stdin=subprocess.DEVNULL, text=True, errors="replace",
                         start_new_session=True, **kw)
    with _p_lock:
        _p_live.add(p)
    try:
        out, _ = p.communicate()
    finally:
        with _p_lock:
            _p_live.discard(p)
    return p.returncode, out


def run_one7(cfg, gate, arm, set_id, env):
    """run4.run_one, except that P runs each heavy gate through GNU sem, with env added."""
    if arm != "P" or not gate["heavy"]:
        return run4.run_one(cfg, gate, arm)
    argv = [gate.get("free_n", "auto") if a == "{n}" else a for a in gate["argv"]]
    return run_p(["sem", "--will-cite", "--id", set_id, "--memsuspend", P_MEMSUSPEND, "-j",
                  str(P_JOBS), "--fg", *argv], cwd=gate["cwd"],
                 env=dict(os.environ, **gate.get("env", {}), **env))


def parallel_home(cfg):
    """PARALLEL_HOME beside the stand's state directory, created so sem never falls back to
    ~/.parallel."""
    run4.checked_state(cfg)
    home = os.path.join(os.path.dirname(cfg["state_dir"]), "parallel-home")
    os.makedirs(home, exist_ok=True)
    return home


def set_timeout(cfg):
    """SET_TIMEOUT_S; only a dry stand may lower it with RUN7_SET_TIMEOUT_S."""
    v = os.environ.get("RUN7_SET_TIMEOUT_S")
    return float(v) if cfg.get("dry") and v else SET_TIMEOUT_S


def output(*argv):
    """argv's stdout, or None when it cannot start or exits non-zero."""
    try:
        r = subprocess.run(argv, capture_output=True, text=True)
    except OSError:
        return None
    return r.stdout if r.returncode == 0 else None


def provenance(cfg, out, phase):
    """Every record's provenance, read once per verdict session; a tree git cannot read counts
    as dirty."""
    status = output("git", "-C", REPO, "status", "--porcelain", "--", "evidence")
    version = output("parallel", "--version")
    meta = {"binary_sha256": run4.sha256(cfg["coreslot"]),
            "evidence_head": (output("git", "-C", REPO, "rev-parse", "HEAD") or "").strip() or None,
            "tree_dirty": status is None or bool(status.strip()),
            "parallel_version": version.splitlines()[0] if version else None}
    if phase == "main":
        meta["pilot_check_sha256"] = run4.sha256(os.path.join(out, "pilot_check.json"))
    return meta


def run_agents(cfg, arm, offsets, slots, set_id, timeout):
    """run4.run_set's agents, each gate through run_one7, under a watchdog: past timeout s it
    stops every gate and kills P's sem calls down to their jobs. Returns (gates, failures,
    errors, samples, t0, timed_out)."""
    env = {"PARALLEL_HOME": parallel_home(cfg)}
    sem = threading.Semaphore(slots or 1)
    gates, failures, errors, lock, expired = [], [], [], threading.Lock(), threading.Event()
    t0 = time.time()

    def agent(name, chain):
        try:
            time.sleep(offsets[name])
            for g in chain:
                held = arm.startswith("S") and g["heavy"]
                if held:
                    sem.acquire()
                try:
                    if expired.is_set():
                        return
                    s = time.time()
                    code, out = run_one7(cfg, g, arm, set_id, env)
                finally:
                    if held:
                        sem.release()
                e = time.time()
                with lock:
                    gates.append({"agent": name, "gate": g["gate"], "label": g.get("label"),
                                  "start": round(s - t0, 1), "end": round(e - t0, 1),
                                  "exit": code})
                    if code:
                        failures.append({"agent": name, "gate": g["gate"],
                                         "lines": run4.fail_lines(g, out)})
        except Exception as exc:  # a runner error invalidates the set, not the run
            with lock:
                errors.append(run4.scrub(f"{name}: {exc!r}"))

    killed = set()

    def stop():
        run4.stop_live()
        with _p_lock:
            todo = [p for p in _p_live if p.pid not in killed]
        for p in todo:
            killed.add(p.pid)
            kill_tree(p.pid)

    sampler = overload.Sampler()
    sampler.start()
    threads = [threading.Thread(target=agent, args=(n, c), daemon=True)
               for n, c in cfg["agents"].items()]
    for t in threads:
        t.start()
    deadline = time.monotonic() + timeout
    try:
        for t in threads:
            while t.is_alive():
                if expired.is_set() or time.monotonic() > deadline:
                    expired.set()
                    stop()  # again on each pass: a gate may have started just before expiry
                t.join(0.5)
    except KeyboardInterrupt:
        stop()
        raise
    finally:
        sampler.stop.set()
        sampler.join()
    return gates, failures, errors, sampler.samples, t0, expired.is_set()


def run_set7(cfg, path, *, arm, offsets, meta, baseline, level_name, slots=None, needs=None):
    """Runs one set of arm at level_name, appends its record to path and returns it. The record
    is run4.run_set's, plus the level's record, the headroom at t0, the background CPU over the
    cooldown and the set's sem id. An arm starting with S holds each heavy gate on one of
    `slots` slots; a K arm runs with K_SHARE and copies needs into the state directory."""
    set_id = f"run7-{os.getpid()}-{time.time_ns()}"
    lockdir = os.path.join(parallel_home(cfg), "semaphores", f"id-{set_id}")
    try:
        snap0, c0 = ps_snapshot(), time.monotonic()
        cd = acceptance.cooldown(run4.pacing(cfg), baseline)
        snap1, cooled = ps_snapshot(), time.monotonic() - c0
        own = {os.getpid(), *descendants(snap1, os.getpid())}
        bg_cpu = background_cpu_cores(snap0, snap1, own, cooled)
        ac = run4.on_ac()
        page, bg = run4.background()
        if arm.startswith("K"):
            run4.write_config(cfg, K_SHARE["cpu"], K_SHARE["mem"])
            dst = os.path.join(run4.checked_state(cfg), "needs.toml")
            shutil.copyfile(needs, dst)
            want = run4.sha256(needs)
            if not run4.budget_empty(cfg):
                sys.exit(f"run7: {meta}: the budget is not empty")
        runlog0 = len(acceptance.runlog_lines(cfg))
        with level(level_name) as lvl:
            h0 = overload.headroom_mib()
            gates, failures, errors, samples, t0, timed_out = run_agents(
                cfg, arm, offsets, slots, set_id, set_timeout(cfg))
        if arm.startswith("K") and (run4.sha256(dst) if os.path.exists(dst) else None) != want:
            errors.append("the needs file changed during the set")
        verdict = {n: round(max(g["end"] for g in gates if g["agent"] == n) - offsets[n], 1)
                   for n in cfg["agents"] if any(g["agent"] == n for g in gates)}
        runlog = ([json.loads(l) for l in acceptance.runlog_lines(cfg)[runlog0:]]
                  if arm.startswith("K") else [])
        flags = (["hot_start"] if cd["cooldown_gave_up"] else []) + lvl["flags"] + (
            ["level_off"] if level_off(h0, level_name) else []) + (
            ["busy_background"] if bg_cpu > BG_CPU_FLAG_CORES else [])
        reasons = run4.invalid_reasons(samples, ac, "; ".join(errors)) + (
            ["set_timeout"] if timed_out else [])
        rec = {**meta, "arm": arm, "set_id": set_id,
               "started": datetime.fromtimestamp(t0, timezone.utc).isoformat(),
               "probe_baseline_s": baseline, **cd, "ac": ac, "background": bg,
               "page_bytes": page, "bg_cpu_cores": bg_cpu,
               **{k: v for k, v in lvl.items() if k != "flags"}, "headroom_t0_mib": h0,
               "flags": flags, "valid": not reasons, "invalid": reasons, "offsets": offsets,
               "gates": sorted(gates, key=lambda g: (g["agent"], g["start"])),
               "failures": failures, "verdict_s": verdict,
               "mean_verdict_s": round(statistics.mean(verdict.values()), 1) if verdict else None,
               "makespan_s": round(max(g["end"] for g in gates), 1) if gates else None,
               "waited_free_s": round(sum(l.get("waited_free_s") or 0 for l in runlog), 1)
               if arm.startswith("K") else None,
               "samples": samples, "runlog": runlog}
        with open(path, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(f"{meta.get('block')} {arm} {level_name}: mean verdict {rec['mean_verdict_s']} s,"
              f" makespan {rec['makespan_s']} s, warn+ {run4.warn_s(rec)} s, failed"
              f" {[x['gate'] for x in failures]}, flags {flags}, valid {rec['valid']}"
              f" {reasons or ''}", flush=True)
        return rec
    finally:
        shutil.rmtree(lockdir, ignore_errors=True)
        with contextlib.suppress(FileNotFoundError):
            os.remove(lockdir + ".lock")


# --- verdict ----------------------------------------------------------------------------------

MIXES = {"heavy": HEAVY_ARMS, "light": LIGHT_ARMS, "wide": WIDE_ARMS, "spread": SPREAD_ARMS}


def level_for(phase, block):
    """A heavy block's level: blocks 1-2, 3-4, ... each hold both levels, in a seeded order."""
    levels = list(LEVELS)
    random.Random(f"run7-level-{phase}-{(block - 1) // 2}").shuffle(levels)
    return levels[(block - 1) % 2]


def order_for(phase, mix, block, arms):
    arms = list(arms)
    random.Random(f"run7-order-{phase}-{mix}-{block}").shuffle(arms)
    return arms


def static_sizes7(cfg):
    """The cpu and mem sizes `coreslot status --json` reports under K_SHARE, in tokens."""
    os.makedirs(run4.checked_state(cfg), mode=0o700, exist_ok=True)
    run4.write_config(cfg, K_SHARE["cpu"], K_SHARE["mem"])
    r = run4.cs(cfg, "status", "--json", capture_output=True, text=True)
    if r.returncode:
        sys.exit(f"run7: status --json exited {r.returncode}")
    res = json.loads(r.stdout)["resources"]
    return {"cpu": res["cpu"]["size"], "mem": res["mem"]["size"]}


def mix_cfg7(cfg, mix):
    return dict(cfg, agents=cfg["wide_agents"]) if mix == "wide" else run6.mix_cfg(cfg, mix)


def arm_kw7(out, arm):
    """run_set7's keyword arguments for arm."""
    if arm.startswith("S"):
        return {"slots": int(arm[1:])}
    return {"needs": os.path.join(out, "needs.toml")} if arm == "K60" else {}


def refuse(why):
    print(f"run7: {why}", file=sys.stderr)
    sys.exit(2)


def verdict(cfg, out, phase, mix, first, last, as_block=None, primer=False):
    if (phase == "sens") != (mix == "spread"):
        refuse(f"mix {mix} does not belong to phase {phase}")
    path = os.path.join(out, "pilot_check.json")
    check = None
    if os.path.exists(path):
        with open(path) as f:
            check = json.load(f)
    if phase == "main" and not (check or {}).get("go"):
        refuse(f"main runs only after a pilot check whose go is true, in {path}")
    arms = [a for a in MIXES[mix] if not (a == "P" and (check or {}).get("p_feasible") is False)]
    if "P" in arms and not shutil.which("sem"):
        refuse("arm P needs GNU sem on PATH")
    run6.check_needs(out)
    path = os.path.join(out, f"{phase}-{mix}.jsonl")
    recs = run4.read_jsonl(path)
    if as_block and (as_block in run6.reruns(recs) or
                     not any(r["block"] == as_block and not r["valid"] for r in recs)):
        refuse(f"block {as_block} is a re-run or has no invalid set")
    done = {(r["block"], r["arm"]) for r in recs}
    blocks = [(b, [a for a in order_for(phase, mix, as_block or b, arms) if (b, a) not in done])
              for b in range(first, last + 1)]
    blocks = [(b, pending) for b, pending in blocks if pending]
    if not blocks:
        return
    prov = provenance(cfg, out, phase)
    if prov["tree_dirty"] and phase != "sens":
        refuse("the evidence tree has uncommitted changes")
    scfg = mix_cfg7(cfg, mix)
    if not scfg["agents"]:
        refuse(f"the stand has no agents for the {mix} mix")
    apps, static, baseline = run6.apps_meta(out), static_sizes7(cfg), acceptance.probe()
    version = run6.coreslot_version(cfg)
    needs_sha = run4.sha256(os.path.join(out, "needs.toml"))
    for i, (b, pending) in enumerate(blocks):
        src = as_block or b
        order = order_for(phase, mix, src, arms)
        level_name = level_for(phase, src) if mix == "heavy" else "quiet"
        offsets = run4.offsets_for(scfg, f"run7-{phase}-{mix}-{src}")
        sets = [("K60", None, os.path.join(out, "primer.jsonl"))] if primer and i == 0 else []
        sets += [(arm, order.index(arm), path) for arm in pending]
        for arm, pos, dst in sets:
            meta = {"phase": phase, "mix": mix, "block": b, "as_block": as_block,
                    "order": " ".join(order), "level_name": level_name, "position": pos,
                    "prev_arm": order[pos - 1] if pos else None, "mix_agents": list(scfg["agents"]),
                    **apps, "coreslot_version": version,
                    "needs_sha256": needs_sha if arm == "K60" else None,
                    "static": static if arm == "K60" else None, **prov}
            run_set7(scfg, dst, arm=arm, offsets=offsets, meta=meta, baseline=baseline,
                     level_name=level_name, **arm_kw7(out, arm))


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
    cfg = load(args.stand)

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
