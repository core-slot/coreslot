# The third run's criteria

Committed, with the script that computes them (`criteria3.py`), before any set of the third run;
that commit's time is the pre-registration. Unlike the first two runs, this one has no exploratory
sets: it runs unattended overnight, and its criteria are fixed from what the first two runs showed.
Neither this file nor `criteria3.py` nor `analyse.py` is changed after this commit.

## What it measures, and on what

coreslot 0.4.1, the release binary (its SHA-256 goes in `meta.jsonl`), on the same MacBook Air, the
same stand at the same pins, in `record-3/` with its own learn phase (three alone runs of each gate).
Docker Desktop is stopped and the machine is kept awake (`caffeinate`) for the whole run, which
starts on an otherwise idle machine on its charger. The parts run in this order.

1. **F against K, the first run again** (`pair-1` to `pair-10`, ABBA, F first in odd pairs; all
   agents start at the set's start, as in the first run).
2. **K against the best semaphore S1** (`sem-pair-1` to `sem-pair-24`, ABBA, S1 first in odd pairs;
   agents arrive one second apart; the semaphore is first come, first served). Each pair has its
   own arrival order, and the 24 pairs use all 24 orders of A1 to A4 once, in this sequence (the
   permutations shuffled with Python's `random.Random(20261102)`):
   `A3,A4,A1,A2; A4,A1,A3,A2; A1,A2,A3,A4; A3,A2,A1,A4; A2,A4,A1,A3; A2,A4,A3,A1; A1,A4,A2,A3;
   A3,A4,A2,A1; A4,A3,A1,A2; A4,A3,A2,A1; A2,A1,A4,A3; A2,A3,A4,A1; A2,A3,A1,A4; A3,A2,A4,A1;
   A4,A2,A3,A1; A2,A1,A3,A4; A1,A2,A4,A3; A3,A1,A4,A2; A1,A3,A4,A2; A1,A4,A3,A2; A4,A2,A1,A3;
   A3,A1,A2,A4; A1,A3,A2,A4; A4,A1,A2,A3`.
3. **K against F with room, the light mix** (`light-pair-1` to `light-pair-20`, ABBA, F first in odd
   pairs; A3 and A4 arrive one second apart, A3 first in odd pairs and A4 first in even ones).
4. **The watchdog demo, five times** (`demos/1` to `demos/5`, each in a fresh state directory, as in
   the first run: three 6 GiB hogs admitted on a need learned from a 256 MiB run).

## Criteria

Each comparison uses the per-set mean over the agents of each agent's time to its verdict (counted
from its own arrival), unrounded, and the seconds at memory pressure warn or above as `analyse.py`
counts them. A tie counts against the claim. Each p below is the one-sided sign test, P(X ≥ k) for
X ~ Binomial(n, 1/2).

1. **Replication.** Both hold: K spent fewer seconds at warn or above than F in at least 9 of the
   10 pairs (p = 0.011), and K's mean time to verdict was lower than F's in at least 9 of the 10
   pairs (p = 0.011).
2. **Against the best semaphore.** K's mean time to verdict is at most 1.05 times S1's in at least
   17 of the 24 pairs (p = 0.032): the median K/S1 is at most 1.05, over every arrival order.
3. **With room.** K's mean time to verdict is at most 1.05 times F's in at least 15 of the 20 pairs
   (p = 0.021).
4. **The watchdog, functionally.** In each of the five demos at least one hog is stopped, the oldest
   hog is never stopped, every hog's command exits 0 in the end, and the outside guard does not
   fire. Reported as how many of the five meet all four, not as a statistic.

The claims are reported separately, each at α = 0.05; the report also says which of 1 to 3 would
hold with a Bonferroni correction for three tests (α = 0.0167): only claim 1 can, by its numbers.
Every set is reported, including any that a part's cap or an error cut short; a part that does not
complete reports the pairs it has, and its claim is then judged on those pairs against the same k
out of the planned n (so an incomplete part cannot pass more easily).
