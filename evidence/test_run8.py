"""Tests of run8.py: python3 -m unittest evidence/test_run8.py"""
import contextlib
import hashlib
import json
import os
import random
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
            return subprocess.CompletedProcess(argv, 0, "", "")

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
            yield {"level": name, "headroom_before_mib": 9000, "ballast_mib": 0,
                   "ballast_rss_mib": None, "flags": []}

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
            (run4, "ROOT", self.root), (run4, "run_gate", run_gate),
            (run4, "on_ac", lambda: True), (run4, "background", lambda: (16384, {})),
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
        # count only job lines for Seq (not the ticks) -> the wrong line or none -> red
        with mock.patch.dict(run8.SET_TIMEOUT_S, heavy=10):  # a missed line ends as a timeout
            rec = self.run_set("P")
        self.assertTrue(rec["valid"], rec["invalid"])
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

    def test_set_timeout_per_mix_and_dry_only_override(self):
        # mutation: honour RUN8_SET_TIMEOUT_S on any stand -> red; one timeout for every mix -> red
        self.assertEqual([run8.set_timeout({}, m) for m in ("trace", "heavy", "wide", "batch")],
                         [3600, 600, 600, 10800])
        with mock.patch.dict(os.environ, {"RUN8_SET_TIMEOUT_S": "5"}):
            self.assertEqual(run8.set_timeout({}, "batch"), 10800)
            self.assertEqual(run8.set_timeout({"dry": True}, "batch"), 5.0)


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
        self.cfg["coreslot_sha256"] = "0" * 64
        self.assertEqual(refused(self, self.verdict, "pilot", "trace-quiet", 1, 1), 2)
        self.cfg["dry"] = True  # a dry stand skips the pin and the tree check
        self.verdict("pilot", "trace-quiet", 1, 1)
        n = len(self.calls)
        self.assertEqual(refused(self, self.verdict, "main", "trace-quiet", 1, 1), 2)
        self.write("pilot_check.json", {"go": True, "p_feasible": {"heavy-quiet": False},
                                        "batch_futile": True})
        self.assertEqual(refused(self, self.verdict, "main", "trace-quiet", 1, 1), 2)
        self.write("power.json", {"dropped": ["wide-quiet"]})
        self.assertEqual(refused(self, self.verdict, "main", "wide-quiet", 1, 1), 2)
        self.assertEqual(refused(self, self.verdict, "main", "batch", 1, 1), 2)
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


class Learn8(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.out = stand(self.root), os.path.join(self.root, "out")
        self.runs, self.supported = [], True

        def measured(cfg, g, form):
            self.runs.append((g["gate"], form, g["cwd"]))
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
        # gate as each agent -> four cwds -> red; leave the classes out -> red
        self.learn()
        self.assertEqual([r for r in self.runs if r[0] == "batch"], [("batch", "K", "/w/A1/b")] * 5)
        self.assertEqual(self.runs[:5], [("batch", "K", "/w/A1/b")] * 5)
        unit = [r for r in self.runs if r[0] == "c-unit"]
        self.assertEqual([r[1] for r in unit], ["K"] * 6 + ["F"] * 3 + ["K"] * 5)
        self.assertEqual({r[2] for r in unit}, {"/w/A1"})
        self.assertEqual({r[0] for r in self.runs},
                         {"batch", "tc", "lint", "build", "c-unit", "c-go", "c-db", "c-mut"})
        recs = run4.read_jsonl(os.path.join(self.out, "learn.jsonl"))
        self.assertEqual([(r["pass"], r["run"]) for r in recs if r["gate"] == "batch"],
                         [("learn", n) for n in range(1, 6)])
        self.assertTrue(os.path.exists(os.path.join(self.out, "needs.sha256")))

    def test_batch_skipped_without_preemptible(self):
        # mutation: learn the batch gate whatever the binary -> five runs -> red
        self.supported = False
        self.learn()
        self.assertNotIn("batch", {r[0] for r in self.runs})
        self.assertIn("c-go", {r[0] for r in self.runs})

    def test_refuses_after_a_probe_record(self):
        # mutation: refuse only on run 6's patterns -> a probe record lets learn run -> red
        os.makedirs(self.out)
        open(os.path.join(self.out, "probe-trace-quiet.jsonl"), "w").close()
        self.assertEqual(refused(self, run8.learn, self.cfg, self.out), 2)
        self.assertEqual(self.runs, [])


if __name__ == "__main__":
    unittest.main()
