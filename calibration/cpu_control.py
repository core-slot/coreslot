#!/usr/bin/env python3
"""A contention control for calibrating the watchdog: busy loops and no memory pressure.

It starts --procs busy-loop processes (default twice the number of cores) for --seconds and appends a
start and a stop line to --log with t_ns on the clock the watchdog's sample log uses. A watchdog
that measures memory must stay calm here: system time is not lost to memory just because the
machine is busy.

Each loop is killed by the pid this script recorded for it, when the script ends, on SIGINT,
SIGTERM or SIGHUP, and by the loop itself if the script dies without cleaning up: a loop leaves when
its deadline passes or its parent is gone.
"""
import argparse
import json
import os
import signal
import subprocess
import sys
import time

CLOCK = time.CLOCK_MONOTONIC_RAW if sys.platform == "darwin" else time.CLOCK_BOOTTIME


def now_ns():
    return time.clock_gettime_ns(CLOCK)


def burn(seconds, parent):
    end = time.monotonic() + seconds
    while time.monotonic() < end and os.getppid() == parent:
        for _ in range(200_000):
            pass


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--procs", type=int, default=2 * (os.cpu_count() or 1))
    p.add_argument("--seconds", type=float, default=60.0)
    p.add_argument("--log")
    p.add_argument("--pidfile", help="write this process's pid here")
    p.add_argument("--burn", type=float, help=argparse.SUPPRESS)  # a loop: the seconds to burn
    p.add_argument("--parent", type=int, help=argparse.SUPPRESS)
    a = p.parse_args(argv)
    if a.burn is not None:
        burn(a.burn, a.parent)
        return
    if a.procs < 1 or a.seconds <= 0 or not a.log:
        p.error("--log is required; --procs and --seconds must be positive")

    reason = "done"

    def leave(signum, _frame):
        nonlocal reason
        reason = "signal"
        raise SystemExit(128 + signum)

    for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(s, leave)
    if a.pidfile:
        with open(a.pidfile, "w") as f:
            f.write(f"{os.getpid()}\n")
    log = open(a.log, "a", buffering=1)

    def emit(**kv):
        log.write(json.dumps({"t_ns": now_ns(), **kv}) + "\n")

    kids = []
    try:
        emit(event="start", procs=a.procs, seconds=a.seconds)
        for _ in range(a.procs):
            kids.append(subprocess.Popen([sys.executable, "-I", os.path.abspath(__file__), "--burn", str(a.seconds),
                                          "--parent", str(os.getpid())]))
        end = time.monotonic() + a.seconds
        while time.monotonic() < end:
            time.sleep(min(0.5, max(0.0, end - time.monotonic())))
    finally:
        for k in kids:  # by the pid recorded at Popen, while it is still ours to wait on
            if k.poll() is None:
                k.kill()
        for k in kids:
            k.wait()
        emit(event="stop", reason=reason)
        log.close()


if __name__ == "__main__":
    main(sys.argv[1:])
