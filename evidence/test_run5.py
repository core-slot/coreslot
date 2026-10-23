"""Tests of run5.py's pure parts: python3 -m unittest evidence/test_run5.py"""
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import run5  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
STATIC = {"cpu": 14, "mem": 76}


def sset(block, arm, warn=0, critical=0, wfree=0.0, exits=None):
    """A minimal set record: `warn` samples at level 2 and `critical` at level 4."""
    samples = ([{"t": i, "pressure": 2} for i in range(warn)]
               + [{"t": warn + i, "pressure": 4} for i in range(critical)]
               + [{"t": warn + critical, "pressure": 1}])
    return {"block": block, "arm": arm, "valid": True, "samples": samples,
            "gates": [{"agent": "A1", "gate": g, "start": 0, "end": 1, "exit": e}
                      for g, e in (exits or {"g": 0}).items()],
            "waited_free_s": wfree if arm != "S" else None}


def pairs(diffs, **kl):
    """One (KS, KL) pair per block, KL at warn_s(KS) + diff."""
    return [(sset(b, "KS", warn=20), sset(b, "KL", warn=20 + d, **kl))
            for b, d in enumerate(diffs, 1)]


class Order(unittest.TestCase):
    def test_each_order_twice(self):
        # mutation: `% (len(PERMS) - 1)` in run4.order_for uses five orders -> red (`block % 6`
        # only shifts the table: green, so block 1's order is pinned too)
        orders = ["".join(run5.order_for(b)) for b in range(1, run5.BLOCKS + 1)]
        self.assertEqual(len(set(orders)), 6)
        for o in set(orders):
            self.assertEqual(orders.count(o), 2, o)
        self.assertEqual(run5.order_for(1), ["KS", "S", "KL"])


class Price(unittest.TestCase):
    def test_median_bound(self):
        # mutation: `<` for `<=` against PRICE_WARN_S -> the 10 s case fails -> red
        ok = run5.price(pairs([9] * 12), {"g": True})
        self.assertTrue(ok["pass"], ok)
        self.assertEqual(ok["warn_diff_median_s"], 9)
        self.assertTrue(run5.price(pairs([10] * 12), {"g": True})["pass"])
        self.assertFalse(run5.price(pairs([11] * 12), {"g": True})["pass"])

    def test_one_critical_second_fails(self):
        # mutation: drop the critical-second condition from price -> this passes -> red
        ps = pairs([0] * 12)
        ps[5] = (ps[5][0], sset(6, "KL", warn=20, critical=1))
        self.assertFalse(run5.price(ps, {"g": True})["pass"])

    def test_one_false_failure_fails(self):
        # mutation: drop the false-failure condition from price -> this passes -> red
        ps = pairs([0] * 12)
        ps[3] = (ps[3][0], sset(4, "KL", warn=20, exits={"g": 1}))
        self.assertFalse(run5.price(ps, {"g": True})["pass"])
        self.assertTrue(run5.price(ps, {"g": False})["pass"])  # g failed its baseline


class Mechanism(unittest.TestCase):
    def test_tie_is_not_a_win(self):
        # mutation: `<=` in run5.mechanism's comparison -> the two ties count, 12 wins -> red
        ks = [10.0] * 12
        kl = [5.0] * 10 + [10.0, 10.0]
        m = run5.mechanism([(sset(b, "KS", wfree=a), sset(b, "KL", wfree=c))
                            for b, (a, c) in enumerate(zip(ks, kl), 1)])
        self.assertEqual((m["wins"], m["n"]), (10, 12))
        self.assertTrue(m["pass"])
        kl[0] = 10.0
        m = run5.mechanism([(sset(b, "KS", wfree=a), sset(b, "KL", wfree=c))
                            for b, (a, c) in enumerate(zip(ks, kl), 1)])
        self.assertEqual(m["wins"], 9)
        self.assertFalse(m["pass"])


def entry(label, cpu, mem, waited, ran, grant_cpu=None, wfree=0.0):
    return {"label": label, "need": {"cpu": [cpu, cpu], "mem": [mem, mem]},
            "grant": {"cpu": grant_cpu or cpu, "mem": mem}, "waited_s": waited, "ran_s": ran,
            "waited_free_s": wfree}


def two_runs(b_cpu, a_grant_cpu):
    """A type check admitted at 0 for 10 s; a test suite queued at 2 and admitted at 10."""
    return {"arm": "KS",
            "gates": [{"agent": "A1", "gate": "tc", "start": 0.0, "end": 10.0, "exit": 0},
                      {"agent": "A2", "gate": "tests", "start": 2.0, "end": 15.0, "exit": 0}],
            "runlog": [entry("bench:tc", 5, 67, 0.0, 10.0, grant_cpu=a_grant_cpu),
                       entry("bench:tests", b_cpu, 27, 8.0, 5.0, wfree=3.0)]}


class Decompose(unittest.TestCase):
    def test_two_runs(self):
        # mutation: classify by the cpu fit alone (drop `not fm`) -> M becomes head_fit -> red
        d = run5.decompose(two_runs(1, 5), STATIC)
        self.assertEqual(d["totals"]["M"], 8.0)
        self.assertEqual(d["totals"]["MC"], 0.0)
        self.assertEqual(d["above_static"], 0)
        # 10 + 5 cpu held and needed: neither fits
        d = run5.decompose(two_runs(5, 10), STATIC)
        self.assertEqual((d["totals"]["M"], d["totals"]["MC"]), (0.0, 8.0))

    def test_above_static(self):
        # mutation: leave the held grants out of above_static (27 alone is not above 76) -> red
        r = two_runs(1, 5)
        r["runlog"][1]["waited_s"] = 0.0
        r["gates"][1]["end"] = 7.0
        self.assertEqual(run5.decompose(r, STATIC)["above_static"], 1)
        r["runlog"][1]["need"]["mem"] = [1, 1]
        r["runlog"][1]["grant"]["mem"] = 1
        self.assertEqual(run5.decompose(r, STATIC)["above_static"], 0)

    def test_run4_totals(self):
        # run 4's wait decomposition on its committed K sets, as published
        # mutation: drop decompose's `head is not run` branch -> HM and HMC become latency -> red
        recs = [json.loads(l) for l in open(os.path.join(HERE, "overload", "run4",
                                                         "results.jsonl"))]
        tot = dict.fromkeys(run5.KEYS, 0.0)
        for r in recs:
            if r["arm"] == "K":
                for k, v in run5.decompose(r, STATIC)["totals"].items():
                    tot[k] += v
        self.assertEqual([round(tot[k], 1) for k in ("M", "MC", "HM", "HMC", "latency")],
                         [385.5, 326.8, 92.3, 85.1, 67.3])
        self.assertEqual((round(tot["C"], 1), round(tot["HC"], 1)), (0.0, 0.0))


if __name__ == "__main__":
    unittest.main()
