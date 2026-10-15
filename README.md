# coreslot

[![CI](https://github.com/core-slot/coreslot/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/core-slot/coreslot/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/core-slot/coreslot.svg)](https://pkg.go.dev/github.com/core-slot/coreslot)
[![Release](https://img.shields.io/github/v/release/core-slot/coreslot?sort=semver)](https://github.com/core-slot/coreslot/releases/latest)
[![License: MIT](https://img.shields.io/github/license/core-slot/coreslot)](LICENSE)

One core budget for every test suite, build and agent on your machine: parallel gates wait their
turn instead of melting the laptop.

Several coding-agent sessions, each in its own worktree, each running its own `pytest -n <cores>`
or `make -j<cores>`, oversubscribe a machine several times over: timing-sensitive tests fail
falsely and every gate runs far slower than it would alone. `coreslot` gives every such command a
share of one machine-wide budget. A command asks for `min..max` cores, waits in one queue (first
come, first served at equal priority) until `min` is free, and runs with what it got. There is no
daemon: the budget is a directory of token files and `flock(2)` locks, so a killed client never
leaks its share.

```sh
coreslot run --need cpu=2..16 --subst -- make -j{n}  # waits for 2 cores, runs with up to 16
coreslot run --need cpu=6 -- pytest -n 12            # 12 workers that keep about 4 cores busy
coreslot status                                      # who holds what, who waits
```

**Status:** `v0`. Before `v1.0.0` the CLI and the library may still change, and a release may
introduce a new on-disk format; the [compatibility policy](docs/compatibility.md) says how.

## Install

With Go 1.26 or newer:

```sh
go install github.com/core-slot/coreslot/cmd/coreslot@latest
```

With Homebrew, from a tap whose formula builds from source
([`packaging/homebrew/coreslot.rb.tmpl`](packaging/homebrew/coreslot.rb.tmpl)):

```sh
brew install core-slot/tap/coreslot
```

Or download a release archive (macOS and Linux, amd64 and arm64) from the
[releases page](https://github.com/core-slot/coreslot/releases). Each release carries
`checksums.txt` and a build-provenance attestation for every archive:

```sh
shasum -a 256 -c checksums.txt --ignore-missing
gh attestation verify coreslot_<version>_darwin_arm64.tar.gz --repo core-slot/coreslot
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

Run a test gate, asking for the cores it keeps busy rather than one per worker. A pytest worker
that mostly waits on I/O uses a fraction of a core; here twelve workers keep about four busy
([choosing a range](#choosing-a-range) says how to measure it):

```sh
coreslot run --need cpu=6 --label gate:unit -- pytest -n 12
```

When the workers are CPU-bound, let the grant set their number instead; an argument that is
exactly `{n}` becomes the number of cores granted:

```sh
coreslot run --need cpu=4..12 --label gate:unit -- pytest -n {n}
```

Run a build; `--subst` also replaces `{n}` inside an argument ([choosing a
range](#choosing-a-range) below says how to pick `2..16`):

```sh
coreslot run --need cpu=2..16 --subst -- make -j{n}
```

Two runs that do not fit side by side queue. On a 10-core budget the second waits until the
first exits, then runs:

```sh
coreslot run --need cpu=8 --label gate:a -- make test &
coreslot run --need cpu=8 --label gate:b -- make test
```

See who holds what and who waits:

```sh
coreslot status            # one line per hold and per ticket; labels are escaped
coreslot status --json     # the same report, "schema": 1
coreslot doctor            # state directory, filesystem, permissions, format, configuration
```

A request whose minimum exceeds the whole budget (`cpu=16..16` on a 10-core budget) is clamped: it
waits for the whole resource, runs alone with all of it, and says so on stderr. `--no-wait` refuses
at once instead of queueing, `--timeout D` bounds the wait, and `--priority P` (from -1000 to 1000;
by default the `[priorities]` entry of the label's longest matching prefix, else 0) places a
request as if it had arrived `P` minutes earlier.
`coreslot cancel <ticket-id>` or `coreslot cancel --label L` withdraws a waiting request.

**Nested runs are pass-through.** A `coreslot run` started inside another run's grant does not
queue: it runs on its parent's tokens, with `min(max, held)`. Nested runs are not budgeted against
each other: two siblings started together under an 8-core grant both run with 8. A parent that
wants concurrent children partitions its grant itself. `--independent` queues as an unrelated
request instead. The Go library applies the same rule ([below](#go-library)).

The command receives `CORESLOT_GRANTED_CPU` (and one `CORESLOT_GRANTED_<RES>` per resource) and
inherits the descriptors that hold its grant
([`PROTOCOL.md` section 17](PROTOCOL.md#17-descriptor-and-environment-contract)). The tokens stay
held until the command exits and coreslot closes its copies; a daemon the command leaves behind
with inherited descriptors keeps them.

### Choosing a range

A token is a core, not a worker. Ask for the cores a command keeps busy, measured on this machine:
each run appends a line to the run log, `log/runs.jsonl` in the state directory, and
(`cpu_user_s` + `cpu_sys_s`) ÷ `ran_s` in it is the cores the run used (the command and the
descendants it waited for; not a database or daemon it talks to). Ask for that times about 1.2. In
the mix measured under [Performance](#performance), a pytest suite's 12 workers kept 2.4 to 4.3
cores busy: asking for 12 tokens left most of the budget idle while the other gates waited, and
with 6 it ran as fast as it did without coreslot
([evidence](evidence/README.md#product-acceptance-acceptancepy)).

`min` is the least the command can usefully run with; `max` is the point past which more
parallelism stops paying, which you find by measuring. `cpu=4..` means `4..16` and `cpu=..8` means
`1..8`; a `max` above the budget simply takes at most the budget.

Keep `min` small. A request waits until `min` tokens are free at once, and while it waits at the
head of the queue nobody behind it is admitted
([`PROTOCOL.md` section 12.1](PROTOCOL.md#121-steps), step 5), so tokens freed in the meantime
stay idle until `min` of them have accumulated. On 8 tokens, a queue of five requests of fixed
sizes 1, 8, 1, 8, 1 runs one after another, with 7 tokens idle during every 1-token run. With the
8-token requests written `4..8`, the first starts at once with 7 beside the 1-token run, and
nothing idles. A grant is fixed for the run's lifetime: it does not grow when tokens free up
later.

Without `--need`, a run whose command has `{n}` (or `{n:cpu}`, or one inside an argument with
`--subst`) asks for `cpu=1..16`, and any other run for `cpu=1..1`: a command that cannot learn its
grant would hold tokens it leaves idle. A command that parallelizes itself without `{n}`, such as
`go test ./...` or `cargo build`, should say `--need cpu=1..16` (or `cpu=1..`) so that it is
counted honestly.

### Exit status

A run exits with its command's own status. Otherwise 75 means refused (`--no-wait`,
`--on-timeout fail`, cancellation, `--strict`; `--refusal-code N` changes it), 64 a usage error,
130 `Ctrl-C` while waiting, 126 / 127 a command that is not executable / not found, and 70 an
internal error. `status` exits 3 while runs fail open, and `doctor` 1 when it finds a problem;
`cancel`, `doctor --clear` and `doctor --reset` exit 75 when they refuse.
[`PROTOCOL.md` section 19](PROTOCOL.md#19-exit-status) has the full table.

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
- **Not a memory budget.** coreslot counts cores and the resources you declare. A command's memory
  is neither reserved nor limited; afterwards the run log records the peak of its largest process.
- **Not for Windows.** coreslot builds there, but runs every command at once, without a budget,
  with a warning.

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
if err != nil {
	return err // a *StateError or ErrUnsupported: the caller decides whether to run unbudgeted
}
defer c.Close()
w, err := c.Enqueue(ctx, coreslot.Request{Label: "make test",
	Need: map[string]coreslot.Range{"cpu": {Min: 2, Max: 8}}})
if err != nil {
	return err
}
g, err := w.Wait(ctx) // or Poll at least every 2 s
if err != nil {
	return err
}
defer g.Close()
cmd := exec.CommandContext(ctx, "make", "test", "-j", strconv.Itoa(g.Granted()["cpu"]))
cmd.ExtraFiles, cmd.Env = g.Files(), g.Environ(os.Environ())
return cmd.Run()
```

Like a nested `coreslot run`, an `Enqueue` made inside another run's grant passes through: a
test suite run under `coreslot run --need cpu=8` that asks the library for cores gets them from
its parent at once instead of queueing behind it. `Options.Independent` queues anyway, like
`--independent`; so does a client opened on another state directory, and `Adopt` never passes
through.

The library never prints, never installs signal handlers and never decides on its own to run
unbudgeted: a `*StateError` tells the caller the budget is unusable. `Open` checks the state
directory before any request, so it returns one even where the parent runs unbudgeted because
that directory is unusable; a caller that wants to follow its parent reads `CORESLOT_UNBUDGETED`.
The [package documentation](https://pkg.go.dev/github.com/core-slot/coreslot) has runnable
examples of `Enqueue`, `Poll`, `Adopt` and `Snapshot`, and of running unbudgeted when the budget
cannot be used; [`api/v0.txt`](api/v0.txt) is the exported API.

## Protocol

[`PROTOCOL.md`](PROTOCOL.md) is the normative definition of format 1: the state directory, its
files and bodies, the admission critical section, and the descriptor and environment contract. Any
client written from it alone shares one budget with every other. An independent Python client
written that way lives in [`conformance/pyclient`](conformance/pyclient).

## Performance

Waiting is cheap: 100 waiting clients (one process each) cost **5.7 % of one core** in total, on
a 14-core Mac, while the queue churns with arrivals, admissions, cancellations and stopped waiters
(median of five 120 s runs, min 5.10 %, max 6.24 %;
[the run](evidence/README.md#the-gate-run-2026-10-12-2bcd53d)). A release is not cut while this
figure is above 10 %. Larger queues:

- **Linux, 1000 waiters:** 361.4 % of one core, measured once inside Docker Desktop's VM on the
  same, loaded Mac.
- **macOS, 1000 waiters:** not measured.
  [A projection](evidence/README.md#projection-to-1000-waiters-on-macos-not-a-measurement) from the
  measured runs gives about 150 % of one core; it is an extrapolation, not a measurement.

On a real mix coreslot cost no measurable time, and it has not yet been shown to save any. Three
test suites and a lint job started together on a busy 14-core Mac, twelve times with coreslot and
twelve without, alternating: the last job finished after a median of 539 s with it and 536 s without
(ratio 1.007; the bound declared before the runs was 1.05), and no job failed. That is a tie, not a
speed-up: on that mix the suites needed about 6 to 9 of the 14 cores, and a mix that the suites
themselves overload is not measured yet. An earlier attempt, with one token per pytest worker,
lost at 1.144, which is why a request asks for the cores a command keeps busy
([choosing a range](#choosing-a-range)).

[`evidence/README.md`](evidence/README.md) has the scripts, the setup and every run, of the
[waiter benchmark](evidence/README.md#waiter-benchmark-the-release-gate) and of the
[product acceptance](evidence/README.md#product-acceptance-acceptancepy).

## More

- [Protocol](PROTOCOL.md)
- [Compatibility policy](docs/compatibility.md) and [changelog](CHANGELOG.md)
- [Contributing](CONTRIBUTING.md), [security](SECURITY.md), [code of conduct](CODE_OF_CONDUCT.md)
- [Third-party licenses](THIRD_PARTY_LICENSES.md)

coreslot sends no telemetry. MIT licensed ([LICENSE](LICENSE)).
