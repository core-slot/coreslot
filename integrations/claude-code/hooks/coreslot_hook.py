#!/usr/bin/env python3
"""Refuse a heavy Bash command once, and offer it back under coreslot's memory guard.

A Claude Code `PreToolUse` hook on `Bash`. When a command runs one of the heavy shapes of the
shape file (`go test ./...`, `cargo test`, `pytest -n`, ...) and `coreslot` is on `PATH`, it
denies the call with the same command offered back: `coreslot run --restart=yes -- ` inserted at
the program word of every recognised segment, every other byte unchanged. The model then runs
the offered command, which starts when the machine has the memory for it instead of piling onto
a machine that is already short; it goes through the user's permission rules like any other
command. coreslot names the command by itself, so the offer carries no label.

`--restart=yes` declares the command safe to kill at any moment and to run again from scratch,
which coreslot does when memory runs short. Every shape must therefore be a test suite, a build
or a check that writes nothing outside its own output: a shape that can deploy, migrate or push
(`make -j`, Gradle, `xcodebuild`, whose targets can do anything) does not belong in the file.

It is a nudge, not containment: a rephrased command escapes it. So it passes, silently, whenever
it is not sure:

- `coreslot` is not on `PATH`;
- the command cannot be split with certainty (`$(`, backticks, heredocs, subshells, `{` or `}`
  even quoted, `[[`, unbalanced quotes, line continuations, `#` comments), or no segment
  matches a shape (a command that already starts with `coreslot` matches none);
- the same command was refused before for the same session and agent: from then on it passes
  every time that session and agent send it, outside coreslot, for as long as the record is
  kept (below), so a user who asks for a bare run gets one;
- anything of its own goes wrong.

The shapes come from `~/.config/coreslot/claude-code-shapes.json` when that file exists, else
from `shapes.json` beside this file. The refusals are remembered in
`~/.local/state/coreslot-claude-code/`, one file per session and agent holding the SHA-256 of
each refused command, never the command; the next refusal deletes every file whose last
refusal is more than seven days old. The home directory comes from the password database, as
coreslot's own does. The exit status is always 0, and the hook prints nothing but a refusal.
Python 3.9 or newer, standard library only.
"""

from __future__ import annotations

import hashlib
import json
import os
import pwd
import re
import shutil
import sys
import time
from pathlib import Path
from typing import NamedTuple

HERE = Path(__file__).resolve().parent
DEFAULT_SHAPES = HERE / "shapes.json"
USER_SHAPES = Path(".config", "coreslot", "claude-code-shapes.json")
STATE = Path(".local", "state", "coreslot-claude-code")
KEEP_SECONDS = 7 * 24 * 3600
PREFIX = b"coreslot run --restart=yes -- "

_ASSIGNMENT = re.compile(rb"[A-Za-z_][A-Za-z0-9_]*=")
_BLANK = b" \t"


class Segment(NamedTuple):
    """One simple command of a command line, as UTF-8 byte offsets `[start, end)`."""

    start: int
    end: int


class Word(NamedTuple):
    start: int
    end: int
    value: str  # with the quoting removed


class Shape(NamedTuple):
    programs: tuple[tuple[str, ...], ...]
    requires: tuple[str, ...]


# --- the shape file -------------------------------------------------------------------------------


def load_shapes(path: Path) -> tuple[Shape, ...]:
    """The shapes of a shape file; raises on anything it does not understand."""
    document = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(document, dict) or set(document) - {"shapes"}:
        raise ValueError("a shape file is an object with one key, shapes")
    shapes = []
    for entry in document.get("shapes", []):
        if not isinstance(entry, dict) or set(entry) - {"programs", "requires"}:
            raise ValueError("a shape has programs and optionally requires")
        programs, requires = entry.get("programs"), entry.get("requires", [])
        if not isinstance(programs, list) or not programs:
            raise ValueError("a shape needs programs")
        for program in programs + [requires]:
            if not isinstance(program, list) or not all(isinstance(w, str) and w for w in program):
                raise ValueError("programs and requires are lists of words")
        if any(not program for program in programs):
            raise ValueError("a program has at least one word")
        shapes.append(Shape(tuple(tuple(p) for p in programs), tuple(requires)))
    return tuple(shapes)


# --- splitting ------------------------------------------------------------------------------------


def _double_quoted_end(command: bytes, i: int) -> int | None:
    """The offset after the `"` that closes the one at `i`; None when there is none, or when
    the quoted text substitutes a command or continues a line."""
    j = i + 1
    while j < len(command):
        c = command[j]
        if c == 0x22:  # "
            return j + 1
        if c == 0x5C:  # backslash
            if j + 1 >= len(command) or command[j + 1] == 0x0A:
                return None
            j += 2
            continue
        if c == 0x60 or command[j : j + 2] == b"$(":  # ` or $(
            return None
        j += 1
    return None


def split_segments(command: bytes) -> list[Segment] | None:
    """The simple commands of `command`, split on `&&`, `||`, `;`, `|`, `|&`, `&` and newline
    outside quotes; None when it cannot be split with certainty.

    A segment runs from just after one separator to the start of the next. Blank segments (a
    trailing `&` or `;`, an empty line) are dropped; one anywhere else is a syntax error and
    gives None.

    A `{` or `}` anywhere, even quoted, gives None: `${...}` nests quotes this splitter does not
    model. So does `[[` anywhere, whose `&&` and `||` do not separate commands.
    """
    if any(mark in command for mark in (b"{", b"}", b"[[")):
        return None
    raw: list[tuple[int, int, bytes | None, bytes | None]] = []
    start, before = 0, None
    at_word_start = True
    i, n = 0, len(command)

    def close(end: int, separator: bytes | None) -> None:
        nonlocal start, before
        raw.append((start, end, before, separator))
        start, before = end + len(separator or b""), separator

    while i < n:
        c = command[i]
        if c == 0x27:  # '
            j = command.find(b"'", i + 1)
            if j < 0:
                return None
            i, at_word_start = j + 1, False
        elif c == 0x22:  # "
            end = _double_quoted_end(command, i)
            if end is None:
                return None
            i, at_word_start = end, False
        elif c == 0x5C:  # backslash: escapes one byte; a trailing one or a continuation gives up
            if i + 1 >= n or command[i + 1] == 0x0A:
                return None
            i, at_word_start = i + 2, False
        elif c in b"`()":
            return None
        elif c == 0x24 and command[i + 1 : i + 2] == b"'":  # $'...' quoting
            return None
        elif c == 0x23 and at_word_start:  # a comment
            return None
        elif c in b"<>":
            if command[i : i + 2] == b"<<":  # heredoc, here-string
                return None
            follow = b">&|" if c == 0x3E else b"&>"
            i += 2 if command[i + 1 : i + 2] and command[i + 1] in follow else 1
            at_word_start = True
        elif c == 0x26:  # &
            if command[i + 1 : i + 2] == b">":  # &> and &>> redirect, they do not separate
                i += 3 if command[i + 2 : i + 3] == b">" else 2
                at_word_start = True
            else:
                separator = b"&&" if command[i + 1 : i + 2] == b"&" else b"&"
                close(i, separator)
                i, at_word_start = i + len(separator), True
        elif c == 0x7C:  # |
            separator = command[i : i + 2] if command[i + 1 : i + 2] in (b"|", b"&") else b"|"
            close(i, separator)
            i, at_word_start = i + len(separator), True
        elif c in b";\n":
            close(i, bytes([c]))
            i, at_word_start = i + 1, True
        else:
            at_word_start = c in _BLANK
            i += 1
    close(n, None)

    segments = []
    for begin, end, preceding, following in raw:
        if command[begin:end].strip(_BLANK):
            segments.append(Segment(begin, end))
        elif following == b"\n" or (following is None and preceding in (None, b"\n", b";", b"&")):
            continue
        else:
            return None
    return segments


def words(command: bytes, segment: Segment) -> list[Word]:
    """The words of a segment `split_segments` returned, with their quoting removed."""
    found = []
    i = segment.start
    while i < segment.end:
        if command[i] in _BLANK:
            i += 1
            continue
        begin, value = i, bytearray()
        while i < segment.end and command[i] not in _BLANK:
            c = command[i]
            if c == 0x27:
                j = command.index(b"'", i + 1)
                value += command[i + 1 : j]
                i = j + 1
            elif c == 0x22:
                j = i + 1
                while command[j] != 0x22:
                    if command[j] == 0x5C and command[j + 1] in b'"\\$`':
                        j += 1
                    value.append(command[j])
                    j += 1
                i = j + 1
            elif c == 0x5C:
                value.append(command[i + 1])
                i += 2
            else:
                value.append(c)
                i += 1
        found.append(Word(begin, i, value.decode("utf-8")))
    return found


def program_index(command: bytes, found: list[Word]) -> int | None:
    """The index of the program word, after any leading `NAME=value` assignments; None when
    the segment is assignments alone."""
    for index, word in enumerate(found):
        assignment = _ASSIGNMENT.match(command, word.start)
        if assignment is None or assignment.end() > word.end:
            return index
    return None


# --- recognising ----------------------------------------------------------------------------------


def _same_word(word: str, expected: str) -> bool:
    """A program word as the shape spells it, or a path-shaped one with a leading `./`."""
    return word == expected or ("/" in expected and word == "./" + expected)


def matches(values: list[str], shapes: tuple[Shape, ...]) -> bool:
    """Whether a shape matches `values`, the words from the program word on.

    A shape matches when the words begin with one of its programs and, when it lists
    `requires`, one of the remaining words equals or starts with one of them (`-j` takes
    `-j8`, `-n` takes `-n4`)."""
    for shape in shapes:
        for program in shape.programs:
            if len(values) < len(program) or not all(
                _same_word(word, expected) for word, expected in zip(values, program)
            ):
                continue
            rest = values[len(program) :]
            if shape.requires and not any(
                word.startswith(required) for word in rest for required in shape.requires
            ):
                continue
            return True
    return False


def inserts(command: str, shapes: tuple[Shape, ...]) -> list[int] | None:
    """The UTF-8 byte offsets of the program words of the segments a shape matches; None when
    no segment matches or the command cannot be split."""
    raw = command.encode("utf-8")
    segments = split_segments(raw)
    if segments is None:
        return None
    found_at: list[int] = []
    for segment in segments:
        found = words(raw, segment)
        index = program_index(raw, found)
        if index is None:
            continue
        if matches([word.value for word in found[index:]], shapes):
            found_at.append(found[index].start)
    return found_at or None


def offer(command: str, shapes: tuple[Shape, ...]) -> str | None:
    """The command with `PREFIX` inserted at the program word of every segment a shape
    matches; None when no segment matches or the command cannot be split."""
    at = inserts(command, shapes)
    if at is None:
        return None
    offered = command.encode("utf-8")
    for offset in reversed(at):
        offered = offered[:offset] + PREFIX + offered[offset:]
    return offered.decode("utf-8")


# --- remembering refusals ---------------------------------------------------------------------------


def _ledger(state: Path, session: str, agent: str) -> Path:
    key = hashlib.sha256(f"{session}\0{agent}".encode("utf-8")).hexdigest()[:32]
    return state / f"{key}.txt"


def refused_before(state: Path, session: str, agent: str, command: str) -> bool:
    """Whether this command was refused for this session and agent; records it when not.

    Two calls at once may both miss each other's record: that costs one extra refusal."""
    digest = hashlib.sha256(command.encode("utf-8")).hexdigest()
    ledger = _ledger(state, session, agent)
    try:
        if digest in ledger.read_text(encoding="utf-8").split():
            return True
    except FileNotFoundError:
        pass
    state.mkdir(mode=0o700, parents=True, exist_ok=True)
    now = time.time()
    for old in state.glob("*.txt"):
        try:
            if now - old.stat().st_mtime > KEEP_SECONDS:
                old.unlink()
        except OSError:
            pass
    descriptor = os.open(ledger, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        os.write(descriptor, (digest + "\n").encode("ascii"))
    finally:
        os.close(descriptor)
    return False


# --- the hook -------------------------------------------------------------------------------------


def respond(stdin: str, home: Path, has_coreslot: bool) -> str:
    """The hook's stdout for one input: the deny JSON for a refusal, "" for everything else. It
    executes nothing: the `coreslot` on PATH may be a repository's own."""
    try:
        payload = json.loads(stdin)
        if payload.get("tool_name") != "Bash" or not has_coreslot:
            return ""
        command = payload["tool_input"]["command"]
        session, agent = payload.get("session_id") or "", payload.get("agent_id") or ""
        if not all(isinstance(value, str) for value in (command, session, agent)):
            return ""
        user_shapes = home / USER_SHAPES
        own = user_shapes.exists()
        shapes = load_shapes(user_shapes if own else DEFAULT_SHAPES)
        at = inserts(command, shapes)
        if at is None or refused_before(home / STATE, session, agent, command):
            return ""
        offered = offer(command, shapes)
        unvetted = own and bool(set(at) - set(inserts(command, load_shapes(DEFAULT_SHAPES)) or []))
    except Exception:  # noqa: BLE001 -- a nudge never blocks a command on its own failure
        return ""
    how = (
        "Run it with the `coreslot run --restart=yes --` prefix inserted, which starts it when "
        "enough memory is free and, if memory runs short, may kill it and run it again from "
        "scratch by itself:\n"
        f"{offered}\n"
        "`--restart=yes` says the command is safe to kill at any moment and run again. Change it "
        "to `--restart=no` if the tests or their setup or teardown scripts touch a shared "
        "database, external services or shared files.\n"
    )
    if unvetted:
        how += (
            "This command matched a shape of your own shape file, not one of coreslot's "
            "defaults: check that it is safe to run twice before you keep `--restart=yes`.\n"
        )
    how += (
        "If coreslot answers that -restart is an unknown flag, it is older than this plugin: run "
        "the command with `coreslot run -- ` instead, or update coreslot.\n"
    )
    reason = (
        "coreslot: several agents share this machine's memory, and this command is one of the "
        f"heavy ones. {how}"
        "It may wait for free memory first: if your tool has a time limit, run it in the "
        "background or raise the limit. Sent again unchanged, the command runs outside coreslot."
    )
    return json.dumps(
        {
            "hookSpecificOutput": {
                "hookEventName": "PreToolUse",
                "permissionDecision": "deny",
                "permissionDecisionReason": reason,
            }
        }
    )


def main() -> int:
    try:
        home = Path(pwd.getpwuid(os.getuid()).pw_dir)
        out = respond(sys.stdin.read(), home, shutil.which("coreslot") is not None)
        if out:
            sys.stdout.write(out)
    except Exception:  # noqa: BLE001
        pass
    return 0


if __name__ == "__main__":
    sys.exit(main())
