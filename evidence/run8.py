#!/usr/bin/env python3
"""Acceptance run 8: coreslot against fixed slot counts, GNU parallel's --memsuspend on one shared
queue and running everything at once, over a traced agent mix, run 7's heavy and wide mixes and a
long batch with short gates queued behind it.

usage:
  run8.py learn STAND OUT
  run8.py verdict STAND OUT PHASE CELL FIRST LAST [--as-block B] [--primer]
  run8.py power PROBE_OUT OUT
  run8.py pilot-check OUT
  run8.py summary OUT

STAND is run 7's stand (run7.py's docstring) with "agent_names", the four agents' names;
"classes", class -> the gate standing in for that class of the traced batch run; "gates_by_class",
class -> how many of the trace mix's 12 gates it holds; "batch", the long batch gate; and
"behind_class", the class of the short gates queued behind it. A class or batch gate's cwd may hold
"{agent}", replaced by the name of the agent that runs it. Every coreslot call goes through run4.cs()
with the stand's state directory; GNU parallel runs with PARALLEL_HOME=<state_dir>/../parallel-home.

The cells (CELLS): trace-quiet and trace-loaded (K, S1, S2, S3, F on the trace mix), heavy-quiet
and heavy-loaded (K, S1, S2, P on the stand's agents), wide-quiet (K, S1, S2, F on the wide agents)
and batch (KP, K, S1, F), each at its own level (run7.level). The trace mix: the gates of
gates_by_class, in sorted class order, shuffled with the seed "run8-mix" and dealt 3 to each of the
four agents in agent_names order, the same in every arm, block and phase. The batch mix: the first
agent runs the batch gate once at offset 0, the other three run the behind class's gate 3 times
each, all from offset BATCH_DELAY_S (a dry stand may set "batch_delay_s"); no random offsets.

The arms: K runs every gate under coreslot with cpu = "140%", mem = "60%" and OUT/needs.toml; KP
the same, the batch label preemptible by coreslot's automatic rule on its learned entry; the batch
cell's K the same with `preemptible = false` inserted into the batch label's entry (k_needs). S1,
S2 and S3 hold each heavy gate on one of 1, 2 or 3 slots of an in-process counting semaphore, the
light gates free; F runs every gate when its agent reaches it. P submits each heavy gate as one
job line to the set's one `parallel --memsuspend 4.5G -j 4 --joblog` and waits for its joblog
line, the light gates free; a no-op line every TICK_S keeps parallel reading, because it reaps a
finished job, writes its joblog line and re-checks its memory only when it reads its input.

  learn    run 7's learn (6 K-form and 3 F-form runs, suggest, 5 relearn runs, suggest, seal) over
           every gate of the agents, the wide agents and the classes, a class gate's "{agent}" as
           the first agent; before it, the batch gate's LEARN_BATCH_K runs in its K form in pass
           "learn", none in F, none in relearn, each record with its run-log line's
           "preemptible" value (None without one). When the binary lacks --preemptible (R8
           below) the batch gate is skipped. Resumes and refuses as run 7's learn; refuses as
           well once any probe record exists, and on a non-dry stand unless the binary is the
           stand's coreslot_sha256 (the value "unpinned" never is).
  verdict  blocks FIRST..LAST of PHASE (probe, pilot or main) and CELL, each block's arms in the
           order order_for gives it (seed run8-order-<phase>-<cell>-<block>), offsets from the seed
           run8-<phase>-<cell>-<block>. Each set as run 7's: cooldown, level, headroom at t0, the
           agents under a timeout of SET_TIMEOUT_S of the cell's mix (a dry stand may lower it
           with RUN8_SET_TIMEOUT_S), past which every live gate, P's parallel and its jobs are
           killed and the set is invalid with set_timeout. A gate that exits non-zero makes the
           set invalid with gate_failed, except a K arm's coreslot exit (run6.CORESLOT_EXITS),
           which run6.coreslot_failed counts against K instead; v0.3.0's preemptible treatment
           only lowers the batch gate's priority, so it never ends KP's gate. The record is run
           7's plus "cell", "dry", the run log of every arm, each sample's parallel_free_mib
           (vm_stat's free and inactive pages, the figure parallel's --memsuspend checks) and
           "parallel_free_t0_mib" (the same at t0), "level_wait_s" (the level's wait before
           t0), "therm_start" and "therm_end" (`pmset -g therm` at t0 and after the agents,
           None when it fails), "timeout_s" (the set's timeout), "treatment" (treatment();
           non-empty makes the set invalid with "treatment"), "needs_sha256" (the sealed
           needs, every arm), "derived_needs_sha256" (the batch cell's K), and in the batch cell
           "short_mean_verdict_s" (the mean over the behind agents of their last gate's end minus
           their offset) and "batch_s" (the batch gate's end minus its start). Each gate carries
           "cpu_s", its process tree's user and system seconds (os.wait4), None for P's queued
           jobs, and a P gate "submitted". The batch cell, when
           `coreslot run --preemptible=false -- true` exits 64 (a usage error: no such flag),
           writes OUT/batch-skipped.json once and exits 0; any exit but 0 and 64, or batch-skipped.json while it exits 0, exits 2. Exit 2
           when P is to run and parallel is not on PATH; on a non-dry stand when the binary is
           not the stand's coreslot_sha256, and on its pilot and main when the evidence tree has
           uncommitted changes; when pilot or main lacks OUT/power.json, or main a pilot_check.json
           whose "go" is true; when the needs files do not match OUT/needs.sha256. A cell not in
           power.json's "n" (dropped or left out), and the batch cell's main when
           pilot_check.json's "batch_futile" is true, exit 0 without a set. P leaves a heavy
           cell's main when pilot_check.json's "p_feasible"[cell] is false. --as-block and
           --primer as run 7's.
  power    from PROBE_OUT's valid probe blocks (P never decides one): per trace cell S* (the
           fixed count with the lowest mean) and d = ln(K/S*), per batch comparison its log
           ratio and its batch_s KP/K against ln(1 + BATCH_COST_MARGIN), each with its mean, sd,
           k and sigma (stats6.sigma_upper at 0.80); each cell's set seconds (a set_timeout set
           counts its timeout_s, a held P set included, and every set its level_wait_s);
           stats8.power_plan's n (the batch cell's at most BATCH_N_MAX), drops (DROP_ORDER),
           hours (pilot and main blocks) and joint power, with "over_cap", and "batch_treatment"
           (each probe KP set's treatment problems), to OUT/power.json. "hours_parts" splits the
           plan's hours per cell into its sets, their level waits, the K primer (one K set, on
           run8.sh's first cell) and the expected re-runs (the cell's share of probe blocks with
           an invalid set but P's, times the pilot's and main's blocks, capped at PILOT_RERUNS
           and n // 10), and adds "learn_h" once: main's relearn, PROBE_OUT/learn.jsonl's first
           start to its last end. "menu" prices three plans the same way, each with its n, hours
           per cell, total_h, over_cap and what it gives up: "all_arms" (every kept cell at the n
           power asks, with its power), "no_S1" (the same without S1's sets) and "drop_order"
           (DROP_ORDER applied until total_h is at or under CAP_H); when the batch cell alone
           still exceeds CAP_H, the last carries a note and the batch hours scaled to a
           SHORT_BATCH_S batch gate, never measured. A skipped batch cell, one whose every probe
           KP set failed the treatment check, or one short of 2 valid probe blocks, is left out
           ("left_out"). Exit 2 when power.json or any pilot or main record exists in OUT, or a
           non-batch cell has fewer than 2 valid probe blocks.
  pilot-check
           from the first PILOT_BLOCKS valid pilot blocks of each cell power.json keeps (re-runs
           capped at PILOT_RERUNS; a dry cell's blocks present): stats8.futile at alpha 0.10 per
           comparison, P's timeouts and feasibility per heavy cell (at most P_INFEASIBLE_TIMEOUTS
           per PILOT_BLOCKS blocks taken, pro rata), "go", "batch_futile" and the
           declared "outcome", to OUT/pilot_check.json. A cell with fewer than 2 blocks is
           untested; "go" is false when a primary cell is futile or untested. Exit 2 without
           power.json, once a main record exists, or while a non-dry
           cell is short with re-runs left.
  summary  from each kept cell's first n valid main blocks (n from power.json, re-runs up to
           10 % of n; a dry cell's blocks present): the primary, secondary and H-PRE families
           (the secondary its own Holm family whatever the primary's outcome), the primary's
           joint power and every cell's effect sizes, to OUT/summary.json and OUT/summary.txt;
           a cell main did not run takes its pilot blocks' effect sizes, marked exploratory. A
           cell short of n with the re-runs spent is untested. After a pilot check whose "go" is
           false it summarises the pilot instead ("phase": "pilot", "exploratory": true): no
           family tested, every planned cell's effect sizes and the pilot's futility legs. Exit
           2 without power.json or pilot_check.json, or while a non-dry cell is short with
           re-runs left.
"""
import argparse
import contextlib
import glob
import hashlib
import json
import math
import os
import random
import re
import shlex
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
import overload  # noqa: E402  (Sampler, headroom_mib)
import run4  # noqa: E402  (cs, run_one, write_config, offsets_for, ...)
import run6  # noqa: E402  (measured, check_needs, write_learn_state, apps_meta, reruns, ...)
import run7  # noqa: E402  (learn, level, provenance, kill_tree, parallel_home, ...)
import stats6  # noqa: E402
import stats7  # noqa: E402
import stats8  # noqa: E402

CELLS = {"trace-quiet": ("trace", "quiet", ("K", "S1", "S2", "S3", "F")),
         "trace-loaded": ("trace", "loaded", ("K", "S1", "S2", "S3", "F")),
         "heavy-quiet": ("heavy", "quiet", ("K", "S1", "S2", "P")),
         "heavy-loaded": ("heavy", "loaded", ("K", "S1", "S2", "P")),
         "wide-quiet": ("wide", "quiet", ("K", "S1", "S2", "F")),
         "batch": ("batch", "quiet", ("KP", "K", "S1", "F"))}
PHASES = ("probe", "pilot", "main")
RECORDS = tuple(f"{p}-*.jsonl" for p in PHASES)
K_SHARE = run7.K_SHARE
QUIET_HEADROOM_MIB = run7.QUIET_HEADROOM_MIB  # (probe) the levels are run7.level's
LOADED_HEADROOM_MIB = run7.LOADED_HEADROOM_MIB  # (probe)
ALPHA = 0.05
MARGIN = math.log(1.03)
PILOT_BLOCKS = 10
PILOT_RERUNS = 3
POWER_MIN = 0.80
DESCRIPTIVE_BELOW = 0.5
CAP_H = 30  # (probe)
N_MAX = 200
BATCH_N_MAX = 20  # (probe) the batch cell's n is clamped to [PILOT_BLOCKS, BATCH_N_MAX]
BATCH_DELAY_S = 60  # (probe)
P_MEMSUSPEND = "4.5G"
P_JOBS = 4
P_INFEASIBLE_TIMEOUTS = 2  # of PILOT_BLOCKS, pro rata over the blocks a pilot check takes
SET_TIMEOUT_S = {"trace": 3600, "heavy": 600, "wide": 600, "batch": 10800}  # (probe)
SHORT_BATCH_S = 780  # a batch gate short enough for the cap; never measured
TRACE_AGENTS, TRACE_GATES = 4, 3
BEHIND_RUNS = 3
LEARN_BATCH_K = 6  # the automatic rule's minimum run count of 5, with one run to exclude
TICK_S = 0.25  # P's no-op line; a joblog end is late by at most about this much


def refuse(why):
    print(f"run8: {why}", file=sys.stderr)
    sys.exit(2)


def pin(cfg, sha=None):
    """Exit 2 unless a non-dry stand's binary has the SHA-256 its coreslot_sha256 pins; the
    value "unpinned" never matches."""
    if not cfg.get("dry") and cfg.get("coreslot_sha256") != (sha or run4.sha256(cfg["coreslot"])):
        refuse(f"{cfg['coreslot']} is not the binary the stand's coreslot_sha256 pins")


# --- the stand --------------------------------------------------------------------------------

def load(path):
    """run7.load, with the class and batch gates' cwd expanded and an integer free_n as text."""
    cfg = run7.load(path)
    for g in [*cfg.get("classes", {}).values(), *([cfg["batch"]] if "batch" in cfg else [])]:
        g["cwd"] = os.path.expanduser(g["cwd"])
        if "free_n" in g:
            g["free_n"] = str(g["free_n"])
    return cfg


def agent_names(cfg):
    names = cfg.get("agent_names", [])
    if len(names) < TRACE_AGENTS:
        refuse(f"the stand names {len(names)} agents, not {TRACE_AGENTS}")
    return names[:TRACE_AGENTS]


def placed(g, name):
    """The gate as agent name runs it: "{agent}" in its cwd replaced by name."""
    return dict(g, cwd=g["cwd"].replace("{agent}", name))


def trace_agents(cfg):
    """The trace mix: [class's gate x count] in sorted class order, shuffled with the seed
    "run8-mix" and dealt TRACE_GATES to each agent in agent_names order."""
    classes, counts = cfg.get("classes", {}), cfg.get("gates_by_class", {})
    if sorted(set(counts) - set(classes)) or sum(counts.values()) != TRACE_AGENTS * TRACE_GATES:
        refuse(f"gates_by_class must sum to {TRACE_AGENTS * TRACE_GATES} over the classes")
    deck = [classes[c] for c in sorted(counts) for _ in range(counts[c])]
    random.Random("run8-mix").shuffle(deck)
    return {n: [placed(g, n) for g in deck[TRACE_GATES * i:TRACE_GATES * (i + 1)]]
            for i, n in enumerate(agent_names(cfg))}


def batch_delay(cfg):
    if "batch_delay_s" in cfg and not cfg.get("dry"):
        refuse("only a dry stand may set batch_delay_s")
    return cfg.get("batch_delay_s", BATCH_DELAY_S)


def batch_agents(cfg):
    """The batch mix: the first agent runs the batch gate, the others BEHIND_RUNS of the behind
    class's gate each."""
    first, *behind = agent_names(cfg)
    gate = cfg["classes"][cfg["behind_class"]]
    return {first: [placed(cfg["batch"], first)],
            **{n: [placed(gate, n)] * BEHIND_RUNS for n in behind}}


def cell_cfg(cfg, cell):
    """The stand with the cell's mix as its agents; the batch mix's cfg names its batch agent."""
    mix = CELLS[cell][0]
    if mix == "trace":
        return dict(cfg, agents=trace_agents(cfg))
    if mix == "batch":
        batch_delay(cfg)
        return dict(cfg, agents=batch_agents(cfg), batch_agent=agent_names(cfg)[0])
    return run7.mix_cfg7(cfg, mix)


def offsets8(scfg, cell, seed):
    if CELLS[cell][0] != "batch":
        return run4.offsets_for(scfg, seed)
    return {n: 0 if n == scfg["batch_agent"] else batch_delay(scfg) for n in scfg["agents"]}


def batch_label(cfg):
    return (cfg.get("batch") or {}).get("label")


# --- learn ------------------------------------------------------------------------------------

def preemptible_supported(cfg):
    """Whether `coreslot run --preemptible=false -- true`, in a scratch state directory beside
    the stand's (so its run-log line never reaches learn's suggest), exits 0; exit 64, coreslot's
    usage error (what an unknown flag gives), is unsupported, and any other exit refuses."""
    scratch = dict(cfg, state_dir=cfg["state_dir"] + "-preemptible-probe")
    run4.fresh_state(scratch)
    r = run4.cs(scratch, "run", "--preemptible=false", "--", "true", capture_output=True,
                text=True)
    if r.returncode not in (0, 64):
        refuse(f"cannot tell whether {cfg['coreslot']} supports --preemptible: exit"
               f" {r.returncode}: {(r.stderr.splitlines() or [''])[0]}")
    return r.returncode == 0


def learn_batch(cfg, out, recs):
    """The batch gate's LEARN_BATCH_K K-form runs, pass "learn", recorded as run7.learn does."""
    g, path = placed(cfg["batch"], agent_names(cfg)[0]), os.path.join(out, "learn.jsonl")
    done = {r["run"] for r in recs if (r["gate"], r["form"], r["pass"]) == (g["gate"], "K", "learn")}
    todo = [n for n in range(1, LEARN_BATCH_K + 1) if n not in done]
    if not todo:
        return
    if not preemptible_supported(cfg):
        print("run8: the coreslot binary lacks --preemptible; learn skips the batch gate")
        return
    pacing, baseline = run4.pacing(cfg), acceptance.probe()
    pacing.cooldown = max(pacing.cooldown, run7.LEARN_COOLDOWN_S)
    for n in todo:
        acceptance.cooldown(pacing, baseline)
        started = datetime.now(timezone.utc).isoformat(timespec="seconds")
        log0 = len(acceptance.runlog_lines(cfg))
        code, output, peak, cores, wall = run6.measured(cfg, g, "K")
        new = acceptance.runlog_lines(cfg)[log0:]
        line = json.loads(new[-1]) if new else {}
        rec = {"started": started, "gate": g["gate"], "label": g["label"], "form": "K",
               "pass": "learn", "run": n, "exit": code, "wall_s": wall, "peak_kib": peak,
               "cores": cores, "failures": run4.fail_lines(g, output) if code else [],
               "ran_s": line.get("ran_s"), "preemptible": line.get("preemptible")}
        with open(path, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(json.dumps(rec), flush=True)


def learn(cfg, out):
    if os.path.exists(os.path.join(out, "needs.sha256")):
        print("run8: the needs are sealed in OUT/needs.sha256; learn has nothing to do")
        return
    for pattern in RECORDS:
        if glob.glob(os.path.join(out, pattern)):
            refuse(f"learn would change the needs {pattern} used")
    pin(cfg)
    os.makedirs(out, exist_ok=True)
    state = os.path.join(out, "learn-state.json")
    if os.path.exists(state):
        with open(state) as f:
            if json.load(f)["state_dir"] != run4.scrub(cfg["state_dir"]):
                refuse("learn started in another state directory")
    else:
        run4.fresh_state(cfg)
        run6.write_learn_state(cfg, out)
    run4.write_config(cfg, K_SHARE["cpu"], K_SHARE["mem"])
    recs = run4.read_jsonl(os.path.join(out, "learn.jsonl"))
    if cfg.get("batch") and not any(r["pass"] == "relearn" for r in recs):
        learn_batch(cfg, out, recs)
    first = agent_names(cfg)[0]
    classes = {f"class:{c}": [placed(g, first)] for c, g in sorted(cfg.get("classes", {}).items())}
    run7.learn(dict(cfg, agents={**cfg["agents"], **classes}), out)


# --- the treatment check and the batch cell's K needs -----------------------------------------

def treatment(cell, arm, runlog, batch_label):
    """The problems with a set's treatment, from its run-log lines: (a) a preemptible key on any
    label but the batch label; (b) one on the batch label in any arm but KP; (c) in the batch
    cell's KP, no batch-label line, or one whose preemptible reason is not the automatic
    rule's ("auto: ...")."""
    problems = []
    for line in runlog:
        if "preemptible" not in line:
            continue
        if line.get("label") != batch_label:
            problems.append(f"preemptible on {line.get('label')}")
        elif arm != "KP":
            problems.append(f"preemptible on the batch label in {arm}")
    if cell == "batch" and arm == "KP":
        mine = [l for l in runlog if l.get("label") == batch_label]
        if not mine:
            problems.append("no run-log line of the batch label")
        problems += [f"the batch label's preemptible reason is {l.get('preemptible')!r}"
                     for l in mine if not str(l.get("preemptible") or "").startswith("auto: ")]
    return problems


def k_needs(text, label):
    """needs.toml's text with `    preemptible = false` right after the label's table header;
    ValueError when the header is not there exactly once or the entry has a preemptible key."""
    header = f'  [needs."{label}"]'
    lines = text.splitlines(keepends=True)
    at = [i for i, l in enumerate(lines) if l.rstrip("\n") == header]
    if len(at) != 1:
        raise ValueError(f"{header.strip()} appears {len(at)} times in needs.toml")
    for l in lines[at[0] + 1:]:
        if l.lstrip().startswith("["):
            break
        if l.split("=")[0].strip() == "preemptible":
            raise ValueError(f"{header.strip()} already has a preemptible key")
    lines.insert(at[0] + 1, "    preemptible = false\n")
    return "".join(lines)


# --- sets -------------------------------------------------------------------------------------

def parallel_free_mib():
    """parallel's own free figure: vm_stat's free and inactive pages, in MiB."""
    r = subprocess.run(["vm_stat"], capture_output=True, text=True)
    m = re.search(r"page size of (\d+) bytes", r.stdout)
    pages = re.findall(r"^Pages (?:free|inactive):\s+(\d+)", r.stdout, re.M)
    return sum(map(int, pages)) * int(m.group(1)) >> 20 if m and len(pages) == 2 else None


class FreeSamples(list):
    """overload.Sampler's samples, each with parallel_free_mib added as it is taken."""

    def append(self, s):
        super().append(dict(s, parallel_free_mib=parallel_free_mib()))


def sampler():
    s = overload.Sampler()
    s.samples = FreeSamples()
    return s


def therm():
    """`pmset -g therm`'s output, trimmed; None when the command fails."""
    t = run7.output("pmset", "-g", "therm")
    return None if t is None else t.strip()


def run_gate(argv, **kw):
    """run4.run_gate, reaped with os.wait4: the result's cpu_s is the user and system seconds of
    the gate's process and every descendant it waited for, never the runner's own."""
    p = subprocess.Popen(argv, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                         stdin=subprocess.DEVNULL, text=True, errors="replace",
                         start_new_session=True, **kw)
    with run4._live_lock:
        run4._live.add(p)
    try:
        with p.stdout:
            out = p.stdout.read()
        _, status, ru = os.wait4(p.pid, 0)
        p.returncode = os.waitstatus_to_exitcode(status)
    finally:
        with run4._live_lock:
            run4._live.discard(p)
    r = subprocess.CompletedProcess(argv, p.returncode, out, "")
    r.cpu_s = round(ru.ru_utime + ru.ru_stime, 2)
    return r


def run_one(cfg, gate, arm):
    """run4.run_one through run_gate: (exit, output, cpu_s)."""
    if arm.startswith("K"):
        r = run4.cs(cfg, "run", "--label", gate["label"], "--", *gate["argv"], runner=run_gate,
                    cwd=gate["cwd"], env=gate.get("env", {}))
    else:
        r = run_gate(run4.f_argv(gate), cwd=gate["cwd"],
                     env=dict(os.environ, **gate.get("env", {})))
    return r.returncode, r.stdout, getattr(r, "cpu_s", None)


class Queue:
    """P's one `parallel` per set, reading job lines on its stdin; its joblog in directory d."""

    def __init__(self, cfg, d):
        os.makedirs(d, exist_ok=True)
        self.dir, self.joblog, self.seq = d, os.path.join(d, "joblog"), 0
        self.lock, self.done = threading.Lock(), threading.Event()
        self.proc = subprocess.Popen(
            ["parallel", "--will-cite", "--memsuspend", P_MEMSUSPEND, "-j", str(P_JOBS),
             "--joblog", self.joblog], stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL, text=True, start_new_session=True,
            env=dict(os.environ, PARALLEL_HOME=run7.parallel_home(cfg), PARALLEL_SHELL="/bin/sh"))
        with run4._live_lock:  # so the watchdog's stop() kills it and its jobs
            run4._live.add(self.proc)
        threading.Thread(target=self.tick, daemon=True).start()

    def submit(self, line_for):
        """Writes line_for(seq) as job seq, parallel's Seq: the input line's number."""
        with self.lock:
            self.proc.stdin.write(line_for(self.seq + 1) + "\n")
            self.proc.stdin.flush()
            self.seq += 1
            return self.seq

    def tick(self):
        while not self.done.wait(TICK_S):
            with contextlib.suppress(OSError, ValueError):
                self.submit(lambda n: ":")

    def line(self, seq):
        with contextlib.suppress(FileNotFoundError), open(self.joblog) as f:
            for l in f:
                p = l.rstrip("\n").split("\t")
                if p[0] == str(seq) and len(p) >= 9:
                    return p
        return None

    def run(self, g, expired):
        """Submits gate g as one job and waits for its joblog line: (exit, output, start, end,
        submitted), exit -signal when a signal ended it; None when the set expired first."""
        argv = [g.get("free_n", "auto") if a == "{n}" else a for a in g["argv"]]
        env = [f"{k}={v}" for k, v in g.get("env", {}).items()]
        out = os.path.join(self.dir, "job-{}.out")
        submitted = time.time()
        seq = self.submit(lambda n: f"cd {shlex.quote(g['cwd'])} && "
                          + shlex.join(["env", *env, *argv] if env else argv)
                          + f" > {shlex.quote(out.format(n))} 2>&1")
        while (p := self.line(seq)) is None:
            if self.proc.poll() is not None and (p := self.line(seq)) is None:
                if expired.is_set():
                    return None
                raise RuntimeError(f"parallel exited {self.proc.returncode} before job {seq}")
            time.sleep(0.1)
        start, sig, output = float(p[2]), int(p[7]), ""
        with contextlib.suppress(FileNotFoundError), open(out.format(seq), errors="replace") as f:
            output = f.read()
        return -sig if sig else int(p[6]), output, start, start + float(p[3]), submitted

    def close(self):
        self.done.set()
        with self.lock, contextlib.suppress(OSError, ValueError):
            self.proc.stdin.close()
        try:
            self.proc.wait(30)
        except subprocess.TimeoutExpired:
            run7.kill_tree([self.proc.pid])
        with run4._live_lock:
            run4._live.discard(self.proc)


def set_timeout(cfg, mix):
    """SET_TIMEOUT_S of the mix; only a dry stand may lower it with RUN8_SET_TIMEOUT_S."""
    v = os.environ.get("RUN8_SET_TIMEOUT_S")
    return float(v) if cfg.get("dry") and v else SET_TIMEOUT_S[mix]


def run_agents(cfg, arm, offsets, slots, timeout, queue_dir):
    """run7.run_agents, P's heavy gates through one Queue in queue_dir, the samples with
    parallel_free_mib. Returns (gates, failures, errors, samples, t0, timed_out)."""
    sem = threading.Semaphore(slots or 1)
    gates, failures, errors, lock, expired = [], [], [], threading.Lock(), threading.Event()
    queue = Queue(cfg, queue_dir) if arm == "P" else None
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
                    s, extra = time.time(), {}
                    if queue and g["heavy"]:
                        job = queue.run(g, expired)
                        if job is None:
                            return
                        code, out, s, e, sub = job  # parallel's joblog holds no CPU time
                        extra = {"submitted": round(sub - t0, 1), "cpu_s": None}
                    else:
                        code, out, cpu = run_one(cfg, g, arm)
                        e, extra = time.time(), {"cpu_s": cpu}
                finally:
                    if held:
                        sem.release()
                with lock:
                    gates.append({"agent": name, "gate": g["gate"], "label": g.get("label"),
                                  "start": round(s - t0, 1), "end": round(e - t0, 1),
                                  "exit": code, **extra})
                    if code:
                        failures.append({"agent": name, "gate": g["gate"],
                                         "lines": run4.fail_lines(g, out)})
        except Exception as exc:  # a runner error invalidates the set, not the run
            with lock:
                errors.append(run4.scrub(f"{name}: {exc!r}"))

    killed = set()

    def stop():  # every gate and P's parallel are in run4._live
        with run4._live_lock:
            todo = [p.pid for p in run4._live if p.pid not in killed]
        killed.update(todo)
        if todo:
            run7.kill_tree(todo)

    smp = sampler()
    smp.start()
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
                    stop()
                t.join(0.5)
    except KeyboardInterrupt:
        expired.set()
        stop()
        raise
    finally:
        if queue:
            queue.close()
        smp.stop.set()
        smp.join()
    return gates, failures, errors, smp.samples, t0, expired.is_set()


def run_set8(cfg, path, *, cell, arm, offsets, meta, baseline, slots=None, needs=None):
    """Runs one set of arm in cell, appends its record to path and returns it: run_set7's record
    with the run log of every arm, the treatment check, and the batch cell's two times. A K arm
    writes the needs text into the state directory."""
    mix, level_name = CELLS[cell][:2]
    set_id = f"run8-{os.getpid()}-{time.time_ns()}"
    queue_dir = os.path.join(run7.parallel_home(cfg), f"queue-{set_id}")
    try:
        snap0, c0 = run7.ps_snapshot(), time.monotonic()
        cd = acceptance.cooldown(run4.pacing(cfg), baseline)
        snap1, cooled = run7.ps_snapshot(), time.monotonic() - c0
        own = {os.getpid(), *run7.descendants(snap1, os.getpid())}
        bg_cpu = run7.background_cpu_cores(snap0, snap1, own, cooled)
        ac = run4.on_ac()
        page, bg = run4.background()
        if arm.startswith("K"):
            run4.write_config(cfg, K_SHARE["cpu"], K_SHARE["mem"])
            dst = os.path.join(run4.checked_state(cfg), "needs.toml")
            with open(dst, "w") as f:
                f.write(needs)
            if not run4.budget_empty(cfg):
                sys.exit(f"run8: {meta}: the budget is not empty")
        runlog0, w0 = len(acceptance.runlog_lines(cfg)), time.monotonic()
        with run7.level(level_name) as lvl:
            level_wait = round(time.monotonic() - w0, 1)
            h0, pf0, limit = overload.headroom_mib(), parallel_free_mib(), set_timeout(cfg, mix)
            therm0 = therm()
            gates, failures, errors, samples, t0, timed_out = run_agents(
                cfg, arm, offsets, slots, limit, queue_dir)
            therm1 = therm()
        if arm.startswith("K") and (open(dst).read() if os.path.exists(dst) else None) != needs:
            errors.append("the needs file changed during the set")
        verdict = {n: round(max(g["end"] for g in gates if g["agent"] == n) - offsets[n], 1)
                   for n in cfg["agents"] if any(g["agent"] == n for g in gates)}
        runlog = [json.loads(l) for l in acceptance.runlog_lines(cfg)[runlog0:]]
        problems = treatment(cell, arm, runlog, batch_label(cfg))
        flags = (["hot_start"] if cd["cooldown_gave_up"] else []) + lvl["flags"] + (
            ["level_off"] if run7.level_off(h0, level_name) else []) + (
            ["busy_background"] if bg_cpu > run7.BG_CPU_FLAG_CORES else [])
        failed = [g for g in gates if g["exit"] and not (  # coreslot's own: run6.coreslot_failed
            arm.startswith("K") and g["exit"] in run6.CORESLOT_EXITS)]
        reasons = run4.invalid_reasons(samples, ac, "; ".join(errors)) + (
            ["set_timeout"] if timed_out else []) + (["treatment"] if problems else []) + (
            ["gate_failed"] if failed else [])
        rec = {**meta, "arm": arm, "set_id": set_id,
               "started": datetime.fromtimestamp(t0, timezone.utc).isoformat(),
               "probe_baseline_s": baseline, **cd, "ac": ac, "background": bg,
               "page_bytes": page, "bg_cpu_cores": bg_cpu,
               **{k: v for k, v in lvl.items() if k != "flags"}, "level_wait_s": level_wait,
               "headroom_t0_mib": h0, "parallel_free_t0_mib": pf0, "timeout_s": limit,
               "therm_start": therm0, "therm_end": therm1,
               "flags": flags, "valid": not reasons, "invalid": reasons, "treatment": problems,
               "offsets": offsets, "gates": sorted(gates, key=lambda g: (g["agent"], g["start"])),
               "failures": failures, "verdict_s": verdict,
               "mean_verdict_s": round(statistics.mean(verdict.values()), 1) if verdict else None,
               "makespan_s": round(max(g["end"] for g in gates), 1) if gates else None,
               "waited_free_s": round(sum(l.get("waited_free_s") or 0 for l in runlog), 1)
               if arm.startswith("K") else None,
               "samples": samples, "runlog": runlog}
        if mix == "batch":
            behind = [v for n, v in verdict.items() if n != cfg["batch_agent"]]
            batch = [g for g in gates if g["agent"] == cfg["batch_agent"]]
            rec["short_mean_verdict_s"] = round(statistics.mean(behind), 1) if behind else None
            rec["batch_s"] = round(batch[0]["end"] - batch[0]["start"], 1) if batch else None
        with open(path, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(f"{meta.get('block')} {cell} {arm}: mean verdict {rec['mean_verdict_s']} s,"
              f" makespan {rec['makespan_s']} s, warn+ {run4.warn_s(rec)} s, failed"
              f" {[x['gate'] for x in failures]}, flags {flags}, valid {rec['valid']}"
              f" {reasons or ''} {problems or ''}", flush=True)
        return rec
    finally:
        shutil.rmtree(queue_dir, ignore_errors=True)


# --- verdict ----------------------------------------------------------------------------------

def order_for(phase, cell, block, arms):
    arms = list(arms)
    random.Random(f"run8-order-{phase}-{cell}-{block}").shuffle(arms)
    return arms


def read_json(path):
    if not os.path.exists(path):
        return None
    with open(path) as f:
        return json.load(f)


def verdict(cfg, out, phase, cell, first, last, as_block=None, primer=False):
    mix, level_name, arms = CELLS[cell]
    check = read_json(os.path.join(out, "pilot_check.json")) or {}
    power = read_json(os.path.join(out, "power.json"))
    if phase != "probe":
        if power is None or phase == "main" and not check.get("go"):
            refuse("the pilot runs after power.json, main after a pilot check whose go is true")
        if cell not in power["n"]:
            print(f"run8: {cell} is not in power.json's plan; nothing to run")
            return
        if phase == "main" and cell == "batch" and check.get("batch_futile"):
            print("run8: the pilot check found the batch cell futile; main runs without it")
            return
    if phase == "main" and check.get("p_feasible", {}).get(cell) is False:
        arms = tuple(a for a in arms if a != "P")
    run6.check_needs(out)
    supported, path = cell != "batch" or preemptible_supported(cfg), os.path.join(
        out, "batch-skipped.json")
    if cell == "batch" and supported and os.path.exists(path):
        refuse("the binary changed since the batch cell was skipped")
    if not supported:
        if not os.path.exists(path):
            with open(path, "w") as f:
                json.dump({"reason": "the coreslot binary lacks --preemptible",
                           "binary_sha256": run4.sha256(cfg["coreslot"])}, f, indent=1)
        print("run8: the coreslot binary lacks --preemptible; the batch cell is skipped")
        return
    path = os.path.join(out, f"{phase}-{cell}.jsonl")
    recs = run4.read_jsonl(path)
    if as_block and (as_block in run6.reruns(recs) or not {
            r["arm"] for r in recs if r["block"] == as_block and not r["valid"]} - {"P"}):
        refuse(f"block {as_block} is a re-run or has no invalid set but P's")
    done = {(r["block"], r["arm"]) for r in recs}
    blocks = [(b, [a for a in order_for(phase, cell, as_block or b, arms) if (b, a) not in done])
              for b in range(first, last + 1)]
    blocks = [(b, pending) for b, pending in blocks if pending]
    if not blocks:
        return
    if "P" in arms and not shutil.which("parallel"):
        refuse("arm P needs GNU parallel on PATH")
    prov = run7.provenance(cfg, out, phase)
    if phase == "main":
        prov["power_sha256"] = run4.sha256(os.path.join(out, "power.json"))
    if phase != "probe" and not cfg.get("dry") and prov["tree_dirty"]:  # dry: not evidence
        refuse("the evidence tree has uncommitted changes")
    pin(cfg, prov["binary_sha256"])
    scfg = cell_cfg(cfg, cell)
    with open(os.path.join(out, "needs.toml")) as f:
        sealed = f.read()
    try:
        derived = k_needs(sealed, batch_label(cfg)) if cell == "batch" else None
    except ValueError as e:
        refuse(str(e))
    apps, static, baseline = run6.apps_meta(out), run7.static_sizes7(cfg), acceptance.probe()
    version = run6.coreslot_version(cfg)
    needs_sha = run4.sha256(os.path.join(out, "needs.toml"))
    for i, (b, pending) in enumerate(blocks):
        src = as_block or b
        order = order_for(phase, cell, src, arms)
        offsets = offsets8(scfg, cell, f"run8-{phase}-{cell}-{src}")
        sets = [("K", None, os.path.join(out, "primer.jsonl"))] if primer and i == 0 else []
        sets += [(arm, order.index(arm), path) for arm in pending]
        for arm, pos, dst in sets:
            k = arm.startswith("K")
            needs = derived if arm == "K" and derived else sealed if k else None
            meta = {"phase": phase, "cell": cell, "mix": mix, "block": b, "as_block": as_block,
                    "order": " ".join(order), "level_name": level_name, "position": pos,
                    "prev_arm": order[pos - 1] if pos else None, "mix_agents": list(scfg["agents"]),
                    "dry": bool(cfg.get("dry")), **apps, "coreslot_version": version,
                    "needs_sha256": needs_sha,
                    "derived_needs_sha256": hashlib.sha256(needs.encode()).hexdigest()
                    if k and needs is derived else None,
                    "static": static if k else None, **prov}
            run_set8(scfg, dst, cell=cell, arm=arm, offsets=offsets, meta=meta,
                     baseline=baseline, slots=int(arm[1:]) if arm.startswith("S") else None,
                     needs=needs)


# --- the deciding side: power, pilot check, summary -----------------------------------------

PRIMARY = ("trace-quiet", "trace-loaded")
LOADED = {"trace": "trace-loaded", "heavy": "heavy-loaded"}  # H-SAFE-<mix>'s cells
DROP_ORDER = ("heavy-loaded", "wide-quiet", "trace-loaded")
BATCH_COST_MARGIN = 0.25  # (probe) declared by the pre-registration
# The safety margins are a tolerance on "not above K's", declared by the pre-registration:
SAFETY_WARN_MARGIN_S = 5  # (probe) KP's warn seconds above K's, per block
SAFETY_SWAP_MARGIN_MIB = 64  # (probe) KP's swap-out above K's, per block: run 7's swap event
OUTCOMES = {"go": "main runs every kept cell at power.json's n",
            "stop": "stop: the primary is futile at the pilot; main does not run; the pilot is"
                    " reported as exploratory, with its estimates",
            "batch": "main runs without the batch cell; H-PRE is reported futile at the pilot"}
SHORT = "its cell is short of n, re-runs spent"
DRY_SHORT = "a dry cell with fewer than 2 valid blocks"


def ln8(blocks, cell, a, b, key=None):
    """ln(a / b) per block on key, by default the cell's time metric."""
    key = key or ("short_mean_verdict_s" if cell == "batch" else "mean_verdict_s")
    return [math.log(s[a][key] / s[b][key]) for s in blocks.values()]


def cell_blocks(out, phase, cell, cap=None):
    """The cell's records of phase, without the re-runs past cap, and its valid blocks over every
    arm but P (P never decides a block). A batch set coreslot failed takes its block's slowest
    short_mean_verdict_s and batch_s, as run6.valid_blocks gives such a set the slowest
    mean_verdict_s; its safety stays as measured."""
    recs = run4.read_jsonl(os.path.join(out, f"{phase}-{cell}.jsonl"))
    recs = recs if cap is None else run6.capped(recs, cap)[0]
    blocks = run6.valid_blocks(recs, tuple(a for a in CELLS[cell][2] if a != "P"))
    for sets in blocks.values() if cell == "batch" else ():
        worst = {k: max(r[k] for r in sets.values()) for k in ("short_mean_verdict_s", "batch_s")}
        sets.update({a: dict(r, **worst) for a, r in sets.items() if r.get("coreslot_failed")})
    return recs, blocks


def legs(blocks, cell):
    """S* (a trace cell's fixed count with the lowest mean over the blocks) and the comparisons
    power and futility judge, name -> (d, bound) for the claim mean(d) < bound."""
    if cell in PRIMARY:
        s = run6.lowest(blocks, list(blocks), ("S1", "S2", "S3"))
        return s, {f"K/{s}": (ln8(blocks, cell, "K", s), MARGIN)}
    if cell == "batch":
        return None, {f"KP/{o}": (ln8(blocks, cell, "KP", o), b)
                      for o, b in (("K", 0.0), ("S1", 0.0), ("F", MARGIN))}
    return None, {}


def write_out(out, name, obj):
    with open(os.path.join(out, name), "w") as f:
        json.dump(obj, f, indent=1)
    print(json.dumps(obj, indent=1))


def power(probe_out, out):
    if os.path.exists(os.path.join(out, "power.json")) or any(
            glob.glob(os.path.join(out, p)) for p in RECORDS[1:]):
        refuse("power is planned once, before any pilot or main record")
    skipped = read_json(os.path.join(probe_out, "batch-skipped.json"))
    cells, res = {}, {"S*": {}, "components": {}, "set_s": {}, "left_out": {}}
    arm_s, rate, batch_s = {}, {}, None
    for cell, (_, _, arms) in CELLS.items():
        recs, blocks = cell_blocks(probe_out, "probe", cell)
        if cell == "batch" and not skipped:
            kp = res["batch_treatment"] = [{"block": r["block"], "treatment": r.get(
                "treatment", [])} for r in recs if r["arm"] == "KP"]
            if all(r["treatment"] for r in kp):
                problems = sorted({p for r in kp for p in r["treatment"]})
                skipped = {"reason": "the treatment check failed in every probe KP set: "
                           + "; ".join(problems)}
        if cell == "batch" and (skipped or len(blocks) < 2):
            res["left_out"][cell] = (skipped or {}).get("reason",
                                                        f"{len(blocks)} valid probe blocks")
            continue
        if len(blocks) < 2:
            refuse(f"probe {cell}: {len(blocks)} valid blocks; power needs 2")
        s, comps = legs(blocks, cell)
        if cell == "batch":
            comps["batch_s KP/K"] = (ln8(blocks, cell, "KP", "K", "batch_s"),
                                     math.log(1 + BATCH_COST_MARGIN))
        res["S*"].update({cell: s} if s else {})
        comps = res["components"][cell] = {name: {
            "mean": statistics.fmean(d), "sd": statistics.stdev(d), "k": len(d), "bound": bound,
            "sigma": stats6.sigma_upper(statistics.stdev(d), len(d), 0.80)}
            for name, (d, bound) in comps.items()}
        arm_s[cell] = {a: (statistics.fmean(x for x, _ in t), statistics.fmean(w for _, w in t))
                       for a in arms if (t := [(
                           (r["timeout_s"] if "set_timeout" in r["invalid"] else r["makespan_s"]
                            or 0) + r["cooldown_s"], r.get("level_wait_s") or 0)
                           for r in recs if r["arm"] == a])}
        res["set_s"][cell] = sum(x + w for x, w in arm_s[cell].values())
        rate[cell] = len({r["block"] for r in recs if not r["valid"] and r["arm"] != "P"}) / len(
            {r["block"] for r in recs})
        if cell == "batch":
            batch_s = statistics.median(r["batch_s"] for bs in blocks.values() for r in bs.values())
        cells[cell] = {"set_s": res["set_s"][cell],
                       "comps": [(c["mean"], c["sigma"], c["bound"]) for c in comps.values()]}

    def plan(kept, cap):
        return stats8.power_plan({c: cells[c] for c in kept}, primary=PRIMARY, batch="batch",
                                 drop_order=DROP_ORDER, cap_h=cap, power_min=POWER_MIN,
                                 lo=PILOT_BLOCKS, hi=N_MAX, batch_n_max=BATCH_N_MAX, alpha=ALPHA,
                                 descriptive_below=DESCRIPTIVE_BELOW)

    def parts(cell, n, arms, primer):
        """The cell's hours at n main blocks over arms: its sets, their level waits, the K primer
        and the expected re-runs (whole blocks), capped as the pilot and main cap them."""
        x, w = (sum(arm_s[cell].get(a, (0, 0))[i] for a in arms) for i in (0, 1))
        reruns = min(PILOT_RERUNS, rate[cell] * PILOT_BLOCKS) + min(n // 10, rate[cell] * n)
        return {"sets": (n + PILOT_BLOCKS) * x / 3600, "level_wait": (n + PILOT_BLOCKS) * w / 3600,
                "primer": sum(arm_s[cell]["K"]) / 3600 if primer else 0.0,
                "reruns": reruns * (x + w) / 3600}

    learn_h = learn_hours(probe_out)

    def option(n, without=(), gives_up=(), p=None):
        """n's hours, the primer on run8.sh's first cell, and the learn once."""
        first = next(c for c in CELLS if c in n)
        hp = {c: parts(c, k, [a for a in CELLS[c][2] if a not in without], c == first)
              for c, k in n.items()}
        hours = {c: sum(x.values()) for c, x in hp.items()}
        total = sum(hours.values()) + learn_h
        o = {"n": n, "hours": hours, "learn_h": learn_h, "total_h": total,
             "over_cap": total > CAP_H, "gives_up": list(gives_up)}
        if p:
            o.update(power=p["power"], joint=p["joint"])
        return o, hp

    res.update(plan(cells, CAP_H))
    hp = option(res["n"])[1]
    res["hours_parts"] = {"cells": hp, "learn_h": learn_h,
                          "total_h": sum(sum(x.values()) for x in hp.values()) + learn_h}
    free = plan(cells, math.inf)  # each n as power asks, clamped at BATCH_N_MAX and N_MAX
    s1 = ["H-S1"] + (["batch KP/S1"] if "batch" in cells else []) + [
        f"{c} K/S1 (S1 is the probe's S*)" for c, s in res["S*"].items() if s == "S1"]
    menu = {"all_arms": option(free["n"], p=free)[0],
            "no_S1": option(free["n"], without=("S1",), gives_up=s1)[0]}
    for k in range(len(DROP_ORDER) + 1):
        gone = [c for c in DROP_ORDER[:k] if c in cells]
        p = plan([c for c in cells if c not in gone], math.inf)
        o = option(p["n"], gives_up=gone + [f"H-SAFE-{m}" for m, c in LOADED.items()
                                            if c in gone], p=p)[0]
        if not o["over_cap"]:
            break
    if o["hours"].get("batch", 0) > CAP_H:
        o["note"] = f"needs a shorter batch ({SHORT_BATCH_S} s), not measured"
        o["batch_scaled"] = {"hours": o["hours"]["batch"] * SHORT_BATCH_S / batch_s,
                             "label": "scaled, not measured"}
    res["menu"] = dict(menu, drop_order=dict(o, dropped=gone))
    write_out(out, "power.json", dict(res, cap_h=CAP_H))


def learn_hours(probe_out):
    """Main's relearn from empty, in hours: PROBE_OUT/learn.jsonl from its first run's start to
    its last run's end (0 without one)."""
    t = [(datetime.fromisoformat(r["started"]).timestamp(), r.get("wall_s") or 0)
         for r in run4.read_jsonl(os.path.join(probe_out, "learn.jsonl"))]
    return (max(s + w for s, w in t) - min(s for s, _ in t)) / 3600 if t else 0.0


def pilot_check(out):
    plan = read_json(os.path.join(out, "power.json"))
    if plan is None or glob.glob(os.path.join(out, "main-*.jsonl")):
        refuse("pilot-check judges the pilot after power.json and before any main record")
    res = {"blocks": {}, "S*": {}, "components": {}, "untested": {}, "p_timeouts": {},
           "p_feasible": {}, "dry": False}
    for cell in plan["n"]:
        recs, blocks = cell_blocks(out, "pilot", cell, PILOT_RERUNS)
        dry = any(r.get("dry") for r in recs)
        res["dry"] |= dry
        blocks = run6.first(blocks, PILOT_BLOCKS)
        if len(blocks) < PILOT_BLOCKS and not dry and len(run6.reruns(recs)) < PILOT_RERUNS:
            refuse(f"pilot {cell}: {len(blocks)} of {PILOT_BLOCKS} valid blocks, re-runs left")
        res["blocks"][cell] = list(blocks)
        if "P" in CELLS[cell][2]:
            taken = {s["K"]["block"] for s in blocks.values()}
            k = sum("set_timeout" in r["invalid"] for r in recs
                    if r["arm"] == "P" and r["block"] in taken)
            res["p_timeouts"][cell] = k
            res["p_feasible"][cell] = k * PILOT_BLOCKS <= P_INFEASIBLE_TIMEOUTS * len(taken)
        if len(blocks) < 2:
            res["untested"][cell] = DRY_SHORT if dry else SHORT
            continue
        s, comps = legs(blocks, cell)
        res["S*"].update({cell: s} if s else {})
        if comps:
            res["components"][cell] = {name: {
                "mean": statistics.fmean(d), "bound": bound, "futile": stats8.futile(d, bound),
                "lower90": stats6.paired_lower(d, bound, 0.10)["lower"]}
                for name, (d, bound) in comps.items()}
    futile = [c for c, cs in res["components"].items() if any(x["futile"] for x in cs.values())]
    short = [c for c in PRIMARY if c in res["untested"]]
    stop = OUTCOMES["stop"] if set(futile) & set(PRIMARY) else short and (
        f"stop: {', '.join(short)} untested at the pilot; main does not run; the pilot is"
        " reported as exploratory, with its estimates")
    write_out(out, "pilot_check.json", dict(
        res, futile=futile, go=not stop, batch_futile="batch" in futile,
        outcome=stop or OUTCOMES["batch" if "batch" in futile else "go"]))


def holm8(ps, why=None):
    """Holm at ALPHA with each row's status; every row "not tested (why)" when why is given."""
    if why:
        return [{"name": n, "status": f"not tested ({why})"} for n in ps]
    rows = stats7.holm(ps, ALPHA)
    for r in rows:
        r["status"] = ("untested" if not r["tested"]
                       else "rejected" if r["rejected"] else "not rejected")
    return rows


def secondary(used, plan):
    """H-S1..H-S3, each a union over its legs (slower, less safe) in every kept non-batch cell
    it runs in, and H-SAFE-<mix> for each loaded cell kept."""
    tests = {}
    for s in ("S1", "S2", "S3"):
        ps = {}
        for c, bs in used.items():
            if c != "batch" and s in CELLS[c][2]:
                ps[f"{c} slower"] = stats6.paired_lower(ln8(bs, c, s, "K"), 0)["p"]
                ps[f"{c} less safe"] = stats7.wilcoxon_less(run7.traffic_diffs(bs, "K", s))["p"]
        tests[f"H-{s}"] = {"legs": ps, "p": stats8.union_p(ps.values())}
    for mix, c in LOADED.items():
        if c in plan["n"]:
            tests[f"H-SAFE-{mix}"] = {"p": stats7.wilcoxon_less(run7.traffic_diffs(
                used[c], "K", "S2"))["p"]} if c in used else {"p": None, "untested": SHORT}
    return tests


def h_pre(bs):
    """H-PRE's legs on the batch cell's blocks: time, cost and safety, and Holm over them."""
    def upper(d, bound):
        return stats6.paired_upper(list(d), bound)["p"]
    t = {f"KP<{o}": stats6.paired_lower(ln8(bs, "batch", o, "KP"), 0)["p"] for o in ("K", "S1")}
    t["KP~F"] = upper(ln8(bs, "batch", "KP", "F"), MARGIN)
    safe = {"warn_s": upper((run4.warn_s(s["KP"]) - run4.warn_s(s["K"]) for s in bs.values()),
                            SAFETY_WARN_MARGIN_S),
            "swapout_mib": upper((run4.swapout_mib(s["KP"]) - run4.swapout_mib(s["K"])
                                  for s in bs.values()), SAFETY_SWAP_MARGIN_MIB)}
    res = {"time": {"legs": t, "p": stats8.iut_p(list(t.values()))},
           "cost": {"p": upper(ln8(bs, "batch", "KP", "K", "batch_s"),
                               math.log(1 + BATCH_COST_MARGIN))},
           "safety": {"legs": safe, "p": stats8.iut_p(list(safe.values()))}}
    return dict(res, rows=holm8({k: v["p"] for k, v in res.items()}))


def effects(bs, cell, recs):
    """Each non-K arm against K (KP against K, S1 and F in the batch cell; P over the blocks
    with a valid P set), the batch's own time KP/K, and each arm's warn seconds and swap
    traffic."""
    p = {r["block"]: r for r in recs if r["arm"] == "P" and r["valid"]}
    by = {a: bs for a in CELLS[cell][2] if a != "P"}
    if "P" in CELLS[cell][2]:
        by["P"] = {b: dict(s, P=p[s["K"]["block"]]) for b, s in bs.items() if s["K"]["block"] in p}
    pairs = ([("KP", o) for o in ("K", "S1", "F")] if cell == "batch"
             else [(a, "K") for a in by if a != "K"])
    res = {"ratio": {f"{a}/{b}": run6.estimate(ln8(by[a], cell, a, b)) for a, b in pairs}}
    if cell == "batch":
        res["ratio"]["batch_s KP/K"] = run6.estimate(ln8(bs, cell, "KP", "K", "batch_s"))
    for name, f in (("warn_s", run4.warn_s), ("swap_traffic_pages", run7.swap_traffic_pages)):
        res[name] = {a: run6.ci(f(s[a]) for s in by[a].values()) for a in by if by[a]}
    return res


def summary_text(s):
    p = s["primary"]
    rows = [f"  {r['name']}: {r['status']}" + ("" if r.get("p") is None else f"; p {r['p']:.4g}")
            for r in s["secondary"] + [dict(r, name=f"H-PRE {r['name']}")
                                       for r in s["H-PRE"].get("rows", [])]]
    return "\n".join([
        "Acceptance run 8: summary" + (" (dry)" if s["dry"] else "") + (
            " of the pilot, exploratory: main did not run" if s.get("exploratory") else ""), "",
        f"Blocks: {json.dumps({c: len(bs) for c, bs in s['blocks'].items()})}",
        f"Re-runs: {json.dumps(s['reruns'])}; dropped by the plan: {s['dropped']}",
        f"Untested cells: {json.dumps(s['untested'])}", "",
        "Primary (intersection-union over the kept trace cells, alpha 0.05): " + (
            p["status"] if "status" in p else "untested" if "untested" in p
            else "descriptive, no rejection claimed" if p["descriptive"]
            else "rejected" if p["rejected"] else "not rejected")
        + f"; joint power {p.get('joint_power')}",
        "Secondary family (Holm, alpha 0.05), then H-PRE (Holm over time, cost and safety):",
        *rows, *(f"  H-PRE: {k}, {v}" for k, v in s["H-PRE"].items() if k != "rows" and
                 isinstance(v, str)), "", "Effect sizes:", json.dumps(s["effects"], indent=1), ""])


def piloted(out, cells):
    """Each cell's first PILOT_BLOCKS valid pilot blocks (re-runs capped at PILOT_RERUNS) and its
    pilot records, for the cells with 2 or more."""
    got = {}
    for cell in cells:
        recs, blocks = cell_blocks(out, "pilot", cell, PILOT_RERUNS)
        if len(blocks := run6.first(blocks, PILOT_BLOCKS)) >= 2:
            got[cell] = blocks, recs
    return got


def exploratory(got):
    """The effect sizes of piloted()'s cells, each marked exploratory."""
    return {c: dict(effects(bs, c, recs), exploratory=True) for c, (bs, recs) in got.items()}


def stopped(out, plan, check):
    """The summary after a pilot check whose go is false: no family tested, every planned cell's
    effect sizes from its pilot blocks, and the pilot check's futility legs."""
    why, got = "stopped at the pilot", piloted(out, plan["n"])
    return {"phase": "pilot", "exploratory": True, "dry": bool(check.get("dry")),
            "dropped": plan["dropped"], "n": plan["n"],
            "blocks": {c: list(bs) for c, (bs, _) in got.items()},
            "reruns": {c: run6.reruns(recs) for c, (_, recs) in got.items()},
            "untested": check.get("untested", {}),
            "pilot_check": {k: check.get(k) for k in ("S*", "components", "futile", "outcome")},
            "primary": {"rejected": False, "status": f"not tested ({why})",
                        "joint_power": plan.get("joint")},
            "secondary": holm8(dict.fromkeys(secondary({}, plan)), why),
            "H-PRE": {"rows": holm8(dict.fromkeys(("time", "cost", "safety")), why)},
            "effects": exploratory(got)}


def summary(out):
    plan, check = (read_json(os.path.join(out, f)) for f in ("power.json", "pilot_check.json"))
    if plan is None or check is None:
        refuse("summary needs power.json and pilot_check.json")
    s = summary_main(out, plan, check) if check.get("go") else stopped(out, plan, check)
    with open(os.path.join(out, "summary.json"), "w") as f:
        json.dump(s, f, indent=1)
    text = summary_text(s)
    with open(os.path.join(out, "summary.txt"), "w") as f:
        f.write(text)
    print(text)


def summary_main(out, plan, check):
    """The summary of the main blocks; a cell main did not run takes its pilot's effect sizes,
    marked exploratory."""
    skipped = read_json(os.path.join(out, "batch-skipped.json"))
    recs, used, untested, spent = {}, {}, {}, {}
    for cell, n in plan["n"].items():
        if cell == "batch" and (skipped or check.get("batch_futile")):
            continue
        recs[cell], blocks = cell_blocks(out, "main", cell, n // 10)
        spent[cell], dry = run6.reruns(recs[cell]), any(r.get("dry") for r in recs[cell])
        if len(blocks) < (2 if dry else n):
            if not dry and len(spent[cell]) < n // 10:
                refuse(f"main {cell}: {len(blocks)} of {n} valid blocks, re-runs left")
            untested[cell] = DRY_SHORT if dry else SHORT
        else:
            used[cell] = blocks if dry else run6.first(blocks, n)
    kept = [c for c in PRIMARY if c in plan["n"]]
    if any(c not in used for c in kept):
        primary = {"rejected": False, "untested": SHORT}
    else:
        cells = {}
        for c in kept:
            s, comps = legs(used[c], c)
            d = comps[f"K/{s}"][0]
            t = stats6.paired_upper(d, MARGIN)
            cells[c] = {"S*": s, "t": run7.finite_t(t), "p": t["p"], "estimate": run6.estimate(d)}
        primary = stats7.iut({c: x["p"] for c, x in cells.items()}, ALPHA)
        primary.update(cells=cells, iut_rejected=primary["rejected"],
                       descriptive=plan["descriptive"],
                       rejected=primary["rejected"] and not plan["descriptive"])
    primary["joint_power"] = plan.get("joint")
    tests = secondary(used, plan)
    hpre = ({"skipped": skipped["reason"]} if skipped else
            {"untested": "the pilot check found the batch cell futile"}
            if check.get("batch_futile") else {"untested": plan["left_out"]["batch"]}
            if "batch" in plan.get("left_out", {}) else {"untested": untested["batch"]}
            if "batch" in untested else h_pre(used["batch"]))
    return {"phase": "main", "dry": any(r.get("dry") for rs in recs.values() for r in rs),
            "dropped": plan["dropped"], "n": plan["n"],
            "blocks": {c: list(bs) for c, bs in used.items()}, "reruns": spent,
            "untested": untested, "primary": primary,
            "secondary": holm8({k: v["p"] for k, v in tests.items()}), "tests": tests,
            "H-PRE": hpre, "effects": {
                **{c: effects(bs, c, recs[c]) for c, bs in used.items()},
                **exploratory(piloted(out, [c for c in CELLS if c not in recs]))}}


def main():
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("learn", "verdict"):
        p = sub.add_parser(name)
        p.add_argument("stand")
        p.add_argument("out")
        if name == "verdict":
            p.add_argument("phase", choices=PHASES)
            p.add_argument("cell", choices=tuple(CELLS))
            p.add_argument("first", type=int)
            p.add_argument("last", type=int)
            p.add_argument("--as-block", type=int, default=None)
            p.add_argument("--primer", action="store_true")
    p = sub.add_parser("power")
    p.add_argument("probe_out")
    p.add_argument("out")
    sub.add_parser("pilot-check").add_argument("out")
    sub.add_parser("summary").add_argument("out")
    args = ap.parse_args()
    if args.cmd == "power":
        return power(args.probe_out, args.out)
    if args.cmd in ("pilot-check", "summary"):
        return (pilot_check if args.cmd == "pilot-check" else summary)(args.out)
    cfg = load(args.stand)

    def interrupted(*_):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    if args.cmd == "learn":
        learn(cfg, args.out)
    else:
        verdict(cfg, args.out, args.phase, args.cell, args.first, args.last, args.as_block,
                args.primer)


if __name__ == "__main__":
    main()
