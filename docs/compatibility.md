# Compatibility policy

What a coreslot release promises to keep working, and for how long. The policy applies from
`v0.4.0` on. [`CHANGELOG.md`](../CHANGELOG.md) records every change against this policy: anything removed or
changed incompatibly appears there under **Removed** or **Changed** first, and under
**Deprecated** before that where this policy asks.

## Versions

coreslot is at **`v0`** until `v1.0.0`. A format number's metadata (the names, bodies and layout of
the state directory) is never redefined once a release has shipped it, before `v1.0.0` as after it:
a release that needs different metadata introduces a new format number instead (below). Within a
format a release adds at most an optional key every reader may ignore, or an optional file no
admission reads, as `needs.toml` is. Before `v1.0.0`, a minor release may still change what a
format number means elsewhere: the configuration's defaults, including the watchdog's thresholds,
or its size grammar ([`PROTOCOL.md` section 5](../PROTOCOL.md#5-sizes-and-configuration)). Such a
change is listed in [`CHANGELOG.md`](../CHANGELOG.md) under **Changed**, with what an older client
on the same machine then gets wrong. `v1.0.0` freezes the format then current; from then on a new
format number, or a change of what one means, needs a new major version.

`v1.0.0` is not cut before the whole conformance suite passes at full length on macOS and on
Linux. That pass is necessary for `v1.0.0`, not its trigger. The suite, in
[`conformance/`](../conformance), is:

- deterministic traces that pause real CLI and helper processes at named points inside the
  protocol's steps, stop, continue or kill them there, and check the state directory and what the
  other clients do meanwhile;
- the stop-round and restart traces, which drive the watchdog through the readings test hook;
- the kill storm, which for 60 s kills, stops, resumes and cancels 50 CLI clients, some of them
  nested, while a sampler checks invariants I1, I2, I3 and I7 of
  [`PROTOCOL.md` section 3](../PROTOCOL.md#3-state-machine-and-invariants).

At full length means without `-short`:

```sh
go test -tags coreslot_testhooks -race ./conformance/
```

CI runs the suite on every change, on Linux and macOS, but shortened with `-short`. What is still
missing is a full-length pass on each system.

Until `v1.0.0` a minor release (`0.x`) may change the CLI surface within the deprecation window,
and may introduce a new format number. `v1.0.0` freezes the format then current and the CLI surface
listed here; after it, an incompatible change to either needs a new major version.

## On-disk formats

| Release | Formats implemented |
|---|---|
| `v0.4.0` | 2 |

`coreslot version` prints the formats a binary implements, for example
`coreslot 0.4.0 (protocol formats: 2)`. [`PROTOCOL.md`](../PROTOCOL.md) defines format 2.

A format number versions the metadata: the names, bodies and layout of the state directory other
than `admit.lock`. A release that changes the metadata bumps the number it writes to `FORMAT`
(`coreslot-protocol <n>`). One change is not a metadata change in this sense: a new optional key in
a body or a run-log line, which a writer may leave out and which every reader of the format ignores
when it does not know it
([`PROTOCOL.md` section 7.2](../PROTOCOL.md#72-rules-for-every-body) and
[section 20](../PROTOCOL.md#20-run-log)). A key a reader must understand, a key removed, renamed
or given another type or meaning, and any change to names or layout bump the number. `admit.lock`,
an exclusive `flock` held briefly by whoever changes admission state, never changes with a format,
so a format change never creates a second budget
([`PROTOCOL.md` section 15.1](../PROTOCOL.md#151-what-every-format-keeps)).

A client that finds a format it does not implement
([`PROTOCOL.md` section 15.2](../PROTOCOL.md#152-a-format-this-client-does-not-implement)) writes
nothing to the state directory and its run fails open: the command runs outside the budget, with a
notice, or is refused under `--strict`. `coreslot doctor --reset` recreates the directory in the
client's own format once nothing is live. Mixing releases across a format change is therefore safe
but leaves the older clients unbudgeted; upgrade every client of a machine together.

## CLI

Supported: the commands, flags, environment variables and exit statuses below. Human-readable
output (`status` without `--json`, `doctor`, diagnostics on stderr) is not a contract and may
change in any release; parse `status --json` instead.

| Surface | Supported |
|---|---|
| `run` | `--mem SIZE` (such as `3G` or `768M`), `--label`, `--timeout`, `--on-timeout run\|fail`, `--no-wait`, `--strict`, `--independent`, `--same-group`, `--refusal-code` |
| `status` | `--json`, `--observe DURATION` |
| `cancel` | `<ticket-id>`, `--label LABEL` |
| `doctor` | `--clear`, `--reset` |
| `stats` | `--label LABEL`, `--json` |
| `version` | the output line `coreslot <version> (protocol formats: <list>)` |
| configuration | `<home>/.config/coreslot/config.toml`: `[memory] reserve`, `[watch]` (`mode`, `sample_log` and the thresholds of [`PROTOCOL.md` section 5.3](../PROTOCOL.md#53-configuration-file)), `[platform] windows_warning`; `needs.toml` beside it: `[needs."<label>"]` with `mem` and `no_restart` |
| environment | `CORESLOT_STATE_DIR` (read); `CORESLOT_GRANTED_MEM` (MiB), `CORESLOT_HOLD` (fd 3), `CORESLOT_UNBUDGETED` (given to the command, read by nested runs) |
| descriptors | the hold at fd 3 |
| exit statuses | the command's own, 130, 75, 64, 126, 127, 70, 3, 1, as [`PROTOCOL.md` section 19](../PROTOCOL.md#19-exit-status) defines them |

The marker line that a run prints when it gives up after 3 stops
([`PROTOCOL.md` section 24.2](../PROTOCOL.md#242-giving-up)) is a contract too, so that an agent
can tell it from a command's own failure.

### `status --json`

The document carries a `"schema"` number. Within one schema number, fields may be added but are
never removed, renamed or given another meaning; a consumer MUST ignore fields it does not know.
Removing or changing a field raises the number, and that release prints the new one. Labels are
carried exactly, JSON-encoded.

## Deprecation window

While at `v0`:

- A flag, command, configuration key or environment variable is first marked **Deprecated** in
  `CHANGELOG.md` (and, for the CLI, warns on stderr when used), and keeps working for at least one
  further minor release before it is removed.
- The deprecation window does not apply to the on-disk format, because an older client is never
  cut off: a release that changes the metadata bumps the `FORMAT` number (above), is announced in
  `CHANGELOG.md` and `PROTOCOL.md`, and from then on older clients on the same machine run
  unbudgeted. Before `v1.0.0` this may happen in any minor release; from `v1.0.0` it needs a new
  major version.
- `status --json` changes follow the schema rule above.

From `v1.0.0`: deprecated surfaces keep working until the next major version.

## Platforms

macOS and Linux on amd64 and arm64, built with the two Go releases the Go project supports
(`go.mod` names the older one). macOS on Apple silicon is the platform coreslot is developed and
measured on. Linux and Intel Macs are experimental: they run the same rules on their own kernel
readings, and the watchdog's defaults are not calibrated for them. CI runs the tests on Linux
amd64, Linux arm64 and macOS arm64 with both Go releases; macOS amd64 is built for the release
archives but not tested in CI. Windows builds, but runs commands without a budget.
