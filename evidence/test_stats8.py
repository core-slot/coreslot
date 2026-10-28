"""Tests of stats8.py: python3 -m unittest evidence.test_stats8"""
import math
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import stats6  # noqa: E402
import stats7  # noqa: E402
import stats8  # noqa: E402

LN103 = math.log(1.03)


class TPowerBelow(unittest.TestCase):
    def test_power_superiority_hand_value(self):
        # mutation: use the (1 - alpha) quantile with the wrong sign (+z instead of -z) -> red
        # n 16, mean -0.1, sigma 0.2: z = 0.1 / 0.05 = 2; power = Phi(2 - 1.644854) = 0.63876
        self.assertAlmostEqual(stats8.t_power_below(16, -0.1, 0.2, 0.0, 0.05), 0.63876, places=5)

    def test_power_noninferiority_bound(self):
        # mutation: ignore the bound (always compare against 0) -> red
        # mean 0, bound ln 1.03, se 0.04 / 4: Phi(0.029559 / 0.01 - 1.644854) = 0.90508
        self.assertAlmostEqual(stats8.t_power_below(16, 0.0, 0.04, LN103, 0.05), 0.90508, places=5)

    def test_power_agrees_with_stats7_t_power(self):
        # mutation: shift the statistic by a constant -> red (the mirror of t_power on -d)
        for n in (4, 16, 50):
            self.assertAlmostEqual(stats8.t_power_below(n, -0.07, 0.15, 0.0, 0.05),
                                   stats7.t_power(n, 0.07, 0.15, 0.05), places=12)

    def test_power_at_or_above_bound_is_zero(self):
        # mutation: return alpha (the size) instead of 0 when mean >= bound -> red
        self.assertEqual(stats8.t_power_below(16, 0.0, 0.2, 0.0, 0.05), 0.0)
        self.assertEqual(stats8.t_power_below(16, 0.1, 0.2, 0.0, 0.05), 0.0)

    def test_power_sigma_zero_edges(self):
        # mutation: divide by sigma without the zero guard (ZeroDivisionError) or return 0 -> red
        self.assertEqual(stats8.t_power_below(16, -0.1, 0.0, 0.0, 0.05), 1.0)
        self.assertEqual(stats8.t_power_below(16, 0.1, 0.0, 0.0, 0.05), 0.0)
        self.assertEqual(stats8.t_power_below(16, 0.0, 0.0, 0.0, 0.05), 0.0)


class NFor(unittest.TestCase):
    COMP = (-0.1, 0.2, 0.0, 0.05)

    def test_smallest_n_reaching_the_target(self):
        # mutation: return the first n above the target (n + 1) -> red
        # power(24) = 0.7896 < 0.80 <= power(25) = 0.8037
        self.assertEqual(stats8.n_for(0.80, [self.COMP], 2, 200), 25)

    def test_lo_floor_and_hi_cap(self):
        # mutation: clamp to lo/hi the other way round -> red
        self.assertEqual(stats8.n_for(0.80, [self.COMP], 30, 200), 30)
        self.assertEqual(stats8.n_for(0.80, [self.COMP], 2, 20), 20)

    def test_every_component_must_reach_it(self):
        # mutation: any() instead of all() -> red
        strong = (-0.5, 0.2, 0.0, 0.05)
        self.assertEqual(stats8.n_for(0.80, [strong, self.COMP], 2, 200), 25)
        self.assertLess(stats8.n_for(0.80, [strong], 2, 200), 25)

    def test_a_component_that_never_reaches_gives_hi(self):
        # mutation: skip components whose power stays 0 -> red
        dead = (0.1, 0.2, 0.0, 0.05)
        self.assertEqual(stats8.n_for(0.80, [self.COMP, dead], 2, 60), 60)


def grid():
    comp = (-0.1, 0.2, 0.0)
    return {"trace-quiet": {"comps": [comp], "set_s": 300},
            "trace-loaded": {"comps": [comp], "set_s": 300},
            "heavy-loaded": {"comps": [comp], "set_s": 300},
            "wide-quiet": {"comps": [comp], "set_s": 300},
            "batch": {"comps": [comp] * 3, "set_s": 300}}


def plan(cap_h, cells=None):
    return stats8.power_plan(
        cells or grid(), primary=("trace-quiet", "trace-loaded"), batch="batch",
        drop_order=("heavy-loaded", "wide-quiet", "trace-loaded"), cap_h=cap_h,
        power_min=0.80, lo=2, hi=200, alpha=0.05)


class PowerPlan(unittest.TestCase):
    def test_a_fits_without_drops(self):
        # mutation: use power_min instead of power_min ** (1 / m) for a primary cell -> red
        p = plan(20)
        # m = 2: target 0.8 ** 0.5 = 0.8944, n = 34; batch target 0.8 ** (1/3), n = 39
        self.assertEqual(p["dropped"], [])
        self.assertEqual(p["n"], {"trace-quiet": 34, "trace-loaded": 34, "heavy-loaded": 34,
                                  "wide-quiet": 34, "batch": 39})
        self.assertAlmostEqual(p["hours"], (4 * 34 * 300 + 39 * 300) / 3600, places=9)
        self.assertEqual(p["scale"], 1.0)
        self.assertGreaterEqual(p["joint"], 0.8)
        self.assertFalse(p["descriptive"])
        self.assertEqual(len(p["power"]["batch"]), 3)
        self.assertAlmostEqual(
            p["joint"], p["power"]["trace-quiet"][0] * p["power"]["trace-loaded"][0], places=12)

    def test_b_one_drop_in_order(self):
        # mutation: drop the largest cell (or the first primary) instead of the next in order -> red
        p = plan(12.5)
        self.assertEqual(p["dropped"], ["heavy-loaded"])
        self.assertNotIn("heavy-loaded", p["n"])
        self.assertEqual(p["n"]["wide-quiet"], 34)
        self.assertAlmostEqual(p["hours"], (3 * 34 * 300 + 39 * 300) / 3600, places=9)
        self.assertEqual(p["scale"], 1.0)

    def test_c_all_drops_then_scaling_is_descriptive(self):
        # mutation: leave the batch n unscaled-check out and scale it too, or floor with round -> red
        p = plan(3.9)
        self.assertEqual(p["dropped"], ["heavy-loaded", "wide-quiet", "trace-loaded"])
        # trace-quiet alone, m = 1: n 25; scale (3.9 * 3600 - 39 * 300) / (25 * 300) = 0.312
        self.assertAlmostEqual(p["scale"], 0.312, places=9)
        self.assertEqual(p["n"], {"trace-quiet": 7, "batch": 39})
        self.assertLessEqual(p["hours"], 3.9)
        self.assertAlmostEqual(p["joint"], p["power"]["trace-quiet"][0], places=12)
        self.assertLess(p["joint"], 0.5)
        self.assertTrue(p["descriptive"])

    def test_c2_scaling_never_goes_below_lo(self):
        # mutation: drop the max(lo, ...) floor -> red
        p = plan(3.3)
        self.assertEqual(p["n"]["trace-quiet"], 2)
        self.assertEqual(p["n"]["batch"], 39)

    def test_d_batch_never_dropped_and_own_target(self):
        # mutation: let the batch cell join the drop order or take the primary n -> red
        p = plan(0.1)
        self.assertNotIn("batch", p["dropped"])
        self.assertEqual(p["n"]["batch"], 39)
        c = grid()
        c["batch"]["comps"] = c["batch"]["comps"][:1]  # one component: target 0.80, n 25
        self.assertEqual(plan(20, c)["n"]["batch"], 25)

    def test_e_dropping_trace_loaded_changes_m(self):
        # mutation: keep m = len(primary) after a primary cell is dropped -> red
        p = plan(5.5)
        self.assertEqual(p["dropped"], ["heavy-loaded", "wide-quiet", "trace-loaded"])
        self.assertEqual(p["n"], {"trace-quiet": 25, "batch": 39})  # 25, not the m = 2 value 34
        self.assertEqual(p["scale"], 1.0)
        self.assertFalse(p["descriptive"])

    def test_secondary_cells_take_the_largest_primary_n(self):
        # mutation: size a secondary cell on its own components -> red
        c = grid()
        c["trace-loaded"]["comps"] = [(-0.2, 0.2, 0.0)]  # easier primary cell
        c["heavy-loaded"]["comps"] = [(-0.9, 0.1, 0.0)]  # trivial on its own
        p = plan(20, c)
        self.assertEqual(p["n"]["heavy-loaded"], max(p["n"]["trace-quiet"], p["n"]["trace-loaded"]))
        self.assertLess(p["n"]["trace-loaded"], p["n"]["trace-quiet"])

    def test_deterministic(self):
        # mutation: iterate a set or use random tie-breaks -> red
        self.assertEqual(plan(3.9), plan(3.9))


class Futile(unittest.TestCase):
    def test_clearly_worse_is_futile(self):
        # mutation: compare the upper instead of the lower bound -> red
        d = [0.30, 0.28, 0.33, 0.31, 0.29, 0.32]
        self.assertTrue(stats8.futile(d, LN103))

    def test_clearly_better_is_not_futile(self):
        # mutation: invert the comparison (<= bound) -> red
        d = [-0.30, -0.28, -0.33, -0.31, -0.29, -0.32]
        self.assertFalse(stats8.futile(d, LN103))

    def test_at_the_bound_is_futile(self):
        # mutation: strict > instead of >= -> red (identical differences: the lower bound is the mean)
        self.assertTrue(stats8.futile([LN103] * 5, LN103))
        self.assertFalse(stats8.futile([LN103 - 1e-9] * 5, LN103))

    def test_uses_the_90_percent_lower_bound(self):
        # mutation: use alpha 0.05 (a 95 % bound) as the default -> red
        d = [0.05, 0.01, 0.08, 0.02, 0.06, 0.03]
        lo90 = stats6.paired_lower(d, 0.0, 0.10)["lower"]
        lo95 = stats6.paired_lower(d, 0.0, 0.05)["lower"]
        self.assertGreater(lo90, lo95)
        bound = (lo90 + lo95) / 2  # the 90 % bound is above it, the 95 % bound below it
        self.assertTrue(stats8.futile(d, bound))
        self.assertFalse(stats8.futile(d, bound, alpha=0.05))

    def test_fewer_than_two_values_is_not_futile(self):
        # mutation: treat an empty or one-value sample as futile -> red
        self.assertFalse(stats8.futile([], 0.0))
        self.assertFalse(stats8.futile([5.0], 0.0))


class UnionAndIut(unittest.TestCase):
    def test_union_is_bonferroni_over_the_tested_legs(self):
        # mutation: multiply by the count including None legs -> red
        self.assertAlmostEqual(stats8.union_p([0.01, None, 0.04]), 0.02, places=12)

    def test_union_caps_at_one_and_takes_the_minimum(self):
        # mutation: drop the min(1, ...) cap or use the max leg -> red
        self.assertEqual(stats8.union_p([0.6, 0.7]), 1.0)
        self.assertAlmostEqual(stats8.union_p([0.30, 0.02, 0.5]), 0.06, places=12)

    def test_union_without_tested_leg_is_none(self):
        # mutation: return 1.0 for no tested leg -> red
        self.assertIsNone(stats8.union_p([]))
        self.assertIsNone(stats8.union_p([None, None]))

    def test_iut_is_the_max_leg(self):
        # mutation: use min instead of max -> red
        self.assertEqual(stats8.iut_p([0.01, 0.2, 0.03]), 0.2)

    def test_iut_with_a_none_leg_is_none(self):
        # mutation: skip None legs like union_p does -> red
        self.assertIsNone(stats8.iut_p([0.01, None]))
        self.assertIsNone(stats8.iut_p([None]))


if __name__ == "__main__":
    unittest.main()
