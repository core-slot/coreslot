#!/usr/bin/env python3
"""Acceptance run 7: coreslot against fixed slot counts (a machine-wide flock and `sem -j2`) and
GNU parallel's --memsuspend, at a quiet and a loaded level of memory headroom.

usage:
  run7.py learn STAND OUT
  run7.py verdict STAND OUT PHASE MIX FIRST LAST [--as-block B] [--primer]
  run7.py pilot-check OUT
  run7.py summary OUT [--dropped]

STAND is overload/stand7.json: run 4's stand with FastAPI's tests under `--dist loadgroup`, and
"wide_agents", the agents of the wide mix. Every coreslot call goes through run4.cs() with the
stand's state directory; GNU parallel runs with PARALLEL_HOME=<state_dir>/../parallel-home.

The arms: K60 runs every gate under coreslot with cpu = "140%", mem = "60%" and OUT/needs.toml;
S1 and S2 hold each heavy gate on one of 1 or 2 slots of an in-process counting semaphore, the
light gates free; P runs each heavy gate through GNU sem with --memsuspend 9G and -j 4; F runs
every gate when its agent reaches it. The mixes: heavy (K60, S1, S2, P on the stand's agents, at
the quiet and the loaded level), light (K60, F on A3 and A4), wide (K60, S2, F on the wide
agents) and spread (K60, S2, arrivals U(0, 30 s)), the last three at the quiet level only.

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
           never run again. Once OUT/needs.sha256 exists, it does nothing and exits 0. Refuses
           with exit 2 once any pilot, main or sensitivity record exists.
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
           tree has uncommitted changes or the coreslot binary's SHA-256 is not the stand's
           coreslot_sha256 (pilot, main; a dry stand skips both), when main lacks a
           pilot_check.json whose "go" is true, or when the files OUT/needs.sha256 names do not
           match it. A command with nothing left to run exits 0 first. --as-block
           and --primer as in run 6. Each record carries the coreslot binary's SHA-256, the
           evidence tree's HEAD and tree_dirty, the first line of `parallel --version` and, for
           main, pilot_check.json's SHA-256, all read once per command. Appends one record per
           set to OUT/<phase>-<mix>.jsonl, skipping each (block, arm) already recorded.
  pilot-check
           from the pilot's first 12 heavy blocks valid over K60, S1 and S2 (P's validity never
           drops one), its first 12 light and 6 wide valid blocks, each mix counting at most 3
           re-runs: the futility check of P1's three components (a mean <= 0, wins <= losses, or
           a power at 72 blocks per level below 0.5) and whether P is feasible (at most 2 of the
           pilot's P sets timed out), to OUT/pilot_check.json; main runs only when its "go" is
           true. Exits 2 while the pilot is short, or once a main record exists.
  summary  from OUT/main-*.jsonl, OUT/sens-spread.jsonl and OUT/pilot_check.json: the first 72
           heavy blocks of each level valid over K60, S1 and S2, and the first 40 light and 30
           wide valid blocks (a mix's `--as-block` re-runs count up to 10 % of its n). P1 by
           intersection-union at 0.05: T1, S2 slower than K60 at loaded (paired t on the log
           ratio); T2, K60's swap traffic below S2's at loaded (signed-rank); T3, S1 slower than
           K60 at quiet. Only once P1 is rejected, the secondary family by Holm at 0.05: H-P, K60
           within 3 % of P at loaded over the blocks where P is valid (untested when P is
           infeasible); H-L, light K60 within 3 % of F (untested past one pilot F set under
           pressure); H-Z, K60's heavy sets with more than 64 MiB swapped out below 3 %; H-W,
           wide S2 slower than K60. Then the reported estimates; to OUT/summary.json and
           OUT/summary.txt. Exits 2 without pilot_check.json, or while a mix lacks n valid
           blocks and has re-runs left; with them spent, its tests are untested. --dropped
           declares the drop past 32 h: the spread run may be absent and the wide n is 24.
"""
import argparse
import collections
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
import stats6  # noqa: E402
import stats7  # noqa: E402

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
N_WIDE_DROPPED = 24  # the wide n once the pre-registered drop past 32 h is declared
SPREAD_BLOCKS = 12
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
    if os.path.exists(os.path.join(out, "needs.sha256")):
        print("run7: the needs are sealed in OUT/needs.sha256; learn has nothing to do")
        return
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
    as dirty. OUT's own records, when it lies under the repository, never make it dirty."""
    spec = ["evidence"]
    rel = os.path.relpath(os.path.realpath(out), os.path.realpath(REPO))
    if rel != os.pardir and not rel.startswith(os.pardir + os.sep):
        spec.append(f":(exclude){rel}")
    status = output("git", "-C", REPO, "status", "--porcelain", "--", *spec)
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
    run6.check_needs(out)
    path = os.path.join(out, f"{phase}-{mix}.jsonl")
    recs = run4.read_jsonl(path)
    # a block whose only invalid set is P's keeps its P1 sets, so a re-run would only spend the cap
    if as_block and (as_block in run6.reruns(recs) or not {
            r["arm"] for r in recs if r["block"] == as_block and not r["valid"]} - {"P"}):
        refuse(f"block {as_block} is a re-run or has no invalid set but P's")
    done = {(r["block"], r["arm"]) for r in recs}
    blocks = [(b, [a for a in order_for(phase, mix, as_block or b, arms) if (b, a) not in done])
              for b in range(first, last + 1)]
    blocks = [(b, pending) for b, pending in blocks if pending]
    if not blocks:
        return
    if "P" in arms and not shutil.which("sem"):
        refuse("arm P needs GNU sem on PATH")
    prov = provenance(cfg, out, phase)
    if phase != "sens" and not cfg.get("dry"):  # a dry run's records are not evidence
        if prov["tree_dirty"]:
            refuse("the evidence tree has uncommitted changes")
        if cfg.get("coreslot_sha256") != prov["binary_sha256"]:  # e.g. setup.sh rebuilt it
            refuse(f"{cfg['coreslot']} is not the binary the stand's coreslot_sha256 pins")
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


# --- pilot check ------------------------------------------------------------------------------

PILOT_RERUNS = 3  # the re-runs each pilot mix counts


def swap_traffic_pages(r):
    """The pages swapped out and in over the set."""
    return run4.counter(r, "swapouts") + run4.counter(r, "swapins")


def by_level7(blocks):
    """level name -> {block: sets}, in LEVELS order."""
    return {lv: {b: sets for b, sets in blocks.items() if sets["K60"]["level_name"] == lv}
            for lv in LEVELS}


def p_sets(blocks, recs):
    """block -> the P set recorded beside the P1 sets the block took, valid or not."""
    by = {(r["block"], r["arm"]): r for r in recs}
    return {b: by[(sets["K60"]["block"], "P")] for b, sets in blocks.items()
            if (sets["K60"]["block"], "P") in by}


def ln_ratios(blocks, num, den):
    return [run6.ln_ratio(sets, num, den) for sets in blocks.values()]


def traffic_diffs(blocks, a, b):
    """a's swap traffic minus b's per block; a set coreslot failed counts as infinite."""
    return [run6.worse(sets[a], swap_traffic_pages) - swap_traffic_pages(sets[b])
            for sets in blocks.values()]


def t_futility(d):
    """The pilot's mean and SD of d and the t test's power at N_LEVEL blocks; fewer than two
    values leave no power."""
    if len(d) < 2:
        return {"mean": statistics.fmean(d) if d else None, "sd": None, "cp_at_n": 0.0}
    mean, sd = statistics.fmean(d), statistics.stdev(d)
    return {"mean": mean, "sd": sd, "cp_at_n": stats7.t_power(N_LEVEL, mean, sd, ALPHA)}


def sign_futility(d, n):
    """Wins (d < 0) and losses of the n pilot blocks, and the sign test's power at N_LEVEL blocks
    with the pilot's discordance and win rate; no discordant pair leaves no power."""
    wins, losses = sum(x < 0 for x in d), sum(x > 0 for x in d)
    m = wins + losses
    cp = stats7.sign_power(round(N_LEVEL * m / n), wins / m, ALPHA) if m else 0.0
    return {"wins": wins, "losses": losses, "cp_at_n": cp}


def pilot_check(out):
    if glob.glob(os.path.join(out, "main-*.jsonl")):
        refuse("pilot-check would judge the pilot after main records exist")
    recs, pilot = {}, {}
    for mix, arms in (("heavy", P1_ARMS), ("light", LIGHT_ARMS), ("wide", WIDE_ARMS)):
        recs[mix] = run6.capped(run6.records(out, "pilot", mix), PILOT_RERUNS)[0]
        pilot[mix] = run6.first(run6.valid_blocks(recs[mix], arms), PILOT_BLOCKS[mix])
    short = [f"{len(bs)} of {PILOT_BLOCKS[m]} {m}" for m, bs in pilot.items()
             if len(bs) < PILOT_BLOCKS[m]]
    if short:
        refuse(f"the pilot has {', '.join(short)} valid blocks")
    lv = by_level7(pilot["heavy"])
    loaded = lv["loaded"]
    p_timeouts = sum("set_timeout" in r["invalid"]
                     for r in p_sets(pilot["heavy"], recs["heavy"]).values())
    res = {"blocks": {m: list(bs) for m, bs in pilot.items()},
           "T1": t_futility(ln_ratios(loaded, "S2", "K60")),
           "T2": sign_futility(traffic_diffs(loaded, "K60", "S2"), len(loaded)) if loaded
           else {"wins": 0, "losses": 0, "cp_at_n": 0.0},
           "T3": t_futility(ln_ratios(lv["quiet"], "S1", "K60")),
           "p_timeouts": p_timeouts, "p_feasible": p_timeouts <= P_INFEASIBLE_TIMEOUTS,
           "light_pressure_sets": sum(run4.warn_s(sets["F"]) > 0
                                      for sets in pilot["light"].values())}
    res["futile"] = [t for t in ("T1", "T2", "T3") if res[t]["cp_at_n"] < FUTILITY_CP or (
        res[t]["wins"] <= res[t]["losses"] if t == "T2"
        else res[t]["mean"] is None or res[t]["mean"] <= 0)]
    res["go"] = not res["futile"]
    with open(os.path.join(out, "pilot_check.json"), "w") as f:
        json.dump(res, f, indent=1)
    print(json.dumps(res, indent=1))


# --- summary ----------------------------------------------------------------------------------

SECONDARY = ("H-P", "H-L", "H-Z", "H-W")
NOT_TESTED = "not tested (P1 not rejected)"
SHORT = "its mix is short of n, re-runs spent"


def finite_t(t):
    return {k: run6.finite(v) for k, v in t.items()}


def paired(r):
    """Whether the mem grants of the set's runs summed above its static mem size at some instant;
    a run holds its grant from its start (time_ns) plus its wait, for ran_s."""
    events = []
    for line in r["runlog"]:
        start = line["time_ns"] / 1e9 + (line.get("waited_s") or 0)
        mem = (line.get("grant") or {}).get("mem", 0)
        events += [(start, 1, mem), (start + (line.get("ran_s") or 0), 0, -mem)]
    held = 0
    for _, _, mem in sorted(events):  # at one instant an end comes before a start
        held += mem
        if held > r["static"]["mem"]:
            return True
    return False


def pairing_count(sets):
    return sum(paired(r) for r in sets)


def p1_test(heavy):
    """P1 by intersection-union: T1 and T2 on the loaded blocks, T3 on the quiet ones."""
    lv = by_level7(heavy)
    d1, d3 = ln_ratios(lv["loaded"], "S2", "K60"), ln_ratios(lv["quiet"], "S1", "K60")
    t1, t3 = stats6.paired_lower(d1, 0), stats6.paired_lower(d3, 0)
    t2 = stats7.wilcoxon_less(traffic_diffs(lv["loaded"], "K60", "S2"))
    res = stats7.iut({"T1": t1["p"], "T2": t2["p"], "T3": t3["p"]}, ALPHA)
    res.update(T1={"blocks": list(lv["loaded"]), "t": finite_t(t1), "estimate": run6.estimate(d1)},
               T2={"blocks": list(lv["loaded"]), **t2},
               T3={"blocks": list(lv["quiet"]), "t": finite_t(t3), "estimate": run6.estimate(d3)})
    return res


def secondary_tests(heavy, recs, light, wide, check):
    """name -> its test with its p, or {"untested": why}; a mix given as None is short."""
    loaded, res = by_level7(heavy)["loaded"], {}
    if check.get("p_feasible") is False:
        res["H-P"] = {"untested": "P is infeasible"}
    else:
        pv = {b: dict(loaded[b], P=r) for b, r in p_sets(loaded, recs).items() if r["valid"]}
        d = ln_ratios(pv, "K60", "P")
        t = stats6.paired_upper(d, MARGIN)
        res["H-P"] = {"blocks": list(pv), "t": finite_t(t), "estimate": run6.estimate(d),
                      "p": t["p"]}
    if light is None or check["light_pressure_sets"] > 1:
        res["H-L"] = {"untested": SHORT if light is None
                      else "more than one light pilot F set under pressure"}
    else:
        d = ln_ratios(light, "K60", "F")
        t = stats6.paired_upper(d, MARGIN)
        res["H-L"] = {"blocks": list(light), "t": finite_t(t), "estimate": run6.estimate(d),
                      "p": t["p"]}
    events = [b for b, sets in heavy.items()
              if run6.swapout_bytes(sets["K60"]) > SWAP_EVENT_MIB * 2**20]
    k, n = len(events), len(heavy)
    res["H-Z"] = {"events": events, "k": k, "n": n, "p": stats7.binom_upper_p(k, n, SWAP_EVENT_P0),
                  "ci95": [run6.finite(x) for x in stats6.clopper_pearson(k, n)] if n else None}
    if wide is None:
        res["H-W"] = {"untested": SHORT}
    else:
        d = ln_ratios(wide, "S2", "K60")
        t = stats6.paired_lower(d, 0)
        res["H-W"] = {"blocks": list(wide), "t": finite_t(t), "estimate": run6.estimate(d),
                      "p": t["p"]}
    return res


def holm_rows(tests, p1):
    """Holm at ALPHA over the secondary family, only once P1 is rejected."""
    if not p1["rejected"]:
        return [{"name": n, "status": NOT_TESTED} for n in SECONDARY]
    rows = stats7.holm({n: tests[n].get("p") for n in SECONDARY}, ALPHA)
    for r in rows:
        r["status"] = ("untested" if not r["tested"]
                       else "rejected" if r["rejected"] else "not rejected")
    return rows


def per_arm7(blocks, arms, out):
    """run6.per_arm, with each arm's swap traffic."""
    res = run6.per_arm(blocks, arms, out)
    for a in res:
        res[a]["swap_traffic_pages"] = run6.ci(swap_traffic_pages(sets[a])
                                               for sets in blocks.values())
    return res


def k60_vs_p(blocks, pb):
    """K60's swap traffic against P's, over the blocks with a valid P set."""
    bs = [b for b in blocks if b in pb]
    k = [swap_traffic_pages(blocks[b]["K60"]) for b in bs]
    p = [swap_traffic_pages(pb[b]["P"]) for b in bs]
    return {"n": len(bs), "K60_lower": sum(x < y for x, y in zip(k, p)),
            "P_lower": sum(x > y for x, y in zip(k, p)),
            "K60": run6.ci(k) if k else None, "P": run6.ci(p) if p else None}


def s_star(blocks):
    """S* in hindsight: the S arm with the lowest mean primary over the blocks, and K60/S*."""
    if not blocks:
        return None
    s = run6.lowest(blocks, list(blocks), ("S1", "S2"))
    return {"S*": s, "K60/S*": run6.estimate(ln_ratios(blocks, "K60", s))}


def spread(out):
    blocks = run6.first(run6.valid_blocks(run6.records(out, "sens", "spread"), SPREAD_ARMS),
                        SPREAD_BLOCKS)
    return {"blocks": list(blocks),
            "means": {a: round(statistics.fmean(sets[a]["mean_verdict_s"]
                                                for sets in blocks.values()), 1)
                      for a in SPREAD_ARMS} if blocks else {},
            "S2/K60": run6.estimate(ln_ratios(blocks, "S2", "K60"))}


def flag_counts(out):
    """Per main or sensitivity file, over every set recorded: each flag's count and the invalid
    sets' reasons."""
    res = {}
    for path in sorted(glob.glob(os.path.join(out, "main-*.jsonl"))
                       + glob.glob(os.path.join(out, "sens-*.jsonl"))):
        recs = run4.read_jsonl(path)
        res[os.path.basename(path)[:-len(".jsonl")]] = {
            "sets": len(recs),
            "flags": dict(sorted(collections.Counter(f for r in recs for f in r["flags"]).items())),
            "invalid": dict(sorted(collections.Counter(x for r in recs
                                                       for x in r["invalid"]).items()))}
    return res


def per_hour(blocks):
    """UTC hour of `started` -> arm -> its sets' count and mean primary."""
    hours = {}
    for sets in blocks.values():
        for a, r in sets.items():
            h = datetime.fromisoformat(r["started"]).astimezone(timezone.utc)
            hours.setdefault(h.strftime("%Y-%m-%dT%H"), {}).setdefault(a, []).append(
                r["mean_verdict_s"])
    return {h: {a: {"n": len(v), "mean_verdict_s": round(statistics.fmean(v), 1)}
                for a, v in sorted(arms.items())} for h, arms in sorted(hours.items())}


def reported(heavy, recs, light, wide, out):
    lv = by_level7(heavy)
    pb = {b: {"P": r} for b, r in p_sets(heavy, recs).items() if r["valid"]}

    def k60_sets(blocks):
        return {"paired": pairing_count(sets["K60"] for sets in blocks.values()),
                "n": len(blocks)}
    return {"per_arm": {"heavy": {lvl: {**per_arm7(bs, P1_ARMS, out),
                                        **per_arm7({b: pb[b] for b in bs if b in pb}, ("P",), out)}
                                  for lvl, bs in lv.items()},
                        "light": per_arm7(light, LIGHT_ARMS, out),
                        "wide": per_arm7(wide, WIDE_ARMS, out)},
            "K60_vs_P_swap_traffic": {lvl: k60_vs_p(bs, pb) for lvl, bs in lv.items()},
            "S*_in_hindsight": {lvl: s_star(bs) for lvl, bs in lv.items()},
            "spread": spread(out),
            "pairing": {**{f"heavy {lvl}": k60_sets(bs) for lvl, bs in lv.items()},
                        "light": k60_sets(light), "wide": k60_sets(wide)},
            "flags": flag_counts(out),
            "order": run6.order_effects(heavy),
            "per_hour": per_hour(heavy)}


def summary_text(s):
    p1 = s["P1"]
    lines = ["Acceptance run 7: summary", "",
             f"Blocks used: heavy quiet {s['blocks']['heavy']['quiet']}",
             f"  heavy loaded {s['blocks']['heavy']['loaded']}",
             f"  light {s['blocks']['light']}", f"  wide {s['blocks']['wide']}",
             f"Re-runs: {s['reruns']}; ignored past 10 % of n: {s['reruns_ignored']}",
             f"Sets coreslot failed, each primary the block's slowest: {s['coreslot_failed']}",
             f"P feasible: {s['pilot']['p_feasible']}; light pilot F sets under pressure:"
             f" {s['pilot']['light_pressure_sets']}", ""]
    if s["incomplete"]:
        lines += [s["incomplete"], ""]
    lines.append("P1 (intersection-union, alpha 0.05): " + (
        "untested" if "untested" in p1 else "rejected" if p1["rejected"] else "not rejected"))
    for name, c in p1.get("components", {}).items():
        lines.append(f"  {name}: p {c['p']:.4g}, {'rejected' if c['rejected'] else 'not rejected'}")
    lines.append("Secondary family (Holm, alpha 0.05):")
    for r in s["secondary"]:
        lines.append(f"  {r['name']}: {r['status']}" + (
            f"; p {r['p']:.4g}, threshold {r['threshold']:.4g}" if r.get("p") is not None else ""))
    lines += ["", "Reported, untested:", json.dumps(s["reported"], indent=1), ""]
    return "\n".join(lines)


def summary(out, dropped=False):
    path = os.path.join(out, "pilot_check.json")
    if not os.path.exists(path):
        refuse(f"{path} is missing; run pilot-check first")
    with open(path) as f:
        check = json.load(f)
    mixes = {"heavy": (P1_ARMS, 2 * N_LEVEL), "light": (LIGHT_ARMS, N_LIGHT),
             "wide": (WIDE_ARMS, N_WIDE_DROPPED if dropped else N_WIDE)}
    recs, used, spent, ignored, full = {}, {}, {}, {}, {}
    for mix, (arms, n) in mixes.items():
        recs[mix], ignored[mix] = run6.capped(run6.records(out, "main", mix), n // 10)
        blocks = run6.valid_blocks(recs[mix], arms)
        if mix == "heavy":  # the first N_LEVEL of each level
            levels = [run6.first(bs, N_LEVEL) for bs in by_level7(blocks).values()]
            used[mix] = dict(sorted(b for bs in levels for b in bs.items()))
            full[mix] = all(len(bs) >= N_LEVEL for bs in levels)
        else:
            used[mix], full[mix] = run6.first(blocks, n), len(blocks) >= n
        spent[mix] = run6.reruns(recs[mix])
        if not full[mix] and len(spent[mix]) < n // 10:
            refuse(f"main {mix}: {len(used[mix])} of {n} valid blocks, {len(spent[mix])} of"
                   f" {n // 10} re-runs; no summary yet")
    heavy, light, wide = used["heavy"], used["light"], used["wide"]
    if full["heavy"]:
        p1 = p1_test(heavy)
        tests = secondary_tests(heavy, recs["heavy"], light if full["light"] else None,
                                wide if full["wide"] else None, check)
    else:
        p1, tests = {"rejected": False, "untested": SHORT}, {}
    short = [m for m in full if not full[m]]
    s = {"dropped": dropped,
         "pilot": {k: check.get(k) for k in ("p_feasible", "light_pressure_sets")},
         "blocks": {"heavy": {lvl: list(bs) for lvl, bs in by_level7(heavy).items()},
                    "light": list(light), "wide": list(wide)},
         "reruns": spent, "reruns_ignored": ignored,
         "coreslot_failed": {m: [f"{b} {a}" for b, sets in bs.items() for a, r in sets.items()
                                 if r.get("coreslot_failed")] for m, bs in used.items()},
         "incomplete": f"incomplete: {' and '.join(short)} short of n with the re-runs spent;"
                       " its tests untested" if short else None,
         "P1": p1, "secondary": holm_rows(tests, p1), "tests": tests,
         "reported": reported(heavy, recs["heavy"], light, wide, out)}
    with open(os.path.join(out, "summary.json"), "w") as f:
        json.dump(s, f, indent=1)
    text = summary_text(s)
    with open(os.path.join(out, "summary.txt"), "w") as f:
        f.write(text)
    print(text)


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
    sub.add_parser("pilot-check").add_argument("out")
    p = sub.add_parser("summary")
    p.add_argument("out")
    p.add_argument("--dropped", action="store_true")
    args = ap.parse_args()
    if args.cmd == "pilot-check":
        return pilot_check(args.out)
    if args.cmd == "summary":
        return summary(args.out, args.dropped)
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
