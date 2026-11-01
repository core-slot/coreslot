import os
import subprocess
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(os.sep, "nonexistent", "record")


def dry(*args):
    return subprocess.run(["bash", os.path.join(HERE, "run.sh"), "--dry", *args, "--out", OUT],
                          capture_output=True, text=True, timeout=60)


class RunDryTest(unittest.TestCase):
    def test_ramp_prints_the_pass_and_starts_nothing(self):
        r = dry("ramp", "--threads", "8", "--step-mib", "256")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn('[watch] mode = "observe", sample_log = true', r.stdout)
        self.assertIn("go build -o /tmp/coreslot-calibration.XXXXXX/coreslot ./cmd/coreslot", r.stdout)
        run = next(line for line in r.stdout.splitlines() if line.startswith("run:"))
        self.assertIn("CORESLOT_STATE_DIR=/tmp/coreslot-calibration.XXXXXX/state", run)
        self.assertIn("coreslot run --label calibration-ramp --mem 256M -- ", run)
        self.assertIn("hog.py --log " + os.path.join(OUT, "work.jsonl"), run)
        self.assertIn("--threads 8 --step-mib 256", run)
        self.assertFalse(os.path.exists(OUT))
        self.assertFalse(os.path.exists("/tmp/coreslot-calibration.XXXXXX"))

    def test_control_runs_the_busy_loops(self):
        r = dry("control", "--procs", "4", "--seconds", "30")
        self.assertEqual(r.returncode, 0, r.stderr)
        run = next(line for line in r.stdout.splitlines() if line.startswith("run:"))
        self.assertIn("--label calibration-control", run)
        self.assertIn("cpu_control.py", run)
        self.assertIn("--seconds 30 --procs 4", run)

    def test_usage_error_exits_64(self):
        r = subprocess.run(["bash", os.path.join(HERE, "run.sh"), "--dry", "bogus"], capture_output=True, text=True, timeout=60)
        self.assertEqual(r.returncode, 64)
        self.assertIn("run.sh [--dry] ramp", r.stderr)


if __name__ == "__main__":
    unittest.main()
