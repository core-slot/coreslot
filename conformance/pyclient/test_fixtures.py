"""The independent client decodes every wire fixture and agrees with its expected validity.

Fixtures: internal/wire/testdata/ (layout in its README.md). Run from this directory:
    python3 -m unittest -v test_fixtures
"""

from __future__ import annotations

import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import coreslot_client as cc  # noqa: E402

TESTDATA = os.path.join(HERE, "..", "..", "internal", "wire", "testdata")
DECODERS = {"ticket": cc.decode_ticket, "hold": cc.decode_hold, "cancel": cc.decode_cancel}


def _read(*parts) -> bytes:
    with open(os.path.join(TESTDATA, *parts), "rb") as fh:
        return fh.read()


def _json(*parts):
    return json.loads(_read(*parts))


class BodyFixtures(unittest.TestCase):
    def test_every_body_fixture(self):
        seen = {"ok": 0, "bad": 0}
        for kind, decode in DECODERS.items():
            d = os.path.join(TESTDATA, "bodies", kind)
            names = sorted(os.listdir(d))
            bodies = [n for n in names if n.endswith(".body")]
            self.assertTrue(bodies, f"no {kind} fixtures")
            for n in names:  # every file belongs to a body fixture
                stem = n.split(".", 1)[0]
                self.assertIn(stem + ".body", bodies, f"{kind}/{n} has no .body")
            for n in bodies:
                slug = n[: -len(".body")]
                raw = _read("bodies", kind, n)
                with self.subTest(kind=kind, fixture=slug):
                    if slug.startswith("ok-"):
                        seen["ok"] += 1
                        try:
                            got = decode(raw)
                        except cc.Malformed as exc:
                            self.fail(f"refused a valid body: {exc}")
                        self.assertEqual(got, _json("bodies", kind, slug + ".expect.json"))
                    elif slug.startswith("bad-"):
                        seen["bad"] += 1
                        self.assertTrue(os.path.exists(os.path.join(d, slug + ".why")))
                        with self.assertRaises(cc.Malformed,
                                               msg=_read("bodies", kind, slug + ".why").decode()):
                            decode(raw)
                    else:
                        self.fail(f"fixture name {n} states no verdict")
        self.assertGreater(seen["ok"], 0)
        self.assertGreater(seen["bad"], 0)
        print(f"\n  body fixtures: {seen['ok']} ok, {seen['bad']} bad", file=sys.stderr)


class NameFixtures(unittest.TestCase):
    def setUp(self):
        self.names = _json("names.json")

    def test_valid_ticket_and_hold_names(self):
        for case in self.names["valid"]:
            with self.subTest(name=case["name"]):
                got = cc.parse_name(case["name"])
                self.assertIsNotNone(got)
                self.assertEqual(got, (int(case["order_key"]), case["pid"], case["seq"], case["display"]))
                self.assertEqual(case["name"][:20], case["order_key"])

    def test_invalid_ticket_and_hold_names(self):
        for case in self.names["invalid"]:
            with self.subTest(name=case["name"], why=case["why"]):
                self.assertIsNone(cc.parse_name(case["name"]))

    def test_cancel_names(self):
        for case in self.names["cancel_valid"]:
            with self.subTest(name=case["name"]):
                self.assertNotIn("display", case)
                self.assertEqual(cc.parse_cancel_name(case["name"]),
                                 (int(case["order_key"]), case["pid"], case["seq"]))
        for case in self.names["cancel_invalid"]:
            with self.subTest(name=case["name"], why=case["why"]):
                self.assertIsNone(cc.parse_cancel_name(case["name"]))


class OrderAndDisplayFixtures(unittest.TestCase):
    def test_order_keys(self):
        for case in _json("order_keys.json"):
            with self.subTest(**case):
                self.assertEqual(f"{cc.order_key(case['arrival_ns'], case['priority']):020d}",
                                 case["order_key"])

    def test_display_derivation_matches_go(self):
        # Informational (PROTOCOL.md §7.1): a different derivation would still conform.
        for case in _json("display.json"):
            with self.subTest(label=case["label"]):
                self.assertEqual(cc.display_of(case["label"]), case["display"])


if __name__ == "__main__":
    unittest.main()
