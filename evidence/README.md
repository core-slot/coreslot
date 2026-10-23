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
