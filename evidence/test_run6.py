"""Tests of run6.py, its ballast and its run4.run_set arms: python3 -m unittest evidence/test_run6.py"""
import json
import os
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import ballast  # noqa: E402
import overload  # noqa: E402
import run4  # noqa: E402
import run6  # noqa: E402


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


def text(path):
    with open(path) as f:
        return f.read()


def gate(name, heavy=True):
    return {"gate": name, "label": f"bench:{name}", "cwd": "/", "heavy": heavy, "argv": [name]}


def stand(root):
    return {"coreslot": "c", "state_dir": os.path.join(root, "state"), "window_s": 5,
            "agents": {"A1": [gate("tc")], "A2": [gate("lint", False), gate("tc")],
                       "A3": [gate("ruff", False), gate("tests")], "A4": [gate("orm")]}}


class Rooted(unittest.TestCase):
    """A temporary ROOT holding the stand's state directory and an OUT."""

    def setUp(self):
        self.root = tempfile.mkdtemp()
        p = mock.patch.object(run4, "ROOT", self.root)
        p.start()
        self.addCleanup(p.stop)
        self.cfg, self.out = stand(self.root), os.path.join(self.root, "out")


class Learn(Rooted):
    def setUp(self):
        super().setUp()
        self.runs, self.cs_calls = [], []

        def run_one(cfg, g, form):
            self.runs.append((g["gate"], form))
            return 0, ""

        def cs(cfg, *args, **kw):
            self.cs_calls.append(args)
            if args == ("suggest", "--write"):
                with open(os.path.join(cfg["state_dir"], "needs.toml"), "w") as f:
                    f.write('[needs]\n  [needs."bench:tc"]\n    cpu = "5"\n    mem = "8576M"\n')
            return subprocess.CompletedProcess(args, 0, "", "")
        for name, fake in (("run_one", run_one), ("cs", cs)):
            p = mock.patch.object(run4, name, fake)
            p.start()
            self.addCleanup(p.stop)
        p = mock.patch.object(run6, "descendants_rss_kib", lambda: 2_000_000)
        p.start()
        self.addCleanup(p.stop)

    def learn(self):
        with mock.patch("sys.stdout"):
            run6.learn(self.cfg, self.out)

    def test_hand_entry(self):
        # mutations: round() for the mem ceiling -> 8.3 GiB gives 8G -> red; round() for the cpu
        # ceiling -> 4.2 gives 4 -> red
        self.assertEqual(run6.hand_entry(8_700_000, 4.8), {"mem": "9G", "cpu": "5", "pinned": True})
        self.assertEqual(run6.hand_entry(1_048_576, 1.0), {"mem": "1G", "cpu": "1", "pinned": True})
        self.assertEqual(run6.hand_entry(2_000_000, 4.2)["cpu"], "5")

    def test_learn_runs_and_writes(self):
        self.learn()
        want = [(g, form) for g in ("tc", "lint", "ruff", "tests", "orm")
                for form in ["K"] * 6 + ["F"] * 3]
        self.assertEqual(self.runs, want)
        self.assertEqual([c for c in self.cs_calls if c[0] == "suggest"], [("suggest", "--write")])
        for name in ("needs.toml", "needs-hand.toml", "needs.sha256"):
            self.assertTrue(os.path.exists(os.path.join(self.out, name)), name)
        self.assertEqual(text(os.path.join(self.cfg["state_dir"], "config.toml")),
                         '[resources]\ncpu = "140%"\nmem = "60%"\n')
        hand = text(os.path.join(self.out, "needs-hand.toml"))
        self.assertIn('[needs."bench:orm"]\n    cpu = "1"\n    mem = "2G"\n    pinned = true\n',
                      hand)
        self.assertEqual(text(os.path.join(self.out, "needs.sha256")),
                         f"{run4.sha256(os.path.join(self.out, 'needs.toml'))}  needs.toml\n"
                         f"{run4.sha256(os.path.join(self.out, 'needs-hand.toml'))}"
                         "  needs-hand.toml\n")
        recs = run4.read_jsonl(os.path.join(self.out, "learn.jsonl"))
        self.assertEqual(len(recs), 45)
        self.assertEqual({k for k in recs[0]}, {"started", "gate", "label", "form", "run", "exit",
                                                "wall_s", "peak_kib", "cores", "failures"})

    def test_learn_resumes_same_state(self):
        # mutation: call fresh_state whenever learn starts -> the state is reset on resume -> red
        os.makedirs(self.out)
        run4.fresh_state(self.cfg)
        run6.write_learn_state(self.cfg, self.out)
        with open(os.path.join(self.out, "learn.jsonl"), "w") as f:
            for i, form in enumerate(["K"] * 6 + ["F"] * 2):
                n = i + 1 if form == "K" else i - 5
                f.write(json.dumps({"gate": "tc", "label": "bench:tc", "form": form, "run": n,
                                    "exit": 0, "peak_kib": 8_700_000, "cores": 4.8}) + "\n")
        fresh = []
        with mock.patch.object(run4, "fresh_state", lambda cfg: fresh.append(cfg)):
            self.learn()
        self.assertEqual(fresh, [])
        self.assertEqual(self.runs[:2], [("tc", "F"), ("lint", "K")])
        self.assertEqual(len(self.runs), 45 - 8)
        hand = text(os.path.join(self.out, "needs-hand.toml"))
        self.assertIn('[needs."bench:tc"]\n    cpu = "5"\n    mem = "9G"\n', hand)

    def test_learn_refuses_after_records(self):
        # mutation: drop the pilot pattern from the refusal -> learn runs -> red
        os.makedirs(self.out)
        open(os.path.join(self.out, "pilot-heavy.jsonl"), "w").close()
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            self.learn()
        self.assertEqual(cm.exception.code, 2)
        self.assertEqual(self.runs, [])


if __name__ == "__main__":
    unittest.main()
