import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest

import analyse
import run

HERE = os.path.dirname(os.path.abspath(__file__))


def a_set(tag, arm, started, levels, exits, swap=(100, 300), verdicts=(10, 20, 30, 40), runlog=()):
    gates = [{"agent": f"A{i}", "gate": g, "exit": e} for i, (g, e) in enumerate(exits, 1)]
    return {"tag": tag, "arm": arm, "started": started, "gates": gates, "makespan_s": max(verdicts),
            "samples": [{"t": i, "level": v, "swap_mib": 0} for i, v in enumerate(levels)],
            "swap_mib": {"start": swap[0], "end": swap[0], "peak": swap[1]}, "runlog": list(runlog),
            "agents": {f"A{i}": {"verdict_s": v, "exit": 0} for i, v in enumerate(verdicts, 1)}}


OK = [("tsc", 0), ("tests", 0)]


class AnalyseTest(unittest.TestCase):
    def setUp(self):
        self.out = tempfile.mkdtemp()
        learn = [{"gate": "tsc", "exit": 0, "dur_s": 10}, {"gate": "tsc", "exit": 0, "dur_s": 12},
                 {"gate": "tests", "exit": 0, "dur_s": 30}, {"gate": "flaky", "exit": 1, "dur_s": 5}]
        sets = [a_set("pilot", "F", "t0", [4, 4, 4], OK),
                a_set("pilot", "K", "t1", [1], OK),
                a_set("pair-1", "F", "t2", [1, 2, 4, None], [("tsc", 1), ("tests", 0), ("flaky", 1)]),
                a_set("pair-1", "K", "t3", [1, 2], OK, swap=(100, 100), verdicts=(10, 10, 10, 10),
                      runlog=[{"waited_s": 4.5, "stops": 1}, {"waited_s": 1.0}]),
                a_set("pair-2", "K", "t4", [1, 1], OK),
                a_set("pair-2", "F", "t5", [2, 2], OK),
                a_set("pair-3", "F", "t6", [2], OK)]  # its K never ran: no pair
        for name, rows in (("learn.jsonl", learn), ("sets.jsonl", sets)):
            with open(os.path.join(self.out, name), "w") as f:
                f.writelines(json.dumps(r) + "\n" for r in rows)

    def test_metrics(self):
        stable = analyse.stable_gates(analyse.load(os.path.join(self.out, "learn.jsonl")))
        self.assertEqual(stable, {"tsc", "tests"})
        sets = analyse.load(os.path.join(self.out, "sets.jsonl"))
        m = analyse.metrics(sets[2], stable)
        self.assertEqual((m["warn_s"], m["crit_s"], m["swap_mib"], m["ff"], m["fail"]), (2, 1, 200, 1, 2))
        self.assertEqual(m["verdict_s"], 25)
        k = analyse.metrics(sets[3], stable)
        self.assertEqual((k["waited_s"], k["stops"]), (5.5, 1))

    def test_sign_test(self):
        self.assertEqual(analyse.sign_test(3, 3), 0.125)
        self.assertEqual(analyse.sign_test(4, 5), 6 / 32)
        self.assertEqual(analyse.sign_test(0, 0), 1.0)

    def test_report_keeps_the_pilot_apart(self):
        text = analyse.report(self.out)
        pilot, pairs = text.index("PILOT (exploratory"), text.index("PAIRS (2 complete)")
        self.assertLess(pilot, pairs)
        self.assertNotIn("pilot", text[pairs:].split("DEMO")[0])
        self.assertIn("warn seconds: K lower in 2 of 2 untied pairs (0 tied); one-sided sign test p = 0.2500", text)
        self.assertIn("false failures: K lower in 1 of 1 untied pairs (1 tied)", text)
        self.assertIn("mean time to verdict: K lower in 1 of 1 untied pairs (1 tied)", text)
        self.assertIn("DEMO: no record", text)

    def test_demo(self):
        os.makedirs(os.path.join(self.out, "demo"))
        hogs = [{"hog": 1, "exit": 0, "attempts": 1, "stops": 0, "gave_up": False},
                {"hog": 2, "exit": 0, "attempts": 1, "stops": 0, "gave_up": False},
                {"hog": 3, "exit": 0, "attempts": 2, "stops": 1, "gave_up": False}]
        with open(os.path.join(self.out, "demo", "demo.json"), "w") as f:
            json.dump({"hog_gib": 6, "stopped_by": None, "hogs": hogs}, f)
        text = "\n".join(analyse.demo(self.out))
        self.assertIn("a stop happened: yes, hog 3", text)
        self.assertIn("stopped hogs restarted: 1 of 1", text)
        self.assertIn("every hog completed: yes", text)


class RunTest(unittest.TestCase):
    def test_abba(self):
        self.assertEqual("".join(arm for _, arm in run.abba(1, 5)), "FKKFFKKFFK")
        self.assertEqual(run.abba(2, 2), [("pair-2", "K"), ("pair-2", "F")])

    def test_readings(self):
        self.assertEqual(run.swap_mib("total = 5120.00M  used = 3895.50M  free = 1224.50M"), 3896)
        self.assertEqual(run.swap_mib("total = 8.00G  used = 1.50G  free = 6.50G"), 1536)
        self.assertIsNone(run.swap_mib(""))
        self.assertEqual(run.vm_counters("Pageouts:    80824.\nSwapins: 3.\nSwapouts:  1158754.\n"),
                         {"pageouts": 80824, "swapouts": 1158754})

    def test_settled(self):
        calm = [(1, 1000)] * 5
        self.assertTrue(run.settled(calm, 15, 15))
        self.assertFalse(run.settled(calm, 14, 15))
        self.assertFalse(run.settled(calm[:4], 15, 15))
        self.assertFalse(run.settled(calm[:4] + [(2, 1000)], 15, 15))
        self.assertFalse(run.settled([(1, 1000)] * 4 + [(1, 1100)], 15, 15))

    def test_guard(self):
        g = run.guard_for(1000, 60)
        self.assertIsNone(g([{"t": 5, "swap_mib": 1000 + run.SWAP_STOP_MIB}]))
        self.assertEqual(g([{"t": 5, "swap_mib": 1001 + run.SWAP_STOP_MIB}]), "swap")
        self.assertEqual(g([{"t": 60, "swap_mib": None}]), "time")

    def test_descendants(self):
        ps = "1 0 1 launchd\n10 1 10 coreslot run\n11 10 11 npm run\n12 11 11 node tsc\n20 1 20 other\n"
        self.assertEqual(run.descendants(ps, [10]), {10: (10, "coreslot run"), 11: (11, "npm run"),
                                                      12: (11, "node tsc")})

    def test_setup_pins_match(self):
        with open(os.path.join(HERE, "setup.sh")) as f:
            setup = f.read()
        for sha in run.PINS.values():
            self.assertIn(sha, setup)

    def dry(self, *args):
        out = os.path.join(tempfile.mkdtemp(), "out")
        p = subprocess.run([sys.executable, os.path.join(HERE, "run.py"), "--dry", "--smoke",
                            "--coreslot", "/bin/coreslot", "--stand", "/stand", "--out", out, *args],
                           capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertFalse(os.path.exists(out))  # a dry run starts and writes nothing
        return p.stdout.splitlines()

    def test_dry(self):
        k = [x for x in self.dry("set", "K", "--tag", "pilot") if x.startswith("pilot K")]
        self.assertEqual(len(k), 5)
        self.assertTrue(all("/bin/coreslot run --label smoke:" in x for x in k))
        f = [x for x in self.dry("pairs", "--first", "2", "--last", "2") if x.startswith("pair-2")]
        self.assertEqual([x.split()[1] for x in f], ["K"] * 5 + ["F"] * 5)
        self.assertTrue(all("coreslot" not in x for x in f[5:]))
        self.assertEqual(len([x for x in self.dry("learn", "--runs", "2") if x.startswith("learn ")]), 8)
        hogs = [x for x in self.dry("demo") if x.startswith("hog ")]
        self.assertEqual(len(hogs), 3)
        self.assertTrue(all("--label demo-hog" in x and "--slow-pct 0" in x for x in hogs))

    def test_bad_tag(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            run.main(["--dry", "--coreslot", "c", "--stand", "s", "--out", "o", "set", "F", "--tag", "x"])


if __name__ == "__main__":
    unittest.main()
