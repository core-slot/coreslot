"""Tests of stats6.py: python3 -m unittest evidence/test_stats6.py"""
import math
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import stats6  # noqa: E402


class Student(unittest.TestCase):
    def test_t_sf(self):
        # mutation: t_sf with df + 1 for df -> 0.0367 becomes 0.0354 -> red
        self.assertAlmostEqual(stats6.t_sf(2.0, 10), 0.036694, delta=1e-5)
        self.assertAlmostEqual(stats6.t_sf(1.812461, 10), 0.05, delta=1e-5)
        self.assertAlmostEqual(stats6.t_sf(-2.0, 10), 1 - 0.036694, delta=1e-5)

    def test_t_ppf(self):
        self.assertAlmostEqual(stats6.t_ppf(0.95, 10), 1.812461, delta=1e-5)

    def test_paired_upper(self):
        r = stats6.paired_upper([0.01, 0.02, -0.01, 0.0, 0.03], math.log(1.03))
        self.assertAlmostEqual(r["mean"], 0.01, places=9)
        self.assertAlmostEqual(r["se"], 0.0070711, delta=1e-7)
        self.assertAlmostEqual(r["t"], -2.76603, delta=1e-5)
        self.assertAlmostEqual(r["p"], 0.025268, delta=1e-5)
        self.assertAlmostEqual(r["upper"], 0.01 + stats6.t_ppf(0.95, 4) * r["se"], places=9)

    def test_paired_lower_mirrors(self):
        d = [0.01, 0.02, -0.01, 0.0, 0.03]
        lo = stats6.paired_lower([-x for x in d], -math.log(1.03))
        self.assertAlmostEqual(lo["p"], 0.025268, delta=1e-5)
        self.assertAlmostEqual(lo["lower"], -stats6.paired_upper(d, 0)["upper"], places=9)


class Sign(unittest.TestCase):
    def test_sign_test(self):
        self.assertAlmostEqual(stats6.sign_test(10, 2), 0.019287, delta=1e-6)
        self.assertAlmostEqual(stats6.sign_test(9, 3), 0.072998, delta=1e-6)

    def test_sign_test_all_ties(self):
        # mutation: range(wins, n) for range(wins, n + 1) in sign_test -> no pairs sum to 0 -> red
        self.assertEqual(stats6.sign_test(0, 0), 1.0)

    def test_clopper_pearson(self):
        lo, hi = stats6.clopper_pearson(0, 30)
        self.assertEqual(lo, 0.0)
        self.assertAlmostEqual(hi, 0.115703, delta=1e-5)
        lo, hi = stats6.clopper_pearson(30, 30)
        self.assertAlmostEqual(lo, 1 - 0.115703, delta=1e-5)
        self.assertEqual(hi, 1.0)


class Size(unittest.TestCase):
    def test_chi2_ppf(self):
        self.assertAlmostEqual(stats6.chi2_ppf(0.20, 11), 6.989, delta=0.05)

    def test_sigma_upper(self):
        self.assertAlmostEqual(stats6.sigma_upper(0.1, 12),
                               0.1 * math.sqrt(11 / stats6.chi2_ppf(0.20, 11)), places=12)

    def test_blocks_needed(self):
        # mutations: drop the clamp -> sigma 0.15 gives 162 -> red; drop the + 2 -> sigma 0.11
        # gives 87 and sigma 0.1046 gives 78 -> red
        self.assertEqual(stats6.blocks_needed(0.11, 30, 120, 3), 90)
        self.assertEqual(stats6.blocks_needed(0.15, 30, 120, 3), 120)
        self.assertEqual(stats6.blocks_needed(0.01, 30, 120, 3), 30)
        self.assertEqual(stats6.blocks_needed(0.1046, 30, 120, 3), 81)


class Bootstrap(unittest.TestCase):
    def test_bca_constant(self):
        self.assertEqual(stats6.bca([0.25] * 8), (0.25, 0.25))

    def test_bca_contains_mean(self):
        d = [0.05, -0.02, 0.11, 0.03, 0.08, -0.04, 0.02, 0.15, 0.01, 0.06]
        lo, hi = stats6.bca(d, reps=2000)
        self.assertLess(lo, sum(d) / len(d))
        self.assertGreater(hi, sum(d) / len(d))
        self.assertEqual(stats6.bca(d, reps=2000), (lo, hi))


if __name__ == "__main__":
    unittest.main()
