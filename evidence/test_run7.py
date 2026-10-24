"""Tests of run7.py: python3 -m unittest evidence/test_run7.py"""
import contextlib
import copy
import hashlib
import json
import math
import os
import subprocess
import statistics
import sys
import tempfile
import threading
import time
import unittest
from datetime import datetime, timezone
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import acceptance  # noqa: E402
import overload  # noqa: E402
import run4  # noqa: E402
import run6  # noqa: E402
import run7  # noqa: E402
import stats7  # noqa: E402


def text(path):
    with open(path) as f:
        return f.read()


def gate(name, heavy=True):
    return {"gate": name, "label": f"bench:{name}", "cwd": "/", "heavy": heavy, "argv": [name]}


def stand(root):
    return {"coreslot": "c", "state_dir": os.path.join(root, "state"), "window_s": 5,
            "agents": {"A1": [gate("tc")], "A2": [gate("lint", False), gate("tc")]},
            "wide_agents": {"W1": [gate("build")], "W2": [gate("tc")]}}


GATES = ("tc", "lint", "build")
PASS1 = [("run", g, form) for g in GATES for form in ["K"] * 6 + ["F"] * 3]
RELEARN = [("run", g, "K") for g in GATES for _ in range(5)]


class Stand(unittest.TestCase):
    def test_stand7_adds_loadgroup(self):
        # mutation: append "--dist loadgroup" to fastapi-ruff too -> the stands differ -> red
        s4 = json.loads(text(os.path.join(HERE, "overload", "stand4.json")))
        s7 = json.loads(text(os.path.join(HERE, "overload", "stand7.json")))
        wide = s7.pop("wide_agents")
        s7.pop("coreslot_sha256")  # the release binary's, filled in before the pilot
        # a label learns one needs entry, so a wide gate that shares one is the heavy gate itself
        heavy = {g["label"]: g for gates in s7["agents"].values() for g in gates}
        for g in (g for gates in wide.values() for g in gates):
            self.assertEqual(g, heavy.get(g["label"], g))
        tests = next(g for g in s7["agents"]["A3"] if g["gate"] == "fastapi-tests")
        self.assertEqual(tests["argv"][-2:], ["--dist", "loadgroup"])
        back = copy.deepcopy(s7)
        next(g for g in back["agents"]["A3"] if g["gate"] == "fastapi-tests")["argv"][-2:] = []
        self.assertEqual(back, s4)


class Learn(unittest.TestCase):
    """A temporary ROOT holding the stand's state directory and an OUT; coreslot, the measured
    run and the cooldown are fakes that log the order of calls."""

    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.out = stand(self.root), os.path.join(self.root, "out")
        self.calls, self.cooldowns = [], []

        def measured(cfg, g, form):
            self.calls.append(("run", g["gate"], form))
            return 0, "", 2_000_000, 4.5, 60.0

        def cs(cfg, *args, **kw):
            self.calls.append(("suggest",) if args[0] == "suggest" else args)
            if args == ("suggest", "--write"):
                n = self.calls.count(("suggest",))
                with open(os.path.join(cfg["state_dir"], "needs.toml"), "w") as f:
                    f.write(f'[needs]\n  [needs."bench:tc"]\n    cpu = "{n}"\n    mem = "8G"\n')
            return subprocess.CompletedProcess(args, 0, "", "")

        def cooldown(args, baseline):
            self.cooldowns.append(args.cooldown)
            self.calls.append(("cool",))
            return {}
        for target, name, fake in ((run4, "ROOT", self.root), (run4, "cs", cs),
                                   (run6, "measured", measured),
                                   (acceptance, "cooldown", cooldown),
                                   (acceptance, "probe", lambda: 1.0)):
            p = mock.patch.object(target, name, fake)
            p.start()
            self.addCleanup(p.stop)

    def learn(self):
        with mock.patch("sys.stdout"):
            run7.learn(self.cfg, self.out)

    def runs(self):
        return [c for c in self.calls if c[0] == "run"]

    def write_learn(self, recs):
        os.makedirs(self.out)
        run4.fresh_state(self.cfg)
        run6.write_learn_state(self.cfg, self.out)
        with open(os.path.join(self.out, "learn.jsonl"), "w") as f:
            for gate_, form, pass_, n in recs:
                f.write(json.dumps({"gate": gate_, "label": f"bench:{gate_}", "form": form,
                                    "pass": pass_, "run": n, "exit": 0}) + "\n")

    def pass1_records(self):
        return [(g, form, "learn", n) for g in GATES for form, runs in (("K", 6), ("F", 3))
                for n in range(1, runs + 1)]

    def test_learn_order_and_relearn(self):
        # mutations: drop the relearn pass -> no runs after the first suggest -> red; learn only
        # cfg["agents"] -> build is never run -> red
        self.learn()
        self.assertEqual([c for c in self.calls if c[0] != "cool"],
                         PASS1 + [("suggest",)] + RELEARN + [("suggest",)])
        self.assertEqual(text(os.path.join(self.out, "needs-pass1.toml")).count('cpu = "1"'), 1)
        self.assertIn('cpu = "2"', text(os.path.join(self.out, "needs.toml")))
        self.assertEqual(text(os.path.join(self.cfg["state_dir"], "config.toml")),
                         '[resources]\ncpu = "140%"\nmem = "60%"\n')
        self.assertEqual(text(os.path.join(self.out, "needs.sha256")),
                         f"{run4.sha256(os.path.join(self.out, 'needs.toml'))}  needs.toml\n"
                         f"{run4.sha256(os.path.join(self.out, 'learn.jsonl'))}  learn.jsonl\n")
        recs = run4.read_jsonl(os.path.join(self.out, "learn.jsonl"))
        self.assertEqual(len(recs), len(PASS1) + len(RELEARN))
        self.assertEqual([r["pass"] for r in recs],
                         ["learn"] * len(PASS1) + ["relearn"] * len(RELEARN))
        self.assertEqual({k for k in recs[0]}, {"started", "gate", "label", "form", "pass", "run",
                                                "exit", "wall_s", "peak_kib", "cores",
                                                "failures", "ran_s"})
        self.assertEqual([r["run"] for r in recs if r["pass"] == "relearn"], [1, 2, 3, 4, 5] * 3)

    def test_learn_resumes_in_pass1(self):
        # mutation: call fresh_state whenever learn starts -> the state is reset on resume -> red
        self.write_learn([("tc", "K", "learn", n) for n in range(1, 7)]
                         + [("tc", "F", "learn", n) for n in (1, 2)])
        fresh = []
        with mock.patch.object(run4, "fresh_state", lambda cfg: fresh.append(cfg)):
            self.learn()
        self.assertEqual(fresh, [])
        self.assertEqual(self.runs(), PASS1[8:] + RELEARN)
        self.assertEqual(self.calls.count(("suggest",)), 2)

    def test_learn_resumes_in_relearn(self):
        # mutations: run suggest #1 whenever pass 1 is complete -> two suggests -> red; key the
        # resume on (gate, form, run) -> pass 1's K runs count as relearn's -> no runs -> red
        self.write_learn(self.pass1_records() + [("tc", "K", "relearn", n) for n in (1, 2)])
        self.learn()
        self.assertEqual(self.runs(), RELEARN[2:])
        self.assertEqual(self.calls.count(("suggest",)), 1)
        self.assertFalse(os.path.exists(os.path.join(self.out, "needs-pass1.toml")))
        recs = run4.read_jsonl(os.path.join(self.out, "learn.jsonl"))
        self.assertEqual([(r["gate"], r["run"]) for r in recs if r["pass"] == "relearn"],
                         [("tc", n) for n in range(1, 6)] + [(g, n) for g in ("lint", "build")
                                                            for n in range(1, 6)])

    def test_learn_resumes_after_suggest2(self):
        # mutations: run suggest #1 once relearn is complete -> needs-pass1.toml written -> red;
        # let a sealed learn run -> suggest and seal again -> red
        self.write_learn(self.pass1_records()
                         + [(g, "K", "relearn", n) for g in GATES for n in range(1, 6)])
        self.learn()
        self.assertEqual((self.runs(), self.cooldowns, self.calls), ([], [], [("suggest",)]))
        self.assertFalse(os.path.exists(os.path.join(self.out, "needs-pass1.toml")))
        sealed = text(os.path.join(self.out, "needs.sha256"))
        self.assertIn("needs.toml", sealed)
        with open(os.path.join(self.out, "learn.jsonl"), "a") as f:  # a stray run, after the seal
            f.write(json.dumps({"gate": "tc", "form": "K", "pass": "relearn", "run": 6}) + "\n")
        self.learn()
        self.assertEqual(self.calls, [("suggest",)])
        self.assertEqual(text(os.path.join(self.out, "needs.sha256")), sealed)

    def test_learn_refuses_after_pilot(self):
        # mutation: drop the sens pattern from the refusal -> learn runs -> red
        for name in ("pilot-heavy.jsonl", "main-light.jsonl", "sens-spread.jsonl"):
            with self.subTest(name=name):
                os.makedirs(self.out, exist_ok=True)
                open(os.path.join(self.out, name), "w").close()
                with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
                    self.learn()
                self.assertEqual(cm.exception.code, 2)
                self.assertEqual(self.calls, [])
                os.remove(os.path.join(self.out, name))

    def test_learn_cools_down_between_runs(self):
        # mutations: cool down once per gate -> fewer cooldowns -> red; take pacing's cooldown
        # without the minimum -> 5 s -> red
        self.cfg["cooldown_s"] = 5
        self.learn()
        self.assertEqual(len(self.cooldowns), len(PASS1) + len(RELEARN))
        self.assertEqual(set(self.cooldowns), {run7.LEARN_COOLDOWN_S})
        runs = [i for i, c in enumerate(self.calls) if c[0] == "run"]
        self.assertTrue(all(self.calls[i - 1] == ("cool",) for i in runs))


class FakeTime:
    """run7's clock: it advances only when slept on."""

    def __init__(self):
        self.now, self.slept = 1000.0, []

    def monotonic(self):
        return self.now

    def time(self):
        return self.now

    def sleep(self, s):
        self.slept.append(s)
        self.now += s


class Level(unittest.TestCase):
    """level() with the clock, the headroom and the ballast faked."""

    def setUp(self):
        self.clock, self.headroom, self.ballasts, self.rss = FakeTime(), [9000], [], None
        test = self

        class Ballast:
            def __init__(self, mib):
                self.mib, self.released = mib, False
                test.ballasts.append(self)

            def __enter__(self):
                return self

            def __exit__(self, *exc):
                self.released = True

            def rss_mib(self):
                return self.mib if test.rss is None else test.rss

        def headroom():
            return self.headroom.pop(0) if len(self.headroom) > 1 else self.headroom[0]
        for target, name, fake in ((run7, "time", self.clock), (run7.ballast, "Ballast", Ballast),
                                   (run7.overload, "headroom_mib", headroom)):
            p = mock.patch.object(target, name, fake)
            p.start()
            self.addCleanup(p.stop)

    def level(self, name):
        with run7.level(name) as rec:
            return rec

    def test_quiet_waits_then_flags(self):
        # mutation: drop the quiet_short flag -> no flag after the wait -> red
        with mock.patch.object(run7, "QUIET_WAIT_S", 2):
            rec = self.level("quiet")
        self.assertEqual(rec, {"level": "quiet", "headroom_before_mib": 9000, "ballast_mib": 0,
                               "ballast_rss_mib": None, "flags": ["quiet_short"]})
        self.assertEqual(self.clock.slept, [1.0, 1.0])
        self.assertEqual(self.ballasts, [])

    def test_quiet_passes(self):
        # mutation: compare with LOADED_HEADROOM_MIB -> 9000 passes at once -> red
        self.headroom = [9000, 11000]
        rec = self.level("quiet")
        self.assertEqual((rec["flags"], self.clock.slept), ([], [1.0]))
        self.headroom = [run7.QUIET_HEADROOM_MIB]
        self.assertEqual(self.level("quiet")["flags"], [])
        self.assertEqual(self.clock.slept, [1.0])

    def test_loaded_sizes_clamps_and_releases(self):
        # mutations: drop the cap -> 12000 MiB at 20000 -> red; drop the floor at 0 -> -1000 -> red
        for headroom, size in ((12000, 4000), (20000, run7.BALLAST_CAP_MIB), (7000, 0)):
            self.headroom = [headroom]
            rec = self.level("loaded")
            self.assertEqual((rec["ballast_mib"], self.ballasts[-1].mib), (size, size))
            self.assertEqual((rec["headroom_before_mib"], rec["flags"]), (headroom, []))
            self.assertTrue(self.ballasts[-1].released)
        self.headroom = [12000]
        with self.assertRaises(RuntimeError):
            with run7.level("loaded"):
                raise RuntimeError("the set failed")
        self.assertTrue(self.ballasts[-1].released)
        # mutation: skip the residency wait -> a ballast short of its size is not flagged -> red
        self.rss, slept = 100, len(self.clock.slept)
        rec = self.level("loaded")
        self.assertEqual((rec["ballast_rss_mib"], rec["flags"]), (100, ["ballast_short"]))
        self.assertEqual(sum(self.clock.slept[slept:]), run7.RESIDENCY_WAIT_S)

    def test_level_off_two_sided(self):
        # mutation: one-sided loaded (only above the target) -> 7400 not off -> red
        for headroom, level, off in ((8600, "loaded", True), (7400, "loaded", True),
                                     (8300, "loaded", False), (10000, "quiet", True),
                                     (13000, "quiet", False)):
            self.assertEqual(run7.level_off(headroom, level), off, (headroom, level))


class Processes(unittest.TestCase):
    def test_parse_ps_time(self):
        # the first two as this Mac's `ps -A -o time=` printed them; minutes run past 59, and
        # H:MM:SS is ps's other form
        for line, secs in (("   0:42.12", 42.12), ("  27:16.95", 1636.95), ("8:03.69", 483.69),
                           ("125:00.00", 7500.0), ("1:02:03", 3723.0)):
            self.assertAlmostEqual(run7.parse_ps_time(line), secs, places=6)

    def test_background_cpu_new_and_reused(self):
        # mutations: skip pids absent before -> 2 cores -> red; no clamp -> pid 11 counts -97 s
        # -> red; ignore own_pids -> pid 12 counts 8 s -> red
        before = {10: (1, 5.0), 11: (1, 100.0), 12: (1, 1.0)}
        after = {10: (1, 7.0), 11: (1, 3.0), 12: (1, 9.0), 13: (1, 4.0)}
        self.assertEqual(run7.background_cpu_cores(before, after, {12}, 2.0), 3.0)


class RunSet7(unittest.TestCase):
    """run_set7 with the machine, the level, the sampler and the gates faked; P's sem calls go to
    a fake Popen that creates sem's semaphore files and runs until done or killed."""

    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.path = stand(self.root), os.path.join(self.root, "sets.jsonl")
        self.cfg["agents"] = {"A1": [dict(gate("tc"), argv=["tc", "-n", "{n}"], free_n="3")],
                              "A2": [gate("lint", False), gate("tc")]}
        self.events, self.popens, self.killed = [], [], []
        self.gate_s, self.gate_raises, self.headroom = 0, False, 11000
        test = self

        class Popen:
            def __init__(self, argv, **kw):
                self.argv, self.env, self.returncode = argv, kw["env"], None
                self.pid, self.done = 9000 + len(test.popens), threading.Event()
                test.popens.append(self)
                sems = os.path.join(self.env["PARALLEL_HOME"], "semaphores")
                os.makedirs(os.path.join(sems, f"id-{argv[3]}"), exist_ok=True)
                open(os.path.join(sems, f"id-{argv[3]}.lock"), "w").close()

            def communicate(self):
                test.events.append(("gate", self.argv[-1]))
                if test.gate_raises:
                    raise RuntimeError("sem failed")
                self.returncode = -15 if self.done.wait(test.gate_s) else 0
                return "", None

        class Sampler:
            def __init__(self):
                self.samples, self.stop = [], mock.Mock()

            def start(self):
                pass

            def join(self):
                pass

        @contextlib.contextmanager
        def level(name):
            self.events.append(("level", name))
            yield {"level": name, "headroom_before_mib": 9000, "ballast_mib": 0,
                   "ballast_rss_mib": None, "flags": []}

        def kill_tree(pid):
            self.killed.append(pid)
            next(p for p in self.popens if p.pid == pid).done.set()

        def run_one(cfg, g, arm):
            self.events.append(("gate", g["gate"]))
            return 0, ""

        def cooldown(*a):
            self.events.append(("cooldown",))
            return {"cooldown_gave_up": False}

        def headroom():
            self.events.append(("t0",))
            return self.headroom
        for target, name, fake in (
                (run4, "ROOT", self.root), (run4, "run_one", run_one),
                (run4, "on_ac", lambda: True),
                (run4, "background", lambda: (16384, {"load1": 0.0})),
                (run4, "budget_empty", lambda cfg: True),
                (acceptance, "cooldown", cooldown), (acceptance, "runlog_lines", lambda cfg: []),
                (overload, "Sampler", Sampler), (overload, "headroom_mib", headroom),
                (run7.subprocess, "Popen", Popen), (run7, "level", level),
                (run7, "kill_tree", kill_tree), (run7, "ps_snapshot", lambda: {})):
            p = mock.patch.object(target, name, fake)
            p.start()
            self.addCleanup(p.stop)

    def run_set(self, arm, meta=None, path=None, **kw):
        with mock.patch("sys.stdout"):
            return run7.run_set7(self.cfg, path or self.path, arm=arm,
                                 offsets={"A1": 0, "A2": 0}, meta=meta or {}, baseline=1.0,
                                 level_name="quiet", **kw)

    def semaphores(self):
        return os.listdir(os.path.join(self.root, "parallel-home", "semaphores"))

    def test_p_argv_env_and_cleanup(self):
        # mutations: run P's light gates through sem too -> three sem calls -> red; drop the
        # semaphore cleanup -> its directory and lock stay -> red
        rec = self.run_set("P")
        self.assertTrue(rec["valid"], rec["invalid"])
        prefix = ["sem", "--will-cite", "--id", rec["set_id"], "--memsuspend", "9G", "-j", "4",
                  "--fg"]
        self.assertEqual(sorted(p.argv for p in self.popens),
                         [prefix + ["tc"], prefix + ["tc", "-n", "3"]])
        for p in self.popens:
            self.assertEqual(p.env["PARALLEL_HOME"], os.path.join(self.root, "parallel-home"))
        self.assertIn(("gate", "lint"), self.events)
        self.assertEqual(self.semaphores(), [])
        self.gate_raises = True
        rec = self.run_set("P")
        self.assertFalse(rec["valid"])
        self.assertTrue(rec["invalid"][0].startswith("runner_error"), rec["invalid"])
        self.assertEqual(self.semaphores(), [])
        # mutation: clean up after the record instead of in a finally -> a failed write leaves it
        self.gate_raises = False
        with self.assertRaises(OSError):
            self.run_set("P", path=self.root)
        self.assertEqual(self.semaphores(), [])

    def test_set_timeout_invalidates_and_kills(self):
        # mutation: on expiry only stop_live -> sem's calls are never killed -> the set waits
        # out its gates and is valid -> red
        self.gate_s = 30
        t = time.monotonic()
        with mock.patch.object(run7, "SET_TIMEOUT_S", 1):
            rec = self.run_set("P")
        self.assertLess(time.monotonic() - t, 10)
        self.assertEqual((rec["valid"], rec["invalid"]), (False, ["set_timeout"]))
        self.assertEqual(sorted(self.killed), sorted(p.pid for p in self.popens))
        self.assertEqual(len(self.killed), 2)

    def test_set_timeout_env_only_when_dry(self):
        # mutation: honour RUN7_SET_TIMEOUT_S on any stand -> a real run can be cut short -> red
        with mock.patch.dict(os.environ, {"RUN7_SET_TIMEOUT_S": "1"}):
            self.assertEqual(run7.set_timeout({}), run7.SET_TIMEOUT_S)
            self.assertEqual(run7.set_timeout({"dry": True}), 1.0)
        self.assertEqual(run7.set_timeout({"dry": True}), run7.SET_TIMEOUT_S)

    def test_order_cooldown_level_t0(self):
        # mutation: read the headroom at t0 before the level -> t0 precedes level -> red
        self.headroom = 9000
        rec = self.run_set("F")
        self.assertEqual(self.events[:3], [("cooldown",), ("level", "quiet"), ("t0",)])
        self.assertEqual(self.events[3][0], "gate")
        self.assertEqual((rec["headroom_t0_mib"], rec["level"]), (9000, "quiet"))
        self.assertEqual(rec["flags"], ["level_off"])

    def test_provenance_meta(self):
        # mutation: tree_dirty from rev-parse's exit -> a modified file reads clean -> red
        binary = os.path.join(self.root, "coreslot")
        with open(binary, "wb") as f:
            f.write(b"binary")
        self.cfg["coreslot"] = binary
        out = os.path.join(self.root, "out")
        os.makedirs(out)
        with open(os.path.join(out, "pilot_check.json"), "w") as f:
            f.write('{"go": true}')
        status, calls = [" M evidence/run7.py\n"], []

        def output(*argv):
            calls.append(argv)
            if "rev-parse" in argv:
                return "abc123\n"
            return status[0] if "status" in argv else "GNU parallel 20260922\nCopyright\n"
        with mock.patch.object(run7, "output", output):
            prov = run7.provenance(self.cfg, out, "pilot")
            status[0] = ""
            clean = run7.provenance(self.cfg, out, "main")
        rec = self.run_set("F", meta=prov)
        self.assertEqual({k: rec[k] for k in prov},
                         {"binary_sha256": hashlib.sha256(b"binary").hexdigest(),
                          "evidence_head": "abc123", "tree_dirty": True,
                          "parallel_version": "GNU parallel 20260922"})
        self.assertFalse(clean["tree_dirty"])
        self.assertEqual(clean["pilot_check_sha256"],
                         hashlib.sha256(b'{"go": true}').hexdigest())
        self.assertIn(("git", "-C", run7.REPO, "status", "--porcelain", "--", "evidence"), calls)

    def test_tree_check_ignores_own_records(self):
        # mutation: drop the exclusion of OUT -> its records read dirty -> red
        self.cfg["coreslot"] = __file__
        out = os.path.join(run7.REPO, "evidence", "overload", "run7")
        modified, specs = [""], []

        def output(*argv):  # git status as it reads with and without OUT excluded
            if "status" not in argv:
                return "x\n"
            specs.append(argv[argv.index("--") + 1:])
            own = "" if ":(exclude)evidence/overload/run7" in argv else "?? " + out + "/\n"
            return own + modified[0]
        with mock.patch.object(run7, "output", output):
            self.assertFalse(run7.provenance(self.cfg, out, "pilot")["tree_dirty"])
            run7.provenance(self.cfg, self.root, "pilot")
            self.assertEqual(specs[-1], ("evidence",))  # an OUT outside the repository
            modified[0] = " M evidence/run7.py\n"
            self.assertTrue(run7.provenance(self.cfg, out, "pilot")["tree_dirty"])


class Design(unittest.TestCase):
    def test_level_for_pairs(self):
        # mutations: seed the level by block -> a pair can repeat a level -> red; no shuffle ->
        # every pair starts quiet -> red
        firsts = set()
        for phase in ("pilot", "main"):
            for pair in range(12):
                levels = [run7.level_for(phase, 2 * pair + i) for i in (1, 2)]
                self.assertEqual(sorted(levels), sorted(run7.LEVELS), (phase, pair))
                firsts.add(levels[0])
        self.assertEqual(firsts, set(run7.LEVELS))
        for b in range(1, 7):
            self.assertEqual(sorted(run7.order_for("main", "heavy", b, run7.HEAVY_ARMS)),
                             sorted(run7.HEAVY_ARMS))
        self.assertNotEqual(run7.order_for("main", "heavy", 1, run7.HEAVY_ARMS),
                            run7.order_for("main", "heavy", 2, run7.HEAVY_ARMS))


class Verdict7(unittest.TestCase):
    """verdict with run_set7, the session's probes, git and parallel faked."""

    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.out = stand(self.root), os.path.join(self.root, "out")
        self.cfg["agents"].update({"A3": [gate("ruff", False), gate("tests")],
                                   "A4": [gate("orm")]})
        self.cfg["coreslot"] = os.path.join(self.root, "coreslot")
        with open(self.cfg["coreslot"], "wb") as f:
            f.write(b"binary")
        self.cfg["coreslot_sha256"] = hashlib.sha256(b"binary").hexdigest()
        os.makedirs(self.out)
        for name in ("needs.toml", "learn.jsonl"):
            with open(os.path.join(self.out, name), "w") as f:
                f.write(f"# {name}\n")
        run7.seal(self.out)
        self.calls, self.status, self.sem = [], "", "/usr/local/bin/sem"

        def run_set7(cfg, path, **kw):
            self.calls.append({"cfg": cfg, "path": path, **kw})

        def output(*argv):
            if "rev-parse" in argv:
                return "abc123\n"
            return self.status if "status" in argv else "GNU parallel 20260922\n"
        for target, name, fake in (
                (run4, "ROOT", self.root), (run7, "run_set7", run_set7),
                (run7, "output", output), (run7.shutil, "which", lambda cmd: self.sem),
                (run7, "static_sizes7", lambda cfg: {"cpu": 14, "mem": 76}),
                (run6, "running_apps", lambda: ["Terminal"]),
                (run6, "coreslot_version", lambda cfg: "coreslot 0.0.0-test"),
                (acceptance, "probe", lambda: 1.0)):
            p = mock.patch.object(target, name, fake)
            p.start()
            self.addCleanup(p.stop)

    def pilot_check(self, **kw):
        with open(os.path.join(self.out, "pilot_check.json"), "w") as f:
            json.dump(kw, f)

    def record(self, phase, mix, block, arms, valid=True, as_block=None):
        with open(os.path.join(self.out, f"{phase}-{mix}.jsonl"), "a") as f:
            for arm in arms:
                f.write(json.dumps({"block": block, "arm": arm, "valid": valid,
                                    "as_block": as_block}) + "\n")

    def verdict(self, *args, **kw):
        with mock.patch("sys.stdout"):
            run7.verdict(self.cfg, self.out, *args, **kw)

    def refused(self, *args, **kw):
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            self.verdict(*args, **kw)
        return cm.exception.code

    def test_verdict_resumes_and_skips(self):
        # mutation: level_for(phase, first) for level_for(phase, src) -> block 6 runs at block
        # 5's level, which differs inside a pair -> red
        self.pilot_check(go=True, p_feasible=True)
        arms = run7.HEAVY_ARMS
        self.record("main", "heavy", 4, arms)
        o5, o6 = (run7.order_for("main", "heavy", b, arms) for b in (5, 6))
        self.record("main", "heavy", 5, o5[:2])
        self.verdict("main", "heavy", 4, 6)
        self.assertEqual([(c["meta"]["block"], c["arm"]) for c in self.calls],
                         [(5, a) for a in o5[2:]] + [(6, a) for a in o6])
        self.assertEqual([c["level_name"] for c in self.calls],
                         [run7.level_for("main", 5)] * 2 + [run7.level_for("main", 6)] * 4)
        self.assertNotEqual(run7.level_for("main", 5), run7.level_for("main", 6))
        c = self.calls[0]
        self.assertEqual(c["path"], os.path.join(self.out, "main-heavy.jsonl"))
        self.assertEqual(c["offsets"], run4.offsets_for(self.cfg, "run7-main-heavy-5"))
        m = c["meta"]
        self.assertEqual((m["level_name"], m["position"], m["prev_arm"], m["mix_agents"]),
                         (c["level_name"], 2, o5[1], ["A1", "A2", "A3", "A4"]))
        self.assertEqual((m["coreslot_version"], m["evidence_head"], m["tree_dirty"]),
                         ("coreslot 0.0.0-test", "abc123", False))
        self.assertEqual(m["pilot_check_sha256"],
                         run4.sha256(os.path.join(self.out, "pilot_check.json")))
        self.assertNotIn("Terminal", json.dumps(m))
        k = next(c for c in self.calls if c["arm"] == "K60")
        self.assertEqual((k["needs"], k["meta"]["static"]),
                         (os.path.join(self.out, "needs.toml"), {"cpu": 14, "mem": 76}))
        self.assertEqual(next(c for c in self.calls if c["arm"] == "S2")["slots"], 2)
        # mutation: drop the skip of recorded (block, arm) -> block 4 runs again -> red
        for c in self.calls:
            self.record("main", "heavy", c["meta"]["block"], [c["arm"]])
        n = len(self.calls)
        self.verdict("main", "heavy", 4, 6)
        self.assertEqual(len(self.calls), n)
        # mutation: re-run a block whose only invalid set is P's -> it runs -> red (P1 never reads
        # the re-run, and it spends the cap)
        self.record("main", "heavy", 4, ["P"], valid=False)
        self.assertEqual(self.refused("main", "heavy", 13, 13, as_block=4), 2)
        self.assertEqual(len(self.calls), n)
        # run 6's --as-block: block 4's order, level and offsets under block 13
        self.record("main", "heavy", 4, ["S1"], valid=False)
        self.verdict("main", "heavy", 13, 13, as_block=4)
        rerun = self.calls[n:]
        self.assertEqual([c["arm"] for c in rerun], run7.order_for("main", "heavy", 4, arms))
        for c in rerun:
            self.assertEqual((c["meta"]["block"], c["meta"]["as_block"], c["level_name"]),
                             (13, 4, run7.level_for("main", 4)))
            self.assertEqual(c["offsets"], run4.offsets_for(self.cfg, "run7-main-heavy-4"))
        self.assertEqual(self.refused("main", "heavy", 14, 14, as_block=5), 2)

    def test_verdict_refusals(self):
        # mutations: refuse a dirty tree only for main -> the pilot runs -> red; check go by the
        # file's presence -> go false runs -> red; refuse a missing sem for every mix -> the
        # light mix is refused -> red
        self.sem = None
        self.assertEqual(self.refused("pilot", "heavy", 1, 1), 2)
        self.verdict("pilot", "light", 1, 1)
        self.assertEqual(sorted(c["arm"] for c in self.calls), sorted(run7.LIGHT_ARMS))
        self.sem, self.status = "/usr/local/bin/sem", " M evidence/run7.py\n"
        self.assertEqual(self.refused("pilot", "heavy", 1, 1), 2)
        self.status = ""
        self.assertEqual(self.refused("main", "heavy", 1, 1), 2)
        self.pilot_check(go=False, p_feasible=True)
        self.assertEqual(self.refused("main", "heavy", 1, 1), 2)
        self.assertEqual(self.refused("sens", "heavy", 1, 1), 2)
        self.assertEqual(len(self.calls), len(run7.LIGHT_ARMS))
        # mutation: refuse a missing sem before the nothing-to-do return -> exit 2 -> red
        self.record("pilot", "heavy", 1, run7.HEAVY_ARMS)
        self.sem = None
        self.verdict("pilot", "heavy", 1, 1)
        self.sem = "/usr/local/bin/sem"
        self.pilot_check(go=True, p_feasible=True)
        with open(os.path.join(self.out, "needs.toml"), "a") as f:
            f.write("# changed\n")
        self.assertEqual(self.refused("main", "heavy", 1, 1), 2)
        self.assertEqual(len(self.calls), len(run7.LIGHT_ARMS))

    def test_verdict_pins_the_binary(self):
        # mutations: skip the binary check for main -> a rebuilt binary runs main -> red; check
        # it on a dry stand too -> the dry run is refused -> red
        self.pilot_check(go=True, p_feasible=True)
        for phase in ("pilot", "main"):
            for pinned in (None, hashlib.sha256(b"rebuilt").hexdigest()):
                self.cfg["coreslot_sha256"] = pinned
                self.assertEqual(self.refused(phase, "light", 1, 1), 2, (phase, pinned))
        self.cfg.pop("coreslot_sha256")
        self.assertEqual(self.refused("pilot", "light", 1, 1), 2)
        self.assertEqual(self.calls, [])
        # mutation: refuse a dirty tree on a dry stand -> red; drop tree_dirty there -> red
        self.cfg["dry"], self.status = True, " M evidence/run7.py\n"
        self.verdict("pilot", "light", 1, 1)
        self.assertEqual([c["meta"]["tree_dirty"] for c in self.calls], [True, True])

    def test_verdict_drops_p_when_infeasible(self):
        # mutation: keep P whatever pilot_check.json says -> four arms, and no sem -> red
        self.pilot_check(p_feasible=False, go=True)
        self.sem = None
        self.verdict("main", "heavy", 1, 1)
        self.assertEqual([c["arm"] for c in self.calls],
                         run7.order_for("main", "heavy", 1, run7.P1_ARMS))

    def test_mix_agents(self):
        # mutations: run the wide mix on the stand's agents -> A1-A4 -> red; let it run with no
        # wide agents -> empty sets recorded -> red
        self.verdict("pilot", "wide", 1, 1)
        self.verdict("pilot", "light", 1, 1)
        self.verdict("sens", "spread", 1, 1)
        got = [(c["meta"]["mix"], c["arm"], list(c["cfg"]["agents"]), c["level_name"])
               for c in self.calls]
        self.assertEqual(sorted(got), sorted(
            [("wide", a, ["W1", "W2"], "quiet") for a in run7.WIDE_ARMS]
            + [("light", a, ["A3", "A4"], "quiet") for a in run7.LIGHT_ARMS]
            + [("spread", a, ["A1", "A2", "A3", "A4"], "quiet") for a in run7.SPREAD_ARMS]))
        self.assertEqual(self.calls[0]["meta"]["mix_agents"], ["W1", "W2"])
        self.assertEqual({c["cfg"]["window_s"] for c in self.calls if c["meta"]["mix"] == "spread"},
                         {run7.SPREAD_WINDOW_S})
        self.cfg["wide_agents"] = {}
        self.assertEqual(self.refused("pilot", "wide", 2, 2), 2)


class Static(unittest.TestCase):
    def test_static_sizes7_writes_one_share(self):
        # mutation: write run 6's two mem shares -> two writes -> red
        root, writes = tempfile.mkdtemp(), []

        def cs(cfg, *args, **kw):
            status = {"resources": {"cpu": {"size": 14}, "mem": {"size": 76}}}
            return subprocess.CompletedProcess(args, 0, json.dumps(status), "")
        with mock.patch.object(run4, "ROOT", root), mock.patch.object(run4, "cs", cs), \
                mock.patch.object(run4, "write_config",
                                  lambda cfg, cpu, mem: writes.append((cpu, mem))):
            self.assertEqual(run7.static_sizes7(stand(root)), {"cpu": 14, "mem": 76})
        self.assertEqual(writes, [("140%", "60%")])


# --- analysis ---------------------------------------------------------------------------------

HEAVY7 = {"quiet": {"K60": 100, "S1": 125, "S2": 102, "P": 100},
          "loaded": {"K60": 100, "S1": 140, "S2": 125, "P": 101}}
SWAP7 = {"quiet": {"K60": 0, "S1": 0, "S2": 0, "P": 0},
         "loaded": {"K60": 10, "S1": 500, "S2": 400, "P": 300}}


def noise(block, arm):
    """A deterministic spread of -2 % to +2 % so that no log ratio is constant."""
    w = sum((i + 1) * ord(c) for i, c in enumerate(arm))  # distinct mod 5 from K60's
    return 1 + ((block * 7 + w * 3) % 5 - 2) / 100


def fake_set(arm, block, level, mean, swapouts=0, swapins=0, warn=0, invalid=None,
             phase="pilot", mix="heavy", as_block=None):
    """A minimal run-7 set record: `warn` samples at pressure 2, swapouts and swapins rising by
    the pages given; a K60 set carries its gate's run-log line and the static sizes."""
    order = run7.order_for(phase, mix, as_block or block, run7.MIXES[mix])
    pos, t0 = order.index(arm), 1_800_000_000 + 600 * block
    samples = [{"t": i, "pressure": 2 if i < warn else 1,
                "swapouts": swapouts if i == warn + 1 else 0,
                "swapins": swapins if i == warn + 1 else 0, "pageins": 0}
               for i in range(warn + 2)]
    return {"phase": phase, "mix": mix, "block": block, "as_block": as_block, "arm": arm,
            "level": level, "level_name": level, "valid": not invalid,
            "invalid": list(invalid or []), "flags": [], "order": " ".join(order),
            "position": pos, "prev_arm": order[pos - 1] if pos else None,
            "started": datetime.fromtimestamp(t0, timezone.utc).isoformat(),
            "mean_verdict_s": mean, "verdict_s": {"A1": mean}, "makespan_s": mean + 10,
            "samples": samples, "page_bytes": 16384,
            "gates": [{"agent": "A1", "gate": "tc", "label": "bench:tc", "start": 0, "end": mean,
                       "exit": 0}],
            "runlog": [{"label": "bench:tc", "time_ns": t0 * 10**9, "waited_s": 0.0,
                        "ran_s": mean, "grant": {"cpu": 4, "mem": 40}, "exit": 0,
                        "fail_open": ""}] if arm == "K60" else [],
            "static": {"cpu": 14, "mem": 76} if arm == "K60" else None}


def heavy_block7(block, phase="pilot", times=None, swap=None, invalid=None, as_block=None):
    """A heavy block's four sets at its level: HEAVY7 with noise, scaled by `times`, SWAP7 with
    `swap` overriding an arm's swapouts, `invalid` an arm's reasons."""
    level = run7.level_for(phase, as_block or block)
    t, sw = times or {}, dict(SWAP7[level], **(swap or {}))
    return [fake_set(a, block, level, round(HEAVY7[level][a] * t.get(a, 1) * noise(block, a), 2),
                     sw[a], 0, 0, (invalid or {}).get(a), phase, "heavy", as_block)
            for a in run7.HEAVY_ARMS]


def light_block7(block, phase="pilot", k60=1.0, f_warn=0):
    return [fake_set("K60", block, "quiet", round(50 * k60 * noise(block, "K60"), 2),
                     phase=phase, mix="light"),
            fake_set("F", block, "quiet", round(50 * noise(block, "F"), 2), warn=f_warn,
                     phase=phase, mix="light")]


def wide_block7(block, phase="pilot"):
    return [fake_set(a, block, "quiet", round(v * noise(block, a), 2), phase=phase, mix="wide")
            for a, v in (("K60", 100), ("S2", 120), ("F", 100))]


def append(out, name, recs):
    with open(os.path.join(out, name), "a") as f:
        for r in recs:
            f.write(json.dumps(r) + "\n")


def pilot_levels():
    """level -> the pilot's heavy blocks 1-12 at that level."""
    return {lv: [b for b in range(1, 13) if run7.level_for("pilot", b) == lv]
            for lv in run7.LEVELS}


class PilotCheck(unittest.TestCase):
    def setUp(self):
        self.out = tempfile.mkdtemp()

    def write_pilot(self, heavy=range(1, 13), kw=lambda b: {}, wide=range(1, 7)):
        for b in heavy:
            append(self.out, "pilot-heavy.jsonl", heavy_block7(b, **kw(b)))
        for b in range(1, 13):
            append(self.out, "pilot-light.jsonl", light_block7(b, f_warn=int(b == 3)))
        for b in wide:
            append(self.out, "pilot-wide.jsonl", wide_block7(b))

    def check(self):
        with mock.patch("sys.stdout"):
            run7.pilot_check(self.out)
        with open(os.path.join(self.out, "pilot_check.json")) as f:
            return json.load(f)

    def ln(self, level, num):
        """ln(num/K60) over the pilot's heavy blocks at level, from the written records."""
        sets = {}
        for r in run4.read_jsonl(os.path.join(self.out, "pilot-heavy.jsonl")):
            sets.setdefault(r["block"], {})[r["arm"]] = r["mean_verdict_s"]
        return [math.log(sets[b][num] / sets[b]["K60"]) for b in pilot_levels()[level]]

    def test_pilot_check_go(self):
        # mutations: T2's discordance over all 12 pilot blocks, not the 6 loaded -> m 30 -> red;
        # T1 over every level's blocks -> its mean moves -> red
        tie, loss = pilot_levels()["loaded"][:2]
        self.write_pilot(kw=lambda b: {"swap": {"S2": 10 if b == tie else 5 if b == loss
                                                else 400}})
        c = self.check()
        for name, level, num in (("T1", "loaded", "S2"), ("T3", "quiet", "S1")):
            d = self.ln(level, num)
            mean, sd = statistics.fmean(d), statistics.stdev(d)
            self.assertAlmostEqual(c[name]["mean"], mean, places=12)
            self.assertAlmostEqual(c[name]["sd"], sd, places=12)
            self.assertEqual(c[name]["cp_at_n"], stats7.t_power(72, mean, sd, 0.05))
        self.assertEqual((c["T2"]["wins"], c["T2"]["losses"]), (4, 1))
        self.assertEqual(c["T2"]["cp_at_n"], stats7.sign_power(60, 0.8, 0.05))
        self.assertEqual((c["p_timeouts"], c["p_feasible"], c["light_pressure_sets"]),
                         (0, True, 1))
        self.assertEqual((c["futile"], c["go"]), ([], True))

    def test_pilot_check_futile_t3(self):
        # mutation: judge T3 on the loaded blocks -> S1 is slower there, T3 not futile -> red
        quiet = pilot_levels()["quiet"]
        self.write_pilot(kw=lambda b: {"times": {"S1": 0.75}} if b in quiet else {})
        c = self.check()
        self.assertLessEqual(c["T3"]["mean"], 0)
        self.assertEqual((c["futile"], c["go"]), (["T3"], False))

    def test_p_invalid_does_not_drop_p1_block(self):
        # mutation: valid_blocks over HEAVY_ARMS -> no heavy block is valid -> refused -> red
        self.write_pilot(kw=lambda b: {"invalid": {"P": ["battery"]}})
        c = self.check()
        self.assertEqual(c["blocks"]["heavy"], list(range(1, 13)))
        self.assertAlmostEqual(c["T1"]["mean"], statistics.fmean(self.ln("loaded", "S2")))
        self.assertAlmostEqual(c["T3"]["mean"], statistics.fmean(self.ln("quiet", "S1")))
        self.assertEqual((c["p_timeouts"], c["p_feasible"], c["go"]), (0, True, True))

    def test_p_infeasible(self):
        # mutation: `<` for `<=` in p_feasible -> 2 timeouts make P infeasible -> red
        timed_out = {"invalid": {"P": ["set_timeout"]}}
        self.write_pilot(kw=lambda b: timed_out if b in (2, 5) else {})
        c = self.check()
        self.assertEqual((c["p_timeouts"], c["p_feasible"]), (2, True))
        os.remove(os.path.join(self.out, "pilot-heavy.jsonl"))
        self.write_pilot(kw=lambda b: timed_out if b in (2, 5, 9) else {})
        c = self.check()
        self.assertEqual((c["p_timeouts"], c["p_feasible"], c["go"]), (3, False, True))

    def refused(self):
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            self.check()
        return cm.exception.code

    def test_pilot_check_refuses(self):
        # mutations: the pilot over the uncapped records -> the fourth re-run completes block
        # 12 -> red; drop the wide mix from the pilot -> 5 wide blocks pass -> red; drop the
        # refusal once main exists -> it runs -> red
        self.write_pilot(heavy=range(1, 12))
        self.assertEqual(self.refused(), 2)
        append(self.out, "pilot-heavy.jsonl", heavy_block7(12, invalid={"K60": ["battery"]}))
        for b in range(13, 17):  # only the fourth re-run is valid, past the cap of 3
            append(self.out, "pilot-heavy.jsonl", heavy_block7(
                b, as_block=12, invalid={"K60": ["battery"]} if b < 16 else None))
        self.assertEqual(self.refused(), 2)
        for name in ("heavy", "light", "wide"):
            os.remove(os.path.join(self.out, f"pilot-{name}.jsonl"))
        self.write_pilot(wide=range(1, 6))
        self.assertEqual(self.refused(), 2)
        append(self.out, "pilot-wide.jsonl", wide_block7(6))
        append(self.out, "main-heavy.jsonl", heavy_block7(1, "main"))
        self.assertEqual(self.refused(), 2)
        self.assertFalse(os.path.exists(os.path.join(self.out, "pilot_check.json")))
        os.remove(os.path.join(self.out, "main-heavy.jsonl"))
        self.assertTrue(self.check()["go"])


class Summary7(unittest.TestCase):
    """summary at 6 heavy blocks per level, 4 light and 4 wide, so 1 heavy re-run counts."""

    def setUp(self):
        self.out = tempfile.mkdtemp()
        p = mock.patch.multiple(run7, N_LEVEL=6, N_LIGHT=4, N_WIDE=4)
        p.start()
        self.addCleanup(p.stop)

    def write_main(self, heavy=range(1, 13), kw=lambda b: {}, arms=run7.HEAVY_ARMS,
                   p_feasible=True, wide=range(1, 5)):
        with open(os.path.join(self.out, "pilot_check.json"), "w") as f:
            json.dump({"go": True, "p_feasible": p_feasible, "light_pressure_sets": 0}, f)
        for b in heavy:
            append(self.out, "main-heavy.jsonl",
                   [r for r in heavy_block7(b, "main", **kw(b)) if r["arm"] in arms])
        for b in range(1, 5):
            append(self.out, "main-light.jsonl", light_block7(b, "main"))
        for b in wide:
            append(self.out, "main-wide.jsonl", wide_block7(b, "main"))

    def summary(self, **kw):
        with mock.patch("sys.stdout"):
            run7.summary(self.out, **kw)
        with open(os.path.join(self.out, "summary.json")) as f:
            return json.load(f)

    def test_summary_iut_all_three(self):
        # mutations: count a swap event at 64 KiB, not MiB -> every K60 set is one -> red; T2 on
        # the quiet blocks -> no swap traffic, P1 not rejected -> red
        self.write_main(kw=lambda b: {"swap": {"K60": 5000}} if b == 1 else {})
        s = self.summary()
        self.assertTrue(s["P1"]["rejected"])
        self.assertEqual({t: c["rejected"] for t, c in s["P1"]["components"].items()},
                         {"T1": True, "T2": True, "T3": True})
        self.assertEqual(s["P1"]["T2"]["wins"], 6)
        rows = s["secondary"]
        self.assertEqual(sorted(r["name"] for r in rows), sorted(run7.SECONDARY))
        self.assertEqual([r["threshold"] for r in rows], [0.05 / 4, 0.05 / 3, 0.05 / 2, 0.05])
        hz = next(r for r in rows if r["name"] == "H-Z")
        self.assertEqual((s["tests"]["H-Z"]["k"], s["tests"]["H-Z"]["n"]), (1, 12))
        self.assertEqual(hz["p"], stats7.binom_upper_p(1, 12, 0.03))
        self.assertEqual(hz["status"], "not rejected")
        hw = next(r for r in rows if r["name"] == "H-W")
        self.assertEqual((hw["tested"], hw["status"]), (True, "rejected"))
        self.assertEqual(s["tests"]["H-P"]["blocks"], s["blocks"]["heavy"]["loaded"])
        self.assertIn("P1 (intersection-union", text(os.path.join(self.out, "summary.txt")))

    def test_summary_iut_one_fails(self):
        # mutation: run Holm whatever P1 gave -> the rows carry thresholds -> red
        self.write_main(kw=lambda b: {"swap": {"S2": 10}})
        s = self.summary()
        self.assertEqual({t: c["rejected"] for t, c in s["P1"]["components"].items()},
                         {"T1": True, "T2": False, "T3": True})
        self.assertFalse(s["P1"]["rejected"] or s["dropped"])
        self.assertEqual(s["P1"]["T2"]["p"], 1.0)
        self.assertEqual(s["secondary"], [{"name": n, "status": "not tested (P1 not rejected)"}
                                          for n in run7.SECONDARY])

    def test_summary_p_infeasible(self):
        # mutation: leave H-P out of Holm when untested -> m 3 -> red
        self.write_main(arms=run7.P1_ARMS, p_feasible=False)
        s = self.summary()
        self.assertTrue(s["P1"]["rejected"])
        rows = s["secondary"]
        self.assertEqual(len(rows), 4)
        hp = next(r for r in rows if r["name"] == "H-P")
        self.assertEqual((hp["p"], hp["tested"], hp["rejected"], hp["status"]),
                         (None, False, False, "untested"))
        self.assertEqual(rows[-1], hp)
        self.assertEqual(rows[0]["threshold"], 0.05 / 4)
        self.assertIn("untested", s["tests"]["H-P"])

    def refused(self, **kw):
        with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
            self.summary(**kw)
        return cm.exception.code

    def test_summary_dropped(self):
        # mutations: ignore --dropped -> 3 of 4 wide blocks, no re-run spent -> refused -> red;
        # keep the wide n with the flag -> the fourth block is used -> red
        p = mock.patch.multiple(run7, N_WIDE=10, N_WIDE_DROPPED=3)  # one wide re-run left
        p.start()
        self.addCleanup(p.stop)
        self.write_main(wide=range(1, 4))
        self.assertEqual(self.refused(), 2)
        self.assertFalse(os.path.exists(os.path.join(self.out, "summary.json")))
        s = self.summary(dropped=True)
        self.assertEqual((s["dropped"], s["blocks"]["wide"], s["incomplete"]),
                         (True, [1, 2, 3], None))
        self.assertEqual(s["tests"]["H-W"]["blocks"], [1, 2, 3])
        self.assertEqual(s["reported"]["spread"]["blocks"], [])
        append(self.out, "main-wide.jsonl", wide_block7(4, "main"))
        self.assertEqual(self.summary(dropped=True)["blocks"]["wide"], [1, 2, 3])
        self.assertEqual(self.refused(), 2)

    def test_summary_refuses_short(self):
        # mutations: `<=` for `<` in the re-run check -> with the one re-run spent it still exits
        # 2 -> red; drop the pilot_check.json refusal -> FileNotFoundError, not exit 2 -> red
        self.assertEqual(self.refused(), 2)
        self.write_main(kw=lambda b: {"invalid": {"K60": ["battery"]}} if b == 11 else {})
        self.assertEqual(self.refused(), 2)
        append(self.out, "main-heavy.jsonl",
               heavy_block7(13, "main", invalid={"S1": ["battery"]}, as_block=11))
        s = self.summary()
        self.assertEqual(s["reruns"]["heavy"], [13])
        self.assertEqual(len(s["blocks"]["heavy"]["quiet"]), 5)
        self.assertIn("untested", s["P1"])
        self.assertFalse(s["P1"]["rejected"])
        self.assertTrue(s["incomplete"])

    def test_pairing_count(self):
        # mutations: hold a grant from the run's start, not after its wait -> set b overlaps ->
        # red; `>=` for `>` against the static size -> set c counts -> red
        def k60(*runs):
            r = fake_set("K60", 1, "quiet", 100, phase="main")
            r["runlog"] = [{"label": "bench:x", "time_ns": t * 10**9, "waited_s": w, "ran_s": ran,
                            "grant": {"cpu": 4, "mem": mem}} for t, w, ran, mem in runs]
            return r
        a = k60((0, 0.0, 10, 41), (2, 1.0, 10, 41))  # 82 tokens held over [3, 10]
        b = k60((0, 0.0, 5, 41), (3, 2.0, 5, 41))  # the second holds from 5, as the first ends
        c = k60((0, 0.0, 10, 40), (2, 0.0, 10, 36))  # 76, not above the static 76
        self.assertEqual(run7.pairing_count([a, b, c]), 1)


if __name__ == "__main__":
    unittest.main()
