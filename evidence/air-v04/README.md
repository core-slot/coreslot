# Acceptance run on a 16 GB MacBook Air

Four coding agents run their gates at the same moment on one laptop, once with nothing in front of
them (F) and once with each command under `coreslot run --label <label> --` (K), in alternating
pairs. The question is whether the memory guard keeps the machine out of memory pressure without
costing the agents their time or their verdicts. A watchdog demo then checks, once, that a real
stop and restart happens on the shipped rules and that every stopped job still completes.

The records of every run, the raw data the numbers below come from, are in a separate repository,
[core-slot/coreslot-evidence](https://github.com/core-slot/coreslot-evidence) (`air-v04/record/`,
`record-2/` and `record-3/`, at commit `2beb02b`), so that this one stays small; the first two were
committed here up to commit `f63c23f`. Paths such as `record-2/` below are relative to its
`air-v04/`; `analyse.py` and the `criteria` scripts take the record's directory as their argument.

The stand is public code at pinned commits:

| agent | gate | source | learned need | median alone time, first run / second run |
|---|---|---|---|---|
| A1, A2 | `npm run --silent typecheck-client` | VS Code 1.142.0 (`d956e8a`) | 8.7 GiB | 15.4 s / 8.5 s |
| A3 | `ruff check fastapi tests`, then `pytest -n auto --dist loadgroup` | FastAPI 0.156.2 (`44cc742`) | 21 MiB, 2.4 GiB | 0.05 s, 17.4 s / 0.06 s, 13.6 s |
| A4 | `pytest -n auto test/orm` | SQLAlchemy rel_2_1_5 (`4fbf72c`) | 2.1 GiB | 35.1 s / 22.2 s |

The first run's alone times are slow: the machine was in a worse state during its learn phase than
later (see below), and the same type check ran alone in 9.4 to 10.4 s inside that run's own K sets.
Toolchain, the same for both runs and not changed on the machine between them: node 26.12.1, npm
11.20.0, uv 0.13.5, Python 3.13.0 for the stand's virtual environments and for the harness; the
records' `meta.jsonl` of these two runs names the machine, macOS and the coreslot binary, not
these.

## Result (2026-11-02)

The [criterion](criterion.md), committed after the pilot pair and before the first counted pair
(commit `9869326`, pushed at 18:24:37 UTC; pair 1's first set started 22 s later, at 18:24:59 UTC),
holds in both parts: K spent fewer seconds at memory pressure warn or above than F in 5 of 5 pairs,
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

"Swap written" is the `vm_stat` swapouts over the set (16 KiB pages); "swap used growth" is the
peak of `vm.swapusage` during the set against its start, as `analyse.py` computes it (at the end of
a set swap used can be below its start). "Mean time to verdict" is the mean over the four agents of
each one's time to its verdict, per set. Per pair, K's mean time to verdict was 0.34 to 0.46 of F's
(median 0.41) and its makespan 0.45 to 0.70 of F's (median 0.53); per agent the ratio ranges from
about 0.09 (A1) to about 0.6 (A3). In K the queue held the agents 51 to 117 s in all per set, and no
command was stopped.

Why K is faster as well: two 8.7 GiB type checks and two 2-3 GiB test suites need about 22 GiB at
once, and in the counted K sets coreslot's waiting lines show at most 6.4 GiB it could hand out (up
to 8 GiB in the pilot). F starts all of them and every one of them pages; K starts the first type
check, holds the rest until it has finished, and each then runs at about the speed it runs alone.
Median per agent over the five pairs, F against K: A1 115.1 s against 9.8 s, A2 116.4 s against 62.1
s, A3 64.8 s against 39.1 s, A4 124.0 s against 52.3 s. The agent that waits longest under K (A2,
the second type check) still gets its verdict sooner than under F. These per-agent medians, the
per-pair ratios and the swap-written column are computed from `sets.jsonl` by hand, not printed by
`analyse.py`.

**The demo.** A 30 s, 256 MiB hog taught the label `demo-hog` a need of 269 MiB (275,696 KiB; the
waiting lines round it to 270 MiB). Three hogs of 6 GiB each then started a second apart under
`coreslot run --label demo-hog`, so admission let all three in on that need: 18 GiB of hot memory on
16 GiB. Timeline from the start of the hogs:

- macOS's pressure level was critical from 4 to 10 s, warn, briefly normal at 12-13 s, warn again
  from 14 to 27 s, and normal from 28 s on.
- The watchdog's own measure (the hogs' share of time lost to memory stalls above their learned
  normal, in points) was 54 to 90 with the paging gate open on every sample from the first hot one
  until the second stop. Red came at its tenth hot sample, and its first red round stopped hog 3,
  the newest, at about 22 s (`memory stop 1 of 3: ... was stopped (memory red, at 6 GiB, 75% of its
  time went to memory stalls against its normal 0%)`).
- Its next round stopped hog 2 at about 38 s, ten seconds after macOS's level had returned to
  normal: by the watchdog's measure the hogs left were still losing most of their time to memory
  stalls (75 points at that sample; hog 2's stop line says 75 % against its normal), which macOS's
  machine-wide level, a coarser signal, no longer showed. The measure was 64 points at the next
  sample, under 4 at the one after, and near 0 from then on. Hog 1, the oldest, was never stopped.
- Each stopped hog re-queued with its 6 GiB peak as its need, waited for room (52 s and 161 s) and
  ran to completion; the outside guard did not fire.

The stop line's "normal 0%" is the label's learned share of 0.33 %, rounded to a whole percent.

### What this does not show, and what to know

- One machine, one stand, one evening, five pairs. The stand is heavy on purpose: on a lighter mix
  that never pages, unwrapped runs lose nothing and coreslot's queue only costs waiting time.
- No false failure happened in either arm, so this run says nothing about flaky tests under memory
  pressure.
- The demo is one run, a functional check of the stop and restart path, not a measurement of how
  often or how fast the watchdog acts.
- The pilot pair (F: 105 s at warn or above, 3 s critical, makespan 118.9 s; K: 0 s, 0 s, 64.3 s)
  chose the criterion and is in no test.
- The Docker Desktop VM (9.7 GiB) was stopped before the learn phase of each run, because it kept
  the machine at pressure level warn, and stayed stopped for every set of both runs; the level was
  normal at the start of every set. The desktop apps of the person running it (a chat client and
  its agents, a terminal) stayed open.
- The learn phase of this run ran in a worse machine state than everything after it, and the cause
  is only partly recorded. A build and a test suite of another job (about 1.6 cores, no large
  memory) overlapped its first 16 s (`criterion.md`, written at the time, says "the first minute",
  an estimate made before the other job's log was read). But the alone runs stayed slow beyond that:
  the second type check run took 18.6 s about 1.5 min in, and the third SQLAlchemy run used 250 s
  of user time against about 190 s in the second run. The same type check ran alone in 9.4 to 10.4
  s inside this run's K sets. The learned needs come from memory peaks, which the slowness does not
  change much; the alone times in the table above are this phase's.
- In that state the type check paged throughout its alone runs, and a run that pages throughout
  measures no normal share, so its label stayed learning and the watchdog could have stopped it
  only at the floor during this run. In the second run's learn phase it did not page (2.5 to 3.4 s
  of system time) and learned a share of about 0.02. Admission used its learned need either way.
- K's state directory keeps learning from the sets' own runs, so K is not strictly a fixed policy
  across the pairs (each start recomputes its need from the label's latest runs), and swap used at
  the start of a counted set drifted from 5.8 to 8.2 GiB over the run. The sign test treats the pairs as independent; the ABBA
  order spreads such drift over both arms.
- `analyse.py` was extended after this run's criterion was committed (commit `4db2895` adds the
  second run's sections); its output for `record/` is byte-identical before and after.
- `meta.jsonl` shows `repo_dirty: true` for every counted invocation of both runs because the
  untracked record directory counted as a change; the harness files themselves were committed.
  `analyse.py` counts a pressure sample it could not read as normal; there were none.
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
pair (commit `f2ad269`, pushed at 20:14:44 UTC; the first counted set started at 20:18:01 UTC).
Records: `record-2/`, with its own learn phase. `criteria2.py` computes both criteria from the raw
records (written after the data, because `analyse.py` was frozen and does not print them); the
ratios below are its unrounded ones, rounded once.

**The best semaphore is S1, by the pre-registered rule.** One exploratory set each of S1, S2 and S3
(one, two or three commands at a time, all agents started at once, the semaphore's waiters racing):
mean time to verdict 28.6, 41.1 and 69.7 s; S2 and S3 paged. These are single exploratory sets.
One exploratory K set in the same state (`sem-pilot`) took 33.5 s, K/S1 1.17.

**Against S1, the criterion fails.** K's mean time to verdict was within 5 % of S1's in three of the
five pairs, not five:

| pair | arrival order | S1 mean s | K mean s | K/S1 | S1 makespan s | K makespan s |
|---|---|---|---|---|---|---|
| 1 | A1 A2 A3 A4 | 32.6 | 35.5 | 1.087 | 59.5 | 62.3 |
| 2 | A2 A3 A4 A1 | 39.5 | 35.0 | 0.888 | 65.5 | 59.3 |
| 3 | A3 A4 A1 A2 | 45.7 | 46.1 | 1.010 | 71.1 | 67.2 |
| 4 | A4 A1 A2 A3 | 45.1 | 45.9 | 1.017 | 69.3 | 70.2 |
| 5 | A4 A3 A2 A1 | 46.1 | 58.4 | 1.266 | 71.1 | 87.4 |

The median K/S1 is 1.017, and K was faster in one pair, but pair 5 shows the cost of a memory-only
guard: SQLAlchemy and FastAPI arrived first, both fit in memory, so coreslot ran them together, and
each is `pytest -n auto` on every core: together they took 49 and 32 s against about 22 and 14 s
alone, while the type checks waited behind them. A semaphore of one never overlaps them. Memory
pressure was level: S1 spent 4 s at warn in pair 3, K 7 s in pair 5, and neither wrote to swap.
What this does not settle: the right size for a semaphore depends on the workload, and on this
stand the exploratory S2 and S3 sets were 1.4 and 2.4 times slower than S1 and paged; how a
semaphore of one would do on the light mix below was not measured.

**With room, the criterion holds, narrowly.** On the light mix (FastAPI and SQLAlchemy, about 4.5
GiB against about 6.4 GiB coreslot could hand out in the counted pairs), K's mean time to verdict
was at most 1.05 times F's in five of five pairs (one-sided sign test, p = 0.031): K/F 0.654, 1.036,
1.011, 1.005 and 0.970 (median 1.005; K was slower in three of the five), makespan 0.65, 0.98,
1.04, 0.97 and 0.99. K's commands waited 0 to 1.3 s in all per set; no set paged. Pair 1 carries the
claim more than it should: its F set was slow (58.5 s), and both light F sets that ran right after a
heavy set were slow (the pilot's F after the S3 set, 44.8 s; pair 1's F after the last heavy K set,
58.5 s), while the ABBA order put F first after the heavy sets. Pair 1's K (38.3 s) was the slowest
K of the five; against the other F sets (34.4 to 37.0 s) it would be 1.04 to 1.11, so had that F set
been typical, pair 1 could have fallen outside 5 %. The claim holds as registered, and it is fragile.
The two claims of this run were registered separately, each at p = 0.031; with a correction for two
tests (0.025 each) five pairs could not reach significance at all.

What to know about this run:

- Agents arrived one second apart in an order fixed per pair (the same in both arms of a pair): the
  four rotations of A1 A2 A3 A4 and the reversed order, chosen by hand before the counted pairs, not
  from per-order predictions. An agent's time to verdict counts from its own arrival; the semaphore
  granted its slot first come, first served.
- Why the order was fixed: the exploratory sets started all agents at once with a semaphore whose
  waiters raced, and K lost there (1.17). An offline model of the records, published in
  [`model/`](model/README.md) with its validation and its limits, put S1 and K level when averaged
  over all 24 arrival orders (33.0 against 33.3 s) and traced S1's lead to the order its race
  happened to admit the agents in. The change was made after K's exploratory loss; the counted
  claim failed anyway.
- The Docker Desktop VM was stopped for every set; it ran only while no set was running, between the
  exploratory sets and the counted pairs. Swap used was about 6 GiB at the start of the counted
  pairs (about 3 GiB before the first run).
- The machine was faster than in the first run: the alone times of the second learn phase are 22 to
  45 % shorter.
- In `record-2/` the same path placeholders are used, and the Python interpreter's framework path is
  shortened to `python3`; `analyse.py` prints the same output before and after. `analyse.py
  record-2` also prints empty sections of the first run's layout and names `criterion.md`; the
  second run's criteria are in `criterion-2.md`.

## Third result (2026-11-03): the same questions, with more pairs, overnight

The first two runs were one evening each, with five pairs per claim, and the second one's two
claims split: within 5 % of the best semaphore in three of five pairs, and within 5 % of unwrapped
with room in five of five, narrowly. The third run asks all of it again, unattended, with more
pairs, on the v0.4.1 release binary (SHA-256 `c13de621…88cf34` in `meta.jsonl`, checked against
the release's `checksums.txt` and its attestation). [criterion-3.md](criterion-3.md) and
`criteria3.py` were committed before any of its sets (commit `12d2286`, pushed to `main` at
22:20:17 UTC on 2026-11-02; the learn phase started at 23:00:40 UTC and the first counted set at
23:04:00 UTC). It has no exploratory sets. Records: `record-3/`, with its own learn phase (alone
medians: type check 9.9 s, FastAPI's tests 13.4 s, SQLAlchemy's 21.9 s, the learned needs as in
the table above). The numbers below are `criteria3.py`'s, or computed from `sets.jsonl` where
marked.

**All four claims hold.** Each p is the one-sided sign test the criterion names; all three
statistical claims also hold with a Bonferroni correction for three tests (α = 0.0167), which the
criterion expected only of the first.

| part | pairs | criterion | result | p |
|---|---|---|---|---|
| 1. F against K, all agents at once | 10 | K lower in warn seconds and in mean time to verdict, each in ≥ 9 | 10 of 10 and 10 of 10 | 0.001 each |
| 2. K against S1, arrivals 1 s apart, all 24 orders | 24 | K ≤ 1.05 × S1 in ≥ 17 | 23 of 24 | 1.5 × 10⁻⁶ |
| 3. K against F, light mix | 20 | K ≤ 1.05 × F in ≥ 15 | 20 of 20 | 9.5 × 10⁻⁷ |
| 4. Watchdog demo | 5 | a hog stopped, never the oldest, all exit 0, guard quiet | 5 of 5 | (not a test) |

**Part 1 replicates the first run.** Unwrapped, the machine sat at warn or above for 59 to 101 s
of each set and wrote 2.0 to 11.6 GiB to swap (computed); under coreslot it stayed at normal in
every set and wrote nothing. K's mean time to verdict was 30.4 to 39.7 s against F's 73.1 to
110.1 s: K/F 0.31 to 0.45, median 0.40 (the first run: 0.34 to 0.46, median 0.41), makespan 0.48
to 0.66 of F's (computed). K's queue held the agents 45 to 96 s in all per set; no command was
stopped and no gate failed in either arm.

**Part 2: K and the best semaphore are level over every arrival order.** K/S1 was 0.961 to 1.060,
median 1.003; K was faster in 11 of the 24 pairs, and its mean over all 24 was 38.58 s against
S1's 38.41 s (0.4 % slower; computed), which is what the offline model in [`model/`](model/README.md)
predicted for the average over orders. Makespan K/S1 was 0.94 to 1.04, median 0.98 (computed).
Neither arm reached warn or wrote to swap in any of the 48 sets. The one pair outside 5 %
(sem-pair-5, order A2 A4 A1 A3, 1.060) is the cost the second run found: coreslot let FastAPI's
suite in beside SQLAlchemy's while the second type check waited, so A3 got its verdict in 34.5 s
instead of 59.9 s, and A1 and A4 in 61.4 and 51.7 s instead of 44.3 and 35.3 s. The second run's
worst order, A4 A3 A2 A1 (1.266 there), was sem-pair-10 here at 1.023; on this run's machine state
it did not repeat.

**Part 3: with room, coreslot costs nothing measurable.** K/F was 0.976 to 1.012, median 1.000;
K was faster in 10 of the 20 pairs, and its commands waited 0.01 to 0.02 s in all per set. The
second run's light claim rested on one slow F set; here no pair came within 3.8 points of the
margin.

**Part 4: the watchdog demo, five times.** In each demo the three 6 GiB hogs were admitted on the
learned 270 MiB need, both newer hogs were stopped on red (the stop lines give 63 to 81 % of their
time lost to memory stalls against a normal of 0.3 to 0.4 %), hog 1 never was, both stopped hogs
came back and every hog exited 0. Per demo, seconds at warn or above 21 to 39 and at critical 0 to
19, swap used growth 0.5 to 6.0 GiB.

What to know about this run:

- One machine, one stand, one night: 108 counted sets in 2 h 13 min, the whole run 2 h 44 min. The
  machine was on its charger and kept awake; the Docker Desktop VM was stopped for the whole run
  and started again after it. The desktop apps of the person running it (a chat client with the
  idle session that launched the run, a terminal) stayed open.
- Every set started at pressure level normal; no cooldown gave up. Swap used at the start of a set
  was 1.8 to 4.3 GiB. The thermal state was "fair" (1) at the start of 105 of the 108 sets and
  "nominal" (0) at three.
- In demo 1, swap used grew 6,011 MiB, 133 MiB short of the outside guard's 6 GiB (`SWAP_STOP_MIB`
  = 6144 MiB), so the guard did not fire, by a narrow margin. In demos 1 and 5 the harness's own
  once-a-second sampler fell up to 4.7 s behind at critical pressure, so a brief excursion between
  two samples cannot be ruled out; the guard reads the same samples, so its verdict is the
  recorded one.
- In its three alone runs the type check recorded no normal share; it recorded one in 44 of its 68
  runs inside K sets. Until it has one the watchdog can stop it only at the floor; no command of
  any K set came near a stop.
- K's state directory keeps learning from the sets' own runs, as in the earlier runs, and the ABBA
  order (F first in odd pairs of parts 1 and 3, S1 first in odd pairs of part 2) spreads that drift
  over both arms.
- `analyse.py record-3` prints the first two runs' sections; its DEMO section says "no record"
  because the five demos are in `record-3/demos/1` to `5`, which `criteria3.py` reads, and it names
  `criterion.md`; the third run's criteria are in `criterion-3.md`.
- `meta.jsonl` shows `repo_dirty: false` for every invocation. The same path placeholders as in
  `record-2/` are used; `analyse.py` and `criteria3.py` print the same output before and after.

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
runs use no network). `setup.sh` installs the stand's dependencies, which runs their build code:
FastAPI's from its lock file, SQLAlchemy's build with unpinned build requirements, and the type
declarations of Electron with install scripts disabled. Then take a coreslot binary: the runs here
used the v0.4.0 release archive for darwin-arm64 (the third run v0.4.1's), checked with `shasum -a 256 -c` against the
release's `checksums.txt` and with `gh attestation verify` (its SHA-256 is in `meta.jsonl`);
building one from the checkout under test, as below, works the same way for a later version:

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
python3 evidence/air-v04/criteria2.py evidence/air-v04/record-2       # the two pre-registered criteria
```

The recorded run differs from these commands in two ways: its tune sets and its exploratory K set
(`run2 set K --tag sem-pilot`) started all agents at once, and its tune sets used the semaphore's
racing mode, which the harness has since replaced with first-come-first-served for every S set.

The third run ([criterion-3.md](criterion-3.md)) ran these in one go, with the 24 orders and the
light orders of the criterion in `ORD24` and `ORD20`:

```bash
run3() { python3 evidence/air-v04/run.py --coreslot /tmp/air-v04/coreslot --stand "$STAND" --out evidence/air-v04/record-3 "$@"; }
run3 learn
run3 pairs --first 1 --last 10
run3 --stagger-s 1.0 pairs --vs S --slots 1 --prefix sem- --first 1 --last 24 --orders "$ORD24"
run3 --mix light --stagger-s 1.0 pairs --prefix light- --first 1 --last 20 --orders "$ORD20"
for i in 1 2 3 4 5; do python3 evidence/air-v04/run.py --coreslot /tmp/air-v04/coreslot --stand "$STAND" --out evidence/air-v04/record-3/demos/$i demo; done
python3 evidence/air-v04/criteria3.py evidence/air-v04/record-3
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
| `record*/` | the runs' records, published in coreslot-evidence and ignored here; each holds the files below |
| `record/meta.jsonl` | one line per invocation: machine model, macOS version, memory, `coreslot version` and its SHA-256, the commit of this repository, the stand's commits |
| `record/learn.jsonl`, `record/learn/` | each alone run, with its run-log line, and its output |
| `record/sets.jsonl`, `record/sets/<tag>-<arm>/` | one line per set, and each gate's output and stderr (`watch.jsonl` for K) |
| `record/demo/` | `demo.json`, the hogs' own logs, each run's stderr and `watch.jsonl`; `record-3/demos/1` to `5` hold the third run's five |
| `record/state/`, `record/demo/state/`, `record/live.json` | the run's coreslot state and the processes it started; never published |
| `criteria2.py`, `criteria3.py` | the second and third runs' pre-registered criteria, computed from their records |
