"""Tests of run6.py, its ballast and its run_set arms: python3 -m unittest evidence/test_run6.py"""
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


class RunSet(Rooted):
    """run4.run_set with the gates, the pacing, the machine and the sampler faked."""

    def setUp(self):
        super().setUp()
        self.acquired, self.slots, self.gates = [], [], []
        real = run4.threading.Semaphore
        test = self

        class Sem(real):
            def __init__(self, value=1):
                test.slots.append(value)
                super().__init__(value)

            def acquire(self, *a, **kw):
                test.acquired.append(1)
                return super().acquire(*a, **kw)

        class Sampler:
            def __init__(self):
                self.samples, self.stop = [], mock.Mock()

            def start(self):
                pass

            def join(self):
                pass

        def run_one(cfg, g, arm):
            self.gates.append(g["gate"])
            return 0, ""
        for target, name, fake in (
                (run4.threading, "Semaphore", Sem), (run4.overload, "Sampler", Sampler),
                (run4, "run_one", run_one), (run4, "on_ac", lambda: True),
                (run4, "background", lambda: (16384, {"load1": 0.0})),
                (run4, "budget_empty", lambda cfg: True),
                (run4.acceptance, "cooldown", lambda *a: {"cooldown_gave_up": False}),
                (run4.acceptance, "runlog_lines", lambda cfg: [])):
            p = mock.patch.object(target, name, fake)
            p.start()
            self.addCleanup(p.stop)
        self.cfg["agents"] = {"A1": [gate("tc")], "A2": [gate("lint", False)]}
        self.path = os.path.join(self.root, "sets.jsonl")

    def run_set(self, arm, **kw):
        self.acquired.clear()
        with mock.patch("sys.stdout"):
            run4.run_set(self.cfg, self.path, arm=arm, offsets={"A1": 0, "A2": 0}, meta={},
                         baseline=1.0, **kw)
        return run4.read_jsonl(self.path)[-1]

    def test_run_set_semaphore_arms(self):
        # mutation: `arm == "S"` for `arm.startswith("S")` in run_set -> S2 holds nothing -> red
        rec = self.run_set("S2", slots=2)
        self.assertEqual((rec["arm"], self.acquired, self.slots[-1]), ("S2", [1], 2))
        rec = self.run_set("S", slots=2)  # run 4's and run 5's semaphore arm
        self.assertEqual((rec["arm"], self.acquired, self.slots[-1]), ("S", [1], 2))
        self.run_set("F")
        self.assertEqual(self.acquired, [])
        self.assertEqual(sorted(self.gates), ["lint", "lint", "lint", "tc", "tc", "tc"])

    def test_cold_arm_has_no_needs(self):
        # mutation: copy the needs file whatever `needs` is (shutil.copyfile(None, ...) raises,
        # so: skip the removal) -> the earlier set's needs.toml stays -> red
        run4.fresh_state(self.cfg)
        needs = os.path.join(self.root, "needs.toml")
        with open(needs, "w") as f:
            f.write("[needs]\n")
        self.assertIsNone(run6.arm_kw(self.out, "KC")["needs"])
        self.assertEqual(run6.arm_kw(self.out, "KC")["kshare"], {"cpu": "140%", "mem": "60%"})
        self.assertEqual(run6.arm_kw(self.out, "S3"), {"slots": 3})
        self.assertEqual(run6.arm_kw(self.out, "K67")["kshare"]["mem"], "67%")
        self.assertEqual(run6.arm_kw(self.out, "KH")["needs"],
                         os.path.join(self.out, "needs-hand.toml"))
        dst = os.path.join(self.cfg["state_dir"], "needs.toml")
        rec = self.run_set("K60", kshare={"cpu": "140%", "mem": "60%"}, needs=needs)
        self.assertTrue(os.path.exists(dst))
        self.assertTrue(rec["valid"], rec["invalid"])
        rec = self.run_set("KC", **run6.arm_kw(self.out, "KC"))
        self.assertFalse(os.path.exists(dst))
        self.assertTrue(rec["valid"], rec["invalid"])


class Design(unittest.TestCase):
    def test_levels_balanced(self):
        # mutation: shuffle with Random(f"run6-level-{phase}-{block}") -> a group repeats a level
        # -> red
        for phase in ("pilot", "main"):
            for group in range(4):
                levels = [run6.level_for(phase, 3 * group + i) for i in (1, 2, 3)]
                self.assertEqual(sorted(levels), list(run6.LEVELS_MIB), (phase, group))

    def test_order_is_a_permutation(self):
        # mutation: drop the block from order_for's seed -> blocks 1 and 2 share an order -> red
        for b in range(1, 13):
            self.assertEqual(sorted(run6.order_for("main", "heavy", b)), sorted(run6.HEAVY_ARMS))
        self.assertNotEqual(run6.order_for("main", "heavy", 1), run6.order_for("main", "heavy", 2))
        self.assertEqual(sorted(run6.order_for("sens", "cold", 1)), sorted(run6.COLD_ARMS))


class Verdict(Rooted):
    """run6.verdict with run_set, the ballast and the session's probes faked."""

    def setUp(self):
        super().setUp()
        os.makedirs(self.out)
        for name in run6.NEEDS_FILES:
            with open(os.path.join(self.out, name), "w") as f:
                f.write(f"# {name}\n")
        run6.seal(self.out)
        self.calls, self.levels = [], []
        test = self

        class Ballast:
            def __init__(self, mib):
                test.levels.append(mib)

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                pass

            def rss_mib(self):
                return None

        def run_set(cfg, path, **kw):
            self.calls.append({"cfg": cfg, "path": path, **kw})
        for target, name, fake in (
                (run4, "run_set", run_set), (run6, "Ballast", Ballast),
                (run6, "static_sizes", lambda cfg: {"60%": {"cpu": 14, "mem": 76},
                                                    "67%": {"cpu": 14, "mem": 85}}),
                (run6, "running_apps", lambda: ["Terminal"]),
                (run6.acceptance, "probe", lambda: 1.0)):
            p = mock.patch.object(target, name, fake)
            p.start()
            self.addCleanup(p.stop)

    def record(self, phase, mix, block, arms):
        with open(os.path.join(self.out, f"{phase}-{mix}.jsonl"), "a") as f:
            for arm in arms:
                f.write(json.dumps({"block": block, "arm": arm}) + "\n")

    def test_resume_keeps_level_and_order(self):
        # mutation: level_for(phase, first) for level_for(phase, src) in verdict -> block 5
        # runs at block 4's level, which differs inside one group -> red
        self.record("main", "heavy", 4, run6.HEAVY_ARMS)
        o5, o6 = run6.order_for("main", "heavy", 5), run6.order_for("main", "heavy", 6)
        self.record("main", "heavy", 5, o5[:3])
        run6.verdict(self.cfg, self.out, "main", "heavy", 4, 6)
        got = [(c["meta"]["block"], c["arm"]) for c in self.calls]
        self.assertEqual(got, [(5, a) for a in o5[3:]] + [(6, a) for a in o6])
        self.assertEqual(self.levels, [run6.level_for("main", 5), run6.level_for("main", 6)])
        self.assertNotEqual(run6.level_for("main", 4), run6.level_for("main", 5))
        c = self.calls[0]
        self.assertEqual((c["meta"]["level_mib"], c["meta"]["position"], c["meta"]["prev_arm"]),
                         (run6.level_for("main", 5), 3, o5[2]))
        self.assertEqual(c["path"], os.path.join(self.out, "main-heavy.jsonl"))
        self.assertEqual(c["offsets"], run4.offsets_for(self.cfg, "run6-main-heavy-5"))

    def test_as_block_reuses(self):
        # mutation: seed the offsets with the block for `--as-block` -> block 13's offsets -> red
        run6.verdict(self.cfg, self.out, "main", "heavy", 13, 13, as_block=4)
        self.assertEqual([c["arm"] for c in self.calls], run6.order_for("main", "heavy", 4))
        self.assertEqual(self.levels, [run6.level_for("main", 4)])
        for c in self.calls:
            self.assertEqual(c["offsets"], run4.offsets_for(self.cfg, "run6-main-heavy-4"))
            self.assertEqual((c["meta"]["block"], c["meta"]["as_block"]), (13, 4))

    def test_needs_hash_checked(self):
        # mutation: return early from check_needs -> the changed file is used -> red
        with open(os.path.join(self.out, "needs.toml"), "a") as f:
            f.write("# changed\n")
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr") as err:
            run6.verdict(self.cfg, self.out, "main", "heavy", 1, 1)
        self.assertEqual(cm.exception.code, 2)
        self.assertIn("needs.toml", "".join(str(c) for c in err.write.call_args_list))
        self.assertEqual(self.calls, [])

    def test_light_mix_agents(self):
        # mutation: pass the stand's cfg to run_set for the light mix -> four agents -> red
        run6.verdict(self.cfg, self.out, "main", "light", 1, 1)
        self.assertEqual(sorted(c["arm"] for c in self.calls), sorted(run6.LIGHT_ARMS))
        self.assertEqual(self.levels, [0])
        for c in self.calls:
            self.assertEqual(list(c["cfg"]["agents"]), ["A3", "A4"])
            self.assertEqual(list(c["offsets"]), ["A3", "A4"])


class BallastOnError(Rooted):
    def test_ballast_stopped_on_error(self):
        # mutation: hold the ballast with a bare __enter__ and no `with` -> it outlives the
        # error -> red
        os.makedirs(self.out)
        for name in run6.NEEDS_FILES:
            open(os.path.join(self.out, name), "w").close()
        run6.seal(self.out)
        started = []

        class Small(ballast.Ballast):
            def __init__(self, mib):
                super().__init__(64)
                started.append(self)

        def boom(*a, **kw):
            raise RuntimeError("boom")
        with mock.patch.object(run6, "Ballast", Small), mock.patch.object(run4, "run_set", boom), \
                mock.patch.object(run6, "static_sizes", lambda cfg: {}), \
                mock.patch.object(run6, "running_apps", lambda: []), \
                mock.patch.object(run6.acceptance, "probe", lambda: 1.0), \
                mock.patch.object(run6, "level_for", lambda phase, block: 64):
            with self.assertRaises(RuntimeError):
                run6.verdict(self.cfg, self.out, "main", "heavy", 1, 1)
        self.assertEqual(len(started), 1)
        self.addCleanup(kill_quietly, started[0].pid)
        self.assertFalse(alive(started[0].pid))


if __name__ == "__main__":
    unittest.main()
