"""Tests of run4.py's pure parts and its guards: python3 -m unittest evidence/test_run4.py"""
import ast
import contextlib
import io
import json
import os
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run4  # noqa: E402


def sset(block, arm, verdict, warn=0, critical=0, valid=True, wfree=0.0, waited=0.0,
         exits=None, as_block=None):
    """A minimal set record: `warn` samples at level 2 and `critical` at level 4."""
    samples = ([{"t": i, "pressure": 2, "swapouts": 0} for i in range(warn)]
               + [{"t": warn + i, "pressure": 4, "swapouts": 0} for i in range(critical)]
               + [{"t": warn + critical, "pressure": 1, "swapouts": 0}])
    return {"block": block, "arm": arm, "as_block": as_block, "valid": valid, "order": "FSK",
            "flags": [], "failures": [],
            "invalid": [] if valid else ["battery"], "mean_verdict_s": verdict,
            "makespan_s": verdict + 10, "samples": samples, "page_bytes": 16384,
            "gates": [{"agent": "A1", "gate": g, "start": 0, "end": 1, "exit": e}
                      for g, e in (exits or {"g": 0}).items()],
            "waited_free_s": wfree if arm == "K" else None,
            "runlog": [{"waited_s": waited, "waited_free_s": wfree}] if arm == "K" else []}


def write_out(d, recs, baseline=None):
    with open(os.path.join(d, "results.jsonl"), "w") as f:
        for r in recs:
            f.write(json.dumps(r) + "\n")
    with open(os.path.join(d, "learn.jsonl"), "w") as f:
        for g, e in (baseline or {"g": 0}).items():
            f.write(json.dumps({"gate": g, "round": 3, "form": "F", "exit": e}) + "\n")
    with open(os.path.join(d, "tuning.json"), "w") as f:
        json.dump({"S": {"N": 2}, "K": {"cpu": "140%", "mem": "60%"}, "medians": {}}, f)


def blocks(n, k=40.0, f=60.0, s=40.0, first=1, **kw):
    out = []
    for b in range(first, first + n):
        out += [sset(b, "F", f, warn=5, **kw), sset(b, "S", s, **kw), sset(b, "K", k, **kw)]
    return out


class Order(unittest.TestCase):
    def test_order_balanced(self):
        # mutation: perms[b % 6] in order_for leaves block 12 on perms[0] twice -> red
        orders = [run4.order_for(b) for b in range(1, 13)]
        for arm in run4.ARMS:
            for pos in range(3):
                self.assertEqual(sum(o[pos] == arm for o in orders), 4, (arm, pos))
        self.assertEqual("".join(orders[0]), "FSK")


class Statistics(unittest.TestCase):
    def test_sign_test_exact(self):
        self.assertAlmostEqual(run4.sign_test(10, 12), 0.0193, places=4)
        self.assertAlmostEqual(run4.sign_test(9, 12), 0.0730, places=4)

    def test_tie_is_not_a_win(self):
        # mutation: `<=` in wins -> the tie counts and this is red
        self.assertEqual(run4.wins([1.0, 2.0, 0.5], [1.0, 1.0, 1.0]), 1)
        self.assertEqual(run4.wins([1.1], [1.0], factor=1.10), 0)


def kconf(cpu, mem, median, critical=0, ff=0):
    return {"cpu": cpu, "mem": mem, "median": median, "critical_s": critical,
            "false_failures": ff}


class Selection(unittest.TestCase):
    def test_select_k_excludes_critical(self):
        # mutation: drop critical_s or false_failures from select_k's key -> the faster wins
        c = {"K-140%-60%": kconf("140%", "60%", 50), "K-200%-80%": kconf("200%", "80%", 40, 3)}
        self.assertEqual(run4.select_k(c), "K-140%-60%")
        c["K-200%-80%"] = kconf("200%", "80%", 40, ff=1)
        self.assertEqual(run4.select_k(c), "K-140%-60%")

    def test_select_fallback_when_none_qualifies(self):
        c = {"a": kconf("140%", "60%", 30, critical=5), "b": kconf("200%", "60%", 50, critical=2),
             "c": kconf("200%", "80%", 20, critical=2, ff=1)}
        self.assertEqual(run4.select_k(c), "b")  # fewest critical, then fewest false failures
        c["b"]["false_failures"] = 1
        self.assertEqual(run4.select_k(c), "c")  # then the lowest median

    def test_select_ties(self):
        c = {"x": kconf("140%", "80%", 40), "y": kconf("200%", "60%", 40),
             "z": kconf("140%", "60%", 40)}
        self.assertEqual(run4.select_k(c), "z")  # lower mem first, then lower cpu
        del c["z"]
        self.assertEqual(run4.select_k(c), "y")
        self.assertEqual(run4.select_s({2: 40.0, 3: 40.0}), 2)
        self.assertEqual(run4.select_s({2: 41.0, 3: 40.0}), 3)


class Records(unittest.TestCase):
    def test_false_failure_needs_baseline_pass(self):
        gates = [{"gate": "a", "exit": 1}, {"gate": "b", "exit": 1}, {"gate": "c", "exit": 0},
                 {"gate": "d", "exit": 1}]
        # a passed its baseline, b failed it, d has none
        self.assertEqual(run4.false_failures(gates, {"a": True, "b": False, "c": True}), 1)

    def test_set_invalid_on_sleep_gap(self):
        ok = [{"t": 100.0}, {"t": 101.0}, {"t": 111.0}]
        self.assertEqual(run4.invalid_reasons(ok, ac=True, error=None), [])
        gap = ok + [{"t": 121.5}]
        self.assertEqual(run4.invalid_reasons(gap, ac=True, error=None), ["sleep_gap"])
        self.assertEqual(run4.invalid_reasons(ok, ac=False, error="boom"),
                         ["runner_error: boom", "battery"])

    def test_scrub(self):
        home = os.path.expanduser("~")
        with mock.patch.object(run4, "_user", return_value="alice"), \
                mock.patch.object(run4, "_hosts", return_value=["box.local", "box"]):
            line = f"FAILED {home}/x.py::t - alice@box.local said box to alice"
            self.assertEqual(run4.scrub(line), "FAILED ~/x.py::t - <user>@<host> said <host> to <user>")


class Summary(unittest.TestCase):
    def test_decision_lines(self):
        # H1: K below F in 10 of 12; H2: K's warn 0 below F's 5 in all; H3: K below 1.1 x S in 9
        recs = blocks(10, k=40, f=60, s=40) + blocks(2, k=70, f=60, s=40, first=11)
        recs = [r for r in recs if not (r["block"] == 10 and r["arm"] == "K")]
        recs.append(sset(10, "K", 45))  # 45 < 60 (H1 win) but not < 44 (1.1 x 40)
        for r in recs:
            if r["arm"] == "K":
                r["waited_free_s"] = 6.0
                r["runlog"] = [{"waited_s": 50.0, "waited_free_s": 6.0}]
        s = run4.analyse(recs, {"g": True}, dry=False)
        self.assertEqual(s["tests"]["H1"]["wins"], 10)
        self.assertTrue(s["tests"]["H1"]["pass"])
        self.assertEqual(s["tests"]["H2"]["wins"], 12)
        self.assertEqual(s["tests"]["H3"]["wins"], 9)
        self.assertFalse(s["tests"]["H3"]["pass"])
        self.assertEqual(s["decision"], [
            "H1 passes, H3 fails: coreslot beats no admission but not a tuned semaphore; the next"
            " step is chosen from waited_free_s.",
            "Live-memory admission is worth designing: waited_free_s >= 5 s and >= 10% of K's"
            " waiting in 12 of 12 blocks."])
        recs[-1]["mean_verdict_s"] = 43  # now 10 of 12 for H3
        s = run4.analyse(recs, {"g": True}, dry=False)
        self.assertEqual(s["decision"][0], "H1 and H3 pass: develop the one-machine scenario"
                         " further and publish it as the headline.")

    def test_summary_refuses_early(self):
        with tempfile.TemporaryDirectory() as d:
            write_out(d, blocks(11))
            with self.assertRaises(SystemExit) as cm:
                with contextlib.redirect_stdout(io.StringIO()):
                    run4.summary(d, dry=False)
            self.assertEqual(cm.exception.code, 2)
            self.assertFalse(os.path.exists(os.path.join(d, "summary.json")))

    def test_incomplete_has_no_decision(self):
        with tempfile.TemporaryDirectory() as d:
            recs = blocks(11) + blocks(4, first=13, valid=False)
            write_out(d, recs)
            with contextlib.redirect_stdout(io.StringIO()):
                run4.summary(d, dry=False)
            md = open(os.path.join(d, "summary.txt")).read()
            self.assertIn("incomplete: 11 valid blocks; no test", md)
            self.assertNotIn("Decision", md)
            self.assertIsNone(json.load(open(os.path.join(d, "summary.json")))["decision"])


class Resume(unittest.TestCase):
    def test_resume_skips_recorded(self):
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "results.jsonl"), "w") as f:
                for arm in "FSK":
                    f.write(json.dumps({"block": 1, "arm": arm}) + "\n")
                f.write(json.dumps({"block": 2, "arm": "F"}) + "\n")
            json.dump({"S": {"N": 2}, "K": {"cpu": "140%", "mem": "60%"}},
                      open(os.path.join(d, "tuning.json"), "w"))
            open(os.path.join(d, "needs.toml"), "w").close()
            cfg = {"agents": {"A1": []}, "window_s": 5, "state_dir": "/nonexistent"}
            calls = []
            with mock.patch.object(run4, "run_set", lambda *a, **kw: calls.append(
                    (kw["meta"]["block"], kw["arm"]))), \
                    mock.patch.object(run4.acceptance, "probe", lambda: 1.0):
                run4.verdict(cfg, d, 1, 2, None)
            self.assertEqual(calls, [(2, "S"), (2, "K")])  # block 2 is SKF


class Guards(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp()
        p = mock.patch.object(run4, "ROOT", self.root)
        p.start()
        self.addCleanup(p.stop)

    def test_cs_sets_state_dir(self):
        seen = {}

        def fake(argv, env=None, **kw):
            seen.update(argv=argv, env=env, kw=kw)
        cfg = {"coreslot": "/bin/coreslot", "state_dir": os.path.join(self.root, "state")}
        run4.cs(cfg, "status", runner=fake, cwd="/tmp")
        self.assertEqual(seen["argv"], ["/bin/coreslot", "status"])
        self.assertEqual(seen["env"]["CORESLOT_STATE_DIR"], cfg["state_dir"])
        self.assertEqual(seen["kw"], {"cwd": "/tmp"})

    def test_cs_refuses_default_dirs(self):
        # mutation: drop the inside-ROOT check -> the first and last paths run the fake
        calls = []
        home = os.path.expanduser("~")
        for d in (os.path.join(home, "Library", "Application Support", "coreslot"),
                  os.path.join(home, ".local", "state", "coreslot"), self.root,
                  os.path.join(self.root, "..", "elsewhere")):
            with self.assertRaises(SystemExit, msg=d):
                run4.cs({"coreslot": "c", "state_dir": d}, "status",
                        runner=lambda *a, **k: calls.append(a))
        with mock.patch.object(run4, "ROOT", home):  # a default inside the root is refused too
            with self.assertRaises(SystemExit):
                run4.cs({"coreslot": "c", "state_dir": os.path.join(
                    home, ".local", "state", "coreslot")}, "status",
                    runner=lambda *a, **k: calls.append(a))
        self.assertEqual(calls, [])

    def test_only_cs_runs_coreslot(self):
        # every read of cfg["coreslot"] lies inside cs: no other path starts the binary
        src = open(run4.__file__).read()
        tree = ast.parse(src)
        allowed = set()
        for node in ast.walk(tree):
            if isinstance(node, ast.FunctionDef) and node.name == "cs":
                allowed |= {id(n) for n in ast.walk(node)}
        reads = [n for n in ast.walk(tree) if isinstance(n, ast.Subscript)
                 and isinstance(n.slice, ast.Constant) and n.slice.value == "coreslot"
                 and isinstance(n.ctx, ast.Load)]
        self.assertTrue(reads)
        self.assertEqual([n.lineno for n in reads if id(n) not in allowed], [])
        self.assertNotIn("check_empty", src)  # acceptance.check_empty starts the binary itself


if __name__ == "__main__":
    unittest.main()
