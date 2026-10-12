"""Self-tests of the independent Python client, against temporary state directories.

Run: python3 -m unittest -v test_client   (from this directory; TMPDIR picks the scratch root)
"""

from __future__ import annotations

import fcntl
import shutil
import json
import os
import select
import signal
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import coreslot_client as cc  # noqa: E402

CLIENT = os.path.join(HERE, "coreslot_client.py")

# A participant in its own process: enqueue, then either hold the grant or never step again.
PARTICIPANT = textwrap.dedent("""
    import json, sys, time
    sys.path.insert(0, sys.argv[1])
    import coreslot_client as cc
    need = {k: tuple(v) for k, v in json.loads(sys.argv[2]).items()}
    t = cc.Client().enqueue(sys.argv[3], None, need)
    print("ticket", t.name, flush=True)
    if sys.argv[4] == "hold":
        g = t.wait()
        print("admitted", json.dumps(g.tokens), flush=True)
    time.sleep(3600)
""")

# A child that reports its descriptors and environment.
REPORTER = textwrap.dedent("""
    import json, os, sys
    out = {"env": {k: v for k, v in os.environ.items() if k.startswith("CORESLOT_")}, "fds": {}}
    for fd in range(3, 40):
        try:
            st = os.fstat(fd)
            out["fds"][fd] = [st.st_dev, st.st_ino]
        except OSError:
            pass
    with open(sys.argv[1], "w") as fh:
        json.dump(out, fh)
    sys.exit(int(sys.argv[2]) if len(sys.argv) > 2 else 0)
""")


def load(path: str, raw: bool = False):
    with open(path) as fh:
        return fh.read() if raw else json.load(fh)


class FakeClock:
    def __init__(self, t: float = 1000.0):
        self.t = t

    def __call__(self) -> float:
        return self.t


def read_line(proc: subprocess.Popen, timeout: float = 10.0) -> str:
    deadline = time.monotonic() + timeout
    buf = b""
    fd = proc.stdout.fileno()
    while time.monotonic() < deadline:
        r, _, _ = select.select([fd], [], [], 0.1)
        if r:
            ch = os.read(fd, 1)
            if not ch:
                break
            if ch == b"\n":
                return buf.decode()
            buf += ch
    raise AssertionError(f"no line from pid {proc.pid} within {timeout}s (got {buf!r})")


class Base(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="cs-")
        self.S = os.path.join(self.root, "state dir")  # a space, as on macOS
        self.env = dict(os.environ, CORESLOT_STATE_DIR=self.S)
        for k in list(self.env):
            if k in cc._CONTEXT_VARS or k.startswith("CORESLOT_GRANTED_"):
                del self.env[k]
        self.procs: list[subprocess.Popen] = []
        self.fds: list[int] = []

    def tearDown(self):
        for p in self.procs:
            if p.poll() is None:
                p.kill()
            p.wait()
            if p.stdout:
                p.stdout.close()
        for fd in self.fds:
            try:
                os.close(fd)
            except OSError:
                pass
        for pid in getattr(self, "kill_pids", []):
            try:
                os.kill(pid, signal.SIGKILL)
            except OSError:
                pass
        shutil.rmtree(self.root, ignore_errors=True)

    def client(self, **kw) -> cc.Client:
        return cc.Client(self.S, diag=lambda m: None, **kw)

    def config(self, text: str):
        os.makedirs(self.S, 0o700, exist_ok=True)
        path = os.path.join(self.S, "config.toml")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        os.write(fd, text.encode())
        os.close(fd)

    def participant(self, need: dict, label: str, mode: str) -> tuple[subprocess.Popen, str]:
        p = subprocess.Popen([sys.executable, "-c", PARTICIPANT, HERE, json.dumps(need), label, mode],
                             env=self.env, stdout=subprocess.PIPE)
        self.procs.append(p)
        kind, name = read_line(p).split(" ", 1)
        self.assertEqual(kind, "ticket")
        return p, name

    def cli(self, *args, **kw) -> subprocess.Popen:
        p = subprocess.Popen([sys.executable, CLIENT, *args], env=kw.pop("env", self.env), **kw)
        self.procs.append(p)
        return p

    def lock(self, path: str) -> int:
        os.makedirs(os.path.dirname(path), 0o700, exist_ok=True)
        fd = os.open(path, os.O_RDONLY | os.O_CREAT, 0o600)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        self.fds.append(fd)
        return fd

    def write(self, path: str, data: bytes, lock: bool = False):
        os.makedirs(os.path.dirname(path), 0o700, exist_ok=True)
        fd = os.open(path, os.O_RDWR | os.O_CREAT | os.O_TRUNC, 0o600)
        os.write(fd, data)
        if lock:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            self.fds.append(fd)
        else:
            os.close(fd)

    def read(self, *parts) -> bytes:
        with open(os.path.join(self.S, *parts), "rb") as fh:
            return fh.read()

    def ls(self, sub: str) -> list[str]:
        try:
            return sorted(os.listdir(os.path.join(self.S, sub)))
        except FileNotFoundError:
            return []

    def gen(self) -> dict:
        with open(os.path.join(self.S, "gen"), "rb") as fh:
            return json.loads(fh.read())

    def wait_for(self, cond, timeout=15.0, what="condition"):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if cond():
                return
            time.sleep(0.05)
        self.fail(f"timed out waiting for {what}")


TICKET_BODY = {"label": "x", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 1}


def body(**over) -> bytes:
    d = dict(TICKET_BODY)
    d.update(over)
    return json.dumps(d).encode()


class BodyTests(unittest.TestCase):
    def ok(self, raw: bytes):
        cc.validate_ticket(cc.parse_body(raw))

    def bad(self, raw: bytes):
        with self.assertRaises(cc.Malformed, msg=raw[:120]):
            cc.validate_ticket(cc.parse_body(raw))

    def test_valid_and_tolerated(self):
        self.ok(body())
        self.ok(b" \n\t" + body() + b"\r\n ")
        self.ok(body()[:-1] + b', "extra": {"deep": [1.5, 1e999, 123456789012345678901234567890]}}')
        self.ok(body(label="\U0001F600"))
        self.ok(b'{"label": "\\ud83d\\ude00", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 0}')
        self.ok(body(label="x" * 128))
        self.ok(body(need={f"r{i}": [64, 256] for i in range(8)}))  # minimums sum to 512

    def test_malformed(self):
        self.bad(b"not json")
        self.bad(b"[]")
        self.bad(body(priority=1.0))
        self.bad(b'{"label": "x", "priority": 1e0, "need": {"cpu": [1, 1]}, "created_ns": 1}')
        self.bad(body(priority=True))
        self.bad(body(priority=1001))
        self.bad(body(created_ns=-1))
        self.bad(body(created_ns=2**63))
        self.bad(b'{"label": "x", "label": "y", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 1}')
        self.bad(b'{"label": "x", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 1, '
                 b'"u": {"a": 1, "a": 2}}')  # duplicate at depth, inside an unknown key
        self.bad(b'{"label": "\\ud800", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 1}')
        self.bad(b'{"label": "\\udc00\\ud800", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 1}')
        self.bad(b'{"label": "x", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 1, "u": "\\udfff"}')
        self.bad(b'{"label": "x", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": NaN}')
        self.bad(b"\xef\xbb\xbf" + body())  # BOM is not JSON whitespace
        self.bad(b'{"label": "\xff", "priority": 0, "need": {"cpu": [1, 1]}, "created_ns": 1}')
        self.bad(body(label="x" * 129))
        self.bad(body(need={}))
        self.bad(body(need={"CPU": [1, 1]}))
        self.bad(body(need={"cpu": [2, 1]}))
        self.bad(body(need={"cpu": [1, 257]}))
        self.bad(body(need={"cpu": [1]}))
        self.bad(body(need={f"r{i}": [1, 1] for i in range(9)}))
        self.bad(body(need={"a": [256, 256], "b": [256, 256], "c": [1, 1]}))
        self.bad(body() + b" " * (8193 - len(body())))
        d = dict(TICKET_BODY)
        del d["created_ns"]
        self.bad(json.dumps(d).encode())

    def test_hold_cancel_gen(self):
        hold = {"label": "", "grant": {"cpu": [0, 1]}, "start_ns": 5, "unbudgeted": False,
                "unbudgeted_resources": [], "clamped": {}, "adopted": False}
        cc.validate_hold(cc.parse_body(json.dumps(hold).encode()), 5)
        for bad in ({"grant": {"cpu": []}}, {"grant": {"cpu": [1, 0]}}, {"grant": {"cpu": [1, 1]}},
                    {"grant": {"cpu": [4096]}}, {"clamped": {"cpu": 0}}, {"adopted": 0},
                    {"unbudgeted_resources": ["Bad"]}):
            with self.assertRaises(cc.Malformed, msg=bad):
                cc.validate_hold(cc.parse_body(json.dumps(dict(hold, **bad)).encode()), 5)
        with self.assertRaises(cc.Malformed):
            cc.validate_hold(cc.parse_body(json.dumps(hold).encode()), 6)
        cc.validate_cancel(cc.parse_body(b'{"requested_ns": 1, "requester_pid": 2}'))
        with self.assertRaises(cc.Malformed):
            cc.validate_cancel(cc.parse_body(b'{"requested_ns": 1}'))
        self.assertEqual(cc.parse_format(b"coreslot-protocol 1"), 1)
        self.assertEqual(cc.parse_format(b"coreslot-protocol 2\n"), 2)
        self.assertEqual(cc.parse_format(b"coreslot-protocol 01"), 1)  # leading zeros (§7.6)
        self.assertEqual(cc.parse_format(b"coreslot-protocol 000000000"), 0)  # 9 digits, unknown
        for raw in (b"coreslot-protocol 1\n\n", b"coreslot-protocol x", b"", b"coreslot-protocol +1",
                    b"coreslot-protocol 0000000001", b"coreslot-protocol 1\r\n", b"coreslot-protocol  1"):
            with self.assertRaises(cc.Malformed):
                cc.parse_format(raw)

    def test_names_sizes_need(self):
        self.assertEqual(cc.display_of("gate:unit"), "gate_unit")
        self.assertEqual(cc.display_of("ci:o-r"), "ci_o_r")
        self.assertEqual(cc.order_key(1791719345000000000 + 600 * 10**9, 10), 1791719345000000000)
        self.assertEqual(cc.order_key(5, 1000), 0)
        self.assertEqual(cc.parse_cpu_list("0-3,8-11\n"), 8)
        self.assertEqual(cc.parse_cpu_list("0-13"), 14)
        self.assertEqual(cc.size_of("-4", 14), 10)
        self.assertEqual(cc.size_of("50%", 14), 7)
        self.assertEqual(cc.size_of("1%", 14), 1)
        self.assertEqual(cc.size_of("-20", 14), 1)
        self.assertEqual(cc.size_of(4096, 14), 4096)
        # §5.2: 1-9 digits, leading zeros allowed; no sign, no space, no tenth digit.
        self.assertEqual(cc.size_of("05%", 14), cc.size_of("5%", 14))
        self.assertEqual(cc.size_of("-007", 14), 7)
        self.assertEqual(cc.size_of("-000000000", 14), 14)
        self.assertEqual(cc.size_of("000000100%", 14), 14)
        for raw in ("0000000005%", "-0000000007", "+5%", " 5%", "5 %", "-+7", "000%", "-"):
            with self.assertRaises(ValueError, msg=raw):
                cc.size_of(raw, 14)
        for raw in (0, 4097, -3, "0%", "101%", "x", True, 1.5, {}):
            with self.assertRaises(ValueError, msg=raw):
                cc.size_of(raw, 14)
        self.assertEqual(cc.parse_token_fds("4-13"), list(range(4, 14)))
        self.assertEqual(cc.parse_token_fds("4,6-7,9"), [4, 6, 7, 9])
        for raw in ("4-5,5", "4-6,5-8", "", "4-", "a", "5-4"):
            with self.assertRaises(ValueError, msg=raw):
                cc.parse_token_fds(raw)
        for need in ({"cpu": (2, 1)}, {"cpu": (1, 257)}, {"cpu": (0, 1)}, {"Cpu": 1},
                     {f"r{i}": 1 for i in range(9)}, {"a": 256, "b": 256, "c": 1}, {}):
            with self.assertRaises(cc.UsageError, msg=need):
                cc.normalize_need(need)


class StateDirTests(Base):
    def test_relative_override_is_state_error(self):
        with self.assertRaises(cc.StateError):
            cc.Client("relative/dir").enqueue("x", 0, {"cpu": 1})
        env = dict(self.env, CORESLOT_STATE_DIR="relative/dir")
        with self.assertRaises(cc.StateError):
            old = os.environ.get("CORESLOT_STATE_DIR")
            os.environ["CORESLOT_STATE_DIR"] = env["CORESLOT_STATE_DIR"]
            try:
                cc.Client().enqueue("x", 0, {"cpu": 1})
            finally:
                if old is None:
                    del os.environ["CORESLOT_STATE_DIR"]
                else:
                    os.environ["CORESLOT_STATE_DIR"] = old

    def test_default_location_comes_from_password_database(self):
        saved = {k: os.environ.pop(k, None) for k in ("CORESLOT_STATE_DIR", "HOME", "XDG_STATE_HOME")}
        os.environ["HOME"] = "/nonexistent-home"
        os.environ["XDG_STATE_HOME"] = "/nonexistent-xdg"
        try:
            c = cc.Client()
            home = __import__("pwd").getpwuid(os.getuid()).pw_dir
            want = (os.path.join(home, "Library", "Application Support", "coreslot")
                    if sys.platform == "darwin" else os.path.join(home, ".local", "state", "coreslot"))
            self.assertEqual(c.state_dir(), want)
            self.assertEqual(c.config_path(), os.path.join(home, ".config", "coreslot", "config.toml"))
            os.environ["CORESLOT_STATE_DIR"] = ""  # empty is not set
            self.assertEqual(cc.Client().state_dir(), want)
        finally:
            for k, v in saved.items():
                if v is None:
                    os.environ.pop(k, None)
                else:
                    os.environ[k] = v

    def test_config_moves_with_the_override(self):
        c = self.client()
        self.assertEqual(c.config_path(), os.path.join(self.S, "config.toml"))
        self.config('[resources]\ncpu = 1\n[priorities]\n"ci:" = 7\n')
        a = c.enqueue("a", None, {"cpu": (1, 4)})
        self.assertEqual(a.priority, 0)
        self.assertEqual(c.enqueue("ci:b", None, {"cpu": 1}).priority, 7)
        kind, g = a.step()
        # "ci:b" sorts ahead of "a" by 7 minutes, so "a" is not the head.
        self.assertEqual(kind, "waiting")

    def test_wrong_types_owners_modes(self):
        self.client().enqueue("x", 0, {"cpu": 1}).withdraw()
        os.chmod(self.S, 0o770)
        with self.assertRaises(cc.StateError):
            self.client().enqueue("x", 0, {"cpu": 1})
        os.chmod(self.S, 0o700)
        os.chmod(os.path.join(self.S, "queue"), 0o722)
        with self.assertRaises(cc.StateError):
            self.client().enqueue("x", 0, {"cpu": 1})
        os.chmod(os.path.join(self.S, "queue"), 0o700)
        link = os.path.join(self.root, "link")
        os.symlink(self.S, link)
        with self.assertRaises(cc.StateError):
            cc.Client(link).enqueue("x", 0, {"cpu": 1})
        os.symlink(os.path.join(self.root, "elsewhere.toml"), os.path.join(self.S, "config.toml"))
        with self.assertRaises(cc.StateError):
            self.client().config()

    def test_invalid_configuration(self):
        for text in ("[resources]\ncpu = 0\n", '[resources]\ncpu = "101%"\n', "[resources]\nCPU = 2\n",
                     "not toml ===", '[priorities]\n"a" = 1001\n', "[platform]\nwindows_warning = 1\n",
                     "resources = 3\n"):
            self.config(text)
            with self.assertRaises(cc.StateError, msg=text):
                self.client().config()
        self.config("[resources]\npg-local = 2\n[other]\nx = 1\n")
        cfg = self.client().config()
        self.assertEqual(cfg.size("cpu", 14), 14)  # cpu defaults to "100%"
        self.assertEqual(cfg.size("pg-local", 14), 2)

    def test_malformed_format_fails_open_without_writing(self):
        self.write(os.path.join(self.S, "FORMAT"), b"coreslot-protocol one\n")
        with self.assertRaises(cc.StateError) as ctx:
            self.client().enqueue("x", 0, {"cpu": 1})
        self.assertEqual(ctx.exception.reason, "format-malformed")
        before = sorted(os.listdir(self.S))
        out = os.path.join(self.root, "out.json")
        p = self.cli("run", "--", sys.executable, "-c", REPORTER, out, stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 0)
        self.assertEqual(load(out)["env"]["CORESLOT_UNBUDGETED"], "format-malformed")
        self.assertNotIn("CORESLOT_HELD_FD", load(out)["env"])
        self.assertEqual(sorted(os.listdir(self.S)), before)  # no hold, degraded, log or gen

    def test_cli_fails_open_on_invalid_configuration(self):
        self.config("[resources]\ncpu = 0\n")
        out = os.path.join(self.root, "out.json")
        p = self.cli("run", "--need", "cpu=2..4", "--", sys.executable, "-c", REPORTER, out, "5",
                     stderr=subprocess.PIPE)
        _, err = p.communicate(timeout=20)
        self.assertEqual(p.returncode, 5, err)
        rep = load(out)
        self.assertEqual(rep["env"]["CORESLOT_UNBUDGETED"], "config")
        self.assertEqual(rep["env"]["CORESLOT_GRANTED_CPU"], "2")
        self.assertEqual(rep["env"]["CORESLOT_HELD_FD"], "3")
        degraded = cc.validate_degraded(cc.parse_body(self.read("degraded")))
        self.assertEqual((degraded["reason"], degraded["count"]), ("config", 1))
        self.assertIn(b"state error", err)


class EnqueueTests(Base):
    def test_enqueue_publishes_a_locked_ticket(self):
        c = self.client()
        t = c.enqueue("gate:unit", 10, {"cpu": (4, 12)})
        self.assertEqual(self.read("FORMAT"), b"coreslot-protocol 1\n")
        self.assertEqual(self.gen()["gen"], 1)
        self.assertEqual(self.ls("queue"), [t.name])
        m = cc.TICKET_RE.fullmatch(t.name)
        self.assertEqual((int(m.group(2)), m.group(4)), (os.getpid(), "gate_unit"))
        path = os.path.join(self.S, "queue", t.name)
        b = cc.validate_ticket(cc.parse_body(self.read("queue", t.name)))
        self.assertEqual((b["label"], b["priority"], b["need"]), ("gate:unit", 10, {"cpu": [4, 12]}))
        self.assertEqual(int(m.group(1)), b["created_ns"] - 10 * 60 * 10**9)
        self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)
        for d in ("", "queue"):
            self.assertEqual(os.stat(os.path.join(self.S, d)).st_mode & 0o777, 0o700)
        self.assertIs(cc._probe_path(path), False)  # live
        t.withdraw(remove=False)
        self.assertIs(cc._probe_path(path), True)  # dead at once
        self.assertEqual(self.ls("queue"), [t.name])

    def test_refresh_moves_mtime_and_resume_bumps_gen(self):
        self.config("[resources]\ncpu = 1\n")
        self.lock(os.path.join(self.S, "res", "cpu", "0"))
        t = self.client().enqueue("x", 0, {"cpu": 1})
        path = os.path.join(self.S, "queue", t.name)
        os.utime(path, ns=(1, 1))
        t._last_refresh_m -= 11  # pretend the waiter was suspended for 11 s
        g0 = self.gen()["gen"]
        t.step()
        self.assertGreater(os.stat(path).st_mtime_ns, 10**18)
        self.assertEqual(self.gen()["gen"], g0 + 1)  # resume bump (§10.2)


class OrderTests(Base):
    def test_fifo_and_priority_across_processes(self):
        """Holder blocks; A, B, C (priority 5) queue in that order; release → C, A, B."""
        self.config("[resources]\ncpu = 1\n")
        holder, _ = self.participant({"cpu": [1, 1]}, "holder", "hold")
        self.assertTrue(read_line(holder).startswith("admitted"))
        order = os.path.join(self.root, "order")
        procs = []
        for name, extra in (("A", []), ("B", []), ("C", ["--priority", "5"])):
            n = len(self.ls("queue"))
            procs.append(self.cli("run", "--need", "cpu=1..1", "--label", name, *extra, "--",
                                  "sh", "-c", f"echo {name} >> '{order}'; sleep 0.2"))
            self.wait_for(lambda: len(self.ls("queue")) == n + 1, what=f"{name}'s ticket")
        holder.kill()
        for p in procs:
            self.assertEqual(p.wait(timeout=30), 0)
        self.assertEqual(load(order, raw=True).split(), ["C", "A", "B"])


class LivenessTests(Base):
    def test_stalled_waiter_is_passed_over(self):
        """A live ticket ahead that is never refreshed is skipped after 10 s of observer time."""
        self.config("[resources]\ncpu = 2\n")
        _, stalled_name = self.participant({"cpu": [1, 1]}, "stalled", "stall")
        clock = FakeClock()
        b = self.client(observer_clock=clock).enqueue("b", 0, {"cpu": 1})
        self.assertEqual(b.step(), ("waiting", 2))
        clock.t += 9.5
        self.assertEqual(b.step(), ("waiting", 2))
        # An observed refresh makes it WAITING again.
        os.utime(os.path.join(self.S, "queue", stalled_name))
        clock.t += 5.0
        self.assertEqual(b.step(), ("waiting", 2))
        clock.t += 9.5
        self.assertEqual(b.step(), ("waiting", 2))
        clock.t += 0.5  # exactly 10 s since the last observed change
        kind, g = b.step()
        self.assertEqual(kind, "admitted")
        self.assertEqual(g.tokens, {"cpu": [0]})
        self.assertIn(stalled_name, self.ls("queue"))  # passed over, not removed
        g.close()

    def test_killed_holders_tokens_are_freed(self):
        self.config("[resources]\ncpu = 2\n")
        holder, _ = self.participant({"cpu": [2, 2]}, "holder", "hold")
        self.assertEqual(read_line(holder), 'admitted {"cpu": [0, 1]}')
        hold = self.ls("holds")
        self.assertEqual(len(hold), 1)
        b = self.client().enqueue("b", 0, {"cpu": 1})
        self.assertEqual(b.step(), ("waiting", 1))  # head, but nothing free
        holder.send_signal(signal.SIGKILL)
        holder.wait()
        kind, g = b.step()
        self.assertEqual(kind, "admitted")
        self.assertEqual(g.tokens, {"cpu": [0]})
        self.assertIn(hold[0], self.ls("holds"))  # dead hold stays until reconciliation
        # Make reconciliation due: closing our grant removes our hold and reconciles first.
        self.write(os.path.join(self.S, "gen"), b'{"gen": 9, "reconciled_ns": 0}')
        g.close()
        self.assertEqual(self.ls("holds"), [])

    def test_malformed_and_non_matching_entries_are_ignored(self):
        self.config("[resources]\ncpu = 1\n")
        q = os.path.join(self.S, "queue")
        bad = [b"not json", body(priority=1.0), b'{"label":"a","label":"b","priority":0,'
               b'"need":{"cpu":[1,1]},"created_ns":1}', b'{"label":"\\ud800","priority":0,'
               b'"need":{"cpu":[1,1]},"created_ns":1}', body(need={"CPU": [1, 1]}), body()[:-1]]
        for i, raw in enumerate(bad):
            self.write(os.path.join(q, f"{i:020d}-0000000001-{i:06d}-bad"), raw, lock=True)  # live
        self.write(os.path.join(q, "garbage-name"), body(), lock=True)
        self.write(os.path.join(q, "00000000000000000009-1-000001"), body(), lock=True)
        self.write(os.path.join(q, "00000000000000000010-0000000001-000099"), body())  # valid but dead
        b = self.client().enqueue("b", 0, {"cpu": 1})
        kind, g = b.step()
        self.assertEqual(kind, "admitted")
        g.close(remove_hold=False)


    def test_front_window_watches_a_stopped_head_behind_a_live_ticket(self):
        """§10.3: a stopped head hidden behind a live ticket is still watched (the front window),
        so it is passed over at once when the live ticket goes, not 10 s later."""
        self.config("[resources]\ncpu = 1\n")
        q = os.path.join(self.S, "queue")
        head = os.path.join(q, "00000000000000000001-0000000001-000001")
        live = os.path.join(q, "00000000000000000002-0000000001-000001")
        self.write(head, body(), lock=True)  # stopped: never refreshed
        live_fd = os.open(live, os.O_RDWR | os.O_CREAT, 0o600)
        os.write(live_fd, body())
        fcntl.flock(live_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        clock = FakeClock()
        b = self.client(observer_clock=clock).enqueue("b", 0, {"cpu": 1})
        for s in range(11):  # one election a second; the live ticket refreshed each time
            os.utime(live, ns=(s + 1, s + 1))
            self.assertEqual(b.step(), ("waiting", 3), f"at {s} s")
            clock.t += 1.0
        os.close(live_fd)  # the live ticket goes (dead): the head has been watched for 11 s
        kind, g = b.step()
        self.assertEqual(kind, "admitted")
        g.close(remove_hold=False)

    def test_position_counts_only_well_formed_tickets(self):
        """§10.3: position is 1 plus the tickets ahead not passed over, malformed ones excluded."""
        self.config("[resources]\ncpu = 1\n")
        q = os.path.join(self.S, "queue")
        for i in range(1, 5):  # the front window: four well-formed live tickets
            self.write(os.path.join(q, f"0000000000000000000{i}-0000000001-000001"), body(), lock=True)
        # Beyond the window and the scan: a malformed one, then the nearest, which ends the scan.
        self.write(os.path.join(q, "00000000000000000005-0000000001-000001"), b"not json", lock=True)
        self.write(os.path.join(q, "00000000000000000006-0000000001-000001"), body(), lock=True)
        b = self.client(observer_clock=FakeClock()).enqueue("b", 0, {"cpu": 1})
        self.assertEqual(b.step(), ("waiting", 6))  # five well-formed live tickets ahead
        b.withdraw()

    def test_gone_window_ticket_is_forgotten(self):
        """A front-window ticket found gone is dropped from the observer's records."""
        self.config("[resources]\ncpu = 1\n")
        q = os.path.join(self.S, "queue")
        front = os.path.join(q, "00000000000000000001-0000000001-000001")
        self.write(front, body(), lock=True)
        self.write(os.path.join(q, "00000000000000000002-0000000001-000001"), body(), lock=True)
        b = self.client(observer_clock=FakeClock()).enqueue("b", 0, {"cpu": 1})
        b.step()
        self.assertIn(os.path.basename(front), b._obs)  # watched through the front window
        os.unlink(front)
        b.step()
        self.assertNotIn(os.path.basename(front), b._obs)
        b.withdraw()

    def test_reelection_watches_a_ticket_published_without_bump(self):
        """§12.1 step 5: a live ticket first seen in the section makes the next election rescan
        (it was published without a gen bump), so it is watched and passed over once STALLED."""
        self.config("[resources]\ncpu = 1\n")
        clock = FakeClock()
        h = self.client(observer_clock=clock).enqueue("h", 0, {"cpu": 1})
        real_elect = h._elect
        ahead = os.path.join(self.S, "queue", "00000000000000000001-0000000001-000001")

        def elect_then_publish():
            position = real_elect()
            if not os.path.exists(ahead):
                self.write(ahead, body(), lock=True)  # no gen bump
            return position

        h._elect = elect_then_publish
        self.assertEqual(h.step(), ("waiting", 1))  # kept out by the re-election
        h._elect = real_elect
        for s in range(1, 13):
            clock.t += 1.0
            kind, value = h.step()
            if kind == "admitted":
                self.assertGreaterEqual(s, 10)
                value.close(remove_hold=False)
                return
        self.fail("never admitted: the ticket published without a bump kept the head out")

    def test_unwatched_interval_does_not_count(self):
        """§10.2: only watched time counts towards a stall; a changed value still restarts it."""
        t = self.client().enqueue("t", 0, {"cpu": 1})
        for s in range(5):  # watched 0..4 s
            t._pass += 1
            self.assertFalse(t._observe("x", 7, 1000.0 + s))
        t._pass += 20  # 20 elections that stopped before x
        self.assertFalse(t._observe("x", 7, 1030.0))  # 4 s watched, the 26 s gap does not count
        for s in range(31, 36):
            t._pass += 1
            self.assertFalse(t._observe("x", 7, 1000.0 + s), f"at {s} s")
        t._pass += 1
        self.assertTrue(t._observe("x", 7, 1036.0))  # 10 s watched
        t._pass += 5
        self.assertFalse(t._observe("x", 8, 1100.0))  # changed during the gap: restarted
        t.withdraw()

    def test_dead_ticket_ahead_is_skipped_at_once(self):
        """§21.5: a withdrawn (unlocked) ticket ahead is skipped without waiting 10 s."""
        self.config("[resources]\ncpu = 1\n")
        b = self.client(observer_clock=FakeClock()).enqueue("b", 0, {"cpu": 1})
        # Created after enqueue's reconciliation, so only the election can skip it.
        self.write(os.path.join(self.S, "queue", "00000000000000000010-0000000001-000099"), body())
        kind, g = b.step()
        self.assertEqual(kind, "admitted")
        g.close(remove_hold=False)


class AdmissionTests(Base):
    def test_reelection_inside_the_section(self):
        """§12.1 step 5: a ticket published ahead after the head's election, before its admitting
        section, is first seen there and counts as live: the head does not take the free token."""
        self.config("[resources]\ncpu = 1\n")
        clock = FakeClock()
        h = self.client(observer_clock=clock).enqueue("h", 0, {"cpu": 1})
        ahead = os.path.join(self.S, "queue", "00000000000000000001-0000000001-000001")
        real_elect = h._elect

        def elect_then_publish():
            position = real_elect()  # h wins the election ...
            self.write(ahead, body(), lock=True)  # ... and a live ticket appears ahead of it
            g = self.gen()  # published as a writer does: with a gen bump
            self.write(os.path.join(self.S, "gen"), json.dumps(dict(g, gen=g["gen"] + 1)).encode())
            return position

        h._elect = elect_then_publish
        self.assertEqual(h.step(), ("waiting", 1))
        self.assertEqual(self.ls("holds"), [])
        h._elect = real_elect
        clock.t += 1.0
        self.assertEqual(h.step(), ("waiting", 2))
        h.withdraw()

    def test_admission_with_clamp(self):
        """§21.3: cpu size 10 with 14 token files, cpu=16..16 and pg-local=1."""
        self.config('[resources]\ncpu = 10\npg-local = 2\n')
        for i in range(14):
            self.write(os.path.join(self.S, "res", "cpu", str(i)), b"")
        c = self.client()
        t = c.enqueue("gate:big", 0, {"cpu": (16, 16), "pg-local": 1})
        g0 = self.gen()["gen"]
        kind, g = t.step()
        self.assertEqual(kind, "admitted")
        self.assertEqual(g.tokens, {"cpu": list(range(10)), "pg-local": [0]})
        self.assertEqual(g.granted, {"cpu": 10, "pg-local": 1})
        self.assertEqual(self.ls("queue"), [])
        self.assertEqual(self.gen()["gen"], g0 + 1)
        [hold] = self.ls("holds")
        hb = cc.validate_hold(cc.parse_body(self.read("holds", hold)),
                              int(hold[:20]))
        self.assertEqual(hb["grant"], {"cpu": list(range(10)), "pg-local": [0]})
        self.assertEqual((hb["clamped"], hb["unbudgeted"], hb["adopted"], hb["unbudgeted_resources"]),
                         ({"cpu": 16}, False, False, []))
        self.assertTrue(hold.endswith("-gate_big"))
        for i in range(14):
            self.assertIs(cc._probe_path(os.path.join(self.S, "res", "cpu", str(i))), i >= 10)
        self.assertEqual(g.environment(), {"CORESLOT_GRANTED_CPU": "10", "CORESLOT_GRANTED_PG_LOCAL": "1",
                                           "CORESLOT_HELD_FD": "3", "CORESLOT_TOKEN_FDS": "4-14"})
        g.close()
        self.assertEqual(self.ls("holds"), [])

    def test_cap_of_512_takes_minimums_first(self):
        """§12.1: cpu/db/gpu 100..256 each, sizes 300: 256 + 156 + 100."""
        cc.raise_nofile(2048)
        self.config("[resources]\ncpu = 300\ndb = 300\ngpu = 300\n")
        t = self.client().enqueue("big", 0, {"cpu": (100, 256), "db": (100, 256), "gpu": (100, 256)})
        kind, g = t.step()
        self.assertEqual(kind, "admitted")
        self.assertEqual(g.granted, {"cpu": 256, "db": 156, "gpu": 100})
        self.assertEqual(g.tokens["db"], list(range(156)))
        g.close()

    def test_shrink_counts_locked_tokens_above_the_size(self):
        """§12.1: size 12, a live hold on 10-13 → free 8, taken only from indices below 12."""
        self.config("[resources]\ncpu = 12\n")
        for i in range(10, 14):
            self.lock(os.path.join(self.S, "res", "cpu", str(i)))
        c = self.client()
        big = c.enqueue("big", 0, {"cpu": (9, 9)})
        self.assertEqual(big.step(), ("waiting", 1))  # free 8 < 9
        big.withdraw()
        t = c.enqueue("t", 0, {"cpu": (1, 12)})
        kind, g = t.step()
        self.assertEqual(kind, "admitted")
        self.assertEqual(g.tokens, {"cpu": list(range(8))})
        g.close()

    def test_undeclared_resource_is_not_budgeted(self):
        t = self.client().enqueue("u", 0, {"cpu": 1, "gpu": (2, 3)})
        kind, g = t.step()
        self.assertEqual(kind, "admitted")
        self.assertEqual(g.tokens, {"cpu": [0]})
        self.assertEqual(g.granted, {"cpu": 1, "gpu": 2})
        self.assertEqual(g.unbudgeted_resources, ["gpu"])
        self.assertNotIn("CORESLOT_UNBUDGETED", g.environment())
        self.assertEqual(g.environment()["CORESLOT_GRANTED_GPU"], "2")
        self.assertFalse(os.path.exists(os.path.join(self.S, "res", "gpu")))
        [hold] = self.ls("holds")
        hb = json.loads(self.read("holds", hold))
        self.assertEqual((hb["grant"], hb["unbudgeted_resources"]), ({"cpu": [0]}, ["gpu"]))
        g.close()

    def test_token_only_on_unknown_format(self):
        """§21.8: FORMAT 2; cpu 0-3 held elsewhere; cpu=2..6 takes 4 and 5 and writes nothing."""
        self.config("[resources]\ncpu = 10\n")
        self.write(os.path.join(self.S, "FORMAT"), b"coreslot-protocol 2\n")
        self.write(os.path.join(self.S, "admit.lock"), b"format-2 body")
        for i in range(4):
            self.lock(os.path.join(self.S, "res", "cpu", str(i)))
        before = sorted(os.listdir(self.S))
        t = self.client().enqueue("x", 0, {"cpu": (2, 6)})
        self.assertTrue(t.token_only)
        kind, g = t.step()
        self.assertEqual(kind, "admitted")
        self.assertEqual(g.tokens, {"cpu": [4, 5]})
        self.assertEqual(g.environment(), {"CORESLOT_GRANTED_CPU": "2", "CORESLOT_TOKEN_FDS": "4-5",
                                           "CORESLOT_UNBUDGETED": "format"})
        self.assertEqual(sorted(os.listdir(self.S)), before)
        self.assertEqual(self.read("admit.lock"), b"format-2 body")
        g.close()

    def test_waiter_withdraws_when_the_format_changes(self):
        self.config("[resources]\ncpu = 1\n")
        hold_fd = self.lock(os.path.join(self.S, "res", "cpu", "0"))
        t = self.client().enqueue("x", 0, {"cpu": 1})
        self.assertEqual(t.step(), ("waiting", 1))
        self.write(os.path.join(self.S, "FORMAT"), b"coreslot-protocol 3")
        self.assertEqual(t.step(), ("waiting", 0))
        self.assertTrue(t.token_only)
        self.assertIs(cc._probe_path(os.path.join(self.S, "queue", t.name)), True)  # withdrawn
        os.close(hold_fd)
        self.fds.remove(hold_fd)
        kind, g = t.step()
        self.assertEqual((kind, g.environment()["CORESLOT_UNBUDGETED"]), ("admitted", "format"))
        g.close()


class CancelTests(Base):
    def test_cancellation_library(self):
        self.config("[resources]\ncpu = 1\n")
        a = self.client().enqueue("a", 0, {"cpu": 1})
        ga = a.wait(5)
        b = self.client().enqueue("b", 0, {"cpu": 1})
        self.assertEqual(b.step(), ("waiting", 1))
        self.assertTrue(self.client().cancel(b.name))  # a full name is accepted, its id is used
        self.assertEqual(self.ls("cancel"), [b.id])
        cc.validate_cancel(cc.parse_body(self.read("cancel", b.id)))
        ga.close()  # capacity frees, but the cancel was published first and wins
        with self.assertRaises(cc.Cancelled):
            b.step()
        self.assertEqual((self.ls("queue"), self.ls("cancel")), ([], []))

    def test_malformed_cancel_request_is_ignored(self):
        b = self.client().enqueue("b", 0, {"cpu": 1})
        self.write(os.path.join(self.S, "cancel", b.id), b'{"requested_ns": 1.5, "requester_pid": 1}')
        kind, g = b.step()
        self.assertEqual(kind, "admitted")
        g.close()

    def test_cancellation_cli_exits_with_refusal_code(self):
        self.config("[resources]\ncpu = 1\n")
        self.lock(os.path.join(self.S, "res", "cpu", "0"))
        p = self.cli("run", "--need", "cpu=1", "--", "true", stderr=subprocess.DEVNULL)
        self.wait_for(lambda: len(self.ls("queue")) == 1, what="ticket")
        [name] = self.ls("queue")
        c = self.cli("cancel", name[:38])
        self.assertEqual(c.wait(timeout=10), 0)
        self.assertEqual(p.wait(timeout=10), 75)
        self.assertEqual((self.ls("queue"), self.ls("cancel")), ([], []))

    def test_interrupted_waiter_withdraws_and_exits_130(self):
        self.config("[resources]\ncpu = 1\n")
        self.lock(os.path.join(self.S, "res", "cpu", "0"))
        p = self.cli("run", "--need", "cpu=1", "--", "true", stderr=subprocess.DEVNULL)
        self.wait_for(lambda: len(self.ls("queue")) == 1, what="ticket")
        time.sleep(0.3)
        p.send_signal(signal.SIGINT)
        self.assertEqual(p.wait(timeout=10), 130)
        for n in self.ls("queue"):
            self.assertIs(cc._probe_path(os.path.join(self.S, "queue", n)), True)


class ReconciliationTests(Base):
    def test_reconciliation_when_due(self):
        """§21.6: dead ticket and hold, an orphan cancel, an old temporary; gen + 2."""
        now = time.time_ns()
        self.write(os.path.join(self.S, "gen"),
                   json.dumps({"gen": 70, "reconciled_ns": now - 45 * 10**9}).encode())
        q, h, c = (os.path.join(self.S, d) for d in ("queue", "holds", "cancel"))
        dead_t = "01791720000000000000-0000004711-000001-gate_unit"
        live_t = "01791721150000000000-0000006100-000002-lint"
        dead_h = "01791720600000000000-0000003999-000005-build"
        self.write(os.path.join(q, dead_t), body())
        self.write(os.path.join(q, live_t), body(), lock=True)
        self.write(os.path.join(h, dead_h), b"{}")
        self.write(os.path.join(h, ".tmp-0000003998-000002"), b"")
        os.utime(os.path.join(h, ".tmp-0000003998-000002"), ns=(now - 120 * 10**9,) * 2)
        self.write(os.path.join(q, ".tmp-0000003998-000003"), b"")  # young: kept
        self.write(os.path.join(c, "01791720900000000000-0000005555-000001"), b"{}")  # orphan
        self.write(os.path.join(c, live_t[:38]), b'{"requested_ns": 1, "requester_pid": 1}')
        self.write(os.path.join(h, "README"), b"not ours")  # non-matching: never removed
        t = self.client().enqueue("gate", 0, {"cpu": 1})
        self.assertEqual(self.ls("queue"), sorted([".tmp-0000003998-000003", live_t, t.name]))
        self.assertEqual(self.ls("holds"), ["README"])
        self.assertEqual(self.ls("cancel"), [live_t[:38]])
        g = self.gen()
        self.assertEqual(g["gen"], 72)
        self.assertLess(abs(g["reconciled_ns"] - time.time_ns()), 5 * 10**9)

    def test_a_stray_never_protects_a_cancel_request(self):
        """§10.3/§10.4 step 2: a queue/ entry ignored as a stray is no entry; its cancel request goes."""
        self.write(os.path.join(self.S, "gen"), b'{"gen": 3, "reconciled_ns": 0}')  # reconciliation due
        q, c = os.path.join(self.S, "queue"), os.path.join(self.S, "cancel")
        elsewhere = os.path.join(self.root, "elsewhere")
        self.write(elsewhere, body())
        link_t = "01791720000000000000-0000004711-000901-link"
        dir_t = "01791720000000000000-0000004711-000902-dir"
        writable_t = "01791720000000000000-0000004711-000903-writable"
        live_t = "01791721150000000000-0000006100-000002-lint"
        os.makedirs(q, 0o700)
        os.symlink(elsewhere, os.path.join(q, link_t))
        os.makedirs(os.path.join(q, dir_t), 0o700)
        self.write(os.path.join(q, writable_t), body())
        os.chmod(os.path.join(q, writable_t), 0o622)
        self.write(os.path.join(q, live_t), body(), lock=True)
        for name in (link_t, dir_t, writable_t, live_t):
            self.write(os.path.join(c, name[:38]), b'{"requested_ns": 1, "requester_pid": 1}')
        c1 = self.client()
        t = c1.enqueue("x", 0, {"cpu": 1})
        self.assertEqual(self.ls("cancel"), [live_t[:38]])  # only the live ticket's request stays
        self.assertEqual(self.ls("queue"), sorted([link_t, dir_t, writable_t, live_t, t.name]))  # strays untouched
        self.assertEqual(c1.ignored_by_reconciliation["queue"], 3)
        self.assertTrue(os.path.exists(elsewhere))

    def test_admission_never_reconciles(self):
        self.write(os.path.join(self.S, "gen"), b'{"gen": 3, "reconciled_ns": 1}')
        t = self.client().enqueue("x", 0, {"cpu": 1})  # enqueue reconciles
        self.write(os.path.join(self.S, "gen"), b'{"gen": 3, "reconciled_ns": 1}')
        dead = os.path.join(self.S, "queue", "09999999999999999999-0000000001-000001")
        self.write(dead, body())
        kind, g = t.step()
        self.assertEqual(kind, "admitted")
        self.assertTrue(os.path.exists(dead))
        self.assertEqual(self.gen(), {"gen": 4, "reconciled_ns": 1})
        g.close(remove_hold=False)


class LauncherTests(Base):
    def test_descriptor_and_environment_contract(self):
        self.config("[resources]\ncpu = 3\npg-local = 2\n")
        out = os.path.join(self.root, "out.json")
        p = self.cli("run", "--need", "cpu=2..2", "--need", "pg-local=1", "--label", "fd:test", "--",
                     sys.executable, "-c", REPORTER, out, "7")
        self.assertEqual(p.wait(timeout=20), 7)
        rep = load(out)
        self.assertEqual(rep["env"], {"CORESLOT_STATE_DIR": self.S, "CORESLOT_GRANTED_CPU": "2",
                                      "CORESLOT_GRANTED_PG_LOCAL": "1", "CORESLOT_HELD_FD": "3",
                                      "CORESLOT_TOKEN_FDS": "4-6"})
        self.assertEqual(sorted(rep["fds"]), ["3", "4", "5", "6"])  # nothing else leaks

        def ident(*parts):
            st = os.stat(os.path.join(self.S, *parts))
            return [st.st_dev, st.st_ino]

        self.assertEqual(rep["fds"]["4"], ident("res", "cpu", "0"))
        self.assertEqual(rep["fds"]["5"], ident("res", "cpu", "1"))
        self.assertEqual(rep["fds"]["6"], ident("res", "pg-local", "0"))
        self.assertEqual(self.ls("holds"), [])  # removed after exit
        log = [json.loads(line) for line in self.read("log", "runs.jsonl").splitlines()]
        self.assertEqual((log[-1]["label"], log[-1]["grant"], log[-1]["exit"]),
                         ("fd:test", {"cpu": 2, "pg-local": 1}, 7))

    def test_hold_descriptor_is_fd_3(self):
        out = os.path.join(self.root, "out.json")
        script = ("import os, sys, json; st = os.fstat(3); d = sys.argv[1] + '/holds'; "
                  "names = os.listdir(d); json.dump([[st.st_dev, st.st_ino], "
                  "[[os.stat(d + '/' + n).st_dev, os.stat(d + '/' + n).st_ino] for n in names]], "
                  f"open({out!r}, 'w'))")
        p = self.cli("run", "--need", "cpu=1", "--", sys.executable, "-c", script, self.S)
        self.assertEqual(p.wait(timeout=20), 0)
        fd3, holds = load(out)
        self.assertEqual(holds, [fd3])

    def test_exit_statuses_and_placeholders(self):
        out = os.path.join(self.root, "args")
        p = self.cli("run", "--need", "cpu=3..3", "--subst", "--", "sh", "-c",
                     f"echo {{n}} -j{{n}} {{{{n}}}} > '{out}'")
        self.assertEqual(p.wait(timeout=20), 0)
        self.assertEqual(load(out, raw=True).split(), ["3", "-j3", "{n}"])
        p = self.cli("run", "--", "sh", "-c", "kill -TERM $$")
        self.assertEqual(p.wait(timeout=20), 128 + signal.SIGTERM)
        p = self.cli("run", "--", "/nonexistent/command", stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 127)
        noexec = os.path.join(self.root, "noexec")
        open(noexec, "w").close()
        p = self.cli("run", "--", noexec, stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 126)
        for bad in (["--need", "cpu=2..1"], ["--need", "cpu=1..257"], ["--need", "cpu=1", "--need", "cpu=2"],
                    ["--bogus"], []):
            p = self.cli("run", *bad, *(["--", "true"] if bad else []), stderr=subprocess.DEVNULL)
            self.assertEqual(p.wait(timeout=20), 64, bad)
        self.assertEqual(self.ls("queue"), [])

    def test_pass_through(self):
        self.config("[resources]\ncpu = 4\npg-local = 2\n")
        out = os.path.join(self.root, "inner.json")
        inner = [sys.executable, CLIENT, "run", "--need", "cpu=2..3", "--need", "gpu=1", "--",
                 sys.executable, "-c", REPORTER, out]
        p = self.cli("run", "--need", "cpu=4..4", "--need", "pg-local=1", "--", *inner,
                     stderr=subprocess.PIPE)
        _, err = p.communicate(timeout=20)
        self.assertEqual(p.returncode, 0, err)
        rep = load(out)
        self.assertEqual(rep["env"], {"CORESLOT_STATE_DIR": self.S, "CORESLOT_GRANTED_CPU": "3",
                                      "CORESLOT_GRANTED_PG_LOCAL": "1", "CORESLOT_GRANTED_GPU": "1",
                                      "CORESLOT_HELD_FD": "3", "CORESLOT_TOKEN_FDS": "4-8"})
        self.assertIn(b"gpu: not held by the parent run", err)
        # A context that does not validate is a refusal: the descriptors are not open here.
        env = dict(self.env, CORESLOT_TOKEN_FDS="40-41")
        p = self.cli("run", "--", "true", env=env, stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 75)
        self.assertEqual(self.ls("queue"), [])


class AlignmentTests(Base):
    """Behaviour the resolved PROTOCOL.md (merge 5c00392) and the maintainers' B1/D6 decision require."""

    def _real_config_client(self, home: str):
        """A client with no override whose password-database home is `home`."""
        saved_home, saved_env = cc.password_home, os.environ.pop("CORESLOT_STATE_DIR", None)
        cc.password_home = lambda: home
        self.addCleanup(setattr, cc, "password_home", saved_home)
        if saved_env is not None:
            self.addCleanup(os.environ.__setitem__, "CORESLOT_STATE_DIR", saved_env)
        return cc.Client(diag=lambda m: None)

    def test_real_config_dangling_symlink_is_missing_and_fifo_is_invalid(self):
        home = os.path.join(self.root, "home")
        cfgdir = os.path.join(home, ".config", "coreslot")
        os.makedirs(cfgdir)
        path = os.path.join(cfgdir, "config.toml")
        os.symlink(os.path.join(self.root, "nowhere.toml"), path)
        c = self._real_config_client(home)
        self.assertEqual(c.config_path(), path)
        self.assertEqual(c.config().size("cpu", 14), 14)  # the defaults
        target = os.path.join(self.root, "real.toml")
        with open(target, "w") as fh:
            fh.write("[resources]\ncpu = 3\n")
        os.chmod(target, 0o666)  # no owner or mode check on the real configuration
        os.unlink(path)
        os.symlink(target, path)
        self.assertEqual(c.config().size("cpu", 14), 3)
        os.unlink(path)
        os.mkdir(path)
        with self.assertRaises(cc.StateError):
            c.config()
        os.rmdir(path)
        os.mkfifo(path)
        # In a subprocess, so a client that blocks on the FIFO fails the test instead of hanging it.
        probe = ("import sys; sys.path.insert(0, sys.argv[1]); import coreslot_client as cc; "
                 "cc.password_home = lambda: sys.argv[2]\n"
                 "try:\n    cc.Client().config(); print('read')\n"
                 "except cc.StateError as e:\n    print('state-error', e.reason)")
        env = {k: v for k, v in os.environ.items() if k != "CORESLOT_STATE_DIR"}
        out = subprocess.run([sys.executable, "-c", probe, HERE, home], env=env, capture_output=True,
                             text=True, timeout=10)
        self.assertEqual(out.stdout.strip(), "state-error config")

    def test_override_config_fifo_does_not_block(self):
        os.makedirs(self.S, 0o700)
        os.mkfifo(os.path.join(self.S, "config.toml"), 0o600)
        probe = ("import sys; sys.path.insert(0, sys.argv[1]); import coreslot_client as cc\n"
                 "try:\n    cc.Client(sys.argv[2]).config(); print('read')\n"
                 "except cc.StateError as e:\n    print('state-error')")
        out = subprocess.run([sys.executable, "-c", probe, HERE, self.S], capture_output=True, text=True,
                             timeout=10)
        self.assertEqual(out.stdout.strip(), "state-error")

    def test_negative_gen_reads_as_malformed(self):
        self.write(os.path.join(self.S, "gen"), b'{"gen": -1, "reconciled_ns": 5}')
        self.assertEqual(self.client()._read_gen(), (0, 0))
        self.write(os.path.join(self.S, "gen"), b'{"gen": 0, "reconciled_ns": 5}')
        self.assertEqual(self.client()._read_gen(), (0, 5))

    def test_token_only_rejoins_the_queue_when_the_format_is_understood_again(self):
        """D8, §15.2: FORMAT 2 → token-only; FORMAT back at 1 (or missing) → a new queued request."""
        self.config("[resources]\ncpu = 1\n")
        self.lock(os.path.join(self.S, "res", "cpu", "0"))  # nothing free: it stays waiting
        self.write(os.path.join(self.S, "FORMAT"), b"coreslot-protocol 2\n")
        t = self.client().enqueue("x", 0, {"cpu": 1})
        self.assertTrue(t.token_only)
        self.assertEqual(t.step(), ("waiting", 0))
        self.assertEqual(self.ls("queue"), [])
        self.write(os.path.join(self.S, "FORMAT"), b"coreslot-protocol 1\n")
        self.assertEqual(t.step(), ("waiting", 1))
        self.assertFalse(t.token_only)
        self.assertEqual(self.ls("queue"), [t.name])
        self.assertIs(cc._probe_path(os.path.join(self.S, "queue", t.name)), False)
        t.withdraw()
        # FORMAT missing: the entry writes it, and the token-only client queues too.
        self.write(os.path.join(self.S, "FORMAT"), b"coreslot-protocol 7\n")
        u = self.client().enqueue("y", 0, {"cpu": 1})
        self.assertTrue(u.token_only)
        os.unlink(os.path.join(self.S, "FORMAT"))
        self.assertEqual(u.step(), ("waiting", 1))
        self.assertEqual(self.read("FORMAT"), b"coreslot-protocol 1\n")
        self.assertIn(u.name, self.ls("queue"))
        u.withdraw()

    def test_empty_context_variables_are_unset(self):
        out = os.path.join(self.root, "out.json")
        env = dict(self.env, CORESLOT_TOKEN_FDS="", CORESLOT_UNBUDGETED="", CORESLOT_HELD_FD="")
        p = self.cli("run", "--need", "cpu=1", "--", sys.executable, "-c", REPORTER, out, env=env)
        self.assertEqual(p.wait(timeout=20), 0)  # an ordinary request, not a refusal
        rep = load(out)["env"]
        self.assertEqual((rep["CORESLOT_GRANTED_CPU"], rep["CORESLOT_TOKEN_FDS"], rep["CORESLOT_HELD_FD"]),
                         ("1", "4", "3"))
        self.assertNotIn("CORESLOT_UNBUDGETED", rep)

    def test_token_fds_bound_is_512(self):
        self.assertEqual(len(cc.parse_token_fds("4-515")), 512)
        self.assertEqual(len(cc.parse_token_fds("4-259,300-555")), 512)
        for raw in ("4-516", "4-259,300-556", "0-99999999999999"):
            with self.assertRaises(ValueError, msg=raw):
                cc.parse_token_fds(raw)

    def _log(self) -> list[dict]:
        try:
            return [json.loads(line) for line in self.read("log", "runs.jsonl").splitlines()]
        except FileNotFoundError:
            return []

    def test_nested_runs_append_a_nested_log_line(self):
        """E6, §20: pass-through and inherited-unbudgeted runs log `"nested": true`, outside the section."""
        self.config("[resources]\ncpu = 4\n")
        inner = [sys.executable, CLIENT, "run", "--label", "inner", "--need", "cpu=2..3", "--", "true"]
        p = self.cli("run", "--label", "outer", "--need", "cpu=4..4", "--", *inner)
        self.assertEqual(p.wait(timeout=20), 0)
        log = self._log()
        self.assertEqual([(r["label"], r.get("nested", False)) for r in log],
                         [("inner", True), ("outer", False)])
        self.assertEqual(log[0]["grant"], {"cpu": 3})
        env = dict(self.env, CORESLOT_UNBUDGETED="parent-reason")
        p = self.cli("run", "--label", "unb", "--need", "cpu=2", "--", "true", env=env,
                     stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 0)
        last = self._log()[-1]
        self.assertEqual((last["label"], last["nested"], last["fail_open"], last["grant"]),
                         ("unb", True, "parent-reason", {"cpu": 2}))
        self.assertNotIn("degraded", os.listdir(self.S))  # rule 1 writes no degraded record

    def test_nested_run_writes_nothing_without_an_understood_format(self):
        os.makedirs(self.S, 0o700)
        env = dict(self.env, CORESLOT_UNBUDGETED="x")
        p = self.cli("run", "--", "true", env=env, stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 0)
        self.assertEqual(os.listdir(self.S), [])  # no FORMAT written, no log/

    def test_bad_entries_are_ignored_counted_and_kept_by_reconciliation(self):
        """B1/D6 (maintainers' decision): matching names with a bad type/owner/mode are never trusted or removed."""
        now = time.time_ns()
        self.write(os.path.join(self.S, "gen"), json.dumps({"gen": 5, "reconciled_ns": 0}).encode())
        q, h, c = (os.path.join(self.S, d) for d in ("queue", "holds", "cancel"))
        elsewhere = os.path.join(self.root, "elsewhere")
        self.write(elsewhere, body())
        os.makedirs(q, 0o700)
        os.makedirs(c, 0o700)
        os.symlink(elsewhere, os.path.join(q, "00000000000000000001-0000000001-000001"))
        os.makedirs(os.path.join(h, "00000000000000000002-0000000001-000002"), 0o700)
        writable = os.path.join(q, "00000000000000000003-0000000001-000003")
        self.write(writable, body())
        os.chmod(writable, 0o622)
        os.symlink(elsewhere, os.path.join(c, "00000000000000000009-0000000001-000009"))  # orphan
        old_tmp = os.path.join(h, ".tmp-0000000001-000004")
        os.symlink(elsewhere, old_tmp)
        os.utime(old_tmp, ns=(now - 120 * 10**9,) * 2, follow_symlinks=False)
        dead = "00000000000000000004-0000000001-000005"
        self.write(os.path.join(q, dead), body())  # a well-formed dead ticket is still removed
        c1 = self.client()
        t = c1.enqueue("x", 0, {"cpu": 1})  # reconciliation is due
        self.assertEqual(sorted(self.ls("queue")), sorted(["00000000000000000001-0000000001-000001",
                                                            "00000000000000000003-0000000001-000003",
                                                            t.name]))
        self.assertEqual(self.ls("holds"), sorted(["00000000000000000002-0000000001-000002",
                                                   ".tmp-0000000001-000004"]))
        self.assertEqual(self.ls("cancel"), ["00000000000000000009-0000000001-000009"])
        self.assertEqual(c1.ignored_by_reconciliation, {"queue": 2, "holds": 2, "cancel": 1})
        g = self.gen()
        self.assertEqual(g["gen"], 5 + 1 + 1)  # reconciliation removed `dead`, then enqueue bumped
        self.assertLess(abs(g["reconciled_ns"] - time.time_ns()), 5 * 10**9)
        kind, grant = t.step()  # the bad tickets ahead are ignored, not trusted
        self.assertEqual(kind, "admitted")
        self.assertEqual(c1.ignored_entries, 2)
        grant.close(remove_hold=False)

    def test_stray_temporary_in_the_state_directory_is_a_state_error(self):
        """§4.2 (resolution): the stray rule covers queue/, holds/, cancel/ only, not $S."""
        for make, reason in ((lambda p: os.symlink(os.path.join(self.root, "elsewhere"), p), "state-type"),
                             (lambda p: os.mkdir(p, 0o700), "state-type"),
                             (lambda p: (self.write(p, b""), os.chmod(p, 0o620)), "state-mode")):
            with self.subTest(reason=reason):
                shutil.rmtree(self.S, ignore_errors=True)
                os.makedirs(self.S, 0o700)
                self.write(os.path.join(self.S, "gen"), b'{"gen": 3, "reconciled_ns": 0}')  # due
                stray = os.path.join(self.S, ".tmp-0000000001-000001")
                make(stray)
                with self.assertRaises(cc.StateError) as ctx:
                    self.client().enqueue("x", 0, {"cpu": 1})
                self.assertEqual(ctx.exception.reason, reason)
                self.assertTrue(os.path.lexists(stray))
                self.assertEqual(self.ls("queue"), [])  # nothing published after the state error
                self.assertEqual(self.gen(), {"gen": 3, "reconciled_ns": 0})  # gen not written
        # The same stray inside queue/ is ignored and counted, and reconciliation completes.
        shutil.rmtree(self.S, ignore_errors=True)
        os.makedirs(os.path.join(self.S, "queue"), 0o700)
        self.write(os.path.join(self.S, "gen"), b'{"gen": 3, "reconciled_ns": 0}')
        os.symlink(os.path.join(self.root, "elsewhere"), os.path.join(self.S, "queue", ".tmp-0000000001-000001"))
        c = self.client()
        t = c.enqueue("x", 0, {"cpu": 1})
        self.assertEqual(c.ignored_by_reconciliation["queue"], 1)
        self.assertGreater(self.gen()["reconciled_ns"], 0)
        t.withdraw()

    def test_fd_3_is_closed_in_the_child_without_a_hold(self):
        """§17: a token-only run passes no hold, so fd 3 is closed even when the run inherited one."""
        self.config("[resources]\ncpu = 2\n")
        self.write(os.path.join(self.S, "FORMAT"), b"coreslot-protocol 2\n")
        out = os.path.join(self.root, "out.json")
        cmd = (f"exec 3</dev/null; exec '{sys.executable}' '{CLIENT}' run --need cpu=1 -- "
               f"'{sys.executable}' -c \"$REPORTER\" '{out}'")
        p = self.cli_sh(cmd, env=dict(self.env, REPORTER=REPORTER))
        self.assertEqual(p.wait(timeout=20), 0)
        rep = load(out)
        self.assertEqual(sorted(rep["fds"]), ["4"])
        self.assertEqual(rep["env"]["CORESLOT_UNBUDGETED"], "format")

    def cli_sh(self, script: str, **kw) -> subprocess.Popen:
        p = subprocess.Popen(["sh", "-c", script], env=kw.pop("env", self.env), stderr=subprocess.DEVNULL,
                             **kw)
        self.procs.append(p)
        return p

    def test_link_unlink_fallback_and_seq_retry_on_a_taken_name(self):
        """§11 step 4 without renameat2/renamex_np: clean publish, then EEXIST → new temporary, next seq."""
        saved = cc._NOREPLACE
        for native in (False, True):
            with self.subTest(native=native):
                cc._NOREPLACE = saved if native else None
                try:
                    shutil.rmtree(self.S, ignore_errors=True)
                    c = self.client()
                    t = c.enqueue("x", 0, {"cpu": 1})
                    self.assertEqual(self.ls("queue"), [t.name])  # no temporary left behind
                    self.assertEqual(os.stat(os.path.join(self.S, "queue", t.name)).st_nlink, 1)
                    self.assertIs(cc._probe_path(os.path.join(self.S, "queue", t.name)), False)
                    d = os.path.join(self.S, "holds")
                    os.makedirs(d, 0o700, exist_ok=True)
                    taken = "00000000000000000001-0000000001-000001"
                    self.write(os.path.join(d, taken), b"first")
                    calls = []

                    def make_name(seq):
                        calls.append(seq)
                        return taken if len(calls) == 1 else f"00000000000000000001-0000000001-{seq:06d}x"

                    fd, name = c._publish_named(d, make_name, b"second", lock=True)
                    self.fds.append(fd)
                    self.assertEqual(len(calls), 2)
                    self.assertNotEqual(calls[0], calls[1])
                    self.assertEqual(self.read("holds", taken), b"first")  # never replaced
                    self.assertEqual(self.read("holds", name), b"second")
                    self.assertEqual(self.ls("holds"), sorted([taken, name]))  # no temporaries
                    self.assertIs(cc._probe_path(os.path.join(d, name)), False)
                    t.withdraw()
                finally:
                    cc._NOREPLACE = saved

    def test_surviving_grandchild_keeps_the_hold(self):
        """§12.3: the hold is unlinked only if its probe succeeds; a grandchild still holds fd 3."""
        pidfile = os.path.join(self.root, "pid")
        p = self.cli("run", "--need", "cpu=1", "--", "sh", "-c", f"sleep 60 & echo $! > '{pidfile}'",
                     stdout=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 0)
        gpid = int(load(pidfile, raw=True))
        self.kill_pids = [gpid]
        [hold] = self.ls("holds")
        self.assertIs(cc._probe_path(os.path.join(self.S, "holds", hold)), False)
        self.assertIs(cc._probe_path(os.path.join(self.S, "res", "cpu", "0")), False)
        os.kill(gpid, signal.SIGKILL)
        self.wait_for(lambda: cc._probe_path(os.path.join(self.S, "holds", hold)) is True,
                      what="the grandchild's exit to free the hold")

    def test_admission_step_1_cancel_check_alone(self):
        """§13.1/§12.1 step 1: a cancel published after the poll read cancel/, before the section."""
        t = self.client().enqueue("b", 0, {"cpu": 1})
        canceller = self.client()
        real_elect = t._elect

        def elect_then_cancel():
            position = real_elect()  # the poll's own cancel check has already run
            self.assertTrue(canceller.cancel(t.id))
            return position

        t._elect = elect_then_cancel
        with self.assertRaises(cc.Cancelled):
            t.step()
        self.assertEqual((self.ls("queue"), self.ls("cancel"), self.ls("holds")), ([], [], []))

    def _hold_admit_lock(self) -> int:
        os.makedirs(self.S, 0o700, exist_ok=True)
        return self.lock(os.path.join(self.S, "admit.lock"))

    def test_cli_timeout_bounds_section_entry(self):
        self._hold_admit_lock()
        started = time.monotonic()
        p = self.cli("run", "--timeout", "1", "--", "true", stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=10), 75)
        self.assertLess(time.monotonic() - started, 6)
        p = self.cli("cancel", "--timeout", "0.5", "01791720000000000000-0000004711-000001",
                     stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=10), 75)

    def test_ctrl_c_during_enqueue_exits_130_without_a_ticket(self):
        self._hold_admit_lock()
        p = self.cli("run", "--", "true", stderr=subprocess.PIPE)
        time.sleep(0.5)
        p.send_signal(signal.SIGINT)
        _, err = p.communicate(timeout=10)
        self.assertEqual(p.returncode, 130)
        self.assertNotIn(b"Traceback", err)
        self.assertEqual(self.ls("queue"), [])

    def test_context_managers(self):
        self.config("[resources]\ncpu = 1\n")
        c = self.client()
        with c.enqueue("a", 0, {"cpu": 1}) as a:
            with a.wait(5) as grant:
                [hold] = self.ls("holds")
                with c.enqueue("b", 0, {"cpu": 1}) as b:
                    self.assertEqual(b.step(), ("waiting", 1))
                    path = os.path.join(self.S, "queue", b.name)
                self.assertIs(cc._probe_path(path), None if not os.path.exists(path) else True)
                self.assertEqual(self.ls("queue"), [])  # withdrawn and removed on exit
            self.assertTrue(grant.closed)
            self.assertEqual(self.ls("holds"), [])  # closed and removed on exit
        self.assertIs(cc._probe_path(os.path.join(self.S, "res", "cpu", "0")), True)

    def test_fail_open_hold_bumps_gen(self):
        self.config("[resources]\ncpu = 0\n")
        p = self.cli("run", "--", "true", stderr=subprocess.DEVNULL)
        self.assertEqual(p.wait(timeout=20), 0)
        # First entry reconciles (no removal) and publishes the hold (+1); removing it bumps again (+1).
        self.assertEqual(self.gen()["gen"], 2)
        self.assertEqual(self.ls("holds"), [])


if __name__ == "__main__":
    unittest.main()
