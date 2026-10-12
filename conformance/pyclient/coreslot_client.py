#!/usr/bin/env python3
"""An independent coreslot format-1 client, written from PROTOCOL.md alone.

It exists to prove that PROTOCOL.md is sufficient: it shares one budget with every other
conforming client. Python 3.11+, standard library only, Unix only. Every place PROTOCOL.md
is silent or ambiguous is recorded in AMBIGUITIES.md next to this file; section numbers in
comments below ("§10.2") refer to PROTOCOL.md.

Library::

    c = Client()                                 # or Client(state_dir="/abs/dir")
    t = c.enqueue("ci:job", None, {"cpu": (1, 4)})
    while True:
        kind, value = t.step()                   # one poll
        if kind == "admitted":
            break
        time.sleep(t.poll_delay())
    grant = value                                # grant.token_fds(), grant.environment()
    grant.close()
    c.cancel(ticket_id)

CLI::

    coreslot_client.py run [--need RES=MIN..MAX]... [--label L] [--priority P]
        [--timeout SECONDS] [--strict] [--independent] [--subst] [--refusal-code N] -- CMD...
    coreslot_client.py cancel TICKET-ID
"""

from __future__ import annotations

import contextlib
import errno
import fcntl
import json
import os
import pwd
import re
import resource
import signal
import stat
import sys
import threading
import time
import tomllib

# --------------------------------------------------------------------------------------
# Constants (§22, §6, §7)

FORMAT_VERSION = 1
REFRESH_S = 2.0
STALL_S = 10.0
MAX_PASS_GAP_S = 2.0  # §10.2: a longer interval between an observer's own passes is not watched
POLL_S = 1.0
HEAD_BACKOFF_MIN, HEAD_BACKOFF_MAX = 0.25, 2.0
FRONT_WINDOW = 4  # §10.3: tickets at the front of the queue every election also watches
ENTRY_BACKOFF_MIN, ENTRY_BACKOFF_MAX = 0.01, 0.25
RECONCILE_NS = 30 * 10**9
TMP_AGE_NS = 30 * 10**9
HOLD_REMOVAL_S = 2.0
WAIT_LINE_S = 30.0
LOG_ROTATE_BYTES = 10 * 1024 * 1024
LOG_LINE_MAX = 4096
MAX_ENTRIES = 65536
MAX_BODY = 8192
MAX_PER_RESOURCE = 256
MAX_TOTAL = 512
MAX_SIZE = 4096
MAX_INDEX = 4095
PRIORITY_NS = 60 * 10**9
INT64_MIN, INT64_MAX = -(1 << 63), (1 << 63) - 1
ORDER_KEY_MAX = (1 << 64) - 1  # §7.1: order-key and start-ns fit 64 unsigned bits

EXIT_USAGE = 64
EXIT_REFUSAL = 75
EXIT_INTERNAL = 70
EXIT_INTERRUPTED = 130
EXIT_NOT_EXECUTABLE = 126
EXIT_NOT_FOUND = 127

RES_RE = re.compile(r"[a-z][a-z0-9-]{0,31}")
TICKET_RE = re.compile(r"([0-9]{20})-([0-9]{10})-([0-9]{6})(?:-([A-Za-z0-9._]{1,32}))?")
ID_RE = re.compile(r"([0-9]{20})-([0-9]{10})-([0-9]{6})")
TMP_RE = re.compile(r"\.tmp-[0-9]{10}-[0-9]{6}")
INDEX_RE = re.compile(r"0|[1-9][0-9]{0,3}")
FORMAT_RE = re.compile(rb"coreslot-protocol ([0-9]{1,9})\n?")
ID_LEN = 38  # 20 + 1 + 10 + 1 + 6

O_FILE = os.O_NOFOLLOW | os.O_CLOEXEC
_ENV_PREFIX = "CORESLOT_"
_CONTEXT_VARS = ("CORESLOT_HELD_FD", "CORESLOT_TOKEN_FDS", "CORESLOT_UNBUDGETED")


# --------------------------------------------------------------------------------------
# Errors (§14)


class CoreslotError(Exception):
    exit_code = EXIT_INTERNAL


class UsageError(CoreslotError):
    exit_code = EXIT_USAGE


class StateError(CoreslotError):
    """A state error (§14); `reason` is a short token used for fail-open."""

    def __init__(self, reason: str, detail: str):
        super().__init__(f"{reason}: {detail}")
        self.reason = reason


class Refusal(CoreslotError):
    exit_code = EXIT_REFUSAL


class Cancelled(Refusal):
    pass


class InvalidContext(Refusal):
    pass


class Malformed(ValueError):
    """A body that §7.2 calls malformed."""


class _SectionBusy(Exception):
    pass


# --------------------------------------------------------------------------------------
# Strict body parsing (§1, §7.2)


class _OutOfRange:
    """An integer literal too long to be a signed 64-bit value."""

    __slots__ = ()


_OUT_OF_RANGE = _OutOfRange()
_SURROGATE = re.compile("[\ud800-\udfff]")


def _parse_int(text: str):
    return int(text) if len(text) <= 20 else _OUT_OF_RANGE


def _object_pairs(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise Malformed(f"duplicate key {key!r}")
        out[key] = value
    return out


def _bad_constant(text: str):
    raise Malformed(f"{text} is not JSON")


def _strings(value):
    stack = [value]
    while stack:
        v = stack.pop()
        if isinstance(v, str):
            yield v
        elif isinstance(v, dict):
            for k, item in v.items():
                yield k
                stack.append(item)
        elif isinstance(v, list):
            stack.extend(v)


def parse_body(data: bytes) -> dict:
    """Parse one body under §7.2; raise Malformed when it is not acceptable."""
    if len(data) > MAX_BODY:
        raise Malformed("body exceeds 8 KiB")
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise Malformed("not UTF-8") from exc
    old_limit = sys.getrecursionlimit()
    try:
        if old_limit < 20000:
            sys.setrecursionlimit(20000)  # an 8 KiB body nests at most ~4096 deep
        obj = json.loads(
            text,
            object_pairs_hook=_object_pairs,
            parse_int=_parse_int,
            parse_constant=_bad_constant,
        )
    except Malformed:
        raise
    except (ValueError, RecursionError) as exc:
        raise Malformed(str(exc)) from exc
    finally:
        sys.setrecursionlimit(old_limit)
    if not isinstance(obj, dict):
        raise Malformed("not a JSON object")
    for s in _strings(obj):
        if _SURROGATE.search(s):
            raise Malformed("unpaired UTF-16 surrogate")
    return obj


def _is_int(v, lo=INT64_MIN, hi=INT64_MAX) -> bool:
    return type(v) is int and lo <= v <= hi


def is_resource_name(name) -> bool:
    return isinstance(name, str) and RES_RE.fullmatch(name) is not None


def _require(obj: dict, keys) -> None:
    for k in keys:
        if k not in obj:
            raise Malformed(f"missing key {k!r}")


def _check_label(label) -> None:
    if not isinstance(label, str) or len(label.encode("utf-8")) > 128:
        raise Malformed("label")


def validate_ticket(obj: dict) -> dict:
    _require(obj, ("label", "priority", "need", "created_ns"))
    _check_label(obj["label"])
    if not _is_int(obj["priority"], -1000, 1000):
        raise Malformed("priority")
    need = obj["need"]
    if not isinstance(need, dict) or not 1 <= len(need) <= 8:
        raise Malformed("need")
    total = 0
    for name, pair in need.items():
        if not is_resource_name(name):
            raise Malformed(f"need key {name!r}")
        if not (
            isinstance(pair, list)
            and len(pair) == 2
            and _is_int(pair[0], 1, MAX_PER_RESOURCE)
            and _is_int(pair[1], 1, MAX_PER_RESOURCE)
            and pair[0] <= pair[1]
        ):
            raise Malformed(f"need[{name}]")
        total += pair[0]
    if total > MAX_TOTAL:
        raise Malformed("minimums sum above 512")
    if not _is_int(obj["created_ns"], 0):
        raise Malformed("created_ns")
    return obj


def validate_hold(obj: dict, name_start_ns: int | None = None) -> dict:
    _require(
        obj,
        ("label", "grant", "start_ns", "unbudgeted", "unbudgeted_resources", "clamped", "adopted"),
    )
    _check_label(obj["label"])
    grant = obj["grant"]
    if not isinstance(grant, dict):
        raise Malformed("grant")
    total = 0
    for name, idx in grant.items():
        if not is_resource_name(name):
            raise Malformed(f"grant key {name!r}")
        if not isinstance(idx, list) or not 1 <= len(idx) <= MAX_PER_RESOURCE:
            raise Malformed(f"grant[{name}]")
        prev = -1
        for i in idx:
            if not _is_int(i, 0, MAX_INDEX) or i <= prev:
                raise Malformed(f"grant[{name}] index")
            prev = i
        total += len(idx)
    if total > MAX_TOTAL:
        raise Malformed("grant above 512 tokens")
    if not _is_int(obj["start_ns"], 0):
        raise Malformed("start_ns")
    if name_start_ns is not None and obj["start_ns"] != name_start_ns:
        raise Malformed("start_ns differs from the name")
    if type(obj["unbudgeted"]) is not bool or type(obj["adopted"]) is not bool:
        raise Malformed("boolean")
    ub = obj["unbudgeted_resources"]
    if not isinstance(ub, list) or not all(is_resource_name(r) for r in ub):
        raise Malformed("unbudgeted_resources")
    clamped = obj["clamped"]
    if not isinstance(clamped, dict):
        raise Malformed("clamped")
    for name, v in clamped.items():
        if not is_resource_name(name) or not _is_int(v, 1, MAX_PER_RESOURCE):
            raise Malformed(f"clamped[{name}]")
    return obj


def validate_cancel(obj: dict) -> dict:
    _require(obj, ("requested_ns", "requester_pid"))
    if not _is_int(obj["requested_ns"], 0) or not _is_int(obj["requester_pid"], 0):
        raise Malformed("cancel")
    return obj


def validate_gen(obj: dict) -> dict:
    _require(obj, ("gen", "reconciled_ns"))
    if not _is_int(obj["gen"], 0) or not _is_int(obj["reconciled_ns"]):
        raise Malformed("gen")
    return obj


def validate_degraded(obj: dict) -> dict:
    _require(obj, ("reason", "first_ns", "count"))
    if not isinstance(obj["reason"], str) or not _is_int(obj["first_ns"]) or not _is_int(obj["count"]):
        raise Malformed("degraded")
    return obj


def parse_format(data: bytes) -> int:
    """Return n of `coreslot-protocol <n>` (§7.6); raise Malformed otherwise."""
    m = FORMAT_RE.fullmatch(data)
    if not m:
        raise Malformed("FORMAT")
    return int(m.group(1))


def parse_name(name: str):
    """A ticket or hold name (§7.1): (order_key, pid, seq, display or None), or None."""
    m = TICKET_RE.fullmatch(name)
    if not m or int(m.group(1)) > ORDER_KEY_MAX:
        return None
    return int(m.group(1)), int(m.group(2)), int(m.group(3)), m.group(4)


def parse_cancel_name(name: str):
    """A cancel request's name, a ticket id without display (§7.1): (order_key, pid, seq) or None."""
    m = ID_RE.fullmatch(name)
    if not m or int(m.group(1)) > ORDER_KEY_MAX:
        return None
    return int(m.group(1)), int(m.group(2)), int(m.group(3))


def decode_ticket(data: bytes) -> dict:
    """A ticket body as its canonical value (known keys only); Malformed otherwise."""
    obj = validate_ticket(parse_body(data))
    return {"label": obj["label"], "priority": obj["priority"],
            "need": {r: list(v) for r, v in obj["need"].items()}, "created_ns": obj["created_ns"]}


def decode_hold(data: bytes, name_start_ns: int | None = None) -> dict:
    obj = validate_hold(parse_body(data), name_start_ns)
    return {k: obj[k] for k in ("label", "grant", "start_ns", "unbudgeted", "unbudgeted_resources",
                                "clamped", "adopted")}


def decode_cancel(data: bytes) -> dict:
    obj = validate_cancel(parse_body(data))
    return {"requested_ns": obj["requested_ns"], "requester_pid": obj["requester_pid"]}


def display_of(label: str) -> str:
    """Sanitized display (§7.1, the examples' rule): bytes outside the set become `_`."""
    raw = label.encode("utf-8", "surrogatepass")
    ok = b"0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._"
    return bytes(c if c in ok else 0x5F for c in raw)[:32].decode("ascii")


def order_key(arrival_ns: int, priority: int) -> int:
    """§9: arrival − priority × 60 s, floored at 0."""
    return min(max(0, arrival_ns - priority * PRIORITY_NS), ORDER_KEY_MAX)


def _dump(obj) -> bytes:
    return json.dumps(obj, ensure_ascii=False).encode("utf-8")


# --------------------------------------------------------------------------------------
# Filesystem helpers (§4.2, §4.3, §4.4)


def _check_stat(st: os.stat_result, want_dir: bool, path: str) -> None:
    if stat.S_ISLNK(st.st_mode):
        raise StateError("state-type", f"{path} is a symlink")
    if want_dir and not stat.S_ISDIR(st.st_mode):
        raise StateError("state-type", f"{path} is not a directory")
    if not want_dir and not stat.S_ISREG(st.st_mode):
        raise StateError("state-type", f"{path} is not a regular file")
    if st.st_uid != os.getuid():
        raise StateError("state-owner", f"{path} is not owned by uid {os.getuid()}")
    if st.st_mode & 0o022:
        raise StateError("state-mode", f"{path} is group- or world-writable")


def _policy_ok(st: os.stat_result, want_dir: bool) -> bool:
    """The §4.2 checks as a predicate (for entries that are ignored rather than fatal)."""
    kind_ok = stat.S_ISDIR(st.st_mode) if want_dir else stat.S_ISREG(st.st_mode)
    return kind_ok and st.st_uid == os.getuid() and not st.st_mode & 0o022


def _open_entry(path: str):
    """Open a queue/, holds/ or cancel/ entry only if it passes §4.2 (stray rule B1/D6).

    Returns a descriptor, "gone" (ENOENT), or "ignored": an entry whose type, owner or mode fails
    the checks is never opened or probed. The inode is re-checked after the open, so a swap
    between lstat and open is also ignored.
    """
    try:
        before = os.lstat(path)
    except FileNotFoundError:
        return "gone"
    if not _policy_ok(before, want_dir=False):
        return "ignored"
    try:
        fd = os.open(path, os.O_RDONLY | O_FILE | os.O_NONBLOCK)
    except FileNotFoundError:
        return "gone"
    except OSError:
        return "ignored"
    after = os.fstat(fd)
    if (after.st_dev, after.st_ino) != (before.st_dev, before.st_ino) or not _policy_ok(after, False):
        os.close(fd)
        return "ignored"
    return fd


def _ensure_dir(path: str, create: bool = True) -> bool:
    """Create (0700, EEXIST tolerated) and check a layout directory; False if absent."""
    if create:
        try:
            os.mkdir(path, 0o700)
        except FileExistsError:
            pass
    try:
        st = os.lstat(path)
    except FileNotFoundError:
        if create:
            raise
        return False
    _check_stat(st, True, path)
    return True


def _open_checked(path: str, flags: int, mode: int = 0o600) -> int:
    """Open a layout file with O_NOFOLLOW|O_CLOEXEC and check it (§4.2)."""
    try:
        fd = os.open(path, flags | O_FILE | os.O_NONBLOCK, mode)  # never waits on a FIFO
    except OSError as exc:
        if exc.errno in (errno.ELOOP, errno.EMLINK):
            raise StateError("state-type", f"{path} is a symlink") from exc
        if exc.errno == errno.EISDIR:
            raise StateError("state-type", f"{path} is not a regular file") from exc
        raise
    try:
        _check_stat(os.fstat(fd), False, path)
    except BaseException:
        os.close(fd)
        raise
    return fd


def _read_upto(fd: int, limit: int) -> bytes:
    chunks, total = [], 0
    while total < limit:
        b = os.read(fd, limit - total)
        if not b:
            break
        chunks.append(b)
        total += len(b)
    return b"".join(chunks)


def _write_all(fd: int, data: bytes) -> None:
    view = memoryview(data)
    while view:
        n = os.write(fd, view)
        view = view[n:]


def _list(path: str) -> list[str]:
    """List a directory, reading at most 65 537 entries (§4.4)."""
    names: list[str] = []
    try:
        with os.scandir(path) as it:
            for entry in it:
                names.append(entry.name)
                if len(names) > MAX_ENTRIES:
                    raise StateError("directory-full", f"{path} holds more than {MAX_ENTRIES} entries")
    except FileNotFoundError:
        return []
    return names


def _probe_path(path: str) -> bool | None:
    """Probe (§1): True if the lock was free, False if held, None if the file is gone."""
    try:
        fd = os.open(path, os.O_RDONLY | O_FILE | os.O_NONBLOCK)
    except FileNotFoundError:
        return None
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return False
        return True
    finally:
        os.close(fd)


class _Counter:
    """Per-process seq counter (§7.1): 000001 upward, wrapping from 999999 to 000000."""

    def __init__(self):
        self._lock = threading.Lock()
        self._pid = None
        self._v = 0

    def next(self) -> int:
        with self._lock:
            pid = os.getpid()
            if pid != self._pid:
                self._pid, self._v = pid, 0
            self._v = (self._v + 1) % 1000000
            return self._v


# Tickets and holds take their name's seq from this counter (their temporary uses the same
# value, as in §21.1/§21.3). Temporaries of gen, FORMAT, degraded and cancel requests use the
# second counter, so they do not advance the names' seq (as §21.3/§21.6 imply).
_NAME_SEQ = _Counter()
_AUX_SEQ = _Counter()


# --- native helpers (ctypes is in the standard library; it is optional at import) --------

try:
    import ctypes
    import ctypes.util

    _LIBC = ctypes.CDLL(None, use_errno=True)
except Exception:  # pragma: no cover - minimal Pythons without ctypes
    ctypes = None
    _LIBC = None


def _native_noreplace():
    if _LIBC is None:
        return None
    if sys.platform == "darwin":
        try:
            f = _LIBC.renamex_np
        except AttributeError:
            return None
        f.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_uint]
        f.restype = ctypes.c_int
        return lambda a, b: f(a, b, 0x00000004)  # RENAME_EXCL
    if sys.platform.startswith("linux"):
        try:
            f = _LIBC.renameat2
        except AttributeError:
            return None
        f.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
        f.restype = ctypes.c_int
        return lambda a, b: f(-100, a, -100, b, 1)  # AT_FDCWD, RENAME_NOREPLACE
    return None


_NOREPLACE = _native_noreplace()
_NOREPLACE_FALLBACK_ERRNOS = {errno.EINVAL, errno.ENOSYS, errno.ENOTSUP, errno.EOPNOTSUPP}


def rename_noreplace(src: str, dst: str) -> None:
    """§11 step 4 for tickets, holds, cancel requests; FileExistsError when dst exists."""
    if _NOREPLACE is not None:
        if _NOREPLACE(os.fsencode(src), os.fsencode(dst)) == 0:
            return
        e = ctypes.get_errno()
        if e == errno.EEXIST:
            raise FileExistsError(e, os.strerror(e), dst)
        if e not in _NOREPLACE_FALLBACK_ERRNOS:
            raise OSError(e, os.strerror(e), dst)
    os.link(src, dst)
    with contextlib.suppress(OSError):
        os.unlink(src)  # if this fails, the extra name is a temporary that reconciliation removes


_DARWIN_REFUSED = {"nfs", "smbfs", "afpfs", "webdav"}
_LINUX_REFUSED = {0x6969, 0xFF534D42, 0xFE534D42, 0x517B, 0x65735546, 0x01021997,
                  0x786F4256, 0x00C36400, 0x5346414F, 0x6B414653}  # + vboxsf, Ceph, AFS, kAFS
_LINUX_REFUSED_NAMES = {"nfs", "nfs4", "cifs", "smb3", "smbfs", "9p", "vboxsf", "ceph", "afs"}


def filesystem_type(path: str):
    """Return the filesystem type of path: a name on macOS, a magic number on Linux."""
    if sys.platform == "darwin":
        if _LIBC is None:
            raise StateError("filesystem", "cannot call statfs without ctypes")
        try:
            f = _LIBC["statfs$INODE64"]
        except AttributeError:
            f = _LIBC.statfs
        buf = ctypes.create_string_buffer(4096)
        if f(os.fsencode(path), buf) != 0:
            e = ctypes.get_errno()
            raise StateError("filesystem", f"statfs {path}: {os.strerror(e)}")
        # struct statfs (64-bit inodes): f_fstypename[16] at offset 72.
        return buf.raw[72:88].split(b"\0", 1)[0].decode("ascii", "replace")
    if sys.platform.startswith("linux"):
        if _LIBC is not None:
            buf = ctypes.create_string_buffer(512)
            if _LIBC.statfs(os.fsencode(path), buf) == 0:
                return ctypes.c_long.from_buffer(buf).value & 0xFFFFFFFF
        return _linux_mountinfo_type(path)
    return None


def _linux_mountinfo_type(path: str) -> str:
    real = os.path.realpath(path)
    best, best_type = "", None
    try:
        with open("/proc/self/mountinfo", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                left, _, right = line.partition(" - ")
                fields = left.split()
                mnt = fields[4].replace("\\040", " ")
                if (real == mnt or real.startswith(mnt.rstrip("/") + "/")) and len(mnt) >= len(best):
                    best, best_type = mnt, right.split()[0]
    except OSError as exc:
        raise StateError("filesystem", f"cannot determine the filesystem of {path}") from exc
    if best_type is None:
        raise StateError("filesystem", f"cannot determine the filesystem of {path}")
    return best_type


def check_filesystem(path: str) -> None:
    """§4.3: refuse network, FUSE and VM-shared filesystems."""
    t = filesystem_type(path)
    if sys.platform == "darwin":
        if t in _DARWIN_REFUSED or t.startswith(("macfuse", "osxfuse")):
            raise StateError("filesystem", f"{path} is on a refused filesystem ({t})")
    elif sys.platform.startswith("linux"):
        if isinstance(t, int):
            if t in _LINUX_REFUSED:
                raise StateError("filesystem", f"{path} is on a refused filesystem (0x{t:x})")
        elif t in _LINUX_REFUSED_NAMES or t.startswith("fuse"):
            raise StateError("filesystem", f"{path} is on a refused filesystem ({t})")


def machine_cpus() -> int:
    """§5.2: the machine-wide online CPU count."""
    if sys.platform == "darwin":
        if _LIBC is not None:
            val = ctypes.c_int(0)
            size = ctypes.c_size_t(ctypes.sizeof(val))
            if _LIBC.sysctlbyname(b"hw.logicalcpu", ctypes.byref(val), ctypes.byref(size), None, 0) == 0:
                if val.value >= 1:
                    return val.value
        raise StateError("cpus", "cannot read hw.logicalcpu")
    if sys.platform.startswith("linux"):
        try:
            with open("/sys/devices/system/cpu/online", encoding="ascii") as fh:
                return parse_cpu_list(fh.read())
        except (OSError, ValueError) as exc:
            raise StateError("cpus", f"cannot read the online CPU list: {exc}") from exc
    n = os.sysconf("SC_NPROCESSORS_ONLN") if hasattr(os, "sysconf") else -1
    if n < 1:
        raise StateError("cpus", "cannot obtain the online processor count")
    return n


def parse_cpu_list(text: str) -> int:
    """Count CPUs in a list such as `0-3,8-11` (§5.2); a final LF is tolerated."""
    if text.endswith("\n"):
        text = text[:-1]
    count = 0
    for item in text.split(","):
        m = re.fullmatch(r"(0|[1-9][0-9]*)(?:-(0|[1-9][0-9]*))?", item)
        if not m:
            raise ValueError(f"bad CPU list item {item!r}")
        a = int(m.group(1))
        b = int(m.group(2)) if m.group(2) is not None else a
        if a > b:
            raise ValueError(f"bad CPU range {item!r}")
        count += b - a + 1
    return count


def raise_nofile(want: int) -> None:
    soft, hard = resource.getrlimit(resource.RLIMIT_NOFILE)
    if soft != resource.RLIM_INFINITY and soft < want:
        new = want if hard == resource.RLIM_INFINITY else min(want, hard)
        if new > soft:
            with contextlib.suppress(ValueError, OSError):
                resource.setrlimit(resource.RLIMIT_NOFILE, (new, hard))


# --------------------------------------------------------------------------------------
# Configuration (§5)


def size_of(raw, cpus: int) -> int:
    """§5.2; raise ValueError for an invalid configured size."""
    if type(raw) is int:
        if 1 <= raw <= MAX_SIZE:
            return raw
        raise ValueError(f"size {raw} outside [1, 4096]")
    if isinstance(raw, str):
        m = re.fullmatch(r"([0-9]{1,9})%", raw)
        if m and 1 <= int(m.group(1)) <= 100:
            return min(MAX_SIZE, max(1, cpus * int(m.group(1)) // 100))
        m = re.fullmatch(r"-([0-9]{1,9})", raw)
        if m:
            return min(MAX_SIZE, max(1, cpus - int(m.group(1))))
    raise ValueError(f"invalid size {raw!r}")


class Config:
    def __init__(self, resources: dict | None = None, priorities: dict | None = None,
                 windows_warning: bool = True):
        self.resources = {"cpu": "100%"}
        self.resources.update(resources or {})
        self.priorities = dict(priorities or {})
        self.windows_warning = windows_warning

    def declared(self, name: str) -> bool:
        return name in self.resources

    def size(self, name: str, cpus: int) -> int:
        return size_of(self.resources[name], cpus)

    def priority_for(self, label: str) -> int:
        best, value = -1, 0
        for prefix, prio in self.priorities.items():
            if label.startswith(prefix) and len(prefix) > best:
                best, value = len(prefix), prio
        return value

    @classmethod
    def parse(cls, data: bytes) -> "Config":
        try:
            doc = tomllib.loads(data.decode("utf-8"))
        except (UnicodeDecodeError, tomllib.TOMLDecodeError) as exc:
            raise ValueError(f"not valid TOML: {exc}") from exc
        resources = doc.get("resources", {})
        priorities = doc.get("priorities", {})
        platform = doc.get("platform", {})
        if not isinstance(resources, dict) or not isinstance(priorities, dict) or not isinstance(platform, dict):
            raise ValueError("[resources], [priorities] and [platform] must be tables")
        for name, raw in resources.items():
            if not is_resource_name(name):
                raise ValueError(f"resource name {name!r}")
            size_of(raw, 1)  # shape check; the size itself is recomputed at each attempt
        for prefix, prio in priorities.items():
            if not _is_int(prio, -1000, 1000):
                raise ValueError(f"priority for {prefix!r}")
        ww = platform.get("windows_warning", True)
        if type(ww) is not bool:
            raise ValueError("windows_warning must be a boolean")
        return cls(resources, priorities, ww)


def password_home() -> str:
    return pwd.getpwuid(os.getuid()).pw_dir


def default_state_dir(home: str | None = None) -> str:
    home = home if home is not None else password_home()
    if sys.platform == "darwin":
        return os.path.join(home, "Library", "Application Support", "coreslot")
    return os.path.join(home, ".local", "state", "coreslot")


# --------------------------------------------------------------------------------------
# Need


def normalize_need(need) -> dict[str, tuple[int, int]]:
    """Validate a request's need (usage errors, §14)."""
    if not isinstance(need, dict) or not need:
        raise UsageError("need must name at least one resource")
    if len(need) > 8:
        raise UsageError("a request names at most 8 resources")
    out: dict[str, tuple[int, int]] = {}
    for name, value in need.items():
        if not is_resource_name(name):
            raise UsageError(f"invalid resource name {name!r}")
        if type(value) is int:
            lo = hi = value
        else:
            try:
                lo, hi = value
            except (TypeError, ValueError) as exc:
                raise UsageError(f"need for {name} must be (min, max)") from exc
        if type(lo) is not int or type(hi) is not int:
            raise UsageError(f"need for {name} must be integers")
        if lo < 1:
            raise UsageError(f"{name}: min must be at least 1")
        if lo > hi:
            raise UsageError(f"{name}: min {lo} > max {hi}")
        if hi > MAX_PER_RESOURCE:
            raise UsageError(f"{name}: max {hi} > 256")
        out[name] = (lo, hi)
    if sum(lo for lo, _ in out.values()) > MAX_TOTAL:
        raise UsageError("minimums sum above 512")
    return out


def parse_need_arg(text: str) -> tuple[str, tuple[int, int]]:
    m = re.fullmatch(r"([^=]+)=(0|[1-9][0-9]*)(?:\.\.(0|[1-9][0-9]*))?", text)
    if not m:
        raise UsageError(f"malformed need {text!r} (want RES=MIN..MAX or RES=N)")
    lo = int(m.group(2))
    hi = int(m.group(3)) if m.group(3) is not None else lo
    return m.group(1), (lo, hi)


# --------------------------------------------------------------------------------------
# The admission critical section (§8)


class _Section:
    def __init__(self, client: "Client", S: str, fd: int, fmt: int | None, understood: bool):
        self.client = client
        self.S = S
        self.fd = fd
        self.format = fmt
        self.understood = understood

    def read_gen(self) -> tuple[int, int]:
        return self.client._read_gen()

    def write_gen(self, gen: int, reconciled_ns: int) -> None:
        if gen > INT64_MAX:
            gen = 0
        self.client._publish_replace(self.S, "gen", _dump({"gen": gen, "reconciled_ns": reconciled_ns}))

    def bump_gen(self) -> None:
        g, r = self.read_gen()
        self.write_gen(g + 1, r)

    def reconcile_if_due(self) -> bool:
        """§10.4: called before its own change by every entry but admission/token-only."""
        g, r = self.read_gen()
        now = time.time_ns()
        if now - r > RECONCILE_NS or r - now > RECONCILE_NS:
            self.client._reconcile(self, g)
            return True
        return False


# --------------------------------------------------------------------------------------
# Client


class Client:
    """A format-1 client bound to one state directory.

    `state_dir` is the library form of the CORESLOT_STATE_DIR override (§4.1): when given (or
    when the variable is set and non-empty) the configuration moves to `<state_dir>/config.toml`
    (§5.3). `observer_clock` is the monotonic clock this client observes ticket freshness on
    (§10.2); tests inject a fake one. `cli` makes the client ignore job-control stops inside the
    section (§8), which a library never does.
    """

    def __init__(self, state_dir: str | None = None, *, observer_clock=None, cli: bool = False,
                 cpus: int | None = None, diag=None):
        if state_dir is None:
            env = os.environ.get("CORESLOT_STATE_DIR", "")
            self._override = env != ""
            self._raw_dir = env if self._override else None
        else:
            self._override = True
            self._raw_dir = state_dir
        self.observer_clock = observer_clock or time.monotonic
        self.cli = cli
        self._cpus = cpus
        self._S: str | None = None
        self._diag = diag or (lambda msg: print(msg, file=sys.stderr, flush=True))
        self.ignored_entries = 0  # queue/ entries the last election ignored (§4.4: counted)
        self.ignored_by_reconciliation: dict[str, int] = {}

    # ---- locations (§4.1, §5.3)

    def state_dir(self) -> str:
        if self._S is None:
            if self._override:
                if not os.path.isabs(self._raw_dir):
                    raise StateError("state-dir", f"CORESLOT_STATE_DIR {self._raw_dir!r} is not absolute")
                self._S = self._raw_dir
            else:
                self._S = default_state_dir()
        return self._S

    def config_path(self) -> str:
        if self._override:
            return os.path.join(self.state_dir(), "config.toml")
        return os.path.join(password_home(), ".config", "coreslot", "config.toml")

    def config(self) -> Config:
        path = self.config_path()
        try:
            if self._override:
                fd = _open_checked(path, os.O_RDONLY)
            else:
                # Through symlinks; a dangling one raises FileNotFoundError (the defaults).
                fd = os.open(path, os.O_RDONLY | os.O_CLOEXEC | os.O_NONBLOCK)
                if not stat.S_ISREG(os.fstat(fd).st_mode):
                    os.close(fd)
                    raise StateError("config", f"{path} is not a regular file")
        except FileNotFoundError:
            return Config()
        except OSError as exc:
            raise StateError("config", f"cannot read {path}: {exc}") from exc
        try:
            data = _read_upto(fd, 1 << 20)
        finally:
            os.close(fd)
        try:
            return Config.parse(data)
        except ValueError as exc:
            raise StateError("config", f"invalid configuration {path}: {exc}") from exc

    def cpus(self) -> int:
        return self._cpus if self._cpus is not None else machine_cpus()

    def _prepare(self) -> str:
        S = self.state_dir()
        try:
            parent = os.path.dirname(S.rstrip("/")) or "/"
            os.makedirs(parent, 0o700, exist_ok=True)
            _ensure_dir(S)
        except StateError:
            raise
        except OSError as exc:
            raise StateError("state-dir", f"cannot use {S}: {exc}") from exc
        check_filesystem(S)
        return S

    def _sub(self, name: str, create: bool = True) -> str:
        path = os.path.join(self.state_dir(), name)
        _ensure_dir(path, create)
        return path

    # ---- section entry (§8)

    @contextlib.contextmanager
    def _section(self, timeout: float | None = None, single: bool = False):
        S = self._prepare()
        saved = self._ignore_stops()
        fd = None
        try:
            fd = _open_checked(os.path.join(S, "admit.lock"), os.O_RDWR | os.O_CREAT)
            deadline = None if timeout is None else time.monotonic() + timeout
            delay = ENTRY_BACKOFF_MIN
            while True:
                try:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    break
                except BlockingIOError:
                    pass
                if single:
                    raise _SectionBusy()
                now = time.monotonic()
                if deadline is not None and now >= deadline:
                    raise _SectionBusy()
                time.sleep(delay if deadline is None else max(0.0, min(delay, deadline - now)))
                delay = min(delay * 2, ENTRY_BACKOFF_MAX)
            fmt = self._read_format()
            understood = fmt in (None, FORMAT_VERSION)
            if understood:
                if fmt is None:
                    self._publish_replace(S, "FORMAT", b"coreslot-protocol 1\n")
                    fmt = FORMAT_VERSION
                with contextlib.suppress(OSError):  # SHOULD; informational (§7.7)
                    os.ftruncate(fd, 0)
                    os.pwrite(fd, _dump({"pid": os.getpid(), "entered_ns": time.time_ns()}), 0)
            yield _Section(self, S, fd, fmt, understood)
        finally:
            if fd is not None:
                os.close(fd)  # leaving the section is closing the descriptor
            self._restore_stops(saved)

    def _ignore_stops(self):
        if not self.cli or threading.current_thread() is not threading.main_thread():
            return None
        sigs = (signal.SIGTSTP, signal.SIGTTIN, signal.SIGTTOU)
        return [(s, signal.signal(s, signal.SIG_IGN)) for s in sigs]

    @staticmethod
    def _restore_stops(saved) -> None:
        for s, h in saved or ():
            signal.signal(s, h)

    def _read_format(self) -> int | None:
        path = os.path.join(self.state_dir(), "FORMAT")
        try:
            fd = _open_checked(path, os.O_RDONLY)
        except FileNotFoundError:
            return None
        try:
            data = _read_upto(fd, 4096)
        finally:
            os.close(fd)
        try:
            return parse_format(data)
        except Malformed as exc:
            raise StateError("format-malformed", f"malformed FORMAT {data[:64]!r}") from exc

    def _read_gen(self) -> tuple[int, int]:
        path = os.path.join(self.state_dir(), "gen")
        try:
            fd = _open_checked(path, os.O_RDONLY)
        except FileNotFoundError:
            return (0, 0)
        try:
            data = _read_upto(fd, MAX_BODY + 1)
        finally:
            os.close(fd)
        try:
            obj = validate_gen(parse_body(data))
        except Malformed:
            return (0, 0)
        return (obj["gen"], obj["reconciled_ns"])

    # ---- atomic publication (§11)

    @staticmethod
    def _create_tmp(d: str, counter: _Counter, seq: int | None = None) -> tuple[int, str, int]:
        pid = os.getpid()
        for _ in range(1000001):
            if seq is None:
                seq = counter.next()
            tmp = os.path.join(d, f".tmp-{pid:010d}-{seq:06d}")
            try:
                fd = os.open(tmp, os.O_CREAT | os.O_EXCL | os.O_RDWR | O_FILE, 0o600)
                return fd, tmp, seq
            except FileExistsError:
                seq = None
        raise StateError("directory-full", f"no free temporary name in {d}")

    def _publish_replace(self, d: str, final: str, body: bytes) -> None:
        """gen, FORMAT, degraded: temporary, body, plain rename."""
        fd, tmp, _ = self._create_tmp(d, _AUX_SEQ)
        try:
            try:
                _write_all(fd, body)
            finally:
                os.close(fd)
            os.rename(tmp, os.path.join(d, final))
        except BaseException:
            with contextlib.suppress(OSError):
                os.unlink(tmp)
            raise

    def _check_room(self, d: str) -> None:
        if len(_list(d)) >= MAX_ENTRIES:
            raise StateError("directory-full", f"{d} already holds {MAX_ENTRIES} entries")

    def _publish_named(self, d: str, make_name, body: bytes, lock: bool) -> tuple[int | None, str]:
        """Ticket or hold: lock-carrying, final name built from the seq (no replacement)."""
        self._check_room(d)
        while True:
            fd, tmp, seq = self._create_tmp(d, _NAME_SEQ)
            try:
                if lock:
                    fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                _write_all(fd, body)
                final = make_name(seq)
                rename_noreplace(tmp, os.path.join(d, final))
                break
            except BaseException as exc:
                os.close(fd)
                with contextlib.suppress(OSError):
                    os.unlink(tmp)
                if isinstance(exc, FileExistsError):
                    continue  # the final name is taken: a new temporary and the next seq
                raise
        if not lock:
            os.close(fd)
            return None, final
        return fd, final

    def _publish_cancel(self, d: str, ticket_id: str, body: bytes) -> bool:
        """Cancel request: fixed final name; False when one already exists."""
        self._check_room(d)
        fd, tmp, _ = self._create_tmp(d, _AUX_SEQ)
        try:
            try:
                _write_all(fd, body)
            finally:
                os.close(fd)
            rename_noreplace(tmp, os.path.join(d, ticket_id))
            return True
        except FileExistsError:
            os.unlink(tmp)
            return False
        except BaseException:
            with contextlib.suppress(OSError):
                os.unlink(tmp)
            raise

    # ---- reconciliation (§10.4)

    def _reconcile(self, sec: _Section, gen: int) -> None:
        S = sec.S
        removed = False
        queue, holds, cancel = (os.path.join(S, n) for n in ("queue", "holds", "cancel"))
        ignored = {"queue": 0, "holds": 0, "cancel": 0}
        live_ids: set[str] = set()  # ids of the queue/ tickets that survive step 1; a stray is none
        for d in (queue, holds):
            if not _ensure_dir(d, create=False):
                continue
            key = os.path.basename(d)
            for name in _list(d):
                if parse_name(name) is None:
                    if not TMP_RE.fullmatch(name):
                        ignored[key] += 1
                    continue
                path = os.path.join(d, name)
                fd = _open_entry(path)
                if fd == "gone":
                    continue
                if fd == "ignored":
                    ignored[key] += 1  # bad type/owner/mode: never probed or removed (§4.2, §10.4)
                    continue
                try:
                    try:
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    except BlockingIOError:
                        if d == queue:
                            live_ids.add(name[:ID_LEN])  # live: its cancel request stays
                        continue
                    try:
                        os.unlink(path)  # while still holding the probe lock
                        removed = True
                    except FileNotFoundError:
                        pass
                finally:
                    os.close(fd)
        if _ensure_dir(cancel, create=False):
            for name in _list(cancel):
                if parse_cancel_name(name) is None:
                    if not TMP_RE.fullmatch(name):
                        ignored["cancel"] += 1
                    continue
                if name in live_ids:
                    continue
                path = os.path.join(cancel, name)
                try:
                    if not _policy_ok(os.lstat(path), want_dir=False):
                        ignored["cancel"] += 1
                        continue
                    os.unlink(path)
                    removed = True
                except FileNotFoundError:
                    pass
        now = time.time_ns()
        for key, d in (("$S", S), ("queue", queue), ("holds", holds), ("cancel", cancel)):  # step 3
            for name in _list(d):
                if not TMP_RE.fullmatch(name):
                    continue
                path = os.path.join(d, name)
                try:
                    st = os.lstat(path)
                    if not _policy_ok(st, want_dir=False):
                        if key == "$S":  # §4.2: a temporary in $S itself is checked like the layout
                            _check_stat(st, False, path)
                        ignored[key] += 1
                        continue
                    if now - st.st_mtime_ns > TMP_AGE_NS:
                        os.unlink(path)
                        removed = True
                except OSError:  # StateError is not an OSError: a bad $S temporary propagates
                    pass
        self.ignored_by_reconciliation = ignored
        sec.write_gen(gen + 1 if removed else gen, time.time_ns())

    # ---- tokens (§6)

    def _count_locked(self, S: str, res: str) -> int:
        d = os.path.join(S, "res", res)
        if not _ensure_dir(os.path.join(S, "res"), create=False) or not _ensure_dir(d, create=False):
            return 0
        locked = 0
        for name in _list(d):
            if not INDEX_RE.fullmatch(name) or int(name) > MAX_INDEX:
                continue
            path = os.path.join(d, name)
            try:
                fd = _open_checked(path, os.O_RDONLY)
            except FileNotFoundError:
                continue
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                locked += 1
            finally:
                os.close(fd)
        return locked

    def _take(self, S: str, res: str, index: int) -> int | None:
        _ensure_dir(os.path.join(S, "res"))
        d = os.path.join(S, "res", res)
        _ensure_dir(d)
        path = os.path.join(d, str(index))
        fd = _open_checked(path, os.O_RDONLY | os.O_CREAT)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            os.close(fd)
            return None
        held = os.fstat(fd)
        try:
            now = os.lstat(path)
        except FileNotFoundError:
            now = None
        if held.st_nlink == 0 or now is None or (now.st_dev, now.st_ino) != (held.st_dev, held.st_ino):
            os.close(fd)
            raise StateError("inode", f"{path} changed under its holder (I1)")
        return fd

    def _admit_locked(self, sec: _Section, label: str, need: dict, *, token_only: bool,
                      ticket: "Ticket | None" = None) -> "Grant | None":
        """§12.1 steps 2-7 (or the §15.2 token-only form); None means leave and wait."""
        S = sec.S
        cfg = self.config()
        cpus = self.cpus()
        sizes: dict[str, int] = {}
        for r in need:
            if cfg.declared(r):
                try:
                    sizes[r] = cfg.size(r, cpus)
                except ValueError as exc:
                    raise StateError("config", str(exc)) from exc
        undeclared = sorted(r for r in need if r not in sizes)
        eff: dict[str, tuple[int, int]] = {}
        clamped: dict[str, int] = {}
        for r in sorted(sizes):
            lo, hi = need[r]
            s = sizes[r]
            if lo > s:
                eff[r] = (s, s)
                clamped[r] = lo
            else:
                eff[r] = (lo, min(hi, s))
        free = {r: sizes[r] - self._count_locked(S, r) for r in eff}
        if any(free[r] < eff[r][0] for r in eff):
            return None
        if ticket is not None and not ticket._reelect():  # §12.1 step 5
            return None
        raise_nofile(sum(e[1] for e in eff.values()) + 64)
        taken: dict[str, list[tuple[int, int]]] = {r: [] for r in eff}

        def release_all():
            for lst in taken.values():
                for _, fd in lst:
                    os.close(fd)
                lst.clear()

        try:
            for r in sorted(eff):  # minimum pass
                want = eff[r][0]
                for i in range(sizes[r]):
                    if len(taken[r]) >= want:
                        break
                    fd = self._take(S, r, i)
                    if fd is not None:
                        taken[r].append((i, fd))
                if len(taken[r]) < want:
                    release_all()
                    return None
            if not token_only:  # extra pass
                total = sum(len(v) for v in taken.values())
                for r in sorted(eff):
                    limit = min(eff[r][1], free[r])
                    have = {i for i, _ in taken[r]}
                    for i in range(sizes[r]):
                        if len(taken[r]) >= limit or total >= MAX_TOTAL:
                            break
                        if i in have:
                            continue
                        fd = self._take(S, r, i)
                        if fd is not None:
                            taken[r].append((i, fd))
                            total += 1
                    taken[r].sort()
            tokens = {r: v for r, v in taken.items() if v}
            if token_only:
                return Grant(self, label, need, tokens, None, None, clamped, undeclared,
                             reason="format", token_only=True)
            start_ns = time.time_ns()
            body = {
                "label": label,
                "grant": {r: [i for i, _ in v] for r, v in sorted(tokens.items())},
                "start_ns": start_ns,
                "unbudgeted": False,
                "unbudgeted_resources": undeclared,
                "clamped": clamped,
                "adopted": False,
            }
            holds = self._sub("holds")
            disp = display_of(label)
            suffix = f"-{disp}" if disp else ""
            pid = os.getpid()
            hold_fd, hold_name = self._publish_named(
                holds, lambda seq: f"{start_ns:020d}-{pid:010d}-{seq:06d}{suffix}", _dump(body), lock=True
            )
        except BaseException:
            release_all()
            raise
        if ticket is not None:
            ticket._close_fd()
            with contextlib.suppress(FileNotFoundError):
                os.unlink(os.path.join(S, "queue", ticket.name))
        sec.bump_gen()
        return Grant(self, label, need, tokens, hold_fd, hold_name, clamped, undeclared)

    # ---- public operations

    def enqueue(self, label: str, priority: int | None, need, *, timeout: float | None = None) -> "Ticket":
        """§10.1. Returns a Ticket; in an unknown format, a token-only Ticket (§15.2)."""
        need = normalize_need(need)
        if not isinstance(label, str):
            raise UsageError("label must be a string")
        try:
            if len(label.encode("utf-8")) > 128:
                raise UsageError("label exceeds 128 bytes")
        except UnicodeEncodeError as exc:
            raise UsageError("label is not valid UTF-8") from exc
        if priority is None:
            priority = self.config().priority_for(label)
        if not _is_int(priority, -1000, 1000):
            raise UsageError("priority must be an integer in [-1000, 1000]")
        try:
            with self._section(timeout=timeout) as sec:
                if not sec.understood:
                    self._diag(f"coreslot: state directory has format {sec.format}, which this client "
                               "does not implement; running token-only")
                    return Ticket(self, None, None, label, priority, need, token_only=True)
                sec.reconcile_if_due()
                fd, name = self._publish_ticket(sec, label, priority, need)
                return Ticket(self, name, fd, label, priority, need)
        except _SectionBusy as exc:
            raise Refusal("could not enter the admission section before the deadline") from exc
        except OSError as exc:
            raise StateError("io", str(exc)) from exc

    def _publish_ticket(self, sec: _Section, label: str, priority: int, need: dict) -> tuple[int, str]:
        """§10.1 inside an understood section: publish the ticket and bump gen."""
        queue = self._sub("queue")
        arrival = time.time_ns()
        key = order_key(arrival, priority)
        body = _dump({"label": label, "priority": priority,
                      "need": {r: [lo, hi] for r, (lo, hi) in need.items()},
                      "created_ns": arrival})
        disp = display_of(label)
        suffix = f"-{disp}" if disp else ""
        pid = os.getpid()
        fd, name = self._publish_named(
            queue, lambda seq: f"{key:020d}-{pid:010d}-{seq:06d}{suffix}", body, lock=True)
        try:
            sec.bump_gen()
        except BaseException:
            os.close(fd)
            raise
        return fd, name

    def cancel(self, ticket_id: str, *, timeout: float | None = 30.0) -> bool:
        """§13.1: publish cancel/<ticket-id>. False when a request already existed."""
        if parse_name(ticket_id) is None:
            raise UsageError(f"not a ticket id: {ticket_id!r}")
        tid = ticket_id[:ID_LEN]
        try:
            with self._section(timeout=timeout) as sec:
                if not sec.understood:
                    raise StateError("format-unknown", f"format {sec.format} is not understood; cannot cancel")
                sec.reconcile_if_due()
                d = self._sub("cancel")
                body = _dump({"requested_ns": time.time_ns(), "requester_pid": os.getpid()})
                return self._publish_cancel(d, tid, body)
        except _SectionBusy as exc:
            raise Refusal("could not enter the admission section") from exc
        except OSError as exc:
            raise StateError("io", str(exc)) from exc

    def remove_hold(self, name: str, timeout: float = HOLD_REMOVAL_S) -> bool:
        """§12.3: probe the hold and unlink it only if the probe succeeds."""
        try:
            with self._section(timeout=timeout) as sec:
                if not sec.understood:
                    return False
                sec.reconcile_if_due()
                path = os.path.join(sec.S, "holds", name)
                try:
                    fd = os.open(path, os.O_RDONLY | O_FILE | os.O_NONBLOCK)
                except FileNotFoundError:
                    return False
                try:
                    try:
                        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                    except BlockingIOError:
                        return False  # a descendant still holds it; reconciliation removes it later
                    os.unlink(path)
                finally:
                    os.close(fd)
                sec.bump_gen()
                return True
        except (_SectionBusy, OSError, StateError):
            return False

    def publish_unbudgeted_hold(self, label: str, need: dict, timeout: float = 2.0):
        """§14 fail-open: a hold with "unbudgeted": true and an empty grant. (fd, name) or None."""
        try:
            with self._section(timeout=timeout) as sec:
                if not sec.understood:
                    return None
                sec.reconcile_if_due()
                start_ns = time.time_ns()
                body = _dump({"label": label, "grant": {}, "start_ns": start_ns, "unbudgeted": True,
                              "unbudgeted_resources": sorted(need), "clamped": {}, "adopted": False})
                disp = display_of(label)
                suffix = f"-{disp}" if disp else ""
                pid = os.getpid()
                published = self._publish_named(
                    self._sub("holds"), lambda seq: f"{start_ns:020d}-{pid:010d}-{seq:06d}{suffix}",
                    body, lock=True)
                try:
                    sec.bump_gen()
                except BaseException:
                    os.close(published[0])
                    raise
                return published
        except (_SectionBusy, OSError, StateError):
            return None

    def record_degraded(self, reason: str, timeout: float = 2.0) -> bool:
        """§7.9: write or update the degraded record."""
        try:
            with self._section(timeout=timeout) as sec:
                if not sec.understood:
                    return False
                sec.reconcile_if_due()
                now = time.time_ns()
                first, count = now, 1
                path = os.path.join(sec.S, "degraded")
                try:
                    fd = _open_checked(path, os.O_RDONLY)
                    try:
                        old = validate_degraded(parse_body(_read_upto(fd, MAX_BODY + 1)))
                        first, count = old["first_ns"], min(old["count"] + 1, INT64_MAX)
                    except Malformed:
                        pass
                    finally:
                        os.close(fd)
                except FileNotFoundError:
                    pass
                self._publish_replace(sec.S, "degraded",
                                      _dump({"reason": reason, "first_ns": first, "count": count}))
                return True
        except (_SectionBusy, OSError, StateError):
            return False

    def append_log(self, record: dict, nested: bool = False) -> bool:
        """§20: one line, single O_APPEND write outside the section; rotation at 10 MiB inside it.

        A nested (pass-through) line is marked `"nested": true` and never rotates the log.
        """
        try:
            if self._read_format() != FORMAT_VERSION:
                return False  # §14/§20: only into a FORMAT read and understood; never write one
            if nested:
                record = dict(record, nested=True)
            d = self._sub("log")
            path = os.path.join(d, "runs.jsonl")
            try:
                if not nested and os.lstat(path).st_size >= LOG_ROTATE_BYTES:
                    with self._section(timeout=2.0) as sec:
                        if not sec.understood:
                            return False
                        sec.reconcile_if_due()
                        if os.lstat(path).st_size >= LOG_ROTATE_BYTES:
                            os.rename(path, os.path.join(d, "runs.1.jsonl"))
            except FileNotFoundError:
                pass
            line = _dump(record) + b"\n"
            if len(line) > LOG_LINE_MAX:
                record = dict(record, label=record.get("label", "")[:64])
                line = _dump(record) + b"\n"
                if len(line) > LOG_LINE_MAX:
                    return False
            fd = _open_checked(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT)
            try:
                os.write(fd, line)
            finally:
                os.close(fd)
            return True
        except (_SectionBusy, OSError, StateError):
            return False


# --------------------------------------------------------------------------------------
# Ticket (§10)


class Ticket:
    """A waiting request. `step()` is one poll (§10.3); the caller sleeps `poll_delay()`.

    `step()` never sleeps, and it is also what refreshes the ticket (§10.2). The caller MUST call it
    at least every 2 s while waiting, or other observers find the ticket STALLED and pass it over.
    `wait()` and the CLI do so. Used as a context manager, a ticket that was not admitted is
    withdrawn on exit.
    """

    def __init__(self, client: Client, name: str | None, fd: int | None, label: str, priority: int,
                 need: dict, token_only: bool = False):
        self.client = client
        self.name = name
        self.id = name[:ID_LEN] if name else None
        self._fd = fd
        self.label = label
        self.priority = priority
        self.need = need
        self.token_only = token_only
        self._grant: Grant | None = None
        self._done = False
        self._gen_seen = None
        self._queue: list[str] = []
        # name -> (mtime, since, at, pass): the unchanged time counts from `since`; `at` and
        # `pass` are the watched time and election of the last examination (§10.2).
        self._obs: dict[str, tuple[int, float, float, int]] = {}
        self._pass = 0
        self._last_seen_at: float | None = None  # observer time of the latest pass or examination
        self._unwatched = 0.0  # the observer's own absences: watched time is observer time minus this
        self._valid: dict[str, bool] = {}
        self._last_refresh_m = time.monotonic()
        self._last_refresh_w = time.time_ns()
        self._resume_bump_pending = False
        self._nonmatching = 0
        self._backoff = HEAD_BACKOFF_MIN
        self.position: int | None = None

    # ---- queue observation

    def _close_fd(self) -> None:
        if self._fd is not None:
            os.close(self._fd)
            self._fd = None

    def _refresh_if_due(self) -> None:
        m, w = time.monotonic(), time.time_ns()
        if m - self._last_refresh_m < REFRESH_S:
            return
        gap_m = m - self._last_refresh_m
        gap_w = (w - self._last_refresh_w) / 1e9
        os.utime(self._fd)  # futimens(fd, NULL): mtime := now, outside the section
        self._last_refresh_m, self._last_refresh_w = m, w
        if gap_m > STALL_S or gap_w > STALL_S:
            self._resume_bump_pending = True
        if self._resume_bump_pending:
            try:
                with self.client._section(timeout=0.25) as sec:
                    if sec.understood:
                        sec.reconcile_if_due()
                        sec.bump_gen()
                self._resume_bump_pending = False
            except _SectionBusy:
                pass

    def _cancel_requested(self) -> bool:
        path = os.path.join(self.client.state_dir(), "cancel", self.id)
        fd = _open_entry(path)
        if not isinstance(fd, int):
            return False  # gone, or ignored (never trusted)
        try:
            validate_cancel(parse_body(_read_upto(fd, MAX_BODY + 1)))
            return True
        except Malformed:
            return False  # a malformed cancel request is ignored, never trusted (§7.2)
        finally:
            os.close(fd)

    def _rescan(self) -> None:
        queue = os.path.join(self.client.state_dir(), "queue")
        names = []
        ignored = 0
        for n in _list(queue):
            if parse_name(n) is not None:
                if n != self.name:
                    names.append(n)
            elif not TMP_RE.fullmatch(n):
                ignored += 1
        names.sort(key=lambda n: n[:ID_LEN])
        self._queue = names
        live = set(names)
        self._obs = {k: v for k, v in self._obs.items() if k in live}
        self._valid = {k: v for k, v in self._valid.items() if k in live}
        self._nonmatching = ignored
        self.client.ignored_entries = ignored

    def _begin_pass(self, now: float) -> None:
        """Start an election at observer time `now` (§10.2): more than MAX_PASS_GAP_S since the
        observer's previous examination means it was stopped, suspended or starved, and that
        interval adds no watched time to any ticket."""
        last = self._last_seen_at
        if last is not None and self._pass > 0 and now - last > MAX_PASS_GAP_S:
            self._unwatched += now - last  # the observer's own absence is not watched
        self._pass += 1
        self._last_seen_at = now if last is None else max(last, now)

    def _observe(self, name: str, mtime: int, now: float) -> bool:
        """Record an mtime read at observer time `now`; True when the ticket is STALLED (§10.2).

        Only watched time counts: a ticket left unexamined by the previous election gets no
        credit for the interval since it was last examined, nor does any ticket for the
        observer's own absence (see _begin_pass), though a changed value still restarts its timer.
        """
        self._last_seen_at = now if self._last_seen_at is None else max(self._last_seen_at, now)
        w = now - self._unwatched
        seen = self._obs.get(name)
        if seen is None or seen[0] != mtime:
            self._obs[name] = (mtime, w, w, self._pass)
            return False
        _, since, at, last_pass = seen
        if self._pass > last_pass + 1:
            since += w - at  # the unwatched interval does not count
        self._obs[name] = (mtime, since, w, self._pass)
        return w - since >= STALL_S

    def _open_valid(self, name: str):
        """Open a queue entry: an fd, or 'gone' or 'ignored' (§4.2, §7.2)."""
        path = os.path.join(self.client.state_dir(), "queue", name)
        fd = _open_entry(path)
        if not isinstance(fd, int):
            return fd  # "gone", or "ignored" without opening it (§4.2, B1/D6)
        ok = self._valid.get(name)
        if ok is None:
            try:
                validate_ticket(parse_body(_read_upto(fd, MAX_BODY + 1)))
                ok = True
            except Malformed:
                ok = False
            self._valid[name] = ok
        if not ok:
            os.close(fd)
            return "ignored"
        return fd

    def _examine(self, name: str, now: float) -> str:
        """One ticket ahead: 'gone', 'ignored', 'dead', 'stalled' or 'live' (§10.3)."""
        fd = self._open_valid(name)
        if not isinstance(fd, int):
            return fd
        try:
            st = os.fstat(fd)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return "dead"
            except BlockingIOError:
                pass
            return "stalled" if self._observe(name, st.st_mtime_ns, now) else "live"
        finally:
            os.close(fd)

    def _watch(self, name: str, now: float) -> str:
        """Watch a front-window ticket: observe its mtime only; 'gone', 'ignored' or 'watched'."""
        fd = self._open_valid(name)
        if not isinstance(fd, int):
            return fd
        try:
            self._observe(name, os.fstat(fd).st_mtime_ns, now)
            return "watched"
        finally:
            os.close(fd)

    def _reelect(self) -> bool:
        """§12.1 step 5, inside the admitting section: re-list queue/; True when every
        well-formed ticket ahead is dead, gone or STALLED. One never observed counts as live."""
        now = self.client.observer_clock()
        queue = os.path.join(self.client.state_dir(), "queue")
        names = sorted((n for n in _list(queue) if parse_name(n) is not None and n != self.name),
                       key=lambda n: n[:ID_LEN])
        for n in names:
            if n[:ID_LEN] >= self.id:
                continue
            seen = n in self._obs
            verdict = self._examine(n, now) if seen else self._probe_only(n)
            if verdict in ("gone", "ignored", "dead", "stalled"):
                continue
            if not seen:
                self._gen_seen = None  # rescan at the next election, so that it is watched
            return False  # live, or first seen here
        return True

    def _probe_only(self, name: str) -> str:
        """A ticket never observed: 'gone', 'ignored', 'dead' or 'live' (§12.1 step 5)."""
        fd = self._open_valid(name)
        if not isinstance(fd, int):
            return fd
        try:
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return "dead"
            except BlockingIOError:
                return "live"
        finally:
            os.close(fd)

    def _elect(self) -> int:
        """The election (§10.3): examine the tickets ahead from the nearest up to the first live,
        non-stalled one, and watch the front window. Returns the position: 1 is the head, else
        1 plus the tickets ahead not passed over (those beyond the end of the scan count)."""
        now = self.client.observer_clock()
        self._begin_pass(now)
        ahead = [n for n in self._queue if n[:ID_LEN] < self.id]
        examined: set[str] = set()
        gone: set[str] = set()
        ignored = 0
        head = True
        for n in reversed(ahead):
            examined.add(n)
            verdict = self._examine(n, now)
            if verdict == "gone":
                gone.add(n)
            elif verdict == "ignored":
                ignored += 1  # malformed, or a bad type/owner/mode: never trusted (§7.2, B1/D6)
            elif verdict not in ("dead", "stalled"):  # dead and STALLED tickets are passed over
                head = False
                break
        watched = 0
        for n in ahead:  # the front window: the first FRONT_WINDOW tickets that are not gone
            if watched >= FRONT_WINDOW or n in examined:
                break
            verdict = self._watch(n, now)
            if verdict == "gone":
                gone.add(n)
            elif verdict == "watched":
                watched += 1
        self.client.ignored_entries = self._nonmatching + ignored
        if gone:
            self._queue = [n for n in self._queue if n not in gone]
            for n in gone:
                self._obs.pop(n, None)
        if head:
            return 1
        unexamined = 0  # well-formed tickets beyond the end of the scan, as Go counts them
        for n in ahead:
            if n in examined or n in gone:
                continue
            if n not in self._valid:
                self._open_valid_and_close(n)
            if self._valid.get(n):
                unexamined += 1
        return 1 + 1 + unexamined  # the ticket that ended the scan, and those beyond it

    def _open_valid_and_close(self, name: str) -> None:
        fd = self._open_valid(name)
        if isinstance(fd, int):
            os.close(fd)

    # ---- the poll

    def step(self):
        """One poll: ("waiting", position) or ("admitted", Grant). Raises Cancelled."""
        if self._grant is not None:
            return ("admitted", self._grant)
        if self._done:
            raise CoreslotError("ticket was withdrawn")
        try:
            if self.token_only:
                return self._step_token_only()
            self._refresh_if_due()
            if self._cancel_requested():
                self._finish_cancel()
                raise Cancelled(f"ticket {self.id} was cancelled")
            fmt = self.client._read_format()
            if fmt is not None and fmt != FORMAT_VERSION:
                self._become_token_only(fmt)
                return self._step_token_only()
            g = self.client._read_gen()
            if g != self._gen_seen:
                self._gen_seen = g
                self._rescan()
            self.position = self._elect()
            if self.position == 1:
                grant = self._try_admit()
                if grant is not None:
                    self._grant = grant
                    return ("admitted", grant)
                self._backoff = min(self._backoff * 2, HEAD_BACKOFF_MAX)
            else:
                self._backoff = HEAD_BACKOFF_MIN
            return ("waiting", self.position)
        except OSError as exc:
            raise StateError("io", str(exc)) from exc

    def poll_delay(self) -> float:
        if self.token_only or self.position == 1:
            return self._backoff
        return POLL_S

    def _try_admit(self) -> "Grant | None":
        try:
            with self.client._section(timeout=0.25) as sec:
                if not sec.understood:
                    self._become_token_only(sec.format)
                    return None
                if self._cancel_requested():  # admission step 1
                    self._close_fd()
                    with contextlib.suppress(FileNotFoundError):
                        os.unlink(os.path.join(sec.S, "queue", self.name))
                    with contextlib.suppress(FileNotFoundError):
                        os.unlink(os.path.join(sec.S, "cancel", self.id))
                    sec.bump_gen()
                    self._done = True
                    raise Cancelled(f"ticket {self.id} was cancelled")
                return self.client._admit_locked(sec, self.label, self.need, token_only=False, ticket=self)
        except _SectionBusy:
            return None

    def _finish_cancel(self) -> None:
        self._close_fd()  # the ticket is dead at once
        self._done = True
        try:
            with self.client._section(timeout=2.0) as sec:
                if sec.understood:
                    sec.reconcile_if_due()
                    with contextlib.suppress(FileNotFoundError):
                        os.unlink(os.path.join(sec.S, "queue", self.name))
                    with contextlib.suppress(FileNotFoundError):
                        os.unlink(os.path.join(sec.S, "cancel", self.id))
                    sec.bump_gen()
        except (_SectionBusy, OSError, StateError):
            pass

    def _become_token_only(self, fmt) -> None:
        """§15.2: a waiter that finds the format changed withdraws and goes token-only."""
        self._close_fd()  # withdrawal without writing into a format it does not understand
        self.token_only = True
        self.client._diag(f"coreslot: state directory changed to format {fmt}; withdrew and "
                          "continuing token-only")

    def _step_token_only(self):
        """§15.2: token-only admission, re-deciding the mode from FORMAT at every entry."""
        rejoined = False
        grant = None
        try:
            with self.client._section(timeout=0.25) as sec:
                if sec.understood:  # format 1 again, or none (just written): enqueue anew (I4)
                    sec.reconcile_if_due()
                    fd, name = self.client._publish_ticket(sec, self.label, self.priority, self.need)
                    self._rejoin(fd, name)
                    rejoined = True
                else:
                    grant = self.client._admit_locked(sec, self.label, self.need, token_only=True)
        except _SectionBusy:
            pass
        if rejoined:
            self.client._diag(f"coreslot: the state format is understood again; queued as {self.id}")
            return self.step()
        if grant is not None:
            self._grant = grant
            return ("admitted", grant)
        self._backoff = min(self._backoff * 2, HEAD_BACKOFF_MAX)
        self.position = 0
        return ("waiting", 0)

    def _rejoin(self, fd: int, name: str) -> None:
        self._fd, self.name, self.id = fd, name, name[:ID_LEN]
        self.token_only = False
        self._gen_seen = None
        self._queue, self._obs, self._valid = [], {}, {}
        self._last_refresh_m, self._last_refresh_w = time.monotonic(), time.time_ns()
        self._backoff = HEAD_BACKOFF_MIN

    def withdraw(self, remove: bool = True) -> None:
        """§13.2: close the ticket descriptor; optionally remove the file in a section."""
        if self._done or self._grant is not None:
            return
        self._done = True
        was_queued = self._fd is not None
        self._close_fd()
        if remove and was_queued and not self.token_only:
            try:
                with self.client._section(timeout=0.5) as sec:
                    if sec.understood:
                        sec.reconcile_if_due()
                        with contextlib.suppress(FileNotFoundError):
                            os.unlink(os.path.join(sec.S, "queue", self.name))
                        sec.bump_gen()
            except (_SectionBusy, OSError, StateError):
                pass

    def __enter__(self) -> "Ticket":
        return self

    def __exit__(self, *exc) -> None:
        self.withdraw()  # no effect once admitted: the Grant owns the descriptors then

    def wait(self, timeout: float | None = None) -> "Grant":
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            kind, value = self.step()
            if kind == "admitted":
                return value
            if deadline is not None and time.monotonic() >= deadline:
                self.withdraw()
                raise Refusal("timed out waiting")
            time.sleep(self.poll_delay())


# --------------------------------------------------------------------------------------
# Grant (§12, §17)


class Grant:
    def __init__(self, client: Client | None, label: str, need: dict, tokens: dict, hold_fd: int | None,
                 hold_name: str | None, clamped: dict, unbudgeted_resources: list, *,
                 reason: str | None = None, token_only: bool = False):
        self.client = client
        self.label = label
        self.need = need
        self._tokens = tokens  # res -> [(index, fd)] ascending
        self.hold_fd = hold_fd
        self.hold_name = hold_name
        self.clamped = clamped
        self.unbudgeted_resources = list(unbudgeted_resources)
        self.reason = reason
        self.token_only = token_only
        self.closed = False

    @property
    def tokens(self) -> dict[str, list[int]]:
        return {r: [i for i, _ in v] for r, v in self._tokens.items()}

    @property
    def granted(self) -> dict[str, int]:
        out = {}
        for r, (lo, _) in self.need.items():
            if r in self.unbudgeted_resources:
                out[r] = lo
            else:
                out[r] = len(self._tokens.get(r, ()))
        return out

    def token_fds(self) -> list[int]:
        return [fd for r in sorted(self._tokens) for _, fd in self._tokens[r]]

    def environment(self) -> dict[str, str]:
        env = {}
        for r, n in self.granted.items():
            env["CORESLOT_GRANTED_" + r.upper().replace("-", "_")] = str(n)
        if self.hold_fd is not None:
            env["CORESLOT_HELD_FD"] = "3"
        n = len(self.token_fds())
        if n:
            env["CORESLOT_TOKEN_FDS"] = "4" if n == 1 else f"4-{3 + n}"
        if self.reason:
            env["CORESLOT_UNBUDGETED"] = self.reason
        return env

    def __enter__(self) -> "Grant":
        return self

    def __exit__(self, *exc) -> None:
        self.close()

    def close(self, remove_hold: bool = True) -> None:
        if self.closed:
            return
        self.closed = True
        for fd in self.token_fds():
            os.close(fd)
        if self.hold_fd is not None:
            os.close(self.hold_fd)
            self.hold_fd = None
        if remove_hold and self.hold_name and self.client is not None:
            self.client.remove_hold(self.hold_name)


# --------------------------------------------------------------------------------------
# Descriptor contract (§17) and pass-through (§18)


def parse_token_fds(text: str) -> list[int]:
    """Parse CORESLOT_TOKEN_FDS; ValueError when it does not parse or names an fd twice."""
    fds: list[int] = []
    seen: set[int] = set()
    for item in text.split(","):
        m = re.fullmatch(r"([0-9]+)(?:-([0-9]+))?", item)
        if not m:
            raise ValueError(f"bad item {item!r}")
        a = int(m.group(1))
        b = int(m.group(2)) if m.group(2) is not None else a
        if a > b:
            raise ValueError(f"bad range {item!r}")
        if len(fds) + (b - a + 1) > MAX_TOTAL:
            raise ValueError("more than 512 descriptors")
        for fd in range(a, b + 1):
            if fd in seen:
                raise ValueError(f"descriptor {fd} listed twice")
            seen.add(fd)
            fds.append(fd)
    return fds


def validate_passthrough(client: Client, text: str) -> dict[str, int]:
    """§18 rule 2: returns held count per resource; InvalidContext when it does not validate."""
    try:
        fds = parse_token_fds(text)
    except ValueError as exc:
        raise InvalidContext(f"CORESLOT_TOKEN_FDS={text!r} does not parse: {exc}") from exc
    try:
        S = client.state_dir()
        res_dir = os.path.join(S, "res")
        index: dict[tuple[int, int], str] = {}
        for r in _list(res_dir):
            d = os.path.join(res_dir, r)
            if not is_resource_name(r):
                continue
            for name in _list(d):
                if INDEX_RE.fullmatch(name) and int(name) <= MAX_INDEX:
                    path = os.path.join(d, name)
                    with contextlib.suppress(OSError):
                        st = os.lstat(path)
                        index[(st.st_dev, st.st_ino)] = r + "/" + name
    except (OSError, StateError) as exc:
        raise InvalidContext(f"cannot read the state directory to validate the context: {exc}") from exc
    held: dict[str, int] = {}
    seen: dict[tuple[int, int], int] = {}
    for fd in fds:
        try:
            st = os.fstat(fd)
        except OSError as exc:
            raise InvalidContext(f"descriptor {fd} is not open (did a launcher close the inherited "
                                 "descriptors?)") from exc
        if (st.st_dev, st.st_ino) in seen:
            raise InvalidContext(f"descriptors {seen[(st.st_dev, st.st_ino)]} and {fd} are the same "
                                 "token file: a launcher duplicated a descriptor, and counting both "
                                 "would overstate the grant")
        seen[(st.st_dev, st.st_ino)] = fd
        key = index.get((st.st_dev, st.st_ino))
        if key is None:
            raise InvalidContext(f"descriptor {fd} is not a token file of {S}")
        if _probe_path(os.path.join(res_dir, key)) is not False:
            raise InvalidContext(f"token {key} is not held (is the parent run gone?)")
        r = key.split("/", 1)[0]
        held[r] = held.get(r, 0) + 1
    return held


_PLACEHOLDER = re.compile(r"\{\{n(?::[a-z][a-z0-9-]{0,31})?\}\}|\{n(?::([a-z][a-z0-9-]{0,31}))?\}")


def placeholder_resources(argv: list[str], subst: bool) -> set[str]:
    out = set()
    for a in argv:
        matches = list(_PLACEHOLDER.finditer(a)) if subst else (
            [m] if (m := _PLACEHOLDER.fullmatch(a)) else [])
        for m in matches:
            if not m.group(0).startswith("{{"):
                out.add(m.group(1) or "cpu")
    return out


def substitute(argv: list[str], granted: dict[str, int], subst: bool) -> list[str]:
    def rep(m):
        if m.group(0).startswith("{{"):
            return m.group(0)[1:-1]
        return str(granted[m.group(1) or "cpu"])

    out = []
    for a in argv:
        if subst:
            out.append(_PLACEHOLDER.sub(rep, a))
        else:
            m = _PLACEHOLDER.fullmatch(a)
            out.append(rep(m) if m else a)
    return out


_CHILD_DEFAULT_SIGNALS = tuple(
    s for s in (getattr(signal, n, None) for n in
                ("SIGINT", "SIGQUIT", "SIGPIPE", "SIGXFSZ", "SIGTSTP", "SIGTTIN", "SIGTTOU"))
    if s is not None)


def spawn_and_wait(argv: list[str], env: dict[str, str], hold_fd: int | None, token_fds: list[int],
                   remap: bool = True) -> int:
    """Start argv with fd 3 = hold and fds 4.. = tokens (§17); return the exit status (§19)."""
    n = len(token_fds)
    raise_nofile(4 + n + 256)
    dups: list[int] = []
    actions = []
    try:
        if remap:
            high = 4 + n + 8
            if hold_fd is not None:
                d = fcntl.fcntl(hold_fd, fcntl.F_DUPFD_CLOEXEC, high)
                dups.append(d)
                actions.append((os.POSIX_SPAWN_DUP2, d, 3))
            else:
                # §17: no hold, so fd 3 is closed in the child even if this process has one open.
                # dup2 first so that the close always names an open descriptor (a close of an
                # unopened fd fails posix_spawn on some systems).
                d = os.open(os.devnull, os.O_RDONLY | os.O_CLOEXEC)
                dups.append(d)
                actions += [(os.POSIX_SPAWN_DUP2, d, 3), (os.POSIX_SPAWN_CLOSE, 3)]
            for i, fd in enumerate(token_fds):
                d = fcntl.fcntl(fd, fcntl.F_DUPFD_CLOEXEC, high)
                dups.append(d)
                actions.append((os.POSIX_SPAWN_DUP2, d, 4 + i))
        old_int = signal.signal(signal.SIGINT, signal.SIG_IGN)
        old_quit = signal.signal(signal.SIGQUIT, signal.SIG_IGN)
        try:
            try:
                pid = os.posix_spawnp(argv[0], argv, env, file_actions=actions,
                                      setsigdef=_CHILD_DEFAULT_SIGNALS)
            except FileNotFoundError:
                print(f"coreslot: {argv[0]}: command not found", file=sys.stderr)
                return EXIT_NOT_FOUND
            except (PermissionError, IsADirectoryError) as exc:
                print(f"coreslot: {argv[0]}: {exc.strerror}", file=sys.stderr)
                return EXIT_NOT_EXECUTABLE
            except OSError as exc:
                if exc.errno == errno.ENOEXEC:
                    print(f"coreslot: {argv[0]}: {exc.strerror}", file=sys.stderr)
                    return EXIT_NOT_EXECUTABLE
                print(f"coreslot: cannot start {argv[0]}: {exc}", file=sys.stderr)
                return EXIT_INTERNAL
            finally:
                for d in dups:
                    os.close(d)
                dups = []
            while True:
                try:
                    _, status = os.waitpid(pid, 0)
                    break
                except InterruptedError:
                    continue
        finally:
            signal.signal(signal.SIGINT, old_int)
            signal.signal(signal.SIGQUIT, old_quit)
    finally:
        for d in dups:
            os.close(d)
    code = os.waitstatus_to_exitcode(status)
    return 128 - code if code < 0 else code


# --------------------------------------------------------------------------------------
# CLI


USAGE = """usage: coreslot_client.py run [--need RES=MIN..MAX]... [--label L] [--priority P]
           [--timeout SECONDS] [--strict] [--independent] [--subst] [--refusal-code N] -- CMD [ARG...]
       coreslot_client.py cancel [--timeout SECONDS] TICKET-ID"""


class _RunOptions:
    def __init__(self):
        self.needs: list[str] = []
        self.label = ""
        self.priority: int | None = None
        self.timeout: float | None = None
        self.strict = False
        self.independent = False
        self.subst = False
        self.refusal_code = EXIT_REFUSAL
        self.command: list[str] = []


def _parse_run(args: list[str]) -> _RunOptions:
    o = _RunOptions()
    i = 0

    def value(flag):
        nonlocal i
        if i + 1 >= len(args):
            raise UsageError(f"{flag} needs a value")
        i += 1
        return args[i]

    while i < len(args):
        a = args[i]
        if a == "--":
            o.command = args[i + 1:]
            break
        flag, eq, inline = a.partition("=") if a.startswith("--") else (a, "", "")
        get = (lambda f: inline) if eq else value
        if flag == "--need":
            o.needs.append(get(flag))
        elif flag == "--label":
            o.label = get(flag)
        elif flag == "--priority":
            try:
                o.priority = int(get(flag))
            except ValueError as exc:
                raise UsageError("--priority must be an integer") from exc
        elif flag == "--timeout":
            try:
                o.timeout = float(get(flag))
            except ValueError as exc:
                raise UsageError("--timeout must be a number of seconds") from exc
        elif flag == "--refusal-code":
            try:
                o.refusal_code = int(get(flag))
            except ValueError as exc:
                raise UsageError("--refusal-code must be an integer") from exc
        elif a == "--strict":
            o.strict = True
        elif a == "--independent":
            o.independent = True
        elif a == "--subst":
            o.subst = True
        else:
            raise UsageError(f"unknown argument {a!r}")
        i += 1
    if not o.command:
        raise UsageError("missing -- CMD")
    return o


def _needs_from(opts: _RunOptions) -> dict[str, tuple[int, int]]:
    raw: dict[str, tuple[int, int]] = {}
    for text in opts.needs or ["cpu=1..1"]:
        name, pair = parse_need_arg(text)
        if name in raw:
            raise UsageError(f"duplicate resource {name}")
        raw[name] = pair
    return normalize_need(raw)


def _env_form(name: str) -> str:
    return name.upper().replace("-", "_")


def _strip_context(env: dict[str, str]) -> dict[str, str]:
    return {k: v for k, v in env.items()
            if not (k in _CONTEXT_VARS or k.startswith("CORESLOT_GRANTED_"))}


def _diag(msg: str) -> None:
    print(msg, file=sys.stderr, flush=True)


def _notices(grant: Grant, sizes_hint: dict | None = None) -> None:
    for r, lo in sorted(grant.clamped.items()):
        n = grant.granted[r]
        _diag(f"coreslot: {r}: requested {lo}, budget is {n}; running alone with {n}")
    for r in grant.unbudgeted_resources:
        if not grant.reason:
            _diag(f"coreslot: {r}: not declared in the configuration; running unbudgeted with "
                  f"{grant.need[r][0]}")


def _run_unbudgeted(opts, needs, env, reason: str, hold=None, strip: bool = True) -> int:
    child_env = _strip_context(env) if strip else dict(env)
    for r, (lo, _) in needs.items():
        child_env["CORESLOT_GRANTED_" + _env_form(r)] = str(lo)
    child_env["CORESLOT_UNBUDGETED"] = reason
    hold_fd = hold[0] if hold else None
    if hold_fd is not None:
        child_env["CORESLOT_HELD_FD"] = "3"
    argv = substitute(opts.command, {r: lo for r, (lo, _) in needs.items()}, opts.subst)
    # A fail-open run builds §17 from its own hold; an inherited-unbudgeted run (§18 rule 1)
    # passes what it inherited on untouched.
    return spawn_and_wait(argv, child_env, hold_fd, [], remap=strip)


def _fail_open(client: Client, opts, needs, env, err: StateError, t0: int) -> int:
    if opts.strict:
        _diag(f"coreslot: state error ({err}); refusing (strict)")
        return opts.refusal_code
    _diag(f"coreslot: state error ({err}); running unbudgeted with the minimum of each resource")
    hold = client.publish_unbudgeted_hold(opts.label, needs)
    if not client.record_degraded(err.reason):
        _diag("coreslot: could not record degraded mode in the state directory")
    started = time.monotonic()
    status = _run_unbudgeted(opts, needs, env, err.reason, hold)
    if hold:
        os.close(hold[0])
        client.remove_hold(hold[1])
    if not client.append_log(_log_record(opts, needs, {r: lo for r, (lo, _) in needs.items()}, [],
                                         sorted(needs), 0.0, time.monotonic() - started, status, t0,
                                         err.reason)):
        _diag("coreslot: could not append to the run log")
    return status


def _log_record(opts, needs, granted, clamped, unbudgeted, waited, ran, status, t0, fail_open=""):
    try:
        load1 = os.getloadavg()[0]
    except OSError:
        load1 = 0.0
    return {"time_ns": t0, "label": opts.label,
            "need": {r: [lo, hi] for r, (lo, hi) in needs.items()},
            "grant": granted, "clamped": clamped, "unbudgeted_resources": unbudgeted,
            "waited_s": round(waited, 3), "ran_s": round(ran, 3), "exit": status,
            "load1": load1, "fail_open": fail_open}


def cli_run(args: list[str]) -> int:
    opts = _parse_run(args)
    needs = _needs_from(opts)
    missing = placeholder_resources(opts.command, opts.subst) - set(needs)
    if missing:
        raise UsageError(f"placeholder names a resource the request does not name: {sorted(missing)}")
    t0 = time.time_ns()
    env = dict(os.environ)
    if not opts.independent and env.get("CORESLOT_UNBUDGETED"):  # §18 rule 1
        _diag(f"coreslot: inside an unbudgeted run ({env['CORESLOT_UNBUDGETED']}); running "
              "unbudgeted with the minimum of each resource")
        started = time.monotonic()
        status = _run_unbudgeted(opts, needs, env, env["CORESLOT_UNBUDGETED"], strip=False)
        _nested_log(opts, needs, {r: lo for r, (lo, _) in needs.items()}, [], sorted(needs),
                    time.monotonic() - started, status, t0, env["CORESLOT_UNBUDGETED"])
        return status
    if not opts.independent and env.get("CORESLOT_TOKEN_FDS"):  # §18 rule 2
        return _cli_passthrough(opts, needs, env, t0)
    _close_inherited_context(env)  # §17: not a pass-through, so inherited grant fds are not passed on
    client = Client(cli=True)
    waited0 = time.monotonic()
    deadline = None if opts.timeout is None else waited0 + opts.timeout
    try:
        ticket = client.enqueue(opts.label, opts.priority, needs, timeout=opts.timeout)
    except StateError as err:
        return _fail_open(client, opts, needs, env, err, t0)
    except Refusal as err:
        _diag(f"coreslot: {err}; refusing")
        return opts.refusal_code
    except KeyboardInterrupt:  # interrupted while entering the section: nothing was published
        return EXIT_INTERRUPTED
    last_line = waited0
    try:
        while True:
            kind, value = ticket.step()
            if kind == "admitted":
                grant = value
                break
            now = time.monotonic()
            if deadline is not None and now >= deadline:
                ticket.withdraw()
                _diag("coreslot: timed out waiting; refusing")
                return opts.refusal_code
            if now - last_line >= WAIT_LINE_S:
                last_line = now
                want = ", ".join(f"{r} {lo}..{hi}" for r, (lo, hi) in needs.items())
                _diag(f"coreslot: waiting for {want}: position {value}")
            delay = ticket.poll_delay()
            if deadline is not None:
                delay = max(0.0, min(delay, deadline - now))
            time.sleep(delay)
    except KeyboardInterrupt:
        ticket.withdraw()
        return EXIT_INTERRUPTED
    except Cancelled as err:
        _diag(f"coreslot: {err}")
        return opts.refusal_code
    except StateError as err:
        ticket.withdraw(remove=False)
        return _fail_open(client, opts, needs, env, err, t0)
    waited = time.monotonic() - waited0
    if grant.token_only:
        _diag("coreslot: running token-only (unknown state format); CORESLOT_UNBUDGETED=format")
    _notices(grant)
    child_env = _strip_context(env)
    child_env.update(grant.environment())
    argv = substitute(opts.command, grant.granted, opts.subst)
    started = time.monotonic()
    status = spawn_and_wait(argv, child_env, grant.hold_fd, grant.token_fds())
    ran = time.monotonic() - started
    granted = grant.granted
    grant.close()
    if not grant.token_only:
        client.append_log(_log_record(opts, needs, granted, sorted(grant.clamped),
                                      grant.unbudgeted_resources, waited, ran, status, t0))
    return status


def _close_inherited_context(env: dict[str, str]) -> None:
    """§17: a run that is not a pass-through does not pass on the inherited grant descriptors.

    The descriptors the inherited CORESLOT_HELD_FD and CORESLOT_TOKEN_FDS name become
    close-on-exec when they are regular files at fd 3 or above. A named standard stream (0-2) or a
    descriptor that is not a regular file cannot be a grant's (a launcher closed the original and
    its number was reused), so it is left open; every other inherited descriptor passes through
    untouched.
    """
    fds: list[int] = []
    with contextlib.suppress(ValueError):
        fds = parse_token_fds(env.get("CORESLOT_TOKEN_FDS", "")) if env.get("CORESLOT_TOKEN_FDS") else []
    if env.get("CORESLOT_HELD_FD", "").isdigit():
        fds.append(int(env["CORESLOT_HELD_FD"]))
    for fd in fds:
        if fd < 3:
            continue
        with contextlib.suppress(OSError):
            if stat.S_ISREG(os.fstat(fd).st_mode):
                os.set_inheritable(fd, False)


def _nested_log(opts, needs, granted, clamped, unbudgeted, ran, status, t0, fail_open="") -> None:
    """E6/§20: a pass-through appends a `"nested": true` line, outside the section, never rotating."""
    client = Client(cli=True)
    if not client.append_log(_log_record(opts, needs, granted, clamped, unbudgeted, 0.0, ran, status,
                                         t0, fail_open), nested=True):
        _diag("coreslot: no run-log line (state directory missing, not understood, or not writable)")


def _cli_passthrough(opts, needs, env, t0) -> int:
    client = Client(cli=True)
    try:
        held = validate_passthrough(client, env["CORESLOT_TOKEN_FDS"])
    except InvalidContext as err:
        _diag(f"coreslot: invalid nested context: {err}; refusing")
        return opts.refusal_code
    child_env = dict(env)
    granted = {}
    for r, (lo, hi) in needs.items():
        if r in held:
            granted[r] = min(hi, held[r])
            if lo > held[r]:
                _diag(f"coreslot: {r}: requested {lo}, the parent holds {held[r]}; running with {held[r]}")
        else:
            granted[r] = lo
            _diag(f"coreslot: {r}: not held by the parent run; running unbudgeted with {lo}")
        child_env["CORESLOT_GRANTED_" + _env_form(r)] = str(granted[r])
    argv = substitute(opts.command, granted, opts.subst)
    started = time.monotonic()
    status = spawn_and_wait(argv, child_env, None, [], remap=False)
    clamped = sorted(r for r, (lo, _) in needs.items() if r in held and lo > held[r])
    _nested_log(opts, needs, granted, clamped, sorted(r for r in needs if r not in held),
                time.monotonic() - started, status, t0)
    return status


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv
    try:
        if not argv:
            raise UsageError("missing subcommand")
        if argv[0] == "run":
            return cli_run(argv[1:])
        if argv[0] == "cancel":
            rest, timeout = argv[1:], 30.0
            if len(rest) == 3 and rest[0] == "--timeout":
                try:
                    timeout = float(rest[1])
                except ValueError as exc:
                    raise UsageError("--timeout must be a number of seconds") from exc
                rest = rest[2:]
            if len(rest) != 1:
                raise UsageError("cancel takes [--timeout SECONDS] and one ticket id")
            try:
                if not Client(cli=True).cancel(rest[0], timeout=timeout):
                    _diag("coreslot: cancellation already requested")
            except Refusal as err:
                _diag(f"coreslot: {err}; refusing")
                return EXIT_REFUSAL
            except StateError as err:
                _diag(f"coreslot: {err}")
                return EXIT_INTERNAL
            return 0
        raise UsageError(f"unknown subcommand {argv[0]!r}")
    except UsageError as err:
        _diag(f"coreslot: {err}\n{USAGE}")
        return EXIT_USAGE


if __name__ == "__main__":
    sys.exit(main())
