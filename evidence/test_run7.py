"""Tests of run7.py: python3 -m unittest evidence/test_run7.py"""
import copy
import json
import os
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import acceptance  # noqa: E402
import run4  # noqa: E402
import run6  # noqa: E402
import run7  # noqa: E402


def text(path):
    with open(path) as f:
        return f.read()


def gate(name, heavy=True):
    return {"gate": name, "label": f"bench:{name}", "cwd": "/", "heavy": heavy, "argv": [name]}


def stand(root):
    return {"coreslot": "c", "state_dir": os.path.join(root, "state"), "window_s": 5,
            "agents": {"A1": [gate("tc")], "A2": [gate("lint", False), gate("tc")]},
            "wide_agents": {"W1": [gate("build")], "W2": [gate("tc")]}}


GATES = ("tc", "lint", "build")
PASS1 = [("run", g, form) for g in GATES for form in ["K"] * 6 + ["F"] * 3]
RELEARN = [("run", g, "K") for g in GATES for _ in range(5)]


class Stand(unittest.TestCase):
    def test_stand7_adds_loadgroup(self):
        # mutation: append "--dist loadgroup" to fastapi-ruff too -> the stands differ -> red
        s4 = json.loads(text(os.path.join(HERE, "overload", "stand4.json")))
        s7 = json.loads(text(os.path.join(HERE, "overload", "stand7.json")))
        self.assertEqual(s7.pop("wide_agents"), {})
        tests = next(g for g in s7["agents"]["A3"] if g["gate"] == "fastapi-tests")
        self.assertEqual(tests["argv"][-2:], ["--dist", "loadgroup"])
        back = copy.deepcopy(s7)
        next(g for g in back["agents"]["A3"] if g["gate"] == "fastapi-tests")["argv"][-2:] = []
        self.assertEqual(back, s4)


class Learn(unittest.TestCase):
    """A temporary ROOT holding the stand's state directory and an OUT; coreslot, the measured
    run and the cooldown are fakes that log the order of calls."""

    def setUp(self):
        self.root = tempfile.mkdtemp()
        self.cfg, self.out = stand(self.root), os.path.join(self.root, "out")
        self.calls, self.cooldowns = [], []

        def measured(cfg, g, form):
            self.calls.append(("run", g["gate"], form))
            return 0, "", 2_000_000, 4.5, 60.0

        def cs(cfg, *args, **kw):
            self.calls.append(("suggest",) if args[0] == "suggest" else args)
            if args == ("suggest", "--write"):
                n = self.calls.count(("suggest",))
                with open(os.path.join(cfg["state_dir"], "needs.toml"), "w") as f:
                    f.write(f'[needs]\n  [needs."bench:tc"]\n    cpu = "{n}"\n    mem = "8G"\n')
            return subprocess.CompletedProcess(args, 0, "", "")

        def cooldown(args, baseline):
            self.cooldowns.append(args.cooldown)
            self.calls.append(("cool",))
            return {}
        for target, name, fake in ((run4, "ROOT", self.root), (run4, "cs", cs),
                                   (run6, "measured", measured),
                                   (acceptance, "cooldown", cooldown),
                                   (acceptance, "probe", lambda: 1.0)):
            p = mock.patch.object(target, name, fake)
            p.start()
            self.addCleanup(p.stop)

    def learn(self):
        with mock.patch("sys.stdout"):
            run7.learn(self.cfg, self.out)

    def runs(self):
        return [c for c in self.calls if c[0] == "run"]

    def write_learn(self, recs):
        os.makedirs(self.out)
        run4.fresh_state(self.cfg)
        run6.write_learn_state(self.cfg, self.out)
        with open(os.path.join(self.out, "learn.jsonl"), "w") as f:
            for gate_, form, pass_, n in recs:
                f.write(json.dumps({"gate": gate_, "label": f"bench:{gate_}", "form": form,
                                    "pass": pass_, "run": n, "exit": 0}) + "\n")

    def pass1_records(self):
        return [(g, form, "learn", n) for g in GATES for form, runs in (("K", 6), ("F", 3))
                for n in range(1, runs + 1)]

    def test_learn_order_and_relearn(self):
        # mutations: drop the relearn pass -> no runs after the first suggest -> red; learn only
        # cfg["agents"] -> build is never run -> red
        self.learn()
        self.assertEqual([c for c in self.calls if c[0] != "cool"],
                         PASS1 + [("suggest",)] + RELEARN + [("suggest",)])
        self.assertEqual(text(os.path.join(self.out, "needs-pass1.toml")).count('cpu = "1"'), 1)
        self.assertIn('cpu = "2"', text(os.path.join(self.out, "needs.toml")))
        self.assertEqual(text(os.path.join(self.cfg["state_dir"], "config.toml")),
                         '[resources]\ncpu = "140%"\nmem = "60%"\n')
        self.assertEqual(text(os.path.join(self.out, "needs.sha256")),
                         f"{run4.sha256(os.path.join(self.out, 'needs.toml'))}  needs.toml\n"
                         f"{run4.sha256(os.path.join(self.out, 'learn.jsonl'))}  learn.jsonl\n")
        recs = run4.read_jsonl(os.path.join(self.out, "learn.jsonl"))
        self.assertEqual(len(recs), len(PASS1) + len(RELEARN))
        self.assertEqual([r["pass"] for r in recs],
                         ["learn"] * len(PASS1) + ["relearn"] * len(RELEARN))
        self.assertEqual({k for k in recs[0]}, {"started", "gate", "label", "form", "pass", "run",
                                                "exit", "wall_s", "peak_kib", "cores",
                                                "failures", "ran_s"})
        self.assertEqual([r["run"] for r in recs if r["pass"] == "relearn"], [1, 2, 3, 4, 5] * 3)

    def test_learn_resumes_in_pass1(self):
        # mutation: call fresh_state whenever learn starts -> the state is reset on resume -> red
        self.write_learn([("tc", "K", "learn", n) for n in range(1, 7)]
                         + [("tc", "F", "learn", n) for n in (1, 2)])
        fresh = []
        with mock.patch.object(run4, "fresh_state", lambda cfg: fresh.append(cfg)):
            self.learn()
        self.assertEqual(fresh, [])
        self.assertEqual(self.runs(), PASS1[8:] + RELEARN)
        self.assertEqual(self.calls.count(("suggest",)), 2)

    def test_learn_resumes_in_relearn(self):
        # mutations: run suggest #1 whenever pass 1 is complete -> two suggests -> red; key the
        # resume on (gate, form, run) -> pass 1's K runs count as relearn's -> no runs -> red
        self.write_learn(self.pass1_records() + [("tc", "K", "relearn", n) for n in (1, 2)])
        self.learn()
        self.assertEqual(self.runs(), RELEARN[2:])
        self.assertEqual(self.calls.count(("suggest",)), 1)
        self.assertFalse(os.path.exists(os.path.join(self.out, "needs-pass1.toml")))
        recs = run4.read_jsonl(os.path.join(self.out, "learn.jsonl"))
        self.assertEqual([(r["gate"], r["run"]) for r in recs if r["pass"] == "relearn"],
                         [("tc", n) for n in range(1, 6)] + [(g, n) for g in ("lint", "build")
                                                            for n in range(1, 6)])

    def test_learn_resumes_after_suggest2(self):
        self.write_learn(self.pass1_records()
                         + [(g, "K", "relearn", n) for g in GATES for n in range(1, 6)])
        self.learn()
        self.assertEqual(self.runs(), [])
        self.assertLessEqual(self.calls.count(("suggest",)), 1)
        self.assertEqual(self.cooldowns, [])

    def test_learn_refuses_after_pilot(self):
        # mutation: drop the sens pattern from the refusal -> learn runs -> red
        for name in ("pilot-heavy.jsonl", "main-light.jsonl", "sens-spread.jsonl"):
            with self.subTest(name=name):
                os.makedirs(self.out, exist_ok=True)
                open(os.path.join(self.out, name), "w").close()
                with self.assertRaises(SystemExit) as cm, mock.patch("sys.stderr"):
                    self.learn()
                self.assertEqual(cm.exception.code, 2)
                self.assertEqual(self.calls, [])
                os.remove(os.path.join(self.out, name))

    def test_learn_cools_down_between_runs(self):
        # mutations: cool down once per gate -> fewer cooldowns -> red; take pacing's cooldown
        # without the minimum -> 5 s -> red
        self.cfg["cooldown_s"] = 5
        self.learn()
        self.assertEqual(len(self.cooldowns), len(PASS1) + len(RELEARN))
        self.assertEqual(set(self.cooldowns), {run7.LEARN_COOLDOWN_S})
        runs = [i for i, c in enumerate(self.calls) if c[0] == "run"]
        self.assertTrue(all(self.calls[i - 1] == ("cool",) for i in runs))


if __name__ == "__main__":
    unittest.main()
