# Evidence

## Waiter benchmark (design section 10.4)

**The gate (ruled 2026-10-12, design section 10.4):** 100 waiters under the stated bursts ≤ 10 %
of one core, median of five 120 s runs, on the macOS reference host; the Linux figure (Docker
Desktop VM on the same host) is published, not gated. It is the default of `bench/waiters`; any
other size, such as the published 1000-waiter run, is a measurement with no pass or fail.

It replaces the original gate, a thousand waiters under 2 % of one core (20 us per waiter per
second), which cannot be met: a Go process that only wakes once a second costs more than that.

All figures below are aggregate waiter CPU (user + system) as a percentage of one core over the
measured window. Microseconds are per waiter-second.

### Results by round

The rounds are measured on this host, each in its own session:

| Round | Commit | Changes |
|---|---|---|
| Before | 2b40885, 2026-10-11 | The election probed every ticket ahead; `Wait` stepped every 250 ms. |
| Round 1 | a677509 | The election stops at the first live, non-STALLED ticket; a non-head waiter works once per second. |
| Round 2 | 7806513 | The front window of 4; re-election inside the admitting section; names-only rescans; holds probed on request; one P while the CLI waits; the harness's gen churn fixed. |

| Platform | Waiters | Before | Round 1 | Round 2 |
|---|---|---|---|---|
| macOS | 50 | 9.7 (2.0 ms; a 30 s trial) | 4.40 (892 us) | **3.54** (753 us) |
| macOS | 100, 5 runs: min / median / max | 25.01 / 25.80 / 28.99 | 9.62 / 11.04 / 11.87 | 6.38 / 6.59 / 6.92 (650 to 709 us); after the stop fix, the gate run: **5.10 / 5.70 / 6.24** |
| macOS | 200 | 99.8 (60 s, profiled) | 30.77; 22.54 profiled | **12.45**; 15.58 profiled (629 / 786 us) |
| Linux (Docker VM) | 100 | not run | 13.67 | **15.99** (1631 us) |
| Linux (Docker VM) | 1000 | 889.7 | not run | **361.4** (3618 us); measured once, for publication |
| macOS | 1000 | 1028.4 (a lower bound) | not run | **not run; a projection: about 150** |

**Round 2 runs.** Each window is 120 s. In every run the wait4 sum equals the harness's
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

Before round 2 the harness's re-adoption added far more, but those runs did not record `gen`. It
retried a short adoption every 20 ms, and each retry published and removed a hold, two bumps.

The Linux 1000-waiter run ran inside the Docker Desktop VM, whose 14 CPUs are the shared macOS
host's own: it competed with the host's other work and drove the VM's load to 151 (warm-up 19 s).
It is not run on the macOS host itself, where 1000 waiters are not run on a shared machine.

### Where a waiter's CPU goes (round 2)

**The floor and the steady waiter**, measured in one session with `bench/floor`: 20 processes per
mode, 60 s each, at a host load of about 20. Each figure is the median in us per process-second.

| Process | Default GOMAXPROCS (14) | GOMAXPROCS=1 |
|---|---|---|
| Go process that sleeps 1 s in a loop, no system calls | **60** | 54 |
| The same plus the 18 system calls a non-head waiter makes each second, made directly | 272 | 239 |
| Library waiter that is not the head, quiet queue (`gen` never changes) | 384 | 370 |
| CLI waiter (`coreslot run`), quiet queue, round 2 (one P while waiting) | 268 | |
| CLI waiter before round 2 (14 P), same session | 453 | |

**The split of the n=100 median of 672 us per waiter-second** (macOS):

| Part | us | How it is measured |
|---|---|---|
| Go runtime floor | 60 | The sleep loop. |
| The 18 system calls themselves | 212 | The syscall twin minus the floor. |
| Engine work beyond the twin, quiet queue | 112 | The library waiter minus the twin; includes the front window's 4 mtime reads. |
| Arrivals as whole processes | about 80 | About 1.3 per second at 6.3 ms each, spread over 100 waiters. |
| Bursts | about 210 | The remainder: rescans after `gen` changes, head admission attempts, stops and resumes. |

The two estimates in the table are derived, not measured directly.

**The in-queue profile** is in `waiter-bench-n200-darwin-round2.pprof`: the middle waiter at n=200,
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
expensive on the quietest host, where idle Ps spin. Round 1 put 530 us on the engine. That compared
two sessions under different loads and was wrong.

**What changed in round 2, each measured on its own** (one run each, shared host):

| Change | Before | After |
|---|---|---|
| Names-only rescans and holds on request, library waiters, n=100 | 10.30 % | 6.30 % |
| One P while the CLI waits, quiet queue (same session as the floor above) | 453 us | 268 us |

The second figure includes the other round-2 changes, but in a quiet queue only the CLI's one P
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

### The gate run (2026-10-12, a56c198)

The gate, re-run after the review fixes of round 2, on the macOS reference host:

```sh
waiters -coreslot coreslot -dir work    # the default: -n 100 -runs 5 -duration 120s
```

**PASS: min 5.10 / median 5.70 / max 6.24 % of one core.** Against 10 %, the margin is 1.75 times
the median and 1.60 times the worst run.

The earlier round-2 runs, 6.38 to 6.92 %, came before one fix. The burst stopped the queue head
only once per run: with stall-for 12 s longer than burst-every 10 s, the previous head was still
stopped, so the other two of the three stops went to random waiters. The burst now stops the first
waiter in queue order it may act on, and records its position.

| Run | % of one core | us per waiter-second | Arrivals | Admitted | gen, start to end | Seconds with a gen change | Load, start to end |
|---|---|---|---|---|---|---|---|
| 1 | 6.110 | 615 | 132 | 13 | 108 to 459 | 68 | 9.13 8.85 9.41 to 23.89 12.80 10.78 |
| 2 | 6.239 | 627 | 130 | 11 | 108 to 462 | 65 | 23.89 12.80 10.78 to 22.40 16.58 12.59 |
| 3 | 5.696 | 574 | 133 | 14 | 108 to 462 | 65 | 22.40 16.58 12.59 to 29.32 20.85 14.78 |
| 4 | 5.098 | 514 | 134 | 15 | 108 to 469 | 65 | 29.32 20.85 14.78 to 12.34 17.52 14.30 |
| 5 | 5.552 | 560 | 135 | 16 | 108 to 463 | 64 | 12.34 17.52 14.30 to 7.38 14.08 13.38 |

**Stops per burst.** Each run had 12 bursts and 36 stops. The table gives the queue position of the
first waiter each burst stopped.

| Position | Bursts | What it means |
|---|---|---|
| 1 | 5 of 60 (the first burst of each run) | The queue head itself. |
| 2 | 54 of 60 | The waiter right behind the previous head, which was still stopped and occupied position 1: the head of the queue that is not stopped. |
| 3 | 1 of 60 | Run 2, last burst. |

Admissions are fewer than in the earlier round-2 runs (11 to 16 against 30 to 41). Each burst now
stops the waiter that would be admitted next, so the following ones wait the 10 s stall first.

**Caveats on the margin:**

- The floor measurements show the per-process cost moving by a factor of 2 to 3 with host load, and
  it was highest on a quiet host. These runs were at load 7 to 29.
- The Linux VM figure is published, not gated. It measured 15.99 % at 100 waiters in round 2; its
  tick-sampled rusage reads 2 to 4 times the macOS figures even for the sleep loop.

### Before the fix: the profile that found the quadratic scan

`waiter-bench-n200-darwin.pprof` covers the middle waiter of the n=200 run of 2026-10-11, about 100
tickets ahead of it, for 50 s:

| Share | What |
|---|---|
| 92 % | `engine.(*Ticket).elect` |
| 75 % | `store.Probe`; `open(2)` alone is 72 % |
| 13 % | `lstat(2)` |

The election then read the mtime and probed the lock of every ticket ahead each second, so a
waiter at position k made O(k) system calls per second. `Wait` stepped every 250 ms, and each Step
read the cancel file. Both were deviations from PROTOCOL.md section 10.3 and design section 5.7.

### Setup

- **Host:** Mac16,7, 14 logical CPUs, macOS 27.0.1 (26A434), Darwin 27.0.0, go1.27.2
  darwin/arm64.
- **Linux:** the `golang:1.26` image (go1.26.9 linux/arm64, Debian 13), running on Docker
  Desktop's linuxkit kernel 7.0.14 with 14 CPUs. The run used an unprivileged user, the same way
  `scripts/linux-test.sh` does. `pids.max` was `max`; `ulimit -n` was 1048576 in round 1 and 1024
  in round 2, which a waiter (a few descriptors each) never approaches; no limit was raised. On
  macOS, `kern.maxprocperuid` is 8000 and `ulimit -n` is 1048576, so no limit was
  raised there either.
- **Waiters:** each waiter is its own process: `bench/waiters waiter`, built on the public library.
  It calls `Open` (with `CORESLOT_STATE_DIR` and `<statedir>/config.toml`), then
  `Enqueue cpu=1..1`, then `Wait`. Before the gate fix `Wait` stepped every 250 ms; since then it
  steps when the ticket's next work is due (`Ticket.Due`), as `coreslot run` does: once per second
  for a waiter that is not the head, on the 0.25 s to 2 s backoff for the head.
  The waiters use the default `GOMAXPROCS`. Once admitted, a waiter closes its grant and exits.
- **Budget:** `cpu = 8`. Between bursts the harness holds all 8 tokens as eight adopted 1-token
  grants, so the budget is always held. Since round 2 it re-adopts a token only when `Free`
  reports one, once a second, instead of adopting, finding it short and closing every 20 ms.
- **Bursts:** one every 10 s, starting 5 s into the window (12 bursts in 120 s). Each burst does
  the following:
  1. It publishes 5 cancel requests (`coreslot cancel <id>`) and sends SIGTERM to 5 waiters. On
     SIGTERM the `Wait` context ends and the library withdraws the ticket.
  2. It spawns arrivals to refill the queue to n.
  3. It releases 4 tokens for 3 s. Heads are admitted and exit, which frees the token for the
     next head, and then the harness adopts the tokens back.
  4. At +4 s it sends SIGSTOP to the queue head (the first live ticket of a snapshot) and 2 random
     waiters, and sends SIGCONT 12 s later. Their successors find them STALLED, and on resume they make the resume bump of `gen`.
- **Window:** the window opens after every one of the n waiters has enqueued, plus 5 s to settle,
  once every waiter has reported its CPU baseline (since round 2; the CPU between a waiter's report
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
default, `-n 1000 -runs 5`, is the gate itself. Every child is in one process group, which the
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

The per-family floor modes (round 1, one session) gave 126 (`refresh`), 103 (`cancel`), 177
(`gen`) and 130 (`probe`) us per process-second against 54 for `sleep`; the families do not add
up, because most of each one's cost is waking a cold process and runtime.

### Other evidence

`probe-docker-flock.sh` checks whether a container sees the host's flock (design section 8.1).
