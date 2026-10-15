## What and why

<!-- The change, and the problem it solves. Link the issue if there is one. -->

## How it was checked

<!-- Commands you ran and what they printed. For a new invariant test, the mutation that turns it red and what the test printed then. -->

## Checklist

The checks `CONTRIBUTING.md` lists under "Test", in the order CI runs them:

- [ ] `gofmt -l .` is empty and `go mod tidy` leaves no diff
- [ ] `go vet` and `staticcheck` are clean on `./...`, with and without `-tags coreslot_testhooks`, and `govulncheck ./...` is clean
- [ ] `go test -race ./...` and `go test -tags coreslot_testhooks -race -short ./...` pass
- [ ] `python3 -m unittest` passes in `conformance/pyclient`
- [ ] `GOOS=windows go build ./...` passes
- [ ] On-disk format or `PROTOCOL.md` changes are called out above
- [ ] No new dependency (or a maintainer approved it)
- [ ] User-visible changes have an entry under **Unreleased** in `CHANGELOG.md`
