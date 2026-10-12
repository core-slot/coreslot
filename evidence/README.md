# Evidence

## Waiter benchmark (the release gate): FAIL

The gate is that a thousand waiters, under bursts of arrivals, admissions, cancellations and stall
recoveries, cost under 2 % of one core in aggregate: 20 ms of CPU per second for all of them, or
20 us per waiter per second. **It still fails after the gate fix of 2026-10-12, by about 5.5 times
at 100 waiters and, projected, by about 200 times at 1000.** The runtime alone, a Go process per
waiter that only wakes once per second, already costs 54 us per waiter-second on macOS, 2.7 times
the budget.

### Results, before and after the gate fix (2026-10-12)

Aggregate waiter CPU as a percentage of one core over the measured window. "Before" is the
benchmark of 2026-10-11 on 2b40885; "after" is a677509, where the election stops at the first
live, non-STALLED ticket and a waiter that is not the head works once per second (PROTOCOL.md
section 10.3). Every run held the host's heavy-work lock; the load averages
are the host's 1/5/15-minute ones at the run's start and end.

| Platform | Waiters | Before: % of one core (us per waiter-second) | After: % of one core (us per waiter-second) | After: load, start to end |
|---|---|---|---|---|
| macOS | 50 | 9.7 (2.0 ms); a 30 s trial, bursts every 5 s | 4.40 (892); 120 s, standard bursts | 7.30 13.51 22.59 to 7.44 11.47 20.48 |
| macOS | 100, 5 runs | min 25.01 / **median 25.80** / max 28.99 (2.5 to 2.9 ms) | min 9.62 / **median 11.04** / max 11.87 (974 to 1192) | 7.44 11.47 20.48 to 30.86 18.66 17.92 over the five |
| macOS | 200 | 99.8 (5.0 ms); 60 s, profiled | **30.77** (1546); 120 s | 30.86 18.66 17.92 to 53.25 26.11 20.65 |
| macOS | 200, profiled | | 22.54 (1130); 120 s | 11.94 18.35 18.09 to 29.41 24.90 20.82 |
| Linux (Docker VM) | 100 | not run | **13.67** (1375); 120 s | 0.30 0.09 3.94 to 3.19 1.13 3.81 (the VM's) |
| macOS | 1000 | 1028.4 (10.4 ms); 318 s, a lower bound | not run (no run above 200); **projected 365 to 503** | |
| Linux (Docker VM) | 1000 | 889.7 (8.9 ms) | not run; see the projection | |

The five n=100 runs after the fix: 11.038, 11.869, 10.728, 11.577 and 9.617 %, with 130 to 146
arrivals, 11 to 27 admissions, 60 cancellations, 60 withdrawals and 36 stops each, and 0 errors.
In every run the wait4 sum equals the harness's RUSAGE_CHILDREN delta to the millisecond. The two
n=200 runs differ by a third; the first started right after the fifth n=100 run, at a 1-minute
load of 31, and the host's load rose to 53 during it. The Linux run used go1.26.9 in `golang:1.26`
(linuxkit 7.0.14, 14 CPUs) as an unprivileged user; the macOS runs used go1.27.2.

### Where a waiter's CPU goes after the fix

**A waiter that is not the head, in a queue whose `gen` does not change**, measured with
`bench/floor -mode waiter`: 20 library waiters behind a head that never gets the only token, each
measured over 120 s by its own getrusage after a 10 s warm-up. It costs **743 us per
waiter-second** on macOS (median of 20; 704 to 772). Its once-per-second work is 18 system calls:
the refresh (`futimes`), the cancel check (one `open`, ENOENT), the `gen` read (`open`, `fstat`,
two `fcntl`, two `read`, `close`) and the examination of the nearest ticket ahead (`lstat`, then
`open`, `fstat`, two `fcntl`, `flock`, `close`). The merged profile of the 20 waiters
(`floor-waiter-darwin-20x120s.pprof`, 1172 ms of samples, about two thirds of the rusage CPU)
splits it as follows; the microseconds scale each share to the 743 us measured.

| Share | us per waiter-second | What |
|---|---|---|
| 61 % | about 450 | Go scheduler outside the waiter's code: idle Ms parking and waking (`findRunnable`, `wakep`/`startm` → `pthread_cond_signal`, `pthread_cond_wait`, `kevent` netpoll) |
| 13 % | about 97 | `gen` read (`ReadGen`: `OpenFile`, read, decode) |
| 10 % | about 72 | refresh (`futimes`) |
| 6 % | about 45 | lock probe of the ticket ahead |
| 4 % | about 29 | cancel check |
| 2 % | about 18 | `lstat` of the ticket ahead |
| 4 % | about 30 | the rest: allocation, decoding, timers; no GC in the window |

**The floor: what a process costs before coreslot's work** (`bench/floor`, 20 processes, 60 s,
median us per process-second; the loop has the shape of `Wait`'s, a timer reset once per second):

| Process | macOS, default GOMAXPROCS (14) | macOS, GOMAXPROCS=1 | Linux VM, default | Linux VM, GOMAXPROCS=1 |
|---|---|---|---|---|
| sleeps 1 s in a loop, no system calls | **54** | 50 | 211 | 179 |
| the same plus the 18 system calls above, made directly | 213 | 210 | 275 | 274 |
| the library waiter that is not the head | 743 | **300** | 440 | 489 |

- On macOS the runtime alone is 54 us per waiter-second, 2.7 times the budget: a thousand idle
  Go processes that wake once per second cost 5.4 % of one core.
- The 18 system calls themselves add about 160 us. Going through the engine adds about 530 us more,
  and almost all of it is the scheduler cost of a 14-P runtime around those calls: with
  GOMAXPROCS=1 the same waiter costs 300 us instead of 743.
- The Linux figures come from a Docker Desktop VM whose rusage is tick-sampled; their spread is
  wide (the sleep loop: 142 to 801 us), so read them as an order of magnitude only.

**What the benchmark adds on top**, from the n=200 profile after the fix
(`waiter-bench-n200-darwin-after.pprof`, the waiter in the middle of the initial queue, 110 s at
1000 Hz, 129 ms of samples, 1.17 ms per second): 44 % is `rescan`, of which `store.List` is 40 %
(`lstat` of every entry 26 %, `readdir` 9 %), and 10 % is the hold probes that follow a rescan.
A waiter rescans `queue/` whenever `gen` changed (PROTOCOL.md section 10.3), and the bursts change
it several times every 10 s (arrivals, admissions, the owners' removals after a cancellation,
resume bumps). Each rescan costs O(n), so this term is again quadratic for the queue as a whole,
though it is paid at most once per second per waiter and only after a change. An arrival costs
about 6.3 ms of CPU as a whole process (start-up, `Open`, enqueue; a Go process that only starts
and exits costs 3.3 ms), so the roughly 1.1 arrivals per second cost about 7 ms per second whatever
n is.

### Projection to 1000 waiters (not a measurement)

The per-waiter-second cost grows with n: 892 us at n=50, 1109 us at n=100 (median) and 1130 to
1546 us at n=200. A straight-line fit through those points, extrapolated to n=1000, gives 3.65 ms
per waiter-second using the mean of the two n=200 runs, or 5.03 ms using the first n=200 run only:
**about 365 to 503 % of one core for a thousand waiters**, against the 2 % gate and the 1028 % (a
lower bound) measured before the fix. The n-dependent part is the rescan. Even with no `gen`
change at all, a thousand non-head waiters would cost 1000 x 743 us = 74 % of one core on macOS
(30 % with GOMAXPROCS=1), and the runtime floor alone 5.4 %. The gate cannot be met with one Go
process per waiter that wakes once per second, whatever coreslot does inside the wake-up; meeting
it needs a different waiting design or a different target, which is the maintainers' decision.

### Before the fix: the profile that found the quadratic scan

`waiter-bench-n200-darwin.pprof` covers the middle waiter of the n=200 run of 2026-10-11 (about
100 tickets ahead of it) for 50 s: 92 % `engine.(*Ticket).elect`, 75 % `store.Probe` (`open(2)`
alone 72 %), 13 % `lstat(2)`. The election then read the mtime and probed the lock of every
ticket ahead each second instead of stopping at the first live, non-STALLED one, so a waiter at
position k made O(k) system calls per second, and `Wait` stepped every 250 ms, each Step reading
the cancel file. Both were deviations from PROTOCOL.md section 10.3, fixed
in a677509.

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
  `Enqueue cpu=1..1`, then `Wait`. Before the gate fix `Wait` stepped every 250 ms; since then it
  steps when the ticket's next work is due (`Ticket.Due`), as `coreslot run` does: once per second
  for a waiter that is not the head, on the 0.25 s to 2 s backoff for the head.
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

### Reproduce

These commands run from the repository root. Wrap each run in the host's heavy-work lock, for
example `until mkdir /tmp/coreslot-heavy.lock 2>/dev/null; do sleep 5; done;
<command>; rmdir /tmp/coreslot-heavy.lock`. Do not run above 200 waiters on a
shared host: 1000 waiters saturated it before the fix.

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
go tool pprof -top -cum "$B/floor" "$B"/prof/waiter-*.pprof
```

`waiters` exits 0 when the median meets the gate, 1 when it does not and 2 on an error; its
default, `-n 1000 -runs 5`, is the gate itself. Every child is in one process group, which the
harness kills on every exit path, including its own SIGINT, SIGTERM or SIGHUP; a child whose
harness died sees its stdin pipe close and exits. For the Linux figures the same commands ran
inside `golang:1.26` as an unprivileged user, built there, the way `scripts/linux-test.sh` runs.
The per-family floor modes on macOS gave 126 (`refresh`), 103 (`cancel`), 177 (`gen`) and
130 (`probe`) us per process-second against 54 for `sleep`; the families do not add up, because
most of each one's cost is waking a cold process and runtime.

### Other evidence

`probe-docker-flock.sh` checks whether a container sees the host's flock.
