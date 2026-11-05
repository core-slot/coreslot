import contextlib
import io
import json
import os
import re
import shutil
import tempfile
import unittest

import analyse

DATA = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata")
S = 10**9


def fired_in(records, points, gate=1000):
    """Samples per band with E at or over points and the gate open, as the red rule counts them."""
    return analyse.by_band([s for r in records for s in r.samples if s["E"] >= points and analyse.gate_open(s, gate)])


def sample(t):
    return {"t_ns": t * S, "E": 0.0, "paging": 0, "band": "full"}


# The fixture's watch.jsonl was logged by a detector at these values.
FIXTURE = {**analyse.CHOSEN, "yellow_points": 10, "red_points": 30, "red_samples": 10, "clear_samples": 15}


def at(**kv):
    return {**analyse.CHOSEN, **kv}


class AnalyseTest(unittest.TestCase):
    def setUp(self):
        self.ramp = analyse.Record(os.path.join(DATA, "ramp"))
        self.control = analyse.Record(os.path.join(DATA, "control"))
        self.both = [self.ramp, self.control]

    def test_band_table(self):
        self.assertEqual(analyse.by_band(self.ramp.samples), {"full": 7, "slowing": 4, "slow": 4, "cliff": 3, "control": 0})
        self.assertEqual(analyse.by_band(self.control.samples), {"full": 0, "slowing": 0, "slow": 0, "cliff": 0, "control": 11})
        self.assertEqual(self.ramp.baseline, 6_000_000)
        rows = {line.split()[0]: line.split() for line in analyse.band_table(self.ramp.samples, 1000)[1:]}
        self.assertEqual(rows["slow"][1:], ["4", "19.5", "22.0", "22.0", "75%", "4000"])
        self.assertEqual(rows["cliff"][1:], ["3", "55.0", "62.0", "62.0", "100%", "-900"])

    def test_gap_and_post_stop_samples_are_not_in_a_band(self):
        self.assertEqual(sum(analyse.by_band(self.ramp.samples).values()), 18)  # 26 lines: 1 gap, 7 after the stop

    def test_red_points_30_fires_in_the_cliff_band_only(self):
        self.assertEqual(fired_in(self.both, 30), {"full": 0, "slowing": 0, "slow": 0, "cliff": 3, "control": 0})
        self.assertEqual(fired_in(self.both, 20)["slow"], 2)  # 20 already fires before the cliff
        self.assertEqual(fired_in(self.both, 10)["slowing"], 0)  # E 12 with the gate shut is not a hit

    def test_report_names_the_pre_cliff_firing(self):
        text = analyse.report(self.both)
        row = next(line for line in text.splitlines() if line.split()[:1] == ["30"] and "cliff 1/1" in line)
        self.assertIn("no pre-cliff", row)
        row = next(line for line in text.splitlines() if line.split()[:1] == ["20"] and "cliff 1/1" in line)
        self.assertIn("pre-cliff in ramp:slow", row)

    def test_a_slow_hog_is_not_a_cliff_without_paging(self):
        self.assertEqual(analyse.band_of(0.2, 0), "slow")  # cache misses slow a single thread
        self.assertEqual(analyse.band_of(0.4, 3000), "slow")
        self.assertEqual(analyse.band_of(0.2, 3000), "cliff")
        self.assertEqual(analyse.band_of(0.8, 150_000), "cliff")  # heavy decompression needs no slowdown
        self.assertEqual([analyse.band_of(x, 0) for x in (0.95, 0.7)], ["full", "slowing"])

    def test_red_bucket_leaks_and_saturates(self):
        hit = dict(sample(0), E=50.0, paging=5000)
        miss = dict(hit, E=0.0)
        self.assertIsNone(analyse.first_red([hit, miss, miss, hit, miss, miss, hit], at(red_samples=2)))
        self.assertIsNotNone(analyse.first_red([hit, miss, hit, miss, hit], at(red_samples=2)))
        levels = analyse.replay([hit] * 15 + [miss], at())
        self.assertEqual(levels[14:], ["red", "yellow"])  # a saturated bucket leaves red on the first calm sample

        seq = self.ramp.samples
        self.assertEqual(analyse.first_red(seq, at(red_samples=3))["band"], "cliff")
        self.assertIsNone(analyse.first_red(seq, at(red_samples=4)))
        self.assertEqual(analyse.first_red(seq, at(red_points=20, red_samples=2))["band"], "slow")
        self.assertIsNone(analyse.first_red(self.control.samples, at(red_points=5, red_samples=2)))

    def test_chosen_matches_the_detector(self):
        """The replay's red_samples and clear_samples are the detector's own defaults."""
        with open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "internal", "watch", "watch.go")) as f:
            go = f.read()
        self.assertEqual(int(re.search(r"DefaultRedSamples = (\d+)", go).group(1)), analyse.CHOSEN["red_samples"])
        self.assertEqual(int(re.search(r"clearSamples\s+= (\d+)", go).group(1)), analyse.CHOSEN["clear_samples"])

    def test_yellow_needs_two_of_three_and_clears_after_fifteen(self):
        hot = dict(sample(0), E=50.0, paging=5000)
        calm = dict(sample(0), E=0.0)
        self.assertEqual(set(analyse.replay([calm] * 3 + [hot] + [calm] * 30, at())), {"green"})  # one is not two
        levels = analyse.replay([hot, hot] + [calm] * 16, at())
        self.assertEqual(levels.index("green", 2), 2 + 15)  # the first calm one still sees two hot ones
        gap = dict(calm, level="unknown")
        self.assertEqual(analyse.replay([hot, gap, hot], at())[1:], ["unknown", "yellow"])

    def test_replay_is_checked_against_the_logged_level(self):
        self.assertEqual(analyse.mismatches(self.ramp, FIXTURE), [])
        with tempfile.TemporaryDirectory() as d:
            for name in ("watch.jsonl", "work.jsonl"):
                with open(os.path.join(DATA, "ramp", name)) as f:
                    lines = f.read().splitlines()
                if name == "watch.jsonl":
                    lines[0] = lines[0].replace('"level":"green"', '"level":"yellow"')
                with open(os.path.join(d, name), "w") as f:
                    f.write("\n".join(lines) + "\n")
            self.assertEqual(analyse.mismatches(analyse.Record(d), FIXTURE), [(0.0, "green", "yellow")])

    def test_calm_comes_from_the_logged_level_and_a_torn_last_line(self):
        self.assertIsNone(analyse.calm_after_stop(self.ramp))  # still yellow when the record ends
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "work.jsonl")
            with open(path, "w") as f:
                f.write('{"event":"start","t_ns":1}\n\n{"event":"rate","t_')
            self.assertEqual(analyse.read_jsonl(path), [{"event": "start", "t_ns": 1}])

    def test_calm_is_scored_on_the_samples_after_the_stop_and_s0_comes_from_the_meta(self):
        def line(t, level):
            return {"t_ns": t * S, "level": level, "excess": 0.0, "room_mib": 0, "jobs": []}

        with tempfile.TemporaryDirectory() as d:
            work = [{"t_ns": 0, "event": "start"}, {"t_ns": S, "event": "rate", "hot_mib": 512, "touches_s": 100},
                    {"t_ns": 10 * S, "event": "stop", "reason": "signal"}]
            watch = [line(t, "red" if t < 14 else "green") for t in range(2, 40, 2)]
            for name, rows in (("work.jsonl", work), ("watch.jsonl", watch), ("meta.json", [{"warmup_share": 0.0123}])):
                with open(os.path.join(d, name), "w") as f:
                    f.write("\n".join(json.dumps(r) for r in rows) + "\n")
            rec = analyse.Record(d)
            self.assertEqual(analyse.calm_after_stop(rec), (4.0, 24.0))  # green from 14 s, 4 s after the stop, to 38 s
            self.assertIn("S0 learned 0.012 by a warm-up", analyse.report([rec]))
        self.assertNotIn("S0", analyse.report([self.ramp]))  # no learned share in the meta: nothing is said of one

    def test_a_control_names_the_s0_its_warm_up_taught(self):
        with tempfile.TemporaryDirectory() as d:
            for name in ("watch.jsonl", "work.jsonl"):
                shutil.copy(os.path.join(DATA, "control", name), d)
            with open(os.path.join(d, "meta.json"), "w") as f:
                json.dump({"kind": "control", "warmup_share": 0.0456}, f)
            rec = analyse.Record(d)
            self.assertTrue(rec.is_control)
            line = next(x for x in analyse.report([rec]).splitlines() if x.startswith("=="))
            self.assertTrue(line.endswith("control, S0 learned 0.046 by a warm-up"), line)
        self.assertNotIn("S0", analyse.report([self.control]))

    def test_main_searches_a_directory_of_records(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            analyse.main([DATA, "--red-points", "20"])
        self.assertIn("== ramp (fixture, fixture, 1 threads)", out.getvalue())
        self.assertIn("red_samples: band of the first red per run (red_points=20)", out.getvalue())


if __name__ == "__main__":
    unittest.main()
