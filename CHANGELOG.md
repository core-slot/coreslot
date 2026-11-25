# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as the
[compatibility policy](docs/compatibility.md) applies it: the module stays at `v0` until
`v1.0.0`, which freezes the on-disk format then current and the CLI surface.

## Unreleased

### Added

- A long head lets short jobs go first: a head whose label typically runs 10 min or more, and that could start now,
  lets the jobs behind it that typically run 2 min or less, and would not fit beside it, start first, until a
  deadline of a quarter of its typical run time (10 min for a 40-min head), which it publishes in the new
  `yield.json`. It starts by the deadline, or once the short jobs still running end; a short job that runs past its
  typical time, or 3 s with nobody taking the room, ends the yield at once. A head never yields twice, nor when it is a
  restart, was moved, cannot read the headroom or the kernel's pressure is not normal (so never on Linux). Tickets
  carry their label's `typical_s`. `status` reads "letting short jobs go first until 14:32"; `status --json` gains the
  top-level `head` and `yield`, under the same `schema` 1; telemetry gains the `waiting` reason `yielding` with
  `until_ns`, `yield_ended`, the `short` pass and its `no_pass` reasons.
- Two marks on a job, set with `coreslot quiet <id>` and `coreslot no-stop <id>` on a waiting or running job and
  removed with `--off`. While a job marked quiet runs, no other job is admitted (a forced start still is); the
  window ends when the job ends, however it ends, or with `--off`. A job marked do-not-stop is never a stop round's
  victim. Neither moves a job in the queue. An `--independent` run started inside a quiet job runs inside its
  parent's grant instead of waiting for the window. `status` shows the marks, and `status --json` each hold's and
  ticket's `quiet` and `no_stop`; the protection reason covers the floor too. Telemetry gains the `waiting` reason
  `quiet` and the `pass_through` reason `quiet`.
- A command with a learned need stops reserving its whole need after its peak: once it has run for its label's
  window, its hold reserves the most it used within that window plus a margin, never more than its need, and rises
  again with its use. The window is 1.5 times the longest gap a run of the label needed, from its last 20 runs, at
  least 120 s and at most a day; 600 s until 3 runs have recorded one. Each run's line in the run log gains
  `mem_window_s`, and `coreslot stats` prints each label's window. A pinned need, a run with a Docker interest, an
  oversized need and a restart keep their whole need.

### Changed

- **Format 7.** Every client must apply one rule for what a live hold reserves (below), since a run may now lower
  its `need_mib` without a request, so a client of format 6 steps out of the budget with its existing notice. Run
  `coreslot doctor --reset` once nothing is live: on a format-6 to format-3 directory it is an upgrade that keeps the
  run log and the telemetry.
- What a hold reserves has one definition: its `need_mib` while its run measures it, and at least its `asked_mib`
  once its command has exited or it has not been sampled for 6 s, so a lowered reservation nobody measures no longer
  stands. The room, the head's expected wait, `status`'s `reserved_mib` and each hold's `need_mib` use it
  (`PROTOCOL.md` section 12.1).

- **Format 6.** The state directory gains `quiet/` and `nostop/`, the marks (`PROTOCOL.md` section 7.18), which
  every client must read at admission and in its own run's ticks, so a client of format 5 steps out of the budget
  with its existing notice. Format 6 also carries the optional `yield.json` (section 7.17) and the ticket's optional
  `typical_s`. Run `coreslot doctor --reset` once nothing is live: on a format-5, format-4 or format-3 directory it
  is an upgrade that keeps the run log and the telemetry.

### Fixed

- `coreslot watch --for` refuses, as a usage error, a duration whose end is past the clock's range, which
  wrote an override with no end while it printed a time. An end on another day prints its date.
- `status` no longer says a stop round written before a switch to observe still stops its jobs once every
  listed job has found a container client, which ignores the stop.
- The telemetry `memory` line's `vm.name` is the VM program's full name; on Apple's hypervisor it was the kernel's
  31-byte `com.apple.Virtualization.Virtua`.
- A command could start on memory a running command had just released and was about to use again:
  the room counted it as free and, until the running command's next report, as still in use. Before
  a command starts beside running ones, coreslot now measures their process trees and the free
  memory at that moment, and waits when the room is not there (`PROTOCOL.md` section 12.1, the
  confirmation). `status` still shows the room from the last reports.
- A gated Docker run kept the VM growth its containers had already taken reserved for up to 8 s after they took
  it, so commands and containers behind it waited for memory that was in use, not about to be. A gated run now samples its
  containers every tick (2 s) until they reach its Docker need, then every third tick as before (`PROTOCOL.md`
  section 23.1).

## v0.11.0 (2026-11-24)

This release writes format 5. Upgrade every client of a machine together.

### Upgrading

A client of this release does not use a format-3 directory (v0.10.0) or a format-4 one: its runs start unbudgeted
with a notice (`--strict` refuses) until `coreslot doctor --reset`. Run the reset when no hold and no ticket is
live; it refuses while one is, and names it. On a format-3 or format-4 directory the reset is an upgrade to format 5
that keeps the run log and the telemetry, so no label learns its need again. A format-2 directory's reset removes
its run log.

### Added

- `coreslot start <ticket-id>` (or `--label L`) starts a waiting job now, ahead of the queue and whatever memory is
  free: its own run admits itself at its next poll with a hold that still reserves its full need, so the room may go
  below zero and other jobs wait. It waits up to 8 s for the run to start, or returns at once with `--no-wait`. A
  started job is a red-round victim by its queue arrival, like any other. `status` marks its hold `started now, out of
  turn`; `status --json` carries each hold's `forced` and the pending `start_requests` (`ticket`, `age_s`,
  `unanswered`), under the same `schema` 1, and the `admitted` telemetry line's `pass` gains `forced`.

- `coreslot watch observe` and `coreslot watch enforce` switch the watchdog's mode on this machine, until
  `coreslot watch config` or, with `--for DURATION`, for a while; the configuration file is never
  written, and a stop round already written still acts. `status` shows an override in its watch line, and `status --json` carries `watch.mode`, `watch.mode_source` and
  `watch.mode_until_ns` whether or not a job runs, under the same `schema` 1. The `enqueue` telemetry's `mode` is
  the mode in force.

- Telemetry records the host's CPU without acting on it: each sample line gains `ncpu`, `load1` and `load5`,
  `cpu_busy_pct` (the busy part of the CPU ticks since the previous sample) and, on Linux with PSI,
  `psi_cpu_some_us_s`. Each key is absent where it was not read, never 0. A run's `ended` line gains `tree_cpu_s`,
  its hold's tree CPU summed over its watch ticks. Admission is unchanged.
- On macOS, a label's Docker need, `docker_mem` in `needs.toml`: each of its runs that gets a forwarder waits until
  that much fits inside Docker Desktop's VM, as well as its own need on the Mac, reading Docker's socket at most
  once every 2 s, plus one immediate retry, while it could otherwise start. Once started, its hold reserves on the
  Mac the growth its containers may still cause the VM, which comes off every run's room. A run that fits on the Mac
  and waits only for the VM has a note in the new `vmwait/` directory, and every other run passes over it. After 8
  failed readings in a row, or for a need that can never fit the VM, the run waits for the Mac alone and says so
  when it starts. `status` shows each hold's `docker_mem` and what it reserves on the Mac for Docker's VM, and each
  waiting run's `docker_mem` and VM wait; `status --json` carries them as `docker_need_mib` and `docker_growth_mib`
  on holds and `docker_need_mib` and `vm_wait` on tickets, under the same `schema` 1. The telemetry `waiting` line's
  `reason` gains `vm_room_short` and `docker_no_reading`. README, "Waiting for Docker's VM", says what the
  reservation does not hold back. The growth reserved is up to twice the unfilled need on Apple's hypervisor, whose
  VM keeps the pages a transient touched, and one and a half times on Docker VMM, which gives them back.

### Changed

- **Format 5.** The state directory gains `watch-mode`, this machine's override of the watchdog's mode
  (`PROTOCOL.md` section 7.15), which every client must read for the mode it ticks in, and `start/`, the start
  requests, whose holds carry the optional key `forced` (sections 7.16 and 13.4), so a client of format 4 steps
  out of the budget with its existing notice. Run `coreslot doctor --reset` once nothing is live: on a format-4 or a
  format-3 directory it is an upgrade that keeps the run log and the telemetry. A format-2 directory is another
  format, whose reset removes its run log.
- **Format 4.** Tickets and holds gain the optional keys `docker_need_mib`, `docker_growth_mib` and
  `docker_sampled_ns`, and the state directory gains `vmwait/` (`PROTOCOL.md` sections 7.14 and 12.6). Every
  client must take the holds' VM growth off its room and pass over a noted ticket, so a client of format 3 steps
  out of the budget with its existing notice. Run `coreslot doctor --reset` once nothing is live: on a format-3
  directory it is an upgrade that keeps the run log and the telemetry. A format-2 directory is now another format,
  whose reset removes its run log.
- Docker VMM is read and budgeted again: Docker Desktop's VM is found on Docker VMM (`com.docker.sailor`) as on
  Apple's hypervisor, for `status --docker`, the telemetry `memory` line's `vm` and a run's Docker need.
- `[docker] enabled = false` also turns off every run's Docker need.

## v0.10.0 (2026-11-21)

### Added

- `coreslot move <ticket-id> --first | --last | --before <id> | --after <id>` (or `--label L`) changes a waiting
  request's place in the queue without ending its run, which `cancel` and a new request could only send to the end.
  The ticket keeps its file and its lock: the new place is an override in `order/`, read by every client that orders
  the queue, removed with the ticket. `status` lists the queue in that order.
- `coreslot trim <hold-id>` (or `--label LABEL`) asks a running job's `coreslot run` to reserve what the job has
  used lately, its largest reading of the last minute plus a margin of a tenth of its need and at least 256 MiB,
  instead of what it asked for; the reservation rises again as the job grows, never above what it asked for. It
  raises the room without changing the queue's order. The request is the new optional file `trim.json` in the
  state directory, which no admission reads (`PROTOCOL.md` sections 7.13 and 12.5); refusals exit 75 as
  `cancel`'s do.
- `[watch] trim_window_s`, from 2 to 3600 seconds and 60 by default: how far back a trimmed job's reservation
  looks. An invalid value is a configuration warning and the default, never an invalid configuration.
- `status` shows each hold's `id`, what a trimmed hold asked for (`(asked …)`) and a `trimmed` mark;
  `status --json` carries a hold's `id`, `asked_mib` and `trimmed`, under the same `schema` 1. A hold body may carry
  the optional keys `asked_mib` and `trimmed`.
- The telemetry `trim` event, written when a run first acts on a request to trim its hold.
- On macOS with Docker Desktop, a run whose label has a Docker interest (a command named `docker`,
  `docker-compose` or `podman`, an earlier run of its label that did container work, or
  `docker_forward = true` in `needs.toml`) gets a forwarder: a `coreslot forward` process that relays its
  command's Docker traffic through a socket in `~/.coreslot-fw/`, which the command's `DOCKER_HOST` names, and
  notes the containers the command creates or touches. The run reads their memory every 6 s, and `status` shows
  each hold's Docker footprint, what its containers grew by since the run first touched them, and its note,
  why a run has no forwarder or stopped counting; `status --json` carries them as `docker_footprint_mib` and
  `docker_note` on each hold, under the same `schema` 1. The format stays 3: the hold body gains these two
  optional keys. `docker_forward = false` turns the forwarder off for a label, and a selected Docker
  context other than `default` or `desktop-linux` gives no forwarder. A run whose forwarder the user turned off
  (`docker_forward = false`, or `[docker] enabled = false`) says nothing about it; only its hold keeps the note.
  A forwarder serves its command's background Docker connections after the command ends; `doctor` lists the
  forwarders still serving and `doctor --reset` removes the entries of `~/.coreslot-fw` that none serves. `net` and `net/http` are now
  linked; coreslot still connects only to unix sockets on this machine. Under an agent sandbox,
  `~/.coreslot-fw` has to be admitted (README, "Under an agent sandbox"); to uninstall, remove it beside the
  state directory.

### Changed

- **Format 3.** The state directory gains `order/` (PROTOCOL.md sections 7.12, 9 and 13.3), which every client must
  read, so a client of format 2 steps out of the budget with its existing notice. Run `coreslot doctor --reset` once
  nothing is live: on a format-2 directory it is an upgrade that keeps the run log and the telemetry, so no label
  learns its need again.
  Format 3 also carries the trim requests of `trim.json` and a hold's optional keys `asked_mib` and `trimmed`
  (sections 7.13 and 12.5).
- A live hold's `need_mib` may now be lower than the need it asked for, after a `coreslot trim`. A consumer that
  took `need_mib` for the need asked (`status --json`, the telemetry `jobs` and `holders`) then sees the current
  reservation where it expected the need asked, which a trimmed hold carries as `asked_mib`; admission arithmetic
  is unaffected, since it computes the room from `need_mib` as the body says it.
- A label's learned need counts the level its runs' memory holds, not their briefest peak. Each run writes the new
  optional run-log key `mem_sustained_kib`, the largest level its tree held for 5 watch ticks in a row (about 10 s),
  and a run's figure is that level, never more than 1 GiB under its peak; a line without the key counts its peak.
  The need of a label with brief peaks falls once its last 20 runs carry the key, by up to 1 GiB, and for a label
  of small runs of 10 to 20 s possibly by more of its need. A label whose measured peaks are all 0 now asks for 1 MiB.
- `coreslot stats` prints `need  <need> sustained (mem = "<peak need>")` when the runs hold less than their peaks,
  and still suggests the peak-only need as the pin; `stats --json`'s `need` is that pin.
- A stopped run whose need was learned comes back with at least its label's peak-only need reserved.

### Fixed

- The watchdog no longer turns yellow or red, or stops a command, because of trees that hold almost no memory. A
  tree under 128 MiB that spent its time in the kernel while the host paged for other reasons could raise the stall
  excess on its own, and a red round then stopped it, freeing nothing, again on every attempt of the same run. The
  excess is now the smaller of the mean over every measured command and the same mean over those of 128 MiB or more,
  so it never rises, and it falls below the large trees' own mean only where the old mean was lower still. A red
  round spares a nearly finished command only when the command it stops in its place holds at least 128 MiB.

## v0.9.1 (2026-11-19)

### Changed

- A job that fits no longer waits behind a head that cannot start. When the head does not fit, one waiter at a
  time may go ahead of it out of turn, for no longer than it has itself watched the head wait, whether or not
  the head's wait is known: a small job no longer waits behind a large one that waits for memory held outside
  coreslot (other programs, Docker Desktop's VM), nor behind one whose expected wait is a little shorter than
  the job. Since runs out of turn never overlap, the head still gets its idle start, and going out of turn at most
  doubles its wait. A run goes out of turn only while the kernel's memory pressure level is normal (macOS); where
  the level is unknown, as on Linux, nobody does. The telemetry `admitted` line says how a run passed the head
  (`pass`), and `no_pass` gains `head_fits`, `pressure_not_normal`, `hold_unreadable` and `out_of_turn_live`.

- The telemetry `no_pass` of a waiter behind the head while no hold is live and the headroom is unread is
  `no_holds`, the head starting at once, rather than `headroom_unknown`.

### Fixed

- `status --json` no longer marks `stoppable` a hold that names pid 0 or 1, which is never a run's; such a hold
  shows `owner_exited`.
- A restart head's `expected_wait_s` no longer reads the whole calm for one poll when a watch sample lands just as
  `status` reads its clock: the snapshot reads the clock once, after the hold bodies and the watch state.

## v0.9.0 (2026-11-18)

### Added

- `coreslot status --docker` shows Docker Desktop's VM (its memory, footprint and a free estimate) and every
  running container's name, image, Compose project and memory, read once from the local Docker socket. It shows
  them to any caller of the user. `status --json` carries them under `docker`, present only with the flag, and a
  top-level `config_warnings`, under the same `schema` 1.
- `[docker] enabled = false` in the configuration reads no Docker socket at all, for `status --docker` and for the
  `containers` telemetry line. A value that is not a boolean switches the features off, and `status` and `doctor`
  print a configuration warning that never changes their exit status.

### Changed

- An automatic label of a command behind a launcher is the shape of the command it runs (README, "Labels are
  automatic"): `timeout 10m npx jest` is `npx jest@proj`, not `timeout@proj`, so it no longer shares one learned
  need with every other command behind the same launcher. History and `needs.toml` keys under a launcher's label
  no longer apply to such commands; move a key to the new label.
- The `vm` of the `memory` telemetry line is Docker Desktop's VM process: the Virtualization VM in the resource
  coalition of `com.docker.backend`, matched by executable path. A VM of another tool no longer counts, and the
  `com.docker.krun`, `sailor` and `virtualization` names are no longer looked for. A setup whose VM is not a
  Virtualization VM, such as Docker's VMM backend, no longer has a `vm`.
- `status --docker` reads only Docker Desktop with its VM on the Apple hypervisor. Docker VMM, OrbStack and
  Colima are not supported in this release: for them it says `not_running` and opens no socket.

### Fixed

- `PROTOCOL.md` section 10.3 says a hold expected to end frees the larger of its `need_mib` and its
  `footprint_mib`, as the CLI has always counted it; the text said `need_mib`.
- `docs/status-json.md` says what `stoppable` and `pid` establish and what they do not, how a reader
  stops a `stoppable` run (match the hold by `name`, send `SIGTERM` to `pid`), and the cases it left
  out; the v0.8.0 entry below now says `expected_wait_s` is on every ticket.

## v0.8.0 (2026-11-12)

### Added

- `coreslot status --json` carries more, under the same `schema` 1: per hold `typical_s`, `left_s`
  (null without a typical run time) and `stoppable`; per ticket `name`; in `mem` `machine_mib`,
  `reserve_mib`, `headroom_mib` and `room_mib`, the room before it is clamped at 0.
- `coreslot status --json` gives every ticket an `expected_wait_s`: on the first live ticket the
  expected wait the passing rule judges other requests by, null on every other ticket and where the
  rule leaves it unknown.
- `docs/status-json.md` describes every key of `status --json`, and a test keeps it complete.

## v0.7.0 (2026-11-11)

### Added

- `coreslot status` marks a running command whose process tree has for 30 minutes or more used next
  to no CPU, shown no new process and held no container client: `no CPU for 2h10m` on its line,
  `no_cpu_s` in `--json`. It only informs. A command that waits without being stuck, a slow download
  say, is marked too, as is work it cannot see: containers whose client has closed, a build daemon, a
  child run as another user. No mark does not say a command is working: a container hung behind a
  live client gets none, nor does a command that keeps starting processes, a wait loop such as
  `until nc -z db 5432; do sleep 1; done` included, since a process that lives between two samples
  leaves no CPU time they see.
- `coreslot doctor` says whether the local record is on. An `enabled` that is not the boolean `true`
  reads as off, and until now nothing said so.

## v0.6.0 (2026-11-09)

### Added

- An optional local record, off by default: `[telemetry] enabled = true` writes to `telemetry/` in the
  state directory why each run waited, how it ended, the machine's memory by part and by process and,
  for container work, each container's memory, read from the local Docker socket. Nothing leaves the
  machine and coreslot opens no network connection. Keys in `docs/telemetry.md`; up to about 1 GiB.
  A run that records sets `CORESLOT_TELEMETRY` for its command, so that a run nested in it records
  without reading the switch again.

### Changed

- A need above what the machine has less the reserve now says it `starts only when no other coreslot
  job runs`, in place of `runs only alone`: a container request may start beside it. With the
  headroom unreadable nothing gates it, and the notice is no longer printed.
- A container request's waiting line no longer gives a queue position or holders while a hold is
  live, as it waits on neither.
- Machine samples move from `log/watch.jsonl` to `telemetry/samples.jsonl`. A file left by an older
  release stays where it is; no reader of the format needs either, but the harness under
  `evidence/air-v04` reads the old file. A run of an older release still alive takes the sample too
  and writes it to the old file or nowhere, so `samples.jsonl` can stay empty while one lives: upgrade
  with none running.

### Deprecated

- `[watch] sample_log`, in favour of `[telemetry] enabled`. It still works and writes the machine
  samples alone, now to `telemetry/samples.jsonl`, rotated at 256 MiB in place of 10 MiB.

## v0.5.2 (2026-11-06)

### Changed

- A command named `docker`, `docker-compose` or `podman` needs no `--restart`, does not honour
  `--restart=yes` and takes 1 MiB when it has no pin and no history. Without a pin (`--mem`, or `mem`
  in `needs.toml`) it passes the queue while another run is live and ignores what other runs
  reserved, but still waits below the reserve and at the yellow level. A stop round never stops it,
  nor a run from the moment coreslot sees such a command in its tree or, on macOS, a connection to a
  socket named `docker.sock`; a stop before that can still land. A Podman library client on macOS is
  not seen. Holds and run-log lines gain an optional `container` key, and `status` and `report` say
  what such work leaves unmeasured.

## v0.5.1 (2026-11-05)

### Changed

- The watchdog's thresholds are back to those before v0.5.0: `[watch] red_samples` defaults to 10
  (red needs about 20 s of danger held) and yellow clears after 15 samples below its threshold.
  v0.5.0's faster values (5 and 5) rested on a simulation alone; they are withdrawn until a
  measurement supports them. The yellow latch still clears after a pause of more than 20 s, and
  `calm_s` stays at 10 s.

## v0.5.0 (2026-11-04)

### Changed

- **Breaking:** `coreslot run` requires `--restart=yes` or `--restart=no` wherever the watchdog
  could stop the command, that is, wherever its command gets a process group of its own. When
  memory runs short coreslot kills the newest commands and runs them again from scratch, which is
  right for tests, builds, linters and type checks (`yes`) and wrong for anything that changes the
  world outside its own output, such as databases, migrations, deploys, pushes, sending messages
  or writing shared files (`no`, never stopped). Without the flag such a run refuses before it
  queues, with a usage error (exit 64) that says which to pick; when unsure, `no`. A run in a
  terminal, with `--same-group`, nested inside another run's reservation or outside the budget is
  never stopped and needs no flag; there `--restart=no` only keeps the Linux `oom_score_adj` the run
  would otherwise raise. `needs.toml`'s `no_restart = true` still works and wins over
  `--restart=yes`. Scripts and wrappers that call `coreslot run` must pass the flag. The stop line
  says that the command was declared restartable.
- The default `[watch] calm_s` is 10 s instead of 30 s: a stopped command comes back, and starts
  are spaced, after 10 s of calm. The value rests on a simulation calibrated on the recorded runs,
  not on a measurement ([the calibration notes](calibration/README.md#what-the-defaults-rest-on));
  the next acceptance run measures it.
- The watchdog acts sooner. The default `[watch] red_samples` is 5 instead of 10, so red needs about
  10 s of danger held instead of about 20 s, and yellow clears after 5 samples below its threshold
  instead of 15. They rest on a simulation calibrated on the recorded runs and, for `red_samples`,
  on a replay of the samples logged by three acceptance runs, which found no false red at 5; not on
  a measurement ([the calibration notes](calibration/README.md#what-the-defaults-rest-on)). The next
  acceptance run measures them.
- The Claude Code hook offers `coreslot run --restart=yes --` and no longer recognises `make -j`,
  Gradle or `xcodebuild`, whose targets can deploy, migrate or upload; its instructions snippet for
  other agents asks them to choose `--restart` per command. The refusal tells the agent to change
  it to `--restart=no` when the tests or their setup or teardown scripts touch a shared database,
  external services or shared files, and says so when the shape came from the user's own shape
  file rather than the defaults. The hook runs no process; its refusal says what to do when an
  older coreslot rejects the flag. The plugin is version 0.2.0 and expects a coreslot newer than
  0.4.1.

### Added

- `coreslot report [--days N] [--json]` summarizes the run log's last N days (7 by default): runs,
  labels and attempts; the attempts that waited 1 s or more (total, median, p90, longest, and how
  many waited over 2 and over 10 minutes); the attempts stopped for memory, the run time lost to
  those stops and the runs that gave up; and the heaviest labels by memory peak. It claims no time
  saved, since what the runs would have done without coreslot is not measured, and points to the
  results measured on one MacBook Air. It reads the existing run log and writes nothing; lines that
  do not decode, lie in the future or hold an impossible value are skipped and counted.
- A red stop round may pass over its newest victim when that command is nearly finished and stop the
  next newest one instead, when that one alone covers what the round must free and has done less CPU
  work. A command is nearly finished when its label has at least 10 counted runs, its CPU time so
  far against the median CPU time of those runs gives a progress strictly between 0 and 1, and the
  remaining time that progress predicts, slowed by its recent share of time lost to memory, is under
  the label's typical run time. The first keep sets a deadline, its time plus that estimate; a later
  round keeps the command again only before the deadline and while the estimate keeps shrinking. The
  run prints `coreslot: kept … (memory red, about N s left, P% done, until HH:MM:SS)`, and a
  later stop of it says that an earlier round kept it. A floor round keeps nothing. Holds carry the
  optional keys `cpu_ns`, `typical_cpu_s` and `stall_ewma`, and `watch.json` the detector's `kept`
  records.

### Fixed

- The watchdog's yellow latch keeps only fresh evidence, as its red bucket does: a reading more than
  20 s after the last one that could be read, or before it, clears it. A set of commands that
  starts after a pause no longer starts at a yellow left over from an earlier one.

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
