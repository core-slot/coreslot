#!/bin/sh
# Runs `go test` on Linux, in the golang image of go.mod's minimum version, as an unprivileged
# user with a real passwd entry and its own home, GOCACHE and GOPATH, so permission-denial
# behaviour matches a developer machine rather than root's.
#
# Usage: scripts/linux-test.sh [go test arguments]     e.g. scripts/linux-test.sh -race ./...
#
# Refuses (exit 2) while the local CI runner's containers (name local-ci-*) are running.
set -eu
cd "$(dirname "$0")/.."

if [ -n "$(docker ps --filter name=local-ci- -q)" ]; then
	echo "linux-test.sh: refusing: local CI runner containers (local-ci-*) are running; retry when they finish" >&2
	exit 2
fi

version=$(sed -n 's/^go \([0-9][0-9.]*\)$/\1/p' go.mod)
if [ -z "$version" ]; then
	echo "linux-test.sh: no go directive in go.mod" >&2
	exit 2
fi

exec docker run --rm -v "$PWD:/src:ro" -w /src -e GOFLAGS=-buildvcs=false "golang:$version" sh -c '
	set -e
	useradd --create-home --shell /bin/sh tester
	cp -r /src /work
	chown -R tester:tester /work
	cd /work
	exec runuser -u tester -- env PATH="$PATH" GOFLAGS="$GOFLAGS" HOME=/home/tester \
		GOCACHE=/home/tester/.cache/go-build GOPATH=/home/tester/go go test "$@"
' sh "$@"
