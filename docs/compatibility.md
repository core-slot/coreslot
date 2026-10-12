# Compatibility policy

What a coreslot release promises to keep working, and for how long.
[`CHANGELOG.md`](../CHANGELOG.md) records every change against this policy: anything removed or changed incompatibly appears there
under **Removed** or **Changed** first, and under **Deprecated** before that where this policy asks.

## Versions

coreslot is at **`v0`** and stays there until on-disk format 1 is frozen, which happens when the
conformance suite, including its adversarial interleavings, passes on macOS and Linux. Until then a
minor release (`0.x`) may change anything below, within the deprecation window. `v1.0.0` freezes
format 1, the CLI surface and the library API listed here; after it, an incompatible change to any
of them needs a new major version.

## On-disk formats

| Release | Formats implemented |
|---|---|
| every `v0` release so far | 1 |

`coreslot version` prints the formats a binary implements, for example
`coreslot 0.1.0 (protocol formats: 1)`. [`PROTOCOL.md`](../PROTOCOL.md) defines format 1.

A format number versions the metadata: the names, bodies and layout of the state directory other
than `admit.lock` and `res/`. Two things never change with a format, so a format change never
creates a second budget:

- `admit.lock`, an exclusive `flock` held briefly by whoever changes admission state;
- the token files `res/<name>/<i>`, held by `flock(LOCK_EX)`, released by closing, and counted by
  probing their locks.

A client that finds a format it does not implement runs in **token-only** mode: under
`admit.lock` it takes the minimum of each declared resource if that many tokens are free, publishes
nothing (no ticket, hold, log line or degraded record), prints a diagnostic and exports
`CORESLOT_UNBUDGETED=format` to the command. Its tokens are counted by every other client, so the
budget still holds, but it has no place in the queue and gets no elastic share. Mixing releases
across a format change is therefore safe but degrades the older clients; upgrade every client of a
machine together. Writing a newer `FORMAT` is the business of the release that introduces it, and
that release's notes say so.

## CLI

Supported: the commands, flags, placeholders, environment variables and exit statuses below.
Human-readable output (`status` without `--json`, `doctor`, diagnostics on stderr) is not a
contract and may change in any release; parse `status --json` instead.

| Surface | Supported |
|---|---|
| `run` | `--need RES=MIN..MAX` (or `RES=N`), `--label`, `--priority`, `--timeout`, `--on-timeout run\|fail`, `--no-wait`, `--strict`, `--independent`, `--subst`, `--refusal-code`; placeholders `{n}`, `{n:RES}`, `{{n}}`, `{{n:RES}}` |
| `status` | `--json`, `--observe DURATION` |
| `cancel` | `<ticket-id>`, `--label LABEL` |
| `doctor` | `--clear`, `--reset` |
| `version` | the output line `coreslot <version> (protocol formats: <list>)` |
| configuration | `<home>/.config/coreslot/config.toml`: `[resources]`, `[priorities]`, `[platform] windows_warning` |
| environment | `CORESLOT_STATE_DIR` (read); `CORESLOT_GRANTED_<RES>`, `CORESLOT_HELD_FD`, `CORESLOT_TOKEN_FDS`, `CORESLOT_UNBUDGETED` (given to the command, read by nested runs) |
| descriptors | the hold at fd 3, tokens from fd 4 in ascending resource name and index |
| exit statuses | the command's own, 130, 75, 64, 126, 127, 70, 3, 1 (README, "Exit status") |

### `status --json`, schema 1

The document carries `"schema": 1`. Within schema 1, fields may be added but are never removed,
renamed or given another meaning; a consumer MUST ignore fields it does not know. Removing or
changing a field raises the schema number, and that release prints the new number. Schema 1's
top-level fields are `schema`, `state_dir`, `state_dir_exists`, `format`, `format_understood`,
`format_error`, `gen`, `reconciled_ns`, `config_error`, `degraded`, `section`, `resources`,
`undeclared_locked`, `holds`, `tickets`, `dead_holds`, `cancel_requests`, `ignored` and
`observed_s`. A ticket's `stalled` is `null` unless `--observe` ran. Labels are carried exactly,
JSON-encoded.

## Go library

The exported API of the root package, `github.com/Nezhinskiy/coreslot`, is the contract, and
[`api/v0.txt`](../api/v0.txt) is its golden file: one line per exported identifier, method
signature and struct field. A test fails when the API and the file differ, or when the Linux,
macOS and Windows builds export different APIs, so every API change is a reviewed change to that
file. Packages under `internal/` are
not part of it.

On Windows, `Open` returns `ErrUnsupported`, and nothing else is reachable.

## Deprecation window

While at `v0`:

- A flag, command, configuration key, environment variable or exported library identifier is
  first marked **Deprecated** in `CHANGELOG.md` (and, for the CLI, warns on stderr when used), and
  keeps working for at least one further minor release before it is removed.
- A change to format 1 before it is frozen is announced in `CHANGELOG.md` and `PROTOCOL.md` in the
  release that makes it. A client of the older format then runs in token-only mode, as described
  above; it is never silently given a second budget.
- `status --json` changes follow the schema rule above.

From `v1.0.0`: deprecated surfaces keep working until the next major version.

## Platforms

macOS and Linux on amd64 and arm64, built and tested with the two Go releases the Go project
supports (`go.mod` names the older one; 1.26 and 1.27 as of 2026-10). Windows builds, but runs
commands without a budget.

The Python client in [`conformance/pyclient`](../conformance/pyclient) is a conformance artifact:
it proves `PROTOCOL.md` is sufficient for an independent implementation and is not a supported
distribution.
