import os
import subprocess
import unittest

RUN = os.path.join(os.path.dirname(os.path.abspath(__file__)), "run.sh")


class RunDryTest(unittest.TestCase):
    def dry(self, *args):
        p = subprocess.run(["bash", RUN, "--dry", *args], capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        return {line.split(":")[0]: line for line in p.stdout.splitlines() if line.startswith(("warm-up:", "run:"))}

    def test_slow_rule_reaches_the_scored_hog_only(self):
        lines = self.dry("ramp", "--slow-s", "40", "--slow-pct", "20")
        self.assertIn("--slow-s 40 --slow-pct 20", lines["run"])
        self.assertNotIn("--slow-", lines["warm-up"])

    def test_defaults_are_the_hogs(self):
        self.assertIn("--slow-s 5 --slow-pct 30", self.dry("ramp")["run"])


if __name__ == "__main__":
    unittest.main()
