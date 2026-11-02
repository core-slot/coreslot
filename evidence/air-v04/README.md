# Acceptance run on a 16 GB MacBook Air

Four coding agents run their gates at the same moment on one laptop, once with nothing in front of
them (F) and once with each command under `coreslot run --label <label> --` (K), in alternating
pairs. The question is whether the memory guard keeps the machine out of memory pressure without
costing the agents their time or their verdicts. A watchdog demo then checks, once, that a real
stop and restart happens on the shipped rules and that every stopped job still completes.

The stand is public code at pinned commits:

| agent | gate | source | alone: learned need, median time |
|---|---|---|---|
| A1, A2 | `npm run --silent typecheck-client` | VS Code 1.142.0 (`d956e8a`) | 8.7 GiB, 15.4 s |
| A3 | `ruff check fastapi tests`, then `pytest -n auto --dist loadgroup` | FastAPI 0.156.2 (`44cc742`) | 21 MiB and 2.4 GiB, 0.05 s and 17.4 s |
| A4 | `pytest -n auto test/orm` | SQLAlchemy rel_2_1_5 (`4fbf72c`) | 2.1 GiB, 35.1 s |

## Result (2026-11-02)

The [criterion](criterion.md), committed after the pilot pair and before the first counted pair
(commit `9869326`, pushed at 18:24:33 UTC; pair 1's first set started at 18:24:59 UTC), holds in
both parts: K spent fewer seconds at memory pressure warn or above than F in 5 of 5 pairs,
and K's mean time to verdict was lower than F's in 5 of 5 pairs (each a one-sided sign test, p =
0.031). The watchdog demo stopped the two newest of three oversized jobs on the red level, never the
oldest, restarted both, and all three completed.

| pair | arm | warn+ s | critical s | swap written | swap used growth | makespan s | mean time to verdict s | failures |
|---|---|---|---|---|---|---|---|---|
| 1 | F | 66 | 0 | 7.8 GiB | 1497 MiB | 88.9 | 76.1 | 0 |
| 1 | K | 0 | 0 | 0 | 0 | 61.9 | 34.8 | 0 |
| 2 | K | 0 | 0 | 0 | 0 | 62.9 | 40.5 | 0 |
| 2 | F | 112 | 0 | 8.6 GiB | 1242 MiB | 140.0 | 118.1 | 0 |
| 3 | F | 95 | 0 | 8.5 GiB | 1181 MiB | 124.0 | 105.1 | 0 |
| 3 | K | 0 | 0 | 0 | 0 | 66.8 | 45.7 | 0 |
| 4 | K | 0 | 0 | 0 | 0 | 62.1 | 40.5 | 0 |
| 4 | F | 89 | 0 | 12.3 GiB | 2158 MiB | 117.1 | 99.0 | 0 |
| 5 | F | 115 | 0 | 12.9 GiB | 1376 MiB | 143.2 | 121.6 | 0 |
| 5 | K | 0 | 0 | 0 | 0 | 65.0 | 42.2 | 0 |

"Swap written" is the `vm_stat` swapouts over the set (16 KiB pages); "swap used growth" is
`vm.swapusage` at the end against the start. Per pair, K's mean time to verdict was 0.34 to 0.46
of F's (median 0.41) and its makespan 0.45 to 0.70 of F's (median 0.53). In K the queue held the
agents 51 to 117 s in all per set, and no command was stopped.

Why K is faster as well: two 8.7 GiB type checks and two 2-3 GiB test suites need about 22 GiB at
once, and coreslot's waiting lines show at most 6.4 GiB it could hand out. F starts all of them and every one of them pages;
K starts the first type check, holds the rest until it has finished, and each then runs at the speed
it runs alone. Median per agent over the five pairs, F against K: A1 115.1 s against 9.8 s, A2 116.4
s against 62.1 s, A3 64.8 s against 39.1 s, A4 124.0 s against 52.3 s. The agent that waits longest
under K (A2, the second type check) still gets its verdict sooner than under F.

**The demo.** A 30 s, 256 MiB hog taught the label `demo-hog` a need of 270 MiB. Three hogs of 6
GiB each then started a second apart under `coreslot run --label demo-hog`, so admission let all
three in on 270 MiB each: 18 GiB of hot memory on 16 GiB. macOS reached pressure level critical 4 s
in. The watchdog logged red for 10 samples; its first red round stopped hog 3, the newest (`memory
stop 1 of 3: ... was stopped (memory red, at 6 GiB, 75% of its time went to memory stalls against
its normal 0%)`), and the next round stopped hog 2; hog 1, the oldest, was never stopped. The
pressure level was back to normal 28 s in and stayed there. Each stopped hog re-queued with its
6 GiB peak as its need, waited for room (52 s and 161 s) and ran to completion; the outside guard
did not fire. The stop line's "normal 0%" is the label's learned share of 0.3 %, rounded.

### What this does not show, and what to know

- One machine, one stand, one evening, five pairs. The stand is heavy on purpose: on a lighter mix
  that never pages, unwrapped runs lose nothing and coreslot's queue only costs waiting time.
- No false failure happened in either arm, so this run says nothing about flaky tests under memory
  pressure.
- The demo is one run, a functional check of the stop and restart path, not a measurement of how
  often or how fast the watchdog acts.
- The pilot pair (F: 105 s at warn or above, 3 s critical, makespan 118.9 s; K: 0 s, 0 s, 64.3 s)
  chose the criterion and is in no test.
- Before the learn phase the Docker Desktop VM (9.7 GiB) was stopped, because it kept the machine at
  pressure level warn; the level was normal from then on. The desktop apps of the person running it
  (a chat client and its agents, a terminal) stayed open; the record's `meta.jsonl` names the
  machine and the versions.
- A build and a test suite of another job (about 1.6 cores, no large memory) overlapped the first
  16 s of the learn phase, that is part of the first type check's alone run. The needs are learned
  from memory peaks.
- The type check label learned no normal share: alone it needs 8.7 GiB with about 7 GiB free and
  pages throughout, and a run that pages throughout measures none. So the watchdog could stop it
  only at the floor; admission used its learned need either way.
- In the committed records the machine's paths are replaced after the run: the checkout by
  `<repo>`, the stand by `<stand>`, the directory of the coreslot binary by `<bin>` and the home
  directory by `~`. Nothing else was changed, and `analyse.py` prints the same output before and
  after.
- The thermal state was "fair" (1) at the start of nine of the ten counted sets and "nominal" (0) at
  one; it is recorded per set.

## Second result (2026-11-02): the best semaphore, and a machine with room

Two questions followed: does coreslot beat the best fixed-size semaphore a developer could put in
front of the same commands, and does it cost anything when the machine has room for everything?
[criterion-2.md](criterion-2.md) fixed how the best semaphore is chosen before any of these sets
ran (commit `e9ba353`), and the two criteria after the exploratory sets and before the first counted
pair (commit `f2ad269`, pushed at 20:14:40 UTC; the first counted set started at 20:18:01 UTC). Records:
`record-2/`, with its own learn phase.

**The best semaphore is S1.** One exploratory set each of S1, S2 and S3 (one, two or three commands
at a time): mean time to verdict 28.6, 41.1 and 69.7 s; S2 and S3 paged. Strictly one at a time is
the best fixed size on this stand.

**Against S1, the criterion fails.** K's mean time to verdict was within 5 % of S1's in three of the
five pairs, not five:

| pair | arrival order | S1 mean s | K mean s | K/S1 | S1 makespan s | K makespan s |
|---|---|---|---|---|---|---|
| 1 | A1 A2 A3 A4 | 32.6 | 35.5 | 1.09 | 59.5 | 62.3 |
| 2 | A2 A3 A4 A1 | 39.5 | 35.0 | 0.89 | 65.5 | 59.3 |
| 3 | A3 A4 A1 A2 | 45.7 | 46.1 | 1.01 | 71.1 | 67.2 |
| 4 | A4 A1 A2 A3 | 45.1 | 45.9 | 1.02 | 69.3 | 70.2 |
| 5 | A4 A3 A2 A1 | 46.1 | 58.4 | 1.27 | 71.1 | 87.4 |

The median K/S1 is 1.02, and K was faster in one pair, but pair 5 shows the cost of a memory-only
guard: SQLAlchemy and FastAPI arrived first, both fit in memory, so coreslot ran them together, and
each is `pytest -n auto` on every core: together they took 49 and 32 s against about 22 and 14 s
alone, while the type checks waited behind them. A semaphore of one never overlaps them. Memory
pressure was level: S1 spent 4 s at warn in pair 3, K 7 s in pair 5, and neither wrote to swap.
A semaphore of the right size has to be chosen per workload, though: on this stand S2 was 1.4 times
and S3 2.4 times slower than S1 and paged, and on the light mix below a semaphore of one would
serialise two commands that fit together.

**With room, the criterion holds.** On the light mix (FastAPI and SQLAlchemy, about 4.5 GiB against
about 6.4 GiB coreslot could hand out), K's mean time to verdict was at most 1.05 times F's in five
of five pairs (one-sided sign test, p = 0.031): K/F 0.66, 1.04, 1.01, 1.01 and 0.97, makespan 0.65,
0.98, 1.04, 0.97 and 0.99. K's commands waited 0 to 1.3 s in all per set; no set paged. Pair 1's F
set was slow for no recorded reason, which favours K there; without it the largest K/F is 1.035.

What to know about this run: agents arrived one second apart in the order fixed per pair (the same
in both arms of a pair), and an agent's time to verdict counts from its own arrival; the semaphore
granted its slot first come, first served. The exploratory sets started all agents at once with a
semaphore that let its waiters race, and an offline model of the records showed S1's lead there
came mostly from that race, which is why the counted pairs fix the order. The Docker Desktop VM was
stopped again before the learn phase and before the counted pairs (Docker was running between the
two, for other work on the machine); the level was normal at the start of every set. Swap used was
about 6 GiB at the start of the counted pairs (about 3 GiB before the first run). In `record-2/` the
same path placeholders are used, and the Python interpreter's framework path is shortened to
`python3`; `analyse.py` prints the same output before and after.

## The phases

1. **learn** (about 5 min): each gate runs alone under coreslot three times, in the run's own state
   directory, so that every label has a learned need and a normal share before the first K set.
   These runs are also the false-failure baseline: a gate whose alone runs all passed is stable,
   and a non-zero exit of a stable gate in a set is a false failure.
2. **pilot** (about 4 min): one F set and one K set, exploratory. The analysis prints it apart and
   no test uses it. After it, the owner writes the criterion into `criterion.md` beside this file
   and commits it before the pairs start; the analysis computes the candidate statistics and does
   not read the criterion.
3. **pairs** (about 20 min): five pairs in ABBA order, F K K F F K K F F K. Every set starts the
   four agents at once, after a cooldown that waits until the memory pressure level is normal and
   swap used has held still (within 64 MiB) for 5 s, at least 15 s and at most 120 s; the record
   says how long it took and whether it gave up.
4. **demo** (about 6 min, n = 1, a functional check): below.

Per set the record holds each agent's start, end, time to verdict and exit; the makespan; the
memory pressure level and swap used every second (`sysctl kern.memorystatus_vm_pressure_level`, 1
normal, 2 warn, 4 critical; `vm.swapusage`); the `vm_stat` swapouts and pageouts over the set; the
thermal state at its start; and for K every run-log line the set added (`waited_s`, `ran_s`,
`mem_peak_kib`, `stops`), each command's stderr with coreslot's own lines, and the watchdog's
samples.

### The demo

`calibration/hog.py` holds a fixed amount of hot memory and rewrites random pages of it. The demo
first teaches the label `demo-hog` a small need: a 256 MiB hog runs alone for 30 s, so the run log
holds one counted run with a memory peak near 280 MiB and a `tick_share`, the label's normal share
(retried up to three times if no tick had the paging gate closed). Then three hogs start a second
apart under `coreslot run --label demo-hog`, each mapping 3/8 of the machine's memory at once
(6 GiB on 16 GiB) and holding it for 90 s, with the hog's own slowness stop turned off. Admission
lets all three in on the learned 280 MiB need; together they hold 18 GiB of hot memory on a 16 GiB
machine, so every touch of a compressed page decompresses it, the paging gate opens and the hogs'
share of time lost to memory rises far above their learned normal share. That is the excess the
red level counts: once it has held at or over `red_points` for about `red_samples` samples, a red
round stops the newest hog, never the oldest, and another round after `post_kill_s` takes the next
one if the machine is still red. A stopped hog re-queues at its first arrival with its peak as its
need, and comes back when memory is calm and has room for it, which on this machine is when the
hog before it has finished. All thresholds are the shipped defaults; the state directory's
`config.toml` only turns on the sample log. From outside, the demo stops everything when swap used
grows 6 GiB from its start or after 600 s, and the record says which.

## Reproducing it

Quit other work first. Build the stand once (git, node with npm, uv and the network; the measured
runs use no network), and build coreslot from the checkout under test outside the repository:

```bash
STAND=/path/to/stand.noindex          # the .noindex suffix keeps Spotlight out
evidence/air-v04/setup.sh "$STAND"
go build -o /tmp/air-v04/coreslot ./cmd/coreslot
run() { python3 evidence/air-v04/run.py --coreslot /tmp/air-v04/coreslot --stand "$STAND" --out evidence/air-v04/record "$@"; }
```

Then, in this order:

```bash
run --dry pairs                      # any phase with --dry prints what it would run and starts nothing
run learn                            # about 5 min
run set F --tag pilot                # about 2 min
run set K --tag pilot                # about 2 min
python3 evidence/air-v04/analyse.py evidence/air-v04/record
# write evidence/air-v04/criterion.md by hand and commit it
run pairs                            # about 20 min; --first N --last N runs part of it
run demo                             # about 6 min; --hog-gib G sets the hogs' size
python3 evidence/air-v04/analyse.py evidence/air-v04/record
```

The second run ([criterion-2.md](criterion-2.md)) writes a record of its own, with its own learn
phase, so the first record stays as it is:

```bash
run2() { python3 evidence/air-v04/run.py --coreslot /tmp/air-v04/coreslot --stand "$STAND" --out evidence/air-v04/record-2 "$@"; }
run2 learn                                   # about 5 min
run2 tune                                    # S1, S2, S3, about 5 min
python3 evidence/air-v04/analyse.py evidence/air-v04/record-2      # TUNING names S*
run2 --mix light set F --tag light-pilot     # about 1 min
run2 --mix light set K --tag light-pilot     # about 1 min
# write the criteria into evidence/air-v04/criterion-2.md and commit them
# the counted pairs: agents arrive 1 s apart in the pre-registered order of each pair
run2 --stagger-s 1.0 pairs --vs S --slots 1 --prefix sem- \
  --orders "A1,A2,A3,A4;A2,A3,A4,A1;A3,A4,A1,A2;A4,A1,A2,A3;A4,A3,A2,A1"   # sem-pair-1..5, S first in odd pairs, about 15 min
run2 --mix light --stagger-s 1.0 pairs --prefix light- \
  --orders "A3,A4;A4,A3;A3,A4;A4,A3;A3,A4"                             # light-pair-1..5, F first in odd pairs, about 10 min
python3 evidence/air-v04/analyse.py evidence/air-v04/record-2
```

Without `--order` (one set) or `--orders` (one order per pair, the same for both sets of a pair)
every agent arrives at the set's start; with them agent i of the order arrives `i x --stagger-s`
after it. A set records its order, each agent's `arrive_s`, and each agent's time to verdict
counted from its own arrival; the makespan is counted from the set's start.

An S set runs each command unwrapped behind `sem.py --fifo LOCKDIR N -- CMD`, a plain N-slot
semaphore (N lock files under the set's `slots/`, taken with `flock` and held until the command
exits) that grants the slots in arrival order, and records `sem_fifo: true`. The exploratory
`tune` sets of `record-2` ran before that, without `--fifo`: all agents arrived at once and any
waiter could win the race for a free slot. A light set runs A3 and A4 only. A tag says what it is for, and `run.py` refuses a tag that does
not fit its arm or mix.

`run.py` refuses to start while a process it started for an earlier set is alive (it lists them
by pid), refuses a K set while the budget holds a live hold or a waiting ticket, and refuses a
stand that is not at its pins. Every process it starts is stopped by its process group at exit
and on INT, TERM or HUP. An interrupted set leaves its directory without a record line; delete
that directory to run the set again. `--smoke` swaps the gates for short Python allocations and
skips the pin check, to try the harness without loading the machine.

## Files

| file | what it holds |
|---|---|
| `setup.sh` | builds the stand at its pins |
| `run.py` | the phases above; stdlib only |
| `analyse.py` | the per-pair table (warn s, critical s, swap growth, makespan, mean time to verdict, false failures, and for K the summed wait and the stops), one-sided sign tests of K against F for warn seconds, false failures and mean time to verdict, the pilot apart, and the demo's verdict; for the second run TUNING (each S set and the S\* the rule picks), SEMAPHORE PAIRS (K against S\*, with K/S\* ratios) and LIGHT (its pilot apart, K against F with K/F ratios and sign tests both ways) |
| `sem.py` | the S arm's N-slot semaphore |
| `test_air.py` | unit tests of the analysis and of the pure parts of `run.py` (`python3 -m unittest` here) |
| `record/meta.jsonl` | one line per invocation: machine model, macOS version, memory, `coreslot version` and its SHA-256, the commit of this repository, the stand's commits |
| `record/learn.jsonl`, `record/learn/` | each alone run, with its run-log line, and its output |
| `record/sets.jsonl`, `record/sets/<tag>-<arm>/` | one line per set, and each gate's output and stderr (`watch.jsonl` for K) |
| `record/demo/` | `demo.json`, the hogs' own logs, each run's stderr and `watch.jsonl` |
| `record/state/`, `record/demo/state/`, `record/live.json` | the run's coreslot state and the processes it started; not committed |
