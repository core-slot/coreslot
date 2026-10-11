#!/usr/bin/env bash
# Does a container on this host see the host's flock on a bind-mounted file?
#
# The host takes an exclusive flock on a file and keeps it until this script exits; a container
# that bind-mounts the same directory tries a non-blocking flock on it. Three outcomes:
#
#   NOT VISIBLE   the container took the lock while the host still held it
#   VISIBLE       the container was refused with flock's contention status (1)
#   INCONCLUSIVE  anything else: the holder was not ready or died, the image or docker failed,
#                 flock exited with another status
#
# Observed on Docker Desktop for macOS on 2026-10-11: NOT VISIBLE (design §7.1). The script
# prints the host, Docker and filesystem versions beside the result.
set -uo pipefail

dir=$(mktemp -d)
holder=""
cleanup() {
  [ -n "$holder" ] && kill "$holder" 2>/dev/null && wait "$holder" 2>/dev/null
  rm -rf "$dir"
}
trap cleanup EXIT

inconclusive() { echo "RESULT: INCONCLUSIVE ($1)"; exit 2; }

# The holder writes `ready` only after it holds the lock, then waits for `done` to exist.
python3 -c '
import fcntl, os, sys, time
d = sys.argv[1]
handle = open(os.path.join(d, "token"), "w")
fcntl.flock(handle, fcntl.LOCK_EX)
open(os.path.join(d, "ready"), "w").close()
while not os.path.exists(os.path.join(d, "done")):
    time.sleep(0.1)
' "$dir" &
holder=$!

for _ in $(seq 1 100); do [ -e "$dir/ready" ] && break; sleep 0.1; done
[ -e "$dir/ready" ] || inconclusive "holder never acquired the lock"

echo "host: $(uname -srm); $(sw_vers -productVersion 2>/dev/null || true)"
echo "docker: $(docker version --format 'client {{.Client.Version}}, server {{.Server.Version}}' 2>/dev/null)"
echo "backend: $(docker info --format '{{.OperatingSystem}} / {{.KernelVersion}}' 2>/dev/null)"
mountpoint=$(df -P "$dir" | awk 'NR == 2 { print $6 }')
echo "filesystem: $(mount | grep " on $mountpoint " | head -1)"

docker image inspect alpine:3.20 >/dev/null 2>&1 || docker pull -q alpine:3.20 >/dev/null 2>&1 \
  || inconclusive "could not pull alpine:3.20"

docker run --rm -v "$dir:/probe" alpine:3.20 flock -n /probe/token true
status=$?

kill -0 "$holder" 2>/dev/null || inconclusive "the holder died before the attempt finished"

case "$status" in
  0) echo "RESULT: NOT VISIBLE (the container took a lock the host held)" ;;
  1) echo "RESULT: VISIBLE (the container was refused)" ;;
  *) inconclusive "docker or flock exited $status" ;;
esac
touch "$dir/done"
