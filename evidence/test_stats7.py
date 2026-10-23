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


if __name__ == "__main__":
    unittest.main()
