# Evidence

Every measured claim in the README is backed by a script in this directory or a benchmark program
under `bench/`. Each record below names the script, when it ran, on what, and what it printed,
verbatim. A record is a reading on one machine on one day, not a guarantee; rerun the script to
check it on yours.

Index of the evidence scripts:

- `bench/waiters` and `bench/floor` (with the `*.pprof` profiles in this directory): the waiter
  benchmark, in "Waiter benchmark (the release gate)" below. Both build only with
  `-tags coreslot_bench`. The profiles were recorded before the module moved, so their symbols
  name `github.com/Nezhinskiy/coreslot`.
- `probe-docker-flock.sh`: whether a container sees the host's flock, in "Docker Desktop and
  host locks" below.
- `acceptance.py`: three test gates and a CI job with and without coreslot, in "Product
  acceptance" below.
- `overload.py` with `overload/`: four agents whose gates overload memory and CPU, free, behind a
  semaphore and under coreslot, in "Acceptance run 3" below.
- `run4.py` with `overload/run4.sh` and `overload/run4/`: the same stand with needs coreslot
  learned itself, against no admission and a tuned semaphore, in "Acceptance run 4" below.
- `run5.py` with `overload/run5.sh` and `overload/run5/`: run 4's stand and needs, coreslot's
  live memory size against its static size and the semaphore, in "Acceptance run 5" below.

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
| Before | 827552e, 2026-10-11 | The election probed every ticket ahead; `Wait` stepped every 250 ms. |
| Pass 1 | 3eb12c4, 2026-10-12 | The election stops at the first live, non-STALLED ticket; a non-head waiter works once per second. |
| Pass 2 | 7e0d1fe, 2026-10-12 | The front window of 4; re-election inside the admitting section; names-only rescans; holds probed on request; one P while the CLI waits; the harness's gen churn fixed. |

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

### The gate run (2026-10-12, c114bc9)

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

### Re-measured 2026-10-17/18 (89897d5 against c114bc9)

The gate, re-run on the same reference host (Mac16,7, macOS 27.0.1, go1.27.2) at the head of
`main`, `89897d5`, and at the commit of the gate run above, `c114bc9`, with the harness's
defaults (`-n 100 -duration 120s`). The host was quieter than on 2026-10-12: the 1-minute load
was 2.6 to 4.9 at the start of every run, though the 15-minute average was still 11.75 at the
first interleaved run. Each build comes from `git archive` of its commit; `89897d5` builds the
benchmark with `-tags coreslot_bench`, and `c114bc9` and `a28590a` predate that tag (no file
there names it). In all 27 runs below the wait4 sum equals the harness's RUSAGE_CHILDREN delta,
no waiter reported an error, and every run stopped 36 waiters.

**`89897d5`, 2026-10-17 23:04 to 23:15 UTC.**

| Run | % of one core | us per waiter-second | Arrivals | Admitted | gen, start to end | Seconds with a gen change | Load, start to end |
|---|---|---|---|---|---|---|---|
| 1 | 6.697 | 674 | 132 | 13 | 108 to 458 | 75 | 3.69 3.46 3.50 to 3.62 3.40 3.46 |
| 2 | 7.290 | 733 | 132 | 13 | 108 to 456 | 71 | 3.62 3.40 3.46 to 4.61 3.70 3.56 |
| 3 | 7.121 | 717 | 133 | 14 | 108 to 460 | 75 | 4.61 3.70 3.56 to 3.59 3.59 3.53 |
| 4 | 7.780 | 782 | 131 | 12 | 108 to 457 | 72 | 3.59 3.59 3.53 to 4.09 3.72 3.59 |
| 5 | 7.798 | 784 | 131 | 12 | 108 to 459 | 73 | 4.09 3.72 3.59 to 4.62 3.95 3.68 |

```text
summary: 5 runs; % of one core: min 6.697, median 7.290, max 7.798; gate median <= 10.0 % at 100 waiters: PASS
```

**`c114bc9`, 2026-10-18 01:07 to 01:17 UTC.**

| Run | % of one core | us per waiter-second | Arrivals | Admitted | gen, start to end | Seconds with a gen change | Load, start to end |
|---|---|---|---|---|---|---|---|
| 1 | 4.376 | 441 | 135 | 16 | 108 to 473 | 73 | 3.73 3.20 3.05 to 3.96 3.64 3.26 |
| 2 | 4.471 | 449 | 130 | 11 | 108 to 452 | 73 | 3.96 3.64 3.26 to 4.90 4.02 3.45 |
| 3 | 5.182 | 520 | 135 | 15 | 108 to 459 | 62 | 4.90 4.02 3.45 to 3.46 3.67 3.38 |
| 4 | 5.632 | 566 | 131 | 12 | 108 to 450 | 74 | 3.46 3.67 3.38 to 3.43 3.53 3.36 |
| 5 | 6.856 | 691 | 135 | 16 | 108 to 465 | 72 | 3.43 3.53 3.36 to 2.77 3.20 3.24 |

```text
summary: 5 runs; % of one core: min 4.376, median 5.182, max 6.856; gate median <= 10.0 % at 100 waiters: PASS
```

**`89897d5` again, 2026-10-18 01:17 to 01:27 UTC.**

| Run | % of one core | us per waiter-second | Arrivals | Admitted | gen, start to end | Seconds with a gen change | Load, start to end |
|---|---|---|---|---|---|---|---|
| 1 | 5.688 | 572 | 132 | 13 | 108 to 458 | 72 | 2.77 3.20 3.24 to 3.74 3.29 3.26 |
| 2 | 7.305 | 735 | 132 | 13 | 108 to 461 | 74 | 3.74 3.29 3.26 to 3.05 3.23 3.24 |
| 3 | 7.688 | 772 | 130 | 10 | 108 to 450 | 73 | 3.05 3.23 3.24 to 3.70 3.46 3.33 |
| 4 | 6.349 | 638 | 131 | 12 | 108 to 458 | 74 | 3.70 3.46 3.33 to 4.33 3.65 3.41 |
| 5 | 7.892 | 793 | 131 | 12 | 108 to 455 | 73 | 4.33 3.65 3.41 to 3.07 3.34 3.31 |

```text
summary: 5 runs; % of one core: min 5.688, median 7.305, max 7.892; gate median <= 10.0 % at 100 waiters: PASS
```

Read one after another, these three series suggest that a waiter became about 40 % more
expensive between `c114bc9` and `89897d5`: medians of 7.29 and 7.31 % against 5.18 %. The
interleaved runs do not bear that out.

**Interleaved, 2026-10-18 02:31 to 02:56 UTC.** Four rounds, each one run of `c114bc9`, then
`a28590a`, then `89897d5`, so that the three builds share the same stretches of host time.
`a28590a` is the merge that brought `c114bc9` into the main line; on the waiters' path it
changes only comments and help text, so the two measure what two builds of the same code differ
by.

| Round, build | % of one core | us per waiter-second | Arrivals | Admitted | gen, start to end | Seconds with a gen change | Load, start to end |
|---|---|---|---|---|---|---|---|
| 1, c114bc9 | 6.323 | 635 | 130 | 11 | 108 to 454 | 75 | 3.20 10.98 11.75 to 3.50 8.72 10.77 |
| 1, a28590a | 7.184 | 722 | 131 | 12 | 108 to 454 | 72 | 3.50 8.72 10.77 to 3.09 6.95 9.79 |
| 1, 89897d5 | 6.130 | 617 | 132 | 13 | 108 to 459 | 74 | 3.09 6.95 9.79 to 2.77 5.63 8.88 |
| 2, c114bc9 | 6.675 | 672 | 132 | 13 | 108 to 458 | 75 | 2.77 5.63 8.88 to 2.58 4.60 8.02 |
| 2, a28590a | 6.467 | 650 | 132 | 13 | 108 to 459 | 75 | 2.58 4.60 8.02 to 3.71 4.22 7.39 |
| 2, 89897d5 | 6.236 | 627 | 132 | 13 | 108 to 460 | 72 | 3.71 4.22 7.39 to 4.78 4.42 7.03 |
| 3, c114bc9 | 6.500 | 653 | 130 | 11 | 108 to 453 | 72 | 4.78 4.42 7.03 to 3.54 4.10 6.54 |
| 3, a28590a | 5.900 | 594 | 132 | 13 | 108 to 460 | 74 | 3.54 4.10 6.54 to 3.81 3.86 6.10 |
| 3, 89897d5 | 5.950 | 599 | 132 | 13 | 108 to 458 | 74 | 3.81 3.86 6.10 to 4.07 3.84 5.78 |
| 4, c114bc9 | 7.175 | 722 | 132 | 13 | 108 to 464 | 74 | 4.07 3.84 5.78 to 2.68 3.42 5.34 |
| 4, a28590a | 8.440 | 849 | 132 | 13 | 108 to 457 | 74 | 2.68 3.42 5.34 to 3.73 3.53 5.12 |
| 4, 89897d5 | 8.324 | 837 | 132 | 13 | 108 to 458 | 73 | 3.73 3.53 5.12 to 3.10 3.46 4.88 |

| Build | Median of the four rounds | Min | Max |
|---|---|---|---|
| `c114bc9` | **6.588** | 6.323 | 7.175 |
| `a28590a` | **6.825** | 5.900 | 8.440 |
| `89897d5` | **6.183** | 5.950 | 8.324 |

**What it shows:**

- No cost increase from `c114bc9` to `89897d5` that these runs can resolve. Interleaved,
  `89897d5`'s median is below `c114bc9`'s, and the spread within each build exceeds the
  difference between the medians. An effect smaller than about one point is not excluded at four
  rounds.
- The spread follows the time, not the build. In round 4 all three builds rose together, to
  7.18, 8.44 and 8.32 %. The queue did the same work in every run (130 to 135 arrivals, 10 to 16
  admitted, a gen change in 62 to 75 seconds of the window); what moved is the CPU each waiter
  spends per second.
- Between sessions the same code moves by more than the step the sequential series suggested:
  `c114bc9` measured a median of 5.18 % (4.38 to 6.86, rising run by run) in one session and
  6.59 % (6.32 to 7.18) in the interleaved runs that followed. The 2026-10-12 median, 5.70 %,
  lies inside the first range. Comparing two builds therefore takes interleaved runs: two
  consecutive 5-run series do not tell a difference of two points from the host's drift.
- The gate holds. Every median of `89897d5` here is 6.18 to 7.31 %, against 10 %: a margin of
  1.37 to 1.62 times the median on this host, not the 1.75 measured on 2026-10-12. No single run
  in these series exceeded 8.44 %.

To repeat the interleaved comparison, build each commit from its own tree (`c114bc9` and
`a28590a` without the tag, which they predate) and alternate single runs:

```sh
git archive c114bc9 | tar -x -C "$SRC_OLD"
(cd "$SRC_OLD" && GOFLAGS=-buildvcs=false go build -o "$B_OLD/coreslot" ./cmd/coreslot \
	&& GOFLAGS=-buildvcs=false go build -o "$B_OLD/waiters" ./bench/waiters)
for r in 1 2 3 4; do
	for b in "$B_OLD" "$B_MID" "$B_NEW"; do
		"$b/waiters" -coreslot "$b/coreslot" -dir "$b/work-$r" -runs 1 -json "$b/round-$r.json"
	done
done
```

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
  (then `Free`) reports one, once a second, instead of adopting, finding it short and closing
  every 20 ms.
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
go build -tags coreslot_bench -o "$B/waiters" ./bench/waiters   # the tag keeps them out of go install
go build -tags coreslot_bench -o "$B/floor" ./bench/floor
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
		for p in cmd/coreslot bench/waiters bench/floor; do go build -tags coreslot_bench -o \$B/\$(basename \$p) ./\$p; done &&
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

**The criterion, declared before the first run:** on a developer machine that is
never quiet, three parallel test gates plus one CI job run with and without coreslot as
alternating pairs in the same window, at least three pairs; with coreslot, the median time until
the last job finishes is at most 1.05 × the median without it, and no gate fails falsely.

`acceptance.py run GATES.json OUTDIR FIRST LAST` starts every job of a set at the same instant,
alternates a set without coreslot and a set with it, samples the load average and the memory
pressure level every 5 s, and keeps each set's run-log lines; `acceptance.py summary OUTDIR`
prints the table, the medians and the ratio of each pair. Run 1 used an earlier copy of the same
runner; `summary` over its records prints the numbers below.

Since runs 1 and 2 the runner can also alternate the order (`--order abba`: even pairs run with
coreslot first, so that neither arm always starts on a machine the other has just heated), wait
between sets until the thermal state is nominal and a single-core speed probe is back within a
tolerance of its first reading (`--probe-tolerance`), record swap in use and the thermal state
beside the load, and measure each job alone (`acceptance.py alone GATES.json OUTDIR RUNS`, under
`coreslot run --need cpu=1..256`) for its duration, its cores used and whether it passes alone.
`summary` then adds mean completion (the mean of the jobs' end times) with a one-sided sign test
over the pairs, and the failures of each arm, counting as false a failure of a job that passed
every alone run, with a one-sided Fisher exact test. Over runs 1 and 2 its first lines are
unchanged.

### Setup

- **Host:** the reference host (Apple M4 Pro, 14 logical CPUs, 48 GiB, macOS 27.0.1), budget
  `cpu = 14` in a state directory of its own, coreslot built from `3788de2`. Never quiet: other
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

**Mean completion, computed later from the same records.** `summary` over run 2's records now
also prints:

```text
mean completion (the mean of the jobs' end times):
median with = 227.7 s, median without = 210.1 s, ratio = 1.084
per pair (with / the preceding without): 1.002, 1.019, 0.906, 1.392, 1.178, 1.202, 1.050, 1.220, 1.180, 1.001, 0.869, 1.146
pairs favouring coreslot: 2 of 12; one-sided sign test p = 0.9968
```

On this light mix coreslot made the jobs finish later on average, by 8 %: nothing queued, and the
CLI suite, granted 6 tokens and so 6 workers in every set, took 263 to 387 s with coreslot against
180 to 304 s with `-n auto` without it. The service suite ended last in every set of both arms,
which is why the time to the last job's end tied.

### Reproduce

```sh
python3 evidence/acceptance.py run gates.json out 1 6
python3 evidence/acceptance.py summary out
python3 evidence/acceptance.py alone gates.json out 3 --probe-tolerance 0.05
python3 evidence/acceptance.py run gates.json out 1 12 --order abba --cooldown 60 --probe-tolerance 0.05
```

`gates.json` is not in the repository: it named local paths, and most of its jobs ran in working
trees of other projects. Write one for your own jobs, following the shape in the script's
docstring, and give it a state directory of its own, so the run is a separate budget and its run
log holds only these jobs.

## Acceptance run 3: agents whose gates overload memory and CPU (`overload.py`)

**Status: pre-registered.** This section was committed before the first measured block; the
results are appended below it, whatever they are. The analysis is `overload.py summary`, which is
not changed after the first block.

**Question.** Several coding agents on one laptop start their verification gates within a few
seconds of each other. Does a coreslot budget, with needs learned from each gate's measured use,
keep the laptop out of memory pressure while returning verdicts sooner than running everything
at once, and sooner than the do-it-yourself fix of a two-slot semaphore?

### Stand

Every workload is public and pinned; `evidence/overload/setup.sh` builds the stand under
`~/bench.noindex` (excluded from Spotlight and Time Machine) and coreslot from this checkout.

| Agent | Repository at its pin | Chain of gates |
|---|---|---|
| A1, A2 | microsoft/vscode 1.142.0 (`d956e8a`), `npm ci`, Electron 43.7.7 types | `npm run typecheck-client` (TypeScript 7.0.2) |
| A3 | fastapi/fastapi 0.156.2 (`44cc742`), `uv sync --frozen --group tests --all-extras` | `ruff check fastapi tests`, then `pytest -n {n}` |
| A4 | sqlalchemy/sqlalchemy rel_2_1_5 (`4fbf72c`), pytest 9.1.1, pytest-xdist 3.8.0 | `pytest -n {n} test/orm` (SQLite) |

`evidence/overload/stand.json` holds the exact argv. Agents arrive at offsets drawn from
U(0, 5 s), once per block, the same for the block's three sets: subagents are started together,
as the reports behind this run describe (anthropics/claude-code#101193, jakebromberg/ship-dag#108).

**Host.** MacBook Air `Mac16,12`, Apple M4, 10 cores (4 performance), 16 GiB, macOS 27.0.1
(26A434), on AC, Low Power Mode off, no other heavy work started during the run; the owner's
desktop applications stay open and are part of the machine as it is used. Node 24.10.0, npm
11.6.0, uv 0.11.19, Python 3.13.0, Go 1.27.1.

### Calibration: the learned needs

`overload.py calibrate stand.json evidence/overload/run3 2` ran each gate alone twice without
coreslot (`run3/calibration.jsonl`) and derived the needs by the rule in the script: memory
ceil(1.2 × the peak of the gate's whole process tree, sampled every second, or of its largest
process if higher); cpu ceil(1.2 × cores used), as a range from 2 for a gate that takes `{n}`.
The budget is the 10 logical CPUs and the memory available before calibration, 9 GiB.

| Gate | Wall (s) | Cores used | Peak tree (GiB) | Largest process (GiB) | Need |
|---|---|---|---|---|---|
| vscode-typecheck | 9.2, 9.2 | 5.04, 4.86 | 7.88, 7.49 | 7.86, 7.45 | cpu=7, mem=10 (clamped to 9) |
| fastapi-ruff | 1.0, 1.0 | 0.03, 0.05 | 0.00, 0.00 | 0.02, 0.02 | cpu=1, mem=1 |
| fastapi-tests | 31.7, 31.6 | 5.28, 5.55 | 2.69, 2.64 | 0.27, 0.27 | cpu=2..7, mem=4 |
| sqlalchemy-orm-tests | 28.9, 31.1 | 8.44, 8.84 | 2.47, 2.44 | 0.28, 0.23 | cpu=2..10, mem=3 |

Every gate passed both runs, so a failure of any gate in a set is a false failure. The pytest
gates show why the needs come from the tree: coreslot's run log records `max_rss_kib`, the
largest single process (0.27 GiB), which would under-declare these gates' memory about tenfold.

### Arms, order and stopping

- **F** — no admission: every gate starts when its agent reaches it; `{n}` is `auto`.
- **S** — a counting semaphore of two slots around the heavy gates (the type checks and the test
  suites); the lint runs free; `{n}` is `auto`.
- **K** — every gate through `coreslot run` with its learned need; `{n}` is the cpu grant.

Six blocks (1 to 6), each running F, S and K once in an order drawn from the seed `run3` and the
block number. Before each set the runner waits 20 s and then until the thermal state is nominal
and a single-core probe is within 5% of its first reading, at most 60 s. Each set records every
gate's start, end and exit, and once a second the memory pressure level, swap, the swapouts
counter and the load, with the thermal state every fifth second.

A pilot block (block 0, seed `pilot`, `evidence/overload/pilot`) ran before this section was
written, to check the runner; it is kept and never pooled with the measured blocks.

### Hypotheses and the criterion, declared before the run

1. **No critical memory pressure under coreslot:** K spends 0 s at the critical level
   (`kern.memorystatus_vm_pressure_level` 4) in every one of the six sets.
2. **Sooner verdicts than free running:** K's mean time to verdict (each agent's chain end minus
   its arrival, averaged over the four agents) is below F's in 6 of 6 blocks (one-sided sign test
   p = 0.016).
3. **Sooner verdicts than the semaphore:** the same against S, in 6 of 6 blocks.
4. **No false failure** in K.

Run 3 passes when 1, 2 and 4 hold; 3 is reported pass or fail on its own. Reported beside them
and not part of the criterion: makespan (expected to be longer under K, which runs one type check
at a time), seconds at warn or worse, swapouts, and the hottest thermal state per arm.

### Reproduce

```sh
evidence/overload/setup.sh
python3 evidence/overload.py calibrate evidence/overload/stand.json out 2
python3 evidence/overload.py run evidence/overload/stand.json out 1 6 --seed run3 \
  --cooldown 20 --probe-tolerance 0.05 --max-cooldown 60
python3 evidence/overload.py summary out
```

### Results (2026-10-20, blocks 1-6, coreslot `de142c7`)

Run 3 **failed** its declared criterion: hypothesis 2 held in 5 of 6 blocks, not 6. Hypotheses 1
and 4 held; hypothesis 3 failed. `overload.py summary evidence/overload/run3` printed:

```text
blocks: [1, 2, 3, 4, 5, 6]
medians over blocks; critical(s) and thermal are the maximum over blocks
arm  mean-verdict(s)  makespan(s)  warn+(s)  critical(s)  swapouts  thermal  false-failures
F             62.35        78.35      15.5            0   49130.0        1               2
S             43.25         63.6       0.0            0       0.0        1               1
K    43.150000000000006        84.25       0.0            0       0.0        1               0
K/F mean_verdict_s: per block [0.509, 0.685, 0.598, 0.682, 0.765, 1.037]; K better in 5 of 6; one-sided sign test p = 0.1094
K/F makespan_s: per block [0.801, 1.07, 1.048, 0.851, 1.167, 1.162]; K better in 2 of 6; one-sided sign test p = 0.8906
K/S mean_verdict_s: per block [0.847, 0.94, 0.722, 0.995, 1.226, 1.483]; K better in 4 of 6; one-sided sign test p = 0.3438
K/S makespan_s: per block [1.234, 1.304, 1.101, 1.103, 1.331, 1.478]; K better in 0 of 6; one-sided sign test p = 1.0000
```

Per block (seconds; swapouts in 16 KiB pages):

| Block | Order | F: verdict / makespan | S: verdict / makespan | K: verdict / makespan | Warn+ s (F, S, K) | Swapouts (F, S, K) | Failed |
|---|---|---|---|---|---|---|---|
| 1 | FSK | 85.9 / 100.1 | 51.6 / 65.0 | 43.7 / 80.2 | 44, 0, 0 | 323692, 0, 0 | F: fastapi-tests |
| 2 | FKS | 59.0 / 75.8 | 43.0 / 62.2 | 40.4 / 81.1 | 14, 0, 0 | 61112, 0, 0 | none |
| 3 | FSK | 60.9 / 83.4 | 50.4 / 79.4 | 36.4 / 87.4 | 14, 9, 0 | 37148, 0, 0 | S: fastapi-tests |
| 4 | SFK | 62.5 / 78.1 | 42.8 / 60.3 | 42.6 / 66.5 | 17, 0, 0 | 0, 0, 0 | F: fastapi-tests |
| 5 | SFK | 63.1 / 78.6 | 39.4 / 68.9 | 48.3 / 91.7 | 20, 0, 0 | 111112, 0, 0 | none |
| 6 | FKS | 62.2 / 76.7 | 43.5 / 60.3 | 64.5 / 89.1 | 9, 0, 0 | 0, 0, 0 | none |

**What it shows.**

- Against running everything at once, coreslot removed the memory pressure and the false
  failures: F reached the warn level in every block (9 to 44 s) and swapped in four, and its
  FastAPI suite failed in two blocks although it passed both calibration runs; K never left the
  normal level, never swapped and never failed. Its mean time to verdict was 32% lower by the
  median per-block ratio (0.6835), lower in 5 of 6 blocks, which is not the significance the run
  declared.
- Against the two-slot semaphore, coreslot was not better. The medians of the mean time to
  verdict tie (43.15 s against 43.25 s, K lower in 4 of 6 blocks) and the semaphore's makespan
  was shorter in every block (coreslot's makespan over the semaphore's: median per-block ratio
  1.269). The semaphore let memory reach
  warn once (block 3, 9 s) and its FastAPI suite failed in that block.
- No arm reached the critical level, so hypothesis 1 holds without separating the arms; the warn
  level and the swapouts separate them.

**Why coreslot lost time, read from the timelines in `run3/results.jsonl`.** In block 3 the
one-second lint waited 39 s: SQLAlchemy's suite, granted all 10 cpu tokens by its learned range
`2..10`, held the head of the line. In block 6 both type checks waited for memory behind the test
suites: each needs 10 GiB, clamped to the whole 9 GiB budget, so it waits until every other
holder is gone. Three rules of this run made the budget conservative: the elastic maximum learned
from a gate run alone on all cores; a memory budget from the memory available before calibration,
while the free arm ran this mix with up to about 20 GiB of demand and reached only warn; and a
strict FIFO with no room for a small request beside a large holder.

**Threats to validity.**

- Six blocks; the arm order drawn from the seed never put K first, so K always started after
  another arm had heated the fanless machine (every arm reached thermal state 1, fair).
- The runner kept only the stderr of a failed gate, and pytest reports to stdout, so the failing
  FastAPI tests are not named; that they passed both calibration runs is all that makes them
  false failures.
- One machine, one mix, the owner's desktop applications open throughout.

**What it does not show.** Any other machine or mix, Linux, a budget tuned past the calibration
rule, or arrivals spread wider than 5 s. The exploratory runs that led to this design are not
pooled with it.

## Acceptance run 4: needs coreslot learned itself, against a tuned semaphore (`run4.py`)

**Status: pre-registered.** This section was committed before the first tuning set; the tuning
result is committed before the first verdict block, and the results are appended below, whatever
they are. The analysis is `run4.py summary`, which is not changed after this commit.

**Question.** On one 16 GiB laptop whose agents' gates overload memory and CPU, does coreslot with
needs it learned itself beat running everything at once, and come close to a semaphore whose slot
count was tuned for this mix?

### Stand

Run 3's stand, unchanged: microsoft/vscode 1.142.0 (`d956e8a`, `typecheck-client`, two agents),
fastapi/fastapi 0.156.2 (`44cc742`, ruff, then pytest), sqlalchemy/sqlalchemy rel_2_1_5
(`4fbf72c`, pytest `test/orm`), built by `evidence/overload/setup.sh` on the same host. coreslot is
built at `9ca499c`, the head of an open pull request (memory admission, the run log's memory peak,
`stats` and `suggest`), not a release. `evidence/overload/stand4.json` holds the exact argv: run
3's, with `-rfE` added to the pytest gates so that failing tests are named, a label
`bench:<gate>` per gate, and a pattern that picks a gate's failure lines from its output.

### Arms

- **F** — no admission: every gate starts when its agent reaches it; `{n}` is `auto`.
- **S** — a counting semaphore of N slots around the heavy gates (the type checks and the test
  suites); the lint runs free; `{n}` is `auto`; N tuned.
- **K** — every gate as `coreslot run --label bench:<gate> -- <argv>`, with no `--need`: the needs
  file supplies it. The needs file is the one `coreslot suggest --write` produced in the learning
  phase; the cpu and mem shares of the budget are tuned.

### Learning

`run4.py learn` starts from an empty state directory of its own, with `cpu = "140%"` and
`mem = "60%"`. Every gate runs alone under coreslot (round 1), `coreslot suggest --write` writes
the needs file, every gate runs alone again on its learned needs (round 2), and `suggest --write`
runs again. Then every gate runs alone once without coreslot, in its F form: that run is the gate's
baseline for false failures. The needs file is copied to `run4/needs.toml` and frozen; before each
K set it is copied into the state directory, and its hash is checked after the set.

### Tuning, on sets not used for the verdict

Six configurations, three sets each, with arrival offsets drawn from the seed
`run4-tune-<configuration>-<repetition>`: S with N = 2 and N = 3 (N = 4 is F, since the stand has
four heavy gates), and K with cpu `"140%"` and `"200%"`, each at mem `"60%"` and `"80%"`.

- **S_best** is the N with the lower median of the mean time to verdict; a tie goes to the smaller
  N.
- **K_best** is the configuration with the lowest median of the mean time to verdict among those
  with zero seconds of critical memory pressure and zero false failures over their three sets. When
  none qualifies, it is the one with the fewest critical seconds, then the fewest false failures,
  then the lowest median. Ties go to the lower mem share, then the lower cpu share.

The tuning result, `run4/tuning.json`, is committed before the first verdict block.

### Verdict design

Twelve valid blocks, each of three sets: F, S_best and K_best. Block b runs the arms in the order
`perms[(b - 1) mod 6]` of the fixed list FSK, SKF, KFS, FKS, KSF, SFK, so each arm runs first in
four blocks and in each position four times. Agents arrive at offsets drawn from U(0, 5 s) with the
seed `run4-<b>`, the same for the block's three sets. Before each set the runner waits 20 s and then
until the thermal state is nominal and a single-core probe is within 10% of the reading taken at the
start of the phase, at most 120 s, and records whether the Mac is on AC power and the 1-minute load.

A set is invalid only for a runner error, battery power, or a gap of more than 10 s between its
once-a-second samples (the Mac slept). A block with an invalid set is re-run at the end under the
next block number, with the same order and offsets (`run4.py verdict 13 13 --as-block B`); at most
four re-runs. The sample size is fixed: `run4.py summary` refuses to analyse before the twelfth
valid block. If the cap is spent with fewer than twelve valid blocks, the run is reported as
incomplete with its descriptive numbers and **no test and no decision**. A set that started after
the cooldown gave up is flagged `hot_start`, and one that started with a 1-minute load above 3
`high_load`; flagged sets stay valid and are reported.

### Hypotheses, declared before the run

One-sided, per block, with an exact sign test over the twelve valid blocks at α = 0.05, so a
hypothesis passes with at least 10 wins of 12. A tie counts as not a win, and the count stays 12.

- **H1.** K's mean time to verdict (each agent's chain end minus its arrival, averaged over the four
  agents) is below F's.
- **H2.** K's seconds at warn-or-worse memory pressure (`kern.memorystatus_vm_pressure_level` ≥ 2)
  are below F's.
- **H3.** K's mean time to verdict is below 1.10 × S's.

**False failures.** A failure of a gate that passed its alone baseline. A gate that failed its
baseline stays in the mix and is excluded from the count.

**Reported without a test.** The makespan ratios, the seconds at critical pressure, swapouts, false
failures, the names of failing tests, and K's `waited_free_s`. Only coreslot measures waiting while
memory was free: it counts every second a run waited while the kernel's headroom covered its memory
need, whatever the wait was for, so it is an upper bound on the waiting that memory admission
caused.

### Decision

- H1 and H3 pass: develop the one-machine scenario further and publish it as the headline.
- H1 passes and H3 fails: coreslot beats no admission but not a tuned semaphore; the next step is
  chosen from `waited_free_s`.
- H1 fails: it is reported, and the project's memory scenario is not shown.
- **Live-memory admission is worth designing** if K's `waited_free_s` is at least 5 s and at least
  10% of K's summed waiting in 6 or more of the 12 blocks.

### Disclosure

The results name the machine, the OS and every version, and give the background at each set's
start as totals only: memory in use (active, wired and compressed pages), the 1-minute load and the
process count. No process names are recorded: the owner's applications stay private. Every set is
reported, invalid and flagged ones included; the raw records (`run4/learn.jsonl`, `tune.jsonl`,
`results.jsonl` with each K set's run-log lines) are committed with the results, together with the
threats to validity.

### Reproduce

```sh
evidence/overload/setup.sh
evidence/overload/run4.sh            # learn, tune, verdict 1 12, summary, figures
evidence/overload/run4.sh verdict 13 13 --as-block 5   # a re-run of an invalid block 5
evidence/overload/run4.sh summary
```

`evidence/overload/run4.sh --dry` runs the same phases on `stand-dry.json`, two agents whose gates
allocate 300 MiB for 2 s or sleep 1 s, with one configuration per arm and one block, to check the
runner in a few minutes; its output is not evidence.

### Results (2026-10-21, blocks 1-12, coreslot `9ca499c`)

Run 4 is **complete**: twelve valid blocks, no invalid set, no re-run. H1 and H2 passed, H3
failed. By the pre-registered decision: **coreslot beats no admission but not a tuned semaphore;
the next step is chosen from `waited_free_s`**, and by its rule **live-memory admission is worth
designing**. Every number below is quoted from, or computed from, the committed records in
`run4/` (`summary.json`, printed as `summary.txt`; `results.jsonl`, `tune.jsonl`, `tuning.json`,
`learn.jsonl`, `needs.toml`), except the machine facts, read on the host with `sw_vers` and
`sysctl` when the results were written.

**Machine.** MacBook Air (Mac16,12), Apple M4, 10 cores, 16 GiB, macOS 27.0.1 (26A434), on AC
power throughout; coreslot built with Go 1.27.1, `CGO_ENABLED=0`; the runner on Python 3.13.0.
The owner's applications stayed open: at the start of the 36 verdict sets, memory in use (active +
wired + compressed) was 5379-7223 MiB (median 6008), the 1-minute load 3.15-16.84 (median 7.18) and
the process count 449-530 (median 483.5). The stand was not rebuilt since run 3 (its three
repository pins were checked before learning), so its tool versions are run 3's, listed above.

**Timeline.** The pre-registration was pushed at 08:53:59 UTC; learning started at 08:55:22,
tuning at 08:59:10; the verdict parameters were pushed at 09:33:02 and the first verdict set
started at 09:33:48, the last at 10:56:23.

**Learning.** Every gate passed alone in both rounds under coreslot and in its F-form baseline, so
every gate counts for false failures. `coreslot suggest --write` wrote (`run4/needs.toml`):

| Gate | cpu | mem | Evidence line it wrote |
|---|---|---|---|
| `vscode-typecheck` | `5` | `8576M` | 2 runs 2026-10-21: max 4.8 cores, 2 of 2 saturated, peak 8.3 GiB (footprint) at 11s |
| `fastapi-ruff` | `1` | `128M` | 2 runs 2026-10-21: max 0.6 cores, 0 of 2 saturated, peak 22 MiB (max_rss) |
| `fastapi-tests` | `1..6` | `3456M` | 2 runs 2026-10-21: max 5.5 cores, 0 of 2 saturated, peak 3.3 GiB (footprint) at 11s |
| `sqlalchemy-orm-tests` | `1..10` | `3200M` | 2 runs 2026-10-21: max 9.0 cores, 1 of 2 saturated, peak 3 GiB (footprint) at 27s |

With the 60 % budget (76 tokens of 128 MiB: 9728 of 16384 MiB), the type check's 8576 MiB leaves 1152 MiB beside it, so while a
type check runs only the lint fits next to it.

**Tuning** (`run4/tuning.json`, 18 sets, all valid): S with N = 2 had a median mean time to verdict
of 44.9 s against 64.6 s for N = 3, so S_best is N = 2. The four K configurations had medians of
34.5 s (cpu `140%`, mem `60%`), 37.0 s (`140%`, `80%`), 37.9 s (`200%`, `60%`) and 34.7 s
(`200%`, `80%`); none had a critical second or a false failure, so K_best is cpu `140%`, mem
`60%`, the defaults. S with N = 3 had one false failure. Every tuning set, as `tune.jsonl`
records it (warn+ counts one-second samples; the background is taken at the set's start; every
tuning set is flagged `high_load`, all are valid):

| Configuration | Set | Mean verdict (s) | Makespan (s) | Warn+ (samples) | Critical | Failed | Flags | Memory in use (MiB) | load1 | Processes |
|---|---|---|---|---|---|---|---|---|---|---|
| S2 | 1 | 44.9 | 68.1 | 31 | 0 | none | high_load | 9042 | 4.79 | 518 |
| S3 | 1 | 47.6 | 59.1 | 26 | 0 | none | high_load | 8698 | 10.44 | 494 |
| K-140%-60% | 1 | 37.0 | 64.7 | 0 | 0 | none | high_load | 7414 | 16.03 | 472 |
| K-140%-80% | 1 | 37.0 | 61.7 | 4 | 0 | none | high_load | 7244 | 10.96 | 471 |
| K-200%-60% | 1 | 45.0 | 73.0 | 0 | 0 | none | high_load | 7275 | 9.64 | 467 |
| K-200%-80% | 1 | 39.9 | 64.1 | 3 | 0 | none | high_load | 7290 | 5.93 | 470 |
| S2 | 2 | 55.7 | 77.4 | 34 | 0 | none | high_load | 7395 | 9.37 | 470 |
| S3 | 2 | 67.7 | 85.3 | 45 | 0 | none | high_load | 6102 | 9.38 | 445 |
| K-140%-60% | 2 | 30.8 | 57.2 | 0 | 0 | none | high_load | 6672 | 7.36 | 468 |
| K-140%-80% | 2 | 37.6 | 63.5 | 0 | 0 | none | high_load | 6243 | 7.13 | 474 |
| K-200%-60% | 2 | 37.9 | 64.8 | 0 | 0 | none | high_load | 6167 | 5.01 | 469 |
| K-200%-80% | 2 | 31.8 | 48.3 | 0 | 0 | none | high_load | 6860 | 3.98 | 511 |
| S2 | 3 | 35.7 | 55.9 | 0 | 0 | none | high_load | 5358 | 6.14 | 511 |
| S3 | 3 | 64.6 | 85.2 | 43 | 0 | fastapi-tests | high_load | 6537 | 7.2 | 515 |
| K-140%-60% | 3 | 34.5 | 65.2 | 0 | 0 | none | high_load | 6097 | 6.19 | 489 |
| K-140%-80% | 3 | 32.8 | 58.2 | 0 | 0 | none | high_load | 6767 | 4.2 | 511 |
| K-200%-60% | 3 | 34.0 | 66.0 | 0 | 0 | none | high_load | 6307 | 5.07 | 505 |
| K-200%-80% | 3 | 34.7 | 59.8 | 0 | 0 | none | high_load | 5816 | 4.15 | 507 |

At the start of the tuning sets memory in use was 5358-9042 MiB (median 6719.5), the 1-minute load
3.98-16.03 (median 6.66) and the process count 445-518 (median 481.5).

**Medians over the twelve valid blocks:**

| Arm | Mean time to verdict (s) | Makespan (s) | Warn+ (s) | Critical (s) | Swapout (MiB) | False failures (median, total) | `waited_free_s` |
|---|---|---|---|---|---|---|---|
| F | 84.45 | 99.1 | 58.0 | 0.0 | 8224.0 | 0, 3 | - |
| S (N = 2) | 34.45 | 52.85 | 0.0 | 0.0 | 0.0 | 0, 0 | - |
| K | 35.3 | 63.1 | 0.0 | 0.0 | 0.0 | 0, 0 | 36.45 |

**The tests** (one-sided exact sign test over 12 blocks, α = 0.05; a tie is not a win):

- **H1, passed:** K's mean time to verdict was below F's in 12 of 12 blocks, p = 0.0002. The
  per-block ratio K/F ran from 0.371 to 0.555 (median 0.4305).
- **H2, passed:** K's seconds at warn-or-worse pressure were below F's in 12 of 12 blocks,
  p = 0.0002: K had no such sample, F a median of 58. The runner counts one-second samples, which
  lay about 1.1 s apart in F's sets, so the time itself was about a tenth longer.
- **H3, failed:** K's mean time to verdict was below 1.10 × S's in 7 of 12 blocks, p = 0.3872.
  The per-block ratio K/S ran from 0.81 to 1.314 (median 1.0065).

**Reported without a test.**

- *Makespan:* K/F per block 0.505-0.826 (median 0.6365); K/S 0.887-1.313 (median 1.1415), so K
  finished the whole mix later than the two-slot semaphore in 9 of 12 blocks.
- *Critical pressure:* 0 s in every set of every arm. *Swapouts:* F a median of 8224 MiB per set;
  S 0 except 24.2, 38.0 and 23.2 MiB in blocks 1, 9 and 12; K 0 except 4.2 MiB in block 12.
- *False failures:* 3, all in F and all in FastAPI's suite, agent A3, in blocks 3, 7 and 11; none
  in S or K. The failing tests: in blocks 3 and 11,
  `ERROR tests/test_tutorial/test_static_files/test_tutorial001.py::test_openapi_schema`; in block
  7, `FAILED tests/test_tutorial/test_static_files/test_tutorial001.py::test_static_files` and the
  same `ERROR` line.
- *K's `waited_free_s`* per block, beside K's summed waiting: 37.6 of 90.5 s, 49.4 of 76.5, 11.5 of
  49.6, 3.8 of 64.6, 10.8 of 69.6, 46.3 of 102.4, 35.3 of 99.4, 43.4 of 75.5, 5.8 of 64.6, 34.3 of
  91.4, 40.6 of 94.4, 51.4 of 78.4 (blocks 1-12; median 36.45 s). It reached at least 5 s and at
  least 10 % of K's waiting in 10 of 12 blocks, the pre-registered rule for designing live-memory
  admission. It is an upper bound: it counts every second a run waited while the kernel's headroom
  covered its memory need, whatever the wait was for, including waits for cpu tokens.
- *Flags:* every one of the 36 verdict sets is flagged `high_load` (the 1-minute load at its start
  was above 3); the F set of block 2 is also
  flagged `hot_start` (the cooldown gave up). Flagged sets stay valid, as pre-registered. The
  thermal state read nominal at every set's start; during the sets it reached state 1 ("fair") in
  all 36 verdict sets, in 145 of F's 217 thermal samples, 56 of S's 129 and 53 of K's 146, and never
  went above it.

Figures: [`run4/ratios.svg`](overload/run4/ratios.svg) (per-block K/F and K/S ratios) and
[`run4/timeline.svg`](overload/run4/timeline.svg) (block 1, the lower of the two blocks around
the median K/F ratio: 0.429, against block 11's 0.432; each arm's gates per agent over time, with
the memory-pressure level).

**What it shows.** On this laptop and this mix, coreslot with needs it learned from two alone runs
per gate had no sample at warn-or-worse pressure where running everything at once had a median of
58, swapped 4.2 MiB in one set where it swapped a median of 8224 MiB, had no false failure where it
had 3 (a count, not tested), and cut the mean time to verdict to less than half (median ratio
0.4305). It did not
come within 10 % of a semaphore whose slot count was tuned for this mix: their mean times to
verdict were close (median ratio 1.0065), but K's makespan was longer. The type check's
learned need takes 8576 of the 9728 MiB budget, so a test suite cannot start beside it, and K's
runs spent a median of 36.45 s per block waiting while the kernel reported their memory need free
(an upper bound); by the pre-registered rule live-memory admission is worth designing, and the
next step is chosen from `waited_free_s`. Of the four static shares tuned, none had a lower median
than the default, on three sets each.

**Threats to validity.** One machine, one mix and one day. A fanless laptop throttles: the cooldown
and the probe bound it, and the thermal state read nominal at every set's start, but it reached
"fair" during all 36 verdict sets, in a larger share of F's samples (145 of 217) than of S's (56 of
129) or K's (53 of 146), which weighs against F; and sets ran under a 1-minute load above 3. The owner's applications were open (totals above). The learned needs come from alone runs, two
per gate. Three tuning sets per configuration choose the configuration but prove nothing about it.
The 1 s sampler can miss a short peak, bounded below by the largest single process's `max_rss`.
`waited_free_s` is an upper bound, as stated. S's slot count was tuned on this exact mix, which a
user of a semaphore would have to do by hand; K's needs were learned without tuning, and its shares
were tuned among four configurations whose medians lay within 3.4 s.

**What it does not show.** Several machines, Linux, other mixes, or any mix where a tuned slot
count is not known in advance.

**Reproduce.** `evidence/overload/setup.sh`, then `evidence/overload/run4.sh` (learn, tune,
verdict 1 12, summary, figures); `run4.sh summary` recomputes the summary from the committed
records. `setup.sh` builds coreslot without `CGO_ENABLED=0`; the measured binary was built with
`CGO_ENABLED=0 go build -o ~/bench.noindex/bin/coreslot ./cmd/coreslot` from this branch, whose Go
code is that of `9ca499c`.

## Acceptance run 5: the live memory size against the static size (`run5.py`)

**Status: pre-registered.** This section was committed and pushed before the first verdict set;
the results are appended below, whatever they are. The analysis is `run5.py summary`, which is not
changed after this commit.

**Question.** On run 4's laptop and mix, does coreslot's live memory size remove the waits that the
static `mem` budget imposed while the kernel could have covered the memory, and at what cost in
memory pressure?

Run 4 found its K runs waiting a median of 36.45 s per block while the kernel's headroom covered
their memory need. The live size, added since, raises the `mem` size for one admission attempt
when the static size less the locked tokens cannot cover a fixed `mem` need: to the locked tokens
plus the kernel's headroom less the unused part of every running hold's reservation, never below
the static size (PROTOCOL.md at `54f8cb0`, "Live size of `mem`"). `[memory] live = false` turns it off.

### Stand

Run 4's stand, unchanged: `evidence/overload/stand4.json`, the same four agents, gates and argv, on
the same host. The needs are run 4's, not learned again: `run5/needs.toml` is a byte-for-byte copy
of `run4/needs.toml`, copied into the state directory before each coreslot set and its hash checked
after it. coreslot is built at `54f8cb0`, the head of an open pull request (the live memory size),
not a release, with `CGO_ENABLED=0` and no build tags, into `~/bench.noindex/bin/coreslot`.

With the shares below the static sizes are 14 cpu tokens and 76 `mem` tokens (9728 of 16384 MiB);
each set records the sizes `coreslot status --json` reports. The type check's 67 tokens leave 9
beside it, so under the static size only the lint fits next to a type check.

### Arms

- **KS** — every gate as `coreslot run --label bench:<gate> -- <argv>`, with no `--need`, cpu
  `"140%"`, mem `"60%"` and `[memory] live = false`: the static size, run 4's K_best.
- **KL** — the same with `[memory] live = true`: the live size.
- **S** — a counting semaphore of 2 slots around the heavy gates; the lint runs free; `{n}` is
  `auto`: run 4's S_best.

There is no tuning phase: every arm's configuration is fixed above.

### Verdict design

Twelve valid blocks, each of three sets: KS, KL and S. Block b runs the arms in the order
`perms[(b - 1) mod 6]` of the fixed list KS S KL, S KL KS, KL KS S, KS KL S, KL S KS, S KS KL (run
4's list with F read as KS and K as KL), so each of the six orders is used twice. Agents arrive at
offsets drawn from U(0, 5 s) with the seed `run5-<b>`, the same for the block's three sets. Pacing,
cooldown and probe are run 4's: before each set the runner waits 20 s and then until the thermal
state is nominal and a single-core probe is within 10% of the reading taken at the start of the
phase, at most 120 s. Every one-second sample adds `headroom_mib`, the free, speculative, purgeable
and file-backed pageable pages (`vm.page_free_count`, `vm.page_speculative_count`,
`vm.page_purgeable_count`, `vm.page_pageable_external_count`) times `vm.pagesize`, the sysctls
coreslot itself reads.

**Validity, as run 4's.** A set is invalid only for a runner error, battery power, or a gap of more
than 10 s between its once-a-second samples. A block with an invalid set is re-run at the end under
the next block number, with the same order and offsets (`run5.py verdict 13 13 --as-block B`); at
most four re-runs. `run5.py summary` refuses to analyse before the twelfth valid block. If the cap
is spent with fewer than twelve valid blocks, the run is reported as incomplete with its
descriptive numbers and **no test and no outcome**. A set that started after the cooldown gave up
is flagged `hot_start`, and one that started with a 1-minute load above 3 `high_load`; flagged sets
stay valid and are reported.

### Criteria, declared before the run

- **Mechanism:** a one-sided exact sign test over the 12 blocks that KL's summed `waited_free_s`
  (over the set's run-log lines) is below KS's; a tie is not a win; α = 0.05, so it passes with at
  least 10 wins of 12.
- **Price:** passes when KL has no critical second (`kern.memorystatus_vm_pressure_level` ≥ 4) and
  no false failure in any block, and the median over the 12 blocks of (KL's warn-or-worse seconds
  − KS's, per block) is at most **10 s**, a sixth of the 58 s median that no admission caused in
  run 4.

**False failures.** As run 4's: a failure of a gate that passed its alone baseline, run 4's F-form
runs in `run4/learn.jsonl`.

**Reported without a test.** The mean time to verdict and the makespan per arm; the per-block
ratios KL/KS and KL/S; for KS and KL, run 4's wait decomposition: each stretch of a run's wait,
replayed against the static sizes, classed as memory only (its own `mem` need did not fit, its cpu
minimum did), memory and cpu, cpu only, behind the head (it fitted, but the earliest queued waiting
run did not, for memory, memory and cpu, or cpu), or latency (nothing was lacking); and how many KL
runs were admitted above the static size (their grant and the grants held at their admission
together above 76 tokens). The same count for KS checks the replay: on run 4's K sets it is 0 in
every block.

### What each outcome means

- Mechanism and price pass: the live size does what it is for at an acceptable cost.
- Mechanism fails: it does not remove the waits on this machine.
- Price fails: it costs more pressure than allowed; reported as is.

### Reproduce

```sh
evidence/overload/run5.sh            # verdict 1 12, summary
evidence/overload/run5.sh verdict 13 13 --as-block 5   # a re-run of an invalid block 5
evidence/overload/run5.sh summary
```

`evidence/overload/run5.sh --dry` learns the needs of `stand-dry.json` with `run4.py learn` and runs
one block of the three arms on it, to check the runner in a minute; its output is not evidence.

### Results (2026-10-21, blocks 1-12, coreslot `54f8cb0`)

Run 5 is **complete**: twelve valid blocks, no invalid set, no re-run. The mechanism criterion
**failed** and the price criterion **passed**. By the pre-registered reading: **the live size does
not remove the waits on this machine.** It cost nothing in memory pressure either: in this run
it never admitted a run above the static size. Every number below is quoted from, or computed from,
the committed records in `run5/` (`summary.json`, printed as `summary.txt`; `results.jsonl`),
except the machine facts, read on the host with `sw_vers` and `sysctl` when the results were
written.

**Machine.** Run 4's: MacBook Air (Mac16,12), Apple M4, 10 cores, 16 GiB, macOS 27.0.1 (26A434),
on AC power throughout; coreslot built with Go 1.27.1, `CGO_ENABLED=0`; the runner on Python
3.13.0. The owner's applications stayed open. At the start of the 36 verdict sets, memory in use
(active + wired + compressed) was 6735-10119 MiB (median 7539.5), about 1.5 GiB more than in run 4
(median 6008). The 1-minute load was 2.23-15.17 (median 6.045) and the process count 517-572
(median 544.5).

**Timeline.** The pre-registration was committed at 15:04:33 UTC and pushed at 15:04:36. The
first verdict set started at 15:06:56 and the last at 16:18:43.

**Medians over the twelve valid blocks:**

| Arm | Mean time to verdict (s) | Makespan (s) | Warn+ (s) | Critical (s) | False failures (total) | `waited_free_s` |
|---|---|---|---|---|---|---|
| KS (static) | 37.8 | 66.1 | 0.0 | 0.0 | 0 | 27.65 |
| KL (live) | 37.15 | 66.65 | 0.0 | 0.0 | 0 | 29.3 |
| S (N = 2) | 37.1 | 62.4 | 6.0 | 0.0 | 0 | - |

**The criteria** (as declared):

- **Mechanism, failed:** KL's summed `waited_free_s` was below KS's in 2 of 12 blocks, p = 0.9968.
  It was lower in blocks 1 and 12, higher in blocks 4 and 6, and equal in the other eight. Per
  block, KS then KL, as `waited_free_s` of all waiting:
  - KS: 25.8 of 59.6 s, 38.3 of 77.4, 28.8 of 95.4, 28.5 of 85.4, 29.8 of 79.4, 21.8 of 106.4,
    0 of 68.6, 17.8 of 44.6, 0 of 81.6, 37.3 of 78.4, 34.8 of 93.5, 26.8 of 108.4.
  - KL: 21.8 of 39.6 s, 38.3 of 77.4, 28.8 of 95.4, 30.5 of 83.4, 29.8 of 79.4, 29.8 of 126.5,
    0 of 68.6, 17.8 of 40.6, 0 of 69.6, 37.3 of 84.4, 34.8 of 93.4, 22.8 of 102.4.

  The test compares the sums as the summary rounds them, to 0.1 s; the pre-registration did not
  say so. On the unrounded sums KL is lower in 6 blocks (1, 3, 5, 8, 11, 12), higher in 4 and
  equal in 2 (7 and 9, both 0), p = 0.6128; in blocks 3, 5, 8 and 11 the difference is at most
  0.02 s. The outcome is the same.
- **Price, passed:** KL had no critical second and no false failure. The per-block difference of
  warn-or-worse seconds, KL less KS, was −14 s in block 1 and 0 in the other eleven (median 0 s,
  limit 10 s).

**Why the two K arms behaved alike.** The replay of every K run against the static sizes counts
the runs whose admission took `mem` above the static 76 tokens. It found **0 in KL**, as in KS. In
this run, by that replay, the live size never raised `mem` for an admission, so KL ran the static
rule. We read the remaining differences between its sets and KS's as run-to-run noise (an
inference: the arms differ only in the switch); in 8 of 12 blocks the two arms' rounded
`waited_free_s` came out equal.

The wait decomposition, summed over the twelve blocks, puts KS's waiting and KL's in the same
places:

| Arm | Own memory | Own memory and cpu | Own cpu | Head's memory | Head's memory and cpu | Head's cpu | Latency | Above static (runs) |
|---|---|---|---|---|---|---|---|---|
| KS | 465.1 | 236.2 | 0 | 149.4 | 64.8 | 0 | 63.2 | 0 |
| KL | 439.4 | 237.9 | 0 | 147.0 | 64.9 | 0 | 71.6 | 0 |

Where the `waited_free_s` came from, summed over the twelve blocks for KS and KL:

| Gate | KS (s) | KL (s) |
|---|---|---|
| The lint | 98.2 | 116.2 |
| The SQLAlchemy suite | 139.9 | 151.9 |
| The FastAPI suite | 44.1 | 15.0 |
| The type check | 7.5 | 8.5 |

The lint needs one token, which the static size leaves free beside any other gate; its waits are
for a head that does not fit, which no live size reaches under strict FIFO.

The records hold each run's memory peak (`mem_peak_kib`, `peak_after_s`) but not the holders'
resident memory over time, so the live room an attempt saw cannot be recomputed. The bound it had
to meet can be stated, and what follows from it is an inference, not a measurement:
- **A suite queued while a type check runs.** The suite needs 25 or 27 tokens (3200 or 3456 MiB).
  The type check's reservation is 8576 MiB, and only the part its tree already uses is subtracted
  from that reservation.
- **What the headroom showed.** Over KL's one-second samples the headroom was 1846-9473 MiB
  (median 6224). Where it covered a suite's need by itself, `waited_free_s` counts the second.
  The type check's footprint peaked at 8017-8914 MiB, 8.0-13.0 s after its start (its 48 K runs'
  `mem_peak_kib`), so its unused reservation is most of 8576 MiB early in its run and little or
  none at its peak. We infer that the headroom less that unused part rarely covered a suite: no
  KL admission went above the static size.

**Reported without a test.**

- *Time to verdict and makespan:*
  - KL/KS per block: mean time to verdict 0.799-1.141 (median 0.996), makespan 0.801-1.112
    (median 1.006).
  - KL/S per block: mean time to verdict 0.476-1.373 (median 0.978), makespan 0.611-1.289
    (median 1.115).
- *S's pressure:*
  - S had warn-or-worse samples in 8 of 12 sets (median 6 s), and the only critical seconds of
    the run: 3 s, in block 8.
  - S swapped out 11965.6, 2235.1, 10040.5, 1994.0 and 7955.8 MiB in blocks 1, 7, 8, 9 and 10.
  - KS swapped out 238.3 MiB in block 1 and KL nothing.
- *False failures:* none in any arm.
- *Flags:* 35 of the 36 verdict sets are flagged `high_load` (the 1-minute load at their start was
  above 3); none is flagged `hot_start`. Flagged sets stay valid, as pre-registered. The thermal
  state read nominal at every set's start. During the sets it reached state 1 ("fair") in 55 of
  KS's 153 thermal samples, 57 of KL's 154 and 64 of S's 153, and never went above it.

**What it shows.** On this laptop and this mix, with memory in use at a median 7.5 GiB at the sets'
start, the live size never admitted a run that the static 60 % share did not (measured, by the
replay). The waits that `waited_free_s` counts stayed: a median of 29.3 s per block under KL and
27.65 s under KS. Pressure, time to verdict and makespan were those of the static rule. Why, by
inference from the bound above: the rule subtracts the part of the type check's 8576 MiB
reservation that its tree has not used yet, and while a type check ran the headroom less that
part did not cover a suite; and a run queued behind a head (the lint, the suites) is not reached
by any size under strict FIFO.

**Threats to validity.**
- One machine, one mix and one day.
- **The background.** It used about 1.5 GiB more memory than in run 4, which shrinks the headroom
  the live size reads.
- **What the records omit.** Holders' resident memory over time was not recorded, so the live
  room per attempt, and the reason no admission went above the static size, are inferred, not
  measured.
- **The sets themselves.** Sets ran under a 1-minute load above 3 and reached "fair" thermal state.
- **The build.** The measured coreslot is the head of an open pull request, not a release.

**What it does not show.** A machine with more memory, or with a smaller learned need against its
share, where the room after reservations covers a second heavy run. Linux. Any head-of-line
change.

**Reproduce.** `evidence/overload/run5.sh` (verdict 1 12, summary); `run5.sh summary` rewrites
`summary.json` and `summary.txt` from the committed records (byte-identical). The measured binary was built with
`CGO_ENABLED=0 go build -trimpath -o ~/bench.noindex/bin/coreslot ./cmd/coreslot` at `54f8cb0`.

## Acceptance run 6: coreslot against no admission and every semaphore, under background memory (`run6.py`)

**Status: pre-registered.** This section, the runner and the frozen inputs (the needs files and
the learn records, with their SHA-256) were committed and pushed before the first pilot set. The
pilot's sizing, `run6/size.json`, is committed and pushed before the first main set. The results
are appended below, whatever they are. The analysis is `run6.py size` and `run6.py summary`. The
code that defines the records, the metrics and the analysis is frozen at the pre-registration
commit: `run6.py`, `stats6.py`, `figures6.py`, `ballast.py`, `overload/run6.sh`, `run4.py`,
`acceptance.py` and `overload.py`; none of them is changed after it.

**Question.** On one 16 GiB laptop, under three levels of background memory, does coreslot with
needs it learned itself cost nothing against running everything at once, and does it match the
best counting semaphore on time while beating it on memory?

### Claims

- **C1, free under pressure.** On the memory-heavy mix, coreslot's mean time to verdict is below
  running everything at once, and its memory pressure and swap are lower (false failures
  reported).
- **C2, free without pressure.** On the light mix, coreslot's mean time to verdict is at most 3 %
  above running everything at once (the 1 % and 5 % margins reported).
- **C3, the best semaphore, time.** coreslot (its share chosen on other blocks, by cross-fitting)
  is at most 3 % slower than the semaphore with the best N, chosen in hindsight per background
  level on the tested blocks (an oracle favouring the semaphore).
- **C4, the best semaphore, safety.** Against that speed-optimal semaphore, coreslot has less
  memory pressure and less swap.
- **C5, robustness.** Every fixed N is worse than coreslot at its untuned default (mem 60 %) at
  some background level: slower beyond 3 %, or more pressure.

**Stated cost, no claim.** The makespan, reported with its interval and its mechanism: strict FIFO
with peak reservations leaves capacity idle while the head of the queue waits.

### Stand

Run 4's stand file, unchanged: `evidence/overload/stand4.json`, on run 4's host.

- **The heavy mix**: run 4's four agents. A1 and A2 each run the VS Code type check; A3 runs
  FastAPI's ruff, then its pytest suite; A4 runs SQLAlchemy's pytest `test/orm`.
- **The light mix**: agents A3 and A4 of the heavy stand, no type check.

coreslot is built at `ebac0a7` (`coreslot 0.1.1-0.20261021174039-ebac0a73fadd`), the head of an
open pull request, not a release, with `CGO_ENABLED=0 go build -trimpath -o
~/bench.noindex/bin/coreslot ./cmd/coreslot`. Every coreslot call goes through the stand's own
state directory; the owner's budget and configuration are never touched.

### Arms

- **F**: no admission; every gate starts when its agent reaches it; `{n}` is `auto`.
- **S1, S2, S3**: a counting semaphore of 1, 2 or 3 slots around the heavy gates (the type checks
  and the test suites); the lint runs free; `{n}` is `auto`. F is the semaphore of four slots,
  one per agent.
- **K60, K67**: every gate as `coreslot run --label bench:<gate> -- <argv>`, with no `--need`,
  cpu `"140%"`, mem `"60%"` or `"67%"`, and the learned `run6/needs.toml`. The configuration
  writes no `[memory]` section, so the live rule is on, the product default. `{n}` is the cpu
  coreslot grants the gate, part of coreslot's treatment.
- **KH**: as K60 with the hand weights `run6/needs-hand.toml`: what a user writes by hand after
  watching one run. Its cpu is fixed per gate, where the learned needs give the two test suites
  an elastic range.
- **KC**: as K60 with no needs file in the state directory, a cold start.

Which arms run where:

| Phase | Mix | Arms | Arrivals | Ballast |
|---|---|---|---|---|
| pilot, main | heavy | F, S1, S2, S3, K60, K67, KH | U(0, 5 s) | 0, 1536 or 3072 MiB |
| pilot, main | light | K60, F | U(0, 5 s) | none |
| sensitivity | spread | F, S1, S2, S3, K60 | U(0, 30 s) | none |
| sensitivity | cold | K60, KC | U(0, 5 s) | none |

With 128 MiB tokens, mem `"60%"` is 76 tokens (9728 of 16384 MiB) and `"67%"` 85 tokens; cpu
`"140%"` is 14 tokens. Each K set records the sizes `coreslot status --json` reports, read once
per verdict command; F and S sets record none. Each set records `coreslot version`, read once per
verdict command.

### Background levels and the ballast

A heavy block holds a ballast for the whole block, every set and the pauses between them: a child
process (`ballast.py`) holding 0, 1536 or 3072 MiB of resident memory, filled from `os.urandom` so
the compressor cannot shrink it, and touching one byte per 16 KiB page once a second so its pages
stay in use. It exits when the runner dies, so a killed runner never leaves it holding memory.
Each set records the ballast's resident size.

**The level schedule.** Blocks 1-3, 4-6, ... each hold every level once, in an order shuffled with
the seed `run6-level-<phase>-<group>`, the group being `(block - 1) div 3`. So 12 pilot blocks hold
4 per level, and the heavy main n, a multiple of 3, holds n / 3 per level.

**The order schedule.** Block b of a phase and mix runs its arms in an order shuffled with the
seed `run6-order-<phase>-<mix>-<b>`. Each set records its position and the arm before it.

**Arrivals.** Agents arrive at offsets drawn from U(0, 5 s), U(0, 30 s) in the spread run, with the
seed `run6-<phase>-<mix>-<b>`, the same for every set of the block.

**Pacing**, as run 4's: before each set the runner waits 20 s and then until the thermal state is
nominal and a single-core probe is within 10 % of the reading taken at the session's start, at
most 120 s.

**The primer.** Each session starts with `--primer`: one K60 set of the session's first block,
inside its ballast, recorded to `run6/primer.jsonl` only. It warms the stand after the pause and
enters no analysis.

### Learning, frozen before the pilot

`run6.py learn` starts from an empty state directory with cpu `"140%"` and mem `"60%"`, and runs
every gate alone, back to back:

- **6 runs in its K form** (under coreslot). `coreslot suggest --write` then writes the needs file
  from the label's last 5 runs, the product's window, so the first, cold run drops out.
- **3 runs in its F form** (its argv with `{n}` as `auto`, no coreslot). They are the false-failure
  baseline: a gate counts for false failures only if it passed all three. They are also the
  slowdown reference for F and S.
- **The hand weights** (KH) come from each gate's first F-form run: its peak footprint (the summed
  physical footprint of the runner's descendants, sampled once a second) rounded up to a whole GiB,
  and its cores (CPU seconds over the run time) rounded up, at least 1 of each; pinned, and fixed.

Every gate passed all 6 K-form and all 3 F-form alone runs, so every gate counts for false
failures. `suggest --write` wrote (`run6/needs.toml`):

| Gate | cpu | mem | tokens | Evidence line it wrote |
|---|---|---|---|---|
| `vscode-typecheck` | `5` | `8960M` | 70 | 5 runs 2026-10-21: max 4.9 cores, 5 of 5 saturated, peak 8.7 GiB (footprint) at 12s |
| `fastapi-ruff` | `2` | `128M` | 1 | 5 runs 2026-10-21: max 1.5 cores, 5 of 5 saturated, peak 21 MiB (max_rss) |
| `fastapi-tests` | `1..6` | `3200M` | 25 | 5 runs 2026-10-21: max 5.6 cores, 0 of 5 saturated, peak 3.1 GiB (footprint) at 8s |
| `sqlalchemy-orm-tests` | `1..10` | `2944M` | 23 | 5 runs 2026-10-21: max 9.0 cores, 0 of 5 saturated, peak 2.8 GiB (footprint) at 33s |

The hand weights (`run6/needs-hand.toml`): the type check cpu 5, mem 9G (72 tokens); the lint cpu
3, mem 1G (8 tokens: its first F-form run ended before a sample saw it, so 1 GiB is the floor, not
a measurement); the FastAPI suite cpu 5, mem 3G (24); the SQLAlchemy suite cpu 9, mem 3G (24).

**Expected before the data.** Under the learned needs, mem 60 % leaves 6 tokens beside a type
check and 67 % leaves 15: both admit only the lint beside it, so K67 is expected to behave almost
like K60. This is stated before the data, not a reason to change the design. Under the hand
weights, the type check's 72 tokens leave 4, so not even the lint runs beside it. In the light mix
the two suites need 48 tokens together, so K60 admits both at once.

**The frozen files.** `run6/needs.sha256`, and beside it the SHA-256 of the learn records
`run6/learn.jsonl`, which `summary` reads for the slowdown reference and the false-failure
baseline:

```
5873710ad727e55bd87ee367cc307b0fba0671be6c6249618e78e5df0f18816a  needs.toml
d46a727f87e808c4f9c94463dc918beeb4be0c02093bee0795866a16640fa98d  needs-hand.toml
2cd410865d5816209cd61d2c23ab5812a5c34300c4bfb00312361b6361557eb7  learn.jsonl
```

Every verdict command exits before its first set unless both needs files match `needs.sha256`;
the check runs once per command. Each K set records the needs file's SHA-256 taken at the
command's start, copies the file into the state directory at the set's start, and is invalid if,
when the set ends, that copy's SHA-256 differs from the file's at the set's start. `learn`
refuses once any pilot, main or sensitivity record exists.

### Metrics

- **Primary: the mean time to verdict.** Each agent's chain end minus its arrival, averaged over
  the mix's agents.
- **Pressure.** Warn seconds: the once-a-second samples with `kern.memorystatus_vm_pressure_level`
  ≥ 2; critical seconds: ≥ 4. A sample with no pressure reading counts as normal.
- **Swap.** Swapout bytes: the rise of the swapout counter over the set times the page size. The
  swapin and pagein counters are reported.
- **Makespan.** The last gate's end.
- **False failures.** A failure of a gate that passed all three F-form alone runs, counted with
  its exact 95 % interval, per arm and per gate. Every nonzero exit counts, coreslot's own exit
  codes and the gates of a set coreslot failed included.
- **Slowdown.** A gate's own execution over its form's median alone time. For F and S it is the
  gate's wall time after the semaphore over the median of its 3 F-form runs; for K it is the run
  log's `ran_s` over the median `ran_s` of K-form runs 2-6. The alone runs ran back to back with no
  cooldown, so this reference was taken on a warm machine.

**A set coreslot failed.** A K set with a gate that left no run-log line, ran fail-open, or exited
with one of coreslot's own codes (64, 70, 75, 126, 127) stays in its block, flagged
`coreslot_failed`. Its primary becomes the block's slowest primary, and in every sign test its
warn seconds and swap count as worse than any value.

### The pilot and the size rule

The pilot runs 12 heavy blocks (4 per level) and 12 light blocks, all arms, with the main phase's
rules; it enters no hypothesis test. `run6.py size` reads the first 12 valid pilot blocks of each
mix, counting at most 3 re-runs per mix, and exits 2 while a mix has fewer:

- **σ̂ heavy**: the standard deviation of the per-block ln(K\*/S\*). K\* is the per-level oracle
  among K60 and K67, S\* among S1, S2, S3 and F: the arm with the lowest mean primary over the
  level's pilot blocks, a tie to the earlier arm in that list.
- **σ̂ light**: the standard deviation of the per-block ln(K60/F).
- **The σ adjustment.** Each σ̂ is raised to its one-sided 80 % upper bound,
  σ = σ̂ · √(11 / χ²₀.₂₀,₁₁), the χ² quantile by Wilson-Hilferty.
- **The size rule.** n = ⌈(z₀.₉₅ + z₀.₈₀)² σ² / ln(1.03)²⌉ + 2: blocks for a one-sided paired
  t test at α = 0.05 with power 0.80 against the margin ln(1.03), when the true ratio is 1. Heavy:
  rounded up to a multiple of 3, then clamped to at least 30 and at most 120. Light: clamped to at
  least 12 and at most 60. When the formula's n is above the cap, `size.json` says
  `underpowered` and the run goes on at the cap.
- **The light mix's pressure count**: the F sets of the first 12 valid light pilot blocks with a
  warn-or-worse sample.

`size.json` holds both σ̂, both σ, the unclamped and clamped n and that count. `size` refuses once a
main record exists.

### Hypotheses, declared before the run

The main phase runs blocks 1..n of each mix. `run6.py summary` reads only the main records (never
the pilot's) and chooses the oracles on exactly the blocks it tests:

- **S\*** per background level: among S1, S2, S3 and F, the arm with the lowest mean primary over
  that level's tested blocks, a tie to the earlier arm. It is chosen in hindsight, on the tested
  blocks, which favours the semaphore.
- **K\*cf** per block, by cross-fitting: each level's blocks, in block order, are split
  alternately into two halves; a block gets the share (K60 or K67) with the lower mean primary over
  the other half, a tie to K60, and K60 when the other half is empty.

Each hypothesis is one-sided at α = 0.05. A paired t test is on per-block log ratios; a sign test
is exact on per-block pairs, ties dropped (all ties: p = 1, no wins). "Coreslot" in C1, C2 and C5
is K60.

- **H1 (C1).** On the heavy blocks: the mean of ln(K60/F) is below 0 (paired t), **and** K60's
  warn seconds are below F's (sign test), **and** K60's swapout bytes are below F's (sign test).
  Rejected when all three have p < 0.05.
- **H4 (C4).** On the heavy blocks: K\*cf's warn seconds are below S\*'s, **and** K\*cf's swapout
  bytes are below S\*'s, both sign tests with p < 0.05.
- **H5 (C5).** For each N of 1, 2 and 3, six tests, each at α / 6: at each of the three levels, the
  mean of ln(S_N/K60) is above ln(1.03) (paired t, its lower bound against the margin), and K60's
  warn seconds are below S_N's (sign test). N passes when any of its six tests has p < 0.05 / 6.
  Rejected when all three N pass.
- **H3 (C3).** On the heavy blocks: the mean of ln(K\*cf/S\*) is below ln(1.03) (paired t),
  p < 0.05. Per level, its geometric mean ratio and one-sided 95 % upper bound are reported, and a
  level whose bound is above 1.03 is flagged.
- **H2 (C2).** On the light blocks: the mean of ln(K60/F) is below ln(1.03) (paired t), p < 0.05.
  The 1 % and 5 % margins are read off H2's reported one-sided 95 % upper bound of the geometric
  mean ratio (below 1.01, below 1.05); they add no test.

H1, H3 and H2 report their geometric mean ratio with a one-sided 95 % upper bound and a BCa 90 %
interval. The H5 time tests report the ratio and its lower bound, and H3's per-level rows the
ratio and its upper bound, with no BCa interval.

**The chain.** The hypotheses are tested in the fixed order H1, H4, H5, H3, H2, each at the full
α = 0.05. The chain stops at the first one not rejected or untested; every hypothesis after it is
reported as exploratory and supports no claim.

**The light mix's pressure rule.** C2 is about a mix without pressure. If more than one F set of
the first 12 valid light pilot blocks had a warn-or-worse sample, H2 is untested.

**Reported without a test.**

- Per arm and mix: the mean primary, the median makespan and warn seconds, the critical seconds,
  swapout bytes, swapins and pageins (median, mean and the BCa 90 % interval of the mean; every
  BCa interval takes 10,000 bootstrap replicates with the seed 0), false
  failures (count, exact 95 % interval, per gate), and each gate's slowdown.
- As geometric mean ratios with a one-sided 95 % upper bound and a BCa 90 % interval:
  - the stated cost, the makespan ratios K60/F (heavy and light) and K\*cf/S\*;
  - K60/S\*, KH/K60, K\*/S\* (coreslot's share chosen in hindsight too), and K\*cf/Scf (the
    semaphore cross-fitted like coreslot), with Scf's choices;
  - each fixed semaphore's regret against S\*, per level.
- Order effects, on the heavy mix: each set's log primary over its block's geometric mean, by
  position and by the arm before it.
- The sensitivity runs, the first 6 valid blocks each, with no re-run cap; a run with fewer valid
  blocks is reported as it is: the spread run's mean primaries and each of F, S1, S2 and S3 over
  K60; the cold run's KC/K60.
- The covariates each set records (memory in use, the thermal state, the 1-minute load),
  tabulated from the set records per arm and background level in the results (`summary` does
  not compute them). No covariate-adjusted test is run.
- The sets coreslot failed, and the oracles chosen.

### Validity, re-runs and an incomplete run

- **A valid set.** As run 4's: a set is invalid only for a runner error, battery power, a gap of
  more than 10 s between its once-a-second samples (the Mac slept), or a needs file that changed.
  A set that started after the cooldown gave up is flagged `hot_start`, and one that started with a
  1-minute load above 3 `high_load`; flagged sets stay valid and are reported.
- **A valid block** is one whose every arm has a valid set. An invalid set drops its whole block
  from every hypothesis and from the oracle choice until a re-run of the block is valid.
- **Re-runs.** A block with an invalid set is re-run at the end under the next block number, with
  its order, level and offsets (`verdict main heavy 31 31 --as-block 5`). `--as-block B` exits 2
  when B is itself a re-run: a failed re-run is re-run against its original block
  (`verdict main heavy 32 32 --as-block 5`). In the main phase at most 10 % of n re-runs count
  (n div 10); later ones are ignored. In the pilot at most 3 re-runs per mix count.
- **The sample size is fixed.** `summary` uses the first n valid blocks of each mix by block number
  and refuses to analyse while a mix has fewer and re-runs left.
- **Incomplete.** With the re-runs spent and fewer than n valid blocks, the mix is reported as
  incomplete with its descriptive numbers, and its hypotheses are untested. By the chain, a short
  heavy mix leaves every hypothesis untested or exploratory.
- **A resumed session** skips each (block, arm) already recorded and takes a block's level, order
  and offsets from its number alone.
- **A hard stop**, the controller's session rule rather than code: a session is stopped when the
  Mac has run on battery for 30 minutes or the system volume has less than 10 GiB free. A set on
  battery power is invalid either way.

### Sessions

Sessions run at night, so that the owner's work does not load the machine during a set. The owner's
applications may stay open: each set records the background as totals (memory in use, the 1-minute
load, the process count), and the application set at each verdict command's start is logged as a
count and a SHA-256 of the names; the names stay on the machine. The raw records stay local until
the publication audit, after which they
are committed with the results.

### What each outcome means

A claim is supported only by its hypothesis rejected inside the chain.

- **H1 not rejected:** coreslot is not shown to be free under pressure on this machine. This is the
  run's headline, so everything after it is exploratory.
- **H1, then H4 not rejected:** coreslot beats no admission, but is not shown to be safer than the
  speed-optimal semaphore.
- **H1 and H4, then H5 not rejected:** some fixed N is not shown to be worse than coreslot's
  default at any level; that semaphore is a fair alternative on this mix.
- **H1, H4 and H5, then H3 not rejected:** coreslot is safer and more robust than every fixed N but
  not shown within 3 % of the hindsight-best semaphore's time.
- **H1, H4, H5 and H3, then H2 not rejected or untested:** coreslot holds against the semaphores
  under pressure, but its cost without pressure is not shown to be at most 3 %.
- **All five rejected:** C1-C5 hold on this machine and mix; the makespan is reported as the cost.

### Reproduce

```sh
evidence/overload/setup.sh
evidence/overload/run6.sh                                  # learn, then pilot heavy and light 1 12
python3 evidence/run6.py size evidence/overload/run6       # run6/size.json, n per mix
evidence/overload/run6.sh verdict main heavy 1 30 --primer  # 1 n_heavy, over as many sessions
evidence/overload/run6.sh verdict main light 1 12 --primer  # 1 n_light
evidence/overload/run6.sh verdict main heavy 31 31 --as-block 5  # a re-run of an invalid block 5
evidence/overload/run6.sh verdict sens spread 1 6 --primer
evidence/overload/run6.sh verdict sens cold 1 6 --primer
python3 evidence/run6.py summary evidence/overload/run6
python3 evidence/run6.py figures evidence/overload/run6
```

Re-issuing an interrupted command resumes it. `evidence/overload/run6.sh --dry` runs learn and one
block of each mix on `stand-dry.json`, to check the runner in a few minutes; its output is not
evidence.

### Results (2026-10-23, coreslot `ebac0a7`)

Run 6 is **complete**: 120 heavy and 60 light main blocks, every set valid (1110 sets: 108 pilot,
960 main, that is 840 heavy and 120 light, and 42 sensitivity; plus 2 primer sets), no re-run, no
set coreslot failed, and one coreslot version in every record
(`coreslot 0.1.1-0.20261021174039-ebac0a73fadd`). By the pre-registered chain: **H1 is rejected
and H4 is not, so the chain stops there; H5, H3 and H2 are exploratory.** C1 is supported on this
machine; C4 is not shown, and C5, C3 and C2 support no claim. Every number below is quoted from,
or computed from, the committed records in `run6/` (`summary.json`, printed as `summary.txt`; the
set records `pilot-*.jsonl`, `main-*.jsonl` and `sens-*.jsonl`), or from `holm.json` for the
secondary analysis, which is not pre-registered. The exceptions are the timeline's times of commits
and pull-request comments, which come from git and GitHub.

**Machine and background.** Run 4's stand and host, on AC power in every set. The owner's
applications stayed open: 12 or 13 at each main verdict command's start (the records keep only
the count and a hash). At the start of the 840 heavy main sets, memory in use was 2786-7138 MiB
(median 5179), with the ballast included; the 1-minute load was 2.83-14.45 (median 6.26) and the
process count 361-525 (median 431). At the start of the 120 light main sets, memory in use was
4063-7474 MiB (median 6872), the load 6.1-18.36 (median 10.635) and the process count 455-554
(median 544): the light mix, which ran later, had the busier background.

**Timeline (UTC).** The pre-registration commit is dated 19:11:15 on 2026-10-21 (git) and its anchor
comment on the pull request 19:11:49 (GitHub). The pilot ran from 20:44:35 (its primer set) to
23:56:03 (the last sample of the last light pilot set). `size.json` was committed at 23:56:34 (git).
The main session started at 23:57:15 (its primer set), the first heavy main set at 23:58:59 on
2026-10-21; the heavy main sets ran until 02:25 on 2026-10-23, the light main sets from 02:25:59 to
04:22, the spread sensitivity run from 04:22:37 and the cold run until 05:28:44. The declared
deviation below was posted at 00:11:32 on 2026-10-23 (GitHub), when 763 of the 840 heavy main sets
had started.

**The pilot's sizes** (`run6/size.json`). Heavy: σ̂ 0.1150, raised to σ 0.1442, asks for 150
blocks; the run went on at the cap of 120, so it is **underpowered** against its design. Light:
σ̂ 0.0774, σ 0.0970, asks for 69; the run went on at the cap of 60. No F set of the first 12 light
pilot blocks had a warn-or-worse sample, so the light pressure rule left H2 tested.

**The oracles.** S\* per background level: S2 at 0 and 1536 MiB, S1 at 3072 MiB. K\*cf chose K60
in 60 blocks and K67 in 60. The cross-fitted semaphore Scf chose S2 in 80 blocks and S1 in 40.

**The hypotheses, in the chain's order** (geometric mean ratio, one-sided 95 % bound, BCa 90 %
interval):

- **H1, rejected.** K60/F mean time to verdict 0.4694, upper bound 0.4857, BCa [0.4546, 0.4863],
  paired t p < 10⁻⁴ (`summary.json` rounds it to 0.0). K60's warn seconds were below F's in 120 of
  120 blocks, p = 7.5·10⁻³⁷; its swapout below F's in 101, with no loss and 19 ties, p = 3.9·10⁻³¹.
- **H4, not rejected.** K\*cf against S\*: warn seconds lower in 9 blocks, higher in none (111
  ties), p = 0.00195; swapout lower in 5 and higher in 1 (114 ties), p = 0.109. The chain stops.
- **H5, exploratory.** Each N's six tests at 0.05 / 6:
  - N = 1 passes none. S1/K60 time 1.0577 (lower bound 1.0303, p = 0.048) at 0 MiB, 1.0233 (0.9918,
    p = 0.64) at 1536, 1.0572 (1.0213, p = 0.106) at 3072; neither arm had a warn second in any
    block (every pair tied, p = 1).
  - N = 2 passes: warn lower than S2's in 24 blocks of 24 untied at 3072 MiB (p = 6.0·10⁻⁸) and 9
    of 9 at 1536 (p = 0.00195); time 1.1156 (1.0625, p = 0.0044) at 3072.
  - N = 3 passes all six (time 1.132, 1.3097, 1.489; every p ≤ 0.00025).
- **H3, exploratory.** K\*cf/S\* 0.9784, upper bound 1.0040, BCa [0.9531, 1.0028], p = 0.0006
  against 1.03. Per level: 1.007 (upper 1.0449, flagged) at 0 MiB, 0.970 (1.0308, flagged) at 1536,
  0.959 (0.9916) at 3072.
- **H2, exploratory.** Light K60/F 0.9985, upper bound 1.0123, BCa [0.9856, 1.0125], p = 0.0002
  against 1.03. The bound is below 1.05 and not below 1.01.

**Reported without a test.** Per arm and mix: the mean primary, the median makespan and warn
seconds, the mean critical seconds and swapout with their BCa 90 % intervals (swapout in MiB,
bytes / 2²⁰), and false failures with their exact 95 % interval:

| Mix | Arm | Mean time to verdict (s) | Makespan (s) | Warn+ (s) | Critical (s) | Swapout (MiB) | False failures |
|---|---|---|---|---|---|---|---|
| heavy | F | 74.6 | 85.85 | 51.0 | 3.325 [2.4667, 4.6583] | 11969.1 [10528.9, 13420.2] | 14 / 600 [0.0128, 0.0388] |
| heavy | S1 | 36.0 | 59.75 | 0.0 | 0 | 0 | 3 / 600 [0.001, 0.0145] |
| heavy | S2 | 35.9 | 51.95 | 0.0 | 0.2083 [0.05, 0.575] | 718.7 [435.1, 1166.1] | 8 / 600 [0.0058, 0.0261] |
| heavy | S3 | 45.6 | 55.9 | 6.0 | 0.5083 [0.275, 0.9083] | 2721.0 [1969.8, 3634.6] | 13 / 600 [0.0116, 0.0368] |
| heavy | K60 | 34.3 | 55.7 | 0.0 | 0 | 0 | 0 / 600 [0, 0.0061] |
| heavy | K67 | 34.3 | 57.8 | 0.0 | 0 | 1.2 [0.2, 4.9] | 0 / 600 [0, 0.0061] |
| heavy | KH | 35.8 | 57.3 | 0.0 | 0 | 0.1 [0.0, 0.2] | 2 / 600 [0.0004, 0.012] |
| light | K60 | 28.7 | 32.15 | 0.0 | 0 | 0 | 0 / 180 [0, 0.0203] |
| light | F | 28.8 | 33.65 | 0.0 | 0 | 0 | 2 / 180 [0.0013, 0.0396] |

The median swapout was 0 in every arm but heavy F (11994.3 MiB). Every false failure was the
FastAPI suite's: F 14 of 120 [0.0653, 0.188], S3 13 [0.059, 0.1781], S2 8 [0.0292, 0.1271], S1 3
[0.0052, 0.0713], KH 2 [0.002, 0.0589], light F 2 of 60 [0.0041, 0.1153]; every other gate failed
in no set. The swapins, pageins and each gate's slowdown are in `summary.json`.

Ratios (geometric mean, one-sided 95 % upper bound, BCa 90 %), heavy unless noted:

| Ratio | GMR | Upper | BCa 90 % |
|---|---|---|---|
| Makespan K60/F, heavy | 0.6725 | 0.6959 | [0.6508, 0.6965] |
| Makespan K60/F, light | 0.9616 | 0.9792 | [0.9459, 0.9800] |
| Makespan K\*cf/S\* | 1.0278 | 1.0568 | [0.9995, 1.0552] |
| K60/S\* | 0.9806 | 1.0078 | [0.9540, 1.0070] |
| KH/K60 | 1.0356 | 1.0529 | [1.0189, 1.0528] |
| K\*/S\* | 0.9740 | 0.9997 | [0.9487, 0.9982] |
| K\*cf/Scf | 0.9784 | 1.0040 | [0.9531, 1.0028] |

K\*cf/Scf equals H3's K\*cf/S\* because Scf chose, in every block, the arm S\* chose for its level.

Each fixed semaphore's regret against S\*, mean time to verdict (GMR, BCa 90 %), 40 blocks per level:

| Level | S1 | S2 | S3 | F |
|---|---|---|---|---|
| 0 MiB | 1.065 [1.0291, 1.1016] | 1 (S\*) | 1.1399 [1.0989, 1.1971] | 1.729 [1.6631, 1.801] |
| 1536 MiB | 1.013 [0.9445, 1.0664] | 1 (S\*) | 1.2965 [1.2297, 1.3816] | 2.2017 [2.0866, 2.298] |
| 3072 MiB | 1 (S\*) | 1.0553 [1.0134, 1.1179] | 1.4085 [1.3102, 1.5238] | 2.3946 [2.3051, 2.4908] |

**Sensitivity** (6 valid blocks each). Spread arrivals, mean time to verdict: F 43.1 s, S1 30.8,
S2 26.7, S3 34.5, K60 28.7; over K60, F 1.4532 [1.2004, 1.7144], S1 1.0719 [0.9895, 1.1683], S2
0.9308 [0.8451, 1.0766], S3 1.1913 [1.0424, 1.4227]. Cold start: KC/K60 1.1574 [1.0763, 1.3374].

**Order effects** (heavy; mean log primary over the block's geometric mean, BCa 90 %). Positions
count from 0, a block's first set, to 6, its last, as in the records. By position, every interval
holds 0 except position 4 (the fifth set), −0.0449 [−0.0800, −0.0025], and position 6 (the
seventh), 0.0682 [0.0195, 0.1236]. By the arm before, after F −0.1412 [−0.1618, −0.1179] and after
S1 0.0915 [0.0368, 0.1522]; the others hold 0. A set after F is never F, the slowest arm.
Inference: that pulls the after-F row down by construction.

**Covariates**, from the set records: medians at the sets' start, and the thermal samples during
the sets that read state 1 ("fair"; none read higher; every set started at nominal).

| Mix, level | Memory in use (MiB), F / S1 / S2 / S3 / K60 / K67 / KH | 1-minute load, same order | Thermal "fair", same order |
|---|---|---|---|
| heavy, 0 | 4477 / 4298.5 / 4407.5 / 4474.5 / 4335 / 4370.5 / 4335.5 | 5.90 / 7.05 / 6.52 / 6.31 / 6.94 / 6.79 / 6.59 | 243/515, 144/471, 122/400, 124/405, 139/441, 120/449, 124/460 |
| heavy, 1536 | 5157 / 5182.5 / 5240 / 5219.5 / 5168.5 / 5284 / 5290.5 | 5.48 / 6.98 / 6.92 / 5.90 / 7.04 / 6.83 / 6.11 | 383/651, 164/478, 143/427, 194/467, 128/448, 128/448, 140/492 |
| heavy, 3072 | 6003 / 5985.5 / 5958.5 / 5809 / 6094.5 / 6165.5 / 5995.5 | 6.11 / 5.81 / 5.92 / 5.57 / 6.03 / 5.83 / 5.66 | 494/752, 166/496, 192/463, 264/532, 144/465, 173/474, 169/482 |
| light, K60 / F | 6872 / 6868.5 | 10.74 / 10.58 | 14/408, 12/420 |

839 of the 840 heavy main sets and all 120 light ones are flagged `high_load`; none is flagged
`hot_start`.

**The ballast.** Its resident size, read before each set, was below 90 % of its level in 80 of the
560 ballasted heavy sets (74 at 3072 MiB, 6 at 1536), in 38 blocks, never in a block's first set,
and down to 7 MiB.

### Deviations

**A declared secondary analysis, not pre-registered.** While the heavy main sets ran, the owner
posted on the pull request a secondary analysis of the same five hypotheses: Holm's step-down at
family-wise α = 0.05 in place of the fixed chain, on the same records, statistics and oracles,
each hypothesis entering with one p-value (H1 the largest of its three, H4 the larger of its two;
H5, for each N, six times its smallest p capped at 1, then the largest over N; H3 and H2 their
single p; H2 left out when the light pressure rule voids it). The primary analysis is unchanged
and comes first. Its disclosure, quoted:

> This was decided on 2026-10-23 after a descriptive look at the valid heavy main blocks 1-106
> (per-arm means, ratios and win counts, computed outside `run6.py`, no test run), prompted by a
> question during the run. That look showed H4's swap comparison unlikely to reach p < 0.05. The
> light mix, the sensitivity runs and every final test had not been computed. Nothing else
> changes: not n, the stopping, the oracles, the metrics, the records or the code.

`evidence/holm6.py` reads `summary.json` only and writes `run6/holm.json` and `run6/holm.txt`.
Secondary analysis, not pre-registered (declared deviation), over k = 5:

| Rank | Hypothesis | p | Holm threshold | Holm decision | Primary chain |
|---|---|---|---|---|---|
| 1 | H1 | 3.9·10⁻³¹ | 0.01 | rejected | rejected |
| 2 | H2 | 0.0002 | 0.0125 | rejected | exploratory |
| 3 | H3 | 0.0006 | 0.0167 | rejected | exploratory |
| 4 | H4 | 0.1094 | 0.025 | not rejected | not rejected |
| 5 | H5 | 0.2879 | 0.05 | not rejected | exploratory |

**Where the two differ.** The primary analysis rejects H1 only and leaves H5, H3 and H2 exploratory.
The secondary analysis, not pre-registered, also rejects H3 and H2. It agrees that H4 is not
rejected, and it does not reject H5: N = 1's smallest p, 0.048, times six is 0.288. The H3 and H2
p-values are `summary.json`'s, rounded to four decimals; no Holm decision is near its threshold. The
commits that add it say "declared secondary analysis" in their subjects; it is the same analysis,
not pre-registered.

**The sessions ran through the day, not at night.** The pre-registration says sessions run at night
(see "Sessions"). On 2026-10-21 at 22:44 local time (20:44 UTC), before the pilot's first set, the
owner asked in the controller's session to start at once rather than wait for a night window. The
pilot ran from 20:44 to 23:56 UTC on 2026-10-21, and the main phase ran as one session from 23:58
UTC on 2026-10-21 to 05:28 UTC on 2026-10-23, about 29.5 hours, through the owner's working day:
the heavy main sets started in every UTC hour of 2026-10-22, 30 to 34 in each (`started` in
`main-heavy.jsonl`). The owner's interactive use is part of the logged background (the memory in
use, the load, the application count). No validity rule depends on the time of day, so every set
stays valid. This deviation was not posted on the pull request before the run.

### Figures

- [`f1-timeline.svg`](overload/run6/f1-timeline.svg): the gates of the block whose ln K\*cf/S\* is
  nearest the median.
- [`f3-paired.svg`](overload/run6/f3-paired.svg): per block, ln K\*cf/S\* and ln K60/F.
- [`f4-pareto-0.svg`](overload/run6/f4-pareto-0.svg),
  [`f4-pareto-1536.svg`](overload/run6/f4-pareto-1536.svg),
  [`f4-pareto-3072.svg`](overload/run6/f4-pareto-3072.svg): per level, each arm's mean time to
  verdict against its warn seconds.
- [`f5-regret.svg`](overload/run6/f5-regret.svg): each fixed arm's regret against S\* per level.
- [`f6-ecdf.svg`](overload/run6/f6-ecdf.svg): time to verdict per agent, pooled over the heavy
  blocks.
- [`f7-light.svg`](overload/run6/f7-light.svg): light K60/F per block.

### What it shows

**The pre-registered verdict.** On this laptop and the memory-heavy mix, coreslot at its default
share with needs it learned itself (K60) is free under pressure against running everything at
once: its mean time to verdict is 0.47 of F's, and it had fewer warn seconds in every block and
less swap in every block that differed (C1). It is not shown to be safer than the speed-optimal
semaphore chosen in hindsight (C4): it had fewer warn seconds in 9 blocks and none more, but
the two differed in swap in only 6 of 120 blocks (K\*cf lower in 5, higher in 1), p = 0.109. The chain stops there, so robustness (C5), the
time margin against the best semaphore (C3) and the cost without pressure (C2) support no claim.

**The secondary analysis** (not pre-registered, declared after a look at blocks 1-106, see
Deviations) would also reject H3 and H2. Read with that label, inferences:

- *Against the best semaphore, time.* K\*cf was within 3 % of S\* (H3's bound 1.004), but the
  per-level bounds exceed 1.03 at 0 and 1536 MiB, so the margin is not shown at every level.
- *Without pressure.* K60 matched F on the light mix (H2's ratio 0.9985, bound 1.0123): within
  3 % and 5 %, not shown within 1 %.
- *Robustness.* S1 is a fair alternative on this mix: no warn second in any heavy set, no swap, and
  its time not shown more than 3 % above K60's at any level at the declared α / 6 = 0.0083. At an
  unadjusted 0.05 it would be shown at 0 MiB: S1/K60 1.0577, lower bound 1.0303, p = 0.048. S2 and
  S3 lose to K60 on warn seconds or time at some level.

Measured, outside the tests: K60 had no false failure in 780 runs of a counted gate, where every
semaphore and F failed the FastAPI suite in 3 to 14 of 120 heavy sets; and coreslot's makespan
was 0.67 of F's on the heavy mix but 1.03 of the best semaphore's (BCa [0.9995, 1.0552]), the
stated cost.

### Threats to validity

- **Underpowered.** The pilot asked for 150 heavy blocks and 69 light ones; the run had 120 and 60.
- **The oracle.** S\* is chosen in hindsight on the tested blocks, which favours the semaphore; at
  this stand it kept pressure near zero, so H4 had few differing blocks to count.
- **The deviation.** The secondary analysis, not pre-registered, was declared after a descriptive
  look at the valid heavy main blocks 1-106: per-arm means, ratios and win counts, H4's swap
  comparison among them. H3's ratio is built from those per-arm means, so its secondary rejection
  carries that caveat, and so does H2's as a rejection of the same declared family, although the
  light mix was not looked at.
- **The ballast.** In 80 of 560 ballasted sets the ballast's resident size read below 90 % of its
  level before the set, so those sets ran with less background than their level names. Inference:
  the sets before them pushed part of it out of memory (never in a block's first set; 22 of the 80
  followed F).
- **The background.** The owner's applications stayed open; 839 of 840 heavy and all light sets
  started with a 1-minute load above 3, and the light mix ran under a median load of 10.6 against
  the heavy mix's 6.3. Thermal state reached "fair" in up to 66 % of the samples of a cell that
  carry a thermal reading, which is every fifth sample (heavy F at 3072 MiB: 494 of 752; 494 of all
  its 3671 samples, 13.5 %), and never higher. The sessions ran through the owner's working day,
  not at night (see Deviations).
- **Order.** Positions 4 and 6 (0-based: the fifth and seventh sets) and the set after S1 show
  order effects whose intervals exclude 0. Each arm's position is shuffled per block. Inference:
  they add noise rather than favour an arm.
- **Rounding.** `summary.json` rounds the t tests' p to four decimals; the Holm column of the
  secondary analysis, not pre-registered, uses those.
- **One machine, one build, one main session of about 29.5 hours:** the measured coreslot is the
  head of an open pull request, not a release.

### What it does not show

Linux, or a machine with more memory. Other mixes, or more agents than four. That coreslot is
safer than the best semaphore chosen in hindsight, or that any fixed N is worse than coreslot at
some level, by the pre-registered analysis. A semaphore tuned without hindsight beyond the
cross-fitted Scf.

### Reproduce

```sh
python3 evidence/run6.py summary evidence/overload/run6   # summary.json and summary.txt from the records
python3 evidence/run6.py figures evidence/overload/run6   # the figures
python3 evidence/holm6.py evidence/overload/run6          # holm.json, holm.txt: not pre-registered
```

The records were taken with the commands of the pre-registration's "Reproduce", with the main
heavy 1 120 and light 1 60. The measured binary was built at `ebac0a7` as stated under "Stand".

## Acceptance run 7: no fixed slot count keeps up across load (`run7.py`)

**Status: pre-registered.** This section, the runner and the frozen inputs (the needs file and the
learn records, with their SHA-256) were committed and pushed before the first pilot set, and
anchored as described under "The anchor". The results are appended below, whatever they are.
The analysis is `run7.py pilot-check` and `run7.py summary`. The code that defines the records,
the metrics and the analysis is frozen at the pre-registration commit: `run7.py`, `stats7.py`,
`overload/run7.sh`, `overload/stand7.json`, and the run-4/6 helpers it imports (`run4.py`,
`run6.py`, `stats6.py`, `ballast.py`, `overload.py`, `acceptance.py`); none of them is changed
after it.

**Question.** On one 16 GiB laptop, is there a slot count a user can set once for parallel test
gates that keeps up with coreslot both when the machine is quiet and when it is loaded?

### The claim, as tested

**P1: no fixed slot count keeps up across load.** A counting semaphore is tuned once, on prior
data, at the condition where it does best, and frozen:

- `sem -j2` (**S2**), the best slot count on a quiet machine in run 6, loses to coreslot under
  load, on time (**T1**) and on swap traffic (**T2**);
- a machine-wide lock of one slot (**S1**), the safe choice under load, loses to coreslot on a
  quiet machine (**T3**).

P1 is rejected only when all three are. Per level, S2 can be marginally faster than coreslot when
the machine is quiet; those cells are shown descriptively and are not part of the claim.

**The secondary family**, tested only if P1 is rejected:

- **H-P:** coreslot is at most 3 % slower than GNU `parallel`'s `sem --memsuspend` under load;
- **H-L:** coreslot costs at most 3 % on a mix without memory pressure;
- **H-Z:** coreslot's sets swap out more than 64 MiB rarely (below 3 %);
- **H-W:** S2 serialises a wide mix that fits in memory, so it is slower than coreslot there too.

**Stated cost, no claim.** The makespan, reported with its interval, as in run 6.

### Stand

`evidence/overload/stand7.json`: run 4's stand, with one change. FastAPI's suite runs with
`--dist loadgroup`, as its own `scripts/test.sh` runs it. FastAPI marks the tests that write to a
shared working directory with an `xdist_group`, and the mark has effect only under that flag;
without it those tests race. All 50 false failures of run 6 were FastAPI's, in three files that
write to that directory; that the flag also removes them under contention is an inference the
pilot must show.

- **The heavy mix:** run 4's four agents. A1 and A2 each run the VS Code type check; A3 runs
  FastAPI's ruff, then its pytest suite; A4 runs SQLAlchemy's pytest `test/orm`.
- **The light mix:** agents A3 and A4, no type check.
- **The wide mix:** four agents whose suites fit in memory together, one heavy gate each, at the
  stand's pinned commits. W1 runs SQLAlchemy's `test/orm`, W2 its `test/sql test/dialect`, W3
  its `test/engine test/ext`, W4 FastAPI's suite. W2 and W3 run in their own checkouts at the
  same commit (`setup.sh` makes them), so no two gates running at once write into one tree.
  Their peak footprints alone sum to about 56 of the 76 tokens of mem `"60%"`.

coreslot is the release `v0.2.1`, installed from its archive, not built locally: the archive
`coreslot_0.2.1_darwin_arm64.tar.gz` matches the release's `checksums.txt`
(`4235018e0de9b8712831ec93aeb1cde3c079d015a27808d5a825d92fd6464803`) and its build-provenance
attestation verifies; `coreslot version` prints `coreslot 0.2.1 (protocol formats: 1)`. The
binary's SHA-256, `82f7181c5b82163a53c9a4d89630d4262fe53d225dfcfef52452cabaa3aed575`, is pinned in
`stand7.json` as `coreslot_sha256`: a pilot or main command refuses to start when the stand's
binary has another (for instance after `setup.sh`, which builds coreslot into the same path).
Every set records the binary's SHA-256. Every coreslot call goes through the stand's own state
directory; the owner's budget and configuration are never touched.

GNU parallel is 20260922 from Homebrew. It runs with `PARALLEL_HOME` under the stand's directory,
so the runner never touches the user's `~/.parallel`.

### Arms

- **K60:** every gate as `coreslot run --label bench:<gate> -- <argv>`, cpu `"140%"`, mem `"60%"`,
  and the learned `run7/needs.toml`. The live memory rule is on, the product default. `{n}` is
  the cpu coreslot grants the gate.
- **S1, S2:** an in-process counting semaphore of 1 or 2 slots around the heavy gates, with the
  lint free; `{n}` is `auto`. This is more favourable to S1 than a `flock(1)` around every
  gate, which would serialise the lint too.
- **P:** each heavy gate as `sem --will-cite --id <set> --memsuspend 9G -j 4 --fg <argv>`, with
  `{n}` as `auto`. That is a separate `sem` per agent, sharing one semaphore id per set. Each `sem`
  suspends only its own job, and starts it only when free plus inactive memory is at least 9 GiB.
  At `-j 4` the slot count never binds on four agents, so P's only admission is memory. P is what
  a user who types `sem --memsuspend` gets. In the exploratory probe (below) its gates started
  and were stopped and continued at quiet, and never started at loaded.
- **F:** no admission; every gate starts when its agent reaches it; `{n}` is `auto`.

Which arms run where:

| Phase | Mix | Arms | Arrivals | Level |
|---|---|---|---|---|
| pilot, main | heavy | K60, S1, S2, P | U(0, 5 s) | quiet or loaded |
| pilot, main | light | K60, F | U(0, 5 s) | quiet |
| pilot, main | wide | K60, S2, F | U(0, 5 s) | quiet |
| sensitivity | spread | K60, S2 | U(0, 30 s) | quiet |

With 128 MiB tokens, mem `"60%"` is 76 tokens and cpu `"140%"` 14 tokens. Each K60 set records the
sizes `coreslot status --json` reports and the needs file's SHA-256.

### Background levels, on coreslot's headroom

The levels are defined on coreslot's own headroom metric (free + speculative + purgeable +
file-backed pages), not on memory in use: after the gates run, "in use" counts gigabytes of file
cache the kernel reclaims at once. On this Mac the headroom swings with the system's state, from
about 6 to 11 GiB over a day and between 2 and 10 GiB within a set, so each set first brings it
to its level:

- **Quiet:** no ballast. The set waits, up to 120 s, for a headroom of at least
  `QUIET_HEADROOM_MIB` (9000 MiB), and starts flagged `quiet_short` if it is still below. With
  the light application set the headroom returns to 9.1-9.6 GiB within about 30 s of a set's
  end and rarely passes 10 GiB, so the floor is set where the quiet wait ends (see "The
  exploratory probe" below). At that level the live size pairs the type check with a suite only
  when the headroom happens to allow it, so no hypothesis rests on pairing; T3 rests on run 6's
  effect without it, and the pairing count is reported.
- **Loaded:** a ballast of clamp(headroom − `LOADED_HEADROOM_MIB`, 0, 6144) MiB, so the headroom
  comes down to `LOADED_HEADROOM_MIB` (8000 MiB), where run 6's S2 pressured. The ballast is
  `ballast.py`'s, filled from `os.urandom`, touched once a second, and exits with the runner. The
  set starts when it is 95 % resident, waiting up to 60 s, and is flagged `ballast_short` otherwise.
- **At t0**, after the level is reached, the set records the headroom, and flags `level_off`
  when it is off its level: for quiet, below the floor; for loaded, more than 512 MiB from the
  target in either direction.

Flags never invalidate a set; their counts are reported, and a reader can see each one per set.

**The exploratory probe** (2026-10-24, not evidence): with the light application set, the v0.2.1
release and these needs, one heavy block at each level with all four arms and one wide block, in
a separate output directory that enters no analysis.

- **Levels:** the loaded sets held a ballast of 0.7-1.5 GiB and started at a headroom of
  7.84-8.04 GiB; the quiet sets started at 9.11-9.48 GiB, every one after the full 120 s wait for
  the former floor of 10500 MiB. That floor would have added about 2 min to each of some 500
  quiet sets, so it was lowered to 9000 MiB before any pilot set.
- **Pairing:** both loaded K60 sets held the type check and FastAPI's suite at once (86 tokens of
  mem grants against the static 76); the quiet K60 set did not.
- **P:** at quiet, P's set ran in 32.9 s, and `sem` stopped each type check once, for about a
  second, while the headroom was at 4.7 and 2.2 GiB. At loaded, no `sem` started its gate in
  600 s: the set timed out. Whether P is feasible is left to the pilot's rule below.
- **Set times,** start to start: about 75 s per loaded heavy set, about 1.5 min per wide set
  besides the quiet wait.

**The order of work in a set:** the cooldown (run 4's pacing: at least 20 s, then until the
thermal state is nominal and a single-core probe is within 10 % of the session's baseline, at most
120 s), then the level, then t0 and the agents. A set whose cooldown gave up is flagged
`hot_start`. Over the cooldown the runner sums the CPU time of processes outside the stand with
`ps`, and flags `busy_background` above 1.5 cores; it replaces run 6's `high_load`, which mostly
measured the stand's own previous set.

**The level schedule.** Blocks 1-2, 3-4, … each hold both levels, in an order shuffled with the
seed `run7-level-<phase>-<(block - 1) div 2>`. The light, wide and spread mixes run at quiet only.

**The order schedule.** Block b runs its arms in an order shuffled with the seed
`run7-order-<phase>-<mix>-<b>`; each set records its position and the arm before it.

**Arrivals.** Offsets from U(0, 5 s), U(0, 30 s) in the spread run, with the seed
`run7-<phase>-<mix>-<b>`, the same for every set of the block.

**A set timeout.** A set still running after 600 s is stopped: no further gate starts, every live
gate of any arm, each P `sem` with its job included, is killed down to its process groups
(SIGTERM, then SIGKILL after 5 s; never the runner's own group), and the set is invalid with the
reason `set_timeout`. Each set removes its own semaphore directory under `PARALLEL_HOME` when it
ends.

**The primer.** Each session's first command starts with `--primer`: one K60 set of its first
block, recorded to `run7/primer.jsonl` only.

### Learning, frozen before the pilot

`run7.py learn` starts from an empty state directory with cpu `"140%"` and mem `"60%"`, and runs
every gate of the heavy and wide mixes alone, each run after a cooldown of at least 20 s:

1. **6 runs in its K form** (under coreslot) and **3 in its F form** (its argv with `{n}` as
   `auto`), as in run 6.
2. `coreslot suggest --write`, kept as `run7/needs-pass1.toml`.
3. **The relearn pass:** 5 more K-form runs per gate under that entry.
4. `suggest --write` again, copied to `run7/needs.toml`.

**Why relearn.** `suggest` takes each label's largest peak over its last 5 runs. In pass 1 the
first K run of a suite runs cold, with the whole default range of `{n}`, and later ones at the
width the first entry grants; a peak taken at a wider grant than the gate will get inflates its
mem need, and the inflated need is what keeps the live size from pairing a suite with the type
check. After the relearn pass the window holds only runs at the granted width. A user reaches the
same state in ordinary use: the run log rolls over to runs at the granted width after a few runs
of each gate, and `suggest`'s evidence line names a peak taken at a larger cpu grant.

Every gate passed all of its 6 K-form, 3 F-form and 5 relearn runs. The second `suggest --write`
wrote (`run7/needs.toml`, 2026-10-24, on the v0.2.1 release):

| Gate | cpu | mem | tokens | Evidence line it wrote |
|---|---|---|---|---|
| `vscode-typecheck` | `6` | `8960M` | 70 | 5 runs 2026-10-24: max 5.4 cores, 5 of 5 saturated, peak 8.7 GiB (footprint) at 8s |
| `fastapi-ruff` | `3` | `128M` | 1 | 5 runs 2026-10-24: max 2.0 cores, 3 of 5 saturated, peak 21 MiB (max_rss) |
| `fastapi-tests` | `1..7` | `2G` | 16 | 5 runs 2026-10-24: max 6.6 cores, 5 of 5 saturated, peak 2 GiB (footprint) at 12s with cpu 8 |
| `sqlalchemy-orm-tests` | `1..9` | `2G` | 16 | 5 runs 2026-10-24: max 8.7 cores, 5 of 5 saturated, peak 2 GiB (footprint) at 18s |
| `sqlalchemy-sql-tests` | `1..4` | `1536M` | 12 | 5 runs 2026-10-24: max 3.8 cores, 0 of 5 saturated, peak 1.5 GiB (footprint) at 13s with cpu 6 |
| `sqlalchemy-engine-tests` | `1` | `640M` | 5 | 5 runs 2026-10-24: max 0.6 cores, 0 of 5 saturated, peak 523 MiB (footprint) at 27s with cpu 3 |

The relearn pass lowered every suite's mem against the first pass (`run7/needs-pass1.toml`):
FastAPI's from 2560M to 2G, the ORM suite's from 2304M to 2G, `test/sql test/dialect`'s from
2432M to 1536M and `test/engine test/ext`'s from 1280M to 640M. It also narrowed the cpu of
`test/engine test/ext` from `1..3` to `1`: at a grant of 3 its K runs used 0.6 cores and took
about 30 s, against about 17 s at the full width of the first pass. That is the product's own
learned state, kept as it is.

**Expected before the data.** Under these needs, mem `"60%"` leaves 6 tokens beside a type check,
so the static size admits only the lint and `test/engine test/ext` beside it; a suite pairs with
it only through the live size, when the headroom allows. In the light mix the two suites need
32 tokens together and run at once. The wide mix needs 49 tokens, so K60 admits all four agents
at once and shares the 14 cpu tokens among them.

**The frozen files.** `run7/needs.sha256` holds the SHA-256 of the needs file and of the learn
records:

```
b74f7f1699e7b01c7d3531ba39fdda9a01b33e18bcff23a1d49526f8a780b2a8  needs.toml
40b6a43f3176ac9d20674817c731d9094b47f1bd7c2bfa543d37bfecab91c05e  learn.jsonl
```

Every verdict command exits before its first set unless they match. `learn`
refuses once any pilot, main or sensitivity record exists without them; once they exist, `learn`
does nothing (no run, no `suggest`, no reseal). An interrupted `learn` resumes inside either pass
and never re-runs the first `suggest` once the relearn pass has started.

### Metrics

- **Primary: the mean time to verdict.** Each agent's chain end minus its arrival, averaged over
  the mix's agents.
- **Swap traffic:** the swapout plus swapin pages over the set. It is continuous where warn
  seconds are tie-heavy.
- **Pressure:** warn seconds (`kern.memorystatus_vm_pressure_level` ≥ 2) and critical seconds
  (≥ 4).
- **Makespan and false failures,** as in run 6.
- **Slowdown:** a gate's own execution over its alone reference, K's the median `ran_s` of its 5
  relearn runs (at the granted width), F's and S's the median wall time of its 3 F-form runs.

### The pilot: a futility check, not a size rule

The pilot runs 12 heavy blocks (6 per level), 12 light and 6 wide, with the main phase's rules;
it enters no hypothesis test. `run7.py pilot-check` reads the first 12 valid heavy blocks (valid
over K60, S1 and S2), the first 12 light and the first 6 wide, at most 3 re-runs each, and writes
`run7/pilot_check.json`:

- **T1 and T3:** the pilot's mean and SD of the block's ln-ratio, and the power a one-sided paired
  t test would have at n = 72 with them (normal approximation).
- **T2:** the pilot's wins and losses of K60 against S2 on swap traffic, and the power of an
  exact one-sided sign test over round(72 × the share of untied pairs) pairs at the pilot's win
  rate.
- **Futile:** a component whose mean is at most 0, whose wins are at most its losses, or whose
  power at n = 72 is below 0.5. A pilot with no untied T2 pair is futile on T2.
- **P's feasibility:** P is infeasible when more than 2 of the 12 pilot heavy P sets timed out.
  An infeasible P is dropped from the main heavy blocks and reported as infeasible on macOS, not
  tuned, and H-P is untested.
- **The light mix's pressure count:** the light pilot's F sets with a warn-or-worse sample.
- **Go** only when no component is futile. A "go" of false stops the run; the owner decides what
  follows, and that decision is recorded as a deviation.

`verdict main` refuses without a `pilot_check.json` whose "go" is true, and every main record
carries that file's SHA-256. `pilot-check` refuses while the pilot is short and once any main
record exists.

**Why n is fixed.** n = 72 blocks per level is fixed now, before the pilot, rather than taken from
it. The pilot can only stop the run, never resize it, so the sample size depends on no look at the
data. At run 6's effects blunted by 0.85 standard errors (T1: mean 0.084, SD 0.183; T3: mean
0.043, SD 0.098), n = 72 gives T1 a power of 0.988 and T3 0.981. T2 with 43 untied pairs of 72
(run 6's discordance, about 0.6) has a power of 0.993 at a win rate of 0.8 and 0.949 at 0.75. The
sign test's power is not monotone in the number of pairs, so it is stated at these counts only.

### Hypotheses, declared before the run

The main phase runs 144 heavy blocks (72 per level), 40 light and 30 wide. `run7.py summary` reads
only the main records, never the pilot's. Every test is one-sided at α = 0.05, on the first n
valid blocks of each mix and level.

**P1, an intersection-union test** (Berger 1982), so no α is split:

- **T1:** on loaded blocks, the mean of ln(S2/K60) is above 0 (paired t).
- **T2:** on loaded blocks, K60's swap traffic is below S2's (Wilcoxon signed-rank on K60 − S2,
  zeros dropped, average ranks for ties; exact for up to 25 nonzero pairs, else the normal
  approximation with the tie correction and a continuity correction of 0.5).
- **T3:** on quiet blocks, the mean of ln(S1/K60) is above 0 (paired t).

P1 is rejected when all three have p < 0.05. A heavy block counts when K60, S1 and S2 are valid;
an invalid P set never drops it.

**The secondary family**, tested only if P1 is rejected, by Holm at 0.05. A member that is not
tested counts in Holm's m with p = 1 and is never rejected:

- **H-P:** on loaded blocks where P is valid too, the mean of ln(K60/P) is below ln(1.03) (paired
  t): time non-inferiority only. Swap traffic and pressure against P are reported, not tested.
  Untested when P is infeasible.
- **H-L:** on light blocks, the mean of ln(K60/F) is below ln(1.03) (paired t). Untested when more
  than one light pilot F set had a warn-or-worse sample: the claim is about a mix without
  pressure.
- **H-Z:** among K60's heavy sets, k swapped out more than 64 MiB; the exact binomial
  P(X ≤ k | n, 0.03) is below 0.05. Over 144 sets, 0 events rejects (p = 0.012) and 1 does not
  (p = 0.068).
- **H-W:** on wide blocks, the mean of ln(S2/K60) is above 0 (paired t).

If P1 is not rejected, each secondary row reads "not tested (P1 not rejected)"; if P1 is untested
(a short heavy mix), "not tested (P1 untested)".

**Reported without a test:**

- per arm, level and mix: the mean primary, the median makespan, warn and critical seconds, swap
  traffic, and false failures with exact 95 % intervals;
- K60 against P, on loaded blocks where P is valid: a Wilcoxon signed-rank test of K60 − P swap
  traffic (below 0) and a sign test of K60 against P on warn seconds (ties dropped), each with
  its p, outside the Holm family;
- K60/S\* per level, S\* the best of S1 and S2 in hindsight, labelled as such;
- the spread run, the first 12 valid blocks;
- the pairing count: K60 sets in which the mem grants of overlapping runs summed above the static
  size, from each set's run-log lines;
- the count of every flag, and the invalid sets by reason;
- order effects (position and the arm before);
- the primary per hour of the session, from each set's start.

### Validity, re-runs and an incomplete run

- **A valid set,** as in run 6, and also not timed out. Flags never invalidate.
- **Re-runs.** A block with an invalid set is re-run at the end under the next block number with
  its level, order and offsets (`--as-block`); at most 10 % of n count (14 heavy, 4 light,
  3 wide, also under the declared drop), 3 per mix in the pilot. A heavy block whose only invalid
  set is P's is not re-run (`--as-block` refuses it): its P1 sets stand, and H-P uses the blocks
  where P is valid. `summary` refuses while a mix is short and re-runs are left;
  with them spent, the mix is reported as incomplete and its tests are untested.
- **Provenance.** Every record carries the binary's SHA-256, the evidence tree's commit, whether
  that tree had uncommitted changes, the first line of `parallel --version` and, for main
  records, the SHA-256 of `pilot_check.json`. A pilot or main command on the run's stand refuses
  to start on a tree with uncommitted changes under `evidence/` (its own output directory aside);
  a dry stand only records the state.

### Sessions and the time budget

The run starts with a light, fixed set of applications: Finder and Claude, with everything heavy
closed. It is launched detached (`nohup`), so the session that starts it can close; the owner does
not use the Mac during the run. The application set at each verdict command's start is logged as
a count and a SHA-256 of the names; the names stay on the machine.

At the probe's set times (about 1.4 min per heavy set, 1.1 per light, 1.7 per wide, and 10 min
for a P set that times out), the budget is:

| Part | Hours |
|---|---|
| Learn, with relearn and cooldowns (done) | 0.85 |
| Pilot: 12 heavy, 12 light, 6 wide blocks, with up to 6 P timeouts | ≈ 3 |
| Heavy: 144 blocks × 3 sets, or × 4 if P is feasible | ≈ 10-14 |
| Light: 40 × 2 | ≈ 1.5 |
| Wide: 30 × 3 | ≈ 2.6 |
| Spread: 12 × 2 | ≈ 0.6 |

About 18-22 h, and at most about 27-30 h at run 6's slower set times. **If the run passes 32 h,**
the spread run is dropped first, then the wide blocks above the first 24, declared to the analysis
with `run7.sh summary --dropped` (recorded as `"dropped": true`). P1's arms and n are never cut.

**No analysis mid-run.** Nobody runs `summary`, or reads a main record's primary, until the main
phase is complete. This is a convention the analyst keeps, stated here, not a control the code
enforces.

### What each outcome means

These are decided now, before the data:

1. **P1 not rejected, with T1's geometric mean ratio at most 1.03 or T2's wins at most its
   losses:** a semaphore tuned once is as good as coreslot on this machine, even under load. The
   one-machine product is dropped, or narrowed to a documented recipe for `sem` and `flock`.
2. **P1 not rejected only through T3** (S1 within noise of K60 at quiet): a machine-wide lock is a
   fair alternative. coreslot's case narrows to "faster than a lock when the machine is quiet and
   safer than `sem -j2` when it is not", and that narrow claim is what is published. Development
   continues only if the wide mix, reported descriptively, shows the same frozen S2 losing too.
3. **P1 rejected; H-P not rejected, or K60 not shown below P on both reported safety rows**
   (the swap-traffic signed-rank test and the warn-seconds sign test, each at p < 0.05):
   `sem --memsuspend` already does much of the job. Development continues only for what P
   lacks (learned needs, cross-agent and cross-tool admission), and that is said publicly.
4. **H-L not rejected or its upper bound above 1.03:** coreslot taxes the common case. It is fixed
   before any publication, whatever P1 says.
5. **P1 and H-P rejected, with both reported safety rows against P at p < 0.05:** development
   continues. The headline is "no tuning: keeps up with `sem -j2` and a lock across load, as
   fast as `sem --memsuspend`, and free when idle", within the rows that were rejected.

If P is infeasible, outcomes 3 and 5 read "P is infeasible on macOS" in place of H-P.

### The anchor

Before the first pilot set:

- the pre-registration commit, which holds this section with its values filled;
- a tag on that commit, `run7-preregistration`;
- an OSF registration made from that commit's text;
- a comment on the pull request carrying the commit's SHA, this section's SHA-256 and the OSF
  registration's URL.

The registration's URL is added to this section afterwards, in a commit that changes nothing
else.

- OSF registration: https://osf.io/px5mr
- The comment on the pull request:
  https://github.com/core-slot/coreslot/pull/28#issuecomment-6379397725

### Reproduce

```sh
evidence/overload/setup.sh
# then install the v0.2.1 release binary into ~/bench.noindex/bin/coreslot (setup.sh builds one
# there, which the pin refuses)
nohup evidence/overload/run7.sh > ~/bench.noindex/run7.log 2>&1 &  # learn, then the pilot
evidence/overload/run7.sh pilot-check                              # run7/pilot_check.json
evidence/overload/run7.sh verdict main heavy 1 144 --primer        # only after "go": true
evidence/overload/run7.sh verdict main light 1 40 --primer
evidence/overload/run7.sh verdict main wide 1 30 --primer
evidence/overload/run7.sh verdict main heavy 145 145 --as-block 5  # a re-run of an invalid block 5
evidence/overload/run7.sh verdict sens spread 1 12 --primer
evidence/overload/run7.sh summary
```

Re-issuing an interrupted command resumes it. `evidence/overload/run7.sh --dry` runs learn and one
block of each mix on `stand-dry.json`, to check the runner; its output is not evidence.

### Results (2026-10-24, coreslot v0.2.1): stopped at the pilot

Run 7 **stopped at the pilot**, as its futility rule requires: `pilot-check` returned `"go": false`
with T3 futile, and P infeasible. No main block was run. The pilot ran from 03:40 to 06:17 UTC:
12 heavy blocks (6 per level), 12 light and 6 wide, every set valid except four P sets that timed
out; no set was flagged. Its records are `run7/pilot-*.jsonl` and `run7/primer.jsonl`, and the
check is `run7/pilot_check.json`. The pilot enters no hypothesis test; the numbers below are
descriptive, over 6 blocks per level.

**The check.**

| Component | Pilot | Power at n = 72 | |
|---|---|---|---|
| T1, loaded ln(S2/K60) | mean 0.207, SD 0.203 | 1.00 | |
| T2, loaded swap traffic, K60 against S2 | 5 wins, 1 loss | 1.00 | |
| T3, quiet ln(S1/K60) | mean 0.005, SD 0.034 | 0.32 | futile |
| P | 4 of 12 heavy P sets timed out (all at loaded) | | infeasible |

The light pilot's F sets had no warn-or-worse sample.

**Mean time to verdict, per level** (seconds, mean over the level's 6 blocks; geometric mean ratio
against K60):

| Level | Headroom at t0 | K60 | S1 | S2 | P |
|---|---|---|---|---|---|
| Loaded | 7.86-7.95 GiB | 33.1 | 32.2 (0.97) | 40.9 (1.23) | 2 of 6 sets ran |
| Quiet | 9.62-11.80 GiB | 27.9 | 28.0 (1.00) | 34.4 (1.20) | 38.4 |

Warn seconds over the level's 6 sets: loaded K60 9, S1 2, S2 68; quiet K60 0, S1 0, S2 28. The
light mix's K60 against F was 0.94; the wide mix's S2 against K60 was 0.94, and F against K60
1.05. K60 held the type check and a suite at once (86 mem tokens against the static 76) in 4 of
its 12 heavy sets, two at each level: the ORM suite (`sqlalchemy-orm-tests`) in blocks 2, 6 and
12, and FastAPI's in block 1, for 8.1 s (beside 0.4 s with the ORM suite). The overlap passed 5 s
only in blocks 1 and 12. Blocks 1 and 6 were loaded sets, at a headroom at t0 of 7926 and
7952 MiB, so pairing depends on the arrival order and on the headroom when a suite is admitted,
not on the headroom at t0.

**What it means, by the outcomes declared before the data.** Run 7 confirmed nothing. The pilot
can only stop the run, so P1 is not tested, and every number above is exploratory. No declared
outcome covers a pilot stop; the stop is mapped by analogy onto outcome 2, "P1 not rejected only
through T3". Outcome 2's narrowed claim, "faster than a lock when the machine is quiet", is
not published as worded: the pilot did not support it. On this machine and mix, a machine-wide
lock of one slot (S1) kept pace with coreslot at both levels, while `sem -j2` lost at both, by
about a fifth of the time to verdict and with most of the memory pressure.

**Why T3's effect did not hold.** T3 rested on run 6's quiet effect (S1/K60 1.058 at 0 MiB), and
that effect came with no pairing: in none of run 6's 120 K60 heavy sets did a type check overlap
a suite for more than 5 s. It came from the two suites running together, for more than 5 s in
0.55 of run 6's K60 sets at 0 MiB and 0.53 at 3072 MiB, and in 0.33 of the pilot's (4 of 12).
Meanwhile S1 got faster: at quiet, its FastAPI suite's median fell from 18.6 to 13.4 s, and its
mean time to verdict from 35.5 to 28.0 s (K60's from 33.4 to 27.9 s). The likely cause is
`--dist loadgroup`, which removed a handicap FastAPI's suite carried in run 6; that risk was noted
before the pilot, though not in the text above. The cause is not a lack of pairing.

Outcome 2 also states: "Development continues only if the wide mix, reported descriptively, shows
the same frozen S2 losing too." In the pilot's 6 wide blocks S2 was faster than K60 in every one
(ln(S2/K60) mean −0.062, SD 0.024). By run 7's own rule, development does not continue on run 7's
strength. `sem --memsuspend 9G` started no gate within 600 s in 4 of its 6 loaded sets, so P is
reported as infeasible at `--memsuspend 9G` with a loaded headroom of 8000 MiB, not tuned.

**Deviations.** One, the owner's. The stop is the procedure's own: a `"go"` of false stops the
run, and no main phase follows it. What follows is the owner's to decide, recorded here. On
2026-10-28 the owner overrode outcome 2's condition and continues development for product
reasons, all visible in run 7's records:

- **`suggest`'s cpu ratchet.** The relearn pass narrowed W3's suite (`sqlalchemy-engine-tests`)
  to a cpu grant of 1 (see "Learning, frozen before the pilot"). Its verdict averaged 62.6 s
  under K60 over the 6 wide sets, against 32.2 s under S2 and 31.6 s under F.
- **Over-reservation.** In 5 of K60's 6 wide sets, the `test/sql test/dialect` suite waited
  16-26 s for cpu while the ORM suite and FastAPI's held 13 or 14 of the 14 cpu tokens.
- **A stand that is not representative.** The heavy mix's type check and both its suites each
  saturate the cores alone (5 of 5 learning runs each), so running them one at a time costs
  little.

A user would recognise both defects. This is an override of run 7's rule, not a result of it.
Corrected on 2026-10-28 (the pairing partner, the mechanism, P's scope, this deviation); the first
version is in the repository's history.
