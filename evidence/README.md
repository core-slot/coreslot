# Evidence

## Waiter benchmark (the release gate): FAIL

The gate is that a thousand waiters, under bursts of arrivals, admissions, cancellations and stall
recoveries, cost under 2 % of one core in aggregate. That is 20 ms of CPU per second of wall time
for all of them together, or 20 us per waiter per second. The release candidate is blocked: the
measured cost is about 500 times the target, and it grows quadratically with the number of waiters.

### Results

The table gives aggregate waiter CPU as a percentage of one core over the measured window.

| Platform | Waiters | Window | Runs | % of one core: min / median / max | CPU per waiter-second |
|---|---|---|---|---|---|
| macOS (host) | 1000 | 318.4 s (planned 120 s, see below) | 1 | 1028.4 | 10.4 ms |
| Linux (Docker Desktop VM) | 1000 | 120.0 s | 1 | 889.7 | 8.9 ms |
| macOS (host) | 200 | 60.0 s | 1 (profiled) | 99.8 | 5.0 ms |
| macOS (host) | 100 | 120.0 s | 5 | 25.01 / 25.80 / 28.99 | 2.52 to 2.93 ms |
| macOS (host) | 50 | 30.0 s | 1 (trial, bursts every 5 s) | 9.7 | 2.0 ms |

**The macOS series at 1000 waiters was stopped after its first run, so it has no 5-run spread.**
That run kept 10 cores busy for more than five minutes and drove the shared host's load average
from 43 to 556. Its own harness was CPU-starved: the event loop ran late, so the 120 s window
stretched to 318 s and only 8 of the planned cancel requests were published. Running four more
copies would only have taken CPU from the other work on the host. The figure is still CPU divided
by wall time over the window that actually ran. It is a lower bound on what 1000 waiters demand,
because the machine could not give them more. The 5-run spread was measured at 100 waiters
instead, where it is narrow (25.0 to 29.0 %). Even there the gate fails by a factor of 13.

Per-run detail:

- **macOS, n=1000, run 1:** waiters used 3274.6 s of CPU over 318.4 s, a mean of 990 queued.
  There were 121 arrivals, 10 admissions, 8 cancellations, 10 withdrawals and 22 stops, with
  0 errors. Warm-up took 159 s. The whole run's wait4 sum and RUSAGE_CHILDREN both came to
  4817.463 s. Load average went from 42.93 34.64 31.05 to 429.33 337.65 190.41.
- **Linux, n=1000, run 1:** waiters used 1067.6 s of CPU over 120.0 s, a mean of 1001 queued.
  There were 135 arrivals, 15 admissions, 60 cancellations, 60 withdrawals and 33 stops, with
  0 errors. Warm-up took 185 s. The whole run's wait4 sum was 2634.780 s against 2634.782 s of
  RUSAGE_CHILDREN. The VM's load average went from 1.20 0.50 0.64 to 976.84 348.26 137.15.
- **macOS, n=100, runs 1 to 5:**

  | Run | % of one core | Load average, start to end (1/5/15 min) |
  |---|---|---|
  | 1 | 25.012 | 24.50 188.01 223.70 to 10.29 126.60 194.29 |
  | 2 | 25.234 | 10.29 126.60 194.29 to 12.14 87.22 169.29 |
  | 3 | 28.985 | 12.14 87.22 169.29 to 29.54 67.96 149.91 |
  | 4 | 27.751 | 29.54 67.96 149.91 to 21.74 50.85 131.81 |
  | 5 | 25.795 | 21.74 50.85 131.81 to 9.89 37.00 115.27 |

  Each run had 133 to 135 arrivals, 14 to 16 admissions, 60 cancellations, 60 withdrawals and
  36 stops, with 0 errors. The high 5- and 15-minute averages are what remained of the
  1000-waiter run.
- **macOS, n=200, profiled:** load average went from 48.22 230.57 240.43 to 24.50 188.01 223.70.

### Setup

- **Host:** Mac16,7, 14 logical CPUs, macOS 27.0.1 (26A434), Darwin 27.0.0, go1.27.2
  darwin/arm64.
- **Linux:** the `golang:1.26` image (go1.26.9 linux/arm64, Debian 13), running on Docker
  Desktop's linuxkit kernel 7.0.14 with 14 CPUs. The run used an unprivileged user, the same way
  `scripts/linux-test.sh` does. `pids.max` was `max` and `ulimit -n` was 1048576, so no limit was
  raised. On macOS, `kern.maxprocperuid` is 8000 and `ulimit -n` is 1048576, so no limit was
  raised there either.
- **Waiters:** each waiter is its own process: `bench/waiters waiter`, built on the public library.
  It calls `Open` (with `CORESLOT_STATE_DIR` and `<statedir>/config.toml`), then
  `Enqueue cpu=1..1`, then `Wait`. `Wait` steps every 250 ms, the same cadence as `coreslot run`.
  The waiters use the default `GOMAXPROCS`. Once admitted, a waiter closes its grant and exits.
- **Budget:** `cpu = 8`. Between bursts the harness holds all 8 tokens as eight adopted 1-token
  grants, so the budget is always held.
- **Bursts:** one every 10 s, starting 5 s into the window (12 bursts in 120 s). Each burst does
  the following:
  1. It publishes 5 cancel requests (`coreslot cancel <id>`) and sends SIGTERM to 5 waiters. On
     SIGTERM the `Wait` context ends and the library withdraws the ticket.
  2. It spawns arrivals to refill the queue to n.
  3. It releases 4 tokens for 3 s. Heads are admitted and exit, which frees the token for the
     next head, and then the harness adopts the tokens back.
  4. At +4 s it sends SIGSTOP to the queue head and 2 random waiters, and sends SIGCONT 12 s
     later. Their successors find them STALLED, and on resume they make the resume bump of `gen`.
- **Window:** the window opens after every one of the n waiters has enqueued, plus 5 s to settle.
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

### Profile: where a waiter's CPU goes

The profile is in `waiter-bench-n200-darwin.pprof`. It covers the waiter in the middle of the
initial queue (serial 100 of 200, so about 100 tickets were ahead of it) for 50 s of the n=200
run, at a requested 500 Hz. It has 316 ms of samples, 6.3 ms per second, consistent with the run's
mean of 5.0 ms per waiter-second.

| Share | Function | What it does |
|---|---|---|
| 92 % | `engine.(*Ticket).elect` | The election each second |
| 75 % | ↳ `store.Probe` (`OpenFile`, `unix.Open`) | `open(2)`, 72 % of all samples |
| 13 % | ↳ `os.Lstat` | `lstat(2)` |
| 8 % | ↳ `rescan` | `List`, the directory read when `gen` changed |
| 4 % | ↳ `liveHolders` | Hold probes |
| about 2 % | `flock(2)` and `close(2)` | Together |
| about 5 % | Runtime scheduling (`kevent`) | |

System calls account for 89 % of the samples. They are dominated by `open(2)`, then `lstat(2)`.

The cause is in `internal/engine/wait.go`, `elect`. Every second, a waiter runs `Lstat` and
`Probe` (open, `flock LOCK_NB`, close) on **every** ticket ahead of it, not only up to the first
live, non-STALLED one. So a waiter at position k costs O(k) system calls per second, and the queue
as a whole costs O(n²/2). That is about 500,000 probes per second at n=1000. The cost per
waiter-second grows with n: 2.0 ms at n=50, 2.5 to 2.9 ms at n=100, 5.0 ms at n=200, and 8.9 to
10.4 ms at n=1000. The term that does not depend on queue position comes to roughly 1 ms per
waiter-second at small n. This is an extrapolation from the four sizes above, and it includes the
start-up of arrivals spread over the window. That term alone is about 50 times the 20 us budget.
It covers 4 Steps per second (cancel check, `gen` read, refresh) plus the Go runtime's wake-ups.
Removing the quadratic term would therefore not be enough on its own to pass the gate.

PROTOCOL.md section 10.3 says that the first live, non-STALLED ticket
ends the examination. Observing every ticket ahead makes stall detection prompt for tickets
further ahead, and that is the trade-off the engine makes. No change was made to the protocol or
the engine. The finding goes to the maintainers.

### Reproduce

These commands run from the repository root. Wrap each run in the host's heavy-work lock.

```sh
B=$(mktemp -d)
go build -o "$B/coreslot" ./cmd/coreslot
go build -o "$B/waiters" ./bench/waiters
"$B/waiters" -coreslot "$B/coreslot" -dir "$B/work" -json "$B/results.json"   # the gate: n=1000, 5 x 120 s
"$B/waiters" -coreslot "$B/coreslot" -dir "$B/work" -n 100 -runs 5              # the 5-run spread above
"$B/waiters" -coreslot "$B/coreslot" -dir "$B/work" -n 200 -runs 1 -duration 60s \
	-profile "$B/waiter.pprof"                                                    # the profile
go tool pprof -top -cum "$B/waiters" "$B/waiter.pprof"
```

The exit status is 0 when the median meets the gate, 1 when it does not and 2 on an error. Every
child is in one process group, which the harness kills on every exit path, including its own
SIGINT, SIGTERM or SIGHUP. A child whose harness died sees its stdin pipe close and exits. For the
Linux figure, the same binary was built and run with `-runs 1` inside `golang:1.26` as an
unprivileged user.

### Other evidence

`probe-docker-flock.sh` checks whether a container sees the host's flock.
