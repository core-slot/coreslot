#!/usr/bin/env bash
# Does a container on this host see the host's flock on a bind-mounted file?
#
# The host holds an exclusive flock on a file; a container that bind-mounts the same directory
# tries a non-blocking flock on it. On Docker Desktop for macOS (observed 2026-10-11) the
# container gets the lock: host locks are invisible inside containers, so a coreslot client in a
# container would see a separate budget (design §7.1).
set -euo pipefail

dir=$(mktemp -d)
trap 'kill "${holder:-0}" 2>/dev/null || true; wait "${holder:-0}" 2>/dev/null || true; rm -rf "$dir"' EXIT

python3 -c '
import fcntl, sys, time
handle = open(sys.argv[1], "w")
fcntl.flock(handle, fcntl.LOCK_EX)
print("host: holding the lock", flush=True)
time.sleep(60)
' "$dir/token" &
holder=$!
sleep 1

docker run --rm -v "$dir:/probe" alpine:3.20 sh -c \
  'if flock -n /probe/token true; then echo "container: got the lock (host locks NOT visible)"; else echo "container: blocked (host locks visible)"; fi'
