#!/usr/bin/env python3
"""A plain N-slot semaphore: sem.py LOCKDIR N -- CMD...

Takes one of the lock files LOCKDIR/slot-0 .. slot-(N-1) with flock(LOCK_EX | LOCK_NB), trying
them all every 50 ms until one is free, then execs CMD with the lock's descriptor inherited: the
slot is held until CMD and every descendant that kept the descriptor have exited. CMD's exit
status and signals are its own, since it replaces this process. 127 when CMD cannot be run.
"""
import fcntl
import os
import sys
import time


def acquire(lockdir, n, poll_s=0.05):
    """The descriptor of the first free slot, waiting until one is."""
    while True:
        for i in range(n):
            fd = os.open(os.path.join(lockdir, f"slot-{i}"), os.O_RDWR | os.O_CREAT, 0o600)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return fd
            except BlockingIOError:
                os.close(fd)
        time.sleep(poll_s)


def main(argv):
    if len(argv) < 4 or argv[2] != "--" or not argv[1].isdigit() or int(argv[1]) < 1:
        print(__doc__.splitlines()[0], file=sys.stderr)
        return 64
    os.makedirs(argv[0], exist_ok=True)
    os.set_inheritable(acquire(argv[0], int(argv[1])), True)
    try:
        os.execvp(argv[3], argv[3:])
    except OSError as e:
        print(f"sem.py: {argv[3]}: {e.strerror}", file=sys.stderr)
        return 127


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
