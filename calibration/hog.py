#!/usr/bin/env python3
"""A memory hog for calibrating the watchdog: it grows hot memory in steps and logs its own speed.

The hog forks --threads worker processes (processes, not threads: Python threads contend on the
interpreter lock and measure that instead of memory). Every --hold-s seconds each worker maps its
share of a step of anonymous memory (each page half random, half zero, about 2:1 compressible), and
keeps rewriting one byte in a random page of all it holds, so every page stays hot. Once a second the
hog appends one JSON line to --log: the touches per second summed over the workers and the hot MiB.
The workers are the hog's children, so the measured tree is the hog and its workers, and its times
sum over them; the system's readings come from the watchdog's sample log. A worker leaves when the
hog does: the hog kills and reaps them on its way out, and a worker whose parent is gone leaves by
itself.

It stops itself, writing a stop line with the reason, when
  slow          its speed stayed below --slow-pct of its first step's for --slow-s seconds,
  max           it holds --max-gib,
  time          it ran --seconds (a warm-up; 0 means no limit),
  signal        it got SIGINT, SIGTERM or SIGHUP.
run.sh stops it from outside when swap grows too far.
Lines carry t_ns on the clock the watchdog's sample log uses (darwin CLOCK_MONOTONIC_RAW, Linux
CLOCK_BOOTTIME), so the two logs join on it.
"""
import argparse
import ctypes
import json
import mmap
import os
import random
import signal
import statistics
import sys
import time

CLOCK = time.CLOCK_MONOTONIC_RAW if sys.platform == "darwin" else time.CLOCK_BOOTTIME
PAGE = mmap.PAGESIZE
HALF = PAGE // 2
BATCH = 1000  # touches between a worker's looks at the shared counters


def now_ns():
    return time.clock_gettime_ns(CLOCK)


def worker(idx, shared, workers, step_pages, parent):
    """One worker: grows by step_pages whenever shared[0] says another step is due, and touches.
    shared holds the steps due, then each worker's touch count, then each worker's pages."""
    for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(s, signal.SIG_DFL)
    rnd = random.Random(idx + 1).random
    memset, memmove = ctypes.memset, ctypes.memmove
    block = ctypes.create_string_buffer(os.urandom(HALF), HALF)
    held = []  # (mmap, c_char, address); the c_char keeps the buffer exported
    while os.getppid() == parent:
        if shared[0] > len(held):
            m = mmap.mmap(-1, step_pages * PAGE)
            anchor = ctypes.c_char.from_buffer(m)
            addr = ctypes.addressof(anchor)
            for i in range(step_pages):
                memmove(addr + i * PAGE, block, HALF)
            held.append((m, anchor, addr))
            shared[1 + workers + idx] = len(held) * step_pages
            continue
        total = len(held) * step_pages
        for _ in range(BATCH):
            j, page = divmod(int(rnd() * total), step_pages)
            memset(held[j][2] + page * PAGE + HALF, 1, 1)
        shared[1 + idx] += BATCH


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--log", required=True, help="JSON lines output")
    p.add_argument("--pidfile", help="write this process's pid here")
    p.add_argument("--threads", type=int, default=1, help="touching worker processes (default 1)")
    p.add_argument("--step-mib", type=int, default=512, help="memory added per step")
    p.add_argument("--hold-s", type=float, default=15.0, help="seconds between steps")
    default_max = os.sysconf("SC_PHYS_PAGES") * os.sysconf("SC_PAGE_SIZE") * 3 / 2 / 2**30
    p.add_argument("--max-gib", type=float, default=default_max, help="stop at this much (default 1.5 x RAM)")
    p.add_argument("--slow-pct", type=float, default=30.0)
    p.add_argument("--slow-s", type=int, default=5)
    p.add_argument("--seconds", type=float, default=0.0, help="stop after this many seconds (default: no limit)")
    a = p.parse_args(argv)
    if a.threads < 1 or a.step_mib < 1 or a.hold_s <= 0:
        p.error("--threads, --step-mib and --hold-s must be positive")

    stopping = []
    for s in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(s, lambda *_: stopping.append("signal"))
    if a.pidfile:
        with open(a.pidfile, "w") as f:
            f.write(f"{os.getpid()}\n")
    log = open(a.log, "a", buffering=1)

    def emit(**kv):
        log.write(json.dumps({"t_ns": now_ns(), **kv}) + "\n")

    n = a.threads
    step_pages = a.step_mib * 2**20 // n // PAGE
    first_mib = n * step_pages * PAGE // 2**20
    shared = memoryview(mmap.mmap(-1, 8 * (1 + 2 * n))).cast("Q")

    def mib():
        return sum(shared[1 + n:1 + 2 * n]) * PAGE // 2**20

    emit(event="start", threads=n, step_mib=a.step_mib, hold_s=a.hold_s, page_bytes=PAGE)
    shared[0] = 1  # the workers' first step
    parent, pids = os.getpid(), []
    try:
        for i in range(n):
            pid = os.fork()
            if pid == 0:
                try:
                    worker(i, shared, n, step_pages, parent)
                finally:
                    os._exit(0)
            pids.append(pid)
        while mib() < first_mib and not stopping:  # the workers need something to touch
            time.sleep(0.05)
        emit(event="grow", hot_mib=mib())

        first_rates, baseline = [], None
        slow = 0
        t0 = last_t = time.monotonic()
        next_grow = t0 + a.hold_s
        last_total, reason = 0, None
        while not reason:
            if time.monotonic() >= next_grow and not stopping:
                shared[0] += 1
                emit(event="grow", hot_mib=shared[0] * first_mib)
                next_grow = time.monotonic() + a.hold_s
            time.sleep(max(0.0, last_t + 1.0 - time.monotonic()))
            t, total = time.monotonic(), sum(shared[1:1 + n])
            rate = (total - last_total) / (t - last_t)
            last_t, last_total = t, total
            emit(event="rate", hot_mib=mib(), touches_s=round(rate))

            if mib() == first_mib:  # the first step's median speed is the reference
                first_rates.append(rate)
                if len(first_rates) > 3:
                    baseline = statistics.median(first_rates)
            slow = slow + 1 if baseline and rate < baseline * a.slow_pct / 100 else 0
            if stopping:
                reason = "signal"
            elif slow >= a.slow_s:
                reason = "slow"
            elif mib() >= a.max_gib * 1024:
                reason = "max"
            elif a.seconds and t - t0 >= a.seconds:
                reason = "time"
        emit(event="stop", reason=reason, hot_mib=mib())
    finally:
        for pid in pids:
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        for pid in pids:  # reaped, so the workers' times count in this process's children
            os.waitpid(pid, 0)
    log.close()


if __name__ == "__main__":
    main(sys.argv[1:])
