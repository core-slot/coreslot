# coreslot

[![CI](https://github.com/core-slot/coreslot/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/core-slot/coreslot/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/core-slot/coreslot?sort=semver)](https://github.com/core-slot/coreslot/releases/latest)
[![License: MIT](https://img.shields.io/github/license/core-slot/coreslot)](LICENSE)

Several coding agents on one laptop each run their own test suite, type check or build, and each of
those sizes itself to the whole machine. Together they use more memory than the machine has: it
swaps, the work slows to a crawl, and in the end the machine freezes or the kernel kills something
you cared about. On the Claude Code tracker, a Mac running a workflow of many agents ended with its
swap full ([anthropics/claude-code#101193](https://github.com/anthropics/claude-code/issues/101193)).

`coreslot` is a memory guard for those heavy commands. Put it in front of one:

```sh
brew install core-slot/tap/coreslot
```

```sh
coreslot run -- go test ./...    # starts when the machine has the memory for it
coreslot status                  # what runs, what waits, whether the watchdog is protecting
```

- **It starts a command when the memory is there.** A command starts when the memory the machine
  can really hand out, less what running commands are still expected to grow into and a small
  reserve, covers what that command typically needs, learned from its earlier runs. Otherwise it
  waits in one first-in, first-out queue. When no coreslot command is running, the head of the queue
  always starts.
- **It stops the newest commands only under real danger.** A little swap is survivable, so it does
  not kill for a full memory. It kills when the machine is paging and coreslot's own commands are
  losing a large share of their time to it, or when swap is about to run out
  ([exact conditions](#when-it-kills)), and it starts with the newest. It restarts each one by itself
  once memory is calm.
- **It never touches what it did not start.** It kills only the process group of a command that its
  own `coreslot run` started and has not yet reaped.
- **There is no daemon, no root and no setup.** State is a directory of files and `flock(2)` locks,
  so a killed run never leaks its reservation, and a client written from
  [`PROTOCOL.md`](PROTOCOL.md) shares the same budget.

**For coding agents.** A [Claude Code plugin](integrations/claude-code/README.md) refuses a bare
`go test ./...`, `cargo test` or `pytest -n 8` once and offers it back as
`coreslot run -- <the same command>`:

```text
/plugin marketplace add core-slot/coreslot
/plugin install coreslot@coreslot
```

Other agents get the same rule as a line in their instructions
([the snippet](integrations/claude-code/README.md#other-coding-agents)).

**Status:** `v0`. macOS on Apple silicon is the supported platform; Linux and Intel Macs are
experimental, and Windows runs commands unbudgeted. The watchdog's defaults are interim until
calibration, and the CLI may still change before `v1.0.0`: the
[compatibility policy](docs/compatibility.md) says how.

## When you do not need coreslot

- One agent, or work that never overlaps, on a machine with plenty of memory.
- Work that runs elsewhere (CI, a cloud sandbox) and never shares your laptop.
- A tool that already queues its own heavy work and is the only one that does.

## Why not ...

| Instead of coreslot | Good at | Where it stops |
|---|---|---|
| cgroup memory limits (`systemd-run -p MemoryMax=`) | A hard cap the kernel enforces | A command over its cap is killed, not queued; Linux only. Pair one with coreslot for a hard cap: coreslot decides when to start, the cap bounds what one command may take. |
| `earlyoom`, `systemd-oomd` | Keeping a Linux machine alive when memory runs out | They kill whatever their policy picks, anywhere on the machine, with no queue and no restart. |
| `make -j` and its jobserver | Sharing job slots inside one build tree | Each top-level build has its own pool, and memory is not counted. |
| GNU `sem`, a semaphore script | Simple, and fine when one slot count fits every command | A fixed slot count for all commands: it does not know what a command needs or how much memory is free. |
| Running agents in the cloud | Not sharing a laptop at all | Do it where you can; coreslot is for work that stays on your machine. |

## How it decides

**Admission.** A command's need is `--mem`, else the `mem` of its label in
[`needs.toml`](#configuration), else the 90th percentile of the memory peaks of the label's last 20
runs, else the smaller of 4 GiB and a quarter of the machine's memory. The room is the free memory
(on macOS free, speculative, purgeable and file-backed pageable pages; on Linux `MemAvailable`),
less the reserve, less what each running command still has to grow into (its need less what it uses
now). A command starts when its need fits the room. A need larger than the machine's memory less the
reserve runs only alone. Waiting runs say why and where they stand when they begin to wait and every 30 s; a tool with
a time limit of its own can expire first, so run coreslot in the background or raise the limit.

**Labels are automatic.** Without `--label`, the label is the command's shape (its executable and
up to two leading word-like arguments: `go test`, `npm run test`) and the name of the git repository
it runs in, such as `go test@proj`. The worktrees of one repository share it, so a label's history
and its `needs.toml` entry follow the project, not the directory.

### When it kills

Every running `coreslot run` measures how much of its command's time goes to memory work: system
time, plus page-wait time where macOS reports it, as a share of the command's running time, beyond
what that label normally spends. One shared state turns those measurements into a level, only while the
kernel is actually paging:

| Level | When | Effect |
|---|---|---|
| yellow | the commands lose noticeably more time to memory than their normal | nothing new starts; starts resume one at a time after it clears |
| red | they lose a large share of it for tens of seconds | a stop round: the newest commands are killed until their footprints cover the shortfall |
| floor | swap is nearly out of room and still growing | the same stop round, at once |

A stopped command is killed with `SIGKILL` and put back in the queue at its **first arrival**, so it
keeps its place; it starts again once memory has been calm for a while and there is room for it
with a margin. After the third stop it gives up with the refusal code and prints `coreslot: gave up
after 3 memory stops`.

Never stopped: the oldest running command, a command whose label has `no_restart = true` in
`needs.toml`, a command that shares coreslot's process group (a run in a terminal, `--same-group`,
or a run nested inside another run's reservation) and anything coreslot did not start. A command that exits `SIGKILL` for another reason is reported
and not restarted. The thresholds are `[watch]` keys with interim defaults; see
[`PROTOCOL.md` section 23](PROTOCOL.md#23-the-watchdog) for the exact rules and
[the configuration](#configuration) to change them. `[watch] mode = "observe"` runs everything but
the kill and says what it would have stopped.

On Linux the run also raises its own `oom_score_adj` to 500, so the kernel's out-of-memory kill
takes a restartable coreslot command before your desktop.

## Install

With Homebrew, from a tap whose formula builds from source
([`packaging/homebrew/coreslot.rb.tmpl`](packaging/homebrew/coreslot.rb.tmpl)):

```sh
brew install core-slot/tap/coreslot
```

With Go 1.26 or newer:

```sh
go install github.com/core-slot/coreslot/cmd/coreslot@latest
```

Or download a release archive (macOS and Linux, amd64 and arm64) from the
[releases page](https://github.com/core-slot/coreslot/releases). Each release carries
`checksums.txt` and a build-provenance attestation for every archive:

```sh
shasum -a 256 -c checksums.txt --ignore-missing
gh attestation verify coreslot_<version>_darwin_arm64.tar.gz --repo core-slot/coreslot
tar -xzf coreslot_<version>_darwin_arm64.tar.gz
```

The binaries are not signed with an Apple Developer ID. Binaries installed with `go install` or
Homebrew are not quarantined, but an archive downloaded with a browser is: if macOS refuses to run
the extracted binary, clear the attribute once:

```sh
xattr -d com.apple.quarantine ./coreslot
```

`coreslot version` prints the version and the protocol formats it implements. A state directory of
another format is not used: runs start unbudgeted with a notice until `coreslot doctor --reset`.

## Usage

```sh
coreslot run -- pytest -n 8                     # label "pytest@proj", need learned from earlier runs
coreslot run --mem 3G -- make test              # say what it needs when it has no history
coreslot run --label gate:unit -- npm test      # a label of your own
coreslot status            # the watchdog's level, running commands, the queue; --json for scripts
coreslot stats             # per label, what the need is learned from
coreslot doctor            # state directory, filesystem, permissions, format, readings
coreslot cancel <ticket-id>   # or: coreslot cancel --label L; withdraws a waiting request
```

`--no-wait` refuses at once instead of queueing, `--timeout D` bounds each wait (it never stops a
running command), and `--on-timeout run` runs the command without the budget when the timeout
passes; a restart that waits past it refuses. The command receives `CORESLOT_GRANTED_MEM` (its
need, in MiB) and inherits the descriptor that holds its reservation
([`PROTOCOL.md` section 17](PROTOCOL.md#17-descriptor-and-environment-contract)).

**Nested runs are pass-through.** A `coreslot run` started inside another run's reservation does not
queue: it runs inside its parent's, with `min(need, parent's)`, and its memory counts in the
parent's footprint. `--independent` queues it as an unrelated request. Either way its command stays
in its parent command's process group, so stopping the parent stops it too.

**Stop a run by killing its process group.** Without a terminal, `run` starts its command in a
process group of its own and relays signals to it, which a SIGKILL of coreslot's group cannot do:
the command would live on holding the reservation. `coreslot run --same-group` keeps the command in
coreslot's group, so a `killpg` reaches the whole tree (a command there is never a stop-round
victim). A SIGINT or SIGQUIT sent to coreslot's pid alone is then dropped and a SIGTERM or SIGHUP
reaches only the command's pid, as in a terminal: signal the group, not coreslot's pid.

**Run a pipeline under one `coreslot run`.** The stages of `a | b` run at the same time: write
`coreslot run -- sh -c 'a | b'` so that they are one command with one need.

### Exit status

A run exits with its command's own status. Otherwise 75 means refused (`--no-wait`,
`--on-timeout fail`, cancellation, `--strict`, giving up after 3 stops; `--refusal-code N` changes
it), 64 a usage error, 130 `Ctrl-C` while waiting, 126 / 127 a command that is not executable / not
found, and 70 an internal error. `status` exits 3 while runs fail open, and `doctor` 1 when it finds
a problem. [`PROTOCOL.md` section 19](PROTOCOL.md#19-exit-status) has the full table.

When the state directory cannot be used (a network filesystem, wrong permissions, an invalid
configuration), `run` fails open: it runs the command at once, says so on stderr, records degraded
mode, and exports `CORESLOT_UNBUDGETED`. `--strict` refuses instead.

## Configuration

Every key is optional. `~/.config/coreslot/config.toml` (the home directory comes from the password
database, not `$HOME`):

```toml
[memory]
reserve = "1G"        # memory kept free beyond every need

[watch]
mode = "enforce"      # "observe": everything but the kill
sample_log = false    # true: log every machine sample to log/watch.jsonl
```

`[watch]` also takes the thresholds and waits
([`PROTOCOL.md` section 5.3](PROTOCOL.md#53-configuration-file)); their defaults are interim until
calibration.

`needs.toml`, beside it, pins what a label needs and spares it from stop rounds. A label with a
space or an `@` is not a bare TOML key, so quote it:

```toml
[needs."go test@proj"]
mem = "3G"            # a pin: learning never replaces it

[needs."gate:docker"]
no_restart = true     # a stop round never kills this label's commands
```

The state lives in `~/Library/Application Support/coreslot/` on macOS and `~/.local/state/coreslot/`
on Linux. `CORESLOT_STATE_DIR` overrides it for tests and moves the configuration to
`$CORESLOT_STATE_DIR/config.toml` with it: an override is a separate budget.

## What it is not

- **Not enforcement.** coreslot is cooperative admission control. It caps nothing: a command may use
  more than its need, and a tool that never goes through `coreslot run` is neither queued nor
  stopped. Work you run outside it is your own priority. A hostile process of the same user can
  corrupt the state ([SECURITY.md](SECURITY.md)).
- **Not a daemon.** Nothing runs between commands; there is no network and no distribution of work
  across machines.
- **Not multi-user.** One budget per user; never share the state directory across users or hosts.
- **Not for Windows.** coreslot builds there, but runs every command at once, without a budget,
  with a warning.

A `coreslot run` inside a container sees the container's filesystem and therefore its own budget.
Containers take part through a client on the host. The memory a run measures is its process tree's,
so a command whose work happens in a container or on another host looks small to it.

## More

- [Protocol](PROTOCOL.md), the normative definition of format 2
- [Compatibility policy](docs/compatibility.md) and [changelog](CHANGELOG.md)
- [Calibration](calibration/README.md), where the watchdog's defaults come from
- [Running a command on another machine under its budget](docs/remote.md)
- [Contributing](CONTRIBUTING.md), [security](SECURITY.md), [code of conduct](CODE_OF_CONDUCT.md)
- [Third-party licenses](THIRD_PARTY_LICENSES.md)

coreslot sends no telemetry. MIT licensed ([LICENSE](LICENSE)).
