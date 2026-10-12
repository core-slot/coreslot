# coreslot

One core budget for every test suite, build and agent on your machine: parallel gates wait their
turn instead of melting the laptop.

**Status:** release candidate; product acceptance
([design §10.5](PROTOCOL.md#10-verification)) pending. The on-disk
format 1 is not frozen until the conformance suite passes on macOS and Linux, and the module stays
at `v0` until it is ([compatibility policy](docs/compatibility.md)).

Several coding-agent sessions, each in its own worktree, each running its own `pytest -n <cores>`
or `make -j<cores>`, oversubscribe a machine several times over: timing-sensitive tests fail
falsely and every gate runs far slower than it would alone. `coreslot` gives every such command a
share of one machine-wide budget. A command asks for `min..max` cores, waits in one queue (first
come, first served at equal priority) until `min` is free, and runs with what it got. There is no
daemon: the budget is a directory of token files and `flock(2)` locks, so a killed client never
leaks its share.

## Install

With Go 1.26 or newer:

```sh
go install github.com/Nezhinskiy/coreslot/cmd/coreslot@latest
```

With Homebrew, from a tap whose formula builds from source
([`packaging/homebrew/coreslot.rb.tmpl`](packaging/homebrew/coreslot.rb.tmpl)):

```sh
brew install Nezhinskiy/tap/coreslot
```

Or download a release archive (macOS and Linux, amd64 and arm64) from the
[releases page](https://github.com/Nezhinskiy/coreslot/releases). Each release carries
`checksums.txt`; releases of the public repository also carry a build-provenance attestation for
every archive:

```sh
shasum -a 256 -c checksums.txt --ignore-missing
gh attestation verify coreslot_<version>_darwin_arm64.tar.gz --repo Nezhinskiy/coreslot
tar -xzf coreslot_<version>_darwin_arm64.tar.gz
```

The binaries are not signed with an Apple Developer ID. Binaries installed with `go install` or
Homebrew are not quarantined, but an archive downloaded with a browser is: if macOS refuses to run
the extracted binary, clear the attribute once:

```sh
xattr -d com.apple.quarantine ./coreslot
```

`coreslot version` prints the version and the protocol formats it implements.

## Quick start

Run a test gate with between 4 and `W` cores, where `W` is the best worker count you measured on
this machine; an argument that is exactly `{n}` becomes the number of cores granted:

```sh
coreslot run --need cpu=4..W --label gate:unit -- pytest -n {n}
```

Run a build; `--subst` also replaces `{n}` inside an argument. Without `--need` a run asks for
`cpu=1..1`, so give a build its range:

```sh
coreslot run --need cpu=2..16 --subst -- make -j{n}
```

See who holds what and who waits:

```sh
coreslot status            # one line per hold and per ticket; labels are escaped
coreslot status --json     # the same report, "schema": 1
coreslot doctor            # state directory, filesystem, permissions, format, configuration
```

A request whose minimum exceeds the whole budget (`cpu=16..16` on a 10-core budget) is clamped: it
waits for the whole resource, runs alone with all of it, and says so on stderr. `--no-wait` refuses
at once instead of queueing, `--timeout D` bounds the wait, and `--priority P` (from -1000 to 1000,
default 0) places a request as if it had arrived `P` minutes earlier.
`coreslot cancel <ticket-id>` or `coreslot cancel --label L` withdraws a waiting request.

**Nested runs are pass-through.** A `coreslot run` started inside another run's grant does not
queue: it runs on its parent's tokens, with `min(max, held)`. Nested runs are not budgeted against
each other: two siblings started together under an 8-core grant both run with 8. A parent that
wants concurrent children partitions its grant itself. `--independent` queues as an unrelated
request instead.

The command receives `CORESLOT_GRANTED_CPU` (and one `CORESLOT_GRANTED_<RES>` per resource), the
hold's descriptor at fd 3 and the token descriptors from fd 4. The tokens stay held until the
command exits and coreslot closes its copies; a daemon the command leaves behind with inherited
descriptors keeps them.

### Exit status

| Status | Meaning |
|---|---|
| the command's own | the command ran (`128 + signal` when it was killed) |
| 130 | interrupted with `Ctrl-C` while waiting (another signal gives `128 + signal`) |
| 75 | refused (`--no-wait`, `--on-timeout fail`, cancellation, `--strict`); `--refusal-code N` changes it |
| 64 | usage error |
| 126 / 127 | the command is not executable / not found |
| 70 | internal error before the command could run |
| 3 | `status` or `doctor`: degraded mode (`coreslot doctor --clear` clears the record) |
| 1 | `doctor` found a problem |

When the state directory cannot be used (a network filesystem, wrong permissions, an invalid
configuration), `run` fails open: it runs the command at once with its minimum, says so on stderr,
records degraded mode, and exports `CORESLOT_UNBUDGETED`. `--strict` refuses instead.

## Configuration

Without a configuration file the budget is `cpu = "100%"` of the machine. To change it, write
`~/.config/coreslot/config.toml` (the home directory comes from the password database, not
`$HOME`):

```toml
[resources]
cpu = "-2"        # an integer; "N%" of the CPU count; or "-K" for all but K
pg-local = 2      # any other resource, for example a shared local database

[priorities]      # label prefix -> priority; --priority overrides
"ci:" = 0
"interactive:" = 10
```

A suite that also needs the shared database adds `--need pg-local=1`. The state lives in
`~/Library/Application Support/coreslot/` on macOS and `~/.local/state/coreslot/` on Linux.
`CORESLOT_STATE_DIR` overrides it for tests and moves the configuration to
`$CORESLOT_STATE_DIR/config.toml` with it: an override is a separate budget.

## What it is not

- **Not enforcement.** coreslot is cooperative admission control. It sets no cgroup quota, `nice`
  value or CPU affinity; a command that ignores its grant, or a tool that never asks, is not stopped
  or counted. A hostile process of the same user can hoard tokens or corrupt the state
  ([SECURITY.md](SECURITY.md)).
- **Not a daemon.** Nothing runs between commands; there is no network and no distribution of work
  across machines.
- **Not multi-user.** One budget per user; never share the state directory across users or hosts.
  Memory is not a resource in v1, and on Windows commands run without a budget, with a warning.

### Containers

A `coreslot run` inside a container sees the container's filesystem and therefore its own budget.
Containers take part through a client on the host. On Docker Desktop for macOS a lock the host
holds on a bind-mounted file is not visible inside the container
([`evidence/probe-docker-flock.sh`](evidence/probe-docker-flock.sh), recorded in
[`evidence/README.md`](evidence/README.md)).

## Go library

The module's root package implements the protocol for other Go programs, such as a CI runner pool:

```go
c, err := coreslot.Open(coreslot.Options{})
w, err := c.Enqueue(coreslot.Request{Label: "ci:o-r",
	Need: map[string]coreslot.Range{"cpu": {Min: 3, Max: 3}}})
g, err := w.Wait(ctx) // or Poll at least every 2 s
cmd.ExtraFiles, cmd.Env = g.Files(), append(env, g.Env()...)
// ... run the command ...
g.Close()
```

The library never prints, never installs signal handlers and never decides to run unbudgeted: a
`*StateError` tells the caller the budget is unusable. The package documentation has the full
example; [`api/v0.txt`](api/v0.txt) is the exported API.

## Protocol

[`PROTOCOL.md`](PROTOCOL.md) is the normative definition of format 1: the state directory, its
files and bodies, the admission critical section, and the descriptor and environment contract. Any
client written from it alone shares one budget with every other. An independent Python client
written that way lives in [`conformance/pyclient`](conformance/pyclient).

## Performance

The release gate ([design §10.4](PROTOCOL.md#10-verification)) is 100
waiters under the stated bursts of arrivals, admissions, cancellations and stall recoveries, at
most 10 % of one core in aggregate, as the median of five 120 s runs on the macOS reference host.
All figures are from [`evidence/README.md`](evidence/README.md#waiter-benchmark-design-section-104),
which has the scripts, the setup and every run:

- **The gate:** measured
  [5.70 % of one core](evidence/README.md#the-gate-run-2026-10-12-a56c198) (min 5.10 %, max
  6.24 %): pass.
- **Linux, 1000 waiters:** 361.4 % of one core, measured once inside Docker Desktop's VM on the
  same loaded host; published, not gated.
- **macOS, 1000 waiters:** not measured.
  [A projection](evidence/README.md#projection-to-1000-waiters-on-macos-not-a-measurement) from the
  measured runs gives about 150 % of one core; it is an extrapolation, not a measurement.

## More

- [Design](PROTOCOL.md) and [protocol](PROTOCOL.md)
- [Compatibility policy](docs/compatibility.md) and [changelog](CHANGELOG.md)
- [Contributing](CONTRIBUTING.md), [security](SECURITY.md), [code of conduct](CODE_OF_CONDUCT.md)
- [Third-party licenses](THIRD_PARTY_LICENSES.md)

coreslot sends no telemetry. MIT licensed ([LICENSE](LICENSE)).
