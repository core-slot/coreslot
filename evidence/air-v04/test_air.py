import contextlib
import io
import json
import os
import subprocess
import sys
import tempfile
import threading
import time
import unittest

import analyse
import criteria2
import run
import sem

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


    def test_second_run_sections(self):
        self.assertNotIn("TUNING", analyse.report(self.out))  # the first run's output is unchanged
        sets = [a_set("tune-S1", "S", "u1", [1], [("tsc", 1)], verdicts=(5, 5)),  # fastest, but a false failure
                a_set("tune-S3", "S", "u3", [1], OK, verdicts=(20, 20)),
                a_set("tune-S2", "S", "u2", [1], OK, verdicts=(20, 20)),  # ties S3: the smaller N
                a_set("sem-pair-1", "S", "v1", [2, 2], OK, verdicts=(40, 40)),
                a_set("sem-pair-1", "K", "v2", [1], OK, verdicts=(20, 20)),
                a_set("light-pilot", "F", "w0", [1], OK),
                a_set("light-pair-1", "F", "w1", [1], OK, verdicts=(10, 10)),
                a_set("light-pair-1", "K", "w2", [1], OK, verdicts=(11, 11))]
        with open(os.path.join(self.out, "sets.jsonl"), "a") as f:
            f.writelines(json.dumps(r) + "\n" for r in sets)
        text = analyse.report(self.out)
        self.assertIn("S* = S2 (lowest", text)
        self.assertIn("SEMAPHORE PAIRS (1 complete", text)
        self.assertIn("mean time to verdict: K lower in 1 of 1 untied pairs (0 tied); one-sided sign test p = 0.5000", text)
        self.assertIn("K/S mean time to verdict per pair: [0.5]", text)
        self.assertIn("K/F makespan per pair: [1.1]; max 1.1", text)
        self.assertIn("makespan: K higher in 1 of 1 untied pairs; one-sided sign test p = 0.5000", text)
        self.assertLess(text.index("PAIRS (2 complete)"), text.index("TUNING"))


class CriteriaTest(unittest.TestCase):
    def test_both_tests_literally(self):
        out = tempfile.mkdtemp()
        sets = [a_set("tune-S1", "S", "t", [1], OK, verdicts=(10, 20))]
        for i, k in enumerate((10, 10.5, 10.6, 9, 10), 1):  # K/S 1.0, 1.05 (within), 1.06 (outside)
            sets += [a_set(f"sem-pair-{i}", "S", "t", [1], OK, verdicts=(10, 10)),
                     a_set(f"sem-pair-{i}", "K", "t", [1], OK, verdicts=(k, k))]
        for i in range(1, 5):  # light-pair-5 never ran
            sets += [a_set(f"light-pair-{i}", "F", "t", [1], OK, verdicts=(20, 40)),
                     a_set(f"light-pair-{i}", "K", "t", [1], OK, verdicts=(30, 30))]
        with open(os.path.join(out, "sets.jsonl"), "w") as f:
            f.writelines(json.dumps(r) + "\n" for r in sets)
        text = criteria2.report(out)
        self.assertIn("sem-pair-2: K 10.5000 s, S 10.0000 s, K/S 1.0500 within", text)
        self.assertIn("sem-pair-3: K 10.6000 s, S 10.0000 s, K/S 1.0600 OUTSIDE", text)
        self.assertIn("within in 4 of 5 pairs; one-sided sign test p = 0.1875; the test fails", text)
        self.assertIn("light-pair-4: K 30.0000 s, F 30.0000 s, K/F 1.0000 within", text)
        self.assertIn("within in 4 of 4 pairs; one-sided sign test p = 0.1875; the test fails", text)
        self.assertIn("tune-S1 S: mean time to verdict 15.0000 s", text)
        self.assertEqual(criteria2.p_at_least(5, 5), 1 / 32)


class SemTest(unittest.TestCase):
    def two(self, slots, *fifo):
        d, t = tempfile.mkdtemp(), time.monotonic()
        ps = [subprocess.Popen([sys.executable, os.path.join(HERE, "sem.py"), *fifo, d, str(slots), "--", "sleep", "0.5"])
              for _ in range(2)]
        self.assertEqual([p.wait() for p in ps], [0, 0])
        return time.monotonic() - t

    def test_one_slot_serialises_two_slots_overlap(self):
        for fifo in ([], ["--fifo"]):
            self.assertGreaterEqual(self.two(1, *fifo), 1.0)
            self.assertLess(self.two(2, *fifo), 0.9)

    def test_fifo_grants_in_arrival_order(self):
        for order in ("abc", "cab", "bca", "acb"):
            d = tempfile.mkdtemp()
            log = os.path.join(d, "log")
            ps = []
            for name in order:  # a holds the slot while b and c queue behind it
                ps.append(subprocess.Popen([sys.executable, os.path.join(HERE, "sem.py"), "--fifo", d, "1", "--",
                                            "sh", "-c", f"echo {name} >> {log}; sleep 0.25"]))
                time.sleep(0.1)
            self.assertEqual([p.wait() for p in ps], [0, 0, 0])
            with open(log) as f:
                self.assertEqual(f.read().split(), list(order))

    def test_exit_status_is_the_commands(self):
        p = subprocess.run([sys.executable, os.path.join(HERE, "sem.py"), tempfile.mkdtemp(), "1", "--", "sh", "-c", "exit 3"])
        self.assertEqual(p.returncode, 3)

    def test_dead_waiter_and_junk_are_dropped(self):
        d = os.path.join(tempfile.mkdtemp(), "locks")
        os.makedirs(d)
        dead = subprocess.Popen(["true"])
        dead.wait()
        with open(os.path.join(d, "queue"), "w") as f:
            f.write(f"junk\n{dead.pid}\n")
        p = subprocess.run([sys.executable, os.path.join(HERE, "sem.py"), "--fifo", d, "1", "--", "true"], timeout=10)
        self.assertEqual(p.returncode, 0)
        with open(os.path.join(d, "queue")) as f:
            self.assertEqual(f.read(), "")

    def test_a_lost_queue_line_queues_again(self):
        d = tempfile.mkdtemp()
        held, got = sem.try_slot(d, 1), []
        t = threading.Thread(target=lambda: got.append(sem.acquire(d, 1, fifo=True, poll_s=0.01)))
        t.start()
        time.sleep(0.2)
        with open(os.path.join(d, "queue"), "w") as f:
            f.write("junk\n")  # our own line is gone
        os.close(held)
        t.join(5)
        self.assertEqual(len(got), 1)
        os.close(got[0])

    def test_lockdir_is_private_and_links_are_refused(self):
        d = os.path.join(tempfile.mkdtemp(), "locks")
        subprocess.run([sys.executable, os.path.join(HERE, "sem.py"), d, "1", "--", "true"], check=True)
        self.assertEqual(os.stat(d).st_mode & 0o777, 0o700)
        os.remove(os.path.join(d, "slot-0"))
        os.symlink(os.path.join(d, "elsewhere"), os.path.join(d, "slot-0"))
        p = subprocess.run([sys.executable, os.path.join(HERE, "sem.py"), d, "1", "--", "true"], capture_output=True)
        self.assertNotEqual(p.returncode, 0)
        self.assertFalse(os.path.exists(os.path.join(d, "elsewhere")))


class RunTest(unittest.TestCase):
    def test_abba(self):
        self.assertEqual("".join(arm for _, arm in run.abba(1, 5)), "FKKFFKKFFK")
        self.assertEqual(run.abba(2, 2), [("pair-2", "K"), ("pair-2", "F")])
        self.assertEqual("".join(arm for _, arm in run.abba(1, 4, "S", "sem-")), "SKKSSKKS")
        self.assertEqual(run.abba(1, 1, "S", "sem-")[0][0], "sem-pair-1")
        self.assertEqual(list(run.agents("light")), ["A3", "A4"])

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

    def test_scrub(self):
        text = "/h/u/dev/repo/evidence /h/u/bench.noindex/vscode /h/u/bin/coreslot /h/u/.cache /h/u2"
        self.assertEqual(run.scrub(text, "/h/u/dev/repo", "/h/u/bench.noindex", "/h/u/bin", "/h/u"),
                         "<repo>/evidence <stand>/vscode <bin>/coreslot ~/.cache /h/u2")

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

    def test_orders(self):
        lines = self.dry("--stagger-s", "1", "set", "S", "--slots", "1", "--order", "A3,A4,A1,A2", "--tag", "sem-pair-1")
        self.assertEqual([x.split()[2] + x.split()[5] for x in lines if x.startswith("sem-pair-1")],
                         ["A3+0.0", "A3+0.0", "A4+1.0", "A1+2.0", "A2+3.0"])
        self.assertTrue(all("sem.py --fifo" in x for x in lines if x.startswith("sem-pair-1")))
        p = self.dry("--mix", "light", "pairs", "--prefix", "light-", "--last", "2", "--orders", "A4,A3;A3,A4")
        self.assertEqual([x.split()[2] for x in p if x.startswith("light-pair-2 K")], ["A3", "A3", "A4"])
        for bad in (["set", "F", "--order", "A1,A2,A3", "--tag", "pilot"],
                    ["pairs", "--last", "2", "--orders", "A1,A2,A3,A4"]):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                run.main(["--dry", "--smoke", "--coreslot", "c", "--stand", "s", "--out", "o", *bad])

    def test_bad_tag(self):
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            run.main(["--dry", "--coreslot", "c", "--stand", "s", "--out", "o", "set", "F", "--tag", "x"])


if __name__ == "__main__":
    unittest.main()
