"""Tests of run6.py, its ballast and its run_set arms: python3 -m unittest evidence/test_run6.py"""
import json
import math
import os
import signal
import statistics
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
            "config": arm, "valid": valid, "invalid": [] if valid else ["battery"],
            "level_mib": run6.level_for(phase, src) if mix == "heavy" else 0, "position": pos,
            "prev_arm": order[pos - 1] if pos else None, "mean_verdict_s": verdict,
            "makespan_s": verdict + 10, "samples": samples, "page_bytes": 16384,
            "gates": [{"agent": "A1", "gate": "tc", "start": 0, "end": verdict, "exit": 0}]}


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
        self.assertEqual(s["oracles"]["S"], {"0": 2, "1536": 2, "3072": 2})
        self.assertEqual(s["tests"]["H1"]["status"], "rejected")

    def test_invalid_block_dropped_everywhere(self):
        # mutation: drop `sets[a]["valid"]` from valid_blocks' check -> block 7 counts in the
        # blocks used, and its S3 and K67 would flip S* and K*cf -> red
        write_size(self.out)
        write_main(self.out, heavy=range(1, 32),
                   values=lambda b: {"S3": 10, "K67": 10} if b == 7 else None)
        with open(os.path.join(self.out, "main-heavy.jsonl")) as f:
            recs = [json.loads(l) for l in f]
        with open(os.path.join(self.out, "main-heavy.jsonl"), "w") as f:
            for r in recs:
                r["valid"] = not (r["block"] == 7 and r["arm"] == "K60")
                f.write(json.dumps(r) + "\n")
        want = [b for b in range(1, 32) if b != 7]
        s = self.summary()
        self.assertEqual(s["blocks"]["heavy"], want)
        self.assertEqual(s["oracles"]["S"], {"0": 2, "1536": 2, "3072": 2})
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
        # mutation: keep going after a non-rejection (`going = True` for `going = h["reject"]`)
        # -> H3 is "rejected" -> red
        write_size(self.out)
        write_main(self.out, k60_light=1.10)
        t = self.summary()["tests"]
        self.assertEqual([t[h]["status"] for h in ("H1", "H2", "H3", "H4", "H5")],
                         ["rejected", "not rejected", "exploratory", "exploratory",
                          "exploratory"])
        self.assertTrue(t["H3"]["reject"])
        write_size(self.out, pressure=2)
        os.remove(os.path.join(self.out, "main-light.jsonl"))
        write_main(self.out, heavy=())
        t = self.summary()["tests"]
        self.assertTrue(t["H2"]["reject"])
        self.assertEqual([t[h]["status"] for h in ("H1", "H2", "H3", "H4", "H5")],
                         ["rejected"] + ["exploratory"] * 4)
        write_size(self.out, pressure=1)
        t = self.summary()["tests"]
        self.assertEqual([t[h]["status"] for h in ("H1", "H2", "H3", "H4", "H5")],
                         ["rejected"] * 5)

    def test_oracle_per_level(self):
        # mutation: choose S* over all the blocks for every level -> one N everywhere -> red
        best = {0: "S3", 1536: "S2", 3072: "S1"}
        write_size(self.out)
        write_main(self.out, values=lambda b: {best[run6.level_for("main", b)]: 85})
        self.assertEqual(self.summary()["oracles"]["S"], {"0": 3, "1536": 2, "3072": 1})

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
            with mock.patch.object(run6.stats6, "sign_test", lambda w, lo: p), \
                    mock.patch.object(run6.stats6, "paired_lower", lambda d, b: {"p": p}):
                t = self.summary()["tests"]
            self.assertEqual(t["H4"]["status"], "rejected")
            self.assertEqual(t["H5"]["status"], want, p)

    def test_size_rule(self):
        # mutation: pass sigma-hat, not its upper bound, to blocks_needed -> n_heavy 69, not 108 -> red
        dev = [0.12, -0.10, 0.15, -0.05, 0.02, -0.14, 0.08, -0.02, 0.11, -0.09, 0.0, -0.08]
        for b in range(1, 13):
            write(self.out, "pilot-heavy.jsonl", heavy_block(b, "pilot", {
                "K60": 80 * math.exp(dev[b - 1]), "K67": 100, "S1": 95, "S2": 90, "S3": 95}))
            write(self.out, "pilot-light.jsonl", light_block(b, "pilot", f_warn=int(b < 3)))
        write(self.out, "main-heavy.jsonl", heavy_block(1))  # never read by size
        with mock.patch("sys.stdout"):
            run6.size(self.out)
        with open(os.path.join(self.out, "size.json")) as f:
            got = json.load(f)
        ln = [math.log(round(80 * math.exp(x), 2) / 90) for x in dev]
        self.assertAlmostEqual(got["sigma_hat_heavy"], statistics.stdev(ln), places=9)
        self.assertAlmostEqual(got["sigma_heavy"],
                               stats6.sigma_upper(statistics.stdev(ln), 12), places=9)
        self.assertEqual(got["n_heavy"], 108)
        self.assertEqual(got["n_light"], 12)
        self.assertEqual(got["light_pressure_sets"], 2)
        with open(os.path.join(self.out, "pilot-light.jsonl")) as f:
            recs = [json.loads(l) for l in f]
        with open(os.path.join(self.out, "pilot-light.jsonl"), "w") as f:
            for r in recs:
                r["valid"] = r["block"] != 12
                f.write(json.dumps(r) + "\n")
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            run6.size(self.out)
        self.assertEqual(cm.exception.code, 2)

    def test_incomplete_when_cap_spent(self):
        # mutation: `<=` for `<` in summary's re-run cap check -> with the 3 re-runs spent it
        # still exits 2 -> red
        write_size(self.out)
        write_main(self.out, heavy=range(1, 30))
        write(self.out, "main-heavy.jsonl", heavy_block(30, invalid=("F",)))
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            self.summary()
        self.assertEqual(cm.exception.code, 2)
        for b in (31, 32, 33):
            write(self.out, "main-heavy.jsonl", heavy_block(b, invalid=("F",), as_block=30))
        s = self.summary()
        self.assertIsNone(s["tests"])
        self.assertIn("incomplete", s["incomplete"])
        self.assertEqual(s["reruns"]["heavy"], [31, 32, 33])
        self.assertEqual(s["reported"]["per_arm"]["heavy"]["F"]["mean_verdict_s"],
                         round(statistics.fmean(100 * noise(b, "F") for b in range(1, 30)), 1))
        with open(os.path.join(self.out, "summary.txt")) as f:
            self.assertIn("incomplete", f.read())


if __name__ == "__main__":
    unittest.main()
