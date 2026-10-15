# Evidence

Every measured claim in the README is backed by a script in this directory or a benchmark program
under `bench/`. Each record below names the script, when it ran, on what, and what it printed,
verbatim. A record is a reading on one machine on one day, not a guarantee; rerun the script to
check it on yours.

Index of the evidence scripts:

- `bench/waiters` and `bench/floor` (with the `*.pprof` profiles in this directory): the waiter
  benchmark, in "Waiter benchmark (the release gate)" below. The profiles were recorded before the
  module moved, so their symbols name `github.com/Nezhinskiy/coreslot`.
- `probe-docker-flock.sh`: whether a container sees the host's flock, in "Docker Desktop and
  host locks" below.
- `acceptance.py`: three test gates and a CI job with and without coreslot, in "Product
  acceptance" below.

## Waiter benchmark (the release gate)

**The gate (set 2026-10-12):** 100 waiters under the bursts of arrivals, admissions, cancellations
and stall recoveries that `bench/waiters` drives ≤ 10 % of one core in aggregate, median of five
120 s runs, on the macOS reference host; failing it blocks a release candidate; the Linux figure
(Docker Desktop VM on the same host) is published, not gated. It is the default of `bench/waiters`; any
other size, such as the published 1000-waiter run, is a measurement with no pass or fail.

It replaces the original gate, a thousand waiters under 2 % of one core (20 us per waiter per
second), which cannot be met: a Go process that only wakes once a second costs more than that.

All figures below are aggregate waiter CPU (user + system) as a percentage of one core over the
measured window. Microseconds are per waiter-second.

### Results by optimization pass

Two optimization passes followed the first measurement, each measured on this host in its own
session:

| Pass | Commit | Changes |
|---|---|---|
| Before | e6c2e31, 2026-10-11 | The election probed every ticket ahead; `Wait` stepped every 250 ms. |
| Pass 1 | ae0fa2c, 2026-10-12 | The election stops at the first live, non-STALLED ticket; a non-head waiter works once per second. |
| Pass 2 | 20a71fe, 2026-10-12 | The front window of 4; re-election inside the admitting section; names-only rescans; holds probed on request; one P while the CLI waits; the harness's gen churn fixed. |

| Platform | Waiters | Before | Pass 1 | Pass 2 |
|---|---|---|---|---|
| macOS | 50 | 9.7 (2.0 ms; a 30 s trial) | 4.40 (892 us) | **3.54** (753 us) |
| macOS | 100, 5 runs: min / median / max | 25.01 / 25.80 / 28.99 | 9.62 / 11.04 / 11.87 | 6.38 / 6.59 / 6.92 (650 to 709 us); after the stop fix, the gate run: **5.10 / 5.70 / 6.24** |
| macOS | 200 | 99.8 (60 s, profiled) | 30.77; 22.54 profiled | **12.45**; 15.58 profiled (629 / 786 us) |
| Linux (Docker VM) | 100 | not run | 13.67 | **15.99** (1631 us) |
| Linux (Docker VM) | 1000 | 889.7 | not run | **361.4** (3618 us); measured once, for publication |
| macOS | 1000 | 1028.4 (a lower bound) | not run | **not run; a projection: about 150** |

**Pass 2 runs.** Each window is 120 s. In every run the wait4 sum equals the harness's
RUSAGE_CHILDREN delta. Host load averages (1/5/15 min) are given at the run's start and end.

| Run | % of one core | us per waiter-second | Arrivals | Admitted | gen, start to end | Seconds with a gen change | Load, start to end |
|---|---|---|---|---|---|---|---|
| n=50 | 3.543 | 753 | 165 | 49 | 58 to 515 | 71 | 13.17 11.16 13.48 to 11.74 11.60 13.36 |
| n=100 #1 | 6.396 | 650 | 145 | 30 | 108 to 505 | 66 | 11.74 11.60 13.36 to 21.09 14.52 14.23 |
| n=100 #2 | 6.834 | 696 | 150 | 32 | 108 to 520 | 67 | 21.09 14.52 14.23 to 26.57 18.98 16.05 |
| n=100 #3 | 6.588 | 672 | 161 | 41 | 108 to 547 | 59 | 26.57 18.98 16.05 to 20.55 19.75 16.79 |
| n=100 #4 | 6.924 | 709 | 157 | 40 | 108 to 538 | 69 | 20.55 19.75 16.79 to 15.40 17.65 16.33 |
| n=100 #5 | 6.377 | 652 | 155 | 38 | 108 to 537 | 70 | 15.40 17.65 16.33 to 13.29 16.69 16.17 |
| n=200 | 12.448 | 629 | 149 | 31 | 208 to 613 | 73 | 13.29 16.69 16.17 to 11.75 14.72 15.43 |
| n=200, profiled | 15.580 | 786 | 145 | 31 | 208 to 608 | 69 | 11.75 14.72 15.43 to 29.69 20.39 17.57 |
| Linux n=100 | 15.992 | 1631 | 150 | 34 | 108 to 520 | 66 | VM: 0.32 0.82 0.89 to 0.40 0.69 0.83 |
| Linux n=1000 | 361.406 | 3618 | 155 | 36 | 1008 to 1410 | 66 | VM: 0.40 0.69 0.83 to 151.21 54.82 20.45 |

Each run also had 60 cancellations, 60 withdrawals, 36 stops and 0 errors.

**The rate of `gen` changes.** The bursts bump `gen` about 3.4 times a second on average: about 400
to 440 bumps in 120 s, from arrivals, admissions and hold removals, the owners' removals after a
cancellation, re-adoption and resume bumps. A waiter reads `gen` once a second, so it rescans at
most once a second. `gen` changed in 59 to 73 of the 120 seconds.

Before pass 2 the harness's re-adoption added far more, but those runs did not record `gen`. It
retried a short adoption every 20 ms, and each retry published and removed a hold, two bumps.

The Linux 1000-waiter run ran inside the Docker Desktop VM, whose 14 CPUs are the shared macOS
host's own: it competed with the host's other work and drove the VM's load to 151 (warm-up 19 s).
It is not run on the macOS host itself, where 1000 waiters are not run on a shared machine.

### Where a waiter's CPU goes (pass 2)

**The floor and the steady waiter**, measured in one session with `bench/floor`: 20 processes per
mode, 60 s each, at a host load of about 20. Each figure is the median in us per process-second.

| Process | Default GOMAXPROCS (14) | GOMAXPROCS=1 |
|---|---|---|
| Go process that sleeps 1 s in a loop, no system calls | **60** | 54 |
| The same plus the 18 system calls a non-head waiter makes each second, made directly | 272 | 239 |
| Library waiter that is not the head, quiet queue (`gen` never changes) | 384 | 370 |
| CLI waiter (`coreslot run`), quiet queue, pass 2 (one P while waiting) | 268 | |
| CLI waiter before pass 2 (14 P), same session | 453 | |

**The split of the n=100 median of 672 us per waiter-second** (macOS):

| Part | us | How it is measured |
|---|---|---|
| Go runtime floor | 60 | The sleep loop. |
| The 18 system calls themselves | 212 | The syscall twin minus the floor. |
| Engine work beyond the twin, quiet queue | 112 | The library waiter minus the twin; includes the front window's 4 mtime reads. |
| Arrivals as whole processes | about 80 | About 1.3 per second at 6.3 ms each, spread over 100 waiters. |
| Bursts | about 210 | The remainder: rescans after `gen` changes, head admission attempts, stops and resumes. |

The two estimates in the table are derived, not measured directly.

**The in-queue profile** is in `waiter-bench-n200-darwin-pass2.pprof`: the middle waiter at n=200,
110 s at 1000 Hz, 97 ms of samples. Shares of the samples:

| Share | What |
|---|---|
| 43 % | Go scheduler (`runtime.schedule`) |
| 20 % | `rescan`, of which `readdir` is 16 % |
| 10 % | `gen` read |
| 8 % | Refresh |
| 5 % | Front window |
| 5 % | Probe of the ticket ahead |
| 4 % | Cancel check |

**The runtime share is not the engine's.** The merged profiles of the syscall twin and the steady
waiter (`floor-syscalls-darwin-20x120s.pprof` and `floor-waiter-darwin-20x120s.pprof`) were taken
in one session at a load of 6 to 9. They give the scheduler the same share: 68 % and 60 % of
samples. In that session the twin cost 643 us and the waiter 688 us.

The scheduler cost belongs to a 14-P Go runtime that makes these system calls, whoever makes them.
It moves with host load: the same twin cost 213, 272 and 643 us in three sessions, and was most
expensive on the quietest host, where idle Ps spin. The analysis after pass 1 put 530 us on the
engine. That compared two sessions under different loads and was wrong.

**What changed in pass 2, each measured on its own** (one run each, shared host):

| Change | Before | After |
|---|---|---|
| Names-only rescans and holds on request, library waiters, n=100 | 10.30 % | 6.30 % |
| One P while the CLI waits, quiet queue (same session as the floor above) | 453 us | 268 us |

The second figure includes the other pass-2 changes, but in a quiet queue only the CLI's one P
and the front window differ.

The 14-P library waiter is now 384 us against 370 us at GOMAXPROCS=1, so the runtime setting
matters less than before. The benchmark's waiters are library waiters with the default GOMAXPROCS;
the library does not change a host program's GOMAXPROCS.

### Projection to 1000 waiters on macOS (not a measurement)

On macOS, the cost per waiter-second hardly grows from 50 to 200 waiters: 753, 672 (median) and
629 to 786 us. On Linux it grows 2.2 times from 100 to 1000 waiters (1631 to 3618 us). The
remaining n-dependent work is the O(n) name listing and parsing of each rescan.

Applying the Linux growth to the macOS median at 100 gives about 1490 us per waiter-second, so
**about 150 % of one core for a thousand waiters on macOS**. A flat per-waiter cost gives a lower
bound of about 67 %. Both are extrapolations.

### The gate run (2026-10-12, 2bcd53d)

The gate, re-run on the macOS reference host after the last fixes of pass 2:

```sh
waiters -coreslot coreslot -dir work    # the default: -n 100 -runs 5 -duration 120s
```

**PASS: min 5.10 / median 5.70 / max 6.24 % of one core.** Against 10 %, the margin is 1.75 times
the median and 1.60 times the worst run.

The earlier pass-2 runs, 6.38 to 6.92 %, came before one fix: their burst stopped the first
live ticket of a snapshot when its waiter was a stall candidate, and otherwise 3 random
candidates; they recorded nothing per burst. The burst now stops the first waiter in queue order
that is a stall candidate (not stopped, leaving or profiled), plus Stall-1 random candidates, and
records the first one's queue position.

| Run | % of one core | us per waiter-second | Arrivals | Admitted | gen, start to end | Seconds with a gen change | Load, start to end |
|---|---|---|---|---|---|---|---|
| 1 | 6.110 | 615 | 132 | 13 | 108 to 459 | 68 | 9.13 8.85 9.41 to 23.89 12.80 10.78 |
| 2 | 6.239 | 627 | 130 | 11 | 108 to 462 | 65 | 23.89 12.80 10.78 to 22.40 16.58 12.59 |
| 3 | 5.696 | 574 | 133 | 14 | 108 to 462 | 65 | 22.40 16.58 12.59 to 29.32 20.85 14.78 |
| 4 | 5.098 | 514 | 134 | 15 | 108 to 469 | 65 | 29.32 20.85 14.78 to 12.34 17.52 14.30 |
| 5 | 5.552 | 560 | 135 | 16 | 108 to 463 | 64 | 12.34 17.52 14.30 to 7.38 14.08 13.38 |

**Stops per burst.** Each run had 12 bursts and 36 stops. The table gives the queue position of the
first waiter each burst stopped.

| Position | Bursts |
|---|---|
| 1 | 5 of 60 (the first burst of each run) |
| 2 | 54 of 60 |
| 3 | 1 of 60 (run 2, last burst) |

Why position 2 dominates is derived from the timing, not recorded: stall-for (12 s) outlasts
burst-every (10 s), so the previous burst's first stopped waiter is usually still stopped.

Admissions are fewer than in the earlier pass-2 runs: 11 to 16 against 30 to 41.

**Caveats on the margin:**

- The floor measurements show the per-process cost moving by a factor of 2 to 3 with host load, and
  it was highest on a quiet host. These runs were at load 7 to 29.
- The Linux VM figure is published, not gated. It measured 15.99 % at 100 waiters in pass 2; its
  tick-sampled rusage reads 2 to 4 times the macOS figures even for the sleep loop.

### Before the fix: the profile that found the quadratic scan

`waiter-bench-n200-darwin.pprof` covers the middle waiter of the n=200 run of 2026-10-11, about 100
tickets ahead of it, for 50 s:

| Share | What |
|---|---|
| 92 % | `engine.(*Ticket).elect` |
| 75 % | `store.Probe`; `open(2)` alone is 72 % |
| 13 % | `lstat(2)` |

The election then read the mtime and probed the lock of every ticket ahead each second, so a waiter
at position k made O(k) system calls per second. `Wait` stepped every 250 ms, and each Step read the
cancel file. The per-ticket scan and the 250 ms cadence were deviations from PROTOCOL.md section
10.3, under which a waiter examines the tickets ahead once per second and stops at the first live,
non-STALLED one; reading the cancel file on each poll is what section 10.3 asks for.

### Setup

- **Host:** Mac16,7, 14 logical CPUs, macOS 27.0.1 (26A434), Darwin 27.0.0, go1.27.2
  darwin/arm64.
- **Linux:** the `golang:1.26` image (go1.26.9 linux/arm64, Debian 13), running on Docker
  Desktop's linuxkit kernel 7.0.14 with 14 CPUs. The run used an unprivileged user, the same way
  `scripts/linux-test.sh` does. `pids.max` was `max`; `ulimit -n` was 1048576 in pass 1 and 1024
  in pass 2, which a waiter (a few descriptors each) never approaches; no limit was raised. On
  macOS, `kern.maxprocperuid` is 8000 and `ulimit -n` is 1048576, so no limit was
  raised there either.
- **Waiters:** each waiter is its own process: `bench/waiters waiter`, built on the public library.
  It calls `Open` (with `CORESLOT_STATE_DIR` and `<statedir>/config.toml`), then
  `Enqueue cpu=1..1`, then `Wait`. Before the gate fix `Wait` stepped every 250 ms; since then it
  steps when the ticket's next work is due (`Ticket.Due`), as `coreslot run` does: once per second
  for a waiter that is not the head, on the 0.25 s to 2 s backoff for the head.
  The waiters use the default `GOMAXPROCS`. Once admitted, a waiter closes its grant and exits.
- **Budget:** `cpu = 8`. Between bursts the harness holds all 8 tokens as eight adopted 1-token
  grants, so the budget is always held. Since pass 2 it re-adopts a token only when `Available`
  reports one, once a second, instead of adopting, finding it short and closing every 20 ms.
- **Bursts:** one every 10 s, starting 5 s into the window (12 bursts in 120 s). Each burst does
  the following:
  1. It publishes 5 cancel requests (`coreslot cancel <id>`) and sends SIGTERM to 5 waiters. On
     SIGTERM the `Wait` context ends and the library withdraws the ticket.
  2. It spawns arrivals to refill the queue to n.
  3. It releases 4 tokens for 3 s. Heads are admitted and exit, which frees the token for the
     next head, and then the harness adopts the tokens back.
  4. At +4 s it sends SIGSTOP to the first waiter in queue order that is a stall candidate (not
     stopped, leaving or profiled), plus Stall-1 (2) random candidates, and sends SIGCONT 12 s
     later. Their successors find them STALLED, and on resume they make the resume bump of `gen`.
- **Window:** the window opens after every one of the n waiters has enqueued, plus 5 s to settle,
  once every waiter has reported its CPU baseline (since pass 2; the CPU between a waiter's report
  and the opening is counted, its wall time is not). The harness reads `gen` at the window's start
  and end and once a second in between, and reports the seconds in which it changed.
- **CPU accounting:** for each waiter, the benchmark takes the kernel's rusage of that child from
  wait4(2) (`ru_utime + ru_stime`). For a waiter that already existed when the window opened, it
  subtracts the waiter's own `getrusage(RUSAGE_SELF)`, which the waiter reports when the harness
  sends SIGUSR1 at the window's start. A waiter that arrives during the window is counted whole,
  including start-up and its enqueue section. The harness's own CPU is not counted. The
  `coreslot cancel` processes are reported separately as control. As a cross-check, the sum of
  wait4 rusage over all children equals the harness's `getrusage(RUSAGE_CHILDREN)` delta for every
  run (above).
- **Load:** the host is shared and heavily loaded (load average 10 to 50 before each run). Rusage
  counts the waiters' own CPU, so it is the right metric under load. The polling cadence is driven
  by the wall clock and does not depend on load, but scheduling delays can shift when the work
  happens. At 1000 waiters the waiters themselves saturate the machine.

### Reproduce

These commands run from the repository root. On a shared host, serialize heavy runs with
whatever lock the host uses, and do not run above 200 waiters there: 1000 waiters saturated it
before the fix.

```sh
B=$(mktemp -d)
go build -o "$B/coreslot" ./cmd/coreslot
go build -o "$B/waiters" ./bench/waiters
go build -o "$B/floor" ./bench/floor
"$B/waiters" -coreslot "$B/coreslot" -dir "$B/work" -n 100 -runs 5 -json "$B/n100.json"   # the 5-run spread
"$B/waiters" -coreslot "$B/coreslot" -dir "$B/work" -n 50 -runs 1
"$B/waiters" -coreslot "$B/coreslot" -dir "$B/work" -n 200 -runs 1
"$B/waiters" -coreslot "$B/coreslot" -dir "$B/work" -n 200 -runs 1 \
	-profile "$B/waiter.pprof" -profile-hz 1000                                   # the in-queue profile
go tool pprof -top -cum "$B/waiters" "$B/waiter.pprof"
for m in sleep refresh cancel gen probe syscalls; do "$B/floor" -mode $m -procs 20 -duration 60s; done
"$B/floor" -mode waiter -procs 20 -duration 120s -cpuprofile "$B/prof"           # steady non-head waiter
GOMAXPROCS=1 "$B/floor" -mode waiter -procs 20 -duration 60s
"$B/floor" -mode cli -coreslot "$B/coreslot" -procs 20 -duration 60s              # coreslot run waiters
go tool pprof -top -cum "$B/floor" "$B"/prof/waiter-*.pprof
```

`waiters` exits 0 when the median meets the gate, 1 when it does not and 2 on an error; its
default, `-n 100 -runs 5 -duration 120s`, is the gate itself, and any other size is a measurement
with no verdict (exit 0). Every child is in one process group, which the
harness kills on every exit path, including its own SIGINT, SIGTERM or SIGHUP; a child whose
harness died sees its stdin pipe close and exits. For the Linux figures the same commands ran
inside `golang:1.26` as an unprivileged user, built there, the way `scripts/linux-test.sh` runs:

```sh
docker run --rm -v "$PWD:/src:ro" -e GOFLAGS=-buildvcs=false golang:1.26 sh -c '
	useradd --create-home tester && cp -r /src /work && chown -R tester:tester /work
	runuser -u tester -- env PATH="$PATH" HOME=/home/tester sh -c "
		cd /work && B=/home/tester/b && mkdir -p \$B &&
		for p in cmd/coreslot bench/waiters bench/floor; do go build -o \$B/\$(basename \$p) ./\$p; done &&
		\$B/waiters -coreslot \$B/coreslot -dir /home/tester/work -n 100 -runs 1
		\$B/waiters -coreslot \$B/coreslot -dir /home/tester/work -n 1000 -runs 1"'
```

The per-family floor modes (pass 1, one session) gave 126 (`refresh`), 103 (`cancel`), 177
(`gen`) and 130 (`probe`) us per process-second against 54 for `sleep`; the families do not add
up, because most of each one's cost is waking a cold process and runtime.

### Other evidence

`probe-docker-flock.sh` checks whether a container sees the host's flock, which decides whether
containers can take part in the host's budget directly or only through a client on the host
(PROTOCOL.md section 4.3).

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

## Product acceptance: `acceptance.py`

**The criterion (README, Status), declared before the first run:** on a developer machine that is
never quiet, three parallel test gates plus one CI job run with and without coreslot as
alternating pairs in the same window, at least three pairs; with coreslot, the median time until
the last job finishes is at most 1.05 × the median without it, and no gate fails falsely.

`acceptance.py run GATES.json OUTDIR FIRST LAST` starts every job of a set at the same instant,
alternates a set without coreslot and a set with it, samples the load average and the memory
pressure level every 5 s, and keeps each set's run-log lines; `acceptance.py summary OUTDIR`
prints the table, the medians and the ratio of each pair. Run 1 used an earlier copy of the same
runner; `summary` over its records prints the numbers below.

### Setup

- **Host:** the reference host (Apple M4 Pro, 14 logical CPUs, 48 GiB, macOS 27.0.1), budget
  `cpu = 14` in a state directory of its own, coreslot built from `a18bbbe`. Never quiet: other
  coding-agent sessions and another project's Docker Desktop VM (about 3.7 cores and 32 GiB when
  sampled) ran throughout, and no new heavy work was started during a run.
- **Jobs**, each in a copy of its working tree:
  - the *service suite*: a Python service's unit tests on pytest-xdist with 12 workers;
  - the *CLI suite*: a Python command-line tool's tests on pytest-xdist, `-n auto` without
    coreslot;
  - the *Go suite*: `go test -race -count=1 ./internal/...` of this repository;
  - the *CI job*: `ruff check` and `mypy` over the CLI suite's tree.
- **Cores used**, from the run log: (`cpu_user_s` + `cpu_sys_s`) ÷ `ran_s`. It counts each command
  and the descendants it waited for.

### Run 1 (2026-10-13): one token per worker, failed

With coreslot each gate asked for a range and ran one worker per granted token:
`--need cpu=4..12 -- pytest -n {n}` (service), `--need cpu=2.. -- pytest -n {n}` (CLI),
`--need cpu=2..8 --subst -- env GOMAXPROCS={n} go test -p {n} …` (Go), `--need cpu=1` (CI).

| Set | Service suite | CLI suite | Go suite | CI job | Last job ends | Peak 1-min load |
|---|---|---|---|---|---|---|
| S1-without | 575.0 | 222.1 | 59.5 | 0.3 | **575.0** | 75 |
| S2-with | 457.2 | 540.9 | 515.1 | 0.5 | **540.9** | 58 |
| S3-without | 487.8 | 222.1 | 59.8 | 0.5 | **487.9** | 91 |
| S4-with | 562.7 | 596.1 | 622.4 | 0.4 | **622.4** | 48 |
| S5-without | 452.0 | 161.0 | 59.3 | 0.4 | **452.0** | 73 |
| S6-with | 378.3 | 542.2 | 58.7 | 380.6 | **542.2** | 21 |
| S7-without | 479.4 | 201.8 | 60.0 | 0.2 | **479.4** | 86 |
| S8-with | 467.7 | 564.0 | 59.7 | 468.7 | **564.0** | 31 |

Times in seconds; the CI job's time includes its wait. No job failed.

```text
median with = 553.1 s, median without = 483.6 s, ratio = 1.144
per pair (with / the preceding without): 0.941, 1.276, 1.199, 1.177
```

**Why it failed.** The service suite arrived first and took 12 of the 14 tokens, but its 12
workers kept only 3.0 to 4.3 cores busy: they mostly wait. The CLI suite got 2 tokens, ran 2
workers and took 540 to 596 s instead of 160 to 222 s; the Go suite (0.06 cores) or the CI job
waited up to 563 s behind a head whose minimum did not fit. A grant is fixed for the run, so
nothing could move. The tokens stood for workers, while the machine has cores.

### Run 2 (2026-10-13): tokens for the cores each gate uses, passed

Each gate asked for the cores it used in run 1, times 1.2, rounded up, and ran its workers
independently of the grant where they mostly wait: `--need cpu=6 -- pytest -n 12` (service),
`--need cpu=4..6 -- pytest -n {n}` (CLI), `--need cpu=1 -- go test -race -count=1 ./internal/...`
(Go), `--need cpu=1` (CI). The maxima sum to 14, so all four start at once. Six pairs were
declared first; six more were declared, before they ran, to be pooled with them for the verdict.

| Set | Service suite | CLI suite | Go suite | CI job | Last job ends | Peak 1-min load | Pressure ≥ 2 |
|---|---|---|---|---|---|---|---|
| S1-without | 721.9 | 227.4 | 61.1 | 0.7 | **721.9** | 113 | 16 % |
| S2-with | 564.5 | 387.4 | 60.8 | 0.4 | **564.5** | 145 | 0 % |
| S3-without | 578.9 | 238.8 | 60.7 | 0.4 | **578.9** | 200 | 0 % |
| S4-with | 536.5 | 299.6 | 58.6 | 0.5 | **536.5** | 65 | 70 % |
| S5-without | 722.1 | 242.6 | 60.4 | 0.5 | **722.1** | 177 | 66 % |
| S6-with | 533.9 | 335.1 | 59.9 | 0.4 | **533.9** | 101 | 0 % |
| S7-without | 436.5 | 204.2 | 59.8 | 0.4 | **436.5** | 82 | 0 % |
| S8-with | 553.4 | 362.5 | 59.5 | 0.4 | **553.4** | 106 | 0 % |
| S9-without | 626.4 | 213.4 | 60.3 | 0.4 | **626.4** | 113 | 0 % |
| S10-with | 642.2 | 358.6 | 59.7 | 0.4 | **642.2** | 194 | 24 % |
| S11-without | 589.4 | 230.2 | 61.7 | 0.5 | **589.4** | 139 | 0 % |
| S12-with | 694.3 | 300.0 | 61.7 | 3.5 | **694.3** | 171 | 81 % |
| S13-without | 555.8 | 197.3 | 60.7 | 0.4 | **555.8** | 71 | 0 % |
| S14-with | 511.4 | 283.1 | 59.8 | 0.5 | **511.4** | 71 | 0 % |
| S15-without | 515.6 | 182.8 | 60.4 | 0.4 | **515.6** | 118 | 0 % |
| S16-with | 542.1 | 324.0 | 59.5 | 0.5 | **542.1** | 86 | 0 % |
| S17-without | 493.1 | 197.3 | 59.7 | 0.4 | **493.1** | 71 | 0 % |
| S18-with | 542.0 | 283.8 | 59.5 | 0.5 | **542.0** | 65 | 0 % |
| S19-without | 514.4 | 244.4 | 60.0 | 0.4 | **514.4** | 76 | 0 % |
| S20-with | 497.4 | 262.7 | 59.7 | 0.5 | **497.4** | 62 | 0 % |
| S21-without | 496.7 | 304.3 | 60.2 | 0.5 | **496.7** | 83 | 0 % |
| S22-with | 409.8 | 279.1 | 59.8 | 0.4 | **409.8** | 74 | 0 % |
| S23-without | 393.2 | 180.4 | 60.0 | 0.4 | **393.2** | 73 | 0 % |
| S24-with | 396.0 | 268.7 | 60.5 | 1.4 | **396.0** | 57 | 0 % |

"Pressure ≥ 2" is the share of 5 s samples at memory pressure level 2 (urgent) or above
(`kern.memorystatus_vm_pressure_level`); the rest were at level 1 (warning). No job failed, and
no request waited more than 3 s. With coreslot the service suite kept 2.4 to 3.9 cores busy on
its 6 tokens and the CLI suite 2.3 to 3.3 on 6.

```text
median with = 539.2 s, median without = 535.7 s, ratio = 1.007
per pair (with / the preceding without): 0.782, 0.927, 0.739, 1.268, 1.025, 1.178, 0.920, 1.051, 1.099, 0.967, 0.825, 1.007
pairs favouring coreslot: 6 of 12
```

**What it shows.** Charging the cores a gate uses, not one token per worker, removed run 1's
loss: the ratio went from 1.144 to 1.007, inside the 1.05 bound, and no job waited. It is a tie,
not a speed-up: six pairs favor each arm, and the run was also declared against a stricter
target, a ratio below 1.00, which it does not meet. On this mix the gates themselves need about 6
to 9 of the 14 cores; the slowdown of the service suite against its 389 s alone came from the
host's other work, which coreslot does not govern.

**What it does not show.** Whether coreslot is faster than free oversubscription on a mix that
the gates themselves overload (several suites each asking for every core), which is the case
coreslot is for; the cores each gate used without coreslot (not recorded); any other host.

### Reproduce

```sh
python3 evidence/acceptance.py run gates.json out 1 6
python3 evidence/acceptance.py summary out
```

`gates.json` is not in the repository: it named local paths, and most of its jobs ran in working
trees of other projects. Write one for your own jobs, following the shape in the script's
docstring, and give it a state directory of its own, so the run is a separate budget and its run
log holds only these jobs.
