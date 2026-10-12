# coreslot protocol, format 1

This document is the normative definition of the coreslot on-disk protocol, format 1: the state
directory, its files and their bodies, the admission critical section, and the descriptor and
environment contract between a run and the command it starts. A client written from this document
alone, in any language, shares one budget with every other conforming client on the machine.

It is the normative subset of the design (`PROTOCOL.md`, revision 4c).
Section references of the form "§N" point there; they are for traceability only, and nothing
in this document depends on reading the design.

Format 1 is not frozen until the conformance suite, including its adversarial interleavings, passes
on macOS and Linux.

## 1. Conventions

The key words MUST, MUST NOT, REQUIRED, SHALL, SHALL NOT, SHOULD, SHOULD NOT, RECOMMENDED, MAY and
OPTIONAL are to be interpreted as described in RFC 2119. Passages marked **Implementation note**
are informative: they say what a particular implementation or test does, and impose no
requirement on a client.

- **Client:** any process that reads or changes the state directory: the `coreslot` CLI, a program
  embedding a library, an independent implementation.
- **`$S`:** the state directory (section 4.1).
- **Wall clock:** `CLOCK_REALTIME`. Every `*_ns` value is nanoseconds since the Unix epoch on the
  wall clock of the process that wrote it.
- **Monotonic clock:** the observing process's own monotonic clock (`CLOCK_MONOTONIC`, or any clock
  that does not step with the wall clock). It is never written to disk.
- **Lock:** `flock(2)` on an open file description. "Probe" means: open the file, try
  `flock(LOCK_EX|LOCK_NB)`, and close the descriptor again at once. A probe that obtains the lock
  proves that no other open file description holds it.
- **Integer** (in a body): a JSON number with no fraction and no exponent part, within the signed
  64-bit range. `1.0` and `1e3` are not integers.
- **Set** (an environment variable): present with a non-empty value. A variable whose value is
  empty is treated exactly as an absent one, everywhere in this document.
- Sizes: `KiB` = 1 024 bytes, `MiB` = 1 048 576 bytes.

## 2. Contract inventory

Every shared file, field, environment variable and transition of format 1, and where it is defined.

| Item | Kind | Defined in | Spec |
|---|---|---|---|
| `FORMAT` | file | [7.6](#76-format), [15](#15-formats-and-token-only-mode) | §5.12 |
| `admit.lock` | file | [7.7](#77-admitlock), [8](#8-the-admission-critical-section) | §5.5 |
| `gen` | file | [7.8](#78-gen), [10](#10-liveness-waiting-and-reconciliation) | §5.7 |
| `res/` | directory of token files | [6](#6-tokens-and-descriptor-ownership) | §5.3 |
| `queue/` | directory of tickets | [7.3](#73-ticket), [10](#10-liveness-waiting-and-reconciliation) | §5.4, §5.7 |
| `holds/` | directory of holds | [7.4](#74-hold), [12](#12-admission) | §5.4, §5.9 |
| `cancel/` | directory of cancel requests | [7.5](#75-cancel-request), [13](#13-cancellation-and-withdrawal) | §5.10 |
| `degraded` | file | [7.9](#79-degraded), [14](#14-errors-fail-open-and-degraded-mode) | §5.11 |
| `log/` | directory of the run log | [7.10](#710-log-run-log), [20](#20-run-log) | §6.5 |
| `config.toml` (under the override only) | file | [5.3](#53-configuration-file) | §5.1, §7 |
| ticket body | body | [7.3](#73-ticket) | §5.4 |
| hold body | body | [7.4](#74-hold) | §5.4 |
| cancel body | body | [7.5](#75-cancel-request) | §5.4 |
| `CORESLOT_GRANTED_*` | environment | [17](#17-descriptor-and-environment-contract) | §6.1 |
| `CORESLOT_HELD_FD` | environment | [17](#17-descriptor-and-environment-contract) | §6.1 |
| `CORESLOT_TOKEN_FDS` | environment | [17](#17-descriptor-and-environment-contract), [18](#18-nested-runs-pass-through) | §6.1, §6.3 |
| `CORESLOT_UNBUDGETED` | environment | [14](#14-errors-fail-open-and-degraded-mode), [18](#18-nested-runs-pass-through) | §5.11, §6.3 |
| `CORESLOT_STATE_DIR` | environment | [4.1](#41-location) | §5.1 |
| enqueue | transition | [10.1](#101-enqueue), example [21.1](#211-enqueue) | §4, §5.6 |
| refresh | transition | [10.2](#102-refresh-and-stalled), example [21.2](#212-refresh) | §5.7 |
| admission | transition | [12](#12-admission), example [21.3](#213-admission-with-clamp) | §5.9 |
| clamp | transition | [12.2](#122-effective-request-and-clamp), example [21.3](#213-admission-with-clamp) | §5.9 |
| cancellation | transition | [13.1](#131-cancellation), example [21.4](#214-cancellation) | §5.10 |
| withdrawal | transition | [13.2](#132-withdrawal), example [21.5](#215-withdrawal) | §5.10 |
| reconciliation | transition | [10.4](#104-reconciliation), example [21.6](#216-reconciliation) | §5.7 |
| adoption | transition | [12.4](#124-adoption), example [21.7](#217-adoption) | §8.1, §9 |
| token-only admission | transition | [15.2](#152-token-only-mode), example [21.8](#218-token-only-admission) | §5.12 |
| pass-through | transition | [18](#18-nested-runs-pass-through), example [21.9](#219-pass-through) | §6.3 |

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
 (none) ── adopt† (already-running work; bypasses the queue) ──▶ HELD
 (none) ── token-only admission† (format not understood; nothing published) ──▶ held tokens
```

WAITING and STALLED are the same file; STALLED is a verdict each observer reaches on its own
(section 10.2). A ticket or hold whose lock can be taken is **dead**, whatever its mtime; the next
reconciliation (section 10.4) removes its file.

Every conforming client MUST preserve these invariants:

- **I1 identity:** no two live holds list the same (resource, index), and every token a live hold
  lists is locked through an open file description whose inode equals
  `stat($S/res/<name>/<i>).st_ino`. A holder that finds `st_nlink == 0` or a different inode at its
  token's path reports degraded mode (section 14).
- **I2 truthful grant:** a live hold's body lists exactly the tokens it locks. Tokens may be locked
  without a published hold only inside the admission section, by a token-only client
  (section 15.2), momentarily by a probe (section 6, inside or outside the section), or between a
  holder's last close and the next reconciliation.
- **I3 bounded admission:** an admission or adoption never brings the number of locked tokens of a
  resource — at **any** index that exists, including indices at or above the current size — above
  that resource's current size. A later shrink may leave locked tokens above the new size; no
  admission happens until they fall below it.
- **I4 order:** ordinary admissions happen in order-key order (section 9) among waiters whose
  tickets were published before the election (section 10.3). There is no backfill: the head blocks
  every waiter behind it, and a waiter is passed over only while it is STALLED, dead or cancelled.
  Adoption and token-only admission are outside the queue by definition.
- **I5 progress:** assuming every live client is scheduled at least once per poll period, if the
  head's minimum is free and no process is stopped inside the admission section, the head is
  admitted within 3 s; up to 4 consecutive stopped tickets at the front of the queue stop blocking
  it within ≈ 11 s (10 s without an observed change plus one poll). A stopped ticket further back
  stops blocking a waiter within 11 s of becoming the nearest ticket that waiter has not passed
  over.
- **I6 one budget:** every client of every format counts the same token files under the same
  admission lock.
- **I7 minimum:** an ordinary admission grants at least `min(min, size)` of every declared resource
  it names. The only grants below a request's `min` are clamped requests (section 12.2) and
  adoptions with a reported shortfall (section 12.4).

## 4. State directory and filesystem policy

### 4.1 Location

- macOS: `<home>/Library/Application Support/coreslot/` (the path contains a space; clients MUST
  handle it everywhere they use the path).
- Linux and every other Unix: `<home>/.local/state/coreslot/`.
- `<home>` MUST come from the password database (`getpwuid(getuid())`), not `$HOME` or any
  `XDG_*` variable, so a shell, a cron job, a user service and an ssh session of the same user
  resolve the same directory.
- The only override is the environment variable `CORESLOT_STATE_DIR` (a library MAY offer the same
  override as an option). When it is set and non-empty, its value is the state directory; it MUST
  be an absolute path, and a relative value is a state error (section 14). The override is for
  tests: it creates a separate budget, and it also moves the configuration (section 5.3).

### 4.2 Modes, types and ownership

- Every directory a client creates is mode `0700`; every file `0600`; files are created with
  `O_NOFOLLOW`.
- On open, a client MUST check that each existing path is a regular file or directory of the
  expected type, owned by the user, not a symlink, and not group- or world-writable; anything else
  is a state error (section 14). "Owned by the user" means owned by the process's real user id
  (`getuid()`, the id section 4.1 resolves the home from); a client whose effective user id
  differs from its real one is unsupported. "Group- or world-writable" means
  `mode & 0o022 != 0`. The check applies to `$S`, to every file and directory of the layout
  (section 4.4), and to every token file (section 6) that the client opens, probes or removes; a
  directory or top-level file of the layout, a temporary file in `$S` or a token file that is a
  symlink, a directory, another user's, or group- or world-writable is a state error. Inside
  `queue/`, `holds/` and `cancel/` the rule is different: an entry whose name matches the
  directory's grammar (section 7.1, temporaries included) but whose type, owner or mode fails the
  checks above is not a state error. It is a **stray**: ignored and counted exactly like an entry
  with a malformed body (section 7.2): never trusted, never opened or probed, never removed
  (reconciliation skips it and goes on), and `status` and `doctor` report it in the ignored count.
  An entry whose name does not match its directory's grammar is ignored whatever its type
  (sections 4.4 and 6).
  These directories are `0700` and the user's own, so only the user (or the superuser) can place an
  entry in them; a hostile same-user process is outside the threat model.
- Every descriptor a client opens on a file under `$S` MUST be close-on-exec (`O_CLOEXEC`), except
  the copies it deliberately passes to a child (section 17). A leaked descriptor keeps a lock alive
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
res/<name>/<i>   token files (section 6); never deleted, semantics fixed for every format
queue/           tickets (section 7.3)
holds/           holds (section 7.4)
cancel/          cancellation requests (section 7.5)
degraded         degraded-mode record (section 7.9)
log/runs.jsonl   run log (section 7.10)
config.toml      configuration (section 5.3); optional, present only under the override
```

`log/` may also contain `runs.1.jsonl`. Each of `queue/`, `holds/`, `cancel/` and `$S` itself may
contain temporary files named `.tmp-<pid>-<seq>` (section 11). A client creates a directory of the
layout when it first needs it, with mode `0700`; `EEXIST` is not an error, and the existing entry is
checked as in section 4.2. Missing ancestors of `$S` (`~/.local/state/` often does not exist) are
created with mode `0700` and are not checked: the policy of section 4.2 starts at `$S`. `queue/`,
`holds/`, `cancel/` and `log/` are created only by a client that has read a `FORMAT` it understands
(section 15.2).

Directory listings are bounded: a directory holds at most 65 536 entries. A reader MUST NOT read
more than 65 537 entries of one directory, and a directory with more than 65 536 is a state error.
Publishing into a directory that already holds 65 536 entries is a state error. Entries that do not
match the grammar of their directory (section 7.1) are ignored and counted (`status` reports the
count); they are never removed except by reconciliation's rules (section 10.4).

The directory is not a cache: token files are permanent and liveness is decided by locks. Deleting
or resetting `$S` while any client is live creates a second set of token inodes and is not
supported. The CLI's `doctor --reset` runs inside the admission section and checks that no holds,
tickets, other section holder or locked tokens exist; only then does it remove the entries of
`queue/`, `holds/` and `cancel/`, `gen`, `FORMAT`, `degraded` and `log/`. It keeps `admit.lock`,
`res/` and `config.toml`: the format-stable files (section 15.1), whose inodes live clients may hold
open, and the configuration. A reset never deletes, renames or replaces `admit.lock` or a token
file. It refuses when `FORMAT` is well-formed and names a format the client does not implement (a
newer client's waiters are invisible to it and its entries are not format-1 grammar); a missing,
malformed or implemented `FORMAT` does not stop it. It does not reconcile first (section 10.4):
it removes every entry of `queue/`, `holds/` and `cancel/` reconciliation would remove, and more.
Temporary files directly in `$S` (section 11) are not entries it removes; the next
reconciliation removes them once they are older than 30 s.

## 5. Resources, sizes and configuration

### 5.1 Resource names

A resource name matches `^[a-z][a-z0-9-]{0,31}$`. Its **environment form** upper-cases it and maps
`-` to `_` (`pg-local` → `PG_LOCAL`); the grammar admits no `_`, so two names never share a form.
The resource `cpu` always exists; other resources exist when the configuration declares them.

### 5.2 Sizes

A size is an integer in `[1, 4096]`. A configured size is one of:

| Configured as | Size |
|---|---|
| an integer `n`, `1 ≤ n ≤ 4096` | `n` |
| a string `"N%"`, `N` an integer `1..100` | `max(1, ⌊cpus × N / 100⌋)` |
| a string `"-K"`, `K` a non-negative integer | `max(1, cpus − K)` |

In both string forms, `N` and `K` are written as 1 to 9 ASCII decimal digits, leading zeros allowed
(`"05%"` means `"5%"`, `"-007"` means `"-7"`), with nothing else in the string: no sign before the
digits, no space.

Any other value (an integer outside `[1, 4096]` such as `0`, `4097` or `-3`, `"0%"`, `"101%"`,
`"+5%"`, `" 5%"`, a digit string longer than 9, a string of another shape, a boolean, a float, a
table) is invalid. A computed size above 4096 is 4096.

`cpus` is the machine-wide online CPU count:

| System | Source |
|---|---|
| macOS | the `hw.logicalcpu` sysctl |
| Linux | `/sys/devices/system/cpu/online`: a comma-separated list of `a` or `a-b` (`a ≤ b`) items, for example `0-3,8-11` (8 CPUs); the count is the number of CPUs the list covers |
| other systems | the system's machine-wide online processor count; a client that cannot obtain one treats it as a state error |

`cpus` is never a process's affinity mask or cgroup quota (for example Go's `runtime.NumCPU` is not
used). Clients MUST recompute sizes from the configuration and `cpus` at every admission attempt.

### 5.3 Configuration file

The configuration is `<home>/.config/coreslot/config.toml` (`<home>` as in section 4.1). Under the
`CORESLOT_STATE_DIR` override it is `$S/config.toml` instead, so state and configuration switch
together: a client MUST NOT pair an overridden state directory with the user's real configuration.
Under the override, `config.toml` is a permitted top-level entry of `$S`, checked as in section 4.2
(regular file, owned by the user, not a symlink, not group- or world-writable). The real
configuration file lies outside `$S` and is not checked that way: it is read through symbolic links
(dotfile managers link it), with no owner or mode check. A path that does not exist, including a
dangling symbolic link, reads as a missing file (the defaults above). A path that resolves to
anything but a regular file that can be read (a directory, a FIFO, a device) is an invalid
configuration, and a client detects that without blocking (it never waits on a FIFO).

The file is TOML 1.0:

```toml
[resources]
cpu = "-2"        # integer; "N%" of the machine-wide CPU count; or "-K"; default "100%"
pg-local = 2      # any declared resource

[priorities]      # label prefix -> priority; --priority overrides
"ci:" = 0
"interactive:" = 10

[platform]
windows_warning = false   # default true (section 16)
```

- `[resources]`: each key MUST be a resource name (section 5.1) and each value a size (section
  5.2). `cpu` is `"100%"` when not declared.
- `[priorities]`: each key is a label prefix and each value an integer in `[-1000, 1000]`. A request
  without an explicit priority takes the value of the longest prefix of its label; with no matching
  prefix its priority is 0. This applies where a request can leave its priority unset (the CLI
  without `--priority`); a library whose request always carries a priority MAY use that value as
  given.

  > **Implementation note.** The Go library uses `Request.Priority` as given and does not read
  > `[priorities]`; the Go CLI applies it when `--priority` is absent.
- `[platform]`: `windows_warning`, a boolean.
- `resources`, `priorities` and `platform`, when present, MUST be tables; any other value of one of
  them is invalid. Other tables and keys are ignored.

A missing file means `cpu = "100%"` and nothing else, with no notice. A file that is not valid TOML
or violates the rules above is an invalid configuration: a state error (section 14). Clients
re-read the file when its mtime changes, at the next admission attempt.

## 6. Tokens and descriptor ownership

Token files are `$S/res/<name>/<i>`, where `<name>` is a resource name and `<i>` an index in
`[0, 4095]` written in decimal without leading zeros (`0`, `1`, ..., `13`). A token file is created
on first use (`open` with `O_CREAT|O_NOFOLLOW|O_CLOEXEC`, mode `0600`, no `O_EXCL`) and never
deleted; its content is empty and never read. A holder holds a token by keeping `flock(LOCK_EX)` on
an open file description of it.

An entry of `res/` whose name is not a resource name, and an entry of `res/<name>/` whose name is
not an index written as above (`007`, `5000`, `x`), is not a token file: it is never probed,
counted, checked (section 4.2) or removed. "An index that exists" below means a token file at such
a name.

Ownership is by open file description, and release is by **closing**: a client MUST NOT release a
token, ticket or hold with `LOCK_UN`, because `LOCK_UN` releases the lock for every descriptor
sharing that description, including a child's. A run passes its grant's descriptors to the command
it starts (section 17) and keeps its own copies until the command exits, so the tokens stay held
while the run or any process that inherited the copies is alive. Processes that are not descendants
(a container started through a daemon) do not extend a grant.

A grant holds at most 256 tokens of one resource and at most 512 in total, so a run never needs more
than 513 inherited descriptors. A request whose minimums sum above 512 is invalid (a usage error,
section 14); admission (section 12.1) and adoption (section 12.4) never take more than 512 tokens in
total. A run SHOULD raise the soft `RLIMIT_NOFILE` it passes to the command
to at least the inherited count plus 256 when the hard limit allows.

The **locked count** of a resource is the number of its existing token files, at every index that
exists, whose probe fails with `EWOULDBLOCK`. Probing a token is permitted inside and outside the
section; a probe's momentary lock can only make another client see a token as locked, never as
free.

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

All numbers in names are decimal and zero-padded to their width, so the byte order of the
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
  whitespace. A writer MUST write every key of its table.
- A body is **malformed** when it is not such an object, exceeds 8 KiB, lacks a key of its table,
  has a key of the wrong type or outside its rule, or has a duplicate key at any depth. Keys are
  compared after JSON unescaping: `"c\u0070u"` duplicates `"cpu"`, so a reader that compares raw
  bytes is not conforming.
- Nesting depth alone never makes a body malformed: a reader MUST accept any depth that fits in
  8 KiB (about 4 096 levels).
- In particular, a body is malformed when a key inside `need`, `grant` or `clamped` does not match
  the resource-name grammar (section 5.1), when `grant` maps a resource to an empty array, or when
  a string anywhere in it contains an unpaired UTF-16 surrogate escape (a `\uD800`-`\uDBFF` escape
  not followed by a `\uDC00`-`\uDFFF` escape, or a `\uDC00`-`\uDFFF` escape not preceded by one).
- Unknown keys are ignored at any depth: bodies only gain optional keys within a format. The value
  of an unknown key may be any JSON value; the integer rule of section 1 applies only to the keys of
  a table, so `1.5`, `1e999` or a 30-digit number under an unknown key is accepted. (The duplicate,
  surrogate and size rules still apply inside unknown keys.)
- A malformed body makes its entry ignored and counted, never trusted. An ignored ticket takes no
  place in the queue.

### 7.3 Ticket

`$S/queue/<ticket>`, lock-carrying: its owner holds `flock(LOCK_EX)` on it for as long as it waits.
Body:

| Key | Type | Rule |
|---|---|---|
| `label` | string | at most 128 bytes in UTF-8; may be empty |
| `priority` | integer | `-1000..1000` |
| `need` | object | resource name → `[min, max]`, an array of exactly two integers, `1 ≤ min ≤ max ≤ 256`; 1 to 8 entries; the minimums sum to at most 512; names need not be declared (section 14) |
| `created_ns` | integer | `≥ 0`; the wall clock read for `arrival_ns` (section 9) |

### 7.4 Hold

`$S/holds/<hold>`, lock-carrying: the holder holds `flock(LOCK_EX)` on it for as long as it holds
its grant, and passes that descriptor to its command as fd 3 (section 17). Body:

| Key | Type | Rule |
|---|---|---|
| `label` | string | as in the ticket |
| `grant` | object | resource name → array of the token indices held, ascending, distinct, each in `[0, 4095]`; never an empty array (a resource of which no token is held is left out); at most 256 per resource and 512 in total; `{}` when no token is held |
| `start_ns` | integer | `≥ 0`; equal to the name's start-ns, and a body whose `start_ns` differs is malformed |
| `unbudgeted` | boolean | `true` for a run outside the budget: a fail-open run, or a run whose wait timed out and that runs anyway (section 14) |
| `unbudgeted_resources` | array of resource names | resources the request named that are not declared, or that a run outside the budget could not budget (section 14); writers list distinct names in ascending order, readers check neither |
| `clamped` | object | resource name → the requested `min`, an integer in `[1, 256]`, for each resource clamped (section 12.2); readers do not check that the resource appears in `grant` |
| `adopted` | boolean | `true` for an adoption (section 12.4) |

Comparing `start_ns` with the name needs the name, so it is done by the reader of `holds/`, not by
a body decoder. A start-ns in a name may be as large as 2^64 − 1 (section 7.1) while `start_ns` is
a signed 64-bit integer: a hold whose name carries a start-ns above 2^63 − 1 never has a
well-formed body and is ignored and counted. No writer produces one, because the wall clock is a
signed 64-bit value.

### 7.5 Cancel request

`$S/cancel/<cancel>`, not lock-carrying; its name is the target ticket's id. Body:

| Key | Type | Rule |
|---|---|---|
| `requested_ns` | integer | `≥ 0`; wall clock at publication |
| `requester_pid` | integer | `≥ 0`; the canceller's pid; informational |

### 7.6 FORMAT

`$S/FORMAT` holds the line `coreslot-protocol <n>` where `<n>` is 1 to 9 ASCII decimal digits, read
as a decimal number; leading zeros are allowed (`01` is format 1) and writers write none. Writers
write the line followed by one LF. Readers MUST accept the line with or without the final LF. Any
other content (a sign, more than 9 digits, a CR or other whitespace, a second line) is a malformed
`FORMAT`: a state error. A well-formed `<n>` other than 1, including 0, is a format this document
does not define (section 15.2). Section 15 defines what a client does with it.

### 7.7 admit.lock

`$S/admit.lock` is the admission lock (section 8). It is created once (`O_CREAT|O_NOFOLLOW`, mode
`0600`) and never deleted, renamed or replaced. Its body is informational: on entering the section
the holder SHOULD rewrite it in place through its locked descriptor (truncate, then write) as the
JSON object `{"pid": <integer>, "entered_ns": <integer>}`. Readers MUST NOT rely on it for anything
but display, and MUST tolerate an empty, stale or malformed body. A token-only client
(section 15.2) does not rewrite it.

### 7.8 gen

`$S/gen` is the JSON object `{"gen": <integer>, "reconciled_ns": <integer>}` (body rules of
section 7.2). A missing or malformed `gen` reads as `{"gen": 0, "reconciled_ns": 0}`. It is written
only inside the section and replaced with a plain `rename` (section 11). `gen` is an integer in
`[0, 2^63 − 1]`; a negative `gen` makes the file malformed. **Bumping** `gen` means writing it with
`gen` increased by one, or 0 when it was 2^63 − 1, and `reconciled_ns` unchanged.

A section bumps `gen` once when it publishes or removes a ticket or hold: enqueue (section 10.1),
admission (12.1), adoption (12.4), publication of an unbudgeted hold (14), and removal of the
client's own ticket after cancellation or withdrawal (13) or of its own hold (12.3); the resume bump
of section 10.2 is the one bump without such a change, and reconciliation increases `gen` when it
removed anything (10.4). Publishing a cancel request does not bump `gen` (the owner polls `cancel/`
itself), nor does writing `FORMAT`, `degraded`, `admit.lock` or the run log. A crash can separate a
removal from its bump, so a reader never relies on a bump to learn that an entry is gone (section
10.3).

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

## 8. The admission critical section

`admit.lock` is the protocol's linearization point. Enqueue, admission, cancellation publication,
adoption, token-only admission, reconciliation and run-log rotation MUST happen while holding
`flock(LOCK_EX)` on it; so does every other publication (section 11) and every removal of a
ticket, hold, cancel request or temporary file. A refresh (section 10.2), a token probe and a
run-log append happen outside it.

- **Entry is non-blocking.** A client tries `LOCK_EX|LOCK_NB` and retries with backoff from 10 ms
  up to 250 ms, checking its own deadline or cancellation between tries; it MUST NOT sleep inside
  `flock`. A single-attempt operation (a library `Poll`) makes one try. A client that cannot enter
  by its deadline gives up without having changed anything.
- **The section never waits** for anything but the filesystem: no sleeping for capacity, no lock
  other than non-blocking ones, bounded work.
- A client SHOULD prevent its own job-control stop while inside the section (the CLI ignores
  `SIGTSTP`, `SIGTTIN` and `SIGTTOU` there; a library never installs signal handlers). `SIGSTOP`
  cannot be ignored: a process stopped inside the section blocks every admission on the machine
  until it continues or dies. This is the one exception to I5. The CLI's stop is deferred, not
  lost: in non-terminal mode it forwards `SIGTSTP` to its command's process group and then stops
  itself, once outside the section, and forwards `SIGCONT` to that group when it is continued.
- On every entry, a client reads `FORMAT` before changing anything and acts on it as section 15
  says; when `FORMAT` is missing it writes `coreslot-protocol 1` (section 11).
- A crash inside the section releases the lock; every step is ordered so that a crash leaves either
  the old state or the new state plus dead files that reconciliation removes.
- Leaving the section is closing the `admit.lock` descriptor.

## 9. Order

Priority is an integer in `[-1000, 1000]`, default 0; higher runs first. Waiting ages a ticket by
one priority point per minute, which gives the same order as a static key:

```
order-key = arrival_ns − priority × 60 000 000 000      (ascending; ties by pid, then seq)
```

An order key computed below zero is 0.

`arrival_ns` is the wall clock read inside the enqueue section. A ticket is **ahead** of another
when its (order-key, pid, seq) is smaller. A wall-clock step misorders the tickets enqueued across
it by at most the size of the step and affects nothing else.

## 10. Liveness, waiting and reconciliation

### 10.1 Enqueue

Inside the section the client reads `arrival_ns`, computes the order key, publishes its ticket
(section 11) keeping the ticket's lock-carrying descriptor open, bumps `gen`, and leaves. From then
on, until admission, cancellation or withdrawal, it MUST keep that descriptor open and refreshed.

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
the longest a conforming waiter leaves between elections, the head's 2 s maximum backoff (section
10.3; the Go library likewise asks for a Poll at least every 2 s), so that a waiter electing at that
interval, with scheduling jitter on top, still watches.

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
  even when the examination ended before reaching them. The front window only keeps every
  waiter's timers on the front of the queue running; the election's rule is unchanged. A dead
  ticket that is not yet removed still takes a slot of the window until reconciliation removes it
  (section 10.4);
- checks `cancel/` for its own ticket id on every poll (section 13.1);
- as head, retries admission with backoff from 0.25 s up to 2 s, repeating the election (with a
  rescan when `gen` changed) before each attempt.

> **Implementation note.** The conformance traces' 22 s bound cannot tell a waiter that watches the
> front window from one that does not; the Go and Python unit tests enforce it.

Only a non-conforming actor removes a live ticket (reconciliation removes dead ones only). A waiter
whose own ticket's name disappears while it still holds the ticket's descriptor keeps waiting, and
the budget stays safe: it is counted from token locks, not tickets, so I3 and I7 still hold. The
waiter does lose its place, though. No other client sees the ticket any more, so waiters behind it
may be elected and admitted first, `status` does not show it, and it cannot be cancelled by label;
a cancel request naming its exact id may still reach it, but only from a canceller that publishes
without checking `queue/` first (section 13.1), and only until a reconciliation removes the request,
whose ticket name is gone (section 10.4, step 2). When it later removes its ticket
(admission, cancellation, withdrawal), `ENOENT` counts as success.

A waiting CLI prints one line every 30 s, for example `coreslot: waiting for cpu 4..12: position 2
(1m30s); holders: gate:unit pid 4711 cpu12 (3m12s), ci:o-r pid 5120 cpu3 (1m05s)`: the request, its
position, how long it has waited and the live holds with their grants and ages (informative).
The position is an upper bound: tickets beyond the one that ended the examination are counted
without being examined.

### 10.4 Reconciliation

Reconciliation removes dead files. It runs inside the section at most once per 30 s machine-wide: a
client that enters the section for any purpose other than admission and token-only admission
(enqueue, cancellation publication, removal of its own ticket or hold, a resume bump, adoption,
publication of an unbudgeted hold, a `degraded` write or removal, log rotation) and finds
`reconciled_ns` in `gen` older than 30 s on its wall clock (or in the future by more than 30 s) MUST
reconcile before its own change. Writing a missing `FORMAT` (section 8) is part of whichever entry
finds it missing and does not by itself make that entry reconcile. Admission and token-only
admission never reconcile, and neither does a reset (section 4.4). Reconciliation:

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

Inside the section, a publication of a ticket, hold, cancel request, `gen`, `FORMAT` or `degraded`:

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
   - `gen`, `FORMAT`, `degraded`: with a plain `rename`.

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

Inside the section the head:

1. checks `cancel/` for its own ticket id; if present, it is cancelled (section 13.1);
2. recomputes sizes (section 5.2) from the configuration and `cpus`;
3. computes the effective request of each requested declared resource (section 12.2);
4. counts, for each such resource, the tokens locked at **every** existing index (section 6);
   `free = size − locked`;
5. if any resource's `free` is below its effective `min`, leaves the section and waits. Otherwise it
   repeats the election (section 10.3) over a fresh listing of `queue/`, the **re-election**: every
   well-formed ticket ahead of it in order is probed, and if any is neither dead nor STALLED for
   this observer (its mtime from the listing counts as an observation), the head leaves the section
   and waits. A ticket the head has never observed, such as one published after its election,
   counts as live;
6. otherwise takes tokens with `LOCK_EX|LOCK_NB` at indices below the size, creating missing token
   files and skipping a token that turns out locked, in two passes, each in ascending resource name
   and then ascending index:
   1. the **minimum pass** takes the effective `min` of every resource; if any resource ends it
      below its effective `min` (a race with a non-queued taker), the head closes everything it
      took this round, leaves the section and waits;
   2. the **extra pass** takes further tokens of each resource, up to `min(effective max, free)`
      for that resource, while the grant holds fewer than 512 tokens in total;

   and checks I1 for every token it now holds: the held file is still linked (`st_nlink > 0`) and
   `(st_dev, st_ino)` of its path is the held file's. A mismatch (the token file was deleted or
   replaced) is a state error (section 14): the head closes every token it took and leaves the
   section without publishing anything. The check MAY be made per token as it is taken;
7. publishes its hold listing exactly the indices it holds, closes its ticket descriptor, unlinks its
   ticket, bumps `gen`, and leaves the section.

Resources the request names that the configuration does not declare are not budgeted: they take
no tokens, appear in the hold's `unbudgeted_resources`, and the run prints one notice line; this is
not degraded mode.

Taking every minimum first means the 512-token cap can never starve a later resource below its
minimum. For example, with sizes of at least 256 and everything free, `cpu=100..256`,
`db=100..256` and `gpu=100..256` take 100 each in the minimum pass; the extra pass then gives `cpu`
156 more (256) and `db` the remaining 56 (156), and `gpu` stays at 100: 512 in total.

A 1-token request on a free 8-token resource leaves 7 acquirable (I2). After a shrink from 14 to 12
with a live hold on indices 10-13, the next request sees `locked = 4`, `free = 8`, and can take at
most 8 tokens from indices 0-9 (I3).

### 12.2 Effective request and clamp

For each requested declared resource of size `s` with request `min..max`:

- when `min > s`, the effective request is `s..s` and the resource is **clamped**: it waits for the
  whole resource, runs alone on it, and its hold's `clamped` records the requested `min`;
- otherwise the effective request is `min..min(max, s)`.

A clamped CLI run prints a notice (`coreslot: cpu: requested 16, budget is 10; running alone with
10`), and its `{n}` is the clamped grant.

### 12.3 After the command exits

The run closes its copies of the grant's descriptors, the hold's before any token's, so that a
live hold never lists a token nobody locks (I1, I2). The CLI then enters the section if it can
within 2 s, probes its hold and unlinks it only if the probe succeeds (no descendant still holds
it), bumping `gen` when it did (section 7.8); otherwise reconciliation removes the hold later.
Other clients MAY remove their hold the same way, or leave it to reconciliation. This applies to
every hold a run published, including a fail-open run's unbudgeted hold (section 14).

### 12.4 Adoption

Adoption admits already-running work without queueing (for example containers that survived a pool
restart). Inside the section the adopter recomputes sizes, counts locked tokens as in step 4, and
takes tokens from what is free, under I3 (indices below the size, `free = size − locked`), in the
two passes of step 6: the minimum pass takes up to `min(min, size)` of each requested declared
resource (as many as are free), the extra pass takes more up to `max` per resource while the grant
holds fewer than 512 tokens in total. It checks I1 for the tokens it took, as in step 6, then
publishes a hold with `"adopted": true` listing exactly what it took, and bumps `gen` (section 7.8).
Adoption reconciles first when it is due (section 10.4). It has no minimum: the **shortfall** is
`max(0, min − granted)` per resource, reported to the caller, and the survivor runs partly
unbudgeted.

## 13. Cancellation and withdrawal

### 13.1 Cancellation

Cancelling never signals across processes. A canceller publishes `$S/cancel/<ticket-id>` inside the
section. It MAY first check, in that section, that `queue/` holds a ticket with that id and publish
nothing when it does not; a request whose ticket is gone is removed by reconciliation either way
(section 10.4, step 2). The owner checks `cancel/` for its id on every poll and again at admission
step 1, so a cancel published before the owner's admitting section wins; one published later finds
no ticket and has no effect: cancellation never revokes a grant. A canceller that finds the request
already published (section 11) treats the cancellation as already requested; that is not an error.
On finding it, the owner closes its ticket descriptor (the ticket is dead at once), removes the
ticket and the cancel request in its next section if it can enter one (bumping `gen`, section 7.8),
and stops: the CLI exits with the refusal code, a library returns its cancellation error.

> **Implementation note.** The Go CLI and library check `queue/` first and refuse, with a
> diagnostic that no such ticket is waiting, when there is none; the independent Python client in
> `conformance/pyclient` publishes without checking.

### 13.2 Withdrawal

A waiter whose own deadline passes, whose wait is abandoned by its caller, or that is interrupted
(`Ctrl-C` on a waiting CLI, which then exits 130) withdraws by closing its ticket descriptor: the
ticket is dead at once, and withdrawal never depends on entering the section. The waiter MAY then
remove its ticket in a section, bumping `gen` (section 7.8); otherwise reconciliation removes it.

## 14. Errors, fail-open and degraded mode

| Class | Examples | Outcome |
|---|---|---|
| usage | malformed need, `min > max`, `max > 256`, more than 8 resources, minimums summing above 512, duplicate resource, unknown flag | exit 64; nothing runs |
| refusal | a no-wait or fail-on-timeout request expiring, cancellation, invalid nested context (section 18), strict mode turning any of the next row into a refusal | the refusal code (section 19); nothing runs |
| state error | unusable directory, refused filesystem, unexpected file types or owners, directory full, malformed `FORMAT`, invalid configuration, an inode mismatch at a held token (I1) | fail-open unless strict |
| internal | the command cannot be started | exit 70, or 126 / 127 |

Two cases are deliberately **not** state errors: an undeclared resource (section 12.1) and an
unknown format (section 15.2).

**Fail-open** runs the command at once with `min` of each named resource, and:

- prints a diagnostic to stderr immediately, always;
- publishes a hold with `"unbudgeted": true`, an empty `grant` and every resource it names in
  `unbudgeted_resources` when the directory is usable and its format understood, so `status` shows
  it, and bumps `gen` in that section (section 7.8);
- records the reason in the run log and in `degraded` when those are writable, and says on stderr
  when they are not;
- exports `CORESLOT_UNBUDGETED=<reason>` to the command, so nested runs pass through (section 18);
- MUST NOT write anything (hold, run log, `degraded`) into a directory whose `FORMAT` it has not
  read and understood during the run; a malformed `FORMAT` is not understood.

The reason, in `CORESLOT_UNBUDGETED` and in `degraded`'s `reason`, is informational text that no
client interprets: section 18 acts on the variable being set, never on its value. Any non-empty
text conforms, except that a fail-open run MUST NOT use `format`, which marks a token-only run
(section 15.2).

A run whose wait timed out and that runs anyway (the CLI's `--on-timeout run`) withdraws its ticket
(section 13.2) and runs with `min` of each named resource outside the budget. It is not a state
error: it publishes the same unbudgeted hold, exports `CORESLOT_UNBUDGETED=<reason>` and records
the reason in its run-log line's `fail_open`, but it writes no `degraded` record. The same `FORMAT`
rule applies.

> **Implementation note.** The Go CLI's reason for such a run is `timeout`.

`status` reports degraded mode and exits 3 until `coreslot doctor --clear` removes the record.

## 15. Formats and token-only mode

### 15.1 What every format keeps

`FORMAT` holds `coreslot-protocol <n>`; `n` versions the metadata (names, bodies, and every part of
the layout other than `admit.lock` and `res/`). Format 1 fixes, for every future format:

- `admit.lock` is an exclusive `flock` held only briefly by whoever changes admission state;
- `res/<name>/<i>` are the token files, held by `flock(LOCK_EX)` and released by closing;
- free capacity is counted by probing token locks at every existing index (section 6).

A client that finds no `FORMAT` writes `coreslot-protocol 1` inside the section. Writing a newer
`FORMAT` is the business of the release that introduces it.

### 15.2 Token-only mode

A client that finds a format it does not implement runs in **token-only** mode:

- it enters the section, computes sizes from its own configuration, counts locked tokens as in
  section 12.1 step 4, and takes `min` (clamped as in section 12.2) of each declared resource if
  that many are free, in the order of step 6, and checks I1 for them as step 6 does; otherwise it
  leaves and retries with the head's backoff;
- it publishes nothing and writes nothing in the state directory (no ticket, hold, log, degraded
  record, `gen` bump or `admit.lock` body) except creating what the format-stable namespace
  (section 15.1) needs: missing token files, `res/` and `res/<name>/` for them (mode `0700`), and
  `admit.lock` when it is missing (`O_CREAT` without `O_TRUNC`). It never creates `queue/`,
  `holds/`, `cancel/` or `log/`. It prints a diagnostic and exports `CORESLOT_UNBUDGETED=format`;
- because every format counts locks, its tokens are counted by every other client (I6); it has no
  place in the queue, so ordering does not apply to it.

The mode is decided at every section entry from the `FORMAT` read there (section 8). A client in
token-only mode that, on a later entry before it holds tokens, finds a format it implements, or no
`FORMAT` (which it then writes, as every entry does), is no longer token-only: it enqueues in that
section as a new request (section 10.1, with a fresh `arrival_ns`) and waits in the queue, so it
never bypasses the queue of a format it understands (I4).

A waiting client reads `FORMAT` at every section entry and MAY also read it on its polls; one that
finds the format changed under it withdraws its ticket (section 13.2) and continues in token-only
mode. It only closes the ticket's descriptor: removing the file would be a write into a format it
does not implement, so the file is left to a client of that format.

Every other operation that would write follows the same rule. Under a format it does not implement,
a client publishes no cancel request, adoption hold or unbudgeted hold, records and clears no
`degraded`, and does not remove its own hold after a run. A fail-open run still runs its command
but writes nothing.

> **Implementation note.** The Go CLI and library refuse cancellation, adoption, `doctor --clear`
> and `doctor --reset` under a format they do not implement.

## 16. Windows

The CLI runs the command without a budget and prints one warning per run; `[platform]
windows_warning = false` silences it; strict mode refuses instead. A library's open operation
returns its unsupported error; no other library function is reachable on Windows.

## 17. Descriptor and environment contract

A run that starts a command with a grant gives it:

| Descriptor | Content |
|---|---|
| fd 3 | the hold's lock-carrying descriptor, when the run has a hold; otherwise fd 3 is closed in the child and the tokens still start at fd 4 (a Go launcher passes a nil `ExtraFiles[0]`) |
| fd 4 onward | the token descriptors, in ascending resource name and then ascending index |

Descriptors 3 through 3 + n of the command, n being the number of token descriptors, are the
run's: an inherited descriptor at one of those numbers is not passed on, so a launcher that
needs its own descriptors in the command places them above.

and this environment:

| Variable | Value |
|---|---|
| `CORESLOT_GRANTED_<RES>` | for each resource the request names, in environment form (section 5.1): the number of tokens granted, or, for a resource run unbudgeted, its requested `min` |
| `CORESLOT_HELD_FD` | `3`, when the run passes a hold descriptor |
| `CORESLOT_TOKEN_FDS` | the token descriptors' numbers, when it passes any |
| `CORESLOT_UNBUDGETED` | the reason, for a fail-open, token-only (`format`) or unbudgeted pass-through run |

```
token-fds = item *( "," item )
item      = fd [ "-" fd ]       ; "a-b" is every fd from a to b inclusive, a <= b
fd        = 1*DIGIT
```

Writers usually write a single range (`4-13`); a single descriptor may be written `4` or `4-4`, and
both conform. Readers MUST accept the whole grammar. A list that
names one descriptor twice, directly or through overlapping ranges, is invalid: a pass-through
treats it as a context that does not validate (section 18). Every
descriptor the run itself opens under `$S` outside this contract stays close-on-exec (section 4.2).
A launcher that starts nested runs passes these
descriptors through (Python: `subprocess.run(..., pass_fds=...)`; Go: `exec.Cmd.ExtraFiles`).

A run that is not a pass-through (section 18 rules 1 and 2) builds this contract from its own grant
only. An ordinary or `--independent` request, a fail-open run and a token-only run remove every
inherited `CORESLOT_GRANTED_*`, `CORESLOT_HELD_FD`, `CORESLOT_TOKEN_FDS` and `CORESLOT_UNBUDGETED`
from the command's environment before setting their own, and close (do not pass on) exactly the
inherited descriptors those inherited variables name: the `CORESLOT_HELD_FD` descriptor and every
descriptor `CORESLOT_TOKEN_FDS` lists. Otherwise a grandchild would pass through to a grant that is
not its parent's. A named descriptor that is a standard stream (0, 1 or 2) or not a regular file
cannot be a grant's (its number was reused after a launcher closed the original), and a client
SHOULD leave it open. Every other inherited descriptor (a jobserver pipe, `LISTEN_FDS` sockets, the
user's redirections) is passed through untouched, and `CORESLOT_STATE_DIR` is not grant context and
is passed on.

> **Implementation note.** The Go CLI closes only the named descriptors that are regular files at
> fd 3 or above.

**Placeholders** (CLI): an argument that is exactly `{n}` (or `{n:RES}`) is replaced by the grant of
`cpu` (or of `RES`); `{{n}}` is a literal `{n}` and `{{n:RES}}` a literal `{n:RES}`; with
`--subst`, placeholders inside arguments are replaced too (`make -j{n}`). The grant of a resource
run unbudgeted is the value of its `CORESLOT_GRANTED_<RES>`. A placeholder naming a resource the
request does not name (`{n}` in a request without `cpu`, `{n:RES}` for another `RES`) is a usage
error, detected before the request queues.

## 18. Nested runs: pass-through

A run decides its context from the environment it inherited ("set" as defined in section 1: an
empty variable is absent), in this order:

1. `CORESLOT_UNBUDGETED` is set (and the run is not `--independent`): it runs unbudgeted with its
   `min` of each resource, prints a notice, passes the variable on, and writes no degraded record
   of its own.
2. `CORESLOT_TOKEN_FDS` is set (and the run is not `--independent`): it is a **pass-through**. The
   context is valid when no descriptor is listed twice, every descriptor listed is open, its
   `(st_dev, st_ino)` equals that of some `$S/res/<name>/<i>`, and a fresh probe of that path fails.
   Two listed descriptors with the same `(st_dev, st_ino)` (a launcher duplicated one) hold one
   token, not two: such a context does not validate.
   A valid pass-through does not queue, enter the section or lock anything. Per resource it names,
   it runs with `min(max, held)`, where `held` is the number of valid token descriptors of that
   resource, counted from the descriptors and not from the environment; a resource it names that the
   parent does not hold is listed as unbudgeted with a notice; a `min` above what the parent holds
   is clamped with a notice. It passes the parent's descriptors on unchanged and sets
   `CORESLOT_GRANTED_<RES>` to the narrowed value for each resource it names. A context that does
   not validate (a launcher closed the descriptors, or the parent is gone) is a refusal with a
   diagnostic naming the likely cause; the run never queues behind a parent that may hold the whole
   budget. A list naming more than 512 descriptors does not validate (a grant holds at most 512
   tokens; a reader checks the count before expanding a range), and neither does a context the run
   cannot check because `$S`, `res/` or a token file cannot be read.
3. Otherwise, or with `--independent`, it is an ordinary request.

A pass-through, valid (rule 2) or unbudgeted (rule 1), enters no section and writes nothing under
`$S` but its run-log line (section 20): no ticket, hold, `gen` bump or degraded record.

Nested runs are **not** budgeted against each other: two siblings started together under an 8-token
grant both run with 8. A parent that wants concurrent children partitions its grant itself.

## 19. Exit status

| Status | Meaning |
|---|---|
| child's own | the command ran (`128 + signal` when killed) |
| 130 | interrupted while waiting (`Ctrl-C`) |
| 75 | refusal (section 14); `--refusal-code` changes it for commands whose own 75 matters |
| 64 | usage error |
| 126 / 127 | the command is not executable / not found |
| 70 | internal error before the command could run |
| 3 | `status` and `doctor`: degraded mode |
| 1 | `doctor`: it found a problem (an unusable directory, filesystem or configuration) |

## 20. Run log

Each run SHOULD append one JSON line of at most 4 KiB to `$S/log/runs.jsonl` with a single
`O_APPEND` write (outside the section), into a directory whose `FORMAT` it has read and understood
during the run (section 14). `FORMAT` may change while the command runs, so the writer reads it
again, outside the section, immediately before the write, and writes no line when it is absent,
malformed or names a format the writer does not implement. A pass-through (section 18), valid or
unbudgeted, appends too: that read is its only one (it never writes a missing `FORMAT`, and then
writes no line), it marks its line `"nested": true`, and it never rotates the log. A token-only run writes no log. A line that
would exceed 4 KiB MAY be fitted by shortening an informative string; a writer that cannot fit it
writes no line. The object carries:

| Key | Type | Content |
|---|---|---|
| `time_ns` | integer | wall clock at the run's start |
| `label` | string | the request's label |
| `need` | object | resource → `[min, max]` as requested |
| `grant` | object | resource → number of tokens granted (or `min` when unbudgeted) |
| `clamped` | array of resource names | clamped resources |
| `unbudgeted_resources` | array of resource names | unbudgeted resources |
| `waited_s` | number | seconds spent waiting |
| `ran_s` | number | seconds the command ran |
| `exit` | integer | the run's exit status |
| `load1` | number | one-minute load average at start |
| `fail_open` | string | the fail-open reason, or empty |
| `nested` | boolean | optional; `true` for a pass-through run (section 18), absent or `false` otherwise |

The log MUST NOT contain argv or the environment; labels are recorded, so they must not carry
secrets. The log's `clamped` is an array of resource names, unlike the hold body's `clamped` object
(section 7.4). A client that finds `runs.jsonl` at or above 10 MiB before appending rotates it
inside the section: re-checks the size, then renames it to `runs.1.jsonl` (replacing the previous
generation). One generation is kept. A token-only client writes no log.

> **Implementation note.** To fit a line into 4 KiB the Go CLI shortens `fail_open` and the
> independent Python client shortens `label`.

## 21. Worked examples

All examples use one Linux machine: `/sys/devices/system/cpu/online` is `0-13` (`cpus = 14`), the
configuration declares `cpu = "-4"` (size 10) and `pg-local = 2`, and `$S` is
`/home/ana/.local/state/coreslot`. `FORMAT` is `coreslot-protocol 1` unless stated. Listings show
the entries under `$S` that matter; `[L]` marks a file whose lock is held by a live process.
`T0` is 2026-10-11T12:00:00Z, `1791720000000000000` ns. Each example is independent of the others;
except in section 21.6, `reconciled_ns` in `gen` is less than 30 s old when the example starts, so
reconciliation is not due. The writers in these examples number ticket and hold names and their
temporaries from one counter and the temporaries of `gen`, `FORMAT` and `degraded` from another;
any numbering conforms (section 7.1).

The examples use the `coreslot` CLI, which is not part of the protocol: `coreslot run
[--need RES=MIN..MAX]... [--label L] [--priority P] [--timeout D] [--on-timeout run|fail]
[--no-wait] [--strict] [--independent] [--subst] [--refusal-code N] -- COMMAND...`. `RES=N` means
`N..N`; with no `--need` the request is `cpu=1..1`; `--on-timeout` defaults to `fail`, so a
`--timeout` that expires refuses; strict mode is `--strict`.

### 21.1 Enqueue

Pid 4711 runs `coreslot run --need cpu=4..12 --label gate:unit -- pytest -n {n}` at `T0`. The
ticket of pid 5120 (label `ci:o-r`, priority 10, enqueued at 11:59:05) is already waiting.

Before:

```
gen                                                   {"gen": 41, "reconciled_ns": 1791719990000000000}
queue/01791719345000000000-0000005120-000007-ci_o_r   [L]
```

Pid 4711 enters the section (rewrites `admit.lock` with `{"pid": 4711, "entered_ns": ...}`), reads
`FORMAT`; reconciliation is not due (10 s old). `arrival_ns = 1791720000000000000`, priority 0, so
the order key is `01791720000000000000`. It creates `queue/.tmp-0000004711-000001`, locks it,
writes the body, renames it without replacement, bumps `gen`, and leaves.

After:

```
gen                                                    {"gen": 42, "reconciled_ns": 1791719990000000000}
queue/01791719345000000000-0000005120-000007-ci_o_r    [L]
queue/01791720000000000000-0000004711-000001-gate_unit [L]
```

Body of the new ticket:

```json
{"label": "gate:unit", "priority": 0, "need": {"cpu": [4, 12]}, "created_ns": 1791720000000000000}
```

The ticket of pid 5120 sorts first: its key is its arrival (11:59:05) minus 10 × 60 s.

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

### 21.3 Admission with clamp

Pid 7300 runs `coreslot run --need cpu=16..16 --need pg-local=1 --label gate:big -- ...` and is
head at 12:10:00. Token files 0-13 of `cpu` exist from before the size dropped from 14 to 10; none is
locked. `pg-local` has only token 0.

Before:

```
gen                                                   {"gen": 57, "reconciled_ns": ...}
queue/01791720540000000000-0000007300-000003-gate_big [L]
res/cpu/0 ... res/cpu/13                              (14 files, none locked)
res/pg-local/0
```

Inside the section: no `cancel/01791720540000000000-0000007300-000003`; sizes `cpu = 10`,
`pg-local = 2`; `cpu` `min = 16 > 10`, so the effective request is `10..10`, clamped; `pg-local` is
`1..1`. Locked counts: `cpu` 0 over 14 indices (`free = 10`), `pg-local` 0 (`free = 2`). The
re-election (section 12.1 step 5) lists `queue/` again and finds no ticket ahead of its own. It
takes `cpu` 0-9 and `pg-local` 0, publishes `holds/.tmp-0000007300-000004` as the hold below,
closes and unlinks its ticket, bumps `gen`, and leaves.

After:

```
gen                                                   {"gen": 58, "reconciled_ns": ...}
holds/01791720600000000000-0000007300-000004-gate_big [L]
res/cpu/0 ... res/cpu/9                               [L] (held by pid 7300)
res/cpu/10 ... res/cpu/13                             (not locked)
res/pg-local/0                                        [L]
```

```json
{"label": "gate:big", "grant": {"cpu": [0,1,2,3,4,5,6,7,8,9], "pg-local": [0]},
 "start_ns": 1791720600000000000, "unbudgeted": false, "unbudgeted_resources": [],
 "clamped": {"cpu": 16}, "adopted": false}
```

The CLI prints `coreslot: cpu: requested 16, budget is 10; running alone with 10` and starts the
command with fd 3 = the hold, fds 4-13 = `cpu` tokens 0-9, fd 14 = `pg-local` token 0, and
`CORESLOT_GRANTED_CPU=10`, `CORESLOT_GRANTED_PG_LOCAL=1`, `CORESLOT_HELD_FD=3`,
`CORESLOT_TOKEN_FDS=4-14`.

### 21.4 Cancellation

Pid 8000 runs `coreslot cancel 01791719345000000000-0000005120-000007` at 12:01:00.

Before:

```
gen                                                   {"gen": 44, "reconciled_ns": ...}
queue/01791719345000000000-0000005120-000007-ci_o_r   [L]
cancel/                                               (empty)
```

Pid 8000 publishes, inside the section, `cancel/01791719345000000000-0000005120-000007` with
`{"requested_ns": 1791720060000000000, "requester_pid": 8000}`. Publishing a cancel request does
not bump `gen` (section 7.8).

After publication:

```
gen                                                   {"gen": 44, "reconciled_ns": ...}
queue/01791719345000000000-0000005120-000007-ci_o_r   [L]
cancel/01791719345000000000-0000005120-000007
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

### 21.7 Adoption

At 12:30:00 a restarted CI pool (pid 6001) adopts a surviving job container with need
`cpu=3..3`. A live hold holds `cpu` 0-7.

Before:

```
gen                                                    {"gen": 80, "reconciled_ns": ...}
holds/01791721500000000000-0000007400-000002-gate      [L] (grant: cpu 0-7)
res/cpu/0 ... res/cpu/7                                [L]
res/cpu/8, res/cpu/9                                   (not locked)
```

Inside the section, without a ticket: size 10, locked 8, `free = 2`. It takes `cpu` 8 and 9,
publishes the hold and bumps `gen`; the shortfall `max(0, 3 − 2) = 1` is returned to the caller.

After:

```
gen                                                    {"gen": 81, "reconciled_ns": ...}
holds/01791721500000000000-0000007400-000002-gate      [L]
holds/01791721800000000000-0000006001-000012-ci_o_r    [L]
res/cpu/0 ... res/cpu/9                                [L]
```

```json
{"label": "ci:o-r", "grant": {"cpu": [8, 9]}, "start_ns": 1791721800000000000,
 "unbudgeted": false, "unbudgeted_resources": [], "clamped": {}, "adopted": true}
```

### 21.8 Token-only admission

`FORMAT` contains `coreslot-protocol 2`. Pid 9100, which implements only format 1, runs
`coreslot run --need cpu=2..6 -- ...`. `cpu` 0-3 are held by format-2 clients.

Before:

```
FORMAT                                                 coreslot-protocol 2
res/cpu/0 ... res/cpu/3                                [L] (format-2 holders)
res/cpu/4 ... res/cpu/9                                (not locked)
```

Pid 9100 enters the section, does not rewrite `admit.lock`, computes size 10 from its own
configuration, counts 4 locked (`free = 6`), takes `min = 2`: tokens 4 and 5, and leaves. It writes
no ticket, hold, `gen`, log or degraded record. It prints a diagnostic and starts the command with
fds 4-5 (no fd 3), `CORESLOT_GRANTED_CPU=2`, `CORESLOT_TOKEN_FDS=4-5` and
`CORESLOT_UNBUDGETED=format`.

After (no file was created, renamed or rewritten; only locks changed):

```
FORMAT                                                 coreslot-protocol 2
res/cpu/0 ... res/cpu/3                                [L] (format-2 holders)
res/cpu/4, res/cpu/5                                   [L] (pid 9100)
res/cpu/6 ... res/cpu/9                                (not locked)
```

### 21.9 Pass-through

Under the run of section 21.3 (fd 3 = hold, fds 4-13 = `cpu` 0-9, fd 14 = `pg-local` 0,
`CORESLOT_TOKEN_FDS=4-14`), the command starts
`coreslot run --need cpu=2..4 --subst -- make -j{n}` (`--subst`, because `{n}` is inside the
argument `-j{n}`; section 17).

The nested run checks fds 4-14: each is open, each matches the inode of a token file
(`res/cpu/0` ... `res/cpu/9`, `res/pg-local/0`), and a fresh probe of each path fails. `held` for
`cpu` is 10, so it runs with `min(4, 10) = 4`. It starts `make -j4` with the same descriptors and
`CORESLOT_GRANTED_CPU=4`; `CORESLOT_GRANTED_PG_LOCAL=1`, `CORESLOT_HELD_FD=3` and
`CORESLOT_TOKEN_FDS=4-14` are passed on unchanged.

A pass-through does not enter the section or lock anything. After `make` exits it reads `FORMAT`
(`coreslot-protocol 1`, understood) and appends one run-log line marked `"nested": true`
(section 20); apart from that line, `$S` is unchanged before and after.

## 22. Timing constants

| Constant | Value |
|---|---|
| ticket refresh | every 2 s |
| STALLED | 10 s without an observed mtime change, on the observer's monotonic clock |
| `gen` read and election | every 1 s |
| front window | the first 4 tickets of the queue, watched in every election |
| observer pass gap | more than 3 s between an observer's last examination and its next election is not watched |
| head admission retry | backoff from 0.25 s up to 2 s |
| section entry retry | `LOCK_NB`, backoff from 10 ms up to 250 ms |
| reconciliation | at most once per 30 s machine-wide; temporary files older than 30 s |
| hold removal after exit | section entry within 2 s, else left to reconciliation |
| waiting line (CLI) | every 30 s |
| run-log rotation | at 10 MiB, one generation |
