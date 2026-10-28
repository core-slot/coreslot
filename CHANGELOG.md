# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as the
[compatibility policy](docs/compatibility.md) applies it: the module stays at `v0` until
`v1.0.0`, which freezes the on-disk format then current, the CLI surface and the library API.

## [Unreleased]

## [0.3.0] - 2026-10-28

A command's worker count is no longer its cpu reservation, long batch runs give way to the rest,
and orphaned commands stop holding the budget. The on-disk protocol stays at format 1.

### Upgrading to v0.3.0

Two defaults change:

- The live size of `mem` is off. `mem` is sized from the configuration alone, as before v0.2.0;
  `[memory] live = true` in the configuration turns the live size back on.
- Long, large runs are demoted automatically: a run that queues with a cpu request of at least half
  the cpu budget, whose label has at least 5 recorded runs with a median of 10 minutes or more, is
  admitted with one cpu token and runs at nice 10 (see **Added**). `preemptible = false` in the
  label's `needs.toml` entry opts it out; set it for runs with wall-clock deadlines (test timeouts,
  browser or end-to-end tests, agent tool timeouts).

An older client on the same machine still sizes `mem` live unless `live = false`, and ignores the
new `needs.toml` keys; [the compatibility policy](docs/compatibility.md) says what it does
differently.

### Added

- `cpu_per_worker = c` in a `needs.toml` entry: each worker of the labelled command takes `c` cpu
  tokens. With a final worker count in the label (`gate:unit@12`) the request is `⌈c × 12⌉`
  tokens; without one, the entry's `cpu` is the worker width and `{n}` is the number of workers
  the grant pays for, never 0. `--need cpu=…` still wins, and `CORESLOT_GRANTED_CPU` stays the
  token count.
- `observed = false` in a `needs.toml` entry, for a label whose work runs outside its process tree
  (a container, another host): `suggest` keeps that entry as written. Without it, a label whose
  runs of a minute or more averaged under 0.05 cores gets a hint in `suggest`'s evidence instead of
  a lowered need.
- Preemptible runs: `coreslot run --preemptible` (`--preemptible=false` unmarks) or
  `preemptible = true` in the label's entry admits a batch run with its full `mem` and one cpu
  token, and runs its command's process group at nice 10. `{n}` keeps the width of the request; the
  run log records the request as `need`, the real grant, and a `preemptible` key with the reason
  (`flag`, `entry` or `auto: …`), which `status` also shows on the hold. The first automatic
  demotion of a label prints one line about runs with wall-clock deadlines.
- `coreslot run --same-group` keeps the command in coreslot's own process group, so a caller that
  kills that group, SIGKILL included, reaches the whole command tree. As in a terminal, a signal
  sent to coreslot's pid alone then reaches only the command's pid, or none: signal the group.
- The run log's optional `workers` key: the value a cpu placeholder took, else the label's worker
  count.
- `status` marks a hold whose owner exited while a descendant still holds the grant
  (`owner exited, held by a descendant`; `"owner_exited"` in `status --json`, schema 1), and
  `doctor` notes it without counting a problem.
- `stats --json` adds `suggestion.cores_per_worker`.

### Changed

- `[memory] live` defaults to `false` (see **Upgrading**): two acceptance runs measured no
  throughput gain from admitting `mem` above the configured size, and one measured swap under load.
- `{n}` and `{n:cpu}` are the worker count for a per-worker request.
- `coreslot suggest` learns cores per worker, the p90 of cores ÷ workers rounded up to 0.05, and
  writes `cpu_per_worker` into a per-worker entry, never `cpu`. A range entry whose runs record
  `workers` is no longer narrowed on every learn, which drove a latency-bound suite from 10 workers
  to 1; its evidence suggests `cpu_per_worker` instead. A saturated run raises the per-worker cost
  through its own measurement, with no separate raise, so learning no longer oscillates. Runs
  without `workers` are suggested as before.
- `suggest` leaves preemptible runs out of its saturation count, and the post-run cpu hint skips
  them.

### Fixed

- A command whose caller killed coreslot's process group no longer lives on with the grant's tokens
  locked, under `--same-group`.
- The root package's test binary marks inherited descriptors close-on-exec, so its helper processes
  never carry an outer run's grant.

## [0.2.1] - 2026-10-24

A patch: a command with a cpu placeholder no longer asks for more cpu than the machine has, and
`suggest` says when the peak behind a `mem` need was taken at a wider cpu grant.

### Changed

- A run without `--need` whose command has a cpu placeholder asks for `cpu=1..16` only up to the
  machine's CPU count, so on a 10-CPU Mac with the `"140%"` default it no longer starts 14 workers.
  The Python client follows.
- `coreslot suggest` says in its evidence when the peak its `mem` rests on was taken with a cpu
  grant above the cpu it suggests (`peak 3.1 GiB (footprint) at 8s with cpu 14`): a command with
  `{n}` uses memory per worker, so the label's runs at the new grant may need less, and a later
  `suggest --write` over them writes it.

## [0.2.0] - 2026-10-23

Memory becomes a budgeted resource beside cores, and coreslot learns what each labelled command
needs from its own runs. The defaults change for every client on a machine, so upgrade them
together (see **Changed**). The on-disk protocol stays at format 1.

### Added

- `coreslot run --light` runs a command too light to budget, such as a download, at once outside
  the budget, still shown by `status` and recorded in the run log.
- After a run of a minute or more, one line on stderr when the cores the command used do not match
  the cpu tokens it held, with the request that would fit.
- A waiting run whose stdin is a pipe says once that the command writing it may be waiting for it.
- The `mem` resource, declared by default at `"60%"` of physical memory in tokens of 128 MiB:
  `--need mem=3G` reserves memory beside cores, and `mem` in `[resources]` takes GiB, `"N%"` of
  memory or a size such as `"768M"`, with `0` switching it off.
- The run log records each run's memory peak, the command's whole process tree sampled every second
  (`mem_peak_kib`, `mem_peak_source`, `peak_after_s`), and for a run that names `mem`, how long it
  waited while that much memory was free (`waited_free_s`).
- `coreslot stats` shows what each label's last 5 runs used; `coreslot suggest` prints the needs
  they suggest, and `--write` writes them into `needs.toml`.
- `needs.toml`, beside the configuration: a labelled run takes from its entry the `cpu` and `mem`
  its `--need` leaves out.
- After a run whose memory peak outgrew its `mem` need, or passed 1 GiB with none, one line on
  stderr pointing at `coreslot suggest --write`.
- A live size for `mem`: when the configured `mem` leaves a fixed `mem` need unmet, the run and the
  library admit it from the memory the kernel can still hand out, less what the running holds have
  reserved and not yet used, up to 4096 tokens (512 GiB).
- `[memory] live = false` in the configuration turns the live size off.
- The hold body's optional `tree` key: `true` when the hold's process tree carries the grant's work,
  so that its resident memory counts against its reservation; the CLI's runs write it.

### Changed

- Without a configuration, the cpu budget is `"140%"` of the CPU count instead of `"100%"`, and
  `"N%"` accepts N up to 400. An older client on the same machine still computes `"100%"`, reads
  `mem = "60%"` as a share of CPUs and refuses `"N%"` above 100; upgrade every client together.
- `mem` is built in, in tokens of 128 MiB, and no longer a resource in the user's own unit. An
  existing `[resources] mem = 16` is now 16 GiB and `mem = "50%"` half of the physical memory, not
  of the CPU count; `--need mem=4` reserves 4 GiB (32 tokens) and `--need mem=2..4` is a usage
  error (exit 64); `CORESLOT_GRANTED_MEM` and `{n:mem}` are in MiB, not tokens; and a library
  `Need{"mem": {4, 4}}` reserves 4 tokens, 512 MiB.
- `status --json` (schema 1) adds `size_mib`, `locked_mib` and `free_mib` to the `mem` resource; the
  human output of `status`, `doctor` and the run's notices shows memory in bytes.
- The configured `mem` is a floor, not a ceiling: runs are admitted above it while free memory
  allows, so `status` may show `mem` with 0 free while runs are still admitted. An older client on
  the same machine sizes `mem` from the configuration alone and waits while those admissions hold
  more than its size.
- On Linux, `waited_free_s` counts the memory free under the tightest cgroup v2 limit when that is
  less than `MemAvailable`, the same headroom the live size reads.
- The waiting head retries admission at most every 0.5 s (was 2 s), so a freed budget is taken
  sooner.

### Fixed

- A release's archives and its binary's version are named after the tag that started the
  release, even when another tag points at the same commit.

## [0.1.0] - 2026-10-15

The first release: one machine-wide core budget that `coreslot run` and a Go library share, kept as
token files and `flock(2)` locks under on-disk protocol format 1, for macOS and Linux.

The module path is `github.com/core-slot/coreslot`; an install from the earlier path,
`github.com/Nezhinskiy/coreslot`, fails with "module declares its path as".

### Added

- On-disk protocol format 1, defined normatively in `PROTOCOL.md`: token files and `flock(2)` locks
  under one admission lock, a single FIFO queue with priorities, elastic `min..max` grants, clamping
  of requests larger than the budget, cancellation, reconciliation of dead entries, adoption of
  already-running work, token-only mode for clients of another format, and pass-through for nested
  runs.
- `coreslot` CLI with `run`, `status` (`--json` schema 1, `--observe`), `cancel`, `doctor`
  (`--clear`, `--reset`) and `version`, for macOS and Linux on amd64 and arm64. On Windows commands
  run without a budget, with a warning. `coreslot version` reports the module version of a
  `go install` build when no version was set at link time.
- One-sided ranges in `run --need`: `RES=MIN..` means `MIN..max(MIN, 16)` and `RES=..MAX` means
  `1..MAX`, beside `RES=MIN..MAX` and `RES=N`.
- A `run` without `--need` asks for `cpu=1..16` when its command has a cpu placeholder (`{n}` or
  `{n:cpu}` as an argument, or inside one with `--subst`), and for `cpu=1..1` otherwise, so a
  command that can use its grant gets a range and one that cannot holds no token it would leave
  idle.
- Configuration in `~/.config/coreslot/config.toml` (`[resources]`, `[priorities]`, `[platform]`),
  read from `<state directory>/config.toml` instead under `CORESLOT_STATE_DIR`.
- Go library in the root package: `Open`; a `Client` that enqueues (`Enqueue`), adopts running
  work (`Adopt`), reports what is free (`Available`), reads the state (`Snapshot`, a `Report`) and
  is released with `Close`; a `Waiter` with `Poll`, `Wait` and `Cancel`; and a `Grant` whose
  `Files` and `Environ` start a command inside it and whose `Shortfall` says what it runs without.
  `Enqueue`, `Adopt` and `Wait` take a `context.Context`, and errors are a `*StateError`, a
  `*UsageError`, a `*SectionBusyError`, the context's error or a sentinel (`ErrCanceled`,
  `ErrRefused`, `ErrAlreadyAdmitted`, `ErrUnsupported`). The exported API is pinned in
  `api/v0.txt`.
- The run log records what each command used: `cpu_user_s` and `cpu_sys_s` (CPU seconds) and
  `max_rss_kib` (peak resident set size in KiB), optional keys of `PROTOCOL.md` section 20 taken
  from the command's `wait4` rusage, so they cover the descendants it waited for. A run whose
  command never started leaves them out, as does a platform that does not report one.
- A library `Enqueue` made inside another run's grant, on a client of that run's state directory,
  passes through as a nested `coreslot run` does (`PROTOCOL.md` section 18) and never queues
  behind its own parent; a context that does not validate matches `ErrRefused`.
  `Options.Independent` queues anyway, `Adopt` never passes through, and `Grant.PassThrough`
  reports such a grant.
- Conformance suite with deterministic queue traces, and an independent Python client written from
  `PROTOCOL.md` alone.
- Release archives with checksums and a build-provenance attestation for every archive, and a
  Homebrew formula template that builds from source.

[Unreleased]: https://github.com/core-slot/coreslot/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/core-slot/coreslot/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/core-slot/coreslot/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/core-slot/coreslot/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/core-slot/coreslot/releases/tag/v0.1.0
