# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as the
[compatibility policy](docs/compatibility.md) applies it: the module stays at `v0` until
`v1.0.0`, which freezes the on-disk format then current and the CLI surface.

## Unreleased

## v0.4.1 (2026-11-02)

### Changed

- README: the section on automatic labels now says that commands behind one launcher (`python -m`,
  `sh -c`, `env`, `timeout`) can share the launcher's label, and that repositories with the same
  directory name share theirs; `--label` separates both (a `needs.toml` key on a shared label pins
  one need for all of them). The `sh -c 'a | b'` advice carries a `--label`.
- `coreslot doctor --reset` of a format-2 directory, or of one without `FORMAT`, is a repair and
  keeps the run log, which every label's learned memory need and normal stall share come from;
  only a reset of another format or of an unreadable `FORMAT` removes it, and says that every
  label learns again.

### Fixed

- The watchdog's red bucket keeps only fresh evidence: it is emptied when a reading comes more than
  20 s after the last one that could be read, or before it, instead of when a sleep or a reboot
  was guessed from the clocks. A bucket written long ago no longer survives a long pause without a
  sleep, and a reading that could not be read neither empties it nor clears the 20 s window.
- A need larger than any moment can hold says the bound it exceeds, the machine's memory less the
  reserve, not the machine's memory.
- The stop line shows a share below 1 % to one decimal (`0.3%`), not as `0%`.

## v0.4.0 (2026-11-02)

coreslot is a memory guard for the heavy commands of coding agents; memory is its only resource.
Its on-disk protocol is format 2.

### Upgrading

A state directory of another format is not used: runs start unbudgeted with a notice (`--strict`
refuses) until `coreslot doctor --reset` recreates it as format 2. Every client of a machine runs
the same release. The watchdog's yellow and red points were chosen on recorded runs of two Macs;
no other default is calibrated ([what the records show](calibration/README.md#what-the-defaults-rest-on)).

### Added

- Admission on real free memory: a command starts when the memory the machine can hand out, less
  what running commands are still expected to grow into and a small reserve, covers its need. The
  need is learned from the label's earlier runs, else the smaller of 4 GiB and a quarter of the
  machine's memory.
  When no coreslot command is running, the head of the queue always starts.
- Passing by duration: while the head of the queue waits for room, or a restarted command for
  calm, a command behind it that fits starts first when its label's earlier runs typically end
  before the head is expected to start. A command with no earlier run never passes.
- A watchdog shared by every `coreslot run`. It measures how much of the commands' time is lost to
  memory beyond their normal share while the kernel is paging; it holds new starts when that is
  noticeable, and under real danger kills the newest commands, never the oldest, one round at a
  time. Each killed command is restarted by its own `coreslot run` at its first arrival in the
  queue, after memory has been calm for a while; after the third stop it gives up with the refusal
  code and the line `coreslot: gave up after 3 memory stops`. The `[watch]` configuration table sets
  the mode (`enforce` or `observe`), the thresholds and the waits; `observe` logs what it would stop.
  A label is not judged until one of its runs has measured its normal share, over the ticks where
  the kernel was not paging (a run shorter than one tick, or paged throughout, measures none): until
  then its commands do not count towards the level and no red round stops them; only a floor round
  does, after every command whose label has one. A round frees the memory the
  machine is short, not what other commands have reserved and not yet used. A command a floor round
  stopped is told the whole machine ran out of memory and to free some outside coreslot.
- Labels are automatic: without `--label`, a run is labelled with the command's shape and its
  repository, such as `go test@proj`, and the worktrees of one repository share the label. A
  `needs.toml` table such as `[needs."go test@proj"]` needs TOML quoting.
- `--mem SIZE` on `coreslot run`; `mem` and `no_restart` in a `needs.toml` entry (`no_restart` spares
  a label from every stop round); `[memory] reserve` in the configuration.
- `CORESLOT_HOLD`: a `coreslot run` inside a command's reservation runs inside it without queueing,
  unless `--independent`.
- On Linux a run raises its own `oom_score_adj` to 500, unless its label has `no_restart = true`,
  so the kernel's out-of-memory kill takes a coreslot command before the desktop.
- `calibration/`: the harness that scores the watchdog's thresholds on runs of real machines, its records, and a measurement of the watchdog's own cost.
- A Claude Code plugin, `integrations/claude-code`: a `PreToolUse` hook that refuses a bare heavy
  test or build command once and offers the same command back as `coreslot run -- <command>`. The
  repository is its marketplace: `/plugin marketplace add core-slot/coreslot`, then
  `/plugin install coreslot@coreslot`.

### Changed

- The state directory is format 2. A hold is the reservation: it carries the command's need, its
  latest footprint and its time deltas in its body, and admission computes the room from the hold
  bodies. The queue is ordered by arrival. [`PROTOCOL.md`](PROTOCOL.md) defines it.
- The run log records memory peaks, `user_s`, `sys_s`, `tick_share` and `stops`, and `coreslot stats`
  shows the window the need is learned from. What is normal for a label is learned from the same
  measure the watchdog compares it with: the median `tick_share` of its earlier runs.
- Linux and Intel Macs are experimental.
