# Calibration records

The passes the watchdog's shipped defaults rest on, taken on 2026-11-02 on two Macs: `Mac16,7` is a
MacBook Pro M4 Pro with 48 GiB, `Mac16,12` a MacBook Air M4 with 16 GiB. A directory is named
`<model>-<os>-<date>-<pass>` and holds the pass's `watch.jsonl`, `work.jsonl` and `meta.json` as
`run.sh` wrote them.

| pass | what ran | build |
|---|---|---|
| `control` | the busy-loop control, on each Mac | Pro: binary of commit 1042c1d; Air: 6b8b599 |
| `ramp1`, `ramp2` | a ramp with one worker, on each Mac | Pro: binary of 1042c1d; Air: 6b8b599 |
| `ramp8` | a ramp with 8 workers, on each Mac | Pro: binary of 1042c1d; Air: 6b8b599 |
| `long1`, `long2` | a one-worker ramp with `--slow-s 40`, on the Pro only | binary of 1042c1d, harness of commit 6cc4823 |

The detector logic is the same in all of them; the only later Go change moved where each job's
learned share is stored. They were logged by a detector at `yellow_points` 10 and `red_points` 30,
the values before these records set the defaults.

`Mac16,7-27.0.1-2026-11-02-ramp8` reached no memory cliff: its slowdown came from CPU contention by
other work on the machine. It is kept as a negative.

To recompute the tables:

```bash
python3 calibration/analyse.py calibration/records/*
```
