#!/usr/bin/env python3
"""Acceptance run 4: coreslot with needs it learned itself, against no admission and a tuned
semaphore, in counterbalanced blocks.

usage:
  run4.py learn STAND OUT
  run4.py tune STAND OUT
  run4.py verdict STAND OUT FIRST LAST [--as-block B]
  run4.py summary STAND OUT [--dry]
  run4.py figures STAND OUT

STAND is overload.py's stand file plus, per gate, "label" ("bench:<gate>") and, where its
failures can be named, "fail_pattern" (a regular expression over its output). Optional keys:
"cooldown_s" (20), "probe_tolerance" (0.10), "max_cooldown_s" (120), "tune" (the configurations
below), and "dry": true, which lets `summary --dry` run on fewer than 12 blocks.

The arms: F runs every gate when its agent reaches it ("{n}" is the gate's "free_n", "auto");
S holds a heavy gate on one of N slots of a counting semaphore; K runs every gate as
`coreslot run --label <label> -- <argv>`, with no --need: the needs file supplies it.

Every coreslot call goes through cs(), which sets CORESLOT_STATE_DIR to the stand's state_dir
and refuses, before running anything, a path outside ~/bench.noindex or a default coreslot
location. The configuration and the needs file are written only into that directory.

  learn    a fresh state directory with cpu = "140%", mem = "60%"; every gate alone under
           coreslot, `suggest --write`, every gate alone again, `suggest --write`; then every gate
           alone in its F form, the false-failure baseline. Writes OUT/learn.jsonl,
           OUT/stats-learn.json and OUT/needs.toml, frozen for every later phase.
  tune     S with each N, K with each (cpu, mem) share, 3 sets each, offsets from the seed
           run4-tune-<config>-<rep>; writes OUT/tune.jsonl and OUT/tuning.json by the rule in
           select_s and select_k.
  verdict  blocks FIRST..LAST of three sets, F, S_best and K_best, in the order PERMS gives the
           block (or block B's, with --as-block B, recorded as "as_block"), offsets from the seed
           run4-<block>; appends to OUT/results.jsonl.
  summary  OUT/summary.json and OUT/summary.txt from results.jsonl, learn.jsonl and tuning.json;
           exits 2 while fewer than 12 blocks are valid and the re-run cap is not spent.
  figures  OUT/ratios.svg and OUT/timeline.svg.

tune and verdict skip each (configuration, repetition) or (block, arm) already recorded, so the
same command continues an interrupted run; learn always starts over; summary and figures always
recompute.
"""
import argparse
import getpass
import hashlib
import json
import os
import random
import re
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone
from types import SimpleNamespace

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import acceptance  # noqa: E402  (probe, cooldown, runlog_lines, sign_test)
import overload  # noqa: E402  (load, Sampler)

ROOT = os.path.expanduser("~/bench.noindex")
DEFAULT_DIRS = ("~/Library/Application Support/coreslot", "~/.local/state/coreslot")
ARMS = ("F", "S", "K")
PERMS = ["FSK", "SKF", "KFS", "FKS", "KSF", "SFK"]
BLOCKS, RERUN_CAP, ALPHA, H3_FACTOR = 12, 4, 0.05, 1.10
LIVE_FREE_S, LIVE_SHARE, LIVE_BLOCKS = 5.0, 0.10, 6
TUNE = {"S": [2, 3], "K": [["140%", "60%"], ["140%", "80%"], ["200%", "60%"], ["200%", "80%"]],
        "reps": 3}
sign_test = acceptance.sign_test


def order_for(block):
    return list(PERMS[(block - 1) % len(PERMS)])


# --- the state directory and coreslot ---------------------------------------------------------

def checked_state(cfg):
    """The stand's state directory, or exit when it is not inside ROOT or is a default
    coreslot location."""
    state, root = os.path.realpath(cfg["state_dir"]), os.path.realpath(ROOT)

    def within(path, top):
        return os.path.commonpath([path, top]) == top

    defaults = [os.path.realpath(os.path.expanduser(d)) for d in DEFAULT_DIRS]
    if state == root or not within(state, root) or any(within(state, d) for d in defaults):
        sys.exit(f"run4: refusing state directory {state}: it must lie inside {root} and be no"
                 " default coreslot location")
    return state


def cs(cfg, *args, runner=subprocess.run, env=None, **kw):
    """Runs coreslot with the stand's state directory; the only place the binary is started."""
    checked_state(cfg)
    e = dict(os.environ, **(env or {}), CORESLOT_STATE_DIR=cfg["state_dir"])
    return runner([cfg["coreslot"], *args], env=e, **kw)


def fresh_state(cfg):
    state = checked_state(cfg)
    if os.path.lexists(state):
        shutil.rmtree(state)
    os.makedirs(state, mode=0o700)
    os.chmod(state, 0o700)


def write_config(cfg, cpu, mem, live=None):
    """Writes the shares, and [memory] live when live is a bool (run 4 wrote no [memory])."""
    with open(os.path.join(checked_state(cfg), "config.toml"), "w") as f:
        f.write(f'[resources]\ncpu = "{cpu}"\nmem = "{mem}"\n')
        if live is not None:
            f.write(f"[memory]\nlive = {str(live).lower()}\n")


def budget_empty(cfg):
    r = cs(cfg, "status", capture_output=True, text=True)
    return "holds: none" in r.stdout and "queue: empty" in r.stdout


def sha256(path):
    return hashlib.sha256(open(path, "rb").read()).hexdigest()


# --- free text --------------------------------------------------------------------------------

def _user():
    return getpass.getuser()


def _hosts():
    h = socket.gethostname()
    return sorted({h, h.split(".")[0]} - {""}, key=len, reverse=True)


def scrub(line):
    """The line with $HOME as ~, the host name as <host> and the user name as <user>."""
    line = line.replace(os.path.expanduser("~"), "~")
    for h in _hosts():
        line = line.replace(h, "<host>")
    u = _user()
    return line.replace(u, "<user>") if u else line


def fail_lines(gate, output):
    pattern = gate.get("fail_pattern")
    if not pattern:
        return []
    return [scrub(l) for l in output.splitlines() if re.search(pattern, l)][:50]


# --- running gates ----------------------------------------------------------------------------

_live, _live_lock = set(), threading.Lock()


def run_gate(argv, **kw):
    """Runs argv in a session of its own with stdout and stderr together, so an interrupt can
    stop exactly the process groups this runner started."""
    p = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                         stdin=subprocess.DEVNULL, text=True, errors="replace",
                         start_new_session=True, **kw)
    with _live_lock:
        _live.add(p)
    try:
        out, _ = p.communicate()
    finally:
        with _live_lock:
            _live.discard(p)
    return subprocess.CompletedProcess(argv, p.returncode, out, "")


def stop_live():
    with _live_lock:
        for p in _live:
            try:
                os.killpg(p.pid, signal.SIGTERM)
            except (ProcessLookupError, PermissionError):
                pass


def run_one(cfg, gate, arm):
    """Runs one gate in the arm's form and returns (exit, output); every arm starting with K is
    a coreslot arm."""
    if arm.startswith("K"):
        r = cs(cfg, "run", "--label", gate["label"], "--", *gate["argv"], runner=run_gate,
               cwd=gate["cwd"], env=gate.get("env", {}))
    else:
        argv = [gate.get("free_n", "auto") if a == "{n}" else a for a in gate["argv"]]
        r = run_gate(argv, cwd=gate["cwd"], env=dict(os.environ, **gate.get("env", {})))
    return r.returncode, r.stdout


# --- the machine around a set -----------------------------------------------------------------

def on_ac():
    r = subprocess.run(["pmset", "-g", "batt"], capture_output=True, text=True)
    return "'AC Power'" in r.stdout if r.returncode == 0 else None


def background():
    """Totals only: memory in use (active + wired + compressed pages), load1, process count."""
    r = subprocess.run(["vm_stat"], capture_output=True, text=True)
    m = re.search(r"page size of (\d+) bytes", r.stdout)
    page = int(m.group(1)) if m else 16384
    pages = sum(int(x) for x in re.findall(
        r"^Pages (?:active|wired down|occupied by compressor):\s+(\d+)", r.stdout, re.M))
    ps = subprocess.run(["ps", "-A", "-o", "pid="], capture_output=True, text=True)
    return page, {"mem_in_use_mib": pages * page // 2**20,
                  "load1": round(os.getloadavg()[0], 2),
                  "processes": len(ps.stdout.split())}


def invalid_reasons(samples, ac, error):
    reasons = []
    if error:
        reasons.append(f"runner_error: {error}")
    if ac is False:
        reasons.append("battery")
    if any(b["t"] - a["t"] > 10 for a, b in zip(samples, samples[1:])):
        reasons.append("sleep_gap")
    return reasons


def pacing(cfg):
    return SimpleNamespace(cooldown=cfg.get("cooldown_s", 20),
                           probe_tolerance=cfg.get("probe_tolerance", 0.10),
                           max_cooldown=cfg.get("max_cooldown_s", 120))


def offsets_for(cfg, seed):
    rnd = random.Random(seed)
    return {n: round(rnd.uniform(0, cfg["window_s"]), 1) for n in cfg["agents"]}


def run_set(cfg, path, *, arm, offsets, meta, baseline, slots=None, kshare=None, needs=None):
    """Runs one set of arm and appends its record to path. An arm starting with S holds each
    heavy gate on one of `slots` slots; a K arm copies needs into the state directory, or with
    needs None removes the needs file there, a cold start."""
    cd = acceptance.cooldown(pacing(cfg), baseline)
    ac = on_ac()
    page, bg = background()
    errors = []
    if arm.startswith("K"):
        write_config(cfg, kshare["cpu"], kshare["mem"], kshare.get("live"))
        dst = os.path.join(checked_state(cfg), "needs.toml")
        if needs:
            shutil.copyfile(needs, dst)
        elif os.path.lexists(dst):
            os.remove(dst)
        want = sha256(needs) if needs else None
        if not budget_empty(cfg):
            sys.exit(f"run4: {meta}: the budget is not empty")
    runlog0 = len(acceptance.runlog_lines(cfg))
    sem = threading.Semaphore(slots or 1)
    gates, failures, lock = [], [], threading.Lock()
    t0 = time.time()

    def agent(name, chain):
        try:
            time.sleep(offsets[name])
            for g in chain:
                held = arm.startswith("S") and g["heavy"]
                if held:
                    sem.acquire()
                s = time.time()
                try:
                    code, out = run_one(cfg, g, arm)
                finally:
                    if held:
                        sem.release()
                e = time.time()
                with lock:
                    gates.append({"agent": name, "gate": g["gate"], "start": round(s - t0, 1),
                                  "end": round(e - t0, 1), "exit": code})
                    if code:
                        failures.append({"agent": name, "gate": g["gate"],
                                         "lines": fail_lines(g, out)})
        except Exception as exc:  # a runner error invalidates the set, not the run
            with lock:
                errors.append(scrub(f"{name}: {exc!r}"))

    sampler = overload.Sampler()
    sampler.start()
    threads = [threading.Thread(target=agent, args=(n, c), daemon=True)
               for n, c in cfg["agents"].items()]
    for t in threads:
        t.start()
    try:
        for t in threads:
            while t.is_alive():
                t.join(0.5)
    except KeyboardInterrupt:
        stop_live()
        raise
    finally:
        sampler.stop.set()
        sampler.join()
    if arm.startswith("K") and (sha256(dst) if os.path.exists(dst) else None) != want:
        errors.append("the needs file changed during the set")
    verdict = {n: round(max(g["end"] for g in gates if g["agent"] == n) - offsets[n], 1)
               for n in cfg["agents"] if any(g["agent"] == n for g in gates)}
    runlog = ([json.loads(l) for l in acceptance.runlog_lines(cfg)[runlog0:]]
              if arm.startswith("K") else [])
    flags = (["hot_start"] if cd["cooldown_gave_up"] else []) + (
        ["high_load"] if bg["load1"] > 3 else [])
    reasons = invalid_reasons(sampler.samples, ac, "; ".join(errors))
    rec = {**meta, "arm": arm, "started": datetime.fromtimestamp(t0, timezone.utc).isoformat(),
           "probe_baseline_s": baseline, **cd, "ac": ac, "background": bg, "page_bytes": page,
           "flags": flags, "valid": not reasons, "invalid": reasons, "offsets": offsets,
           "gates": sorted(gates, key=lambda g: (g["agent"], g["start"])), "failures": failures,
           "verdict_s": verdict,
           "mean_verdict_s": round(statistics.mean(verdict.values()), 1) if verdict else None,
           "makespan_s": round(max(g["end"] for g in gates), 1) if gates else None,
           "waited_free_s": round(sum(l.get("waited_free_s") or 0 for l in runlog), 1)
           if arm.startswith("K") else None,
           "samples": sampler.samples, "runlog": runlog}
    with open(path, "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(f"{meta} {arm}: mean verdict {rec['mean_verdict_s']} s, makespan {rec['makespan_s']} s,"
          f" warn+ {warn_s(rec)} s, failed {[x['gate'] for x in failures]},"
          f" valid {rec['valid']} {reasons or ''}", flush=True)


# --- phases -----------------------------------------------------------------------------------

def read_jsonl(path):
    return [json.loads(l) for l in open(path)] if os.path.exists(path) else []


def unique_gates(cfg):
    return list({g["gate"]: g for chain in cfg["agents"].values() for g in chain}.values())


def gate_baseline(out):
    """gate -> whether its alone run in the F form passed (the last such run of learn)."""
    return {r["gate"]: r["exit"] == 0 for r in read_jsonl(os.path.join(out, "learn.jsonl"))
            if r.get("form") == "F"}


def learn(cfg, out):
    for later in ("tune.jsonl", "results.jsonl"):
        if os.path.exists(os.path.join(out, later)):
            print(f"run4: learn would replace the needs {later} used", file=sys.stderr)
            sys.exit(2)
    os.makedirs(out, exist_ok=True)
    path = os.path.join(out, "learn.jsonl")
    if os.path.exists(path):
        os.remove(path)
    fresh_state(cfg)
    write_config(cfg, "140%", "60%")

    def alone(g, rnd, form):
        started, t0 = datetime.now(timezone.utc).isoformat(timespec="seconds"), time.time()
        code, output = run_one(cfg, g, form)
        rec = {"started": started, "gate": g["gate"], "round": rnd, "form": form, "exit": code,
               "wall_s": round(time.time() - t0, 1),
               "failures": fail_lines(g, output) if code else []}
        with open(path, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(json.dumps(rec), flush=True)

    for rnd in (1, 2):
        for g in unique_gates(cfg):
            alone(g, rnd, "K")
        r = cs(cfg, "suggest", "--write", capture_output=True, text=True)
        print(scrub(r.stdout + r.stderr), end="", flush=True)
        if r.returncode:
            sys.exit(f"run4: suggest --write exited {r.returncode}")
    for g in unique_gates(cfg):
        alone(g, 3, "F")
    r = cs(cfg, "stats", "--json", capture_output=True, text=True)
    if r.returncode:
        sys.exit(f"run4: stats --json exited {r.returncode}")
    with open(os.path.join(out, "stats-learn.json"), "w") as f:
        f.write(scrub(r.stdout))
    shutil.copyfile(os.path.join(checked_state(cfg), "needs.toml"), os.path.join(out, "needs.toml"))


def pct(v):
    return float(str(v).rstrip("%"))


def select_s(medians):
    """N -> median mean time to verdict; the lower median wins, a tie goes to fewer slots."""
    return min(medians, key=lambda n: (medians[n], n))


def select_k(confs):
    """The lowest median among configurations with no critical second and no false failure;
    when none qualifies, the fewest critical seconds, then false failures, then the median.
    Ties go to the lower mem share, then the lower cpu share. One ordering does both: a
    qualifying configuration has the least possible critical seconds and false failures."""
    return min(confs, key=lambda n: (confs[n]["critical_s"], confs[n]["false_failures"],
                                     confs[n]["median"], pct(confs[n]["mem"]),
                                     pct(confs[n]["cpu"])))


def tune_configs(cfg):
    t = {**TUNE, **cfg.get("tune", {})}
    confs = [(f"S{n}", "S", {"slots": n}) for n in t["S"]]
    confs += [(f"K-{c}-{m}", "K", {"kshare": {"cpu": c, "mem": m}}) for c, m in t["K"]]
    return confs, t["reps"]


def tune(cfg, out):
    confs, reps = tune_configs(cfg)
    path, needs = os.path.join(out, "tune.jsonl"), os.path.join(out, "needs.toml")
    done = {(r["config"], r["rep"]) for r in read_jsonl(path)}
    pending = [(rep, c) for rep in range(1, reps + 1) for c in confs if (c[0], rep) not in done]
    baseline = acceptance.probe() if pending else None
    for rep, (name, arm, kw) in pending:
        run_set(cfg, path, arm=arm, offsets=offsets_for(cfg, f"run4-tune-{name}-{rep}"),
                meta={"block": None, "as_block": None, "config": name, "rep": rep},
                baseline=baseline, needs=needs, **kw)
    recs, base = read_jsonl(path), gate_baseline(out)
    per = {}
    for name, arm, kw in confs:
        rs = [r for r in recs if r["config"] == name]
        per[name] = {"median": statistics.median(r["mean_verdict_s"] for r in rs),
                     "critical_s": sum(critical_s(r) for r in rs),
                     "false_failures": sum(false_failures(r["gates"], base) for r in rs),
                     **kw.get("kshare", {})}
    n = select_s({kw["slots"]: per[name]["median"] for name, arm, kw in confs if arm == "S"})
    k = select_k({name: per[name] for name, arm, _ in confs if arm == "K"})
    tuning = {"S": {"N": n}, "K": {"cpu": per[k]["cpu"], "mem": per[k]["mem"]},
              "medians": {name: p["median"] for name, p in per.items()},
              "critical_s": {name: p["critical_s"] for name, p in per.items()},
              "false_failures": {name: p["false_failures"] for name, p in per.items()}}
    json.dump(tuning, open(os.path.join(out, "tuning.json"), "w"), indent=1)
    print(json.dumps(tuning, indent=1))


def verdict(cfg, out, first, last, as_block):
    tuning = json.load(open(os.path.join(out, "tuning.json")))
    path, needs = os.path.join(out, "results.jsonl"), os.path.join(out, "needs.toml")
    done = {(r["block"], r["arm"]) for r in read_jsonl(path)}
    pending = [(b, arm) for b in range(first, last + 1) for arm in order_for(as_block or b)
               if (b, arm) not in done]
    baseline = acceptance.probe() if pending else None
    kshare, n = tuning["K"], tuning["S"]["N"]
    names = {"F": "F", "S": f"S{n}", "K": f"K-{kshare['cpu']}-{kshare['mem']}"}
    for b, arm in pending:
        src = as_block or b
        run_set(cfg, path, arm=arm, offsets=offsets_for(cfg, f"run4-{src}"),
                meta={"block": b, "as_block": as_block, "order": "".join(order_for(src)),
                      "config": names[arm]},
                baseline=baseline, slots=n, kshare=kshare, needs=needs)


# --- analysis ---------------------------------------------------------------------------------

def warn_s(r):
    return sum((s["pressure"] or 1) >= 2 for s in r["samples"])


def critical_s(r):
    return sum((s["pressure"] or 1) >= 4 for s in r["samples"])


def swapout_mib(r):
    c = [s["swapouts"] for s in r["samples"] if s.get("swapouts") is not None]
    return round((c[-1] - c[0]) * r.get("page_bytes", 16384) / 2**20, 1) if len(c) > 1 else 0


def waited_s(r):
    return sum(l.get("waited_s") or 0 for l in r["runlog"])


def false_failures(gates, baseline):
    """Failures of gates that passed their baseline; a gate without a passing one is excluded."""
    return sum(1 for g in gates if g["exit"] and baseline.get(g["gate"]) is True)


def wins(k, other, factor=1.0):
    """Blocks where K is strictly below factor x the other arm; a tie is not a win."""
    return sum(a < factor * b for a, b in zip(k, other))


def analyse(recs, baseline, dry):
    by = {}
    for r in recs:
        if r.get("block") is not None:
            by.setdefault(r["block"], {})[r["arm"]] = r
    complete = [b for b in sorted(by) if all(a in by[b] for a in ARMS)]
    valid = [b for b in complete if all(by[b][a]["valid"] for a in ARMS)][:BLOCKS]
    reruns = [b for b in by if b > BLOCKS]
    full, spent = len(valid) >= BLOCKS, len(reruns) >= RERUN_CAP
    if not full and not spent and not dry:
        print(f"run4: verdict incomplete: {len(valid)} valid blocks, {len(reruns)} of"
              f" {RERUN_CAP} re-runs; no analysis yet", file=sys.stderr)
        sys.exit(2)
    rs = {a: [by[b][a] for b in valid] for a in ARMS}

    def med(xs):
        xs = list(xs)
        return statistics.median(xs) if xs else None

    per_arm = {a: {"mean_verdict_s": med(r["mean_verdict_s"] for r in rs[a]),
                   "makespan_s": med(r["makespan_s"] for r in rs[a]),
                   "warn_s": med(warn_s(r) for r in rs[a]),
                   "critical_s": med(critical_s(r) for r in rs[a]),
                   "swapout_mib": med(swapout_mib(r) for r in rs[a]),
                   "false_failures": med(false_failures(r["gates"], baseline) for r in rs[a]),
                   "false_failures_total": sum(false_failures(r["gates"], baseline) for r in rs[a])}
               for a in ARMS}
    per_arm["K"]["waited_free_s"] = med(r["waited_free_s"] or 0 for r in rs["K"])
    v = {a: [r["mean_verdict_s"] for r in rs[a]] for a in ARMS}
    ratios = {f"K/{o} {key}": [round(by[b]["K"][key] / by[b][o][key], 3) for b in valid]
              for o in ("F", "S") for key in ("mean_verdict_s", "makespan_s")}
    s = {"blocks": valid, "reruns": sorted(reruns), "per_arm": per_arm, "ratios": ratios,
         "tests": None, "decision": None, "incomplete": None}
    if not full and not dry:
        s["incomplete"] = f"incomplete: {len(valid)} valid blocks; no test"
        return s
    n = len(valid)

    def test(w):
        p = sign_test(w, n) if n else 1.0
        return {"wins": w, "n": n, "p": round(p, 4), "pass": p < ALPHA}

    s["tests"] = {"H1": test(wins(v["K"], v["F"])),
                  "H2": test(wins([warn_s(r) for r in rs["K"]], [warn_s(r) for r in rs["F"]])),
                  "H3": test(wins(v["K"], v["S"], H3_FACTOR))}
    h1, h3 = s["tests"]["H1"]["pass"], s["tests"]["H3"]["pass"]
    if h1 and h3:
        first = ("H1 and H3 pass: develop the one-machine scenario further and publish it as the"
                 " headline.")
    elif h1:
        first = ("H1 passes, H3 fails: coreslot beats no admission but not a tuned semaphore; the"
                 " next step is chosen from waited_free_s.")
    else:
        first = "H1 fails: report it; the project's memory scenario is not shown."
    live = sum((r["waited_free_s"] or 0) >= LIVE_FREE_S
               and (r["waited_free_s"] or 0) >= LIVE_SHARE * waited_s(r) for r in rs["K"])
    worth = "is worth designing" if live >= LIVE_BLOCKS else "is not shown worth designing"
    s["decision"] = [first, f"Live-memory admission {worth}: waited_free_s >= 5 s and >= 10% of"
                            f" K's waiting in {live} of {n} blocks."]
    return s


def markdown(s, recs, tuning):
    lines = ["# Acceptance run 4: summary", "",
             f"Tuning: S N = {tuning['S']['N']}; K cpu = {tuning['K']['cpu']},"
             f" mem = {tuning['K']['mem']}.", "",
             f"Valid blocks used: {s['blocks']}; re-runs: {s['reruns']}.", "",
             "Medians over the valid blocks (false failures also as a total):", "",
             "| Arm | Mean verdict (s) | Makespan (s) | Warn+ (s) | Critical (s) | Swapout (MiB)"
             " | False failures (median, total) | waited_free_s |", "|---|---|---|---|---|---|---|---|"]
    for a in ARMS:
        p = s["per_arm"][a]
        lines.append(f"| {a} | {p['mean_verdict_s']} | {p['makespan_s']} | {p['warn_s']} |"
                     f" {p['critical_s']} | {p['swapout_mib']} | {p['false_failures']},"
                     f" {p['false_failures_total']} | {p.get('waited_free_s', '-')} |")
    lines += ["", "Per-block ratios (valid blocks):", ""]
    lines += [f"- {k}: {v}" for k, v in s["ratios"].items()]
    lines += ["", "Every verdict set:", "",
              "| Block | As block | Order | Arm | Mean verdict (s) | Makespan (s) | Warn+ (s)"
              " | Critical (s) | Failed | Flags | Valid |", "|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in sorted((r for r in recs if r.get("block") is not None),
                    key=lambda r: (r["block"], r["order"].index(r["arm"]))):
        failed = ", ".join(f["gate"] for f in r["failures"]) or "none"
        lines.append(f"| {r['block']} | {r['as_block'] or ''} | {r['order']} | {r['arm']} |"
                     f" {r['mean_verdict_s']} | {r['makespan_s']} | {warn_s(r)} | {critical_s(r)} |"
                     f" {failed} | {', '.join(r['flags'])} |"
                     f" {'yes' if r['valid'] else 'no: ' + '; '.join(r['invalid'])} |")
    lines.append("")
    if s["incomplete"]:
        lines += [s["incomplete"], ""]
        return "\n".join(lines)
    lines += ["Tests (one-sided exact sign test, alpha 0.05):", ""]
    names = {"H1": "K's mean time to verdict below F's",
             "H2": "K's seconds at warn-or-worse below F's",
             "H3": "K's mean time to verdict below 1.10 x S's"}
    for h, t in s["tests"].items():
        lines.append(f"- {h} ({names[h]}): {t['wins']} of {t['n']}, p = {t['p']:.4f},"
                     f" {'pass' if t['pass'] else 'fail'}")
    lines += ["", "Decision:", ""] + [f"- {d}" for d in s["decision"]] + [""]
    return "\n".join(lines)


def summary(out, dry):
    recs = read_jsonl(os.path.join(out, "results.jsonl"))
    s = analyse(recs, gate_baseline(out), dry)
    tuning = json.load(open(os.path.join(out, "tuning.json")))
    s["tuning"] = tuning
    json.dump(s, open(os.path.join(out, "summary.json"), "w"), indent=1)
    md = markdown(s, recs, tuning)
    with open(os.path.join(out, "summary.txt"), "w") as f:
        f.write(md)
    print(md)


# --- figures ----------------------------------------------------------------------------------

INK, MUTED, GRID, SURFACE = "#0b0b0b", "#52514e", "#e4e3df", "#fcfcfb"
SERIES = {"K/F": "#2a78d6", "K/S": "#eb6834"}
STATUS = {2: ("#fab219", "warn"), 4: ("#d03b3b", "critical")}


def svg(w, h, body):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{w}" height="{h}" '
            f'viewBox="0 0 {w} {h}" font-family="sans-serif" font-size="12">'
            f'<rect width="{w}" height="{h}" fill="{SURFACE}"/>{"".join(body)}</svg>\n')


def ratios_svg(blocks, rk_f, rk_s):
    w, h, l, r, t, b = 640, 320, 50, 90, 30, 40
    top = max([1.2] + rk_f + rk_s) * 1.05
    x = (lambda i: l + (i + 0.5) * (w - l - r) / max(1, len(blocks)))
    y = (lambda v: t + (1 - v / top) * (h - t - b))
    body = [f'<text x="{l}" y="18" fill="{INK}">Per-block ratio of mean time to verdict'
            ' (below 1: K sooner)</text>']
    for v in [0, 0.5, 1.0, 1.5, 2.0, 2.5, 3.0]:
        if v <= top:
            body.append(f'<line x1="{l}" x2="{w - r}" y1="{y(v):.1f}" y2="{y(v):.1f}" '
                        f'stroke="{GRID}"/><text x="{l - 6}" y="{y(v) + 4:.1f}" '
                        f'text-anchor="end" fill="{MUTED}">{v:g}</text>')
    body.append(f'<line x1="{l}" x2="{w - r}" y1="{y(1):.1f}" y2="{y(1):.1f}" stroke="{INK}" '
                'stroke-dasharray="4 3"/>')
    for i, bl in enumerate(blocks):
        body.append(f'<text x="{x(i):.1f}" y="{h - b + 16}" text-anchor="middle" '
                    f'fill="{MUTED}">{bl}</text>')
    body.append(f'<text x="{(l + w - r) / 2}" y="{h - 6}" text-anchor="middle" '
                f'fill="{MUTED}">block</text>')
    for name, vals in (("K/F", rk_f), ("K/S", rk_s)):
        pts = " ".join(f"{x(i):.1f},{y(v):.1f}" for i, v in enumerate(vals))
        body.append(f'<polyline points="{pts}" fill="none" stroke="{SERIES[name]}" '
                    'stroke-width="2"/>')
        for i, v in enumerate(vals):
            body.append(f'<circle cx="{x(i):.1f}" cy="{y(v):.1f}" r="4" fill="{SERIES[name]}" '
                        f'stroke="{SURFACE}" stroke-width="2"><title>block {blocks[i]}: {name}'
                        f' {v}</title></circle>')
        if vals:
            body.append(f'<text x="{w - r + 8}" y="{y(vals[-1]) + 4:.1f}" '
                        f'fill="{INK}">{name}</text>')
    return svg(w, h, body)


def timeline_svg(block, sets, order, agents):
    w, l, r, row, gap = 760, 90, 20, 18, 34
    end = max(max(g["end"] for g in s["gates"]) for s in sets.values()) or 1
    x = (lambda t: l + t / end * (w - l - r))
    h = 40 + len(order) * (len(agents) * row + 12 + gap) + 30
    body = [f'<text x="{l}" y="18" fill="{INK}">Block {block}: gates per agent over time;'
            ' the band is memory pressure</text>']
    y0 = 40
    for arm in order:
        s = sets[arm]
        t0 = datetime.fromisoformat(s["started"]).timestamp()
        body.append(f'<text x="8" y="{y0 + 12}" fill="{INK}" font-weight="bold">{arm}</text>')
        for i, a in enumerate(agents):
            yy = y0 + i * row
            body.append(f'<text x="{l - 8}" y="{yy + 12}" text-anchor="end" '
                        f'fill="{MUTED}">{a}</text>')
            for g in s["gates"]:
                if g["agent"] != a:
                    continue
                fill = STATUS[4][0] if g["exit"] else SERIES["K/F"]
                body.append(f'<rect x="{x(g["start"]):.1f}" y="{yy + 2}" rx="2" '
                            f'width="{max(1.0, x(g["end"]) - x(g["start"])):.1f}" height="{row - 4}" '
                            f'fill="{fill}" stroke="{SURFACE}" stroke-width="2"><title>{g["gate"]}:'
                            f' {g["start"]}-{g["end"]} s, exit {g["exit"]}</title></rect>')
        band = y0 + len(agents) * row + 2
        for smp in s["samples"]:
            lvl = smp["pressure"] or 1
            if lvl >= 2:
                color, name = STATUS[4 if lvl >= 4 else 2]
                t = smp["t"] - t0
                body.append(f'<rect x="{x(t):.1f}" y="{band}" width="{max(1.0, x(t + 1) - x(t)):.1f}"'
                            f' height="8" fill="{color}"><title>{name} at {t:.0f} s</title></rect>')
        body.append(f'<line x1="{l}" x2="{w - r}" y1="{band + 10}" y2="{band + 10}" '
                    f'stroke="{GRID}"/>')
        y0 += len(agents) * row + 12 + gap
    body.append(f'<text x="{l}" y="{h - 10}" fill="{MUTED}">0 to {end:.0f} s from each set\'s'
                f' start. Bars: blue passed, red failed. Band: amber {STATUS[2][1]},'
                f' red {STATUS[4][1]}.</text>')
    return svg(w, h, body)


def figures(cfg, out):
    recs = read_jsonl(os.path.join(out, "results.jsonl"))
    by = {}
    for r in recs:
        if r.get("block") is not None:
            by.setdefault(r["block"], {})[r["arm"]] = r
    full = [b for b in sorted(by) if all(a in by[b] for a in ARMS)]
    blocks = [b for b in full if all(by[b][a]["valid"] for a in ARMS)] or full
    if not blocks:
        sys.exit("run4: no complete block to draw")
    rk = {o: [round(by[b]["K"]["mean_verdict_s"] / by[b][o]["mean_verdict_s"], 3) for b in blocks]
          for o in ("F", "S")}
    with open(os.path.join(out, "ratios.svg"), "w") as f:
        f.write(ratios_svg(blocks, rk["F"], rk["S"]))
    mid = sorted(zip(rk["F"], blocks))[(len(blocks) - 1) // 2][1]
    with open(os.path.join(out, "timeline.svg"), "w") as f:
        f.write(timeline_svg(mid, by[mid], by[mid]["F"]["order"], list(cfg["agents"])))
    print(f"wrote ratios.svg ({len(blocks)} blocks) and timeline.svg (block {mid})")


def main():
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("learn", "tune", "verdict", "summary", "figures"):
        p = sub.add_parser(name)
        p.add_argument("stand")
        p.add_argument("out")
        if name == "verdict":
            p.add_argument("first", type=int)
            p.add_argument("last", type=int)
            p.add_argument("--as-block", type=int, default=None)
        if name == "summary":
            p.add_argument("--dry", action="store_true")
    args = ap.parse_args()
    cfg = overload.load(args.stand)

    def interrupted(*_):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    if args.cmd == "learn":
        learn(cfg, args.out)
    elif args.cmd == "tune":
        tune(cfg, args.out)
    elif args.cmd == "verdict":
        verdict(cfg, args.out, args.first, args.last, args.as_block)
    elif args.cmd == "summary":
        if args.dry and not cfg.get("dry"):
            sys.exit("run4: --dry is for a dry stand only")
        summary(args.out, args.dry)
    else:
        figures(cfg, args.out)


if __name__ == "__main__":
    main()
