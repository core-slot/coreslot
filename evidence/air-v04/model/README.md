# An offline model of the stand

An exploratory fluid processor-sharing model of the four agents' gates, calibrated on this run's
own records. It is not evidence and no claim rests on it. It was used once, before the counted pairs of the
second run: averaged over all 24 arrival orders it put S1 and K level on the heavy stand (mean
time to verdict 33.0 against 33.3 s, `pol.txt`, `run2-heavy`), which suggested that S1's lead in
the exploratory sets came from the order its racing semaphore admitted the agents. That is why
[criterion-2.md](../criterion-2.md) fixes the arrival order of every counted pair.

## Assumptions

- 10 identical cores. The machine's performance and efficiency cores cannot be told apart in the
  records, so the difference is folded into `eta`.
- Each gate alone takes its measured time `T` at its measured width `w` (cores) and memory: the
  learn medians for the second run, the K sets for the first (`sim.labels`).
- Running gates share the cores by water-filling: equal shares, each capped at its own width.
  When the widths add up to more than 10 cores, every gate runs at `eta` of its share (0.80 in
  the second run, fitted by `calib.py` on `sem-pilot`; 0.95 in the first, which `calib.py` does
  not fit). Above 8.5 GiB of summed memory a paging penalty `1 + beta × (excess share)` slows
  everything (`beta` = 2.05, fitted on `tune-S2`).
- K is coreslot's admission reduced to memory: a gate starts when its learned memory fits a room
  of 6.4 GiB, or when nothing runs, and a later gate may pass the head only when its typical time
  is no longer than the head's expected wait. S is N slots. Nothing models the watchdog, the
  growth of swap or thermal throttling.

## Validation

`validate.py` replays every set recorded before the counted pairs, starting each gate at its
observed start, and compares the predicted mean time to verdict and makespan with the measured
ones (`val_all.txt`; rows marked `CALIB` were used for fitting).

- **Where it fits:** within about 5 % on every heavy K set of both runs and on S1 to S3 (S3's
  makespan is 5 % low).
- **Where it fails:**
  - On the light mix it predicts 20 to 33 % faster than measured: two pytest runs on a machine
    with room were slower than water-filling allows.
  - On the first run's F sets, which paged hard, its one prediction (97.0 s) missed the measured
    sets by −20 % to +27 %. Paging there is not a smooth penalty.
- **Not covered:** the counted pairs ran with staggered arrivals, and their records do not show
  when tsc got its S slot, so `validate.py` skips them.

`policies.py` runs every policy over every arrival order of each mix (`pol.txt`: the mean, its
range, makespan, ratios to S1 and to K, and the observed order). Rows beyond S1 and K are
hypothetical admission rules, more speculative still.

## Running it

Python 3, standard library only; it reads `record/` and `record-2/` from the directory named by
`AIR_RECORDS` (a checkout of [coreslot-evidence](https://github.com/core-slot/coreslot-evidence)'s
`air-v04/`), or from `..` without it:

```bash
cd evidence/air-v04/model
python3 calib.py                                        # eta and beta fits
python3 validate.py 0.80 0.95 2.05 8.5 all > val_all.txt # eta run 2, eta run 1, beta, M_ok GiB, paging mode
python3 policies.py > pol.txt
```

Both outputs, regenerated from these files and records, are byte-identical to the ones the model
produced before the counted pairs ran.
