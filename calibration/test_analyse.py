import contextlib
import io
import os
import tempfile
import unittest

import analyse

DATA = os.path.join(os.path.dirname(os.path.abspath(__file__)), "testdata")
S = 10**9


def fired_in(records, points, gate=1000):
    """Samples per band with E at or over points and the gate open, as the red rule counts them."""
    return analyse.by_band([s for r in records for s in r.samples if s["E"] >= points and analyse.gate_open(s, gate)])


def sample(t, headroom, swap_used):
    return {"t_ns": t * S, "E": 0.0, "paging": 0, "headroom_mib": headroom, "swap_used_mib": swap_used, "band": "full"}


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

    def test_red_bucket_leaks(self):
        hit = dict(sample(0, 9999, 0), E=50.0, paging=5000)
        miss = dict(hit, E=0.0)
        self.assertIsNone(analyse.first_red([hit, miss, miss, hit, miss, miss, hit], 30, 2, 1000))
        self.assertIsNotNone(analyse.first_red([hit, miss, hit, miss, hit], 30, 2, 1000))

        seq = self.ramp.samples
        self.assertEqual(analyse.first_red(seq, 30, 3, 1000)["band"], "cliff")
        self.assertIsNone(analyse.first_red(seq, 30, 4, 1000))
        self.assertEqual(analyse.first_red(seq, 20, 2, 1000)["band"], "slow")
        self.assertIsNone(analyse.first_red(self.control.samples, 5, 2, 1000))

    def test_yellow_needs_two_of_three_and_clears_after_fifteen(self):
        seq = self.ramp.samples
        self.assertEqual([s["band"] for s in analyse.yellow_samples(seq, 30, 1000)], ["cliff", "cliff"])
        calm = [dict(sample(t, 9999, 0), E=0.0) for t in range(40)]
        calm[3].update(E=50.0, paging=5000)
        self.assertEqual(analyse.yellow_samples(calm, 10, 1000), [])  # one hit is not two of three

    def test_floor_is_an_edge(self):
        low = [sample(t * 2, 1000, 100 + 40 * t) for t in range(12)]
        self.assertEqual(len(analyse.floor_samples(low, 2048)), 5)  # 40 MiB per 2 s reaches 256 MiB at the 8th sample
        still = [sample(t * 2, 1000, 100) for t in range(12)]
        self.assertEqual(analyse.floor_samples(still, 2048), [])

    def test_recovery_and_a_torn_last_line(self):
        self.assertEqual(analyse.recovery_s(self.ramp), 6.0)
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "work.jsonl")
            with open(path, "w") as f:
                f.write('{"event":"start","t_ns":1}\n\n{"event":"rate","t_')
            self.assertEqual(analyse.read_jsonl(path), [{"event": "start", "t_ns": 1}])

    def test_main_searches_a_directory_of_records(self):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            analyse.main([DATA, "--red-points", "20"])
        self.assertIn("== ramp (fixture, fixture, 1 threads)", out.getvalue())
        self.assertIn("red_samples: band of the first red per run (red_points=20)", out.getvalue())


if __name__ == "__main__":
    unittest.main()
