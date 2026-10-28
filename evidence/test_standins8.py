"""Tests of standins8.py: python3 -m unittest evidence/test_standins8.py"""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import standins8  # noqa: E402

TARGET = {"wall_s": [16.427, 78.936], "cores": [0.364, 1.174], "mem_mib": [290.438, 546.327],
          "p50": {"wall_s": 29.651, "cores": 0.614, "mem_mib": 437.062}}
RULE = {"wall_s_min": 780, "wall_s_max": 1800, "cores": [8.0, 9.5],
        "mem_mib_max_share_cap": 4038.323, "behind_class_mem_p90_mib": 853.869, "behind_agents": 3}


def fig(wall=30.0, cores=0.6, mem=400.0, peak=None, ratio=1.0):
    return {"failed": False, "wall_s": wall, "cores": cores, "mem_mib": mem,
            "peak_mib": mem if peak is None else peak, "ratio": ratio}


def recs(gate, *walls, run0=None):
    out = [{"gate": gate, "run": i + 1, "ran_s": w, "cores": 0.5, "max_rss_kib": 1024,
            "mem_peak_kib": 2048, "failed": False} for i, w in enumerate(walls)]
    return out + ([] if run0 is None else [dict(out[0], run=0, ran_s=run0)])


class Bounds(unittest.TestCase):
    def test_each_band_edge_is_inside_and_a_hair_past_it_is_out(self):
        for key in ("wall_s", "cores", "mem_mib"):
            for edge, step in ((TARGET[key][0], -1e-3), (TARGET[key][1], 1e-3)):
                f = fig(**{{"wall_s": "wall", "mem_mib": "mem"}.get(key, key): edge})
                self.assertTrue(standins8.bounds(f, TARGET, 1.15)[key], (key, edge))
                f[key] = edge + step
                self.assertFalse(standins8.bounds(f, TARGET, 1.15)[key], (key, edge + step))

    def test_repeat_ratio_passes_at_1_15_and_fails_just_above(self):
        f = standins8.figures(recs("g", 100.0, 115.0, 110.0), "g")
        self.assertTrue(standins8.bounds(f, TARGET | {"wall_s": [1, 999]}, 1.15)["repeat"])
        f = standins8.figures(recs("g", 100.0, 115.02, 110.0), "g")
        self.assertFalse(standins8.bounds(f, TARGET | {"wall_s": [1, 999]}, 1.15)["repeat"])

    def test_batch_memory_cap_comes_from_h_quiet(self):
        f = fig(wall=1000, cores=9.0, mem=3500, peak=3400)
        self.assertTrue(all(standins8.batch_bounds(f, RULE, 1.15, 9000).values()))
        b = standins8.batch_bounds(f, RULE, 1.15, 6000)  # cap 6000 - 3 x 853.869 = 3438.393
        self.assertEqual((b["mem_mib"], b["peak_mib"]), (False, True))
        self.assertFalse(standins8.batch_bounds(fig(1000, 9.0, 3000, 4100), RULE, 1.15, 9000)["peak_mib"])


class Gates(unittest.TestCase):
    def test_each_pick_comes_before_its_candidates_and_the_batch_is_long_batchs_pick(self):
        stand = {"classes": {"py_db": {"gate": "p"}}, "batch": {"gate": "b"},
                 "candidates": {"long_batch": [{"gate": "b2"}], "py_db": [{"gate": "p2"}]}}
        self.assertEqual([(c, g["gate"]) for c, g in standins8.gates(stand)],
                         [("py_db", "p"), ("py_db", "p2"), ("long_batch", "b"), ("long_batch", "b2")])

    def test_agent_and_worker_count_are_substituted(self):
        cwd, argv = standins8.expand({"cwd": "/r/{agent}/x", "free_n": 3, "argv": ["-n", "{n}", "a {n}"]})
        self.assertEqual((cwd, argv), ("/r/A1/x", ["-n", "3", "a 3"]))


class Figures(unittest.TestCase):
    def test_warm_up_run_is_excluded_from_the_medians(self):
        self.assertEqual(standins8.figures(recs("g", 20, 22, 21, run0=90), "g")["wall_s"], 21)
        self.assertIsNone(standins8.figures(recs("g", 20, 22, run0=90), "g"))

    def test_a_failed_run_fails_the_gate(self):
        rs = recs("g", 20, 22, 21)
        rs[1]["failed"] = True
        f = standins8.figures(rs, "g")
        self.assertEqual(standins8.bounds(f, TARGET, 1.15), {"runs_ok": False})


class Choose(unittest.TestCase):
    def rows(self, *figs):
        return [(f"g{i}", f, standins8.bounds(f, TARGET, 1.15)) for i, f in enumerate(figs)]

    def test_out_of_tolerance_takes_fewest_misses_then_nearest_p50(self):
        rows = self.rows(fig(wall=90, mem=600), fig(wall=80), fig(wall=100))
        self.assertEqual(standins8.choose(rows, TARGET["p50"]), ("g1", "out_of_tolerance", ["wall_s"]))
        rows = self.rows(fig(wall=90, mem=600), fig(wall=79), fig(cores=0.3))
        self.assertEqual(standins8.choose(rows, TARGET["p50"])[0], "g2")

    def test_a_passing_candidate_wins_and_fewer_than_three_is_none(self):
        self.assertEqual(standins8.choose(self.rows(fig(wall=90), fig()), {}), ("g1", "pass", []))
        self.assertEqual(standins8.choose(self.rows(fig(wall=90)), {})[1], "none")


if __name__ == "__main__":
    unittest.main()
