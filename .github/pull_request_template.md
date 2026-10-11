## What and why

<!-- The change, and the problem it solves. Link the issue if there is one. -->

## How it was checked

<!-- Commands you ran and what they printed. For an invariant, name the mutation that turns its test red. -->

## Checklist

- [ ] `gofmt -l .` is empty and `go mod tidy` leaves no diff
- [ ] `go vet ./...`, `staticcheck ./...` and `govulncheck ./...` are clean
- [ ] `go test -race ./...` and `go test -tags coreslot_testhooks -race -short ./conformance/` pass
- [ ] `GOOS=windows go build ./...` passes
- [ ] On-disk format or `PROTOCOL.md` changes are called out above, and the Python client still passes
- [ ] No new dependency (or the owner approved it)
