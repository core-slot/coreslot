"""Tests of holm6.py: python3 -m unittest evidence/test_holm6.py"""
import contextlib
import io
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import holm6  # noqa: E402


def h5(*ps_per_n):
    """per_n as summary.json writes it: string keys, six rows per N."""
    return {"per_n": {str(n): {"tests": [{"p": p} for p in ps]}
                      for n, ps in enumerate(ps_per_n, 1)}}


def fixture(h1=(0.0, 0.0, 0.0), h4=(0.0, 0.0), h5_ps=((0.0,) * 6,) * 3, h3=0.0, h2=0.0,
            h2_untested=False):
    h2d = {"t": {"p": h2}}
    if h2_untested:
        h2d["untested"] = "more than one light pilot F set under pressure"
    return {"tests": {
        "H1": {"t": {"p": h1[0]}, "warn": {"p": h1[1]}, "swapout": {"p": h1[2]},
               "status": "rejected"},
        "H4": {"warn": {"p": h4[0]}, "swapout": {"p": h4[1]}, "status": "not rejected"},
        "H5": {**h5(*h5_ps), "status": "exploratory"},
        "H3": {"t": {"p": h3}, "status": "exploratory"},
        "H2": {**h2d, "status": "exploratory"}}}


class OneP(unittest.TestCase):
    def test_each_hypothesis_enters_with_its_defined_p(self):
        # mutation: H5 without the factor 6 (the smallest p of each N as it is) -> H5 0.2 -> red
        # mutation: H1 the smallest of its three for the largest -> H1 0.001 -> red
        # mutation: H5 not capped at 1 -> H5 1.2 -> red
        s = fixture(h1=(0.001, 0.003, 0.002), h4=(0.002, 0.11),
                    h5_ps=((0.5, 0.9, 0.01, 1.0, 1.0, 0.3), (0.2,) * 6, (1e-9, 0.5, 1, 1, 1, 1)),
                    h3=0.0006, h2=0.0002)
        r = holm6.analyse(s)["hypotheses"]
        self.assertEqual(r["H1"]["p"], 0.003)
        self.assertEqual(r["H4"]["p"], 0.11)
        # N = 1: 6 x 0.01; N = 2: 6 x 0.2 capped at 1; N = 3: 6e-9; the largest over N
        self.assertEqual(r["H5"]["p"], 1.0)
        self.assertEqual(r["H3"]["p"], 0.0006)
        self.assertEqual(r["H2"]["p"], 0.0002)

    def test_h5_takes_the_largest_over_n(self):
        # mutation: H5 the smallest over N for the largest -> 6e-9 -> red
        s = fixture(h5_ps=((0.001,) * 6, (0.002,) * 6, (1e-9,) * 6))
        self.assertAlmostEqual(holm6.analyse(s)["hypotheses"]["H5"]["p"], 0.012, places=12)


class StepDown(unittest.TestCase):
    def test_four_hypotheses_when_h2_is_untested(self):
        # mutation: Bonferroni instead of the step-down (every p against 0.05 / k) -> only H1
        #   rejected -> red
        # mutation: an untested H2 kept in the family -> k 5, H2 present -> red
        s = fixture(h1=(0.001, 0.001, 0.001), h4=(0.014, 0.0), h5_ps=((0.02 / 6,) * 6,) * 3,
                    h3=0.04, h2=1e-9, h2_untested=True)
        r = holm6.analyse(s)
        self.assertEqual(r["k"], 4)
        self.assertNotIn("H2", r["hypotheses"])
        self.assertIn("H2", r["left_out"])
        self.assertEqual({n: h["rejected"] for n, h in r["hypotheses"].items()},
                         {"H1": True, "H4": True, "H5": True, "H3": True})
        self.assertAlmostEqual(r["hypotheses"]["H4"]["threshold"], 0.05 / 3, places=12)

    def test_stops_at_the_second_hypothesis(self):
        # mutation: no stop (each p against its own threshold only) -> H3 0.014 < 0.05 / 3,
        #   H2 0.015 < 0.05 / 2 and H5 0.016 < 0.05 rejected -> red
        s = fixture(h1=(0.001, 0.001, 0.001), h4=(0.013, 0.0), h5_ps=((0.016 / 6,) * 6,) * 3,
                    h3=0.014, h2=0.015)
        r = holm6.analyse(s)["hypotheses"]
        self.assertEqual({n: h["rejected"] for n, h in r.items()},
                         {"H1": True, "H4": False, "H3": False, "H2": False, "H5": False})
        self.assertEqual(r["H4"]["rank"], 2)

    def test_writes_the_labelled_outputs(self):
        with tempfile.TemporaryDirectory() as out:
            with open(os.path.join(out, "summary.json"), "w") as f:
                json.dump(fixture(h4=(0.2, 0.0)), f)
            with contextlib.redirect_stdout(io.StringIO()):
                holm6.main(["holm6.py", out])
            with open(os.path.join(out, "holm.json")) as f:
                r = json.load(f)
            with open(os.path.join(out, "holm.txt")) as f:
                text = f.read()
        self.assertEqual(r["label"], holm6.LABEL)
        self.assertIn(holm6.LABEL, text)
        self.assertFalse(r["hypotheses"]["H4"]["rejected"])
