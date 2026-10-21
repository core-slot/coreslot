"""Tests of run6.py, its ballast and its run4.run_set arms: python3 -m unittest evidence/test_run6.py"""
import os
import signal
import subprocess
import sys
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import ballast  # noqa: E402
import overload  # noqa: E402


def alive(pid):
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    return True


def kill_quietly(pid):
    if pid:
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass


def rss_kib(pid):
    r = subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True)
    return int(r.stdout) if r.stdout.strip().isdigit() else 0


class Ballast(unittest.TestCase):
    def test_ballast_zero_is_noop(self):
        with ballast.Ballast(0) as b:
            self.assertIsNone(b.pid)
            self.assertIsNone(b.rss_mib())

    def test_ballast_holds_and_releases(self):
        # mutations: `mib << 10` for `mib << 20` in hold (KiB, not MiB) -> the RSS stays near
        # 17 MiB -> red; skip the SIGTERM in __exit__ -> the pid is still there -> red
        b = ballast.Ballast(64)
        with b:
            self.addCleanup(kill_quietly, b.pid)
            deadline = time.monotonic() + 10
            while rss_kib(b.pid) < 60 << 10 and time.monotonic() < deadline:
                time.sleep(0.2)
            self.assertGreaterEqual(rss_kib(b.pid), 60 << 10)
        self.assertFalse(alive(b.pid))

    def test_ballast_dies_with_parent(self):
        # mutation: drop the parent check in hold's loop -> the ballast outlives its parent -> red
        helper = subprocess.Popen(
            [sys.executable, "-c", "import sys, time; sys.path.insert(0, sys.argv[1]);"
             " import ballast; b = ballast.Ballast(64).__enter__(); print(b.pid, flush=True);"
             " time.sleep(60)", HERE], stdout=subprocess.PIPE, text=True)
        self.addCleanup(helper.wait)
        self.addCleanup(kill_quietly, helper.pid)
        pid = int(helper.stdout.readline())
        helper.stdout.close()
        self.addCleanup(kill_quietly, pid)
        self.assertTrue(alive(pid))
        helper.kill()
        helper.wait()
        deadline = time.monotonic() + 2
        while alive(pid) and time.monotonic() < deadline:
            time.sleep(0.1)
        self.assertFalse(alive(pid))

    def test_ballast_ready_timeout(self):
        # mutation: skip the kill on a failed entry -> the child keeps allocating -> red
        b = ballast.Ballast(64, ready_timeout_s=0.01)
        try:
            with self.assertRaises(TimeoutError):
                b.__enter__()
            self.assertIsNotNone(b.pid)
            self.assertFalse(alive(b.pid))
        finally:
            kill_quietly(b.pid)


class Sampler(unittest.TestCase):
    def test_sampler_has_swapins_pageins(self):
        s = overload.Sampler()
        s.start()
        try:
            deadline = time.monotonic() + 5
            while not s.samples and time.monotonic() < deadline:
                time.sleep(0.05)
        finally:
            s.stop.set()
            s.join()
        for key in ("swapouts", "swapins", "pageins"):
            self.assertIsInstance(s.samples[0][key], int, key)


if __name__ == "__main__":
    unittest.main()
