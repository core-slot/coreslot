# Evidence

Every measured claim in the README is backed by a script in this directory. Each record below
names the script, when it ran, on what, and what it printed, verbatim. A record is a reading on
one machine on one day, not a guarantee; rerun the script to check it on yours.

## Docker Desktop and host locks: `probe-docker-flock.sh`

Does a container see a `flock` the host holds on a bind-mounted file? The script prints the host,
Docker and filesystem versions beside one of three results: `NOT VISIBLE`, `VISIBLE` or
`INCONCLUSIVE` (its setup failed, so it claims neither). It needs Docker and Python 3.

### 2026-10-12

```text
$ bash evidence/probe-docker-flock.sh
host: Darwin 27.0.0 arm64; 27.0.1
docker: client 29.8.2, server 29.8.2
backend: Docker Desktop / 7.0.14-linuxkit
filesystem: /dev/disk3s5 on /System/Volumes/Data (apfs, local, journaled, nobrowse, protect, root data)
RESULT: NOT VISIBLE (the container took a lock the host held)
$ echo $?
0
```

Started 2026-10-12T01:49:44Z. The same result as the run of 2026-10-11: on
Docker Desktop for macOS, a container that bind-mounts the state directory does not see the host's
token locks, so containers must take part through a client on the host.

## Benchmark gate

A thousand waiters under bursts of arrivals, admissions, cancellations and stall recoveries must
cost under 2 % of one core in aggregate. Pending re-measurement: no reading is recorded here yet,
and the README makes no performance claim until one is.
