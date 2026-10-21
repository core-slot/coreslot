"""Tests of run6.py, its ballast and its run_set arms: python3 -m unittest evidence/test_run6.py"""
import hashlib
import json
import math
import os
import re
import signal
import statistics
import subprocess
import sys
import tempfile
import time
import unittest
import xml.etree.ElementTree as ET
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import ballast  # noqa: E402
import overload  # noqa: E402
import run4  # noqa: E402
import run6  # noqa: E402
import stats6  # noqa: E402


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
        p = mock.patch.object(run6, "descendants_kib", lambda: 2_000_000)
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


class Meter(Rooted):
    @unittest.skipUnless(sys.platform == "darwin", "the physical footprint is macOS's")
    def test_footprint_counts_shared_pages_once(self):
        # mutation: sum each descendant's RSS in descendants_kib -> the file the child and its
        # three forked children read counts four times, over 256 MiB -> red
        data = os.path.join(self.root, "data")
        with open(data, "wb") as f:
            f.write(b"x" * (64 << 20))
        child = subprocess.Popen([sys.executable, "-c", (
            "import mmap, os, sys, time\nb = b'y' * (32 << 20)\n"
            "m = mmap.mmap(os.open(sys.argv[1], os.O_RDONLY), 0, prot=mmap.PROT_READ)\n"
            "for _ in range(3):\n    if os.fork() == 0:\n        m[::4096]; time.sleep(60)\n"
            "m[::4096]; print('ok', flush=True); time.sleep(60)"),
            data], stdout=subprocess.PIPE, text=True, start_new_session=True)
        self.addCleanup(child.wait)
        self.addCleanup(os.killpg, child.pid, signal.SIGKILL)
        self.assertEqual(child.stdout.readline(), "ok\n")
        time.sleep(1)  # the forked children's reads
        self.assertTrue(32 << 10 <= run6.descendants_kib() < 64 << 10)

    def test_interrupted_gate_is_stopped(self):
        # mutation: drop run4.run_gate's stop on an exception -> learn's interrupted gate, its
        # sleep, outlives the interrupt -> red
        pidfile = os.path.join(self.root, "pid")
        g = dict(gate("sleeper"), argv=["sh", "-c", f"sleep 37 & echo $! > {pidfile}; wait"])

        def interrupt(*_):
            raise KeyboardInterrupt
        self.addCleanup(signal.signal, signal.SIGALRM, signal.signal(signal.SIGALRM, interrupt))
        signal.alarm(1)
        with self.assertRaises(KeyboardInterrupt):
            run6.measured(self.cfg, g, "F")
        pid = int(text(pidfile))
        self.addCleanup(kill_quietly, pid)
        deadline = time.monotonic() + 2
        while alive(pid) and time.monotonic() < deadline:
            time.sleep(0.1)
        self.assertFalse(alive(pid))


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

    def record(self, phase, mix, block, arms, valid=True):
        with open(os.path.join(self.out, f"{phase}-{mix}.jsonl"), "a") as f:
            for arm in arms:
                f.write(json.dumps({"block": block, "arm": arm, "valid": valid}) + "\n")

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
        # mutation: record the apps' names -> "Terminal" is in the record -> red
        self.assertEqual((c["meta"]["apps_count"], c["meta"]["apps_sha256"]),
                         (1, hashlib.sha256(b"Terminal").hexdigest()))
        self.assertNotIn("Terminal", json.dumps(c["meta"]))
        self.assertIn('["Terminal"]', text(os.path.join(self.out, "apps.local.txt")))

    def test_as_block_reuses(self):
        # mutations: seed the offsets with the block for `--as-block` -> block 13's offsets ->
        # red; drop the refusal of a block with no invalid set -> block 5 is re-run -> red
        self.record("main", "heavy", 5, run6.HEAVY_ARMS)
        self.record("main", "heavy", 4, ["F"], valid=False)
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            run6.verdict(self.cfg, self.out, "main", "heavy", 13, 13, as_block=5)
        self.assertEqual((cm.exception.code, self.calls), (2, []))
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


# --- analysis fixtures ----------------------------------------------------------------------------

HEAVY_BASE = {"F": 100, "S1": 92, "S2": 90, "S3": 94, "K60": 80, "K67": 82, "KH": 81}
HEAVY_WARN = {"F": 20, "S1": 10, "S2": 10, "S3": 10, "K60": 2, "K67": 2, "KH": 2}
HEAVY_SWAP = {"F": 1000, "S1": 500, "S2": 500, "S3": 500, "K60": 100, "K67": 100, "KH": 100}


def noise(block, arm):
    """A deterministic spread of -2 % to +2 % so that no log ratio is constant."""
    return 1 + ((block * 7 + sum(map(ord, arm)) * 3) % 5 - 2) / 100


def sset(phase, mix, block, arm, verdict, warn=0, swap=0, valid=True, as_block=None):
    """A minimal set record: `warn` samples at pressure level 2, swapouts rising by `swap`."""
    src = as_block or block
    order = run6.order_for(phase, mix, src)
    pos = order.index(arm)
    samples = [{"t": i, "pressure": 2 if i < warn else 1, "swapouts": swap if i == warn + 1 else 0,
                "swapins": 0, "pageins": 0} for i in range(warn + 2)]
    return {"phase": phase, "mix": mix, "block": block, "as_block": as_block, "arm": arm,
            "valid": valid, "invalid": [] if valid else ["battery"],
            "started": "1970-01-01T00:00:00+00:00", "order": " ".join(order),
            "level_mib": run6.level_for(phase, src) if mix == "heavy" else 0, "position": pos,
            "prev_arm": order[pos - 1] if pos else None, "mean_verdict_s": verdict,
            "verdict_s": {"A1": verdict}, "makespan_s": verdict + 10, "samples": samples,
            "page_bytes": 16384, "runlog": [{"label": "bench:tc", "time_ns": 0, "ran_s": verdict,
                                             "exit": 0, "fail_open": ""}] * arm.startswith("K"),
            "gates": [{"agent": "A1", "gate": "tc", "label": "bench:tc", "start": 0,
                       "end": verdict, "exit": 0}]}


def heavy_block(block, phase="main", values=None, invalid=(), as_block=None):
    """A heavy block's seven sets: HEAVY_BASE with noise, `values` overriding an arm's verdict."""
    v = {a: HEAVY_BASE[a] * noise(block, a) for a in run6.HEAVY_ARMS}
    v.update(values or {})
    return [sset(phase, "heavy", block, a, round(v[a], 2), HEAVY_WARN[a], HEAVY_SWAP[a],
                 a not in invalid, as_block) for a in run6.HEAVY_ARMS]


def light_block(block, phase="main", k60=1.0, f_warn=0):
    return [sset(phase, "light", block, "K60", round(50 * k60 * noise(block, "K60"), 2)),
            sset(phase, "light", block, "F", round(50 * noise(block, "F"), 2), warn=f_warn)]


def write(out, name, recs):
    with open(os.path.join(out, name), "a") as f:
        for r in recs:
            f.write(json.dumps(r) + "\n")


def edit(out, name, change):
    """Rewrites out/name with change(record) applied to every record."""
    recs = run4.read_jsonl(os.path.join(out, name))
    with open(os.path.join(out, name), "w") as f:
        for r in recs:
            change(r)
            f.write(json.dumps(r) + "\n")


def write_size(out, n_heavy=30, n_light=12, pressure=0):
    with open(os.path.join(out, "size.json"), "w") as f:
        json.dump({"n_heavy": n_heavy, "n_light": n_light, "light_pressure_sets": pressure}, f)


def write_main(out, heavy=range(1, 31), light=range(1, 13), values=None, k60_light=1.0):
    """main-heavy and main-light; `values(block)` gives a heavy block's overrides."""
    for b in heavy:
        write(out, "main-heavy.jsonl", heavy_block(b, values=values(b) if values else None))
    for b in light:
        write(out, "main-light.jsonl", light_block(b, k60=k60_light))


def levels_of(blocks, phase="main"):
    """level -> its blocks in block order."""
    levels = {}
    for b in blocks:
        levels.setdefault(run6.level_for(phase, b), []).append(b)
    return levels


class Analysis(unittest.TestCase):
    def setUp(self):
        self.out = tempfile.mkdtemp()

    def summary(self):
        with mock.patch("sys.stdout"):
            run6.summary(self.out)
        with open(os.path.join(self.out, "summary.json")) as f:
            return json.load(f)

    def test_summary_ignores_pilot(self):
        # mutation: read every `*-heavy.jsonl` in summary (pilot and main) -> the pilot's S1 wins
        # four blocks of each level and S* becomes 1 -> red
        write_size(self.out)
        write_main(self.out)
        for b in range(1, 13):
            write(self.out, "pilot-heavy.jsonl", heavy_block(b, "pilot", {"S1": 40}))
        s = self.summary()
        self.assertEqual(s["oracles"]["S"], {"0": "S2", "1536": "S2", "3072": "S2"})
        self.assertEqual(s["tests"]["H1"]["status"], "rejected")

    def test_invalid_block_dropped_everywhere(self):
        # mutation: drop `sets[a]["valid"]` from valid_blocks' check -> block 7 counts in the
        # blocks used, and its S3 and K67 would flip S* and K*cf -> red
        write_size(self.out)
        write_main(self.out, heavy=range(1, 32),
                   values=lambda b: {"S3": 10, "K67": 10} if b == 7 else None)
        edit(self.out, "main-heavy.jsonl",
             lambda r: r.update(valid=not (r["block"] == 7 and r["arm"] == "K60")))
        want = [b for b in range(1, 32) if b != 7]
        s = self.summary()
        self.assertEqual(s["blocks"]["heavy"], want)
        self.assertEqual(s["oracles"]["S"], {"0": "S2", "1536": "S2", "3072": "S2"})
        self.assertEqual(sorted(map(int, s["oracles"]["Kcf"])), want)
        self.assertEqual(set(s["oracles"]["Kcf"].values()), {"K60"})
        for h in ("H1", "H3", "H4", "H5"):
            self.assertEqual(s["tests"][h]["blocks"], want, h)
        write(self.out, "main-heavy.jsonl", heavy_block(32, as_block=7))
        s = self.summary()
        self.assertEqual(s["blocks"]["heavy"], list(range(1, 31)))
        self.assertIn("7", s["oracles"]["Kcf"])
        for h in ("H1", "H3", "H4", "H5"):
            self.assertEqual(s["tests"][h]["blocks"], list(range(1, 31)), h)

    def test_chain_stops(self):
        # mutations: keep going after a non-rejection (`going = True` for `going = h["reject"]`)
        # -> H4 is "rejected" -> red; CHAIN in the order H1-H5 -> a failed H2 stops H3 -> red;
        # drop "estimate" from H1, H2 or H3 -> KeyError -> red
        write_size(self.out)
        write_main(self.out, k60_light=1.10)
        t = self.summary()["tests"]
        self.assertEqual(list(t), ["H1", "H4", "H5", "H3", "H2"])
        self.assertEqual([h["status"] for h in t.values()], ["rejected"] * 4 + ["not rejected"])
        for h, gmr in (("H1", 0.8), ("H2", 1.1), ("H3", 80 / 90)):
            self.assertAlmostEqual(t[h]["estimate"]["gmr"], gmr, delta=0.02)
            self.assertLess(t[h]["estimate"]["bca90"][0], t[h]["estimate"]["upper"])
        os.remove(os.path.join(self.out, "main-heavy.jsonl"))
        write_main(self.out, light=(), values=lambda b: {"K60": 105})
        t = self.summary()["tests"]
        self.assertTrue(t["H3"]["reject"])
        self.assertEqual([h["status"] for h in t.values()],
                         ["not rejected"] + ["exploratory"] * 4)
        write_size(self.out, pressure=2)
        for mix in ("heavy", "light"):
            os.remove(os.path.join(self.out, f"main-{mix}.jsonl"))
        write_main(self.out)
        t = self.summary()["tests"]
        self.assertTrue(t["H2"]["reject"])
        self.assertEqual([h["status"] for h in t.values()], ["rejected"] * 4 + ["untested"])
        write_size(self.out, pressure=1)
        t = self.summary()["tests"]
        self.assertEqual([h["status"] for h in t.values()], ["rejected"] * 5)

    def test_oracle_per_level(self):
        # mutations: choose S* over all the blocks for every level -> one N everywhere -> red;
        # S_ARMS without F -> S2 at 3072 MiB, where F, four slots, is the fastest -> red
        best = {0: "S3", 1536: "S2", 3072: "F"}
        write_size(self.out)
        write_main(self.out, values=lambda b: {best[run6.level_for("main", b)]: 85})
        self.assertEqual(self.summary()["oracles"]["S"], {"0": "S3", "1536": "S2", "3072": "F"})

    def test_cross_fit_uses_other_half(self):
        # mutation: cross_fit choosing on all the level's blocks -> K67's mean, 77, beats K60's 80
        # everywhere -> every block gets K67 -> red
        half = {b: i % 2 for bs in levels_of(range(1, 31)).values() for i, b in enumerate(bs)}
        write_size(self.out)
        write_main(self.out, values=lambda b: {"K60": 80, "K67": 70 if half[b] == 0 else 84})
        kcf = self.summary()["oracles"]["Kcf"]
        self.assertEqual({int(b): a for b, a in kcf.items()},
                         {b: "K60" if half[b] == 0 else "K67" for b in range(1, 31)})

    def test_h5_bonferroni(self):
        # mutation: `p < ALPHA` for `p < ALPHA / 6` in H5 -> 0.012 rejects -> red
        write_size(self.out)
        write_main(self.out)
        for p, want in ((0.012, "not rejected"), (0.008, "rejected")):
            with mock.patch.object(run6.run4, "sign_test", lambda w, n: p), \
                    mock.patch.object(run6.stats6, "paired_lower",
                                      lambda d, b: {"p": p, "mean": 0.1, "lower": 0.05}):
                t = self.summary()["tests"]
            self.assertEqual(t["H4"]["status"], "rejected")
            self.assertEqual(t["H5"]["status"], want, p)
        # mutation: drop the "test" label from H5's rows -> KeyError -> red
        rows = t["H5"]["per_n"]["1"]["tests"]
        self.assertEqual([(r["level"], r["test"]) for r in rows],
                         [(lv, k) for lv in run6.LEVELS_MIB for k in ("time", "warn")])
        self.assertEqual((rows[0]["gmr"], rows[0]["lower"], rows[1]["wins"]),
                         (round(math.exp(0.1), 4), round(math.exp(0.05), 4), 10))

    def size(self):
        with mock.patch("sys.stdout"):
            run6.size(self.out)
        with open(os.path.join(self.out, "size.json")) as f:
            return json.load(f)

    def test_size_rule(self):
        # mutations: pass sigma-hat, not its upper bound, to blocks_needed -> n_heavy 69, not 108
        # -> red; size over every valid pilot block -> blocks 13-15 move sigma-hat -> red; count
        # every light F record with a warn sample -> block 5's invalid one makes 3 -> red; drop
        # the refusal once a main record exists -> size runs -> red
        dev = [0.12, -0.10, 0.15, -0.05, 0.02, -0.14, 0.08, -0.02, 0.11, -0.09, 0.0, -0.08]
        for b in range(1, 16):
            write(self.out, "pilot-heavy.jsonl", heavy_block(b, "pilot", {
                "K60": 80 * math.exp(dev[b - 1]) if b < 13 else 200, "K67": 100, "S1": 95,
                "S2": 90, "S3": 95}))
            write(self.out, "pilot-light.jsonl", light_block(b, "pilot", f_warn=int(b < 3)))
        write(self.out, "pilot-light.jsonl", [sset("pilot", "light", 5, "F", 50, 1, valid=False)])
        got = self.size()
        ln = [math.log(round(80 * math.exp(x), 2) / 90) for x in dev]
        self.assertAlmostEqual(got["sigma_hat_heavy"], statistics.stdev(ln), places=9)
        self.assertAlmostEqual(got["sigma_heavy"],
                               stats6.sigma_upper(statistics.stdev(ln), 12), places=9)
        self.assertEqual((got["n_heavy"], got["n_heavy_unclamped"], got["n_light"]), (108, 108, 12))
        self.assertEqual((got["light_pressure_sets"], got["underpowered"]), (2, False))
        # mutation: drop the underpowered update -> 108 above a cap of 90 is not flagged -> red
        with mock.patch.object(run6, "N_HEAVY", (30, 90, 3)):
            got = self.size()
        self.assertEqual((got["n_heavy"], got["n_heavy_unclamped"], got["underpowered"]),
                         (90, 108, True))
        edit(self.out, "pilot-light.jsonl", lambda r: r.update(valid=r["block"] < 3))
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            run6.size(self.out)
        self.assertEqual(cm.exception.code, 2)
        write(self.out, "main-heavy.jsonl", heavy_block(1))
        os.remove(os.path.join(self.out, "pilot-light.jsonl"))
        for b in range(1, 13):
            write(self.out, "pilot-light.jsonl", light_block(b, "pilot"))
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            run6.size(self.out)
        self.assertEqual(cm.exception.code, 2)

    def test_incomplete_when_cap_spent(self):
        # mutations: `<=` for `<` in summary's re-run cap check -> with the 3 re-runs spent it
        # still exits 2 -> red; test only when every mix is full -> the short light mix leaves
        # H1 untested -> red
        write_size(self.out)
        write_main(self.out, heavy=range(1, 30))
        write(self.out, "main-heavy.jsonl", heavy_block(30, invalid=("F",)))
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            self.summary()
        self.assertEqual(cm.exception.code, 2)
        for b in (31, 32, 33):
            write(self.out, "main-heavy.jsonl", heavy_block(b, invalid=("F",), as_block=30))
        s = self.summary()
        self.assertEqual([h["status"] for h in s["tests"].values()],
                         ["untested"] + ["exploratory"] * 4)
        self.assertTrue(all("untested" in s["tests"][h] for h in ("H1", "H4", "H5", "H3")))
        self.assertTrue(s["tests"]["H2"]["reject"])
        self.assertIn("incomplete: heavy", s["incomplete"])
        self.assertEqual(s["reruns"]["heavy"], [31, 32, 33])
        self.assertEqual(s["reported"]["per_arm"]["heavy"]["F"]["mean_verdict_s"],
                         round(statistics.fmean(100 * noise(b, "F") for b in range(1, 30)), 1))
        with open(os.path.join(self.out, "summary.txt")) as f:
            self.assertIn("incomplete", f.read())
        os.remove(os.path.join(self.out, "main-light.jsonl"))
        write_main(self.out, heavy=range(34, 35), light=range(1, 12))
        write(self.out, "main-light.jsonl", light_block(12)[:1] + [
            sset("main", "light", b, "F", 50, valid=False, as_block=12 if b > 12 else None)
            for b in (12, 13)])
        t = self.summary()["tests"]
        self.assertEqual([h["status"] for h in t.values()], ["rejected"] * 4 + ["untested"])

    def test_rerun_only_for_invalid_within_cap(self):
        # mutations: prefer a valid re-run to the block's own valid sets -> block 5's F is 130
        # -> red; drop the cap in capped -> re-run 35 completes block 2 -> red
        write_size(self.out)
        write_main(self.out, heavy=range(1, 32),
                   values=lambda b: {"F": 130} if b in (1, 2) else None)
        edit(self.out, "main-heavy.jsonl",
             lambda r: r.update(valid=not (r["block"] in (1, 2) and r["arm"] == "F")))
        for b, src, ok in ((32, 5, True), (33, 1, True), (34, 2, False), (35, 2, True)):
            write(self.out, "main-heavy.jsonl", heavy_block(
                b, values={"F": 130}, invalid=() if ok else ("F",), as_block=src))
        s = self.summary()
        self.assertEqual(s["blocks"]["heavy"], [1] + list(range(3, 32)))
        self.assertEqual(s["reruns_ignored"]["heavy"], [35])
        f = [130] + [round(100 * noise(b, "F"), 2) for b in range(3, 32)]
        self.assertEqual(s["reported"]["per_arm"]["heavy"]["F"]["mean_verdict_s"],
                         round(statistics.fmean(f), 1))

    def test_coreslot_failure_counts_against_it(self):
        # mutations: drop `line is None`, `line.get("fail_open")` or the CORESLOT_EXITS check in
        # coreslot_failed -> block 1, 2 or 3 is not flagged -> red; skip the imputation -> K60's
        # mean stays 80 -> red; drop worse() -> H1's warn test counts 30 wins -> red
        write_size(self.out)
        write_main(self.out)
        broken = {1: {"runlog": []}, 2: {"runlog": [dict(time_ns=0, label="bench:tc", ran_s=1,
                                                          exit=0, fail_open="timeout")]},
                  3: {"gates": [dict(agent="A1", gate="tc", label="bench:tc", start=0, end=1,
                                     exit=70)]}}
        edit(self.out, "main-heavy.jsonl",
             lambda r: r.update(broken.get(r["block"], {}) if r["arm"] == "K60" else {}))
        s = self.summary()
        self.assertEqual(s["coreslot_failed"]["heavy"], ["1 K60", "2 K60", "3 K60"])
        self.assertEqual(s["tests"]["H1"]["warn"], {"wins": 27, "losses": 3,
                                                    "p": run4.sign_test(27, 30)})
        k60 = [100 * noise(b, "F") if b < 4 else 80 * noise(b, "K60") for b in range(1, 31)]
        self.assertAlmostEqual(s["reported"]["per_arm"]["heavy"]["K60"]["mean_verdict_s"],
                               statistics.fmean(k60), delta=0.1)

    def test_slowdown_and_intervals(self):
        # mutations: `g["end"] - g["start"]` for every arm in per_arm -> K60's slowdown counts
        # its admission wait, 2.0 -> red; drop the BCa interval from ci or by_gate from the
        # false failures -> KeyError -> red; sensitivity over every valid block -> 8 -> red
        write_size(self.out)
        write_main(self.out)
        edit(self.out, "main-heavy.jsonl", lambda r: [l.update(ran_s=r["mean_verdict_s"] / 2)
                                                      for l in r["runlog"]])
        write(self.out, "learn.jsonl", [{"gate": "tc", "form": form, "run": n, "exit": 0,
                                         "wall_s": 40.0} for form in "KF" for n in (1, 2, 3)])
        for b in range(1, 9):
            write(self.out, "sens-spread.jsonl", [sset("sens", "spread", b, a, 50 * noise(b, a))
                                                  for a in run6.SPREAD_ARMS])
        r = self.summary()["reported"]
        k60 = r["per_arm"]["heavy"]["K60"]
        self.assertAlmostEqual(k60["slowdown"]["tc"]["mean"], 1.0, delta=0.03)
        self.assertLess(k60["slowdown"]["tc"]["bca90"][0], k60["slowdown"]["tc"]["bca90"][1])
        self.assertEqual(k60["swapout_bytes"]["bca90"], [100 * 16384] * 2)
        self.assertEqual(k60["false_failures"]["by_gate"]["tc"]["n"], 30)
        self.assertIn("bca90", r["order"]["prev_arm"]["none"])
        self.assertEqual(r["sensitivity"]["spread"]["blocks"], list(range(1, 7)))

    def test_figures_written(self):
        # mutations: drop f7 from figures6.write's figures -> f7-light.svg is missing -> red;
        # draw f4 over `("K60", "F")` for arms_of(heavy) -> f4-pareto-0.svg lacks KH -> red;
        # `len(ARMS)` for `len(order)` in run4.timeline_svg's height -> the last arms' lanes fall
        # below the picture -> red
        write_size(self.out)
        write_main(self.out)
        self.summary()
        with mock.patch("sys.stdout"):
            run6.figures(self.out)
        heavy = set(run6.HEAVY_ARMS)
        want = {"f1-timeline.svg": heavy, "f3-paired.svg": {"K*cf", "S*", "K60", "F"},
                "f5-regret.svg": {"S1", "S2", "S3", "K60"}, "f6-ecdf.svg": heavy,
                "f7-light.svg": {"K60", "F"}}
        want.update({f"f4-pareto-{lv}.svg": heavy for lv in run6.LEVELS_MIB})
        for name, arms in want.items():
            root = ET.parse(os.path.join(self.out, name)).getroot()
            self.assertEqual(root.tag, "{http://www.w3.org/2000/svg}svg", name)
            seen = [e.text or "" for e in root.iter("{http://www.w3.org/2000/svg}text")
                    if float(e.get("y", 0)) <= float(root.get("height"))]
            words = set(re.findall(r"[\w*]+", " ".join(seen)))
            self.assertLessEqual(arms, words, name)


if __name__ == "__main__":
    unittest.main()
