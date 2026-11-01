#!/usr/bin/env python3
"""A memory hog for calibrating the watchdog: it grows hot memory in steps and logs its own speed.

The hog maps a step of anonymous memory every --hold-s seconds (each page half random, half zero,
about 2:1 compressible) and from --threads threads keeps rewriting one byte in a random page of all
it holds, so every page stays hot. Once a second it appends one JSON line to --log: the touches per
second, the hot MiB, and its own reading of swap and decompressions.

It stops itself, writing a stop line with the reason, when
  slow          its speed stayed below --slow-pct of its first step's for --slow-s seconds,
  cliff_decomp  decompressions stayed at or above --cliff-decomp-s per second for --slow-s seconds
                (a cache-bound single thread slows without any paging, so speed alone is not the cliff),
  swap          swap used grew by --stop-swap-gib from the start (a Mac may carry GiB already),
  max           it holds --max-gib,
  signal        it got SIGINT, SIGTERM or SIGHUP.
Lines carry t_ns on the clock the watchdog's sample log uses (darwin CLOCK_MONOTONIC_RAW, Linux
CLOCK_BOOTTIME), so the two logs join on it.
"""
import argparse
import bisect
import ctypes
import json
import mmap
import os
import random
import re
import signal
import statistics
import subprocess
import sys
import threading
import time

CLOCK = time.CLOCK_MONOTONIC_RAW if sys.platform == "darwin" else time.CLOCK_BOOTTIME
PAGE = mmap.PAGESIZE
HALF = PAGE // 2


def now_ns():
    return time.clock_gettime_ns(CLOCK)


def swap_used_mib():
    """Swap in use in MiB from vm.swapusage, or None where the platform has no such sysctl."""
    try:
        out = subprocess.run(["sysctl", "-n", "vm.swapusage"], capture_output=True, text=True).stdout
    except OSError:
        return None
    m = re.search(r"used = ([0-9.]+)([KMG])", out)
    if not m:
        return None
    return float(m.group(1)) * {"K": 1 / 1024, "M": 1, "G": 1024}[m.group(2)]


def decompressions():
    """The machine's decompression counter from vm_stat, or None."""
    try:
        out = subprocess.run(["vm_stat"], capture_output=True, text=True).stdout
    except OSError:
        return None
    m = re.search(r"Decompressions:\s+(\d+)", out)
    return int(m.group(1)) if m else None


class Memory:
    """The mapped regions, published as one immutable tuple so touching threads never see a half step."""

    def __init__(self):
        self.regions = []  # (mmap, c_char) pairs; the c_char keeps the buffer exported
        self.view = ((), (), 0)  # region addresses, first page index of each, total pages
        self.pages = 0
        block = os.urandom(HALF)
        self.block = ctypes.create_string_buffer(block, HALF)

    def grow(self, step_bytes):
        n = step_bytes // PAGE
        m = mmap.mmap(-1, n * PAGE)
        anchor = ctypes.c_char.from_buffer(m)
        addr = ctypes.addressof(anchor)
        for i in range(n):  # memmove drops the GIL, so the other threads keep running while this faults
            ctypes.memmove(addr + i * PAGE, self.block, HALF)
        self.regions.append((m, anchor))
        addrs, starts, _ = self.view
        self.view = (addrs + (addr,), starts + (self.pages,), self.pages + n)
        self.pages += n

    def mib(self):
        return self.pages * PAGE // 2**20


def toucher(idx, memory, counts, stop):
    rnd = random.Random(idx + 1).random
    memset = ctypes.memset
    while not stop.is_set():
        addrs, starts, total = memory.view
        for _ in range(1000):
            x = int(rnd() * total)
            j = bisect.bisect_right(starts, x) - 1
            memset(addrs[j] + (x - starts[j]) * PAGE + HALF, 1, 1)
        counts[idx] += 1000


def main(argv):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--log", required=True, help="JSON lines output")
    p.add_argument("--pidfile", help="write this process's pid here")
    p.add_argument("--threads", type=int, default=1, help="touching threads (default 1)")
    p.add_argument("--step-mib", type=int, default=512, help="memory added per step")
    p.add_argument("--hold-s", type=float, default=15.0, help="seconds between steps")
    default_max = os.sysconf("SC_PHYS_PAGES") * os.sysconf("SC_PAGE_SIZE") * 3 / 2 / 2**30
    p.add_argument("--max-gib", type=float, default=default_max, help="stop at this much (default 1.5 x RAM)")
    p.add_argument("--slow-pct", type=float, default=30.0)
    p.add_argument("--slow-s", type=int, default=5)
    p.add_argument("--cliff-decomp-s", type=int, default=100_000)
    p.add_argument("--stop-swap-gib", type=float, default=6.0)
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

    memory, stop = Memory(), threading.Event()
    counts = [0] * a.threads
    swap0, decomp_prev = swap_used_mib(), decompressions()
    emit(event="start", threads=a.threads, step_mib=a.step_mib, hold_s=a.hold_s, page_bytes=PAGE,
         swap_used_mib=swap0)
    memory.grow(a.step_mib * 2**20)  # the touching threads need something to touch
    first_mib = memory.mib()
    emit(event="grow", hot_mib=first_mib)
    for i in range(a.threads):
        threading.Thread(target=toucher, args=(i, memory, counts, stop), daemon=True).start()

    first_rates, baseline = [], None
    slow = hot = 0
    next_grow = time.monotonic() + a.hold_s
    last_t, last_total, reason = time.monotonic(), 0, None
    while not reason:
        if time.monotonic() >= next_grow:
            memory.grow(a.step_mib * 2**20)
            emit(event="grow", hot_mib=memory.mib())
            next_grow = time.monotonic() + a.hold_s
        time.sleep(max(0.0, last_t + 1.0 - time.monotonic()))
        t, total = time.monotonic(), sum(counts)
        dt = t - last_t
        rate = (total - last_total) / dt
        last_t, last_total = t, total
        swap, decomp = swap_used_mib(), decompressions()
        decomp_s = None
        if decomp is not None and decomp_prev is not None:
            decomp_s = round((decomp - decomp_prev) / dt)
        decomp_prev = decomp
        emit(event="rate", hot_mib=memory.mib(), touches_s=round(rate), decomp_s=decomp_s, swap_used_mib=swap)

        if memory.mib() == first_mib:  # the first step's median speed is the reference
            first_rates.append(rate)
            if len(first_rates) > 3:
                baseline = statistics.median(first_rates)
        slow = slow + 1 if baseline and rate < baseline * a.slow_pct / 100 else 0
        hot = hot + 1 if decomp_s is not None and decomp_s >= a.cliff_decomp_s else 0
        if stopping:
            reason = "signal"
        elif slow >= a.slow_s:
            reason = "slow"
        elif hot >= a.slow_s:
            reason = "cliff_decomp"
        elif swap is not None and swap0 is not None and swap - swap0 > a.stop_swap_gib * 1024:
            reason = "swap"
        elif memory.mib() >= a.max_gib * 1024:
            reason = "max"
    emit(event="stop", reason=reason, hot_mib=memory.mib())
    log.close()
    os._exit(0)  # the touching threads are daemons; leaving at once frees the memory at once


if __name__ == "__main__":
    main(sys.argv[1:])
