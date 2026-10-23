# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as the
[compatibility policy](docs/compatibility.md) applies it: the module stays at `v0` until
`v1.0.0`, which freezes the on-disk format then current, the CLI surface and the library API.

## [Unreleased]

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

[Unreleased]: https://github.com/core-slot/coreslot/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/core-slot/coreslot/releases/tag/v0.1.0
