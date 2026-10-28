"""Tests of run8.py: python3 -m unittest evidence/test_run8.py"""
import contextlib
import hashlib
import json
import math
import os
import random
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import acceptance  # noqa: E402
import overload  # noqa: E402
import run4  # noqa: E402
import run6  # noqa: E402
import run7  # noqa: E402
import run8  # noqa: E402
import stats6  # noqa: E402


def text(path):
    with open(path) as f:
        return f.read()


def gate(name, heavy=True, cwd="/"):
    return {"gate": name, "label": f"bench:{name}", "cwd": cwd, "heavy": heavy, "argv": [name]}


def stand(root):
    return {"coreslot": "c", "state_dir": os.path.join(root, "state"), "window_s": 5,
            "agents": {"A1": [gate("tc")], "A2": [gate("lint", False), gate("tc")]},
            "wide_agents": {"W1": [gate("build")], "W2": [gate("tc")]},
            "agent_names": ["A1", "A2", "A3", "A4"],
            "classes": {c: gate(f"c-{c}", c != "unit", "/w/{agent}")
                        for c in ("unit", "go", "db", "mut")},
            "gates_by_class": {"unit": 5, "go": 3, "db": 2, "mut": 2},
            "behind_class": "unit", "batch": gate("batch", cwd="/w/{agent}/b")}


def refused(test, fn, *args, **kw):
    with test.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"), mock.patch("sys.stdout"):
        fn(*args, **kw)
    return cm.exception.code


def patch_all(test, patches):
    for target, name, fake in patches:
        p = mock.patch.object(target, name, fake)
        p.start()
        test.addCleanup(p.stop)


class Stand(unittest.TestCase):
    def setUp(self):
        self.cfg = stand(tempfile.mkdtemp())

    def test_trace_deal_is_seeded_and_paired(self):
        # mutations: shuffle with an unseeded Random -> two deals differ -> red; deal round-robin
        # (agent i takes items i, i+4, ...) -> red; keep "{agent}" in cwd -> red
        deck = [f"c-{c}" for c in sorted(self.cfg["gates_by_class"])
                for _ in range(self.cfg["gates_by_class"][c])]
        random.Random("run8-mix").shuffle(deck)
        agents = run8.trace_agents(self.cfg)
        self.assertEqual(agents, run8.trace_agents(self.cfg))
        self.assertEqual({n: [g["gate"] for g in c] for n, c in agents.items()},
                         {n: deck[3 * i:3 * i + 3] for i, n in enumerate(["A1", "A2", "A3", "A4"])})
        for n, chain in agents.items():
            self.assertEqual({g["cwd"] for g in chain}, {f"/w/{n}"})
        self.assertEqual(self.cfg["classes"]["go"]["cwd"], "/w/{agent}")  # the stand untouched
        # one deal for every cell, phase and block: cell_cfg takes no seed
        self.assertEqual(run8.cell_cfg(self.cfg, "trace-loaded")["agents"], agents)

    def test_trace_refuses_a_bad_stand(self):
        # mutations: drop the sum check -> 11 gates dealt -> red; drop the class check -> KeyError,
        # not exit 2 -> red; drop the agent_names check -> three agents dealt -> red
        for change in ({"gates_by_class": dict(self.cfg["gates_by_class"], unit=4)},
                       {"gates_by_class": dict(self.cfg["gates_by_class"], unit=4, lint=1)},
                       {"agent_names": ["A1", "A2", "A3"]}):
            with self.subTest(change=change):
                self.assertEqual(refused(self, run8.trace_agents, dict(self.cfg, **change)), 2)

    def test_trace_refuses_more_gates_than_twelve(self):
        # mutation: refuse only fewer than 12 (`<` for `!=`) -> 13 gates dealt, one dropped -> red
        more = dict(self.cfg["gates_by_class"], unit=6)
        self.assertEqual(sum(more.values()), 13)
        self.assertEqual(refused(self, run8.trace_agents, dict(self.cfg, gates_by_class=more)), 2)

    def test_batch_agents_offsets_and_delay(self):
        # mutations: random offsets in the batch cell -> red; let a non-dry stand set
        # batch_delay_s -> no exit -> red; ignore it on a dry stand -> 60 -> red
        scfg = run8.cell_cfg(self.cfg, "batch")
        self.assertEqual({n: [(g["gate"], g["cwd"]) for g in c] for n, c in scfg["agents"].items()},
                         {"A1": [("batch", "/w/A1/b")],
                          **{n: [("c-unit", f"/w/{n}")] * 3 for n in ("A2", "A3", "A4")}})
        self.assertEqual(run8.offsets8(scfg, "batch", "any"),
                         {"A1": 0, "A2": 60, "A3": 60, "A4": 60})
        dry = run8.cell_cfg(dict(self.cfg, dry=True, batch_delay_s=2), "batch")
        self.assertEqual(run8.offsets8(dry, "batch", "x"), {"A1": 0, "A2": 2, "A3": 2, "A4": 2})
        self.assertEqual(refused(self, run8.cell_cfg, dict(self.cfg, batch_delay_s=2), "batch"), 2)

    def test_cell_agents(self):
        # mutation: run the wide cell on the stand's agents -> A1, A2 -> red
        got = {c: list(run8.cell_cfg(self.cfg, c)["agents"]) for c in run8.CELLS}
        self.assertEqual(got, {"trace-quiet": ["A1", "A2", "A3", "A4"],
                               "trace-loaded": ["A1", "A2", "A3", "A4"],
                               "heavy-quiet": ["A1", "A2"], "heavy-loaded": ["A1", "A2"],
                               "wide-quiet": ["W1", "W2"], "batch": ["A1", "A2", "A3", "A4"]})
        self.assertEqual([v[1] for v in run8.CELLS.values()],
                         ["quiet", "loaded", "quiet", "loaded", "quiet", "quiet"])

    def test_stand_dry_run8_keys(self):
        # mutations: gates_by_class summing to 11, behind_class "lint", or two class gates with
        # one label -> red
        s = json.loads(text(os.path.join(HERE, "overload", "stand-dry.json")))
        self.assertTrue(s["dry"])
        self.assertGreaterEqual(len(s["agent_names"]), 4)
        self.assertEqual(sum(s["gates_by_class"].values()), 12)
        self.assertEqual(set(s["gates_by_class"]), set(s["classes"]))
        self.assertIn(s["behind_class"], s["classes"])
        labels = [g["label"] for g in [*s["classes"].values(), s["batch"]]]
        old = {g["label"] for a in ("agents", "wide_agents") for c in s[a].values() for g in c}
        self.assertEqual(len(set(labels)), len(labels))
        self.assertFalse(set(labels) & old)
        self.assertLess(s["batch_delay_s"], run8.BATCH_DELAY_S)


class Treatment(unittest.TestCase):
    B = "bench:batch"

    def t(self, cell, arm, *lines):
        return run8.treatment(cell, arm, [dict(l) for l in lines], self.B)

    def test_a_preemptible_on_another_label(self):
        # mutation: check only the batch label -> another label's mark passes -> red
        line = {"label": "bench:tc", "preemptible": "auto: median 14m >= 10m, cpu 12 >= 7"}
        for cell in run8.CELLS:
            for arm in run8.CELLS[cell][2]:
                with self.subTest(cell=cell, arm=arm):
                    self.assertTrue(any("bench:tc" in p for p in self.t(cell, arm, line)))
        self.assertEqual(self.t("trace-quiet", "K", {"label": "bench:tc"}), [])

    def test_b_batch_label_marked_outside_kp(self):
        # mutation: allow the batch label's mark in every arm -> K passes -> red
        line = {"label": self.B, "preemptible": "auto: median 14m >= 10m, cpu 12 >= 7"}
        for arm in ("K", "S1", "F"):
            self.assertEqual(len(self.t("batch", arm, line)), 1, arm)
        self.assertEqual(self.t("batch", "KP", line), [])

    def test_c_kp_needs_the_automatic_reason_on_every_line(self):
        # mutations: drop the no-line check -> red; accept any reason (key present) -> "entry"
        # passes -> red; check only the first line -> red; accept a missing key -> red
        auto = {"label": self.B, "preemptible": "auto: median 14m >= 10m, cpu 12 >= 7"}
        self.assertEqual(self.t("batch", "KP", {"label": "bench:c-unit"}),
                         ["no run-log line of the batch label"])
        self.assertEqual(len(self.t("batch", "KP", dict(auto, preemptible="entry"))), 1)
        self.assertEqual(len(self.t("batch", "KP", auto, dict(auto, preemptible="flag"))), 1)
        self.assertEqual(len(self.t("batch", "KP", {"label": self.B})), 1)
        self.assertEqual(self.t("batch", "KP", auto, auto, {"label": "bench:c-unit"}), [])


class KNeeds(unittest.TestCase):
    NEEDS = ('[needs]\n  [needs."bench:tc"]\n    cpu = "4"\n    mem = "8G"\n'
             '  [needs."bench:batch"]\n    cpu = "12"\n    mem = "6G"\n')

    def test_inserts_after_the_header(self):
        # mutation: append the key at the end of the file -> another table's -> red
        self.assertEqual(run8.k_needs(self.NEEDS, "bench:batch"),
                         self.NEEDS.replace('[needs."bench:batch"]\n',
                                            '[needs."bench:batch"]\n    preemptible = false\n'))

    def test_refusals(self):
        # mutations: take the first header of two -> red; drop the existing-key check -> two keys
        # -> red; look for the key in the whole file -> tc's key refuses batch -> red
        dup = self.NEEDS + '  [needs."bench:batch"]\n    cpu = "1"\n'
        has = self.NEEDS.replace('cpu = "12"\n', 'cpu = "12"\n    preemptible = true\n')
        for t in (self.NEEDS.replace("batch", "other"), dup, has):
            with self.subTest(t=t), self.assertRaises(ValueError):
                run8.k_needs(t, "bench:batch")
        other = self.NEEDS.replace('cpu = "4"\n', 'cpu = "4"\n    preemptible = false\n')
        self.assertIn("preemptible = false\n    cpu = \"12\"", run8.k_needs(other, "bench:batch"))


class RunSet8(unittest.TestCase):
    """run_set8 with the machine, the level and the sampler faked; gates go to a fake run_gate,
    P's parallel to a fake Popen that answers each job line with a joblog line."""

    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.path = stand(self.root), os.path.join(self.root, "sets.jsonl")
        self.cfg["agents"] = {"A1": [dict(gate("tc"), argv=["tc", "-n", "{n}"], free_n="3")],
                              "A2": [gate("lint", False), gate("tc")]}
        os.makedirs(self.cfg["state_dir"])
        self.argvs, self.running, self.peak, self.lines = [], set(), [0], []
        self.parallels, self.killed, self.answer, self.mark = [], [], True, {}
        self.codes, self.level_sleep, self.therm, self.outputs = {}, 0, [], []
        test, lock = self, threading.Lock()

        def run_gate(argv, **kw):
            with lock:
                test.argvs.append(argv)
                heavy = "lint" not in argv
                if heavy:
                    test.running.add(id(argv))
                    test.peak[0] = max(test.peak[0], len(test.running))
            time.sleep(0.05 if "lint" in argv else 0.3)
            with lock:
                test.running.discard(id(argv))
                test.lines.append(json.dumps({"label": "bench:lint" if "lint" in argv
                                              else "bench:tc", **test.mark}))
                test.mark = {}
            r = subprocess.CompletedProcess(argv, test.codes.get("lint" in argv, 0), "", "")
            r.cpu_s = 0.5
            return r

        class Parallel:
            def __init__(self, argv, **kw):
                self.argv, self.env, self.returncode = argv, kw["env"], None
                self.pid, self.seq, self.buf, self.stdin = 7000 + len(test.parallels), 0, "", self
                self.joblog = argv[argv.index("--joblog") + 1]
                test.parallels.append(self)

            def write(self, s):
                self.buf += s

            def flush(self):
                for line in self.buf.splitlines():
                    self.seq += 1
                    if line != ":" and test.answer:
                        sig = 0 if "-n" in line else 9
                        with open(self.joblog, "a") as f:
                            f.write(f"{self.seq}\t:\t{time.time() + 5:.3f}\t     7.000\t0\t0\t0"
                                    f"\t{sig}\t{line}\n")
                self.buf = ""

            def close(self):
                self.returncode = 0 if self.returncode is None else self.returncode

            def poll(self, timeout=None):
                return self.returncode
            wait = poll

        @contextlib.contextmanager
        def level(name):
            time.sleep(test.level_sleep)
            yield {"level": name, "headroom_before_mib": 9000, "ballast_mib": 0,
                   "ballast_rss_mib": None, "flags": []}

        def output(*argv):
            test.outputs.append(argv)
            return test.therm.pop(0) if test.therm else None

        def Sampler():  # run8.sampler swaps in its own samples list before start
            s = mock.Mock(samples=[])
            s.start = lambda: s.samples.append({"t": 1.0, "pressure": 1})
            return s

        def kill_tree(pids):
            self.killed.extend(pids)
            for p in self.parallels:
                if p.pid in pids:
                    p.returncode = -15
        patch_all(self, (
            (run4, "ROOT", self.root), (run4, "run_gate", run_gate), (run8, "run_gate", run_gate),
            (run7, "output", output), (run4, "on_ac", lambda: True),
            (run4, "background", lambda: (16384, {})),
            (run4, "budget_empty", lambda cfg: True),
            (acceptance, "cooldown", lambda *a: {"cooldown_gave_up": False}),
            (acceptance, "runlog_lines", lambda cfg: list(self.lines)),
            (overload, "Sampler", Sampler), (overload, "headroom_mib", lambda: 11000),
            (run7, "level", level), (run7, "kill_tree", kill_tree),
            (run7, "ps_snapshot", lambda: {}), (run7, "QUIET_HEADROOM_MIB", 10500),
            (run8, "parallel_free_mib", lambda: 4321), (run8, "TICK_S", 0.01),
            (run8.subprocess, "Popen", Parallel)))

    def run_set(self, arm, cell="heavy-quiet", offsets=None, **kw):
        with mock.patch("sys.stdout"):
            return run8.run_set8(self.cfg, self.path, cell=cell, arm=arm,
                                 offsets=offsets or {"A1": 0, "A2": 0},
                                 meta={"cell": cell, "dry": True}, baseline=1.0, **kw)

    def test_arm_forms(self):
        # mutations: hold no slot in S1 -> two heavy gates at once -> red; run K's argv free ->
        # no coreslot -> red; write a K arm's needs nowhere -> red
        for arm, slots, needs in (("K", None, "[needs]\n"), ("KP", None, "[needs] # kp\n"),
                                  ("S1", 1, None), ("S2", 2, None), ("F", None, None)):
            self.argvs.clear()
            self.peak[0] = 0
            rec = self.run_set(arm, slots=slots, needs=needs)
            self.assertTrue(rec["valid"], (arm, rec["invalid"]))
            tc = sorted(a for a in self.argvs if "lint" not in a)
            if arm.startswith("K"):
                self.assertEqual(tc[0], ["c", "run", "--label", "bench:tc", "--", "tc"])
                self.assertEqual(tc[1], ["c", "run", "--label", "bench:tc", "--", "tc", "-n", "{n}"])
                self.assertEqual(text(os.path.join(self.cfg["state_dir"], "needs.toml")), needs)
                self.assertEqual(text(os.path.join(self.cfg["state_dir"], "config.toml")),
                                 '[resources]\ncpu = "140%"\nmem = "60%"\n')
            else:
                self.assertEqual(tc, [["tc"], ["tc", "-n", "3"]])
            self.assertEqual(self.peak[0], 1 if arm == "S1" else 2, arm)
            self.assertEqual(rec["samples"], [{"t": 1.0, "pressure": 1, "parallel_free_mib": 4321}])

    def test_p_one_shared_queue_and_joblog_times(self):
        # mutations: start a parallel per gate -> two Popens -> red; time a gate from its
        # submission -> start == submitted -> red; take Exitval only -> the signal is lost -> red;
        # count only job lines for Seq (not the ticks) -> the wrong line or none -> red; P's
        # joblog exit not checked for gate_failed -> valid -> red
        with mock.patch.dict(run8.SET_TIMEOUT_S, heavy=10):  # a missed line ends as a timeout
            rec = self.run_set("P")
        self.assertEqual(rec["invalid"], ["gate_failed"])  # A2's job ended on signal 9
        self.assertEqual(len(self.parallels), 1)
        p = self.parallels[0]
        self.assertEqual(p.argv[:7], ["parallel", "--will-cite", "--memsuspend", "4.5G", "-j", "4",
                                      "--joblog"])
        self.assertEqual(p.env["PARALLEL_HOME"], os.path.join(self.root, "parallel-home"))
        self.assertGreater(p.seq, 2)  # ticks as well as the two jobs
        self.assertEqual([a for a in self.argvs], [["lint"]])  # the light gate runs free
        tc = sorted((g for g in rec["gates"] if g["gate"] == "tc"), key=lambda g: g["agent"])
        for g in tc:
            self.assertAlmostEqual(g["start"] - g["submitted"], 5.0, delta=0.3)
            self.assertAlmostEqual(g["end"] - g["start"], 7.0, delta=0.11)
        self.assertEqual([g["exit"] for g in tc], [0, -9])  # A1's job has "-n"
        self.assertEqual([f["agent"] for f in rec["failures"]], ["A2"])
        self.assertEqual(os.listdir(os.path.join(self.root, "parallel-home")), [])

    def test_p_timeout_kills_parallel(self):
        # mutation: leave parallel out of run4._live -> never killed, the agents wait on -> red
        self.answer, done = False, []
        with mock.patch.dict(run8.SET_TIMEOUT_S, heavy=1):
            t = threading.Thread(target=lambda: done.append(self.run_set("P")), daemon=True)
            t.start()
            t.join(15)
        self.assertEqual(len(done), 1, "the set did not end")
        self.assertEqual((done[0]["valid"], done[0]["invalid"]), (False, ["set_timeout"]))
        self.assertIn(self.parallels[0].pid, self.killed)
        with run4._live_lock:
            self.assertNotIn(self.parallels[0], run4._live)

    def test_runlog_of_every_arm_and_treatment(self):
        # mutations: keep the run log for K arms only -> S1's empty -> red; record the problems
        # without invalidating the set -> valid -> red
        rec = self.run_set("S1", slots=1)
        self.assertEqual(sorted(l["label"] for l in rec["runlog"]),
                         ["bench:lint", "bench:tc", "bench:tc"])
        self.assertEqual((rec["treatment"], rec["dry"], rec["cell"]), ([], True, "heavy-quiet"))
        self.mark = {"preemptible": "flag"}  # on the set's first gate, lint
        rec = self.run_set("F")
        self.assertEqual((rec["valid"], rec["invalid"]), (False, ["treatment"]))
        self.assertEqual(rec["treatment"], ["preemptible on bench:lint"])

    def test_batch_cell_times(self):
        # mutations: count the batch agent in short_mean_verdict_s -> red; batch_s from t0 -> red
        self.cfg["agents"] = {"A1": [gate("batch")] * 2, "A2": [gate("lint", False)],
                              "A3": [gate("lint", False)]}
        self.cfg["batch_agent"] = "A1"
        offsets = {"A1": 0.3, "A2": 0.4, "A3": 0.4}  # a batch start of 0 hides batch_s's origin
        rec = self.run_set("F", cell="batch", offsets=offsets)
        g = {x["agent"]: x for x in reversed(rec["gates"])}  # A1's first gate
        v = rec["verdict_s"]  # the batch agent's 0.6 s would raise the mean
        self.assertEqual(rec["short_mean_verdict_s"], round(statistics.mean([v["A2"], v["A3"]]), 1))
        self.assertNotEqual(rec["short_mean_verdict_s"], round(statistics.mean(v.values()), 1))
        self.assertGreaterEqual(g["A1"]["start"], 0.3)
        self.assertAlmostEqual(rec["batch_s"], g["A1"]["end"] - g["A1"]["start"], delta=0.11)
        self.assertNotIn("batch_s", self.run_set("F"))

    def test_a_failed_gate_invalidates_its_set(self):
        # mutation: no gate_failed reason -> red; coreslot's own exit in K invalidates too -> the
        # K set with exit 75 invalid -> red; skip K arms entirely -> K's exit 1 valid -> red
        for arm, code, invalid in (("F", 1, ["gate_failed"]), ("S1", -15, ["gate_failed"]),
                                   ("K", 1, ["gate_failed"]), ("K", 75, []), ("F", 0, [])):
            self.codes = {True: code}  # the light gate's exit
            rec = self.run_set(arm, slots=1 if arm == "S1" else None,
                               needs="[needs]\n" if arm == "K" else None)
            self.assertEqual(rec["invalid"], invalid, (arm, code))

    def test_level_wait_thermal_state_and_gate_cpu(self):
        # mutations: time the level wait after the level -> 0.0 -> red; therm_end taken before
        # the agents or dropped -> red; a failing pmset recorded as "" -> red; cpu_s left off the
        # gates -> red
        self.level_sleep = 0.3
        self.therm = ["  Thermal warning level: none \n", "CPU_Speed_Limit = 100\n"]
        rec = self.run_set("F")
        self.assertGreaterEqual(rec["level_wait_s"], 0.3)
        self.assertEqual((rec["therm_start"], rec["therm_end"]),
                         ("Thermal warning level: none", "CPU_Speed_Limit = 100"))
        self.assertEqual(self.outputs, [("pmset", "-g", "therm")] * 2)
        self.assertEqual([g["cpu_s"] for g in rec["gates"]], [0.5] * 3)
        rec = self.run_set("F")
        self.assertEqual((rec["therm_start"], rec["therm_end"]), (None, None))

    def test_set_timeout_per_mix_and_dry_only_override(self):
        # mutation: honour RUN8_SET_TIMEOUT_S on any stand -> red; one timeout for every mix -> red
        self.assertEqual([run8.set_timeout({}, m) for m in ("trace", "heavy", "wide", "batch")],
                         [3600, 600, 600, 10800])
        with mock.patch.dict(os.environ, {"RUN8_SET_TIMEOUT_S": "5"}):
            self.assertEqual(run8.set_timeout({}, "batch"), 10800)
            self.assertEqual(run8.set_timeout({"dry": True}, "batch"), 5.0)


class GateCpu(unittest.TestCase):
    def test_cpu_of_the_reaped_tree(self):
        # mutations: cpu_s from the runner's own rusage (RUSAGE_SELF) -> about 0 -> red; the exit
        # not taken from wait4 -> None -> red
        spin = "import time\nt = time.process_time()\nwhile time.process_time() - t < 0.3: pass"
        child = f"import subprocess, sys; subprocess.run([sys.executable, '-c', {spin!r}]); " \
                "print('ok'); sys.exit(3)"
        r = run8.run_gate([sys.executable, "-c", child])
        self.assertEqual((r.returncode, r.stdout), (3, "ok\n"))
        self.assertGreaterEqual(r.cpu_s, 0.3)  # the grandchild's time, reaped by the child


class FreeFigure(unittest.TestCase):
    def test_parallel_free_mib(self):
        # mutation: count speculative pages too -> red
        out = ("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:   6400.\n"
               "Pages active:  9.\nPages inactive:   57600.\nPages speculative:  640.\n")
        with mock.patch.object(run8.subprocess, "run",
                               lambda *a, **k: subprocess.CompletedProcess(a, 0, out, "")):
            self.assertEqual(run8.parallel_free_mib(), 1000)


class Verdict8(unittest.TestCase):
    """verdict with run_set8, the session's probes, git and parallel faked."""

    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.out = stand(self.root), os.path.join(self.root, "out")
        self.cfg["coreslot"] = os.path.join(self.root, "coreslot")
        with open(self.cfg["coreslot"], "wb") as f:
            f.write(b"binary")
        self.cfg["coreslot_sha256"] = hashlib.sha256(b"binary").hexdigest()
        os.makedirs(self.out)
        with open(os.path.join(self.out, "needs.toml"), "w") as f:
            f.write('[needs]\n  [needs."bench:batch"]\n    cpu = "12"\n')
        with open(os.path.join(self.out, "learn.jsonl"), "w") as f:
            f.write("{}\n")
        run7.seal(self.out)
        self.write("power.json", {"n": dict.fromkeys(run8.CELLS, 10), "dropped": []})
        self.calls, self.status, self.which, self.supported = [], "", "/bin/parallel", True

        def output(*argv):
            if "rev-parse" in argv:
                return "abc123\n"
            return self.status if "status" in argv else "GNU parallel 20260922\n"
        patch_all(self, (
            (run4, "ROOT", self.root),
            (run8, "run_set8", lambda cfg, path, **kw: self.calls.append(
                {"cfg": cfg, "path": path, **kw})),
            (run7, "output", output), (run8.shutil, "which", lambda cmd: self.which),
            (run7, "static_sizes7", lambda cfg: {"cpu": 14, "mem": 76}),
            (run6, "running_apps", lambda: []),
            (run6, "coreslot_version", lambda cfg: "coreslot 0.0.0-test"),
            (acceptance, "probe", lambda: 1.0),
            (run8, "preemptible_supported", lambda cfg: self.supported)))

    def write(self, name, obj):
        with open(os.path.join(self.out, name), "w") as f:
            json.dump(obj, f)

    def verdict(self, *args, **kw):
        with mock.patch("sys.stdout"):
            run8.verdict(self.cfg, self.out, *args, **kw)

    def test_seeds_paths_and_meta(self):
        # mutations: seed the order without the cell -> trace-quiet and trace-loaded share it ->
        # red (on these seeds); offsets from run 7's seed -> red; "dry" absent -> red
        self.verdict("pilot", "trace-loaded", 1, 2)
        arms = run8.CELLS["trace-loaded"][2]
        self.assertEqual([(c["meta"]["block"], c["arm"]) for c in self.calls],
                         [(b, a) for b in (1, 2) for a in run8.order_for("pilot", "trace-loaded",
                                                                       b, arms)])
        self.assertNotEqual(run8.order_for("pilot", "trace-quiet", 1, arms),
                            run8.order_for("pilot", "trace-loaded", 1, arms))
        c = self.calls[0]
        self.assertEqual(c["path"], os.path.join(self.out, "pilot-trace-loaded.jsonl"))
        self.assertEqual(c["offsets"], run4.offsets_for(c["cfg"], "run8-pilot-trace-loaded-1"))
        self.assertEqual(list(c["cfg"]["agents"]), ["A1", "A2", "A3", "A4"])
        m = c["meta"]
        self.assertEqual((m["cell"], m["mix"], m["level_name"], m["dry"], m["tree_dirty"]),
                         ("trace-loaded", "trace", "loaded", False, False))
        k = next(x for x in self.calls if x["arm"] == "K")
        self.assertEqual(k["needs"], text(os.path.join(self.out, "needs.toml")))
        self.assertEqual(next(x for x in self.calls if x["arm"] == "S3")["slots"], 3)
        self.assertIsNone(next(x for x in self.calls if x["arm"] == "F")["needs"])
        # mutation: the needs digest on K arms only -> S and F records carry None -> red
        self.assertEqual({x["meta"]["needs_sha256"] for x in self.calls},
                         {run4.sha256(os.path.join(self.out, "needs.toml"))})
        self.cfg["dry"] = True
        self.verdict("pilot", "trace-quiet", 1, 1)
        self.assertTrue(self.calls[-1]["meta"]["dry"])

    def test_batch_cell_needs_and_offsets(self):
        # mutations: give KP the derived needs -> red; give K the sealed needs -> red; random
        # offsets in the batch cell -> red
        self.verdict("pilot", "batch", 1, 1)
        by = {c["arm"]: c for c in self.calls}
        sealed = text(os.path.join(self.out, "needs.toml"))
        derived = run8.k_needs(sealed, "bench:batch")
        self.assertEqual((by["KP"]["needs"], by["K"]["needs"]), (sealed, derived))
        self.assertEqual(by["K"]["meta"]["derived_needs_sha256"],
                         hashlib.sha256(derived.encode()).hexdigest())
        self.assertIsNone(by["KP"]["meta"]["derived_needs_sha256"])
        self.assertEqual(by["F"]["offsets"], {"A1": 0, "A2": 60, "A3": 60, "A4": 60})
        with open(os.path.join(self.out, "needs.toml"), "w") as f:  # sealed, yet no batch entry
            f.write("[needs]\n")
        run7.seal(self.out)
        n = len(self.calls)
        self.assertEqual(refused(self, self.verdict, "pilot", "batch", 2, 2), 2)
        self.assertEqual(len(self.calls), n)

    def test_r8_skip(self):
        # mutations: run the batch sets without --preemptible -> sets -> red; rewrite the file on
        # every call -> red
        self.supported = False
        self.verdict("pilot", "batch", 1, 3)
        path = os.path.join(self.out, "batch-skipped.json")
        self.assertEqual(json.loads(text(path)),
                         {"reason": "the coreslot binary lacks --preemptible",
                          "binary_sha256": self.cfg["coreslot_sha256"]})
        self.assertEqual(self.calls, [])
        os.utime(path, (0, 0))
        self.verdict("pilot", "batch", 1, 1)
        self.assertEqual(os.stat(path).st_mtime, 0)
        self.verdict("pilot", "trace-quiet", 1, 1)
        self.assertEqual(len(self.calls), 5)

    def test_r8_skip_is_not_sticky(self):
        # mutation: ignore batch-skipped.json once the probe says supported -> sets -> red
        self.supported = False
        self.verdict("pilot", "batch", 1, 1)
        self.supported = True
        self.assertEqual(refused(self, self.verdict, "pilot", "batch", 1, 1), 2)
        self.assertEqual(self.calls, [])

    def test_refusals(self):
        # mutations: refuse a dirty tree only for main -> the pilot runs -> red; refuse it for the
        # probe too -> red; check go without power.json -> main runs -> red; skip the binary pin
        # -> red; require parallel for every cell -> red
        self.status = " M evidence/run8.py\n"
        self.assertEqual(refused(self, self.verdict, "pilot", "wide-quiet", 1, 1), 2)
        self.verdict("probe", "wide-quiet", 1, 1)
        self.assertEqual(len(self.calls), 4)
        self.status, self.which = "", None
        self.assertEqual(refused(self, self.verdict, "pilot", "heavy-quiet", 1, 1), 2)
        self.verdict("pilot", "wide-quiet", 1, 1)
        self.which = "/bin/parallel"
        # mutation: pin the binary at the pilot and main only -> the probe runs on it -> red
        for sha in ("0" * 64, "unpinned"):
            self.cfg["coreslot_sha256"] = sha
            for phase in ("probe", "pilot"):
                self.assertEqual(refused(self, self.verdict, phase, "trace-quiet", 1, 1), 2)
        self.assertEqual(len(self.calls), 8)
        self.cfg["dry"] = True  # a dry stand skips the pin and the tree check
        self.verdict("pilot", "trace-quiet", 1, 1)
        n = len(self.calls)
        self.assertEqual(refused(self, self.verdict, "main", "trace-quiet", 1, 1), 2)
        self.write("pilot_check.json", {"go": True, "p_feasible": {"heavy-quiet": False},
                                        "batch_futile": True})
        os.remove(os.path.join(self.out, "power.json"))
        for phase in ("pilot", "main"):  # mutation: refuse main only -> the pilot runs -> red
            self.assertEqual(refused(self, self.verdict, phase, "trace-quiet", 1, 1), 2)
        # mutations: run a cell power.json does not plan, at the pilot or main -> sets -> red;
        # refuse it -> exit 2 -> red; run the batch cell's main after a futile pilot -> red
        self.write("power.json", {"n": {"trace-quiet": 10, "heavy-quiet": 10}, "dropped": [
            "wide-quiet"], "left_out": {"batch": "x"}})
        for phase, cell in (("pilot", "wide-quiet"), ("pilot", "batch"), ("main", "wide-quiet")):
            self.verdict(phase, cell, 1, 1)
        self.write("power.json", {"n": dict.fromkeys(run8.CELLS, 10), "dropped": []})
        self.verdict("main", "batch", 1, 1)
        self.write("pilot_check.json", {"go": False})
        self.assertEqual(refused(self, self.verdict, "main", "trace-quiet", 1, 1), 2)
        self.assertEqual(len(self.calls), n)
        # mutation: keep P whatever pilot_check.json says -> red
        self.write("pilot_check.json", {"go": True, "p_feasible": {"heavy-quiet": False}})
        self.verdict("main", "heavy-quiet", 1, 1)
        self.assertEqual([c["arm"] for c in self.calls[n:]],
                         run8.order_for("main", "heavy-quiet", 1, ("K", "S1", "S2")))
        self.assertEqual(self.calls[-1]["meta"]["power_sha256"],
                         run4.sha256(os.path.join(self.out, "power.json")))
        with open(os.path.join(self.out, "needs.toml"), "a") as f:
            f.write("# changed\n")
        self.assertEqual(refused(self, self.verdict, "main", "heavy-loaded", 1, 1), 2)


class PreemptibleSupported(unittest.TestCase):
    def test_argv_and_the_three_outcomes(self):
        # mutations: probe another argv or the stand's state directory -> red; ignore the exit
        # code's value (any non-zero is unsupported) -> exit 1 answers False -> red; exit 64 refused
        # -> red
        root = tempfile.mkdtemp()
        cfg, calls = stand(root), []
        for code, want in ((0, True), (64, False), (1, 2)):
            def cs(cfg, *args, **kw):
                calls.append((cfg["state_dir"], args, kw))
                return subprocess.CompletedProcess(args, code, "", "bad state\nmore\n")
            with self.subTest(code=code), mock.patch.object(run4, "ROOT", root), \
                    mock.patch.object(run4, "cs", cs):
                got = refused(self, run8.preemptible_supported, cfg) if want == 2 else \
                    run8.preemptible_supported(cfg)
                self.assertEqual(got, want)
        self.assertEqual(calls[0], (cfg["state_dir"] + "-preemptible-probe",
                                    ("run", "--preemptible=false", "--", "true"),
                                    {"capture_output": True, "text": True}))
        with mock.patch.object(run4, "ROOT", root), mock.patch.object(run4, "cs", cs), \
                mock.patch("sys.stderr") as err, self.assertRaises(SystemExit):
            run8.preemptible_supported(cfg)
        self.assertIn("exit 1: bad state", "".join(c.args[0] for c in err.write.call_args_list))


class Learn8(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.out = stand(self.root), os.path.join(self.root, "out")
        self.runs, self.supported = [], True
        self.cfg["coreslot"] = os.path.join(self.root, "coreslot")
        with open(self.cfg["coreslot"], "wb") as f:
            f.write(b"binary")
        self.cfg["coreslot_sha256"] = hashlib.sha256(b"binary").hexdigest()

        def measured(cfg, g, form):
            self.runs.append((g["gate"], form, g["cwd"]))
            if g["gate"] == "batch":  # its run-log line, marked from the fourth run on
                os.makedirs(os.path.join(cfg["state_dir"], "log"), exist_ok=True)
                mark = {"preemptible": "auto: x"} if len(self.runs) > 3 else {}
                with open(os.path.join(cfg["state_dir"], "log", "runs.jsonl"), "a") as f:
                    f.write(json.dumps({"label": g["label"], "ran_s": 2.0, **mark}) + "\n")
            return 0, "", 1000, 1.0, 1.0

        def cs(cfg, *args, **kw):
            if args[0] == "suggest":
                with open(os.path.join(cfg["state_dir"], "needs.toml"), "w") as f:
                    f.write("[needs]\n")
            return subprocess.CompletedProcess(args, 0, "", "")
        patch_all(self, ((run4, "ROOT", self.root), (run4, "cs", cs),
                         (run6, "measured", measured),
                         (acceptance, "cooldown", lambda *a: {}),
                         (acceptance, "probe", lambda: 1.0),
                         (run8, "preemptible_supported", lambda cfg: self.supported)))

    def learn(self):
        with mock.patch("sys.stdout"):
            run8.learn(self.cfg, self.out)

    def test_batch_counts_and_class_gates(self):
        # mutations: give the batch gate run 7's forms -> F and relearn runs -> red; learn a class
        # gate as each agent -> four cwds -> red; leave the classes out -> red; LEARN_BATCH_K
        # back to 5 -> red
        self.learn()
        self.assertEqual([r for r in self.runs if r[0] == "batch"], [("batch", "K", "/w/A1/b")] * 6)
        self.assertEqual(self.runs[:6], [("batch", "K", "/w/A1/b")] * 6)
        unit = [r for r in self.runs if r[0] == "c-unit"]
        self.assertEqual([r[1] for r in unit], ["K"] * 6 + ["F"] * 3 + ["K"] * 5)
        self.assertEqual({r[2] for r in unit}, {"/w/A1"})
        self.assertEqual({r[0] for r in self.runs},
                         {"batch", "tc", "lint", "build", "c-unit", "c-go", "c-db", "c-mut"})
        recs = run4.read_jsonl(os.path.join(self.out, "learn.jsonl"))
        self.assertEqual([(r["pass"], r["run"]) for r in recs if r["gate"] == "batch"],
                         [("learn", n) for n in range(1, 7)])
        # mutation: leave the run-log line's preemptible value off the record -> red
        self.assertEqual([r["preemptible"] for r in recs if r["gate"] == "batch"],
                         [None] * 3 + ["auto: x"] * 3)
        self.assertTrue(os.path.exists(os.path.join(self.out, "needs.sha256")))

    def test_batch_skipped_without_preemptible(self):
        # mutation: learn the batch gate whatever the binary -> five runs -> red
        self.supported = False
        self.learn()
        self.assertNotIn("batch", {r[0] for r in self.runs})
        self.assertIn("c-go", {r[0] for r in self.runs})

    def test_pins_the_binary(self):
        # mutation: no pin in learn -> it runs on an unpinned binary -> red; pin a dry stand too
        # -> the dry learn refuses -> red
        for sha in ("unpinned", "0" * 64):
            self.cfg["coreslot_sha256"] = sha
            self.assertEqual(refused(self, run8.learn, self.cfg, self.out), 2)
        self.assertEqual(self.runs, [])
        self.cfg["dry"] = True
        self.learn()
        self.assertIn("c-go", {r[0] for r in self.runs})

    def test_refuses_after_a_probe_record(self):
        # mutation: refuse only on run 6's patterns -> a probe record lets learn run -> red
        os.makedirs(self.out)
        open(os.path.join(self.out, "probe-trace-quiet.jsonl"), "w").close()
        self.assertEqual(refused(self, run8.learn, self.cfg, self.out), 2)
        self.assertEqual(self.runs, [])


# --- the deciding side ------------------------------------------------------------------------

def rec8(cell, arm, block, t, phase, invalid=(), warn=0, swap=0, batch_s=100.0, **kw):
    """A set record: t as both time metrics, `warn` samples at pressure 2, `swap` MiB out."""
    return {"phase": phase, "cell": cell, "block": block, "as_block": None, "arm": arm,
            "valid": not invalid, "invalid": list(invalid), "gates": [], "runlog": [],
            "started": "2026-10-28T00:00:00+00:00", "mean_verdict_s": t, "batch_s": batch_s,
            "short_mean_verdict_s": t, "makespan_s": t + 10, "cooldown_s": 5, "page_bytes": 2**20,
            "samples": [{"pressure": 1 + (i < warn), "swapouts": swap * (i > warn), "swapins": 0}
                        for i in range(warn + 2)], **kw}


def write8(out, phase, cell, blocks, base=None, t=None, **kw):
    """Every arm's set per block: t(b, a) seconds, else base[a] (100) with +-2 % noise differing
    between arms; kw holds functions of (b, a) for rec8's other fields."""
    with open(os.path.join(out, f"{phase}-{cell}.jsonl"), "a") as f:
        for b in blocks:
            for i, a in enumerate(run8.CELLS[cell][2]):
                sec = t(b, a) if t else (base or {}).get(a, 100) * (1 + ((b * 7 + i) % 5 - 2) / 100)
                more = {k: v(b, a) for k, v in kw.items()}
                f.write(json.dumps(rec8(cell, a, b, sec, phase, **more)) + "\n")


def dump(path, obj):
    with open(path, "w") as f:
        json.dump(obj, f)


def deciding(fn, out, *args, name=None):
    with mock.patch("sys.stdout"):
        fn(*args, out)
    return json.loads(text(os.path.join(out, name)))


class Power8(unittest.TestCase):
    def setUp(self):
        self.probe, self.out = tempfile.mkdtemp(), tempfile.mkdtemp()
        base = {"trace-quiet": {"K": 90, "S1": 120, "S2": 110, "S3": 100},
                "trace-loaded": {"K": 90, "S2": 130, "S3": 120}, "batch": {"KP": 60, "F": 62}}
        self.base = base
        for cell in run8.CELLS:
            write8(self.probe, "probe", cell, range(1, 5), base.get(cell))

    def power(self):
        return deciding(run8.power, self.out, self.probe, name="power.json")

    def test_inputs(self):
        # mutations: S* fixed to S1 -> red; sigma = sd -> red; set_s without cooldown_s -> red;
        # KP/F against 0 -> red
        p = self.power()
        self.assertEqual(p["S*"], {"trace-quiet": "S3", "trace-loaded": "S1"})
        for cell, s in p["S*"].items():
            t = {(r["block"], r["arm"]): r["mean_verdict_s"]
                 for r in run4.read_jsonl(os.path.join(self.probe, f"probe-{cell}.jsonl"))}
            d = [math.log(t[b, "K"] / t[b, s]) for b in range(1, 5)]
            c = p["components"][cell][f"K/{s}"]
            self.assertAlmostEqual(c["mean"], statistics.fmean(d), places=12)
            self.assertEqual(c["sigma"], stats6.sigma_upper(statistics.stdev(d), 4, 0.80))
        # mutation: leave the cost leg out of the batch cell's components -> red
        self.assertEqual({k: v["bound"] for k, v in p["components"]["batch"].items()},
                         {"KP/K": 0, "KP/S1": 0, "KP/F": run8.MARGIN,
                          "batch_s KP/K": math.log(1 + run8.BATCH_COST_MARGIN)})
        self.assertEqual(p["batch_treatment"], [{"block": b, "treatment": []} for b in range(1, 5)])
        sets = run4.read_jsonl(os.path.join(self.probe, "probe-heavy-quiet.jsonl"))
        self.assertAlmostEqual(p["set_s"]["heavy-quiet"],
                               sum(r["makespan_s"] + 5 for r in sets) / 4, places=9)
        # mutation: a held set counted by its finished gates' makespan -> red
        os.remove(os.path.join(self.probe, "probe-heavy-loaded.jsonl"))
        write8(self.probe, "probe", "heavy-loaded", range(1, 5), timeout_s=lambda b, a: 600,
               invalid=lambda b, a: ["set_timeout"] if a == "P" else [])
        shutil.rmtree(self.out)
        os.mkdir(self.out)
        sets = run4.read_jsonl(os.path.join(self.probe, "probe-heavy-loaded.jsonl"))
        self.assertAlmostEqual(self.power()["set_s"]["heavy-loaded"], sum(
            (600 if r["arm"] == "P" else r["makespan_s"]) + 5 for r in sets) / 4, places=9)
        self.assertEqual((p["over_cap"], p["cap_h"], p["dropped"]), (False, 30, []))

    def test_drop_order_and_refusals(self):
        # mutations: DROP_ORDER reversed -> red; drop the power.json, pilot or short-primary
        # refusal -> it plans -> red; the batch kept when skipped -> red; refuse when the batch
        # cell alone exceeds the cap -> no power.json -> red; the batch hours left unscaled -> red
        with mock.patch.object(run8, "CAP_H", 1):
            p = self.power()
        o = p["menu"]["drop_order"]
        self.assertEqual((p["over_cap"], o["over_cap"], o["dropped"]),
                         (True, True, ["heavy-loaded", "wide-quiet", "trace-loaded"]))
        self.assertEqual(o["note"], "needs a shorter batch (780 s), not measured")
        self.assertAlmostEqual(o["batch_scaled"]["hours"], o["hours"]["batch"] * 7.8, places=9)
        self.assertEqual(o["batch_scaled"]["label"], "scaled, not measured")
        os.remove(os.path.join(self.out, "power.json"))
        with mock.patch.object(run8, "CAP_H", 3):
            p = self.power()
        self.assertEqual((p["dropped"], p["over_cap"], "batch" in p["n"]),
                         (["heavy-loaded", "wide-quiet", "trace-loaded"], True, True))
        self.assertEqual(refused(self, run8.power, self.probe, self.out), 2)
        os.remove(os.path.join(self.out, "power.json"))
        dump(os.path.join(self.out, "pilot-wide-quiet.jsonl"), {})
        self.assertEqual(refused(self, run8.power, self.probe, self.out), 2)
        os.remove(os.path.join(self.out, "pilot-wide-quiet.jsonl"))
        dump(os.path.join(self.probe, "batch-skipped.json"), {"reason": "no flag"})
        p = self.power()
        self.assertEqual((p["left_out"], "batch" in p["n"]), ({"batch": "no flag"}, False))
        os.remove(os.path.join(self.out, "power.json"))
        os.remove(os.path.join(self.probe, "probe-trace-loaded.jsonl"))
        write8(self.probe, "probe", "trace-loaded", [1])
        self.assertEqual(refused(self, run8.power, self.probe, self.out), 2)


    def test_hours_parts_and_menu(self):
        # mutations: the level wait left out -> red; main's re-runs not capped at n // 10 -> red;
        # P's invalid sets counted as re-runs -> heavy-loaded's non-zero -> red; learn_h to the
        # last start -> 4.0 -> red; the primer on every cell -> red; no_S1 keeping S1's sets -> red
        bad = {("heavy-quiet", "S1", 1), ("heavy-loaded", "P", 2)}
        for cell in run8.CELLS:
            os.remove(os.path.join(self.probe, f"probe-{cell}.jsonl"))
            write8(self.probe, "probe", cell, range(1, 5), self.base.get(cell),
                   level_wait_s=lambda b, a: 30,
                   invalid=lambda b, a: ["ac"] if (cell, a, b) in bad else [])
        with open(os.path.join(self.probe, "learn.jsonl"), "w") as f:
            for at, wall in (("00", 100), ("04", 1800)):
                f.write(json.dumps({"started": f"2026-10-28T{at}:00:00+00:00", "wall_s": wall})
                        + "\n")
        p = self.power()
        recs = run4.read_jsonl(os.path.join(self.probe, "probe-heavy-quiet.jsonl"))
        x = sum(r["makespan_s"] + 5 for r in recs) / 4
        n = p["n"]["heavy-quiet"]
        self.assertAlmostEqual(p["set_s"]["heavy-quiet"], x + 120, places=9)
        want = {"sets": (n + 10) * x / 3600, "level_wait": (n + 10) * 120 / 3600, "primer": 0,
                "reruns": (2.5 + n // 10) * (x + 120) / 3600}
        got = p["hours_parts"]["cells"]
        for k, v in want.items():
            self.assertAlmostEqual(got["heavy-quiet"][k], v, places=9, msg=k)
        self.assertEqual(got["heavy-loaded"]["reruns"], 0)
        k = [r["makespan_s"] + 5 for r in run4.read_jsonl(os.path.join(
            self.probe, "probe-trace-quiet.jsonl")) if r["arm"] == "K"]
        self.assertAlmostEqual(got["trace-quiet"]["primer"], (statistics.fmean(k) + 30) / 3600)
        self.assertEqual(p["hours_parts"]["learn_h"], 4.5)
        self.assertAlmostEqual(p["hours_parts"]["total_h"],
                               sum(sum(c.values()) for c in got.values()) + 4.5, places=9)
        menu = p["menu"]
        a, s1 = menu["all_arms"], menu["no_S1"]
        self.assertEqual(set(a["power"]), set(run8.CELLS))
        self.assertAlmostEqual(a["total_h"], sum(a["hours"].values()) + 4.5, places=9)
        xs1 = statistics.fmean(r["makespan_s"] + 5 for r in recs if r["arm"] == "S1")
        self.assertAlmostEqual(s1["hours"]["heavy-quiet"],
                               a["hours"]["heavy-quiet"] * (x - xs1 + 90) / (x + 120), places=9)
        self.assertEqual(s1["gives_up"], ["H-S1", "batch KP/S1",
                                          "trace-loaded K/S1 (S1 is the probe's S*)"])
        self.assertEqual((menu["drop_order"]["dropped"], "note" in menu["drop_order"]),
                         ([], False))

    def test_short_cells_and_the_treatment_check(self):
        # mutation: refuse only a short primary cell -> heavy-quiet planned on 1 block -> red
        os.remove(os.path.join(self.probe, "probe-heavy-quiet.jsonl"))
        write8(self.probe, "probe", "heavy-quiet", [1])
        self.assertEqual(refused(self, run8.power, self.probe, self.out), 2)
        write8(self.probe, "probe", "heavy-quiet", [2])
        # mutation: no treatment rule -> the batch left out for "0 valid probe blocks" -> red
        bad = ["no run-log line of the batch label"]
        os.remove(os.path.join(self.probe, "probe-batch.jsonl"))
        write8(self.probe, "probe", "batch", range(1, 5),
               treatment=lambda b, a: bad if a == "KP" else [],
               invalid=lambda b, a: ["treatment"] if a == "KP" else [])
        p = self.power()
        self.assertEqual(p["left_out"], {"batch": "the treatment check failed in every probe KP"
                                                  " set: no run-log line of the batch label"})
        self.assertEqual(p["batch_treatment"], [{"block": b, "treatment": bad}
                                                for b in range(1, 5)])
        # mutation: leave the cell out when any KP set failed (all for any) -> red
        os.remove(os.path.join(self.out, "power.json"))
        write8(self.probe, "probe", "batch", range(5, 7))
        self.assertIn("batch", self.power()["n"])


class PilotCheck8(unittest.TestCase):
    CELLS = ("trace-quiet", "heavy-quiet", "batch")

    def setUp(self):
        self.out = tempfile.mkdtemp()
        dump(os.path.join(self.out, "power.json"), {"n": dict.fromkeys(self.CELLS, 12)})

    def check(self, base=None, blocks=range(1, 11), **kw):
        for cell in self.CELLS:
            write8(self.out, "pilot", cell, blocks, (base or {}).get(cell), **kw)
        return deciding(run8.pilot_check, self.out, name="pilot_check.json")

    def test_outcomes(self):
        # mutations: go over every cell -> the futile batch stops main -> red; batch futile only
        # on all three comparisons -> red; outcomes swapped -> red
        for base, go, futile, outcome in (
                ({"trace-quiet": {"K": 120}}, False, False, "stop"),
                ({"trace-quiet": {"K": 80}, "batch": {"KP": 60, "F": 50}}, True, True, "batch"),
                ({"trace-quiet": {"K": 80}, "batch": {"KP": 60, "F": 62}}, True, False, "go")):
            self.setUp()
            c = self.check(base)
            self.assertEqual((c["go"], c["batch_futile"], c["outcome"]),
                             (go, futile, run8.OUTCOMES[outcome]))

    def test_futile_on_the_90_bound(self):
        # mutation: futility at alpha 0.05 -> the bound just past ln 1.03 is not futile -> red
        se = 0.05 * math.sqrt(10 / 9) / math.sqrt(10)
        for eps, futile in ((-1e-6, False), (1e-6, True)):
            self.setUp()
            c0 = run8.MARGIN + stats6.t_ppf(0.90, 9) * se + eps
            c = self.check(t=lambda b, a: 100 * math.exp(c0 + (0.05 if b % 2 else -0.05))
                           if a == "K" else 60 if a == "KP" else 100)
            self.assertEqual((c["go"], c["components"]["trace-quiet"]["K/S1"]["futile"]),
                             (not futile, futile))

    def test_p_feasibility_and_refusals(self):
        # mutations: `<` for `<=` -> 2 timeouts infeasible -> red; P deciding block validity ->
        # red; drop the short, main or power.json refusal -> red; refuse a short dry cell -> red
        for bs, feasible in (((2, 5), True), ((2, 5, 9), False)):
            self.setUp()
            c = self.check(invalid=lambda b, a: ["set_timeout"] if a == "P" and b in bs else [])
            self.assertEqual((c["p_timeouts"]["heavy-quiet"], c["p_feasible"], c["go"]),
                             (len(bs), {"heavy-quiet": feasible}, True))
            self.assertEqual(c["blocks"]["heavy-quiet"], list(range(1, 11)))
        # mutation: the count not pro rata -> a dry check's 2 timeouts in 2 blocks feasible -> red
        for bs, feasible in (((), True), ((1,), False)):
            self.setUp()
            c = self.check(blocks=(1, 2), dry=lambda b, a: True,
                           invalid=lambda b, a: ["set_timeout"] if a == "P" and b in bs else [])
            self.assertEqual(c["p_feasible"], {"heavy-quiet": feasible})
        self.setUp()
        self.assertEqual(refused(self, self.check, blocks=range(1, 10)), 2)
        for cell in self.CELLS:
            write8(self.out, "pilot", cell, range(11, 14), as_block=lambda b, a: 10,
                   invalid=lambda b, a: ["ac"])
        self.assertEqual(self.check(blocks=())["blocks"]["batch"], list(range(1, 10)))
        dump(os.path.join(self.out, "main-batch.jsonl"), {})
        self.assertEqual(refused(self, run8.pilot_check, self.out), 2)
        os.remove(os.path.join(self.out, "power.json"))
        os.remove(os.path.join(self.out, "main-batch.jsonl"))
        self.assertEqual(refused(self, run8.pilot_check, self.out), 2)
        self.setUp()
        c = self.check(blocks=(1, 2), dry=lambda b, a: True,
                       invalid=lambda b, a: ["treatment"] if a == "KP" else [])
        self.assertEqual((c["go"], c["batch_futile"], c["dry"], c["untested"]),
                         (True, False, True, {"batch": run8.DRY_SHORT}))


    def test_an_untested_primary_cell_stops(self):
        # mutation: go over the futile primary cells only -> an untested trace-quiet goes -> red
        c = self.check(blocks=(1, 2), dry=lambda b, a: True,
                       invalid=lambda b, a: ["ac"] if a == "S1" and b == 2 else [])
        self.assertEqual((c["go"], c["untested"]["trace-quiet"]), (False, run8.DRY_SHORT))
        self.assertEqual(c["outcome"], "stop: trace-quiet untested at the pilot; main does not"
                                       " run; the pilot is reported as exploratory, with its"
                                       " estimates")


GOOD = {"trace-quiet": {"K": 80}, "trace-loaded": {"K": 80}, "batch": {"KP": 60, "F": 62}}


class Summary8(unittest.TestCase):
    def summary(self, base=GOOD, dropped=(), joint=0.9, check=None, skipped=None, n=6,
                phase="main", pilot=(), **kw):
        """phase's records of every planned cell, and pilot records of the cells in pilot."""
        self.out = tempfile.mkdtemp()
        n = {c: n for c in run8.CELLS if c not in dropped}
        dump(os.path.join(self.out, "power.json"), {"n": n, "dropped": list(dropped),
                                                    "joint": joint, "descriptive": joint < 0.5})
        dump(os.path.join(self.out, "pilot_check.json"), check or {"go": True})
        if skipped:
            dump(os.path.join(self.out, "batch-skipped.json"), {"reason": skipped})
        for c in n:
            write8(self.out, phase, c, range(1, 7), base.get(c), **kw)
        for c in pilot:
            write8(self.out, "pilot", c, range(1, 7), base.get(c))
        return deciding(run8.summary, self.out, name="summary.json")

    def test_primary_iut_s_star_and_effects(self):
        # mutations: the primary by union -> one rejecting cell suffices -> red; S* fixed to S1
        # -> red; effects only for rejected cells -> red
        s = self.summary(dict(GOOD, **{"trace-quiet": {"K": 80, "S3": 90},
                                       "trace-loaded": {"K": 110}}))
        p = s["primary"]
        self.assertEqual((p["cells"]["trace-quiet"]["S*"], p["rejected"]), ("S3", False))
        self.assertTrue(p["components"]["trace-quiet"]["rejected"])
        self.assertEqual(set(s["effects"]), set(run8.CELLS))
        self.assertIsNotNone(s["effects"]["trace-loaded"]["ratio"]["S1/K"])
        # mutation: test the secondary only once the primary is rejected -> red
        self.assertTrue(s["tests"])
        self.assertFalse(any(r["status"].startswith("not tested") for r in s["secondary"]))
        self.assertTrue(self.summary()["primary"]["rejected"])
        # mutation: the joint power ignored -> a descriptive primary claims its rejection -> red
        p = self.summary(joint=0.4)["primary"]
        self.assertEqual((p["iut_rejected"], p["descriptive"], p["rejected"]), (True, True, False))
        # mutation: power.json's joint power left out of the primary -> red
        self.assertEqual(p["joint_power"], 0.4)
        self.assertIn("joint power 0.4", text(os.path.join(self.out, "summary.txt")))

    def test_a_pilot_stop_reports_the_pilot(self):
        # mutations: summarise main after a stop -> refused, no main blocks -> red; a family
        # tested -> red; effects for the primary cells only -> red; the futility legs dropped
        # -> red
        legs = {"trace-quiet": {"K/S3": {"mean": 0.1, "futile": True}}}
        s = self.summary(check={"go": False, "components": legs, "outcome": "stop: x"},
                         phase="pilot")
        self.assertEqual((s["phase"], s["exploratory"], s["pilot_check"]["components"]),
                         ("pilot", True, legs))
        rows = [s["primary"], *s["secondary"], *s["H-PRE"]["rows"]]
        self.assertEqual({r["status"] for r in rows}, {"not tested (stopped at the pilot)"})
        self.assertEqual(len(s["secondary"]), 5)  # H-S1..H-S3, H-SAFE-trace and -heavy
        self.assertEqual((set(s["effects"]), s["primary"]["joint_power"]), (set(run8.CELLS), 0.9))
        self.assertTrue(all(e["exploratory"] for e in s["effects"].values()))

    def test_cells_main_did_not_run_take_their_pilot_effects(self):
        # mutation: effect sizes for the main cells only -> no heavy-loaded or batch -> red
        s = self.summary(dropped=("heavy-loaded",), check={"go": True, "batch_futile": True},
                         pilot=("heavy-loaded", "batch"))
        self.assertEqual({c: e.get("exploratory", False) for c, e in s["effects"].items()},
                         {c: c in ("heavy-loaded", "batch") for c in run8.CELLS})
        self.assertIsNotNone(s["effects"]["batch"]["ratio"]["batch_s KP/K"])

    def test_secondary_union_and_safe(self):
        # mutations: H-S by IUT over its legs -> one slower cell is not enough -> red; H-SAFE for
        # a dropped loaded cell -> red; H-S legs over the batch cell -> 10 legs -> red
        s = self.summary(dict(GOOD, **{"heavy-quiet": {"S1": 130}}), dropped=("heavy-loaded",))
        h = s["tests"]["H-S1"]
        self.assertEqual((len(h["legs"]), h["p"]), (8, min(1, 8 * min(h["legs"].values()))))
        self.assertEqual(len(s["tests"]["H-S3"]["legs"]), 4)
        self.assertEqual({r["name"]: r["status"] for r in s["secondary"]}["H-S1"], "rejected")
        self.assertEqual([t for t in s["tests"] if "SAFE" in t], ["H-SAFE-trace"])

    def test_h_pre(self):
        # mutations: time over K and S1 only -> red; cost against ln 1.03 -> red; safety p of
        # one margin only -> red; a batch set coreslot failed keeps its own time -> red
        failed = [{"label": "x", "start": 0, "exit": 0}]
        for kw, leg, status in (
                ({"batch_s": lambda b, a: 120.0 if a == "KP" else 100.0}, "cost", "rejected"),
                ({}, "time", "rejected"), ({}, "safety", "rejected"),
                ({"base": dict(GOOD, batch={"KP": 60, "F": 55})}, "time", "not rejected"),
                ({"warn": lambda b, a: 10 if a == "KP" else 0}, "safety", "not rejected"),
                ({"swap": lambda b, a: 100 if a == "KP" else 0}, "safety", "not rejected"),
                ({"gates": lambda b, a: failed if a == "KP" else []}, "time", "not rejected"),
                # mutation: a failed KP set keeps its own batch_s -> cost 1.0, rejected -> red
                ({"gates": lambda b, a: failed if a == "KP" else [],
                  "batch_s": lambda b, a: 200.0 if a == "F" else 100.0}, "cost", "not rejected")):
            rows = self.summary(**kw)["H-PRE"]["rows"]
            self.assertEqual(({r["name"]: r["status"] for r in rows}[leg], len(rows)), (status, 3))

    def test_h_pre_cost_margin_is_ln_1_25(self):
        # mutation: ln(1 + 4 * BATCH_COST_MARGIN) for the cost leg -> 1.30 slower is rejected -> red
        for ratio, status in ((1.30, "not rejected"), (1.20, "rejected")):
            rows = self.summary(batch_s=lambda b, a: 100.0 * ratio if a == "KP" else 100.0)[
                "H-PRE"]["rows"]
            self.assertEqual({r["name"]: r["status"] for r in rows}["cost"], status, ratio)

    def test_primary_margin_is_ln_1_03(self):
        # mutation: the primary tested against 2 * MARGIN -> K 4.5 % slower is rejected -> red
        s = self.summary(t=lambda b, a: 104.5 if a == "K" else 100.0)
        self.assertFalse(s["primary"]["rejected"])
        self.assertTrue(all(c["p"] >= run8.ALPHA for c in s["primary"]["cells"].values()))

    def test_h_pre_skipped_untested_and_refusals(self):
        # mutations: the skip reason dropped -> red; the pilot's batch drop ignored -> red; a
        # dry cell judged against n or cut to n -> red; drop the pilot_check.json refusal ->
        # red; drop the refusal while a cell is short with re-runs left -> red
        self.assertEqual(self.summary(skipped="no flag")["H-PRE"], {"skipped": "no flag"})
        h = self.summary(check={"go": True, "batch_futile": True})["H-PRE"]
        self.assertEqual(h, {"untested": "the pilot check found the batch cell futile"})
        for dry, untested, wide in (
                (True, {"batch": run8.DRY_SHORT}, list(range(1, 7))),
                (False, dict.fromkeys(("trace-quiet", "trace-loaded", "batch"), run8.SHORT),
                 [1, 2, 3, 4])):  # n 4: a dry cell keeps every valid block
            s = self.summary(n=4, dry=lambda b, a: dry, invalid=lambda b, a: ["ac"] if a in (
                "KP", "S3") and b > (1 if a == "KP" else 2) else [])
            self.assertEqual((s["dry"], s["untested"], s["blocks"].get("wide-quiet")),
                             (dry, untested, wide))
            self.assertEqual(s["H-PRE"], {"untested": untested["batch"]})
        os.remove(os.path.join(self.out, "pilot_check.json"))
        self.assertEqual(refused(self, run8.summary, self.out), 2)
        self.assertEqual(refused(self, self.summary, n=10), 2)


class Wrapper8(unittest.TestCase):  # overload/run8.sh under stub tools and a stub run8.py
    SH = os.path.join(HERE, "overload", "run8.sh")
    ENV = {"STUB_BATT": "AC Power", "STUB_INDEX": "Indexing disabled.", "STUB_PGREP": "1 /bin/node -k=hunter2",
           "STUB_UPDATE": "Automatic checking for updates is turned on", "STUB_CHECK": "", "STUB_DOWNLOAD": "0",
           "STUB_INSTALL": "0", "STUB_VM": ""}
    CELLS = "trace-quiet trace-loaded heavy-quiet heavy-loaded wide-quiet batch".split()

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.tmp)
        self.out, self.log = os.path.join(self.tmp, "out"), os.path.join(self.tmp, "calls.log")
        os.mkdir(self.out)
        listing = 'echo "PID Status Label"; echo "- 0 com.apple.x"; echo "- 0 com.example.agent"'
        for tool, body in {"pmset": "$STUB_BATT", "mdutil": "$STUB_INDEX", "softwareupdate": "$STUB_UPDATE",
                           "pgrep": 'case $2 in *Virtualization*) echo "$STUB_VM" ;; *) echo "$STUB_PGREP" ;; esac',
                           "caffeinate": "", "launchctl": listing,
                           "defaults": 'case $3 in *Check*) echo "$STUB_CHECK" ;; *Download) echo "$STUB_DOWNLOAD" ;; '
                                       '*) echo "$STUB_INSTALL" ;; esac'}.items():
            body = f'echo "{body}"' if body.startswith("$") else body
            with open(os.path.join(self.tmp, tool), "w") as f:
                f.write(f'#!/bin/sh\necho "{tool} $*" >> "$LOG"\n{body}\n')
            os.chmod(os.path.join(self.tmp, tool), 0o755)
        with open(os.path.join(self.tmp, "run8.py"), "w") as f:  # logs argv; pilot-check says go
            f.write("import os, sys\nopen(os.environ['LOG'], 'a').write('run8.py ' + ' '.join(sys.argv[1:]) + '\\n')\n"
                    "if sys.argv[1] == 'pilot-check':\n    open(sys.argv[2] + '/pilot_check.json', 'w').write('{\"go\": true}')\n")

    def sh(self, *args, power=True, sealed=True, **env):
        for name, present in (("power.json", power), ("needs.sha256", sealed)):
            path = os.path.join(self.out, name)
            if present:
                open(path, "w").close()
            elif os.path.exists(path):
                os.remove(path)
        path = [self.tmp, os.path.dirname(sys.executable), "/usr/bin", "/bin", "/usr/sbin"]
        env = dict(os.environ, **{**self.ENV, **env}, LOG=self.log, OUT=self.out, STAND="S",
                   RUN8_PY=os.path.join(self.tmp, "run8.py"), PATH=":".join(path),
                   RUN8_PRIVATE=os.path.join(self.tmp, "private"))
        code, err = (lambda p: (p.returncode, p.stderr))(subprocess.run(
            ["/bin/sh", self.SH, *args], env=env, capture_output=True, text=True))
        lines = text(self.log).splitlines() if os.path.exists(self.log) else []
        os.path.exists(self.log) and os.remove(self.log)
        return code, err, [x.replace(self.out, "OUT")[8:] for x in lines if x.startswith("run8.py")], lines

    def test_preflight(self):
        # mutations: no AC check, or it run after python -> red; the indexing or the update
        # check dropped, the update check read from softwareupdate --schedule or from
        # AutomaticCheckEnabled, either update key unchecked, or either check enforced on a dry
        # run -> red; a running VM not refused, or refused on a dry run -> red; caffeinate without -w, or pgrep's full command lines logged, or the
        # apple agents listed -> red
        batt, off, on = {"STUB_BATT": "Battery Power"}, {"STUB_INDEX": "Indexing enabled."}, {"STUB_DOWNLOAD": "1"}
        vm = {"STUB_VM": "47376 /System/Library/Frameworks/x/com.apple.Virtualization.VirtualMachine"}
        for args, env, rc in (((), batt, 2), (("--dry",), batt, 2), (("learn",), off, 2), (("verdict",), on, 2),
                              (("learn",), {"STUB_INSTALL": "1"}, 2), (("learn",), {"STUB_INSTALL": ""}, 2),
                              (("verdict",), vm, 2), (("--dry", "learn"), vm, 0),
                              (("--dry", "verdict"), {**off, **on}, 0), (("preflight",), {}, 0), (("learn",), {}, 0)):
            code, err, py, _ = self.sh(*args, **env)
            self.assertEqual((code, bool(py), "run8: " in err), (rc, rc == 0 and args[-1] != "preflight", rc == 2), args)
        # mutations: an agent or process name in OUT/preflight.log, or the names not in the
        # private file -> red; counts by another pattern -> red
        log = text(os.path.join(self.out, "preflight.log"))
        self.assertTrue("hunter2" not in log and "com.example.agent" not in log
                        and "/bin/node" not in log and "com.apple.x" not in log
                        and "background agents not com.apple: 1" in log
                        and "workload processes: go 0, pytest 0, node 1, docker 0, claude 0" in log
                        and "Indexing enabled" in log
                        and "AutomaticCheckEnabled: ; AutomaticDownload: 1" in log
                        and "virtual machines: 47376 com.apple.Virtualization.VirtualMachine" in log, log)
        private = text(os.path.join(self.tmp, "private", "preflight-out.log"))
        self.assertTrue("com.example.agent" in private and "1 node" in private
                        and "hunter2" not in private and "com.apple.x" not in private, private)
        self.assertIn("caffeinate -dimsu -w ", "\n".join(self.sh("preflight")[3]))

    def test_dispatch_and_default_paths(self):
        # mutations: power, pilot-check or summary given STAND, or power not given OUT last ->
        # red; the primer on every pilot or on none -> red; the pilot started without power.json
        # -> red; the dry pipeline's order, its probe, power OUT OUT or main loop changed -> red
        v = lambda phase, c, last, primer=False: f"verdict S OUT {phase} {c} 1 {last}" + " --primer" * primer
        for args, want in ((("pilot-check",), "pilot-check OUT"), (("summary", "--x"), "summary OUT --x"),
                           (("power", "PROBE"), "power PROBE OUT"), (("verdict", "main", "batch", "1", "2"), v("main", "batch", 2))):
            self.assertEqual(self.sh(*args)[2], [want])
        self.assertEqual(self.sh()[2], [v("pilot", c, 10, i == 0) for i, c in enumerate(self.CELLS)])
        code, err, py, _ = self.sh(power=False, sealed=False)
        self.assertEqual((code, py, "power.json" in err), (2, ["learn S OUT"], True))
        dry = (["learn S OUT"] + [v("probe", c, 2) for c in self.CELLS] + ["power OUT OUT"]
               + [v("pilot", c, 2, i == 0) for i, c in enumerate(self.CELLS)] + ["pilot-check OUT"]
               + [v("main", c, 2) for c in self.CELLS] + ["summary OUT"])
        self.assertEqual(self.sh("--dry", power=False, sealed=False)[2], dry)


if __name__ == "__main__":
    unittest.main()
