#!/usr/bin/env python3
"""Acceptance run of the memory guard: four agents' gates at once, unwrapped (F) and under
`coreslot run` (K), in ABBA pairs, plus a watchdog demo.

  run.py [--dry] [--smoke] --coreslot BIN --stand DIR --out DIR learn [--runs N]
  run.py ... set F|K --tag pilot|pair-N
  run.py ... pairs [--first N] [--last N]
  run.py ... demo [--hogs N] [--hog-gib G] [--hog-s S] [--cap-s S]

learn runs each gate alone under coreslot --runs times (default 3), so that the run's own state
directory (OUT/state) learns each label's need and normal share; its exits are the false-failure
baseline. A set starts the four agents at once after a cooldown (memory pressure normal and swap
used steady for 5 s, at least --cool-min-s, at most --cool-max-s) and samples the pressure level
and swap used every second. pairs runs pair-FIRST..pair-LAST in ABBA order (odd pairs F first).
demo teaches the label demo-hog a small need with a short hog, then starts --hogs hogs that each
grow to --hog-gib at once and hold it for --hog-s, under coreslot in OUT/demo/state; it stops them
all when swap used grows 6 GiB or after --cap-s. --dry prints what would run and starts nothing;
--smoke swaps the gates for trivial ones and skips the stand's pin check. Every process started
is recorded in OUT/live.json and stopped by its process group at exit, INT, TERM or HUP; a run
refuses to start while one of them is still alive.
"""
import argparse
import hashlib
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
PINS = {"vscode": "d956e8a6ac5d904486557eea07e1d6bd62a4d36f",
        "fastapi": "44cc7420b798b70b406516f5f0abf039f99edbe3",
        "sqlalchemy": "4fbf72c07f5bebcabb02e35ee8b742a217ac649d"}
AGENTS = {"A1": ["vscode-typecheck"], "A2": ["vscode-typecheck"],
          "A3": ["fastapi-ruff", "fastapi-tests"], "A4": ["sqlalchemy-orm"]}
PYTEST = [".venv/bin/python", "-m", "pytest", "-q", "-rfE", "-p", "no:cacheprovider", "-n", "auto"]
SWAP_STOP_MIB = 6144


def gates(stand, smoke):
    """gate -> (cwd, argv): the stand's commands, or with --smoke trivial ones in the stand dir."""
    if smoke:
        alloc = "import time; b = bytearray({} << 20); time.sleep({})"
        py = [sys.executable, "-I", "-c"]
        return {"vscode-typecheck": (stand, py + [alloc.format(200, 4)]),
                "fastapi-ruff": (stand, ["sleep", "1"]),
                "fastapi-tests": (stand, py + [alloc.format(100, 3)]),
                "sqlalchemy-orm": (stand, py + [alloc.format(150, 3)])}
    j = os.path.join
    return {"vscode-typecheck": (j(stand, "vscode"), ["npm", "run", "--silent", "typecheck-client"]),
            "fastapi-ruff": (j(stand, "fastapi"), [".venv/bin/ruff", "check", "fastapi", "tests"]),
            "fastapi-tests": (j(stand, "fastapi"), PYTEST + ["--dist", "loadgroup"]),
            "sqlalchemy-orm": (j(stand, "sqlalchemy"), PYTEST + ["test/orm"])}


def label(gate, smoke):
    return ("smoke:" if smoke else "air:") + gate


def wrap(coreslot, lab, argv):
    return [coreslot, "run", "--label", lab, "--"] + argv


def abba(first, last):
    """[(tag, arm)] of pairs first..last: odd pairs run F first, even pairs K first."""
    return [(f"pair-{i}", arm) for i in range(first, last + 1)
            for arm in (("F", "K") if i % 2 else ("K", "F"))]


# --- machine readings -----------------------------------------------------------------------

def sh(*argv):
    try:
        r = subprocess.run(argv, capture_output=True, text=True)
    except OSError:
        return ""
    return r.stdout.strip() if r.returncode == 0 else ""


def swap_mib(text):
    """Swap used, in MiB, from `sysctl vm.swapusage`."""
    m = re.search(r"used = ([0-9.]+)([KMG])", text)
    return round(float(m.group(1)) * {"K": 1 / 1024, "M": 1, "G": 1024}[m.group(2)]) if m else None


def vm_counters(text):
    return {k.lower(): int(v) for k, v in re.findall(r"^(Swapouts|Pageouts):\s+(\d+)", text, re.M)}


def reading():
    """The pressure level (1 normal, 2 warn, 4 critical) and swap used, from one sysctl call."""
    lines = sh("sysctl", "-n", "kern.memorystatus_vm_pressure_level", "vm.swapusage").splitlines()
    level = int(lines[0]) if lines and lines[0].isdigit() else None
    return level, swap_mib(lines[1] if len(lines) > 1 else "")


def thermal():
    """The macOS thermal state (0 nominal .. 3 critical), or None."""
    v = sh("osascript", "-l", "JavaScript", "-e",
           'ObjC.import("Foundation"); $.NSProcessInfo.processInfo.thermalState')
    return int(v) if v.isdigit() else None


def settled(samples, now_s, min_s, steady=5, tol_mib=64):
    """Whether a cooldown may end: at least min_s passed, the level is normal and swap used has
    moved at most tol_mib over the last `steady` samples [(level, swap)]."""
    last = samples[-steady:]
    swaps = [s for _, s in last if s is not None]
    return (now_s >= min_s and len(last) == steady and samples[-1][0] == 1
            and len(swaps) == steady and max(swaps) - min(swaps) <= tol_mib)


def cooldown(min_s, max_s):
    t0, samples = time.monotonic(), []
    while True:
        samples.append(reading())
        el = time.monotonic() - t0
        if settled(samples, el, min_s) or el >= max_s:
            break
        time.sleep(1)
    return {"cooldown_s": round(el, 1), "gave_up": not settled(samples, el, min_s),
            "level": samples[-1][0], "swap_mib": samples[-1][1]}


# --- the processes this run started ---------------------------------------------------------

def descendants(ps_text, roots):
    """{pid: (pgid, command)} of roots and every descendant, from `ps -axo pid=,ppid=,pgid=,command=`."""
    rows, kids = {}, {}
    for line in ps_text.splitlines():
        f = line.split(None, 3)
        if len(f) >= 3 and f[0].isdigit():
            pid, ppid = int(f[0]), int(f[1])
            rows[pid] = (int(f[2]), f[3] if len(f) > 3 else "")
            kids.setdefault(ppid, []).append(pid)
    out, todo = {}, [p for p in roots if p in rows]
    while todo:
        pid = todo.pop()
        if pid not in out:
            out[pid] = rows[pid]
            todo.extend(kids.get(pid, []))
    return out


def ps():
    return sh("ps", "-axo", "pid=,ppid=,pgid=,command=")


class Procs:
    """Every process this run started and their descendants, kept in OUT/live.json."""

    def __init__(self, path):
        self.path, self.roots, self.seen, self.lock = path, [], {}, threading.Lock()

    def alive(self):
        """Recorded processes still running the recorded command (an earlier run's included)."""
        try:
            rec = {int(k): v for k, v in json.loads(read(self.path) or "{}").items()}
        except ValueError:
            return {}
        now = descendants(ps(), list(rec))
        return {p: c for p, c in rec.items() if p in now and now[p][1] == c[1]}

    def add(self, pid):
        with self.lock:
            self.roots.append(pid)
        self.refresh()

    def refresh(self):
        with self.lock:
            self.seen.update(descendants(ps(), self.roots))
            tmp = self.path + ".tmp"
            with open(tmp, "w") as f:
                json.dump({str(p): v for p, v in self.seen.items()}, f)
            os.replace(tmp, self.path)

    def stop(self):
        """SIGTERM every recorded process group, then SIGKILL what is left after 5 s."""
        if not self.roots:
            return
        self.refresh()
        own = os.getpgid(0)
        for sig in (signal.SIGTERM, signal.SIGKILL):
            groups = {g for g, _ in self.alive().values() if g != own}
            for g in groups:
                try:
                    os.killpg(g, sig)
                except OSError:
                    pass
            for _ in range(10 if groups else 0):
                time.sleep(0.5)
                if not self.alive():
                    break
        left = self.alive()
        if not left:
            self.roots, self.seen = [], {}
            self.refresh()
        else:
            print("still alive, stop them by pid:", *[f"{p} {c[1]}" for p, c in left.items()],
                  sep="\n  ", file=sys.stderr)


PROCS = None


def start(argv, cwd, env, out_base):
    with open(out_base + ".out", "w") as fo, open(out_base + ".err", "w") as fe:
        p = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL, stdout=fo,
                             stderr=fe, start_new_session=True)
    PROCS.add(p.pid)
    return p


def watch(done, t0, guard=None):
    """Samples every second until done() or the guard names a reason; returns (samples, reason)."""
    samples, reason, w0 = [], None, time.monotonic()
    while not done():
        level, swap = reading()
        samples.append({"t": round(time.monotonic() - t0, 1), "level": level, "swap_mib": swap})
        PROCS.refresh()
        reason = guard(samples) if guard else None
        if reason:
            break
        time.sleep(max(0.0, w0 + len(samples) - time.monotonic()))
    return samples, reason


def guard_for(swap0, cap_s, swap_stop=SWAP_STOP_MIB):
    def guard(samples):
        s = samples[-1]
        if s["swap_mib"] is not None and swap0 is not None and s["swap_mib"] - swap0 > swap_stop:
            return "swap"
        return "time" if s["t"] >= cap_s else None
    return guard


# --- state directory and records ------------------------------------------------------------

def coreslot_env(state):
    return dict(os.environ, CORESLOT_STATE_DIR=state)


def make_state(state):
    os.makedirs(state, mode=0o700, exist_ok=True)
    os.chmod(state, 0o700)
    with open(os.path.join(state, "config.toml"), "w") as f:
        f.write("[watch]\nsample_log = true\n")


def log_size(state, name):
    try:
        return os.path.getsize(os.path.join(state, "log", name))
    except OSError:
        return 0


def read(path, offset=0):
    try:
        with open(path, "rb") as f:
            f.seek(offset)
            return f.read().decode(errors="replace")
    except OSError:
        return ""


def log_since(state, name, offset):
    return read(os.path.join(state, "log", name), offset)


def runlog_since(state, offset):
    out = []
    for line in log_since(state, "runs.jsonl", offset).splitlines():
        try:
            out.append(json.loads(line))
        except ValueError:
            pass
    return out


def budget_empty(a, state):
    """No live hold and no ticket but dead ones: a run killed while waiting leaves its ticket
    dead, and reconciliation removes it later (PROTOCOL.md sections 13.2 and 10.4)."""
    r = subprocess.run([a.coreslot, "status", "--json"], capture_output=True, text=True,
                       env=coreslot_env(state))
    try:
        st = json.loads(r.stdout)
    except ValueError:
        return False
    return not st.get("holds") and all(t.get("state") == "dead" for t in st.get("tickets") or [])


def append(path, rec):
    with open(path, "a") as f:
        f.write(json.dumps(rec) + "\n")


def meta(a, cmd):
    def sha256(p):
        try:
            with open(p, "rb") as f:
                return hashlib.sha256(f.read()).hexdigest()
        except OSError:
            return None
    stand = {n: sh("git", "-C", os.path.join(a.stand, n), "rev-parse", "HEAD") for n in PINS}
    return {"cmd": cmd, "time": datetime.now(timezone.utc).isoformat(timespec="seconds"),
            "smoke": a.smoke, "machine": sh("sysctl", "-n", "hw.model"),
            "os": sh("sw_vers", "-productVersion"), "mem_bytes": int(sh("sysctl", "-n", "hw.memsize") or 0),
            "ncpu": os.cpu_count(), "coreslot": os.path.abspath(a.coreslot),
            "coreslot_version": sh(a.coreslot, "version"), "coreslot_sha256": sha256(a.coreslot),
            "repo_commit": sh("git", "-C", REPO, "rev-parse", "HEAD"),
            "repo_dirty": bool(sh("git", "-C", REPO, "status", "--porcelain", "--", ".")),
            "stand": stand, "pins": PINS}


# --- phases ---------------------------------------------------------------------------------

def run_learn(a):
    state, g = os.path.join(a.out, "state"), gates(a.stand, a.smoke)
    if not a.dry:
        os.makedirs(os.path.join(a.out, "learn"), exist_ok=True)
    for r in range(1, a.runs + 1):
        for gate, (cwd, argv) in g.items():
            if a.dry:
                print(f"learn {gate} run {r}: cd {cwd} && {shlex.join(wrap(a.coreslot, label(gate, a.smoke), argv))}")
                continue
            cd = cooldown(min(a.cool_min_s, 3), a.cool_max_s)
            off, t0 = log_size(state, "runs.jsonl"), time.monotonic()
            base = os.path.join(a.out, "learn", f"{gate}-{r}")
            rc = start(wrap(a.coreslot, label(gate, a.smoke), argv), cwd, coreslot_env(state), base).wait()
            rec = {"gate": gate, "label": label(gate, a.smoke), "run": r, "exit": rc,
                   "dur_s": round(time.monotonic() - t0, 2), **cd, "runlog": runlog_since(state, off)}
            append(os.path.join(a.out, "learn.jsonl"), rec)
            print(f"learn {gate} run {r}: exit {rc}, {rec['dur_s']} s", flush=True)


def run_set(a, arm, tag):
    state, g = os.path.join(a.out, "state"), gates(a.stand, a.smoke)
    setdir = os.path.join(a.out, "sets", f"{tag}-{arm}")

    def argv_of(gate):
        cwd, argv = g[gate]
        return cwd, wrap(a.coreslot, label(gate, a.smoke), argv) if arm == "K" else argv

    if a.dry:
        for agent, chain in AGENTS.items():
            for gate in chain:
                cwd, argv = argv_of(gate)
                print(f"{tag} {arm} {agent} {gate}: cd {cwd} && {shlex.join(argv)}")
        return
    if os.path.exists(setdir):
        sys.exit(f"{setdir} exists: a set is recorded once")
    if arm == "K" and not budget_empty(a, state):
        sys.exit(f"{tag} {arm}: the budget in {state} has a hold or a ticket")
    cd = cooldown(a.cool_min_s, a.cool_max_s)
    os.makedirs(setdir)
    th, vm0, run_off, watch_off = thermal(), vm_counters(sh("vm_stat")), log_size(state, "runs.jsonl"), log_size(state, "watch.jsonl")
    recs, t0, wall0 = [], time.monotonic(), datetime.now(timezone.utc)

    def agent(name, chain):
        for gate in chain:
            cwd, argv = argv_of(gate)
            s = time.monotonic() - t0
            try:
                rc = start(argv, cwd, coreslot_env(state), os.path.join(setdir, f"{name}-{gate}")).wait()
            except OSError:
                rc = 127
            recs.append({"agent": name, "gate": gate, "label": label(gate, a.smoke),
                         "start_s": round(s, 2), "end_s": round(time.monotonic() - t0, 2), "exit": rc})

    threads = [threading.Thread(target=agent, args=item, daemon=True) for item in AGENTS.items()]
    for t in threads:
        t.start()
    samples, reason = watch(lambda: not any(t.is_alive() for t in threads), t0,
                            guard_for(None, a.set_cap_s))
    if reason:
        PROCS.stop()
        for t in threads:
            t.join()
    vm1 = vm_counters(sh("vm_stat"))
    swaps = [s["swap_mib"] for s in samples if s["swap_mib"] is not None]
    agents = {}
    for name in AGENTS:
        mine = [r for r in recs if r["agent"] == name]
        agents[name] = {"verdict_s": max(r["end_s"] for r in mine),
                        "exit": next((r["exit"] for r in mine if r["exit"]), 0)}
    if arm == "K":
        with open(os.path.join(setdir, "watch.jsonl"), "w") as f:
            f.write(log_since(state, "watch.jsonl", watch_off))
    rec = {"tag": tag, "arm": arm, "started": wall0.isoformat(timespec="seconds"), "cooldown": cd,
           "thermal_start": th, "stopped_by": reason, "gates": sorted(recs, key=lambda r: (r["agent"], r["start_s"])),
           "agents": agents, "makespan_s": max(r["end_s"] for r in recs),
           "swap_mib": {"start": swaps[0] if swaps else None, "end": swaps[-1] if swaps else None,
                        "peak": max(swaps) if swaps else None},
           "vm_delta": {k: vm1.get(k, 0) - v for k, v in vm0.items()}, "samples": samples,
           "runlog": runlog_since(state, run_off) if arm == "K" else []}
    append(os.path.join(a.out, "sets.jsonl"), rec)
    warn = sum((s["level"] or 1) >= 2 for s in samples)
    print(f"{tag} {arm}: cooldown {cd['cooldown_s']} s, makespan {rec['makespan_s']} s, warn+ {warn} s,"
          f" exits {[r['exit'] for r in rec['gates']]}", flush=True)


def run_pairs(a):
    for tag, arm in abba(a.first, a.last):
        run_set(a, arm, tag)


def run_demo(a):
    d = os.path.join(a.out, "demo")
    state, hog = os.path.join(d, "state"), [sys.executable, "-I", os.path.join(REPO, "calibration", "hog.py")]
    mem_gib = int(sh("sysctl", "-n", "hw.memsize") or 0) / 2**30
    gib = a.hog_gib or (0.125 if a.smoke else max(0.5, round(mem_gib * 0.375 * 2) / 2))
    teach = wrap(a.coreslot, "demo-hog", hog + ["--log", os.path.join(d, "teach.jsonl"), "--step-mib", "256",
                                                "--hold-s", "3600", "--seconds", str(a.teach_s)])
    hogs = [wrap(a.coreslot, "demo-hog", hog + ["--log", os.path.join(d, f"hog-{i}.jsonl"), "--step-mib",
                                                str(int(gib * 1024)), "--hold-s", "3600", "--seconds",
                                                str(a.hog_s), "--slow-pct", "0"]) for i in range(1, a.hogs + 1)]
    if a.dry:
        print(f"demo state: {state} ([watch] sample_log = true, every other key its default)")
        print(f"teach (up to 3 times, until the run log has a tick_share for demo-hog): {shlex.join(teach)}")
        for i, h in enumerate(hogs, 1):
            print(f"hog {i} (1 s apart, oldest first): {shlex.join(h)}")
        print(f"guard: stop everything when swap used grows {SWAP_STOP_MIB} MiB or after {a.cap_s} s")
        return
    os.makedirs(d)
    make_state(state)
    env, learned = coreslot_env(state), None
    for i in range(1, 4):
        off = log_size(state, "runs.jsonl")
        start(teach, d, env, os.path.join(d, f"teach-{i}")).wait()
        learned = next((r for r in runlog_since(state, off) if r.get("tick_share") is not None and not r.get("stops")), None)
        if learned:
            break
    if not learned:
        sys.exit("demo: three teaching runs left demo-hog without a normal share; nothing to judge a stop by")
    cd = cooldown(a.cool_min_s, a.cool_max_s)
    off, t0, swap0 = log_size(state, "runs.jsonl"), time.monotonic(), reading()[1]
    procs = []
    for i, h in enumerate(hogs, 1):
        procs.append((time.monotonic() - t0, start(h, d, env, os.path.join(d, f"hog-{i}"))))
        time.sleep(1)
    samples, reason = watch(lambda: all(p.poll() is not None for _, p in procs), t0, guard_for(swap0, a.cap_s))
    if reason:
        PROCS.stop()
    runs = []
    for i, (s, p) in enumerate(procs, 1):
        err = read(os.path.join(d, f"hog-{i}.err"))
        attempts = read(os.path.join(d, f"hog-{i}.jsonl")).count('"event": "start"')
        runs.append({"hog": i, "start_s": round(s, 1), "exit": p.wait(), "attempts": attempts,
                     "stops": len(re.findall(r"^coreslot: memory stop \d", err, re.M)),
                     "gave_up": "coreslot: gave up after" in err})
    with open(os.path.join(d, "watch.jsonl"), "w") as f:
        f.write(log_since(state, "watch.jsonl", 0))
    rec = {"hog_gib": gib, "hog_s": a.hog_s, "taught": learned, "cooldown": cd, "stopped_by": reason,
           "end_s": round(time.monotonic() - t0, 1), "swap_start_mib": swap0, "hogs": runs,
           "runlog": runlog_since(state, off), "samples": samples}
    with open(os.path.join(d, "demo.json"), "w") as f:
        json.dump(rec, f, indent=1)
    print(f"demo: stopped_by {reason}, hogs {runs}", flush=True)


def main(argv=None):
    global PROCS
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--dry", action="store_true")
    ap.add_argument("--smoke", action="store_true")
    ap.add_argument("--coreslot", required=True)
    ap.add_argument("--stand", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--cool-min-s", type=float, default=15)
    ap.add_argument("--cool-max-s", type=float, default=120)
    ap.add_argument("--set-cap-s", type=float, default=900)
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("learn").add_argument("--runs", type=int, default=3)
    s = sub.add_parser("set")
    s.add_argument("arm", choices=("F", "K"))
    s.add_argument("--tag", required=True)
    p = sub.add_parser("pairs")
    p.add_argument("--first", type=int, default=1)
    p.add_argument("--last", type=int, default=5)
    d = sub.add_parser("demo")
    d.add_argument("--hogs", type=int, default=3)
    d.add_argument("--hog-gib", type=float, default=0, help="each hog's size (default 3/8 of memory)")
    d.add_argument("--hog-s", type=int, default=90)
    d.add_argument("--teach-s", type=int, default=30)
    d.add_argument("--cap-s", type=float, default=600)
    a = ap.parse_args(argv)
    if a.cmd == "set" and not re.fullmatch(r"pilot|pair-\d+", a.tag):
        ap.error("--tag: want pilot or pair-N")
    a.out, a.stand = os.path.abspath(a.out), os.path.abspath(a.stand)
    PROCS = Procs(os.path.join(a.out, "live.json"))
    left = PROCS.alive()
    if left:
        print("a process of an earlier set is still alive; stop it by pid first:",
              *[f"{p} {c[1]}" for p, c in left.items()], sep="\n  ", file=sys.stderr)
        if not a.dry:
            return 3
    if not a.smoke:
        bad = {n: h for n, h in meta(a, a.cmd)["stand"].items() if h != PINS[n]}
        if bad:
            print(f"the stand is not at its pins: {bad}", file=sys.stderr)
            if not a.dry:
                return 2
    phase = {"learn": run_learn, "set": lambda a: run_set(a, a.arm, a.tag), "pairs": run_pairs,
             "demo": run_demo}[a.cmd]
    if a.dry:
        print(f"state: {os.path.join(a.out, 'state')}; cooldown {a.cool_min_s}..{a.cool_max_s} s")
        return phase(a)
    make_state(os.path.join(a.out, "state"))
    append(os.path.join(a.out, "meta.jsonl"), meta(a, " ".join(sys.argv[1:] if argv is None else argv)))
    for sig in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, lambda n, _: sys.exit(128 + n))
    try:
        phase(a)
    except KeyboardInterrupt:
        return 130
    finally:
        for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
            signal.signal(sig, signal.SIG_IGN)
        PROCS.stop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
