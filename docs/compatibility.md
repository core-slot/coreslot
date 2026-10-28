# Compatibility policy

What a coreslot release promises to keep working, and for how long.
[`CHANGELOG.md`](../CHANGELOG.md) records every change against this policy: anything removed or
changed incompatibly appears there under **Removed** or **Changed** first, and under
**Deprecated** before that where this policy asks.

## Versions

coreslot is at **`v0`** until `v1.0.0`. A format number's metadata (the names, bodies and layout of
the state directory) is never redefined once a release has shipped it, before `v1.0.0` as after it:
a release that needs different metadata introduces a new format number instead (below). Within a
format a release adds at most an optional key every reader may ignore, or an optional file no
admission reads, as `needs.toml`, `needs.toml.lock` and their temporary files are. Before
`v1.0.0`, a minor release may still change what a format number means elsewhere, such as the
configuration's defaults or its size grammar ([`PROTOCOL.md` section 5](../PROTOCOL.md#5-resources-sizes-and-configuration)).
Such a change is listed in [`CHANGELOG.md`](../CHANGELOG.md) under **Changed**, with what an older
client on the same machine then gets wrong: when the cpu default went from `"100%"` to `"140%"`, an
older client on a 10-CPU machine without a configuration file computes a cpu budget of 10 where a
new one computes 14, reads `mem = "60%"` as a share of CPUs, refuses `"N%"` above 100 as an invalid
configuration, and finds a `mem` need undeclared. When `mem` became a built-in resource, a `mem`
declared in the user's own unit changed meaning: `mem = 16` is 16 GiB, `mem = "50%"` half of the
physical memory, `--need mem=4` 4 GiB and `CORESLOT_GRANTED_MEM` MiB. When `mem` gained a live size
([`PROTOCOL.md` section 5.2](../PROTOCOL.md#52-sizes)), the configured `mem` became a floor: a newer
client admits a fixed `mem` need above it from the memory the kernel can still hand out, unless
`[memory] live = false`. An older client sizes `mem` from the configuration alone, so it waits while
newer clients' live admissions hold more `mem` than its size, as after a shrink, and `status` may
show `mem` with 0 free while newer clients are still admitted above it. `v1.0.0` freezes the format then current; frozen
means that from then on a new format number, or a change of what one means, needs a new major
version.

Release v0.3.0 changed two defaults and added optional keys. When `[memory] live` became `false` by
default, the configuration's default changed, not the format: a v0.2.x client on the same machine
reading a configuration without a `[memory]` table still computes the live size, so it may be
admitted above the configured `mem` while v0.3.0 clients wait, as after a shrink. When the run log
gained the keys `workers` and `preemptible`, and a preemptible run's `need` became its request
before it was cut to one cpu token ([`PROTOCOL.md` section 20](../PROTOCOL.md#20-run-log)), the keys
were optional ones every reader ignores when it does not know them, so the format did not change; an
older `suggest` reading the same log ignores `preemptible` and may count a preemptible run as
saturated (12 cores on 1 token). When the needs file gained `cpu_per_worker` sizing, `observed` and
`preemptible` ([`PROTOCOL.md` section 5.4](../PROTOCOL.md#54-needs-file)), an older client, which
ignores what it does not know there, requests an entry's `cpu` as tokens, a worker width included,
never demotes a run, and its `suggest --write` may lower an `observed = false` entry's weights
unless the entry is pinned. A preemptible run, marked or made one by the client's automatic rule
(the other default), is no format change either: its ticket and hold carry the shrunk request,
nothing is lent, rewritten or acquired again, and the lower priority its command runs at is not
protocol state.

`v1.0.0` is not cut before the whole conformance suite passes at full length on macOS and on
Linux. That pass is necessary for `v1.0.0`, not its trigger. The suite is:

- the deterministic traces in [`conformance/`](../conformance), which pause real CLI and helper
  processes at named points inside the protocol's steps, stop, continue or kill them there, and
  check the state directory and what the other clients do meanwhile;
- the kill storm there, which for 60 s kills, stops, resumes and cancels 50 CLI, library and
  Python clients, some of them nested and some in token-only mode, while it shrinks and grows the
  budget and a sampler checks invariants I1, I2, I3 and I7 of
  [`PROTOCOL.md` section 3](../PROTOCOL.md#3-state-machine-and-invariants);
- the interoperability test there, which queues the Python client, the CLI and the library in one
  FIFO order and checks that the Go and Python decoders agree on every wire fixture;
- the Python client's own tests in [`conformance/pyclient`](../conformance/pyclient).

At full length means without `-short` and with the Python client required:

```sh
CORESLOT_REQUIRE_PYTHON=1 go test -tags coreslot_testhooks -race ./conformance/
(cd conformance/pyclient && python3 -m unittest)
```

CI runs the suite on every change, on Linux and macOS, but shortened: with `-short` the kill storm
runs 20 clients for 10 s, `TestClockStepThenHeadStops` skips its forward clock step and
`TestSuspendAllWaiters` its third phase; and the Python clients take part only when Python 3.11 or
newer is found, which CI provides but does not require. What is still missing is a full-length pass
on each system, which is recorded in [`evidence/README.md`](../evidence/README.md) when it is made.

Until `v1.0.0` a minor release (`0.x`) may change the CLI surface and the library API within the
deprecation window, and may introduce a new format number (below). `v1.0.0` freezes the format then
current (format 1, unless a `v0` release introduced a newer one), the CLI surface and the library
API listed here; after it, an incompatible change to any of them needs a new major version.

## On-disk formats

| Release | Formats implemented |
|---|---|
| every `v0` release so far | 1 |

`coreslot version` prints the formats a binary implements, for example
`coreslot 0.1.0 (protocol formats: 1)`. [`PROTOCOL.md`](../PROTOCOL.md) defines format 1.

A format number versions the metadata: the names, bodies and layout of the state directory other
than `admit.lock` and `res/`. A release that changes the metadata bumps the number it writes to
`FORMAT` (`coreslot-protocol <n>`); the meaning of a number that a release has shipped never
changes, before `v1.0.0` as after it. One change is not a metadata change in this sense: a new
optional key in a body or a run-log line, which a writer may leave out and which every reader of
the format ignores when it does not know it, so that a reader that ignores it reads the rest as
before ([`PROTOCOL.md` section 7.2](../PROTOCOL.md#72-rules-for-every-body) and
[section 20](../PROTOCOL.md#20-run-log)). A key a reader must understand, a key removed, renamed
or given another type or meaning, and any change to names or layout bump the number. The bump is
what tells older clients apart: a client compares the number in `FORMAT` with the formats it
implements, and an unknown number is what puts it in token-only mode (below). Two things never
change with a format, so a format change never creates a second budget
([`PROTOCOL.md` section 15.1](../PROTOCOL.md#151-what-every-format-keeps)):

- `admit.lock`, an exclusive `flock` held briefly by whoever changes admission state;
- the token files `res/<name>/<i>`, held by `flock(LOCK_EX)`, released by closing, and counted by
  probing their locks.

A client that finds a format it does not implement runs in **token-only** mode ([`PROTOCOL.md`
section 15.2](../PROTOCOL.md#152-token-only-mode)): under `admit.lock` it takes the minimum of each
declared resource if that many tokens are free, publishes nothing (no ticket, hold, log line or
degraded record), prints a diagnostic and exports `CORESLOT_UNBUDGETED=format` to the command. Its
tokens are counted by every other client, so the budget still holds, but it has no place in the
queue and gets no elastic share. Mixing releases across a format change is therefore safe but
degrades the older clients; upgrade every client of a machine together. The release that introduces
a format writes its number, and its notes say so.

## CLI

Supported: the commands, flags, placeholders, environment variables and exit statuses below.
Human-readable output (`status` without `--json`, `doctor`, diagnostics on stderr) is not a
contract and may change in any release; parse `status --json` instead.

| Surface | Supported |
|---|---|
| `run` | `--need RES=MIN..MAX` (or `RES=N`, `RES=MIN..`, `RES=..MAX`; `mem=SIZE` such as `mem=3G` or `mem=768M`), `--label`, `--priority`, `--timeout`, `--on-timeout run\|fail`, `--no-wait`, `--strict`, `--independent`, `--light`, `--subst`, `--refusal-code`, `--preemptible` (a boolean flag; `--preemptible=false` unmarks); placeholders `{n}`, `{n:RES}`, `{{n}}`, `{{n:RES}}` (`{n:mem}` in MiB) |
| `status` | `--json`, `--observe DURATION` |
| `cancel` | `<ticket-id>`, `--label LABEL` |
| `doctor` | `--clear`, `--reset` |
| `stats` | `--label LABEL`, `--json` (its own `"schema": 1`) |
| `suggest` | `--label LABEL`, `--write` |
| `version` | the output line `coreslot <version> (protocol formats: <list>)` |
| configuration | `<home>/.config/coreslot/config.toml`: `[resources]` (with `cpu` and `mem` declared by default), `[priorities]`, `[platform] windows_warning`, `[memory] live`; `needs.toml` beside it: `[needs."<label>"]` with `cpu`, `mem`, `pinned`, `evidence`, `cpu_per_worker`, `observed`, `preemptible` |
| environment | `CORESLOT_STATE_DIR` (read); `CORESLOT_GRANTED_<RES>` (`CORESLOT_GRANTED_MEM` in MiB), `CORESLOT_HELD_FD`, `CORESLOT_TOKEN_FDS`, `CORESLOT_UNBUDGETED` (given to the command, read by nested runs and by the library) |
| descriptors | the hold at fd 3, tokens from fd 4 in ascending resource name and index |
| exit statuses | the command's own, 130, 75, 64, 126, 127, 70, 3, 1, as [`PROTOCOL.md` section 19](../PROTOCOL.md#19-exit-status) defines them |

### `status --json`, schema 1

The document carries `"schema": 1`. Within schema 1, fields may be added but are never removed,
renamed or given another meaning; a consumer MUST ignore fields it does not know. Removing or
changing a field raises the schema number, and that release prints the new number. Schema 1's
top-level fields are `schema`, `state_dir`, `state_dir_exists`, `format`, `format_present`,
`format_understood`, `format_error`, `gen`, `reconciled_ns`, `config_error`, `degraded`, `section`,
`resources`, `undeclared_locked`, `holds`, `tickets`, `dead_holds`, `cancel_requests`, `ignored`
and `observed_s`. A ticket's `stalled` is `null` unless `--observe` ran. Counts are tokens
everywhere; the `mem` resource alone adds `size_mib`, `locked_mib` and `free_mib`, the same amounts
in MiB. Labels are carried exactly, JSON-encoded.

## Go library

The exported API of the root package, `github.com/core-slot/coreslot`, is the contract, and
[`api/v0.txt`](../api/v0.txt) is its golden file: one line per exported identifier, method
signature and struct field. A test fails when the API and the file differ, or when the Linux,
macOS and Windows builds export different APIs, so every API change is a reviewed change to that
file. Packages under `internal/` are not part of it.

On a target without Unix (Windows among them), `Open` returns `ErrUnsupported`, so no `Client`,
`Waiter` or `Grant` ever exists there. Their methods are declared so that portable programs
compile; each returns `ErrUnsupported` where it returns an error, `Grant.Environ` returns a copy
of its argument, and the others return zero values. AIX is not supported: although it is Unix, the
package does not build there.

## Deprecation window

While at `v0`:

- A flag, command, configuration key, environment variable or exported library identifier is
  first marked **Deprecated** in `CHANGELOG.md` (and, for the CLI, warns on stderr when used), and
  keeps working for at least one further minor release before it is removed.
- The deprecation window does not apply to the on-disk format, because an older client is never
  cut off: a release that changes the metadata bumps the `FORMAT` number (above), is announced in
  `CHANGELOG.md` and `PROTOCOL.md`, and from then on older clients on the same machine run in
  token-only mode, degraded but still counted in the one budget. Before `v1.0.0` this may happen
  in any minor release; from `v1.0.0` it needs a new major version.
- `status --json` changes follow the schema rule above.

From `v1.0.0`: deprecated surfaces keep working until the next major version.

## Platforms

macOS and Linux on amd64 and arm64, built with the two Go releases the Go project supports
(`go.mod` names the older one; 1.26 and 1.27 as of 2026-10). CI runs the tests on Linux amd64,
Linux arm64 and macOS arm64 with both releases; macOS amd64 is built for the release archives but
not tested in CI. Windows builds, but runs commands without a budget.

The Python client in [`conformance/pyclient`](../conformance/pyclient) is a conformance artifact:
it proves `PROTOCOL.md` is sufficient for an independent implementation and is not a supported
distribution.
