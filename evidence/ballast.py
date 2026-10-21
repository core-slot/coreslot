#!/usr/bin/env python3
"""A memory ballast: a child process that holds a fixed amount of resident, incompressible memory
while a set runs, the background-memory level of acceptance run 6.

usage:
  ballast.py hold MIB

The child records its parent at start, allocates MIB MiB in 64 MiB chunks filled from os.urandom
(so the compressor cannot shrink them), checking its parent between chunks, prints "ready", then
once a second writes one byte per 16 KiB page so the pages stay in use. It exits when its parent
changes (the parent died and it was re-parented) or on SIGTERM, so a killed runner never leaves
it holding memory.

Ballast(MIB) is the parent's side, a context manager: entering starts the child and returns once it
printed "ready" (on timeout the child is killed and the entry raises); leaving stops it with
SIGTERM, then SIGKILL after 2 s, and reaps it. Ballast(0) starts nothing.
"""
import os
import select
import signal
import subprocess
import sys
import time

CHUNK, PAGE, PIECE = 64 << 20, 16 << 10, 1 << 20


def hold(mib):
    parent = os.getppid()
    chunks, left = [], mib << 20
    while left > 0:
        if os.getppid() != parent:
            return
        buf = bytearray(min(CHUNK, left))
        view = memoryview(buf)
        for off in range(0, len(buf), PIECE):
            n = min(PIECE, len(buf) - off)
            view[off:off + n] = os.urandom(n)
        chunks.append(buf)
        left -= len(buf)
    print("ready", flush=True)
    while os.getppid() == parent:
        for buf in chunks:
            for i in range(0, len(buf), PAGE):
                buf[i] = (buf[i] + 1) & 0xFF
        time.sleep(1.0)


class Ballast:
    def __init__(self, mib, ready_timeout_s=60):
        self.mib, self.ready_timeout_s = mib, ready_timeout_s
        self.pid, self._p = None, None

    def __enter__(self):
        if not self.mib:
            return self
        self._p = subprocess.Popen([sys.executable, "-I", os.path.abspath(__file__), "hold",
                                    str(self.mib)], stdout=subprocess.PIPE,
                                   stdin=subprocess.DEVNULL, start_new_session=True)
        self.pid = self._p.pid
        fd, got = self._p.stdout.fileno(), b""
        deadline = time.monotonic() + self.ready_timeout_s
        try:
            while b"ready\n" not in got:
                left = deadline - time.monotonic()
                if left <= 0 or not select.select([fd], [], [], left)[0]:
                    raise TimeoutError(f"ballast: not ready within {self.ready_timeout_s} s")
                more = os.read(fd, 64)
                if not more:
                    raise RuntimeError(f"ballast: the child exited {self._p.wait()}")
                got += more
        except BaseException:
            self.__exit__()
            raise
        return self

    def __exit__(self, *exc):
        p = self._p
        if p is None:
            return
        self._p = None
        if p.poll() is None:
            p.send_signal(signal.SIGTERM)
            try:
                p.wait(2)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()
        p.stdout.close()

    def rss_mib(self):
        """The child's resident memory in MiB, or None without a child."""
        if self._p is None:
            return None
        r = subprocess.run(["ps", "-o", "rss=", "-p", str(self.pid)], capture_output=True,
                           text=True)
        return int(r.stdout) // 1024 if r.stdout.strip().isdigit() else None


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] != "hold" or not sys.argv[2].isdigit():
        sys.exit(__doc__)
    hold(int(sys.argv[2]))
