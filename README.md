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
coreslot run --restart=yes -- go test ./...   # starts when the machine has the memory for it
coreslot status                                # what runs, what waits, whether the watchdog is protecting
```

- **It starts a command when the memory is there.** A command starts when the memory the machine
  can really hand out, less what running commands are still expected to grow into and a small
  reserve, covers what that command typically needs, learned from its earlier runs. Otherwise it
  waits in one first-in, first-out queue. A command that fits may pass the head of the queue when
  its earlier runs say it will be done before the head could start, or, while the head does not fit
  and the kernel reports normal memory pressure, out of turn: one at a time, and for no longer than
  it has itself watched the head wait. A command with no earlier run never passes. The exception is
  container work: while another command runs, a command named `docker`, `docker-compose` or
  `podman` with no pinned need passes the queue, with or without
  earlier runs ([how it decides](#how-it-decides)). When no coreslot command is running, the head of
  the queue always starts.
- **It stops the newest commands only under real danger.** A little swap is survivable, so it does
  not kill for a full memory. It kills when the machine is paging and coreslot's own commands are
  losing a large share of their time to it, or when swap is about to run out
  ([exact conditions](#when-it-kills)), and it starts with the newest. It restarts each one by itself
  once memory is calm.
- **It never touches what it did not start.** It kills only the process group of a command that its
  own `coreslot run` started and has not yet reaped.
- **There is no daemon, no root and no setup.** State is a directory of files and `flock(2)` locks,
  so a killed run never leaks its reservation, and a client written from
  [`PROTOCOL.md`](PROTOCOL.md) shares the same budget. On macOS a run's Docker helper may outlive
  its command while that command's background Docker connections stay open
  ([Docker on macOS](#how-it-decides)).

**Measured.** On a 16 GB MacBook Air, four agents started a VS Code type check (twice), FastAPI's
tests and SQLAlchemy's ORM tests at once, five times unwrapped and five times under `coreslot run`,
in alternating order. Unwrapped, the machine sat at memory pressure warn for 66 to 115 s of each set
and wrote 8 to 13 GiB to swap; under coreslot it never left normal and wrote none, and the agents'
mean time to a verdict was 0.41 of the unwrapped one (median over the pairs). Both held in 5 of 5
pairs against a criterion committed before them. Against the best fixed semaphore, one command at a
time, coreslot was within 5 % in three of five pairs, not five (median 1.02, worst 1.27: it lets
two suites that each use every core run together when both fit in memory). With room for all the
commands it stayed within 5 % of unwrapped in five of five pairs (median 1.005, worst 1.036), a
narrow result that one slow unwrapped set carries. A third, overnight run on the v0.4.1 release
asked all of it again with more pairs, each claim registered before its sets: coreslot beat
unwrapped in 10 of 10 pairs (K/F 0.40), was within 5 % of the semaphore of one in 23 of 24 pairs
over all 24 arrival orders (median 1.003), and within 5 % of unwrapped with room in 20 of 20 (median
1.000, worst 1.012); the watchdog demo met its four checks in five of five. One machine, one stand
([the runs, their records and their limits](evidence/air-v04/README.md)).

**For coding agents.** A [Claude Code plugin](integrations/claude-code/README.md) refuses a bare
`go test ./...`, `cargo test` or `pytest -n 8` once and offers it back as
`coreslot run --restart=yes -- <the same command>`:

```text
/plugin marketplace add core-slot/coreslot
/plugin install coreslot@coreslot
```

Other agents get the same rule as a line in their instructions
([the snippet](integrations/claude-code/README.md#other-coding-agents)).

**Status:** `v0`. macOS on Apple silicon is the supported platform; Linux and Intel Macs are
experimental, and Windows runs commands unbudgeted. The watchdog's yellow and red points were chosen
on recorded runs of two Macs and its other defaults are not calibrated ([what the records
show](calibration/README.md#what-the-defaults-rest-on)); the CLI may still change before `v1.0.0`:
the [compatibility policy](docs/compatibility.md) says how.

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
[`needs.toml`](#configuration), else the 90th percentile of what the label's last 20 counted runs
held ([`PROTOCOL.md` section 20](PROTOCOL.md#20-run-log)): the level a run's memory held for about
10 s, at most 1 GiB under its peak, or its peak for a run shorter than that. Brief peaks are
therefore not reserved: the watchdog acts when pressure lasts, a stopped run comes back with its
label's peak reserved, and a label pinned in `needs.toml` reserves its full peak, which `coreslot
stats` prints. Else 1 MiB for a command named
`docker`, `docker-compose` or `podman`, else the smaller of 4 GiB and a quarter of the machine's memory. The room is the free memory
(on macOS free, speculative, purgeable and file-backed pageable pages; on Linux `MemAvailable`),
less the reserve, less what each running command still has to grow into (its need less what it uses
now). A command starts when its need fits the room. A need larger than the machine's memory less the
reserve starts only when no other coreslot job runs. Waiting runs say why, and queued ones where they stand, when they begin to wait and every 30 s; a tool with
a time limit of its own can expire first, so run coreslot in the background or raise the limit.

**Container work.** A container's processes are not in the command's tree, so the run's footprint
leaves them out (on macOS a forwarder reads them apart, below). Container work is a command named `docker`, `docker-compose` or `podman`, or a run whose tree
holds one or, on macOS, talks to a socket named `docker.sock`. A program named `docker` counts
whatever it is, a wrapper script of your own included. While another command runs, a command so
named waits only on the machine's real memory: it passes the queue and ignores what running commands
still have to grow into, but it waits below the reserve and at the yellow level and above. With none
running it queues like any other, so the head of the queue goes first. A `mem` pin in `needs.toml`
or `--mem` reserves and queues as usual. No container work is stopped to free memory. coreslot
cannot see inside a VM, so it does not prevent a container being killed there, and after its work a
VM's memory can stay out of the host's free memory for a long time, which later commands wait on.
A run marked from its tree starts like any other, and is only spared from then on. On Linux coreslot
goes by process names alone. It does not see a client that connects by TCP, a Podman library client
on macOS, a call shorter than a second, or a client outside the run's tree. So an indirect command
whose only Docker call is a quick `docker run -d` stays ordinary work and can be stopped: give it
`--restart=no`, or `no_restart` in `needs.toml`.

**Docker on macOS.** With Docker Desktop, a run whose label has a Docker interest (a command named
`docker`, `docker-compose` or `podman`, an earlier run of its label that did container work, or
`docker_forward = true` in `needs.toml`) gets a *forwarder*: a small `coreslot forward` process that
relays its command's Docker traffic and notes which containers the command creates or touches. The
command's `DOCKER_HOST` names the forwarder's socket in `~/.coreslot-fw/`. Every 6 s the run reads
the memory of those containers, and `status` shows what they have grown by since the run first
touched them, as the hold's `docker footprint`. Such a run is container work from its first Docker
connection, a library's included. A forwarder outlives its command while the command's background
Docker connections stay open (`docker compose logs -f &`), and `coreslot doctor` lists it. A
`DOCKER_HOST` that names another engine gives none, and so does a selected context other than
`default` or `desktop-linux` (`docker context use`, `DOCKER_CONTEXT`), unless you set `DOCKER_HOST`.
A run without a forwarder runs its command untouched and, with a Docker interest, says why once;
`status` shows the reason as the hold's `docker:` note. `docker_forward = false` turns the forwarder
off for a label, and `[docker] enabled = false` for every run; a run you turned it off for says
nothing.

Under a forwarder a few things look different:

- `docker context show` prints `default`, `docker context ls` warns that `DOCKER_HOST` overrides the
  context, and buildx names its builder `default` rather than `desktop-linux`, with the same driver.
- A context the command selects during the run is not used by its later calls, since
  `DOCKER_HOST` wins over it.
- A command that reads `$DOCKER_HOST` sees a path in `~/.coreslot-fw`.
- If the forwarder is killed, the run points the address straight at Docker at once, but the
  streams open at that moment end, as they would if Docker restarted.

No forwarder sees image builds, containers the daemon starts by itself, or work sent with an
explicit `-H` or `--context`, or by a library configured elsewhere than `DOCKER_HOST` (a
testcontainers `tc.host` line, say): that work stays unattributed. A container can bind-mount the
forwarder's socket only when its path is at most 96 bytes, so a user name longer than 45 bytes, or a
home directory whose path has a space, gets no forwarder. Java Docker libraries have not been tested
with it.

**Waiting for Docker's VM.** Give a label a Docker need, `docker_mem = "4G"` in
[`needs.toml`](#configuration), and each of its runs that gets a forwarder waits until that much
fits inside Docker Desktop's VM, as well as its own need on the Mac: the VM's memory less what its
running containers use, less 3 GiB or a quarter of the VM if that is smaller, less what the
containers of running such runs have yet to take and what such runs waiting ahead of it ask for. A
run without a forwarder ignores its Docker need. While it waits, the run reads Docker's socket at
most once every 2 s, plus one immediate retry when a running hold has just sampled its containers,
and only when it would otherwise start on the Mac; after 8 readings in a row that fail (Docker quit
or restarting, its VM not found), or once the need can never fit the VM, it waits for the Mac alone
and says so when it starts. A run that fits on the Mac and waits only for the VM shows `waits for
Docker's VM` in `status`, and every other run passes it: a `go test` or a `docker compose down`
behind it starts. Once started, its hold reserves on the Mac the growth its containers may still
cause the VM, up to twice what they have yet to reach of the need on Apple's hypervisor and one
and a half times on Docker VMM, and never more than the VM can still grow. `status` shows each
hold's `docker_mem`, its `docker footprint` and what is `reserved on the Mac for Docker's VM`, and
each waiting run's `docker_mem`. Docker Desktop's VM is read on Apple's hypervisor and on Docker
VMM alike. `--mem` and `mem` stay the memory of the command's own processes: on a command named
`docker`, a pin reserves that on the Mac and makes it queue as usual, so a pin meant as the
containers' memory belongs in `docker_mem`.

What the reservation does not hold back:

- It is held from a run's start, not tested before it: a run with a Docker need starts on its own
  need on the Mac even when the Mac has no room left for the growth. Later runs leave that room,
  but what goes beyond it shows as memory pressure, and since no stop round stops container work,
  the watchdog stops other commands to make up for it.
- Growth caused by Docker work without a Docker need, and by the VM's file cache, is not reserved.
- A run whose containers never reach its need (a stack that was already up, a need set too large)
  keeps up to twice the need reserved (one and a half times on Docker VMM) for as long as it runs,
  on a VM that has not grown to its size.
- On Docker VMM the VM gives memory back to the Mac as its containers free it, so a hold reserves
  for as long as its containers have not filled its need, and for up to 6 s after the VM gives
  memory back its reservation can be too low. The factor 1.5 does not cover a transient that briefly
  touches more, which Docker VMM gives back within about 2 s, nor the guest's cache creeping above
  it after a step. On Apple's hypervisor a VM grown to its size reserves nothing more.
- Runs behind one that waits for the VM may take Mac memory it needs. Room in the VM goes to
  waiting runs in arrival order, so containers outside coreslot that keep the VM full hold every
  such run; only `--timeout` bounds that.

**Under an agent sandbox.** A sandboxed run can make and reach the forwarder's socket only where the
sandbox admits `~/.coreslot-fw`. For the Claude Code sandbox, add it beside the Docker socket the
sandbox already allows, with your own home directory in place of `/Users/ana`:

```json
"sandbox": {
  "network":    { "allowUnixSockets": ["/Users/ana/.coreslot-fw"] },
  "filesystem": { "allowWrite":       ["/Users/ana/.coreslot-fw"] }
}
```

Without it, a run with a Docker interest notes `no-forwarder: address` when it cannot make
`~/.coreslot-fw`, or `no-forwarder: start` when its forwarder cannot make or bind its socket there,
and runs its command untouched.

**Labels are automatic.** Without `--label`, the label is the command's shape (its executable and
up to two leading word-like arguments: `go test`, `npm run test`) and the name of the git repository
it runs in, such as `go test@proj`. The worktrees of one repository share it, so a label's history
and its `needs.toml` entry follow the project, not the directory.

A command behind a launcher (`caffeinate`, `env`, `nice`, `nohup`, `stdbuf`, `time`, `timeout`) takes
the shape of the command it runs, found past the launcher's arguments that start with a digit or
contain `=` and taken again when it is a launcher too: `timeout 10m npx jest` is `npx jest@proj`,
`env A=1 timeout 10m npm run check` is `npm run check@proj` and `timeout 10m ./scripts/x.sh` is
`x.sh@proj`. Those skipped arguments never reach the label. A launcher followed by an option
(`nice -n 10 make`, `time -p make`), an empty argument or no command keeps its own name; give such
commands a `--label`.

The label is only that shape and name, so commands that share them share one learned need. Two cases
to know. First, the shape ends at the first argument that starts with `-` or a digit, or contains
`/`, `=` or `.`, so a command run by an interpreter or a shell collapses to its label:
`python -m pytest` and `python -m mypy` both become `python@proj` (`python3` gives `python3@proj`),
and `sh -c '…'` becomes `sh@proj`. Give such commands a `--label` of their own. Second, the
repository part is the directory name only, not its path, so two repositories with the same
directory name share their automatic labels. A `--label` separates them; a `needs.toml` key on the
shared label pins one need for both.

### When it kills

Every running `coreslot run` measures how much of its command's time goes to memory work: system
time, plus page-wait time where macOS reports it, as a share of the command's running time, beyond
what that label normally spends. One shared state turns those measurements into a level, only while the
kernel is actually paging:

| Level | When | Effect |
|---|---|---|
| yellow | the commands lose noticeably more time to memory than their normal | nothing new starts; starts resume one at a time after it clears |
| red | they lose a large share of it for tens of seconds | a stop round: the newest commands are killed until their footprints cover how far free memory is under the reserve |
| floor | swap is nearly out of room and still growing | the same stop round, at once, and it may take a command that is still learning |

A command whose tree holds under 128 MiB adds nothing to the share of time lost to memory, so it never
makes the level yellow or red by itself.

A red round may spare its newest victim when it is nearly finished and stop the next newest instead,
if that one alone covers what the round must free, holds at least 128 MiB, and has done less CPU
work. Nearly finished means
a command whose label has at least 10 earlier runs, whose CPU time so far, against the median CPU
time of those runs, says it will end within the label's typical run time (slowed by the share of its
recent time it lost to memory). A later round spares it again only before the time that first
estimate gave and while the estimate keeps shrinking. The run says so:
`coreslot: kept … (memory red, about 9 s left, 80% done, until 12:00:10)`. A floor round spares
nothing.

A stopped command is killed with `SIGKILL` and put back in the queue at its **first arrival**, so it
keeps its place; it starts again once memory has been calm for a while and there is room for it
with a margin. After the third stop it gives up with the refusal code and prints `coreslot: gave up
after 3 memory stops`. After a floor stop the run also tells the agent to free memory outside
coreslot before the command comes back.

Never stopped: the oldest running command, a command run with `--restart=no`, a command whose
label has `no_restart = true` in `needs.toml` (which wins over `--restart=yes`), a command that shares coreslot's process group (a run in a terminal, `--same-group`,
or a run nested inside another run's reservation), container work (a command named `docker`,
`docker-compose` or `podman`, or a run seen using one), a run outside the budget and anything
coreslot did not start. A stop kills the command's process group: a process that left it (`setsid`, a build
daemon, a dev server) survives and may still hold memory or a port when the command comes back. A
command that exits `SIGKILL` for another reason is reported and not restarted. A `--no-wait` run
whose command is stopped refuses instead of restarting it. The thresholds are `[watch]` keys; [the calibration
notes](calibration/README.md#what-the-defaults-rest-on) say what recorded runs of two Macs show about
each default, and which are not calibrated. See
[`PROTOCOL.md` section 23](PROTOCOL.md#23-the-watchdog) for the exact rules and
[the configuration](#configuration) to change them. `[watch] mode = "observe"` runs everything but
the kill and says what it would have stopped. `coreslot watch observe` or `coreslot watch enforce` switches
the mode on this machine without touching the configuration, until `coreslot watch config`, or for a while
with `--for 2h`; `status` says which mode applies and where it comes from. A stop round already written
still acts after a switch to observe.

A command is judged only once a run of its label has completed and measured that. Until then coreslot does
not know what is normal for it (the median share its earlier runs measured, the same way, while the
kernel was not paging), so it does not count towards the level and no red round
stops it; only a floor round does, after every command that has a normal share. A run shorter than
one sampling period (2 s), or one during which the kernel paged all along, measures no share and
leaves its label learning. A learning command still holds its memory, and `status` says the
watchdog is learning and how many running commands have no normal share yet.

On Linux the run also raises its own `oom_score_adj` to 500, so the kernel's out-of-memory kill
takes a coreslot command before your desktop; a `--restart=no` run, a label with
`no_restart = true` and a command named `docker`, `docker-compose` or `podman` keep their score.
The floor needs swap to grow, so on a host without swap it never fires and the kernel's
out-of-memory killer acts instead, and the paging gate opens on any rise of the memory stall, which
is nearly every period. A label's normal share is measured only while the gate is closed, so on
Linux most labels stay learning: red rounds stop almost nothing there, and only the floor acts,
which needs swap. Linux is experimental and its defaults are not calibrated.

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
another format is not used: runs start unbudgeted with a notice until `coreslot doctor --reset`,
which also removes that format's run log, so every label learns its memory need again. On a
directory of this client's format, or of format 3 or 4 (which the reset upgrades), the reset is a repair
and keeps the run log; to make the labels learn again,
remove `log/runs.jsonl` and `log/runs.1.jsonl` from the state directory.

To uninstall, remove the binary, the state directory and `~/.config/coreslot/` and, on macOS,
`~/.coreslot-fw/` beside the state directory.

## Usage

```sh
coreslot run --restart=yes -- pytest -n 8                 # label "pytest@proj", need learned from earlier runs
coreslot run --restart=yes --mem 3G -- make test          # say what it needs when it has no history
coreslot run --restart=yes --label gate:unit -- npm test  # a label of your own
coreslot run --restart=no -- ./migrate.sh                 # never killed and run twice
coreslot status            # the watchdog's level, running commands, the queue; --json for scripts
coreslot status --docker   # also Docker Desktop's VM and each running container, read from the Docker socket
coreslot stats             # per label, what the need is learned from
coreslot report            # the last 7 days: runs, waits, memory stops, heaviest labels
coreslot doctor            # state directory, filesystem, permissions, format, readings, forwarders still serving
coreslot cancel <ticket-id>   # or: coreslot cancel --label L; withdraws a waiting request
coreslot start <ticket-id>    # or: coreslot start --label L; starts a waiting job now, out of turn
coreslot move <ticket-id> --first   # or --last, --before <id>, --after <id>; changes its place, keeps its run
coreslot trim <hold-id>       # or: coreslot trim --label L; a running job reserves what it uses
```

**`--restart=yes|no` is required** wherever coreslot could stop the command: when memory runs
short it kills the newest commands and runs them again from scratch. `--restart=yes` is for a
command that is safe to kill at any moment and run again (tests, builds, linters, type checks);
`--restart=no` for anything that changes the world outside its own output (databases,
migrations, deploys, pushes, sending messages, writing shared files), which is then never
stopped. When unsure, choose no. Without it such a run refuses with a usage error (exit 64) that
says so. Where the command is never stopped anyway (a run in a terminal, `--same-group`, a run
nested inside another run's reservation, a run outside the budget) the flag is not required;
there `--restart=no` only keeps the Linux `oom_score_adj` the run would otherwise raise, and
`--restart=yes` changes nothing. A command named `docker`, `docker-compose` or `podman` needs no
flag and does not honour `--restart=yes`: a stop would kill the client and leave its container
running. With no pin and no history it needs 1 MiB. `sh -c 'docker run ...'` is not such a command:
only the name of the command itself counts.

`--no-wait` refuses at once instead of queueing, `--timeout D` bounds all the waiting of the run,
restarts included, counted from its start (it never stops a running command), and
`--on-timeout run` runs the command without the budget when the timeout passes; a restart that
waits past it refuses. The command receives `CORESLOT_GRANTED_MEM` (its
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
`coreslot run --restart=yes --label gate:pipe -- sh -c 'a | b'` so that they are one command with one need. The
`--label` matters here: without it the pipeline is labelled `sh@proj`, like every other `sh -c`.

**Lower a running job's reservation by hand.** A job reserves what it asked for until it ends,
even when it uses far less, and the queue waits on what it may still grow into. `coreslot trim
<hold-id>`, with the id `coreslot status` shows at the end of the job's line, asks the job's own
`coreslot run` to reserve what the job has used lately (its largest reading of the last minute, or
of `[watch] trim_window_s`) plus a margin of a tenth of its need, at least 256 MiB, instead of what
it asked for. A job younger than that window keeps what it asked for until it is that old, and the
reservation rises again as the job grows, never above what it asked for. It raises the room and
nothing more: it does not change the queue's order, and nothing is admitted past the room. The
job's run must be a coreslot that knows the request; an older one never answers, and `trim` says
so. It is your call and your risk: if the job grows back and memory runs short, a stop round stops
the newest restartable job, which may be another one. There is no undo: a trim holds until the
job's attempt ends, and a job that is stopped and restarted comes back untrimmed, with its full
need. For a container command the reservation follows the client's own memory, not its
containers'. Exit status 75 means nothing was written; `trim` also exits 0 when the run never
answers. Any process of yours can trim any of your jobs, and the job's own session learns of it
from the one line its `coreslot run` prints. `status` marks such a job `trimmed` and shows
`(asked …)` beside what it reserves; `status --json` has each hold's `id`, `asked_mib` and
`trimmed`.

**Start a waiting job now.** `coreslot start <ticket-id>` (or `--label L`) starts a waiting job at
once, ahead of the queue and whatever memory is free: its own `coreslot run` acts on the request at
its next poll, and `start` waits up to 8 s for it (`--no-wait` returns at once). Its full need still
counts, so the room may go below zero and other jobs wait until memory frees up; if memory runs out,
the watchdog may stop it as it would any job that queued when it did, and a job stopped that way
comes back as an ordinary waiting job. The job's run must be a coreslot that knows the request; an
older one never answers, and `status --json` then marks the request `unanswered` after 10 s. Exit
status 75 means nothing was written. `status` marks such a job `started now, out of turn`, and
`status --json` has each hold's `forced` and the pending `start_requests`.

### What it did: `coreslot report`

`coreslot report [--days N] [--json]` reads the run log of the last N days (7 by default) and
prints what was measured: the runs, their labels and attempts (a stopped command's restart is a
new attempt of the same run); the attempts that waited 1 s or more, with the total, median, p90 and
longest wait, and how many waited over 2 and over 10 minutes, where an agent's tool call may time
out (Claude Code's Bash tool waits 2 minutes by default, 10 at most); the attempts stopped for
memory and the run time lost to those stops, and the runs that gave up; and the heaviest labels by
memory peak, with their need, run time and wait; a label with container work is marked, since its
peak and need leave out its containers. `--json` prints the same as one document (schema 1). Lines that do not decode, lie in the future or hold an impossible value are skipped
and counted.

What the runs would have done without coreslot is not measured, so the report claims no time
saved; it points to the [results measured on one MacBook Air](evidence/air-v04/README.md).

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
sample_log = false    # deprecated, use [telemetry]; true: machine samples only
trim_window_s = 60    # 2 to 3600: the seconds of use a trimmed job's reservation follows

[telemetry]
enabled = false       # true: keep a local record of what each run waited for

[docker]
enabled = true        # false: never read the Docker socket; no forwarder, no Docker need
```

`[watch]` also takes the thresholds and waits
([`PROTOCOL.md` section 5.3](PROTOCOL.md#53-configuration-file)); [the calibration
notes](calibration/README.md#what-the-defaults-rest-on) say which defaults the records support.

`[telemetry] enabled = true` keeps a record, on this machine only, of what each run waited for and
of where the machine's memory was, in `telemetry/` under the state directory. It is off by default.
coreslot sends nothing anywhere and opens no network connection itself; looking up the home
directory may contact a directory service such as LDAP ([SECURITY.md](SECURITY.md)). While the
record is on, a run doing container work reads each container's memory from the local Docker
socket, a unix socket, about every 30 s. [The keys](docs/telemetry.md) say what each line
means, what the record holds and how large it grows. `sample_log` is deprecated: alone, it writes
the machine samples and nothing else.

Besides the record, coreslot talks to the Docker socket, always a unix socket on this machine, in
three ways. On macOS every run with a forwarder relays whatever connects to the forwarder's socket,
and whatever is still open after its command, and reads its own containers' memory every 6 s, and
a run with a Docker need reads Docker's VM while it waits, at most once every 2 s plus one immediate
retry ([Docker on macOS](#how-it-decides)). `coreslot status --docker` reads it
only when you ask: once per call, read-only, within 2 s. It shows Docker Desktop's VM and, for every running
container, its name, image, Compose project and memory, to whoever runs it as you, so a process of
yours can learn what your containers are called. `[docker] enabled = false` makes it print `off` and
open no socket, keeps the record from reading the engine too, and gives no run a forwarder. A value that is not a boolean
switches it off as well, and `status` and `doctor` say so. It reads only Docker Desktop, on Apple's
hypervisor or Docker VMM; OrbStack and Colima are not supported, and for them it says
`not_running`.

`needs.toml`, beside it, pins what a label needs and spares it from stop rounds. A label with a
space or an `@` is not a bare TOML key, so quote it:

```toml
[needs."go test@proj"]
mem = "3G"            # a pin: learning never replaces it

[needs."gate:docker"]
no_restart = true     # never stopped by a stop round, so never restarted; wins over --restart=yes
docker_forward = true # macOS: always give its runs a forwarder; false: never
docker_mem = "4G"     # macOS: its runs wait for this much room in Docker's VM
```

The state lives in `~/Library/Application Support/coreslot/` on macOS and `~/.local/state/coreslot/`
on Linux. `CORESLOT_STATE_DIR` overrides it for tests and moves the configuration to
`$CORESLOT_STATE_DIR/config.toml` with it: an override is a separate budget.

## What it is not

- **Not enforcement.** coreslot is cooperative admission control. It caps nothing: a command may use
  more than its need, and a tool that never goes through `coreslot run` is neither queued nor
  stopped. Work you run outside it is your own priority. A hostile process of the same user can
  corrupt the state ([SECURITY.md](SECURITY.md)).
- **Not a daemon.** Nothing runs between commands, except a forwarder still serving a finished
  command's background Docker connections; there is no network and no distribution of work across
  machines.
- **Not multi-user.** One budget per user; never share the state directory across users or hosts.
- **Not for Windows.** coreslot builds there, but runs every command at once, without a budget,
  with a warning.

A `coreslot run` inside a container sees the container's filesystem and therefore its own budget.
Containers take part through a client on the host, as container work
([how it decides](#how-it-decides)). The memory a run measures is its process tree's, so a command
whose work happens in a container or on another host looks small to it.

## More

- [Protocol](PROTOCOL.md), the normative definition of format 5
- [Compatibility policy](docs/compatibility.md) and [changelog](CHANGELOG.md)
- [Calibration](calibration/README.md), the harness, the records and what each default rests on
- [Running a command on another machine under its budget](docs/remote.md)
- [Contributing](CONTRIBUTING.md), [security](SECURITY.md), [code of conduct](CODE_OF_CONDUCT.md)
- [Third-party licenses](THIRD_PARTY_LICENSES.md)

coreslot sends nothing anywhere and opens no network connection itself; its optional local record is off
by default ([Configuration](#configuration)). MIT licensed ([LICENSE](LICENSE)).
