# Contributing

Thanks for helping. Issues and pull requests are welcome; for anything larger than a fix, open an
issue first so the design can be settled before the code. Security problems go through
[SECURITY.md](SECURITY.md), not a public issue. Everyone follows the
[code of conduct](CODE_OF_CONDUCT.md).

## Ground rules

- **The protocol comes first.** [`PROTOCOL.md`](PROTOCOL.md) is normative. A change to on-disk
  behaviour changes `PROTOCOL.md` in the same pull request, keeps its contract inventory (section 2)
  complete, and is called out in the description; the [compatibility policy](docs/compatibility.md)
  says what a release may change.
- **No new dependencies** beyond the standard library, `golang.org/x/sys` and
  `github.com/BurntSushi/toml` (`github.com/creack/pty` in tests only) without the maintainer's
  agreement. A dependency change regenerates [`THIRD_PARTY_LICENSES.md`](THIRD_PARTY_LICENSES.md).
- **No telemetry.** coreslot opens no network connection and records nothing outside the user's
  state directory. The run log never records argv or the environment.
- **The library stays quiet.** It never prints, never installs signal handlers, never chooses exit
  codes and never decides to run unbudgeted (`library_purity_test.go` checks the first three).
- **Windows builds.** `internal/store`, `internal/engine` and the root package's implementation are
  `//go:build unix`; the Windows build must keep compiling.
- **Every invariant-bearing test names the mutation that turns it red.** Run that mutation before
  you push, and say in the pull request what it printed. An assertion nobody has seen fail is not
  coverage.
- English for all prose, comments and messages.

## Build

Go 1.26 or newer (`go.mod` names the minimum; CI also runs the latest release).

```sh
go build ./...
go build -o coreslot ./cmd/coreslot
GOOS=windows go build ./...
```

## Test

The checks CI runs, in the order it runs them:

```sh
gofmt -l .                                      # prints nothing
go mod tidy && git diff --exit-code go.mod go.sum
go vet ./... && go vet -tags coreslot_testhooks ./...
staticcheck ./... && staticcheck -tags coreslot_testhooks ./...
govulncheck ./...
go test -race ./...
go test -tags coreslot_testhooks -race -short ./conformance/
(cd conformance/pyclient && python3 -m unittest)
GOOS=windows go build ./...
```

The pinned versions of `staticcheck` and `govulncheck` are in
[`.github/workflows/ci.yml`](.github/workflows/ci.yml).

- **Test hooks.** Pause points for deterministic traces compile only with the build tag
  `coreslot_testhooks` and are inert without it. `go test -tags coreslot_testhooks ./...` runs the
  hook-dependent tests of every package.
- **Conformance.** `./conformance/` builds the CLI and a helper client with the test-hook tag and
  drives real processes through the queue traces, against temporary state directories whose paths
  contain a space.
- **Fuzzing.** CI discovers every `Fuzz` function in every package (`go test -list '^Fuzz'`) and
  fuzzes each for 30 s on Linux, so a new target needs no workflow change. One target runs with
  `go test -run '^$' -fuzz '^FuzzName$' -fuzztime 30s ./path/to/package/`.
- **The exported API.** `api/v0.txt` pins the root package's API. After a deliberate API change,
  `CORESLOT_UPDATE_API=1 go test -run TestExportedAPIGolden .` rewrites it; review the diff.
- **The Python client.** [`conformance/pyclient`](conformance/pyclient) is an independent client
  written from `PROTOCOL.md` alone (Python 3.11 or newer, standard library only). It must keep
  passing after any protocol change; a place where `PROTOCOL.md` is silent or ambiguous goes into
  its `AMBIGUITIES.md` and is fixed in `PROTOCOL.md`, not resolved by reading the Go code.

### Linux from a Mac

`scripts/linux-test.sh` runs `go test` in the `golang` image of `go.mod`'s minimum version, as an
unprivileged user with a real passwd entry and its own home, so permission-denial behaviour matches
a developer machine rather than root's. It needs Docker and passes its arguments to `go test`:

```sh
scripts/linux-test.sh -race ./...
scripts/linux-test.sh -tags coreslot_testhooks -race -short ./conformance/
```

### State directories in tests

Never point a test at your real budget. `CORESLOT_STATE_DIR` (an absolute path) gives a separate
budget and moves the configuration to `$CORESLOT_STATE_DIR/config.toml` with it:

```sh
CORESLOT_STATE_DIR="$(mktemp -d)/state" \
  ./coreslot run --need cpu=2 -- sh -c 'echo $CORESLOT_GRANTED_CPU'
```

## Pull requests

Fill in the template: what changed and why, the commands you ran with what they printed, and the
mutation that reddens each new invariant test. Add an entry under **Unreleased** in
[`CHANGELOG.md`](CHANGELOG.md) for anything a user or a client implementer would notice.

## Releases

Releases are cut by the maintainer by pushing a `v*` tag. The release workflow reruns CI on the
tagged commit, builds the archives with GoReleaser (`.goreleaser.yaml`) and attests their build
provenance. `goreleaser release --snapshot --clean` builds the same archives locally into `dist/`
without publishing anything.
