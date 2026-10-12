# Changelog

All notable changes to this project are recorded here. The format follows
[Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html) as the
[compatibility policy](docs/compatibility.md) applies it: the module stays at `v0` until on-disk
format 1 is frozen.

## [Unreleased]

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
- Configuration in `~/.config/coreslot/config.toml` (`[resources]`, `[priorities]`, `[platform]`),
  moved into the state directory together with the state under `CORESLOT_STATE_DIR`.
- Go library in the root package (`Open`, `Enqueue`, `Poll`, `Wait`, `Cancel`, `Adopt`, `Free`,
  `Snapshot`, `Grant`), with its exported API pinned in `api/v0.txt`.
- Conformance suite with deterministic queue traces, and an independent Python client written from
  `PROTOCOL.md` alone.
- Release archives with checksums, build-provenance attestations for releases of the public
  repository, and a Homebrew formula template that builds from source.

[Unreleased]: https://github.com/Nezhinskiy/coreslot/commits/HEAD
