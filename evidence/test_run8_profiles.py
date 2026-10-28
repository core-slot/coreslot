"""Tests of run8-profiles.json: python3 -m unittest evidence/test_run8_profiles.py"""
import copy
import json
import os
import re
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
MACHINES = {"air", "pro"}
CLASSES = {"go_test", "py_unit", "py_db", "typecheck", "mutation", "long_batch", "other"}
TOP_KEYS = {"schema", "memory_source", "machines", "classes", "excluded", "short_runs",
            "concurrency", "long_batch_neighbours", "mix"}
PLAIN = re.compile(r"^[a-z0-9_+.\-]+$")
SCHEMA_ID = re.compile(r"^[a-z0-9-]+/[0-9]+$")
ISO_DATE = re.compile(r"^\d{4}-\d{2}-\d{2}$")


def load():
    with open(os.path.join(HERE, "run8-profiles.json"), encoding="utf-8") as f:
        return json.load(f)


def offending_strings(node, path=()):
    """Every key or string value in the document that is not a plain identifier, as (path, text).

    Allowed besides identifiers, numeric bucket keys (`6+`) and quantile keys (`p50`): the schema id
    in the top-level `schema` field and ISO dates as values.
    """
    found = []
    if isinstance(node, dict):
        for k, v in node.items():
            if not isinstance(k, str) or not PLAIN.match(k):
                found.append((path + (k,), k))
            found.extend(offending_strings(v, path + (k,)))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            found.extend(offending_strings(v, path + (i,)))
    elif isinstance(node, str):
        if path == ("schema",):
            ok = SCHEMA_ID.match(node)
        else:
            ok = PLAIN.match(node) or ISO_DATE.match(node)
        if not ok:
            found.append((path, node))
    return found


def entering(doc):
    """The short-gate classes the entry criterion admits, from the profiles' own shares."""
    mix = doc["mix"]
    return sorted(c for c, per in doc["classes"].items() if c != "long_batch" and any(
        p and (p.get("share_of_short_gate_" + by) or 0) >= mix["entry_share"]
        for p in per.values() for by in mix["entry_by"]))


class Profiles(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.doc = load()

    def test_top_level(self):
        d = self.doc
        self.assertEqual(set(d), TOP_KEYS)
        self.assertEqual(d["schema"], "coreslot-run8-profiles/2")
        self.assertEqual(d["memory_source"], "max_rss_kib")

    def test_machine_keys(self):
        d = self.doc
        keyed = {"machines": d["machines"], "short_runs": d["short_runs"],
                 "concurrency": d["concurrency"],
                 "long_batch_neighbours": d["long_batch_neighbours"],
                 "mix.uncovered_share": d["mix"]["uncovered_share"]}
        for name, m in d["classes"].items():
            keyed["classes." + name] = m
        for name, m in d["excluded"].items():
            keyed["excluded." + name] = m
        for where, m in keyed.items():
            self.assertTrue(set(m) <= MACHINES, (where, sorted(m)))
        self.assertEqual(set(d["machines"]), MACHINES)
        for name, m in d["machines"].items():
            for key in ("cpus", "mem_gib"):
                self.assertIs(type(m[key]), int, (name, key))

    def test_class_keys_only(self):
        d, mix = self.doc, self.doc["mix"]
        self.assertEqual(set(d["classes"]), CLASSES)
        used = {"mix.classes": set(mix["classes"]),
                "mix.gates_by_class": set(mix["gates_by_class"]),
                "mix.targets": set(mix["targets"]),
                "mix.long_batch.behind_class": {mix["long_batch"]["behind_class"]}}
        for machine, per in d["long_batch_neighbours"].items():
            for field, by_class in per.items():
                used["long_batch_neighbours.%s.%s" % (machine, field)] = set(by_class)
        for where, keys in used.items():
            self.assertTrue(keys <= CLASSES, (where, sorted(keys - CLASSES)))
        self.assertEqual(len(mix["classes"]), len(set(mix["classes"])))

    def test_no_label_repository_or_path(self):
        self.assertEqual(offending_strings(self.doc), [])

    def test_walker_reports_a_label_or_a_path(self):
        # mutation: let the walker skip strings inside lists, or accept ':' or '/' -> red
        bad = {"proj:unit": ("mix", "classes"), "var/lib/x": ("mix", "classes"),
               "user@host": ("mix", "classes"), "~/x": ("mix", "classes"),
               "Upper": ("mix", "classes"), "has space": ("mix", "classes")}
        for text, where in bad.items():
            d = copy.deepcopy(self.doc)
            d[where[0]][where[1]].append(text)
            self.assertIn(text, [t for _, t in offending_strings(d)], text)
        d = copy.deepcopy(self.doc)
        d["classes"]["go_test"]["air"]["proj:unit"] = 1  # a key
        self.assertIn("proj:unit", [t for _, t in offending_strings(d)])
        d = copy.deepcopy(self.doc)
        d["machines"]["air"]["first_day"] = "var/lib/x"  # a value where a date goes
        self.assertEqual([t for _, t in offending_strings(d)], ["var/lib/x"])
        d = copy.deepcopy(self.doc)
        d["mix"]["long_batch"]["behind_class"] = "py_unit:x"
        self.assertEqual([t for _, t in offending_strings(d)], ["py_unit:x"])
        # the schema id is allowed only where the schema id goes
        d = copy.deepcopy(self.doc)
        d["memory_source"] = "coreslot-run8-profiles/2"
        self.assertEqual([t for _, t in offending_strings(d)], ["coreslot-run8-profiles/2"])

    def test_mix_is_consistent(self):
        d, mix = self.doc, self.doc["mix"]
        self.assertEqual(sum(mix["gates_by_class"].values()),
                         mix["agents"] * mix["gates_per_agent"])
        for c in mix["classes"]:
            self.assertGreaterEqual(mix["gates_by_class"].get(c, 0), 1, c)
            self.assertIn(c, mix["targets"], c)
        self.assertEqual(set(mix["gates_by_class"]), set(mix["classes"]))
        self.assertEqual(set(mix["targets"]), set(mix["classes"]))
        for c, t in mix["targets"].items():
            for field in ("wall_s", "cores", "mem_mib"):
                lo, hi = t[field]
                self.assertLessEqual(lo, t["p50"][field], (c, field))
                self.assertLessEqual(t["p50"][field], hi, (c, field))
            self.assertGreaterEqual(t["cores"][0], 0, c)
        lb = mix["long_batch"]
        standin = lb["standin"]
        self.assertGreaterEqual(standin["wall_s_min"], 600)
        self.assertLessEqual(standin["wall_s_min"], standin["wall_s_max"])
        lo, hi = standin["cores"]
        self.assertGreaterEqual(lo, 0)
        self.assertLessEqual(lo, hi)
        self.assertLessEqual(hi, d["machines"]["air"]["cpus"])
        self.assertIn(lb["behind_class"], mix["classes"])

    def test_entry_follows_the_criterion(self):
        # a short-gate class enters when, on either machine, its share of the short-gate runs or of
        # their wall time reaches half of one of the mix's gates
        self.assertEqual(entering(self.doc), self.doc["mix"]["classes"])
        mix = self.doc["mix"]
        self.assertEqual(mix["entry_share"], round(1 / (2 * mix["agents"] * mix["gates_per_agent"]), 3))
        self.assertTrue(mix["entry_by"] and set(mix["entry_by"]) <= {"runs", "wall"}, mix["entry_by"])
        for machine, share in mix["uncovered_share"].items():
            left = [p[machine]["share_of_short_gate_wall"] for c, p in self.doc["classes"].items()
                    if c not in mix["classes"] and c != "long_batch" and p.get(machine)]
            self.assertAlmostEqual(share, sum(left), delta=0.002, msg=machine)

    def test_entry_check_can_fail(self):
        # mutation: a class under the share on both measures and machines, or one listed without
        # reaching it -> red
        d = copy.deepcopy(self.doc)
        for p in d["classes"]["other"].values():
            if p:
                p["share_of_short_gate_runs"] = p["share_of_short_gate_wall"] = 0.01
        self.assertNotIn("other", entering(d))
        self.assertNotEqual(entering(d), d["mix"]["classes"])

    def test_no_trace_stand_in_can_be_preempted_automatically(self):
        # coreslot v0.3.0 demotes a label with cpu max >= 7 tokens over >= 5 runs at a median of
        # >= 600 s; every trace band ends below that median, and only the batch reaches it
        rule = self.doc["mix"]["auto_preempt"]
        self.assertEqual(rule, {"cpu_tokens_min": 7, "runs_min": 5, "median_s_min": 600})
        for c, t in self.doc["mix"]["targets"].items():
            self.assertLess(t["wall_s"][1], rule["median_s_min"], c)
        self.assertGreaterEqual(self.doc["mix"]["long_batch"]["standin"]["wall_s_min"], rule["median_s_min"])

    def test_concurrency_shares_sum_to_one(self):
        for machine, per in self.doc["concurrency"].items():
            for field in ("demand", "running"):
                self.assertAlmostEqual(sum(per[field].values()), 1.0, delta=0.01,
                                       msg=(machine, field))


if __name__ == "__main__":
    unittest.main()
