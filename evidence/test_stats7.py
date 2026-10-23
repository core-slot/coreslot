"""Tests of stats7.py: python3 -m unittest evidence.test_stats7"""
import itertools
import math
import os
import random
import sys
import unittest
from statistics import NormalDist

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import stats7  # noqa: E402


class Wilcoxon(unittest.TestCase):
    def test_wilcoxon_all_negative_small(self):
        r = stats7.wilcoxon_less([-1, -2, -3, -4, -5])
        self.assertEqual((r["n"], r["w_plus"], r["wins"], r["losses"]), (5, 0, 5, 0))
        self.assertAlmostEqual(r["p"], 1 / 32, places=12)

    def test_wilcoxon_zeros_dropped(self):
        r = stats7.wilcoxon_less([0, 0, -1])
        self.assertEqual(r["n"], 1)
        self.assertAlmostEqual(r["p"], 0.5, places=12)

    def test_wilcoxon_empty_and_all_zero(self):
        self.assertEqual(stats7.wilcoxon_less([])["p"], 1.0)
        self.assertEqual(stats7.wilcoxon_less([0, 0])["p"], 1.0)

    def test_wilcoxon_ties_average_ranks(self):
        # |d| = 1, 1, 1 share the rank 2, so the one positive gives W+ = 2
        self.assertEqual(stats7.wilcoxon_less([-1, -1, 1])["w_plus"], 2)

    def test_wilcoxon_exact_matches_brute_force(self):
        rnd = random.Random(7)
        for _ in range(30):
            n = rnd.randint(4, 12)
            d = [rnd.choice((-1, 1)) * rnd.choice((1, 2, 3, 4, 5)) for _ in range(n)]
            order = sorted(range(n), key=lambda i: abs(d[i]))
            ranks, i = [0.0] * n, 0
            while i < n:
                j = i
                while j + 1 < n and abs(d[order[j + 1]]) == abs(d[order[i]]):
                    j += 1
                for k in range(i, j + 1):
                    ranks[order[k]] = (i + j) / 2 + 1
                i = j + 1
            observed = sum(r for r, x in zip(ranks, d) if x > 0)
            flips = sum(1 for signs in itertools.product((0, 1), repeat=n)
                        if sum(r for r, s in zip(ranks, signs) if s) <= observed)
            r = stats7.wilcoxon_less(d)
            self.assertAlmostEqual(r["p"], flips / 2 ** n, places=12)
            self.assertEqual(r["w_plus"], observed)

    def test_wilcoxon_normal_branch_with_ties(self):
        d = [-3, -3, -2, -2, -2, -1, -1, 1, 2, -4, -4, -5, 3, -3, -1, -2, 4, -6, -6, -1,
             -7, 2, -2, -3, -8, -4]
        self.assertEqual(len(d), 26)
        a = sorted(abs(x) for x in d)
        ranks = {}
        for v in set(a):
            first = a.index(v) + 1
            ranks[v] = first + (a.count(v) - 1) / 2
        w = sum(ranks[abs(x)] for x in d if x > 0)
        n = len(d)
        ties = sum(a.count(v) ** 3 - a.count(v) for v in set(a))
        var = n * (n + 1) * (2 * n + 1) / 24 - ties / 48
        want = NormalDist().cdf((w + 0.5 - n * (n + 1) / 4) / math.sqrt(var))
        self.assertGreater(ties, 0)
        self.assertAlmostEqual(stats7.wilcoxon_less(d)["p"], want, delta=1e-9)


class Binomial(unittest.TestCase):
    def test_binom_upper(self):
        self.assertAlmostEqual(stats7.binom_upper_p(0, 100, 0.03), 0.97 ** 100, places=12)
        self.assertAlmostEqual(stats7.binom_upper_p(1, 100, 0.03),
                               0.97 ** 100 + 100 * 0.03 * 0.97 ** 99, places=12)
        self.assertEqual(stats7.binom_upper_p(0, 0, 0.03), 1.0)


class Rules(unittest.TestCase):
    def test_iut_needs_all(self):
        r = stats7.iut({"a": 0.01, "b": 0.2}, 0.05)
        self.assertFalse(r["rejected"])
        self.assertTrue(r["components"]["a"]["rejected"])
        self.assertFalse(r["components"]["b"]["rejected"])
        self.assertTrue(stats7.iut({"a": 0.01, "b": 0.04}, 0.05)["rejected"])

    def test_holm_order_and_stop(self):
        rows = stats7.holm({"x": 0.001, "y": 0.04, "z": 0.02}, 0.05)
        self.assertEqual([r["name"] for r in rows], ["x", "z", "y"])
        for r, t in zip(rows, (0.05 / 3, 0.025, 0.05)):
            self.assertAlmostEqual(r["threshold"], t, places=12)
            self.assertTrue(r["rejected"] and r["tested"])
        rows = stats7.holm({"x": 0.001, "y": 0.06, "z": 0.02}, 0.05)
        self.assertEqual([r["name"] for r in rows], ["x", "z", "y"])
        self.assertEqual([r["rejected"] for r in rows], [True, True, False])
        rows = stats7.holm({"x": 0.001, "y": None, "z": 0.02}, 0.05)
        by = {r["name"]: r for r in rows}
        self.assertEqual([r["name"] for r in rows], ["x", "z", "y"])
        self.assertTrue(by["x"]["rejected"] and by["z"]["rejected"])
        self.assertFalse(by["y"]["rejected"] or by["y"]["tested"])
        self.assertIsNone(by["y"]["p"])
        self.assertAlmostEqual(by["y"]["threshold"], 0.05, places=12)


class Power(unittest.TestCase):
    def test_sign_power(self):
        self.assertAlmostEqual(stats7.sign_power(5, 1.0, 0.05), 1.0, places=12)
        self.assertEqual(stats7.sign_power(4, 1.0, 0.05), 0.0)
        smallest = next(m for m in range(1, 200) if stats7.sign_power(m, 0.8, 0.05) >= 0.93)
        self.assertEqual(smallest, 26)

    def test_t_power(self):
        self.assertAlmostEqual(stats7.t_power(60, 0.084, 0.183, 0.05), 0.972, delta=0.002)
        self.assertAlmostEqual(stats7.t_power(72, 0.043, 0.098, 0.05), 0.981, delta=0.002)
        self.assertEqual(stats7.t_power(60, -0.01, 0.183, 0.05), 0.0)
        self.assertEqual(stats7.t_power(60, 0.05, 0.0, 0.05), 0.0)


if __name__ == "__main__":
    unittest.main()
