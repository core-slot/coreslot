# The second criterion: the best semaphore, and a machine with room

Two questions follow the [first result](README.md#result-2026-11-02): does coreslot beat the best
fixed-size semaphore a developer could put in front of the same commands, and does it cost the
agents anything when the machine has room for all of them? This file is committed in two steps.
This first commit, before any of the sets below has run, fixes how the best semaphore is chosen and
what the light mix is. The criteria are added after the exploratory sets and before the first
counted pair, and that commit's time is their pre-registration.

## The semaphore arm

S_N runs the same four agents unwrapped, each command behind a plain N-slot semaphore (N lock files
taken with `flock(2)`; a command waits for any free slot, first come first served by the kernel,
and holds it until it exits). No coreslot is involved.

**Choosing S\*.** Three exploratory sets run first, in this order: S1, S2, S3 (S4 would be F, which
the first run measured). S\* is the N whose set had the lowest mean time to verdict over the four
agents; a set with a non-zero exit of a stable gate is not eligible; a tie goes to the smaller N.
These sets are in no test. Then five pairs of K against S\* run in ABBA order.

## The light mix

The light mix is two agents that fit in the memory the machine can hand out: A3 (FastAPI `ruff`,
then its tests) and A4 (SQLAlchemy's ORM tests), about 4.5 GiB together against about 6.4 GiB
coreslot saw free during the first run. One exploratory pair of F and K runs first, then five pairs
in ABBA order.

## Criteria

Added after the exploratory sets and before the first counted pair; this commit's time is their
pre-registration. `analyse.py` is not changed after it.

**What the exploratory sets showed.** A fresh learn phase (three alone runs of each gate), then
S1, S2 and S3 (mean time to verdict 28.6, 41.1 and 69.7 s; makespan 55.5, 64.4 and 94.0 s; S2 and
S3 paged), so the rule above picks **S\* = S1**. One exploratory K set (`sem-pilot`, added to see K
in the same machine state) gave 33.5 s and 59.7 s. The light pilot gave F 44.8 s and K 39.4 s mean
time to verdict, K waiting 0 s. All agents of those sets started at the same instant.

**What changed before the counted pairs, and why.** An offline model of the records (processor
sharing over the cores, calibrated on these sets, within about 5 % on every set that did not page)
put S1 and K level when averaged over all 24 arrival orders (33.0 against 33.3 s): S1's lead in the
exploratory sets came mostly from the order in which its racing semaphore happened to admit the
agents. So the counted pairs compare policies, not that race:

- Agents arrive one second apart, in an order fixed here for each pair, the same for both arms of
  the pair. An agent's time to verdict is counted from its own arrival.
- The semaphore grants its slots first come, first served (`sem.py --fifo`); the tuning sets used
  the racing mode. S\* stays S1, which the rule picked by a wide margin.

Orders (arrival order of the agents):

| pair | heavy, K against S1 (`sem-pair-N`) | light, K against F (`light-pair-N`) |
|---|---|---|
| 1 | A1, A2, A3, A4 | A3, A4 |
| 2 | A2, A3, A4, A1 | A4, A3 |
| 3 | A3, A4, A1, A2 | A3, A4 |
| 4 | A4, A1, A2, A3 | A4, A3 |
| 5 | A4, A3, A2, A1 | A3, A4 |

Both run in ABBA order, the heavy pairs starting with S1 and the light pairs with F.

1. **K against the best semaphore (heavy stand):** K's mean time to verdict is at most 1.05 times
   S1's in each of the five pairs `sem-pair-1` to `sem-pair-5`: one-sided sign test of K ≤ 1.05 ×
   S1, five of five, p = 0.031. Seconds at memory pressure warn or above, makespan and K's waits
   are reported, not tested.
2. **A machine with room (light mix):** K's mean time to verdict is at most 1.05 times F's in each
   of the five pairs `light-pair-1` to `light-pair-5`: the same test, p = 0.031. Makespan and K's
   waits are reported, not tested.

The two are separate claims, each reported as it falls; the margin of 5 % is the owner's
definition of "practically the same", set before these pairs. The exploratory light pair differed
by 13 % between two arms with the same schedule, so a pair can fall outside 5 % on noise alone; if
one does, the claim fails and the ratios are published as they are.
