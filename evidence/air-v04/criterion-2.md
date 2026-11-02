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

To be added here after the exploratory sets, before the first counted pair.
