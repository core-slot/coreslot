"""Tests of the Claude Code hook: `python3 -m unittest` in integrations/claude-code.

Each invariant test names, in its docstring, the mutation of `coreslot_hook.py` that turns it red.
"""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HOOK = Path(__file__).resolve().parents[1] / "hooks" / "coreslot_hook.py"
_spec = importlib.util.spec_from_file_location("coreslot_hook", HOOK)
hook = importlib.util.module_from_spec(_spec)
sys.modules["coreslot_hook"] = hook
_spec.loader.exec_module(hook)

SHAPES = hook.load_shapes(hook.DEFAULT_SHAPES)


def payload(command: str, session: str = "s1", agent: str = "", tool: str = "Bash") -> str:
    return json.dumps(
        {
            "session_id": session,
            "agent_id": agent,
            "hook_event_name": "PreToolUse",
            "tool_name": tool,
            "tool_input": {"command": command},
            "cwd": "/tmp",
        }
    )


def offered(command: str) -> str | None:
    found = hook.offer(command, SHAPES)
    return None if found is None else found[0]


class Splitting(unittest.TestCase):
    def test_gives_up_on_what_it_cannot_split_with_certainty(self) -> None:
        """Mutation: treat `(` and `)` as ordinary bytes -> `(a)` and `a $(b)` split."""
        for command in ["a $(b)", "a `b`", "a <<EOF\nb\nEOF", "a <<< b", "(a)", "a; { b; }",
                        "a 'b", 'a "b', "a # c", "a \\", "a \\\n b", "a $'b'", "a <(b)",
                        "a && && b", "; a"]:
            with self.subTest(command=command):
                self.assertIsNone(hook.split_segments(command.encode("utf-8")))

    def test_splits_on_separators_outside_quotes_only(self) -> None:
        """Mutation: treat `'` as an ordinary byte -> `'x;y'` splits on its `;`."""
        cases = [
            ("a && b || c ; d | e |& f & g\nh", ["a", "b", "c", "d", "e", "f", "g", "h"]),
            ("a 2>&1 | b", ["a 2>&1", "b"]),
            ("a &> out ; b >| f", ["a &> out", "b >| f"]),
            ("a 'x;y' \"p|q\" k\\;l", ["a 'x;y' \"p|q\" k\\;l"]),
            ("a#b c", ["a#b c"]),
            ("a &", ["a"]),
            ("\na\n\nb\n", ["a", "b"]),
        ]
        for command, pieces in cases:
            raw = command.encode("utf-8")
            with self.subTest(command=command):
                segments = hook.split_segments(raw)
                self.assertIsNotNone(segments)
                self.assertEqual([raw[s.start:s.end].strip().decode() for s in segments], pieces)

    def test_offsets_are_utf8_bytes(self) -> None:
        """Offsets count UTF-8 bytes, so a multi-byte character before the segment keeps its place."""
        self.assertEqual(
            offered("echo 'é—ж' && X=1 go test ./..."),
            "echo 'é—ж' && X=1 coreslot run --label go-test -- go test ./...",
        )


class Offering(unittest.TestCase):
    def test_inserts_at_the_program_word_after_assignments(self) -> None:
        """Mutation: insert at the segment's start -> `coreslot run ... -- GOFLAGS=-race go`."""
        self.assertEqual(
            offered("GOFLAGS=-race CGO_ENABLED=1 go test ./..."),
            "GOFLAGS=-race CGO_ENABLED=1 coreslot run --label go-test -- go test ./...",
        )

    def test_every_matching_segment_is_offered_and_every_other_byte_kept(self) -> None:
        """Mutation: stop after the first insert -> the cargo segment is left bare."""
        self.assertEqual(
            offered("cd 'my repo' &&  go test -race ./... ;cargo test  2>&1 | tail -5"),
            "cd 'my repo' &&  coreslot run --label go-test -- go test -race ./... ;"
            "coreslot run --label cargo -- cargo test  2>&1 | tail -5",
        )

    def test_requires_one_of_its_words_or_a_word_starting_with_one(self) -> None:
        """Mutation: drop the `requires` check -> `go test ./internal/x` and serial pytest match."""
        self.assertEqual(offered("make -j8 all"), "coreslot run --label make -- make -j8 all")
        self.assertEqual(offered("uv run pytest -n4"), "coreslot run --label pytest -- uv run pytest -n4")
        for command in ["go test ./internal/x", "pytest tests/unit", "make fmt"]:
            with self.subTest(command=command):
                self.assertIsNone(offered(command))

    def test_a_path_shaped_program_may_be_spelled_with_a_leading_dot_slash(self) -> None:
        """Mutation: compare program words with == only -> `./.venv/bin/pytest -n 4` passes."""
        self.assertEqual(
            offered("./.venv/bin/pytest -n 4"),
            "coreslot run --label pytest -- ./.venv/bin/pytest -n 4",
        )

    def test_a_wrapped_or_unknown_command_is_not_offered(self) -> None:
        for command in ["coreslot run --label go-test -- go test ./...", "ls -la",
                        "ssh host go test ./...", "A=1", "echo $(go test ./...)"]:
            with self.subTest(command=command):
                self.assertIsNone(offered(command))


class ShapeFile(unittest.TestCase):
    def write(self, document: object) -> Path:
        directory = Path(tempfile.mkdtemp())
        path = directory / "shapes.json"
        path.write_text(json.dumps(document), encoding="utf-8")
        return path

    def test_a_label_a_shell_would_read_is_refused(self) -> None:
        """Mutation: drop the LABEL check -> a label of `x;rm` loads and lands in the offer."""
        for label in ["x;rm", "a b", "$(x)", "", "-x", "a@4"]:
            with self.subTest(label=label):
                path = self.write({"shapes": [{"label": label, "programs": [["go"]]}]})
                with self.assertRaises(ValueError):
                    hook.load_shapes(path)

    def test_unknown_keys_and_empty_programs_are_refused(self) -> None:
        for document in [{"shape": []}, {"shapes": [{"label": "a", "programs": []}]},
                         {"shapes": [{"label": "a", "programs": [[]]}]},
                         {"shapes": [{"label": "a", "programs": [["go"]], "need": "cpu=2"}]}]:
            with self.subTest(document=document):
                with self.assertRaises(ValueError):
                    hook.load_shapes(self.write(document))


class Responding(unittest.TestCase):
    def setUp(self) -> None:
        self.home = Path(tempfile.mkdtemp())

    def respond(self, stdin: str, has_coreslot: bool = True) -> dict | None:
        out = hook.respond(stdin, self.home, has_coreslot)
        return json.loads(out) if out else None

    def test_a_refusal_denies_with_the_offer(self) -> None:
        decision = self.respond(payload("go test ./..."))["hookSpecificOutput"]
        self.assertEqual(decision["hookEventName"], "PreToolUse")
        self.assertEqual(decision["permissionDecision"], "deny")
        self.assertIn("\ncoreslot run --label go-test -- go test ./...\n",
                      decision["permissionDecisionReason"])

    def test_an_identical_retry_passes_for_that_session_and_agent_only(self) -> None:
        """Mutation: make refused_before always return False -> the retry is refused again."""
        self.assertIsNotNone(self.respond(payload("go test ./...")))
        self.assertIsNone(self.respond(payload("go test ./...")))
        self.assertIsNotNone(self.respond(payload("go test ./...", agent="sub")))
        self.assertIsNotNone(self.respond(payload("go test ./...", session="s2")))
        self.assertIsNotNone(self.respond(payload("go test -race ./...")))

    def test_the_ledger_holds_digests_not_commands(self) -> None:
        self.respond(payload("go test -run SecretName ./..."))
        (ledger,) = (self.home / hook.STATE).iterdir()
        self.assertNotIn("SecretName", ledger.read_text())
        self.assertEqual(ledger.stat().st_mode & 0o777, 0o600)

    def test_passes_without_coreslot_or_outside_bash(self) -> None:
        """Mutation: drop the `has_coreslot` check -> a machine without coreslot is refused."""
        self.assertIsNone(self.respond(payload("go test ./..."), has_coreslot=False))
        self.assertIsNone(self.respond(payload("go test ./...", tool="Edit")))

    def test_the_user_shape_file_replaces_the_defaults(self) -> None:
        user = self.home / hook.USER_SHAPES
        user.parent.mkdir(parents=True)
        user.write_text(json.dumps({"shapes": [{"label": "unit", "programs": [["./ci.sh"]]}]}))
        self.assertIsNone(self.respond(payload("go test ./...")))
        reason = self.respond(payload("./ci.sh"))["hookSpecificOutput"]["permissionDecisionReason"]
        self.assertIn("\ncoreslot run --label unit -- ./ci.sh\n", reason)

    def test_its_own_failure_is_a_pass(self) -> None:
        """Mutation: re-raise in respond's handler -> a broken shape file blocks every command."""
        user = self.home / hook.USER_SHAPES
        user.parent.mkdir(parents=True)
        user.write_text("{not json")
        self.assertIsNone(self.respond(payload("go test ./...")))
        self.assertIsNone(self.respond("not json"))
        self.assertIsNone(self.respond(json.dumps({"tool_name": "Bash", "tool_input": {}})))


class Process(unittest.TestCase):
    def test_the_script_exits_0_and_prints_only_a_refusal(self) -> None:
        bin_dir = Path(tempfile.mkdtemp())
        fake = bin_dir / "coreslot"
        fake.write_text("#!/bin/sh\nexit 0\n")
        fake.chmod(0o755)
        env = {**os.environ, "PATH": f"{bin_dir}{os.pathsep}{os.environ.get('PATH', '')}"}
        for stdin, prints in [("garbage", False), (payload("ls"), False)]:
            with self.subTest(stdin=stdin):
                result = subprocess.run([sys.executable, str(HOOK)], input=stdin, env=env,
                                        capture_output=True, text=True, check=False)
                self.assertEqual(result.returncode, 0)
                self.assertEqual(bool(result.stdout), prints)
                self.assertEqual(result.stderr, "")


if __name__ == "__main__":
    unittest.main()
