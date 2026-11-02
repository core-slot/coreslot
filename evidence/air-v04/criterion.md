# The criterion

Written and committed after the pilot pair and before the first counted pair; the commit's time
is the pre-registration. `analyse.py` is not changed after this commit.

## Hypothesis

On this MacBook Air (M4, 16 GiB), when four agents start their gates at once on the stand in
[README.md](README.md), running each command under `coreslot run` (arm K, coreslot 0.4.0, the
release binary) instead of unwrapped (arm F) both keeps the machine out of memory pressure and
gets the agents their verdicts sooner.

## Primary claim

Both of these hold, over the five counted pairs `pair-1` to `pair-5` (ABBA order, F K K F F K K F
F K):

1. **Memory pressure:** K's seconds at memory pressure level warn or above are lower than F's in
   each untied pair, five of five: one-sided sign test, p = 0.031.
2. **Time to verdict:** K's mean time to verdict over the four agents is lower than F's in each
   untied pair, five of five: one-sided sign test, p = 0.031.

The claim is the intersection of the two, so it needs no correction for multiplicity. A tied pair
counts against the claim: with a tie, fewer than five untied pairs remain and neither test can
reach p < 0.05. If either part fails, the claim fails, and the result is reported as it falls.

## Reported, not tested

Seconds at critical, swap growth over the set, makespan, false failures (a non-zero exit of a gate
whose alone runs all passed: none of the four failed alone, and neither pilot set had one), K's
summed wait and the watchdog's stops. The watchdog demo is a functional check of one run, reported
as what happened, never as a statistic.

## What came before this commit

- The learn phase (three alone runs of each gate under coreslot) and the pilot pair, both kept in
  `record/` and shown by the analysis apart from the pairs. The pilot is exploratory: it chose this
  criterion and is no part of any test. Pilot F: 105 s at warn or above, 3 s critical, swap +3904
  MiB, makespan 118.9 s, mean time to verdict 107.4 s, no false failure. Pilot K: 0 s, 0 s, swap
  +0 MiB, makespan 64.3 s, mean time to verdict 39.9 s, no false failure, 96.2 s waited, no stop.
- Before the learn phase the machine's Docker VM was stopped (it held 9.7 GiB and kept the
  pressure level at warn); the level was normal from then on.
- Another job on the machine (a build and a test suite of about 1.6 cores, no large memory)
  overlapped the first minute of the learn phase, so the first alone run of the typecheck, of the
  FastAPI tests and part of the first SQLAlchemy run shared the CPU with it. The needs are learned
  from memory peaks; the alone runs' times are reported with that note.
- The typecheck label recorded no normal share in its three alone runs: alone, it needs 8.7 GiB
  on a machine with about 7 GiB free and pages throughout, which by v0.4's rules measures none, so
  its runs stay learning and the watchdog may stop them only at the floor. Admission uses its
  learned need either way.
