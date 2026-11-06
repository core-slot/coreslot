# coreslot protocol, format 2

This document is the normative definition of the coreslot on-disk protocol, format 2: the state
directory, its files and their bodies, the admission critical section, the watchdog that keeps
memory pressure from stalling the work the clients started, and the descriptor and environment
contract between a run and the command it starts. Memory is the only resource. A client written
from this document alone, in any language, shares one memory budget and one watchdog with every
other conforming client on the machine.

Format 2's names, bodies and layout are never redefined once released: a release that needs
different metadata introduces a new format number instead (section 15). Within a format the only
additions are an optional key that every reader may ignore (section 7.2) and an optional file that
no admission reads, as the needs file is (section 5.4). Until `v1.0.0` a minor release may
introduce such a number, and may change what format 2 means elsewhere, such as the configuration's
defaults and size grammar (section 5); from `v1.0.0` on, either needs a new major version
([compatibility policy](docs/compatibility.md#versions)).

## 1. Conventions

The key words MUST, MUST NOT, REQUIRED, SHALL, SHALL NOT, SHOULD, SHOULD NOT, RECOMMENDED, MAY and
OPTIONAL are to be interpreted as described in RFC 2119. Passages marked **Implementation note**
are informative: they say what a particular implementation or test does, and impose no
requirement on a client.

- **Client:** any process that reads or changes the state directory: the `coreslot` CLI or an
  independent implementation.
- **`$S`:** the state directory (section 4.1).
- **Wall clock:** `CLOCK_REALTIME`. Every `*_ns` value of a body is nanoseconds since the Unix epoch
  on the wall clock of the process that wrote it, except where section 7.11 says otherwise.
- **Monotonic clock:** the observing process's own monotonic clock (`CLOCK_MONOTONIC`, or any clock
  that does not step with the wall clock). It is never written to disk.
- **Lock:** `flock(2)` on an open file description. "Probe" means: open the file, try
  `flock(LOCK_EX|LOCK_NB)`, and close the descriptor again at once. A probe that obtains the lock
  proves that no other open file description holds it.
- **Integer** (in a body): a JSON number with no fraction and no exponent part, within the signed
  64-bit range. `1.0` and `1e3` are not integers.
- **Set** (an environment variable): present with a non-empty value. A variable whose value is
  empty is treated exactly as an absent one, everywhere in this document.
- Sizes: `KiB` = 1 024 bytes, `MiB` = 1 048 576 bytes, `GiB` = 1 024 MiB.
- **Need:** the memory a run asks for, in MiB. **Footprint:** the memory the run's process tree
  uses, in MiB. **Room:** what the live holds leave of the machine's free memory (section 12.1).

## 2. Contract inventory

Every shared file, field, environment variable and transition of format 2, and where it is defined.

| Item | Kind | Defined in |
|---|---|---|
| `FORMAT` | file | [7.6](#76-format), [15](#15-formats-and-an-unimplemented-format) |
| `admit.lock` | file | [7.7](#77-admitlock), [8](#8-the-admission-critical-section) |
| `gen` | file | [7.8](#78-gen), [10](#10-liveness-waiting-and-reconciliation) |
| `watch.json` | file | [7.11](#711-watchjson), [23](#23-the-watchdog) |
| `queue/` | directory of tickets | [7.3](#73-ticket), [10](#10-liveness-waiting-and-reconciliation) |
| `holds/` | directory of holds | [7.4](#74-hold), [6](#6-holds-and-descriptor-ownership), [12](#12-admission) |
| `cancel/` | directory of cancel requests | [7.5](#75-cancel-request), [13](#13-cancellation-and-withdrawal) |
| `degraded` | file | [7.9](#79-degraded), [14](#14-errors-fail-open-and-degraded-mode) |
| `log/` | directory of the run log | [7.10](#710-log-run-log), [20](#20-run-log) |
| `config.toml` (under the override only) | file | [5.3](#53-configuration-file) |
| `needs.toml` (under the override only) | file | [5.4](#54-needs-file) |
| ticket body | body | [7.3](#73-ticket) |
| hold body | body | [7.4](#74-hold) |
| cancel body | body | [7.5](#75-cancel-request) |
| run-log memory keys | field | [20](#20-run-log) |
| `CORESLOT_GRANTED_MEM` | environment | [17](#17-descriptor-and-environment-contract) |
| `CORESLOT_HOLD` | environment | [17](#17-descriptor-and-environment-contract), [18](#18-nested-runs-pass-through) |
| `CORESLOT_UNBUDGETED` | environment | [14](#14-errors-fail-open-and-degraded-mode), [18](#18-nested-runs-pass-through) |
| `CORESLOT_STATE_DIR` | environment | [4.1](#41-location) |
| enqueue | transition | [10.1](#101-enqueue), example [21.1](#211-enqueue) |
| refresh | transition | [10.2](#102-refresh-and-stalled), example [21.2](#212-refresh) |
| admission | transition | [12](#12-admission), example [21.3](#213-admission) |
| level gate | transition | [12.4](#124-the-level-gate), example [21.8](#218-tick-and-level) |
| tick | transition | [23.1](#231-the-tick), example [21.8](#218-tick-and-level) |
| stop round | transition | [23.5](#235-stop-rounds), example [21.9](#219-stop-round-and-restart) |
| restart | transition | [24](#24-restart-of-a-stopped-run), example [21.9](#219-stop-round-and-restart) |
| cancellation | transition | [13.1](#131-cancellation), example [21.4](#214-cancellation) |
| withdrawal | transition | [13.2](#132-withdrawal), example [21.5](#215-withdrawal) |
| reconciliation | transition | [10.4](#104-reconciliation), example [21.6](#216-reconciliation) |
| pass-through | transition | [18](#18-nested-runs-pass-through), example [21.7](#217-pass-through) |

## 3. State machine and invariants

A request is in exactly one of these states. Transitions marked † happen inside the admission
critical section (section 8).

```
           enqueue†              admit†                    child exits; last descriptor closed
 (none) ───────────────▶ WAITING ─────────▶ HELD ─────────────────────────────────▶ (none)
                           │   ▲
     no change observed    │   │ refresh observed
     for 10 s (10.2)       ▼   │
                         STALLED
 WAITING/STALLED ── cancel or owner gone (descriptor closed) ──▶ (none)
 HELD ── stopped for memory, fewer than 3 times (24) ──▶ WAITING (enqueue† at its first arrival)
```

WAITING and STALLED are the same file; STALLED is a verdict each observer reaches on its own
(section 10.2). A ticket or hold whose lock can be taken is **dead**, whatever its mtime; the next
reconciliation (section 10.4) removes its file.

Every conforming client MUST preserve these invariants:

- **I1 identity:** a live hold is locked through an open file description whose inode equals
  `stat($S/holds/<hold>).st_ino`, and keeps that inode while it lives: its body is rewritten in
  place, never replaced (section 7.4). A holder checks this when it rewrites its body and at
  release (section 12.3); a holder that finds `st_nlink == 0` or a different inode at its hold's
  path has a state error (section 14) and reports degraded mode to its user.
- **I2 truthful reservation:** a live hold's body says what it reserves (`need_mib`, section 7.4),
  and nothing is reserved without a live hold.
- **I3 soft room:** an ordinary admission starts a request only when its need fits the room
  (section 12.1). The budget is soft: the room is computed from the kernel's readings and the hold
  bodies, and is not a cap. The exceptions are the idle rule (a head starts when no hold is live),
  an unreadable headroom (nothing gates the head), both of section 12.1, and a container request
  (section 12.1), whose room leaves out the hold bodies.
- **I4 order:** ordinary admissions happen in order-key order (section 9) among waiters whose
  tickets were published before the election (section 10.3): the head blocks every waiter behind
  it except one that passes it by duration (section 10.3) and, while a hold is live, a container
  request (section 12.1), and a waiter is passed over only while it is STALLED, dead or canceled.
- **I5 progress:** assuming every live client is scheduled at least once per poll period, if the
  head's need fits the room (or no hold is live), the level gate (section 12.4) lets it start, and
  no process is stopped inside the admission section, the head is admitted within 3 s; up to 4
  consecutive stopped tickets at the front of the queue stop blocking it within ≈ 11 s (10 s
  without an observed change plus one poll). A stopped ticket further back stops blocking a waiter
  within 11 s of becoming the nearest ticket that waiter has not passed over. While a hold is live,
  container requests (section 12.1) may take the room a head waits for and so delay it, for as long
  as they keep coming and within its own timeout; the head keeps its place. With no hold live they
  leave the head its start.
- **I6 one budget:** every client of format 2 reads the same holds and the same
  `watch.json` under the same admission lock.
- **I7 own children only:** a stop round lists only running holds (section 23.3), never the oldest
  of them, one whose `victim` is false nor one whose label has no normal share, and a run signals only the process group of its own
  command, only while it has not reaped it (section 23.5).

## 4. State directory and filesystem policy

### 4.1 Location

- macOS: `<home>/Library/Application Support/coreslot/` (the path contains a space; clients MUST
  handle it everywhere they use the path).
- Linux and every other Unix: `<home>/.local/state/coreslot/`.
- `<home>` MUST come from the password database (`getpwuid(getuid())`), not `$HOME` or any
  `XDG_*` variable, so a shell, a cron job, a user service and an ssh session of the same user
  resolve the same directory.

  > **Implementation note.** On every Unix, when the Go CLI's lookup reports no entry
  > for the uid, or finds no `/etc/passwd`, it runs `getent passwd <uid>` from a fixed system path,
  > never through `$PATH`, for at most 5 s and takes the home directory from its answer, which
  > resolves through NSS as `getpwuid` does. That matters most to a cgo-free build for Linux (the
  > release archives), which reads `/etc/passwd` alone; a cgo build has asked NSS already, and
  > getent asks it a second time.
- The only override is the environment variable `CORESLOT_STATE_DIR`. When it is set and
  non-empty, its value is the state directory; it MUST be an absolute path, and a relative value is
  a state error (section 14). The override is for tests: it creates a separate budget, and it also
  moves the configuration (section 5.3).

### 4.2 Modes, types and ownership

- Every directory a client creates is mode `0700`; every file `0600`; files are created with
  `O_NOFOLLOW`.
- On open, a client MUST check that each existing path is a regular file or directory of the
  expected type, owned by the user, not a symlink, and not group- or world-writable; anything else
  is a state error (section 14). "Owned by the user" means owned by the process's real user id
  (`getuid()`, the id section 4.1 resolves the home from); a client whose effective user id
  differs from its real one is unsupported. "Group- or world-writable" means
  `mode & 0o022 != 0`. The check applies to `$S` and to every file and directory of the layout
  (section 4.4) that the client opens, probes or removes; a directory or top-level file of the
  layout, or a temporary file in `$S`, that is a symlink, a directory, another user's, or group- or
  world-writable is a state error. Inside `queue/`, `holds/` and `cancel/` the rule is different:
  an entry whose name matches the directory's grammar (section 7.1, temporaries included) but whose
  type, owner or mode fails the checks above is not a state error. It is a **stray**: ignored and
  counted exactly like an entry with a malformed body (section 7.2): never trusted, never opened
  or probed, never removed (reconciliation skips it and goes on), and `status` and `doctor` report
  it in the ignored count. An entry whose name does not match its directory's grammar is ignored
  whatever its type (section 4.4).
  These directories are `0700` and the user's own, so only the user (or the superuser) can place an
  entry in them; a hostile same-user process is outside the threat model.
- Every descriptor a client opens on a file under `$S` MUST be close-on-exec (`O_CLOEXEC`), except
  the copy it deliberately passes to a child (section 17). A leaked descriptor keeps a lock alive
  for as long as the process that inherited it.

### 4.3 Host-local filesystems only

A client MUST check the filesystem with `statfs` and refuse network, FUSE and VM-shared filesystems.
At least these MUST be refused, as a state error:

| System | Refused filesystems |
|---|---|
| macOS (`f_fstypename`) | `nfs`, `smbfs`, `afpfs`, `webdav`, and every macFUSE type (names beginning with `macfuse` or `osxfuse`) |
| Linux (`f_type`) | NFS `0x6969`, CIFS `0xFF534D42`, SMB2 `0xFE534D42`, SMB `0x517B`, FUSE `0x65735546`, 9p `0x01021997`, vboxsf `0x786F4256`, Ceph `0x00C36400`, AFS `0x5346414F`, kAFS `0x6B414653` |

On Linux, `f_type` is compared in its low 32 bits (`f_type & 0xFFFFFFFF`): its C type is a signed
`long` on some architectures, and the magic numbers are 32-bit values. A client that cannot
determine the filesystem type (`statfs` fails or is not available to it) treats that as a state
error. A client MAY read the type from another kernel source when `statfs` cannot be called (for
example `/proc/self/mountinfo` on Linux), provided it refuses at least the filesystems above.

A run inside a container sees the container's filesystem and therefore its own budget; containers
take part through a client on the host.

### 4.4 Layout

```
FORMAT           metadata format (section 7.6)
admit.lock       the admission lock (section 7.7); never deleted, semantics fixed for every format
gen              {"gen": n, "reconciled_ns": t} (section 7.8)
watch.json       the shared watchdog state (section 7.11)
queue/           tickets (section 7.3)
holds/           holds (section 7.4)
cancel/          cancellation requests (section 7.5)
degraded         degraded-mode record (section 7.9)
log/runs.jsonl   run log (section 7.10)
config.toml      configuration (section 5.3); optional, present only under the override
needs.toml       needs file (section 5.4); optional, present only under the override; never checked
```

`log/` may also contain `runs.1.jsonl`, `watch.jsonl` and `watch.1.jsonl` (section 23.6). Each of
`queue/`, `holds/`, `cancel/` and `$S` itself may contain temporary files named `.tmp-<pid>-<seq>`
(section 11). A client creates a directory of the layout when it first needs it, with mode `0700`;
`EEXIST` is not an error, and the existing entry is checked as in section 4.2. Missing ancestors
of `$S` (`~/.local/state/` often does not exist) are created with mode `0700` and are not checked:
the policy of section 4.2 starts at `$S`. `queue/`, `holds/`, `cancel/` and `log/` are created only
by a client that has read a `FORMAT` it understands (section 15.2).

Directory listings are bounded: a directory holds at most 65 536 entries. A reader MUST NOT read
more than 65 537 entries of one directory, and a directory with more than 65 536 is a state error.
Publishing into a directory that already holds 65 536 entries is a state error. Entries that do not
match the grammar of their directory (section 7.1) are ignored and counted (`status` reports the
count); they are never removed except by reconciliation's rules (section 10.4).

The directory is not a cache: liveness is decided by locks. Deleting or resetting `$S` while any
client is live loses the reservations of its holds and is not supported. The CLI's `doctor
--reset` runs inside the admission section and checks that no holds or tickets exist that some
process still locks; only then does it remove the entries of `queue/`, `holds/` and `cancel/`,
`gen`, `watch.json`, `degraded` and the `res/` directory a directory of another format may hold,
and writes `FORMAT` naming format 2. It keeps `admit.lock`, `config.toml` and `needs.toml`: the
format-stable file (section 15.1), whose inode live clients may hold open, and the configuration.
When `FORMAT` names format 2, or is missing (which the first client writes as 2, section 15.1),
the reset is a repair and keeps `log/` too: the run log is what every label's learned need and
normal stall share come from (sections 20 and 23). When `FORMAT` names another format or cannot be
read, it removes the entries of `log/` as well, and the CLI says so: every label learns its need
again. To make the labels learn again without a format change, remove `log/runs.jsonl` and
`log/runs.1.jsonl`; to set one label's need, pin it in the needs file (section 5.4). A reset
never deletes, renames or replaces `admit.lock`. Whatever `FORMAT` names, missing, malformed or another format, it recreates
the directory as format 2; a live hold or ticket of any format stops it. It does not reconcile
first (section 10.4): it removes every entry of `queue/`, `holds/` and `cancel/` reconciliation
would remove, and more. Temporary files directly in `$S` (section 11) are not entries it removes;
the next reconciliation removes them once they are older than 30 s.

## 5. Sizes and configuration

### 5.1 The machine's memory

Two readings of the machine decide admission, both in MiB:

- **`memory`**, the physical memory, rounded down: the `hw.memsize` sysctl on macOS; `MemTotal` in
  `/proc/meminfo` on Linux, never a cgroup's limit. On another system, the system's physical
  memory; a client that cannot obtain it treats that as a state error.
- **`headroom`**, the memory the kernel can hand out now without paging anonymous memory out. On
  macOS, the free, speculative, purgeable and file-backed pageable pages times the page size. On
  Linux, `MemAvailable` of `/proc/meminfo`, and inside a memory-limited cgroup v2 at most the room
  under the tightest limit, the least `memory.max` less `memory.current` of the client's cgroup and
  its ancestors. A headroom that cannot be read is **unknown**.

A client reads both at every admission attempt (section 12.1). The **cold start** need, for a label
nothing is known of, is `max(1, min(4096, memory / 4))` MiB.

### 5.2 Sizes

A size, in the configuration's `reserve` and `floor`, the needs file's `mem` and the CLI's
`--mem`, is a number of 1 to 6 ASCII decimal digits, optionally followed by `.` and 1 to 3 digits,
then optionally `G` (GiB, also meant without a suffix) or `M` (MiB). Its value is the number in
MiB, rounded up to a whole MiB (`"9.5G"` is 9 728 MiB, `"768M"` is 768 MiB, `"3"` is 3 072 MiB). In
a TOML file a size may also be an integer, which counts GiB. A size of 0 is valid where the key
allows it (`reserve`) and invalid where it does not (`floor`, a need). Any other value (a negative
number, `"1.5T"`, more than 6 digits or 3 decimals, a boolean, a float, a table) is invalid.

A need is at most `1073741824` MiB (1 PiB; section 7.3). Memory has no granularity: a need is
reserved as it is written.

### 5.3 Configuration file

The configuration is `<home>/.config/coreslot/config.toml` (`<home>` as in section 4.1). Under the
`CORESLOT_STATE_DIR` override it is `$S/config.toml` instead, so state and configuration switch
together: a client MUST NOT pair an overridden state directory with the user's real configuration.
Under the override, `config.toml` is a permitted top-level entry of `$S`, checked as in section 4.2
(regular file, owned by the user, not a symlink, not group- or world-writable). The real
configuration file lies outside `$S` and is not checked that way: it is read through symbolic links
(dotfile managers link it), with no owner or mode check. A path that does not exist, including a
dangling symbolic link, reads as a missing file (the defaults). A path that resolves to anything
but a regular file that can be read (a directory, a FIFO, a device) is an invalid configuration,
and a client detects that without blocking (it never waits on a FIFO).

The file is TOML 1.0:

```toml
[memory]
reserve = "1G"        # memory admission keeps free beyond every need (section 12.1)

[watch]
mode = "enforce"      # or "observe": everything but the stop (section 23.6)
sample_log = false    # true: log every machine sample (section 23.6)
yellow_points = 20    # the thresholds of section 23.4
red_points = 40
red_samples = 10
gate_pages_s = 1000
floor = "2G"          # a size
calm_s = 10           # the waits of sections 12.4 and 23.5, in seconds
post_kill_s = 15
spacing_s = 3

[platform]
windows_warning = false   # default true (section 16)
```

- `[memory]`: `reserve`, a size (section 5.2), 0 allowed.
- `[watch]`: `mode`, `"enforce"` or `"observe"`; `sample_log`, a boolean; `floor`, a size above 0;
  `yellow_points`, `red_points`, `red_samples` and `gate_pages_s`, numbers above 0; the waits
  `calm_s`, `post_kill_s` and `spacing_s`, numbers not below 0, where 0 waits not at all. A key that
  is absent takes the release's default. The defaults and
  the values of the examples are not part of the format: a minor release may change them
  (introduction).
- `[platform]`: `windows_warning`, a boolean.
- `memory`, `watch` and `platform`, when present, MUST be tables; any other value of one of them is
  invalid. Other tables and keys are ignored.

A missing file means the defaults, with no notice. A file that is not valid TOML or violates the
rules above is an invalid configuration: a state error (section 14). Clients read the file afresh
at every admission attempt.

### 5.4 Needs file

The needs file is `needs.toml` in the configuration's directory: `<home>/.config/coreslot/needs.toml`,
or `$S/needs.toml` under the override. It says what a labelled request needs, and which labels a
stop round must spare. No client is required to read it, and none of the protocol's decisions
depends on it; a client that reads it MUST NOT refuse a run because of it: a file or an entry that
cannot be read is a notice, and the request goes on without it. It is read through symbolic links,
as the real configuration is, and is not checked as in section 4.2, also under the override.

```toml
[needs."go test@proj"]
mem = "3G"          # a size (section 5.2); the need, which learning never replaces

[needs."gate:docker"]
no_restart = true   # a stop round never lists this label's runs (section 23.5)
```

Each key of `[needs]` is a label, matched exactly. An entry's `mem` is a size above 0, and
`no_restart` a boolean; a known key with another value makes the entry unreadable. A key the
reader does not know is ignored.

> **Implementation note.** The Go CLI takes a run's need from `--mem`, else the label's `mem`, else
> the 90th percentile of the `mem_peak_kib` of the label's last 20 counted runs (section 20) rounded
> up to a whole MiB, else, for a container command, 1 MiB, else the cold start (section 5.1). A
> **container command** is one whose argv[0] has the basename `docker`, `docker-compose` or
> `podman`; `sh -c 'docker run ...'` is not one. A run's `victim` (section 7.4) is true only when it
> was started with `--restart=yes` and its label has no `no_restart = true`, which wins over the
> flag; `--restart=no` makes it false. A container command counts as `no_restart` however its need
> was settled, a pin included: a stop would kill the client and leave its container running. The CLI
> requires `--restart` of every run that could be a victim (its command gets a process group of its
> own and it is not `no_restart`), and accepts it elsewhere, where `--restart=no` only keeps the
> run's `oom_score_adj` (section 17) and `--restart=yes` changes nothing.

## 6. Holds and descriptor ownership

A run's reservation is its hold (section 7.4): the memory a live hold's `need_mib` names is
reserved for as long as some open file description holds the hold's `flock(LOCK_EX)`. There is no
other lock that reserves memory.

Ownership is by open file description, and release is by **closing**: a client MUST NOT release a
ticket or hold with `LOCK_UN`, because `LOCK_UN` releases the lock for every descriptor sharing
that description, including a child's. A run passes its hold's descriptor to the command it starts
(section 17) and keeps its own copy until the command exits, so the hold stays live while the run
or any process that inherited the copy is alive. Processes that are not descendants (a container
started through a daemon) do not extend a hold.

Probing a ticket or hold is permitted inside and outside the section; a probe's momentary lock can
only make another client see a dead entry as live, never a live one as dead.

## 7. Names and bodies

### 7.1 Names

```
ticket    = order-key "-" pid "-" seq [ "-" display ]
hold      = start-ns  "-" pid "-" seq [ "-" display ]
cancel    = order-key "-" pid "-" seq          ; the target ticket's identity
ticket-id = order-key "-" pid "-" seq          ; a ticket name without its display
order-key = 20DIGIT            ; section 9
start-ns  = 20DIGIT            ; wall clock at admission, nanoseconds
pid       = 10DIGIT            ; the owner's process id
seq       = 6DIGIT             ; per process, wraps from 999999 to 000000
display   = 1*32( ALPHA / DIGIT / "." / "_" )   ; sanitized label; informational, never parsed
temporary = ".tmp-" pid "-" seq
```

All numbers in names are decimal and zero-padded to a fixed length, so the byte order of the
`order-key "-" pid "-" seq` prefix is the numeric order of (order-key, pid, seq). An order-key or
start-ns is at most 18 446 744 073 709 551 615 (2^64 − 1); a 20-digit value above it makes the
name invalid, as does any name that does not match the grammar. `display` contains
no `-`, so a name splits on `-` into three or four fields. A writer MAY include a display; how it
derives one from the label is not part of the protocol, and any derivation that matches the grammar
conforms (the examples below replace each byte outside the set with `_` and keep the first 32).
Readers MUST NOT interpret it.

The **name is authoritative** for identity: a ticket's or hold's owner is the pid in its name, its
order-key or start time is the one in its name, and a ticket's id is its first three fields.

`seq` keeps one process's names apart; how a process draws it is not part of the protocol. A
process MAY number every name and temporary from one counter or use several counters, and a
temporary's `seq` need not equal the `seq` of the name it is moved to. Uniqueness never rests on the
counter: a temporary is created with `O_EXCL`, a final name is taken without replacing an existing
file (section 11), and a writer whose temporary or final name exists takes another `seq` and
retries. Two live entries of one directory therefore never share a name. (A cancel request's name
is its target's id, not its writer's `seq`; section 11 says what an existing one means.)

### 7.2 Rules for every body

- A body is one UTF-8 JSON object, at most 8 KiB (8 192 bytes), optionally surrounded by JSON
  whitespace. A writer MUST write every key of its table that the table does not mark optional; an
  absent optional key reads as `false` or `0`.
- A body is **malformed** when it is not such an object, exceeds 8 KiB, lacks a required key,
  has a key of the wrong type or outside its rule, or has a duplicate key at any depth. Keys are
  compared after JSON unescaping: `"label"` duplicates `"label"`, so a reader that compares
  raw bytes is not conforming.
- Nesting depth alone never makes a body malformed: a reader MUST accept any depth that fits in
  8 KiB (about 4 096 levels).
- In particular, a body is malformed when a string anywhere in it contains an unpaired UTF-16
  surrogate escape (a `\uD800`-`\uDBFF` escape
  not followed by a `\uDC00`-`\uDFFF` escape, or a `\uDC00`-`\uDFFF` escape not preceded by one).
- Unknown keys are ignored at any depth: bodies only gain optional keys within a format. The value
  of an unknown key may be any JSON value; the integer rule of section 1 applies only to the keys of
  a table, so `1.5`, `1e999` or a 30-digit number under an unknown key is accepted. (The duplicate,
  surrogate and size rules still apply inside unknown keys.)
- A malformed body makes its entry ignored and counted, never trusted. An ignored ticket takes no
  place in the queue. A live hold whose body is malformed is not ignored: admission, the watchdog
  and `status` alike count it as reserving the cold start need (section 12.1), since unknown is not
  zero, under the pid and start its name carries and with no label. It is no running hold (section
  23.3).

### 7.3 Ticket

`$S/queue/<ticket>`, lock-carrying: its owner holds `flock(LOCK_EX)` on it for as long as it waits.
Body:

| Key | Type | Rule |
|---|---|---|
| `label` | string | at most 128 bytes in UTF-8; may be empty |
| `need_mib` | integer | `1..1073741824` (1 PiB); the memory the run needs, in MiB |
| `created_ns` | integer | `≥ 0`; the run's arrival: the wall clock when the ticket was published, a restarted run's first arrival (section 24) |
| `restart` | boolean | optional; `true` for a stopped run coming back (section 24), which waits under the restart conditions of section 12.4 |

### 7.4 Hold

`$S/holds/<hold>`, lock-carrying: the holder holds `flock(LOCK_EX)` on it for as long as it holds
its reservation, and passes that descriptor to its command as fd 3 (section 17). Body:

| Key | Type | Rule |
|---|---|---|
| `label` | string | as in the ticket |
| `pid` | integer | `≥ 0`; equal to the name's pid |
| `start_ns` | integer | `≥ 0`; equal to the name's start-ns |
| `arrival_ns` | integer | `≥ 0`; when the run first arrived in the queue, also after a restart (section 24) |
| `need_mib` | integer | `0..1073741824`; what the hold reserves, in MiB: the need asked; `0` for a run outside the budget (section 14), which reserves nothing |
| `typical_s` | integer | optional; `≥ 0`; the run's typical run time in seconds (section 10.3), as it learned it when it started; `0` when it has none |
| `stops` | integer | `≥ 0`; how often the run was stopped for memory before this attempt |
| `footprint_mib` | integer | `0..1073741824`; the run's footprint as it last measured it; `0` when not measured |
| `d_user_ns`, `d_sys_ns`, `d_page_wait_ns` | integer | `≥ 0`; the time its process tree spent in user mode, in system mode and waiting for pages, in the run's latest tick period; `d_page_wait_ns` is `0` where the kernel reports no page wait (section 23.1) |
| `normal_share` | number | `0..1`; the label's normal share `S0` (section 23.3) as the run read it from the run log when it started; `0` when `learned` is false |
| `sampled_ns` | integer | `≥ 0`; when the run last measured, `0` before it has |
| `victim` | boolean | `true` when a stop round may list the hold: its run declared its command safe to stop and run again, its label has no `no_restart` (section 5.4), its command does not share coreslot's process group, and it is not container work (section 23.5) |
| `learned` | boolean | `true` when the run log gave the label a normal share when the run started; otherwise the hold is learning (section 23.3) |
| `child_exited` | boolean | `true` once the run's command has exited |
| `cpu_ns` | integer | optional; `≥ 0`; the user and system time its process tree spent in this attempt so far: the sum of its ticks' `d_user_ns` and `d_sys_ns` (section 23.1) |
| `typical_cpu_s` | number | optional; `≥ 0`; the label's typical CPU seconds: the median of `user_s + sys_s` (section 20) over its last 20 counted runs that report both, when at least 10 do, as the run read it when it started; `0` otherwise |
| `stall_ewma` | number | optional; `0..1`; its share `S` (section 23.3) smoothed over its ticks with a time constant of 10 s: the first tick's `S`, then `S̃ ← S̃ + (1 − e^(−p / 10 s)) (S − S̃)` for a tick period `p` |
| `container` | boolean | optional; `true` once the run is container work (section 23.5); absent means `false`, and a writer omits it then |

A body whose `pid` or `start_ns` differs from its name's is malformed. Comparing them needs the
name, so it is done by the reader of `holds/`, not by a body decoder. A start-ns in a name may be
as large as 2^64 − 1 (section 7.1) while `start_ns` is a signed 64-bit integer: a hold whose name
carries a start-ns above 2^63 − 1 never has a well-formed body and is ignored and counted. No
writer produces one, because the wall clock is a signed 64-bit value.

A hold is published with `stops`, `footprint_mib`, the three deltas, `normal_share` and
`sampled_ns` at 0 and `victim`, `learned` and `child_exited` false. Once its command has started
the run writes `stops`, `victim`, `normal_share`, `learned`, `typical_cpu_s` and a container
command's `container`, which hold for the attempt, except that finding a client in its tree clears
`victim` and sets `container` (section 23.5). Each tick (section 23.1) writes the measured keys,
`cpu_ns` and `stall_ewma` among them. Only
the holder writes its body, at publication and then in place: one write at offset 0
of the whole body, padded with spaces to the length it replaces, so that the file and its lock keep
their inode and the file never shrinks (I1). It keeps `label`, `pid` and
`start_ns` as they were. A reader MAY still see a body that does not parse; it then treats the
live hold as section 7.2 says.

### 7.5 Cancel request

`$S/cancel/<cancel>`, not lock-carrying; its name is the target ticket's id. Body:

| Key | Type | Rule |
|---|---|---|
| `requested_ns` | integer | `≥ 0`; wall clock at publication |
| `requester_pid` | integer | `≥ 0`; the canceller's pid; informational |

### 7.6 FORMAT

`$S/FORMAT` holds the line `coreslot-protocol <n>` where `<n>` is 1 to 9 ASCII decimal digits, read
as a decimal number; leading zeros are allowed (`02` is format 2) and writers write none. Writers
write the line followed by one LF. Readers MUST accept the line with or without the final LF. Any
other content (a sign, more than 9 digits, a CR or other whitespace, a second line) is a malformed
`FORMAT`: a state error. A well-formed `<n>` other than 2, including 0, is a format this document
does not define (section 15.2). Section 15 defines what a client does with it.

### 7.7 admit.lock

`$S/admit.lock` is the admission lock (section 8). It is created once (`O_CREAT|O_NOFOLLOW`, mode
`0600`) and never deleted, renamed or replaced. Its body is informational: on entering the section
the holder SHOULD rewrite it in place through its locked descriptor (truncate, then write) as the
JSON object `{"pid": <integer>, "entered_ns": <integer>}`. Readers MUST NOT rely on it for anything
but display, and MUST tolerate an empty, stale or malformed body.

### 7.8 gen

`$S/gen` is the JSON object `{"gen": <integer>, "reconciled_ns": <integer>}` (body rules of
section 7.2). A missing or malformed `gen` reads as `{"gen": 0, "reconciled_ns": 0}`. It is written
only inside the section and replaced with a plain `rename` (section 11). `gen` is an integer in
`[0, 2^63 − 1]`; a negative `gen` makes the file malformed. **Bumping** `gen` means writing it with
`gen` increased by one, or 0 when it was 2^63 − 1, and `reconciled_ns` unchanged.

A section bumps `gen` once when it publishes or removes a ticket or hold: enqueue (section 10.1),
admission (12.1), publication of an unbudgeted hold (14), and removal of the
client's own ticket after cancellation or withdrawal (13) or of its own hold (12.3); the resume bump
of section 10.2 is the one bump without such a change, and reconciliation increases `gen` when it
removed anything (10.4). Publishing a cancel request does not bump `gen` (the owner polls `cancel/`
itself), nor does writing `FORMAT`, `degraded`, `watch.json`, `admit.lock` or the run log, nor the
holder's rewrite of its own hold's body. A crash can separate a removal from its bump, so a reader
never relies on a bump to learn that an entry is gone (section 10.3).

### 7.9 degraded

`$S/degraded` exists while degraded mode is recorded. Its body is the JSON object
`{"reason": <string>, "first_ns": <integer>, "count": <integer>}`: the most recent fail-open reason,
the wall clock of the first fail-open since the record was created, and the number of fail-open
runs recorded since. It is written inside the section and replaced with a plain `rename`.
Degraded mode is recorded exactly while the file exists, whatever its body; a reader that finds it
malformed still reports degraded mode. A client recording a fail-open over a malformed record
starts a new record: `first_ns` is now and `count` is 1.

### 7.10 log/ (run log)

`$S/log/runs.jsonl` holds one JSON object per line (section 20). Readers MUST skip lines they cannot
parse.

### 7.11 watch.json

`$S/watch.json` is the watchdog's shared state (section 23): the last machine sample, the detector
that has advanced on it, and the last stop list. It is written only inside the section and replaced
with a plain `rename` (section 11), without a `gen` bump. It is one JSON object of at most 1 MiB,
but it is not a strict body: a missing, unreadable, oversized or malformed file reads as the empty
state (no sample, level green, no stop list), and a key a reader does not know is ignored. Every
client that does not run the detector needs only these keys:

| Key | Type | Content |
|---|---|---|
| `sampled_ns` | integer | the wall clock of the client that took the sample, when it did; `0` with no sample |
| `sample.host.wall_ns` | integer | the sample's host clock, on a clock that counts the time the machine slept (section 23.2) |
| `detector.level` | integer | `0` green, `1` unknown, `2` yellow, `3` red, `4` floor (section 23.4) |
| `detector.yellow_cleared_ns` | integer | the host clock when the level last fell from yellow, red or floor to green; `0` before |
| `detector.calm_since_ns` | integer | the host clock since which the level has been green without a break; `0` while it is not green |
| `detector.last_admit_ns` | integer | the host clock of the last admission recorded for spacing (section 12.4); `0` before |
| `detector.last_round_ns` | integer | the host clock of the last stop round (section 23.5); `0` before |
| `detector.stop` | array of strings | the hold names the last stop round listed |
| `detector.observed` | boolean | the last stop round was written in observe mode (section 23.6) |
| `detector.round_floor` | boolean | the last stop round was a floor round, not a red one (section 23.5) |
| `detector.kept` | object | optional; by hold name, the holds a red round kept as nearly finished (section 23.5), each `{"deadline_ns": <integer>, "left_s": <number>, "done": <number>, "round_ns": <integer>}`: the host clock by which its first keep predicted it to end, its predicted remaining seconds and its progress at the round that last kept it, and that round's `last_round_ns` |

The other keys under `sample` and `detector` are the detector's own state (the readings, the
window, the rates, the excess, the leaky bucket, the yellow latch). A detector
that finds them missing starts afresh; a client that does not run the detector ignores them.
Times named `…_ns` under `detector` are on the host clock of the sample, not on the wall clock: a
reader brings one to the present by adding the sample's age, `host_now = sample.host.wall_ns + (now
− sampled_ns)`.

## 8. The admission critical section

`admit.lock` is the protocol's linearization point. Enqueue, admission, cancellation publication,
reconciliation and run-log rotation MUST happen while holding
`flock(LOCK_EX)` on it; so does every other publication (section 11), every write of `watch.json`
and every removal of a ticket, hold, cancel request or temporary file. A refresh (section 10.2), a
probe, the rewrite of a hold's own body and a run-log append happen outside it.

- **Entry is non-blocking.** A client tries `LOCK_EX|LOCK_NB` and retries with backoff from 10 ms
  up to 250 ms, checking its own deadline or cancellation between tries; it MUST NOT sleep inside
  `flock`. A single-attempt operation (a no-wait try) makes one try. A client that cannot enter
  by its deadline gives up without having changed anything.
- **The section never waits** for anything but the filesystem: no sleeping for memory, no lock
  other than non-blocking ones, bounded work. Reading the machine's memory and the host readings
  (section 23.2) is bounded work: a few sysctls or `/proc` reads, no subprocess.
- A client SHOULD prevent its own job-control stop while it holds any lock it took only
  momentarily: while inside the section, and while it holds the probe lock of a ticket or hold
  (section 6), inside the section or outside it. A process stopped holding a probe lock makes
  every other client count that ticket or hold as live until it continues, so a head whose need
  that hold's reservation keeps out is not admitted although nobody is stopped inside the section.
  The locks a client owns (its ticket while it waits, its hold while its reservation lives) are not
  momentary: a stopped holder keeps only its own reservation. The CLI
  ignores `SIGTSTP`, `SIGTTIN` and `SIGTTOU` from each try to take such a lock until it has
  closed the descriptor that took it, but not while it sleeps between tries to enter the section;
  a client never installs signal handlers on behalf of its caller. `SIGSTOP` cannot be ignored: a
  process stopped inside the section blocks every admission on the machine until it continues or
  dies. This is the one exception to I5. The CLI's stop is deferred, not lost: in non-terminal mode
  without `run --same-group` it forwards `SIGTSTP` to its command's process group and then stops
  itself, once it holds no momentary lock, and forwards `SIGCONT` to that group when it is
  continued.
- On every entry, a client reads `FORMAT` before changing anything and acts on it as section 15
  says; when `FORMAT` is missing it writes `coreslot-protocol 2` (section 11).
- A crash inside the section releases the lock; every step is ordered so that a crash leaves either
  the old state or the new state plus dead files that reconciliation removes.
- Leaving the section is closing the `admit.lock` descriptor.

## 9. Order

The queue orders by arrival alone:

```
order-key = max(0, arrival_ns)      (ascending; ties by pid, then seq)
```

`arrival_ns` is the wall clock read inside the enqueue section, except for a run that comes back
after a stop: its `arrival_ns` is its first arrival, and so it keeps its place (section 24). A
ticket is **ahead** of another when its (order-key, pid, seq) is smaller. A wall-clock step
misorders the tickets enqueued across it by at most the size of the step and affects nothing else.

## 10. Liveness, waiting and reconciliation

### 10.1 Enqueue

Inside the section the client reads `arrival_ns` (a restarted run takes its first, section 24),
computes the order key, publishes its ticket (section 11) keeping the ticket's lock-carrying
descriptor open, bumps `gen`, and leaves. From then on, until admission, cancellation or
withdrawal, it MUST keep that descriptor open and refreshed.

### 10.2 Refresh and STALLED

**Freshness is observed, not computed.** A waiter MUST refresh its own ticket's mtime every 2 s, by
setting the mtime of its ticket to the current time through its descriptor (`futimens(fd, NULL)` or
equivalent); a refresh is done outside the section and writes no content. An observer MUST NOT
compare an mtime with a clock: for each ticket it watches, it remembers the last mtime value it saw
and the time, on its **own** monotonic clock, at which that value last changed (or at which it
first saw the ticket). A ticket is **STALLED**, for that observer, when no change has been observed
for 10 s. A refresh that is observed makes the ticket WAITING again. Only time the observer spent
watching the ticket counts: when an election leaves a ticket unexamined (section 10.3), the time
until the observer next examines it is not counted towards the 10 s, though a value that changed
meanwhile still counts as a change. Nor is the observer's own absence counted: when more than 3 s
pass between its last examination and its next election (it was stopped, suspended or starved),
that interval is not counted towards the 10 s for any ticket it observes. The 3 s are 1.5 times
the longest a conforming waiter leaves between elections, 2 s, the interval of its refreshes, so
that a waiter electing at that interval, with scheduling jitter on top, still watches. The head's
maximum backoff, 0.5 s (section 10.3), is shorter and stays inside it.

When a waiter's own refresh gap exceeded 10 s on either its monotonic or its wall clock (it was
suspended), it bumps `gen` in the section after refreshing. The bump never delays a refresh: a
waiter that cannot enter the section at once retries the bump at its next refresh.

### 10.3 Waiting and election

A waiter:

- reads `gen` once per second; when its `gen` field changed, rescans `queue/` (a change of
  `reconciled_ns` alone MAY also trigger a rescan, which does no harm);
- each second, examines the tickets ahead of it, from the nearest: a dead one (its probe succeeds)
  is skipped; a gone one (it no longer exists: its probe fails with `ENOENT`) is skipped; a STALLED
  one is skipped; an ignored (malformed, non-matching or stray) entry is skipped; the
  first live, non-stalled ticket ends the examination. When none is found the waiter is the head:
  this examination is the **election**, and the waiter enters the section and tries admission
  (section 12);
- in each election also watches the **front window**: the first 4 tickets of the queue that are
  not gone (fewer when fewer are ahead of it), reading their mtimes as an observer (section 10.2)
  even when the examination ended before reaching them. Watching the front window keeps every
  waiter's timers on the front of the queue running; the election's rule is unchanged. A dead
  ticket that is not yet removed still takes a slot of the window until reconciliation removes it
  (section 10.4);
- checks `cancel/` for its own ticket id on every poll (section 13.1);
- as head, retries admission with backoff from 0.25 s up to 0.5 s, repeating the election (with a
  rescan when `gen` changed) before each attempt;
- behind a live ticket, when its run has a typical run time or its request is a container request
  (section 12.1), makes a single admission attempt per election to pass the head (below).

**Passing.** A run's **typical run time** `d`, in whole seconds, is learned from its label's
counted runs (section 20) when it starts, and its hold carries it as `typical_s`; a run of a label
without a counted run has none and never passes. In the section, the **head** is the first live
ticket of the re-election (section 12.1 step 5). A waiter is admitted ahead of the head when:

1. its own request fits and the level gate lets it in, as for a head (section 12.1 steps 4 and 6),
   and the headroom is known;
2. it has a typical run time `d`;
3. `d ≤ W`, the head's **expected wait**: the smallest `t` at which the room plus the `need_mib` of
   the live holds expected to end within `t` seconds covers the head's need, where a live hold is
   expected to end `max(0, typical_s − (now − start_ns) / 10^9)` seconds from now. A hold with
   `typical_s` 0 is never expected to end: when the others do not cover the head's need, `W` is
   unknown and nobody passes. When the head's ticket has `restart`, `W` is at least the calm it
   still needs (section 12.4): `calm_s`, less `host_now − calm_since_ns` while the stored sample is
   fresh and green. With no live hold the head starts by the idle rule (section 12.4), so its
   expected wait is 0 and nobody passes, a container request included (section 12.1).

Each waiter between the head and a passer is judged by the same rule on its own, so one that may not
pass keeps nobody behind it from passing. A passer only starts when it is expected to end before the
head could start, so the head, a restart included (section 24), keeps its place. A passer that runs
longer than `d` is never stopped for it. A waiter without a typical run time never enters the section
to be admitted while a live ticket is ahead of it, unless it is a container request, which while a
hold is live needs no pass (section 12.1) and leaves the head its place.

> **Implementation note.** The Go CLI's typical run time is the 90th percentile of the `ran_s` of
> the window its need is learned from (section 5.4), rounded up to a whole second and at least 1.

Only a non-conforming actor removes a live ticket (reconciliation removes dead ones only). A waiter
whose own ticket's name disappears while it still holds the ticket's descriptor keeps waiting. It
loses its place, though. No other client sees the ticket any more, so waiters behind it may be
elected and admitted first, `status` does not show it, and it cannot be canceled by label; a cancel
request naming its exact id may still reach it, but only from a canceller that publishes without
checking `queue/` first (section 13.1), and only until a reconciliation removes the request, whose
ticket name is gone (section 10.4, step 2). When it later removes its ticket (admission,
cancellation, withdrawal), `ENOENT` counts as success.

A waiting CLI prints a line when it begins to wait and then every 30 s: why it waits, its position
and the live holds with their needs and ages (informative). The position is one plus the tickets
ahead that the examination has not passed over, an upper bound: tickets beyond the one that ended
the examination are counted without being examined.

### 10.4 Reconciliation

Reconciliation removes dead files. It runs inside the section at most once per 30 s machine-wide: a
client that enters the section for any purpose other than admission
(enqueue, cancellation publication, removal of its own ticket or hold, a resume bump,
publication of an unbudgeted hold, a `degraded` write or removal, log rotation) and finds
`reconciled_ns` in `gen` older than 30 s on its wall clock (or in the future by more than 30 s) MUST
reconcile before its own change. Writing a missing `FORMAT` (section 8) is part of whichever entry
finds it missing and does not by itself make that entry reconcile. Admission never reconciles, and
neither does a reset (section 4.4), and neither does a watchdog tick (section 23.1). Reconciliation:

1. probes every ticket in `queue/` and every hold in `holds/`; for each probe that succeeds, unlinks
   the file **while still holding the probe lock**, then closes it (an entry whose name matches but
   that is not a regular file of the user, section 4.2, is ignored and counted, never probed or
   removed, and reconciliation goes on and writes `gen` as usual);
2. removes every cancel request in `cancel/` whose ticket is gone: its ticket is any `queue/` entry
   that is not ignored as a stray (section 4.2) whose name matches the ticket grammar and whose
   first three fields are that id, whatever its body. An entry ignored as a stray (a wrong type,
   owner or mode) is not an entry for this purpose: it never protects a cancel request;
3. removes every temporary file (`.tmp-*`) in `$S`, `queue/`, `holds/` and `cancel/` whose mtime is
   older than 30 s;
4. writes `gen` with `reconciled_ns` set to the current wall clock, and with `gen` increased by one
   if it removed anything.

Entries that do not match their grammar are not removed. The cost is O(entries) per 30 s for the
whole machine.

## 11. Atomic publication

Inside the section, a publication of a ticket, hold, cancel request, `gen`, `FORMAT`, `degraded`
or `watch.json`:

1. creates `<dir>/.tmp-<pid>-<seq>` with `O_CREAT|O_EXCL|O_NOFOLLOW|O_CLOEXEC`, mode `0600`, in the
   target's directory; on `EEXIST` (a young leftover of an earlier process with the same pid) the
   writer takes its next `seq` and retries;
2. takes `flock(LOCK_EX)` on it if the file is lock-carrying (ticket, hold);
3. writes the body;
4. moves it to its final name:
   - ticket, hold, cancel request: without replacing an existing file: `renameat2(RENAME_NOREPLACE)`
     on Linux, `renamex_np(RENAME_EXCL)` on macOS, and where those are unavailable (the call or its
     library wrapper is missing, or it fails with `ENOSYS`) or unsupported by the filesystem (it
     fails with `EINVAL`, `ENOTSUP` or `EOPNOTSUPP`), `link(2)` to the final name followed by
     `unlink(2)` of the temporary name. Any other error fails the publication. On `EEXIST` a
     ticket or hold writer removes its temporary (closing its descriptor, which nothing else
     shares yet) and starts again at step 1, in the same section, with a new temporary and a final
     name carrying its next `seq`. A cancel request is
     not retried: its name is its target's id, so an existing one means that ticket's cancellation
     is already requested; the canceller removes its temporary and reports the cancellation as
     already requested, which is not an error (section 13.1);
   - `gen`, `FORMAT`, `degraded`, `watch.json`: with a plain `rename`.

The writer keeps its original descriptor: writing the body never closes the only lock-carrying
descriptor. Temporary files are created only inside the section. A writer whose publication fails
(writing the body fails, the move fails, a final name is taken) removes its own temporary before it
leaves the section. A temporary left behind belongs to a writer that died inside the section and is
removed by reconciliation; no client removes another writer's temporary in any other way. After a
`link(2)` fallback whose `unlink(2)` failed, the temporary name is a second link to the published
entry; the entry stays published, and reconciliation removes the extra name once its mtime is
older than 30 s (section 10.4).

## 12. Admission

### 12.1 Steps

The room is

```
room = headroom − reserve − Σ over live holds of max(0, need − footprint)
```

where `headroom` and `reserve` are the readings of section 5.1 and the configuration (section 5.3),
and a **live hold** is a well-formed hold of `holds/` whose probe fails because some process holds
it. A live hold contributes its `need_mib` less its `footprint_mib`, and nothing when the footprint
is the larger or `need_mib` is 0 (a run outside the budget). A live hold whose body does not
parse contributes the cold start need (section 5.1) in full. Stray and gone holds are left out.
The room may be negative. A request **fits** when its need is at most the room.

Inside the section the head, a waiter that may pass it (section 10.3) or a container request
(below):

1. checks `cancel/` for its own ticket id; if present, it is canceled (section 13.1);
2. reads the configuration and the machine's memory (section 5.1); an invalid configuration or an
   unknown `memory` is a state error (section 14);
3. lists the live holds and computes the room;
4. applies the **idle rule**: when no hold is live, the head starts whatever the numbers say; idle
   means "no coreslot job running", never machine load. When a hold is live and the headroom is
   known, the head leaves the section and waits unless its request fits. When the headroom is
   unknown nothing gates the head;
5. repeats the election (section 10.3) over a fresh listing of `queue/`, the **re-election**: every
   well-formed ticket ahead of it in order is probed, and if any is neither dead nor STALLED for
   this observer (its mtime from the listing counts as an observation), the head leaves the section
   and waits, unless it may pass the first of them (section 10.3). A ticket the head has never
   observed, such as one published after its election, counts as live;
6. while a hold is live, applies the level gate (section 12.4) and leaves and waits if it holds the
   request back;
7. publishes its hold (section 7.4), closes its ticket descriptor, unlinks its ticket, bumps `gen`,
   and leaves the section.

A **container request** waits only on real memory while a hold is live. Its room then leaves out the
live holds, so it is `headroom − reserve`, and step 5 does not apply: it needs no pass and starts
ahead of any live ticket, at most once per election (section 10.3). With no hold live it is an
ordinary ticket: step 5 applies, so the head goes first by the idle rule. Steps 4 and 6 apply as to
any request, so while a hold is live it waits while the level gate holds it and while the headroom
is below the reserve plus its need. Only its own process knows a request is one: nothing marks its ticket or its hold, and the
format is unchanged.

> **Implementation note.** The Go CLI makes a container request of a container command (section 5.4)
> with neither `--mem` nor a `mem` in the needs file: a person who pins a need chose to reserve it
> and to wait for it.

### 12.2 Oversized request

A need above `memory` less `reserve` fits no room, since the room never exceeds the headroom less
the reserve. Such a request starts only under the idle rule, when no hold is live, a start no
container request behind it takes (section 12.1), and runs alone: its remaining growth, the need
less its footprint, keeps the room of every later ordinary request negative.
The hold's `need_mib` is the need asked, and so is `CORESLOT_GRANTED_MEM`. A CLI run says so once,
before it queues.

### 12.3 After the command exits

At release, before it closes anything, a holder MUST check the identity of its hold, comparing
`fstat` of its descriptor with its path. A **mismatch** is any of: a held file with
`st_nlink == 0`; a path whose `(st_dev, st_ino)` is not the held file's; a hold path that is
missing or cannot be stat'ed. A mismatch is a state error (section 14): the hold's path no longer
names the file it holds, because the directory, or an entry in it, was removed or replaced. On a
mismatch the holder MUST close its descriptor of the hold, and MUST NOT write anything into the
state directory as it now exists:

- it MUST NOT remove its hold by path;
- it MUST NOT enter the admission section: entering may write `FORMAT` (section 15.1) into a
  directory this run never read (section 14);
- it MUST NOT write a `degraded` record;
- it MUST NOT append a run-log line (section 20).

It reports the state error to its caller on stderr; the CLI still exits with the command's own
status (section 19). Reporting degraded mode at release therefore means telling the user, not
writing a `degraded` record.

Without a mismatch, the run closes its copy of the hold's descriptor. The CLI then enters the
section if it can within 2 s, probes its hold and unlinks it only if the probe succeeds (no
descendant still holds it), bumping `gen` when it did (section 7.8); otherwise reconciliation
removes the hold later. Other clients MAY remove their hold the same way, or leave it to
reconciliation. This applies to every hold a run published, including a fail-open run's unbudgeted
hold (section 14), and to the hold of a stopped attempt (section 24).

### 12.4 The level gate

The gate reads `watch.json` (section 7.11) inside the section, and only while a hold is live. The
stored sample is **fresh** when `0 < sampled_ns ≤ now` and `now − sampled_ns ≤ 6 s` on the wall
clock, three tick periods; a stale or absent sample is no reading. With `host_now` as section 7.11
defines it:

- not fresh: the gate holds nothing back, except from a restarted run (below);
- fresh and `detector.level` is yellow, red or floor: nothing new starts. Unknown does not hold a
  request back;
- unknown: the gate holds nothing back;
- otherwise (green): for `calm_s` after `yellow_cleared_ns`, at most one request starts per
  `spacing_s`: an admission is recorded in `detector.last_admit_ns`, and no further request starts
  until `spacing_s` has passed since the recorded one. Spacing applies only while the stored
  sample's `wall_ns` is not 0 and neither `yellow_cleared_ns` nor `last_admit_ns` is later than
  `host_now`: a host clock that restarted (a reboot keeps `watch.json`) spaces nothing.

A **restarted** run (section 24) that finds a hold live first takes its part in the tick (section
23.1), as a running command's run does, so that calm is measured when no run ticks. It then needs a
fresh sample, the level green, the level calm for `calm_s` (`host_now − calm_since_ns ≥ calm_s`),
and, when the headroom is known, a room of at least its need plus `max(256 MiB, need / 10)`. With
no hold live, the idle rule starts it like any other.

## 13. Cancellation and withdrawal

### 13.1 Cancellation

Canceling never signals across processes. A canceller publishes `$S/cancel/<ticket-id>` inside the
section. It MAY first check, in that section, that `queue/` holds a ticket with that id and publish
nothing when it does not; a request whose ticket is gone is removed by reconciliation either way
(section 10.4, step 2). The owner checks `cancel/` for its id on every poll and again at admission
step 1, so a cancel published before the owner's admitting section wins; one published later finds
no ticket and has no effect: cancellation never revokes a hold, and never stops a command. A
canceller that finds the request already published (section 11) treats the cancellation as already
requested; that is not an error. On finding it, the owner closes its ticket descriptor (the ticket
is dead at once), removes the ticket and the cancel request in its next section if it can enter one
(bumping `gen`, section 7.8), and stops: the CLI exits with the refusal code.

> **Implementation note.** The Go CLI checks `queue/` first and refuses, with a diagnostic that no
> such ticket is waiting, when there is none.

### 13.2 Withdrawal

A waiter whose own deadline passes, whose wait is abandoned by its caller, or that is interrupted
(`Ctrl-C` on a waiting CLI, which then exits 130) withdraws by closing its ticket descriptor: the
ticket is dead at once, and withdrawal never depends on entering the section. The waiter MAY then
remove its ticket in a section, bumping `gen` (section 7.8); otherwise reconciliation removes it.

## 14. Errors, fail-open and degraded mode

| Class | Examples | Outcome |
|---|---|---|
| usage | a need of 0 or above 1 PiB, a size that is not one, a label above 128 bytes, a flag the command does not know, no command | exit 64; nothing runs |
| refusal | a no-wait or fail-on-timeout request expiring, a stopped `--no-wait` run, cancellation, strict mode turning any of the next row into a refusal, giving up after 3 stops (section 24) | the refusal code (section 19); nothing runs, or the run ends |
| state error | unusable directory, refused filesystem, unexpected file types or owners, directory full, malformed `FORMAT`, invalid configuration, an inode mismatch at a held hold (I1) | fail-open unless strict |
| internal | the command cannot be started | exit 70, or 126 / 127 |

An unknown format (section 15.2) is deliberately **not** a state error.

**Fail-open** runs the command at once, outside the budget, and:

- prints a diagnostic to stderr immediately, always;
- publishes a hold with `"need_mib": 0` when the directory is usable and its format understood, so
  `status` shows the run, and bumps `gen` in that section (section 7.8);
- records the reason in the run log and in `degraded` when those are writable, and says on stderr
  when they are not;
- exports `CORESLOT_UNBUDGETED=<reason>` to the command, so nested runs pass through (section 18);
- MUST NOT write anything (hold, run log, `degraded`) into a directory whose `FORMAT` it has not
  read and understood during the run; a malformed `FORMAT` is not understood.

The reason, in `CORESLOT_UNBUDGETED` and in `degraded`'s `reason`, is informational text that no
client interprets: section 18 acts on the variable being set, never on its value. Any non-empty
text conforms.

A state error found at release (section 12.3) is not a fail-open: the command has already run with
its reservation. It is reported and nothing is written; the rules above do not apply to it.

A run whose wait timed out and that runs anyway (the CLI's `--on-timeout run`) withdraws its ticket
(section 13.2) and runs outside the budget. It is not a state error: it publishes the same
unbudgeted hold, exports `CORESLOT_UNBUDGETED=<reason>` and records the reason in its run-log
line's `fail_open`, but it writes no `degraded` record. The same `FORMAT` rule applies.

> **Implementation note.** The Go CLI's reason for such a run is `timeout`.

`status` reports degraded mode and exits 3 until `coreslot doctor --clear` removes the record. It
also exits 3 while a state error makes runs fail open (section 19).

## 15. Formats and an unimplemented format

### 15.1 What every format keeps

`FORMAT` holds `coreslot-protocol <n>`; `n` versions the metadata (names, bodies, and every part of
the layout other than `admit.lock`). Every format keeps `admit.lock`, an exclusive `flock` held
only briefly by whoever changes admission state.

A client that finds no `FORMAT` writes `coreslot-protocol 2` inside the section. Writing a newer
`FORMAT` is the business of the release that introduces it.

### 15.2 A format this client does not implement

A state directory whose format is not 2 is not used. A client that finds another format leaves
the state directory as it found it and takes no part in the budget: it publishes nothing and
writes nothing (no ticket, hold, log, degraded record, `gen` bump, `watch.json` or `admit.lock`
body), and its run fails open (section 14) with one notice that names the format and `coreslot doctor
--reset`; under `--strict` it refuses with that notice. The decision is made at
every section entry from the `FORMAT` read there (section 8).

A waiting client reads `FORMAT` at every section entry; one that finds the format changed under
it closes its ticket's descriptor, so the ticket is dead at once, and fails open. It does not
remove the file: that would be a write into a format it does not implement.

Every other operation that would write follows the same rule, except a reset (section 4.4): under
another format a client publishes no cancel request or unbudgeted hold, records and clears no
`degraded`, and does not remove its own hold after a run. A fail-open run still
runs its command but writes nothing. `coreslot doctor --reset` recreates the directory as format 2
when nothing of either format is live, the other format's run log removed with the rest.

> **Implementation note.** The Go CLI refuses cancellation and `doctor --clear` under another
> format, with the same notice.

## 16. Windows

The CLI runs the command without a budget and prints one warning per run; `[platform]
windows_warning = false` silences it; strict mode refuses instead. `status`, `cancel` and `doctor`
exit 70 on Windows (there is no budget to inspect, cancel or repair), and their `--help` exits 0.
There is no watchdog on Windows.

## 17. Descriptor and environment contract

A run that starts a command with a hold gives it fd 3, the hold's lock-carrying descriptor;
otherwise fd 3 is closed in the child (a Go launcher passes a nil `ExtraFiles[0]`). An inherited
descriptor at 3 is not passed on, so a launcher that needs its own descriptors in the command
places them above. It sets this environment:

| Variable | Value |
|---|---|
| `CORESLOT_GRANTED_MEM` | the memory the run reserves, in MiB: its need, also for a run outside the budget |
| `CORESLOT_HOLD` | the hold's name, when the run passes a hold descriptor, which is always fd 3 |
| `CORESLOT_UNBUDGETED` | the reason, for a fail-open or unbudgeted pass-through run |

Every descriptor the run itself opens under `$S` outside this contract stays close-on-exec (section
4.2). A run that starts its command again after a stop (section 24) builds the contract from the
hold of the new attempt: `CORESLOT_HOLD` names that hold.

A run that is not a pass-through (section 18 rules 1 and 2) builds this contract from its own hold
only. An ordinary or `--independent` request, a fail-open run removes every inherited
`CORESLOT_GRANTED_*`, `CORESLOT_HOLD` and `CORESLOT_UNBUDGETED` from the command's environment
before setting their own, and closes (does not pass on) exactly the inherited descriptor 3 when
`CORESLOT_HOLD` is set, so that a grandchild does not keep a hold that is not its parent's alive.
A descriptor 3 that is not a regular file cannot be a hold's (its number was reused after a
launcher closed the original), and a client SHOULD leave it open. Every other inherited descriptor (a jobserver pipe, `LISTEN_FDS` sockets, the
user's redirections) is passed through untouched, and `CORESLOT_STATE_DIR` is not hold context and
is passed on.

> **Implementation note.** The Go CLI closes the named descriptor only when it is a regular file at
> fd 3 or above.

On Linux a run SHOULD raise its own `oom_score_adj` to 500, never lowering a higher one, before it
starts the command, which inherits it, so that the kernel's out-of-memory kill takes a coreslot
job rather than the desktop (systemd-oomd does not read the score: it picks a cgroup by its memory
pressure or swap use). A run whose label has `no_restart` or whose command is a container command
(section 5.4), or that its caller declared not to be run again (the CLI's `--restart=no`), leaves
its score alone: the kernel would kill a container's client and leave the container.

## 18. Nested runs: pass-through

A run decides its context from the environment it inherited ("set" as defined in section 1: an
empty variable is absent), in this order:

1. `CORESLOT_UNBUDGETED` is set (and the run is not `--independent`): it runs unbudgeted with its
   need, prints a notice, passes the variable on, and writes no degraded record of its own.
2. `CORESLOT_HOLD` names a hold (and the run is not `--independent`) whose pid is an ancestor of
   the run and whose fresh probe in `$S/holds/` fails (some process holds it): it is a
   **pass-through**, inside its parent's reservation. It does not queue, enter the section or lock
   anything. It runs with its need, or with the parent's `CORESLOT_GRANTED_MEM` when that is less
   (the CLI says so when the need was given as `--mem`); it passes the parent's descriptor on
   unchanged and sets `CORESLOT_GRANTED_MEM` to the narrowed value. A hold that is malformed, dead, gone or
   not an ancestor's, or a `$S` that cannot be resolved, makes the run an ordinary request: a
   parent that holds the budget is always an ancestor whose hold is live. (When the process table
   cannot be read, the Go CLI takes the hold's pid for an ancestor's.)
3. Otherwise, or with `--independent`, it is an ordinary request.

A pass-through, valid (rule 2) or unbudgeted (rule 1), enters no section and writes nothing under
`$S`: no ticket, hold, `gen` bump, degraded record or run-log line. It has no
hold of its own, so it neither ticks nor is listed by a stop round; the memory it uses is part of
its parent's footprint.

A run whose `CORESLOT_HOLD` names such a hold, a pass-through or an `--independent` run, keeps its
command in the process group it inherited, as `--same-group` does, so that a stop round's kill of
its parent's command reaches it; such a run is never a victim itself (section 23.5).

Nested runs are **not** budgeted against each other: two siblings started together under a 3 GiB
reservation both run with 3 GiB. A parent that wants concurrent children partitions its reservation
itself.

## 19. Exit status

| Status | Meaning |
|---|---|
| child's own | the command ran (`128 + signal` when killed) |
| 130 | interrupted while waiting (`Ctrl-C`) |
| 75 | refusal (section 14), including giving up after 3 stops (section 24); `--refusal-code` changes it for commands whose own 75 matters; `cancel`: the request was not published (no such waiting ticket, an ambiguous label, a state error, a format it does not implement, or the admission section held for longer than its bound); `doctor --clear`: the record is kept because `FORMAT` is malformed or names a format it does not implement; `doctor --reset`: nothing is removed because something is live (a hold, a ticket, a client inside the admission section) |
| 64 | usage error |
| 126 / 127 | the command is not executable / not found |
| 70 | internal error: something other than a state error, a refusal or a usage error stopped the command from running |
| 3 | `status`: runs fail open, because degraded mode is recorded or a state error (an unusable directory, a malformed `FORMAT` or one naming a format this client does not implement, an invalid configuration) stands; `doctor`: degraded mode is recorded and nothing else is wrong |
| 1 | `doctor`: it found a problem (an unusable directory, filesystem, configuration, or a `FORMAT` that is malformed or names a format this client does not implement), including a state error that keeps `--clear` or `--reset` from acting or leaves `--reset` partial; the `FORMAT` refusal of `--clear` is 75 |

A state error (section 14) is never exit 70: `run` fails open (or refuses under `--strict`),
`status` exits 3, `doctor` exits 1 and `cancel` refuses with 75. `--help` prints on stdout and exits
0 for every command. A command's own exit 75 is its status like any other: the run prints no marker
for it, and only a give-up (section 24.2) prints one.

## 20. Run log

Each run SHOULD append one JSON line of at most 4 KiB to `$S/log/runs.jsonl` with a single
`O_APPEND` write (outside the section), into a directory whose `FORMAT` it has read and understood
during the run (section 14); a run whose release found a mismatch writes no line (section 12.3). A
run that was stopped for memory appends one for each stopped attempt and one for the attempt that
ends the run (section 24). `FORMAT` may change while the command runs, so the writer reads it
again, outside the section, immediately before the write, and writes no line when it is absent, malformed or names a format the writer does not
implement. A pass-through (section 18) appends no line. A line that would exceed 4 KiB MAY be fitted by shortening an
informative string; a writer that cannot fit it writes no line. The object carries:

| Key | Type | Content |
|---|---|---|
| `time_ns` | integer | wall clock at the attempt's start: the run's, or for an attempt after a stop its re-enqueue |
| `label` | string | the request's label |
| `waited_s` | number | seconds spent waiting |
| `ran_s` | number | seconds the command ran |
| `user_s` | number | optional; user seconds of the command (below) |
| `sys_s` | number | optional; system seconds of the command (below) |
| `tick_share` | number | optional; `0..1`; the run's share of time lost to memory over its ticks whose paging gate was closed (below) |
| `mem_peak_kib` | integer | optional; memory peak of the command's process tree, in KiB (below) |
| `stops` | integer | optional; on the line of a stopped attempt, how often the run has been stopped for memory, this stop included; absent on every other line |
| `exit` | integer | the run's exit status |
| `fail_open` | string | the fail-open reason, or empty |
| `container` | boolean | optional; `true` when the run was container work (section 23.5); absent otherwise |

The usage keys `user_s` and `sys_s` are OPTIONAL. A writer MUST omit a key
whose value it does not know rather than write a zero: a run whose command could not be started
(section 19) has none, and neither has a value the platform does not report. A reader MUST treat
an absent key as unknown, never as zero. The values are the command's resource usage as the
wait that reaped it reports (`wait4(2)`'s `struct rusage`, or the same from `waitid`): they cover
the command and every descendant it waited for, but not a descendant it left running when it exited.
They feed no stall baseline (that reads `tick_share`, below); the median of their sum is a label's
typical CPU time (`typical_cpu_s`, section 7.4). The same rusage's `ru_maxrss`
is the peak of the largest single process among those, not a sum. It
is in KiB on Linux and the BSDs but in bytes on macOS; a writer MUST convert it to KiB (1 024
bytes), and MUST NOT use it on a platform whose unit it does not know. As in a body (section 7.2), a reader
MUST ignore a key it does not know: lines only gain optional keys within a format.

The key `tick_share` is OPTIONAL too, under the same rule. It is
`(Σ d_sys + Σ d_page_wait) / (Σ d_user + Σ d_sys + Σ d_page_wait)`, summed over the attempt's ticks
(section 23.1) of the `d_` values the run wrote into its hold, for the ticks whose sample had the
paging gate (section 23.3) closed: the sample the tick took or read in its step 2, whose pair was
compared (`level` green or yellow in `watch.json`) and with `gate` false. A tick whose sample was
not read, was the first or came across a gap adds nothing. A writer omits the key when the sums are
zero (no tick, every gate open or unread, or no time). It is the only source of a label's normal share
(section 23.3).

The memory key `mem_peak_kib` is OPTIONAL too, under the same rule: a writer omits what it does not know, and a reader treats an absent key as
unknown. A writer that samples reads the private memory of the command and every descendant every
1 s while the command runs, and sums it: on macOS each process's physical footprint; on Linux
each process's `RssAnon` plus `VmSwap` from `/proc/<pid>/status`, with the largest `RssShmem`
among them added once, since shared memory is held once. A process that exits between the listing
and its reading is skipped. `mem_peak_kib` is the largest sum, or the rusage's `ru_maxrss` when no
reading succeeded or that is larger.

The key `container` is OPTIONAL too: a writer writes it only when true, and a reader treats an
absent key as false. It says that the run's memory peak leaves out its containers.
It changes no counting: such a run counts as any other.

The line of a stopped attempt carries the attempt's own peak and `stops`; the line of the attempt that
ends the run carries neither mark and counts like any other. A **counted** run is one that is
labelled, not a pass-through, not run outside the budget (`fail_open` empty) and has no `stops`: only
counted runs say what a label needs (section 5.4) or what its stall baseline is.

The log MUST NOT contain argv or the environment; labels are recorded, so they must not carry
secrets. A client that finds `runs.jsonl` at or above 10 MiB before appending rotates it
inside the section: re-checks the size, then renames it to `runs.1.jsonl` (replacing the previous
generation). One generation is kept.

> **Implementation note.** To fit a line into 4 KiB the Go CLI shortens `fail_open`. It samples
> memory on macOS and Linux and writes the rusage's peak elsewhere when it knows it. `coreslot
> stats` and `coreslot report` read both generations of the log and skip lines that do not
> parse. A label's need is the 90th percentile of the `mem_peak_kib` of its last 20 counted runs.

## 21. Worked examples

All examples use one Linux machine: `MemTotal` is 16 GiB (`memory = 16384`), the configuration
declares `[memory] reserve = "1G"` and, in `[watch]`, `yellow_points = 20`, `red_points = 40`,
`red_samples = 10`, `gate_pages_s = 1000`, `floor = "2G"`, `calm_s = 30`, `post_kill_s = 15` and
`spacing_s = 3`, and `$S` is `/home/ana/.local/state/coreslot`. `FORMAT` is `coreslot-protocol 2`
unless stated. Listings show the entries under `$S` that matter; `[L]` marks a file whose lock is
held by a live process. `T0` is 2026-10-11T12:00:00Z, `1791720000000000000` ns. Each example is
independent of the others; except in section 21.6, `reconciled_ns` in `gen` is less than 30 s old
when the example starts, so reconciliation is not due. The writers in these examples number ticket
and hold names and their temporaries from one counter and the temporaries of `gen`, `FORMAT`,
`degraded` and `watch.json` from another; any numbering conforms (section 7.1).

The examples use the `coreslot` CLI, which is not part of the protocol: `coreslot run [--mem SIZE]
[--label L] [--timeout D] [--on-timeout run|fail] [--no-wait] [--strict] [--independent]
[--same-group] [--refusal-code N] -- COMMAND...`. Without `--mem` the need is the label's entry in
the needs file (section 5.4), else what the label's last counted runs needed (section 20), else
1 MiB for a container command (section 5.4), else the cold start (section 5.1). `--timeout` defaults to 0, no limit, and is one deadline from the run's start for
every wait of every attempt, restarts included; it never stops a running command; a negative one is a usage error. `--on-timeout`
defaults to `fail`, so a `--timeout` that expires refuses; with `run` an ordinary request runs
outside the budget, while a restart (section 24) refuses whatever `--on-timeout` says. Strict mode
is `--strict`; `--refusal-code` defaults to 75. The other
commands are `coreslot status [--json] [--observe DURATION]`, `coreslot cancel <ticket-id>`,
`coreslot cancel --label LABEL`, `coreslot doctor [--clear] [--reset]`,
`coreslot stats [--label LABEL] [--json]`, `coreslot report [--days N] [--json]` and
`coreslot version`.

### 21.1 Enqueue

Pid 4711 runs `coreslot run --restart=yes --mem 3G --label gate:unit -- pytest` at `T0`. The
ticket of pid 5120 (label `ci:o-r`, enqueued at 11:59:05) is already waiting.

Before:

```
gen                                                   {"gen": 41, "reconciled_ns": 1791719990000000000}
queue/01791719945000000000-0000005120-000007-ci_o_r   [L]
```

Pid 4711 enters the section (rewrites `admit.lock` with `{"pid": 4711, "entered_ns": ...}`), reads
`FORMAT`; reconciliation is not due (10 s old). `arrival_ns = 1791720000000000000`, so the order
key is `01791720000000000000`. It creates `queue/.tmp-0000004711-000001`, locks it, writes the body,
renames it without replacement, bumps `gen`, and leaves.

After:

```
gen                                                    {"gen": 42, "reconciled_ns": 1791719990000000000}
queue/01791719945000000000-0000005120-000007-ci_o_r    [L]
queue/01791720000000000000-0000004711-000001-gate_unit [L]
```

Body of the new ticket:

```json
{"label": "gate:unit", "need_mib": 3072, "created_ns": 1791720000000000000}
```

The ticket of pid 5120 sorts first: it arrived first.

### 21.2 Refresh

No name changes. Pid 4711 calls `futimens` on its ticket every 2 s; pid 6001, a waiter behind it,
observes the mtime once per second on its own monotonic clock `m`:

| `m` (s) | mtime pid 6001 reads | Last change observed at | Verdict |
|---|---|---|---|
| 100 | 12:00:02 | 100 (first sight) | WAITING |
| 101 | 12:00:04 | 101 | WAITING |
| 103 | 12:00:06 | 103 | WAITING |
| 104 | 12:00:06 | 103 | WAITING (pid 4711 was stopped after 12:00:06) |
| 113 | 12:00:06 | 103 | STALLED (10 s without a change): pid 6001 skips it |
| 115 | 12:00:16 | 115 | WAITING again (pid 4711 continued and refreshed) |

Pid 4711, resuming after a gap above 10 s, refreshes and then bumps `gen` in the section:
`{"gen": 42, ...}` becomes `{"gen": 43, ...}`; the files are otherwise unchanged.

### 21.3 Admission

Pid 7300 runs `coreslot run --restart=yes --mem 3G --label gate:big -- ./build`, enqueued at
12:09:00, and is head at 12:10:00. Pid 6500 holds 4 GiB for `pool:a`; its tree uses 1 536 MiB of them. The kernel's
headroom reads 7 168 MiB.

Before:

```
gen                                                   {"gen": 57, "reconciled_ns": ...}
queue/01791720540000000000-0000007300-000003-gate_big [L]
holds/01791719000000000000-0000006500-000002-pool_a   [L]
watch.json                                            (sampled 1.2 s ago, level green)
```

Inside the section: no `cancel/01791720540000000000-0000007300-000003`. The room is `7168 − 1024 −
max(0, 4096 − 1536) = 3584` MiB; the need, 3 072 MiB, fits. The re-election (section 12.1 step 5)
lists `queue/` again and finds no ticket ahead of its own. The stored sample is fresh and green, and
no level has cleared lately, so the gate holds nothing back. It publishes
`holds/.tmp-0000007300-000004` as the hold below, closes and unlinks its ticket, bumps `gen`, and
leaves.

After:

```
gen                                                   {"gen": 58, "reconciled_ns": ...}
holds/01791719000000000000-0000006500-000002-pool_a   [L]
holds/01791720600000000000-0000007300-000004-gate_big [L]
```

```json
{"label": "gate:big", "pid": 7300, "start_ns": 1791720600000000000,
 "arrival_ns": 1791720540000000000, "need_mib": 3072, "stops": 0, "footprint_mib": 0,
 "d_user_ns": 0, "d_sys_ns": 0, "d_page_wait_ns": 0, "normal_share": 0, "sampled_ns": 0,
 "victim": false, "learned": false, "child_exited": false}
```

The CLI starts the command with fd 3 = the hold and `CORESLOT_GRANTED_MEM=3072`,
`CORESLOT_HOLD=01791720600000000000-0000007300-000004-gate_big`. Had no hold
been live, the idle rule would have started the request whatever the headroom said; had the room
been 3 000 MiB, the head would have left the section and tried again at its next election.

### 21.4 Cancellation

Pid 8000 runs `coreslot cancel 01791719945000000000-0000005120-000007` at 12:01:00.

Before:

```
gen                                                   {"gen": 44, "reconciled_ns": ...}
queue/01791719945000000000-0000005120-000007-ci_o_r   [L]
cancel/                                               (empty)
```

Pid 8000 publishes, inside the section, `cancel/01791719945000000000-0000005120-000007` with
`{"requested_ns": 1791720060000000000, "requester_pid": 8000}`. Publishing a cancel request does
not bump `gen` (section 7.8).

After publication:

```
gen                                                   {"gen": 44, "reconciled_ns": ...}
queue/01791719945000000000-0000005120-000007-ci_o_r   [L]
cancel/01791719945000000000-0000005120-000007
```

Pid 5120 finds the request on its next poll, closes its ticket descriptor (the ticket is dead at
once), enters the section, unlinks both files, bumps `gen` (it removed its own ticket), and exits
with the refusal code 75.

After the owner's section:

```
gen                                                   {"gen": 45, "reconciled_ns": ...}
queue/                                                (no entry for pid 5120)
cancel/                                               (empty)
```

### 21.5 Withdrawal

Pid 4711 was started with `--timeout 5m --on-timeout fail`; at 12:05:00 its deadline passes.

Before:

```
queue/01791720000000000000-0000004711-000001-gate_unit [L]
```

It closes its ticket descriptor and exits 75 without entering the section.

After:

```
queue/01791720000000000000-0000004711-000001-gate_unit     (dead: a probe succeeds)
```

Every waiter behind it skips the dead ticket at once; the next reconciliation removes it.

### 21.6 Reconciliation

Pid 9200 enqueues at 12:20:00; `reconciled_ns` is 45 s old, so it reconciles first.

Before:

```
gen                                                    {"gen": 70, "reconciled_ns": 1791721155000000000}
queue/01791720000000000000-0000004711-000001-gate_unit     (dead)
queue/01791721150000000000-0000006100-000002-lint          [L]
holds/01791720600000000000-0000003999-000005-build         (dead: its holder exited)
holds/.tmp-0000003998-000002                               (mtime 12:18:00)
cancel/01791720900000000000-0000005555-000001              (no such ticket)
```

It probes and unlinks the dead ticket and the dead hold (holding each probe lock while unlinking),
removes the orphan cancel request and the 2-minute-old temporary file, and writes `gen` with
`reconciled_ns` = now and `gen` + 1. Then it publishes its own ticket and bumps `gen` again.

After:

```
gen                                                    {"gen": 72, "reconciled_ns": 1791721200000000000}
queue/01791721150000000000-0000006100-000002-lint          [L]
queue/01791721200000000000-0000009200-000001-gate          [L]
```

### 21.7 Pass-through

Under the run of section 21.3 (fd 3 = hold, `CORESLOT_GRANTED_MEM=3072`,
`CORESLOT_HOLD=01791720600000000000-0000007300-000004-gate_big`), the command starts
`coreslot run --mem 4G -- make test`.

The nested run finds `CORESLOT_HOLD` set, pid 7300 among its ancestors, and a fresh probe of
`holds/01791720600000000000-0000007300-000004-gate_big` that fails: some process holds it. It runs
with `min(4096, 3072) = 3072` MiB, saying so because its need was given as `--mem`, starts
`make test` with the same descriptor, and passes `CORESLOT_HOLD` on unchanged with
`CORESLOT_GRANTED_MEM=3072`.

A pass-through does not enter the section, lock anything or append a run-log line: `$S` is
unchanged before and after.

### 21.8 Tick and level

The run of section 21.3 ticks at 12:10:02 (`1791720602000000000`). The kernel reports no page wait.

1. It lists its command's tree once: the footprint is 1 984 MiB; in the last period the tree spent
   1.6 s in user mode and 0.3 s in system mode. It rewrites its hold in place:

   ```json
   {"label": "gate:big", "pid": 7300, "start_ns": 1791720600000000000,
    "arrival_ns": 1791720540000000000, "need_mib": 3072, "stops": 0, "footprint_mib": 1984,
    "d_user_ns": 1600000000, "d_sys_ns": 300000000, "d_page_wait_ns": 0, "normal_share": 0,
    "sampled_ns": 1791720602000000000, "victim": true, "learned": false, "child_exited": false}
   ```
2. It enters the section. The stored sample of `watch.json` is 3.2 s old, more than one period, so
   it takes the machine sample: the paging counters rose by 40 pages per second (below
   `gate_pages_s`, so the gate is closed), and the two live holds report their deltas. For this hold
   `S = 0.3 / 1.9 = 15.8 %`; its label has no counted run, so the hold is learning, no part of
   `E`, and `E` is 0. The detector stays green, and the bucket stays 0. The run replaces
   `watch.json` with the new sample (`sampled_ns` = `1791720602000000000`, `detector.level` 0) and
   leaves. A stop round was not due.
3. It finds no hold of its own name in `detector.stop`.

### 21.9 Stop round and restart

At 12:14:40 the detector's bucket reaches `red_samples` and the sample leaves the level red. Four
holds are live: `pool:a` (arrival 11:43:20, need 4 096 MiB, footprint 1 536 MiB), `gate:big`
(arrival 12:09:00, 3 072 / 1 984), `lint` (pid 7400, arrival 12:13:00, 2 048 / 2 300) and `watcher`
(pid 7500, arrival 12:14:00, 1 024 / 900), whose label has `no_restart = true`. The headroom reads
600 MiB against the 1 024 MiB reserve: the machine is `1024 − 600 = 424` MiB short.

1. The ticker that took the sample builds the stop list from the victims, newest arrival first,
   never the oldest running hold: `watcher` is not a victim; `lint` is next and its footprint, 2 300
   MiB, covers the shortage. It writes `detector.stop` =
   `["01791720781000000000-0000007400-000009-lint"]` and `detector.last_round_ns` into
   `watch.json`.
2. Pid 7400's next tick finds its own hold in the list. It sends SIGKILL to the process group of
   its command, which it has not reaped, reaps it, and sets `child_exited` in its hold.
3. No new round starts until `lint`'s hold reports `child_exited` and 15 s have passed.
4. Pid 7400 appends the stopped attempt's run-log line, closes its hold, and enqueues again:

   ```json
   {"time_ns": 1791720780000000000, "label": "lint", "waited_s": 1.0, "ran_s": 99.0,
    "mem_peak_kib": 2355200, "stops": 1,
    "exit": 137, "fail_open": ""}
   ```

   Its new ticket is `queue/01791720780000000000-0000007400-000010-lint`, the order key being its
   first arrival, with `need_mib` 2 300 (the larger of its peak and its learned need). It starts
   again when it is admitted, the level has been green for 30 s and the room is at least `2 300 +
   max(256, 230) = 2 556` MiB. The new hold has `"arrival_ns": 1791720780000000000` and, from its first tick,
   `"stops": 1`.
   After a third stop the run prints `coreslot: gave up after 3 memory stops (run <hold>)`, naming
   the hold of its third attempt, and exits 75.

## 22. Timing constants

| Constant | Value |
|---|---|
| ticket refresh | every 2 s |
| STALLED | 10 s without an observed mtime change, on the observer's monotonic clock |
| `gen` read and election | every 1 s |
| front window | the first 4 tickets of the queue, watched in every election |
| observer pass gap | more than 3 s between an observer's last examination and its next election is not watched |
| head admission retry | backoff from 0.25 s up to 0.5 s |
| section entry retry | `LOCK_NB`, backoff from 10 ms up to 250 ms |
| reconciliation | at most once per 30 s machine-wide; temporary files older than 30 s |
| hold removal after exit | section entry within 2 s, else left to reconciliation |
| waiting line (CLI) | when the wait begins, then every 30 s |
| run-log rotation | at 10 MiB, one generation |
| memory sampling (CLI) | every 1 s while the command runs |
| tick | every 2 s while the command runs; one machine sample per period |
| stored sample | the period's below 1.5 s; fresh up to 6 s; a gap above 6 s or a 1 s clock skew |
| restart | gives up after 3 stops; margin `max(256 MiB, need / 10)` |
| stall share smoothing | time constant 10 s (`stall_ewma`) |

## 23. The watchdog

The watchdog keeps memory pressure from stalling the work coreslot started. It is not a process:
every run that has a command running measures its own work, one shared detector turns the
measurements into a level, and a run stops only its own command. All its state lives in hold bodies
(section 7.4) and `watch.json` (section 7.11). It writes only under `$S`; it starts no subprocess.

### 23.1 The tick

A run with a command running under a hold that reserves memory ticks every 2 s, from the command's
start until it exits, in both modes of section 23.6. Each tick:

1. **Measures.** Outside the section the run lists its command's process tree once and reads, per
   process, user time, system time and, where the kernel reports it (macOS 27 and later), the time
   spent waiting for pages (`ri_page_wait_time_mach` of `proc_pid_rusage`; `/proc/<pid>/stat`
   `utime` and `stime` on Linux). It rewrites its own hold's body in place (section 7.4):
   `footprint_mib` (the tree's footprint, as section 20 measures it), `d_user_ns`, `d_sys_ns` and
   `d_page_wait_ns` (the tree's times since the run's previous tick), `cpu_ns` and `stall_ewma`
   (section 7.4) and `sampled_ns`. It sets `child_exited` once, when the command has exited.
   `stops`, `victim`, `normal_share`, `learned` and `typical_cpu_s` hold for the attempt: the run
   writes them once, when its command starts, before its first tick.
2. **Samples.** It enters the section briefly (section 8; a tick that cannot enter within a short
   bound leaves its part of that period to the others) and reads `watch.json`. If the stored sample
   is at least 1.5 s old (a period less a quarter, so that runs ticking every 2 s never leave the
   next sample 4 s away), or lies in the future, it takes a new machine sample
   (section 23.2), advances the detector (section 23.4) and, when a stop round is due, writes the
   stop list (section 23.5); otherwise it only reads. One sample is taken per period (1.5 to 2 s)
   machine-wide, one bucket step per sample, and no writer races another: the section serializes
   them.
3. **Acts.** It looks for its own hold's name in `detector.stop` (section 23.5 step 2).

A stored sample more than 3 periods (6 s) old is no reading: admission ignores the level (section
12.4), and the next ticker takes a fresh sample.

### 23.2 The machine sample

A **machine sample** is the host readings, the running holds' latest measurements (section 23.3)
and the room (section
12.1; 0 when the headroom is unknown), taken together. The host readings, which a sample takes in
well under a millisecond with no subprocess:

| Reading | macOS | Linux |
|---|---|---|
| decompressions and swap-ins, cumulative | `host_statistics64` (`HOST_VM_INFO64`) | swap-ins: `pswpin` of `/proc/vmstat`; no decompressions |
| memory stall (PSI) | none | the `total` of the `some` line of `/proc/pressure/memory`, in µs; absent or `EOPNOTSUPP` when the kernel does not report it, and then not used |
| swap used, swap free | `vm.swapusage` | `SwapTotal − SwapFree` and `SwapFree` of `/proc/meminfo` |
| free space of the volume that holds the swap files | `/System/Volumes/VM` | none |
| available memory | none | `MemAvailable` of `/proc/meminfo` |
| the sleep-inclusive clock, the **host clock** | `CLOCK_MONOTONIC_RAW` | `CLOCK_BOOTTIME` |
| the sleep-exclusive clock | `CLOCK_UPTIME_RAW` | `CLOCK_MONOTONIC` |

Two consecutive samples are a **gap**, and cannot be compared, when the host clock advanced by more
than 6 s, when the two clocks advanced by amounts more than 1 s apart (the machine slept in the
period), or when the host clock went back (a reboot). A counter that went back also makes them not
comparable. A reading that cannot be read is no sample: the detector stores an empty sample, so
that the next comparison starts afresh.

### 23.3 The stall measure

A **running** hold is a live hold whose body is well-formed, whose `child_exited` is false and whose
`sampled_ns` is at most 6 s old on the wall clock: its run still ticks. Any other live hold reserves
memory (section 12) but is no job of the watchdog. For each running hold,
`S = (d_sys + d_page_wait) / (d_user + d_sys + d_page_wait)`
over its latest tick period. Its normal share `S0` is the median of `tick_share` (section 20, the same
measure over the ticks whose gate was read and closed) over the last 20 counted runs of its label that report
it. When there are none it
has no normal share and is **learning**: no value stands for a command nothing is known of. Each run decides its own
command's `S0` and whether it is learned once, from its own read of the run log when it starts, and
writes both in its hold (`normal_share`, `learned`, section 7.4). A machine sample takes them from the
holds and reads no run log, so every run that samples computes the same `E`, and a command started
before the first run of its label completes is learning until it ends. The machine's **excess** `E`, in percentage points, is

```
E = 100 × Σ wᵢ · max(0, Sᵢ − S0ᵢ) / Σ wᵢ      wᵢ = d_userᵢ + d_sysᵢ + d_page_waitᵢ
```

over the holds that have a normal share and worked for at least a tenth of the period,
`wᵢ ≥ 0.1 × (the period)`, and 0 when there are none: an idle job's few milliseconds, mostly system
time, say nothing about memory. A learning hold is no part of `E`: it still reserves memory (section
12), counts in the room and is listed by `status`, which says `learning:` and how many running holds
have no normal share (`status --json`: `watch.learning`); at red, when a red round has no victim
(every running hold is the oldest, learning or never a victim), it does not say that the watchdog is
protecting but that a red round can stop no running command.
A command is judged only once a run of its label has
completed with a `tick_share` (a run shorter than one tick, or whose every gate was open or unread, leaves the label learning); until then only a floor round stops it (section 23.5). The **paging gate** is open when the
decompressions and swap-ins together rose by at least `gate_pages_s` per second over the period or,
where the kernel reports it, the memory stall `some` rose. The gate only tells memory work from
other system time. On Linux the stall rises in almost every period, so there the gate is nearly
always open and tells nothing.

### 23.4 Levels

The detector turns each comparable pair of samples into a level, from `E`, the gate and the swap
room. Its state is in `watch.json`. The levels, in increasing order, are green, unknown, yellow,
red and floor. A sample that is unknown has that level; any other has the highest of the levels
below that applies.

- **Unknown:** a gap, a counter that went back, an empty sample, or a reading that cannot be read.
  It never counts towards calm; the bucket and the yellow latch are kept as they were, except as
  the red bucket's freshness below says. An empty sample leaves the detector's 20 s window as it
  was. The floor
  needs no rate, so a pair that cannot be compared but whose current reading was read
  (`wall_ns > 0`) is still tested for it, and is floor when it holds; an empty sample never is.
  An optional reading the platform lacks (the memory stall, page wait) is not unknown.
- **Yellow:** `E ≥ yellow_points` with the gate open in 2 of the last 3 samples. It clears when `E`
  stays below `yellow_points` for 15 samples in a row, and when the red bucket's freshness rule
  below empties the bucket, so that yellow does not outlast a pause.
- **Red:** a leaky bucket: +1 for a sample with `E ≥ red_points` and the gate open,
  −0.5 for any other comparable sample, never below 0, never above `red_samples`. It keeps only
  fresh evidence: a real reading whose host clock is more than 20 s after that of the last real
  reading, or before it, empties it and clears the yellow latch. Nothing else does, so a sleep
  shorter than that keeps both, as does an empty sample between two real readings.
  Red while it is at `red_samples`, so the first sample after the danger leaves red.
- **Floor:** the swap room (the free space of the swap volume plus the unused swap on macOS;
  `SwapFree` plus `MemAvailable` on Linux) is under `floor`, and swap used grew by at least 256 MiB
  over the last 20 s.
  It is an edge: a calm machine with little room never reaches it. Without swap, swap used never
  grows: on a Linux host without swap the floor never fires and the kernel's own out-of-memory
  killer acts instead (a coreslot run carries `oom_score_adj` 500, section 17, so it goes first).
- **Green** otherwise.

`calm_since_ns` is the host clock of the sample that made the level green, kept while it stays
green, and 0 while it is not. `yellow_cleared_ns` is that of the sample at which the level fell from
yellow, red or floor to green, unknown samples between the two left out.

### 23.5 Stop rounds

A **round** is due when a sample leaves the level at red or floor, `post_kill_s` has passed since
the last round, and every hold the last round listed is gone, reports `child_exited` or no longer
has `victim` true. Rounds are one at a time.

1. The ticker that advanced the detector writes the stop list into `detector.stop`, and the round's
   time into `detector.last_round_ns`. The list holds the **victims** from the newest arrival to the
   oldest: running holds whose `victim` is true, never the oldest running hold by arrival (of equal
   arrivals the smallest hold name counts as the oldest, and the greatest as the newest). A red
   round takes the victims that have a normal share (section 23.3). A floor round takes every
   such victim first and only then the learning ones, newest first within each. It takes victims
   until their footprints sum to at least the **shortage**, `max(reserve − headroom, 0)` MiB of the
   sample it just took (the room counts reservations not yet used, which free no memory), and at
   least one. With only the oldest hold or holds that are not victims left, the list is empty.

   A red round may pass over its newest victim when that victim is **nearly finished**, without
   counting its footprint, and go on to the next one; a floor round passes over none.
   For a victim, with `T` its hold's `typical_s`, `C` its `typical_cpu_s` and `c` its `cpu_ns` in
   seconds, its progress is `p = c / C` and its predicted remaining time
   `t3 = T × (1 − p) / (1 − min(stall_ewma, 0.8))`. It is nearly finished when `C > 0`, `p < 1`
   and `t3 < T` (which needs `T > 0` and `p > 0`) and, when an earlier red round kept it, the
   round's time is before that record's `deadline_ns` and `t3` is smaller than its `left_s`: the
   first keep sets the deadline to the round's time plus its `t3`, and every later one needs the
   estimate to keep shrinking within it. The newest victim is passed over only when exactly one
   job takes its place: the next victim of the round's order alone has a footprint of at least
   what the round must free, `max(shortage, 1)`, and a smaller `cpu_ns`, so that work done counts
   against work done and the round never stops a chain of older jobs for it. With no next victim,
   or one that does not cover the shortage alone or has done as much work, it is stopped as
   before. The round writes `detector.kept`: the record
   `{deadline_ns, left_s: t3, done: p, round_ns}` of the victim it kept, the deadline kept from
   the first. A record lasts as long as its hold is live, also while its run misses a tick, and
   goes when a sample finds the hold gone. The rule spares work that is almost done when memory
   is red, by coreslot's own measurements of the label; it never changes the order of the
   victims, and the floor stays the safety net.

   > **Implementation note.** The Go CLI's `cpu_ns` sums the deltas of its tree listings, which
   > miss what a process spent between the last listing and its exit, so its progress reads low
   > against `typical_cpu_s`, which comes from the reaping `wait4`: the rule errs towards stopping.
2. A run kills only its own command, only when its own hold's name is in the list. It sends
   `SIGKILL` to the command's process group, and only while it has not reaped the command, so the
   group id cannot have been reused. The kill and the reap are serialized under one lock in the
   run's supervisor. That is the one control: no other signal, no pid list.
3. No new round starts until every listed hold reports `child_exited`, is gone or no longer has
   `victim` true (a run that became container work ignores the stop, and may run for hours), and
   `post_kill_s` has passed, except that under `mode = "enforce"` a round written in observe mode
   (`detector.observed`), whose holds never exit, holds nothing back.

The oldest running hold, a hold whose run did not declare its command safe to run again (the
CLI's `--restart=no`), a hold whose label has `no_restart`, container work and a run that shares
coreslot's process group are never victims, and a hold that is learning (it has no normal share) is a victim of
a floor round only. A run outside the budget (section 14, `--on-timeout run`) ticks nothing, so it
is no running hold: it is never a victim and never the oldest. A run that finds its command ended
by a `SIGKILL` it did not send does not restart it (section 24.3). The kill reaches the command's
process group only: a descendant that left it (`setsid`, a daemon) survives the stop and may still
hold memory, ports or files when the command comes back, although its footprint counted as freed.

**Container work** is a run whose command is a container command (section 5.4) or whose process
tree holds a container client. A stop would kill the client and leave its container running, so it
is never a victim. A run that writes a run-log line and is not yet container work looks for a
client at every listing of its tree, every 1 s, until the first hit: a process named `docker`,
`docker-compose` or `podman`, or, where the platform reports it, a process with a unix socket
connected to a socket whose basename is `docker.sock` or matches `podman*.sock`. A process that
cannot be read is skipped. A hit lasts for the attempt, since a client may close while its
containers run on. On a hit the run writes `victim` false and `container` true into its hold, on a
later listing when that write fails, and from then on ignores every stop list, one written before
the hit included; `--restart` and the Linux score (section 17) stay as they were settled before the
start. A stop in the window before the hit can still stop the run. A run outside the budget has no
ticking hold to mark and scans for its run-log line alone; a pass-through (section 18) writes no
line and does not scan. The scan sees only the run's own tree, at its own pace: a client that
connects by TCP, a call shorter than a second and a client outside the tree are not seen, and such
work stays ordinary.

> **Implementation note.** The Go CLI reads `p_comm` and, on macOS, the peer path of each unix
> socket with `proc_pidfdinfo`. On Linux it reads `/proc/PID/comm` alone: a client socket has no path
> of its own there, so a library client stays ordinary work.

A run stopped by a floor round tells its agent that the whole machine ran out of memory (swap
nearly full) and to free memory outside coreslot before the command comes back, and restarts it by
itself as for any stop (section 24.1); a give-up or a refusal that follows a floor stop says it too.
A red stop does not say so. What a run does about a round that lists it follows that round as it
was written, its kind (`detector.round_floor`) and its mode (`detector.observed`), not the level or
the mode at the run's own later tick.

### 23.6 Observe mode and the sample log

With `mode = "observe"` every step runs, the stop list included, except the kill: a run whose hold
is listed reports `would stop <label> (<reason>)` and goes on. With `sample_log = true`
the ticker that takes a machine sample appends one JSON line to `$S/log/watch.jsonl` with the
readings, `E`, the level and the bucket, in either mode. The file informs and nothing reads it but
the calibration harness; a writer appends under the rules of the run log, rotating it inside the
section to `watch.1.jsonl` at 10 MiB, and a reader skips lines it cannot parse.

## 24. Restart of a stopped run

### 24.1 A stopped run comes back

The `coreslot run` of a stopped command keeps running. It reaps the command, closes its hold
(section 12.3), appends one run-log line for the stopped attempt with `stops` (section 20) and
enqueues again (section 10.1) with its **first arrival**: the order key of the new ticket and the
`arrival_ns` of its next hold are those of its first attempt, so it keeps its place. Its need is
the larger of the peak footprint the stopped attempt reached and the need the run had at its start.
It waits as any request does and, in addition, under the restart conditions of section 12.4; on admission it
starts the same command again, under a new hold whose `stops` is the number of stops so far.
A run with `--no-wait` does not come back, since a restart waits for calm: it refuses with the
refusal code, naming the stop.

### 24.2 Giving up

After the third stop the run does not restart. It prints the marker line `coreslot: gave up after 3
memory stops (run <hold>)`, where `<hold>` is the name of the hold of the last attempt, as the first
line of its give-up, and exits with the refusal code (section 19).

### 24.3 A kill from outside

A command that ends by `SIGKILL` which the run did not send, the kernel's out-of-memory killer or
another user's kill, is not restarted (a command that exits with status 137 itself was not killed
and is not reported): the run reports that the command was ended by SIGKILL from
outside coreslot and not restarted, and exits with the command's status.
