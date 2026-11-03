#!/usr/bin/env python3
"""A plain N-slot semaphore: sem.py [--fifo] LOCKDIR N -- CMD...

Takes one of the lock files LOCKDIR/slot-0 .. slot-(N-1) with flock(LOCK_EX | LOCK_NB), trying
them all every 50 ms until one is free, then execs CMD with the lock's descriptor inherited: the
slot is held until CMD and every descendant that kept the descriptor have exited. CMD's exit
status and signals are its own, since it replaces this process. 127 when CMD cannot be run.

With --fifo, slots go in arrival order: a waiter appends its pid to LOCKDIR/queue under an
exclusive flock of LOCKDIR/queue.lock, and takes a slot only while its pid is the first live one
in the queue; the pids of waiters that died are dropped. A waiter holds an flock on its own
LOCKDIR/waiter-PID while it waits, and a pid counts as a live waiter only while that lock is held:
the kernel drops it when the waiter exits, so a pid reused by another process does not stall the
queue. Without it, any waiter may win a race.
"""
import fcntl
import os
import sys
import time


def nofollow(path, flags, mode=0o600):
    """os.open that refuses a symbolic link at the last component."""
    return os.open(path, flags | os.O_NOFOLLOW, mode)


def try_slot(lockdir, n):
    for i in range(n):
        fd = nofollow(os.path.join(lockdir, f"slot-{i}"), os.O_RDWR | os.O_CREAT)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return fd
        except BlockingIOError:
            os.close(fd)
    return None


def waiting(lockdir, pid):
    """Whether pid is a waiter still alive: its waiter file's flock is held."""
    try:
        fd = nofollow(os.path.join(lockdir, f"waiter-{pid}"), os.O_RDWR)
    except FileNotFoundError:
        return False
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        return True
    finally:
        os.close(fd)
    return False


def acquire(lockdir, n, fifo=False, poll_s=0.05):
    """The descriptor of the slot taken, waiting until one is free (and, with fifo, our turn)."""
    me, qpath = os.getpid(), os.path.join(lockdir, "queue")
    if fifo:
        mine = os.path.join(lockdir, f"waiter-{me}")
        wfd = nofollow(mine, os.O_RDWR | os.O_CREAT)
        fcntl.flock(wfd, fcntl.LOCK_EX)
    while True:
        if not fifo:
            fd = try_slot(lockdir, n)
        else:
            with open(os.path.join(lockdir, "queue.lock"), "a", opener=nofollow) as ql:
                fcntl.flock(ql, fcntl.LOCK_EX)
                with open(qpath, "a+", opener=nofollow) as q:
                    q.seek(0)  # one pid per line; a line that is not one is dropped
                    pids = [int(x) for x in q.read().split() if x.isdigit()]
                pids = [p for p in pids if p == me or waiting(lockdir, p)]
                if me not in pids:  # the first look, or our line was lost: queue (again) at the end
                    pids.append(me)
                fd = try_slot(lockdir, n) if pids[0] == me else None
                with open(qpath, "w", opener=nofollow) as q:
                    q.write("".join(f"{p}\n" for p in pids if fd is None or p != me))
        if fd is not None:
            if fifo:
                os.unlink(mine)
                os.close(wfd)
            return fd
        time.sleep(poll_s)


def main(argv):
    fifo = argv[:1] == ["--fifo"]
    argv = argv[1:] if fifo else argv
    if len(argv) < 4 or argv[2] != "--" or not argv[1].isdigit() or int(argv[1]) < 1:
        print(__doc__.splitlines()[0], file=sys.stderr)
        return 64
    os.makedirs(argv[0], mode=0o700, exist_ok=True)
    os.set_inheritable(acquire(argv[0], int(argv[1]), fifo), True)
    try:
        os.execvp(argv[3], argv[3:])
    except OSError as e:
        print(f"sem.py: {argv[3]}: {e.strerror}", file=sys.stderr)
        return 127


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
