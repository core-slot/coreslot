#!/usr/bin/env python3
"""Measure the watchdog's own CPU cost: N concurrent `coreslot run --restart=yes -- sleep S`, for each N.

usage: cost.py CORESLOT [--seconds 120] [--runs 1 4 8] [--out DIR]

Each coreslot process is read twice with ps, 3 s after the last run has started its command and
3 s before the first one ends, so that every run is ticking throughout. Its CPU time between the
readings (its own: ps reports the one process, not the sleep it started) is divided by the wall
time between them: a share of one core, per run. DIR gets cost.jsonl (one line per run) and
meta.json; by default records/<model>-<os>-<date>-cost.
"""
import argparse
import json
import os
import subprocess
import sys
import tempfile
import time

EDGE_S = 3  # settling time at each end


def cpu_seconds(text):
    """ps's cputime ([[H:]M:]S.cc) in seconds."""
    secs = 0.0
    for part in text.strip().split(":"):
        secs = secs * 60 + float(part)
    return secs


def read_cpu(pids):
    out = subprocess.run(["ps", "-o", "pid=,cputime=", "-p", ",".join(map(str, pids))],
                         check=True, capture_output=True, text=True).stdout
    return {int(p): cpu_seconds(t) for p, t in (line.split() for line in out.splitlines())}


def admitted(pid):
    """Whether the run has started its command (it has a child)."""
    return subprocess.run(["pgrep", "-P", str(pid)], capture_output=True).returncode == 0


def measure(binary, n, seconds, state):
    env = {**os.environ, "CORESLOT_STATE_DIR": state}
    cmd = [binary, "run", "--restart=yes", "--mem", "64M", "--", "sleep", str(seconds)]
    procs = [subprocess.Popen(cmd, env=env, stdin=subprocess.DEVNULL) for _ in range(n)]
    try:
        pids = [p.pid for p in procs]
        began = {}  # starts are spaced, so the runs are admitted one after another
        while len(began) < n:
            time.sleep(0.25)
            began.update({pid: time.monotonic() for pid in pids if pid not in began and admitted(pid)})
        if min(began.values()) + seconds - max(began.values()) < 2 * EDGE_S + 30:
            raise SystemExit("--seconds is too short for this many runs to tick together")
        time.sleep(max(max(began.values()) + EDGE_S - time.monotonic(), 0))
        c0, t0 = read_cpu(pids), time.monotonic()
        time.sleep(max(min(began.values()) + seconds - EDGE_S - time.monotonic(), 0))
        c1, t1 = read_cpu(pids), time.monotonic()
        for p in procs:
            if p.wait(timeout=2 * EDGE_S + seconds) != 0:
                raise SystemExit(f"a run exited {p.returncode}: the measurement is void")
    finally:
        for p in procs:
            if p.poll() is None:
                p.kill()
    return [{"runs": n, "pid": pid, "cpu_s": round(c1[pid] - c0[pid], 3), "wall_s": round(t1 - t0, 3),
             "core_share": round((c1[pid] - c0[pid]) / (t1 - t0), 5)} for pid in pids]


def sh(*argv):
    return subprocess.run(argv, capture_output=True, text=True).stdout.strip()


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("coreslot")
    ap.add_argument("--seconds", type=int, default=120)
    ap.add_argument("--runs", type=int, nargs="+", default=[1, 4, 8])
    ap.add_argument("--out")
    a = ap.parse_args(argv)
    if a.seconds <= 2 * EDGE_S + 10:
        ap.error("--seconds is too short to measure")
    model, osv = sh("sysctl", "-n", "hw.model"), sh("sw_vers", "-productVersion")
    out = a.out or os.path.join(os.path.dirname(os.path.abspath(__file__)), "records",
                                f"{model}-{osv}-{time.strftime('%Y-%m-%d', time.gmtime())}-cost")
    os.makedirs(out, exist_ok=True)
    meta = {"kind": "cost", "machine": model, "os": osv, "ncpu": os.cpu_count(), "seconds": a.seconds,
            "runs": a.runs, "commit": sh("git", "-C", os.path.dirname(os.path.abspath(__file__)), "rev-parse", "--short", "HEAD"),
            "started": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
    rows = []
    for n in a.runs:
        with tempfile.TemporaryDirectory() as state:  # a fresh state directory per N
            rows += measure(a.coreslot, n, a.seconds, state)
        mine = [r["core_share"] for r in rows if r["runs"] == n]
        print(f"{n} runs: per run {min(mine):.3%} to {max(mine):.3%} of one core", flush=True)
    with open(os.path.join(out, "cost.jsonl"), "w") as f:
        f.writelines(json.dumps(r) + "\n" for r in rows)
    with open(os.path.join(out, "meta.json"), "w") as f:
        json.dump(meta, f)
        f.write("\n")


if __name__ == "__main__":
    main(sys.argv[1:])
