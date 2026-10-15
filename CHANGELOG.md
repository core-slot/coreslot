# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as the
[compatibility policy](docs/compatibility.md) applies it: the module stays at `v0` until
`v1.0.0`, which freezes the on-disk format then current, the CLI surface and the library API.

## [Unreleased]

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
