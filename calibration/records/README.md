# Calibration records

The passes the watchdog's yellow and red points were chosen on, taken on 2026-11-02 on two Macs:
`Mac16,7` is a MacBook Pro M4 Pro with 48 GiB, `Mac16,12` a MacBook Air M4 with 16 GiB. A directory
is named `<model>-<os>-<date>-<pass>` and holds the pass's `watch.jsonl`, `work.jsonl` and
`meta.json` as `run.sh` wrote them. [`../README.md`](../README.md#what-the-defaults-rest-on) says what
each default rests on.

| pass | what ran | build |
|---|---|---|
| `control` | the busy-loop control, on each Mac | Pro: binary of commit e974779; Air: a646bab |
| `ramp1`, `ramp2` | a ramp with one worker, on each Mac; the Air's with `--slow-s 40` | Pro: binary of e974779; Air: a646bab |
| `ramp8` | a ramp with 8 workers, on each Mac; the Air's with `--slow-s 40` | Pro: binary of e974779; Air: a646bab |
| `long1`, `long2` | a one-worker ramp with `--slow-s 40`, on the Pro | binary of e974779, harness of a646bab |
| `cost` | the watchdog's own CPU cost (`../cost.py`), on the Air | the checkout named in its `meta.json` |

## Provenance

The builds are named by their commit in this repository, found by subject. The passes were built
from commits that a history filter rewrote before publication, so the short hashes in the metas
and in earlier notes (`1042c1d`, `6b8b599`, `6cc4823`) name no commit here: `1042c1d` is `e974779`
("conformance: the stop traces teach their victims' labels; a label with no history is never
listed"), and `6b8b599` and `6cc4823` are both `a646bab` ("calibration: run.sh ramp takes --slow-s
and --slow-pct for the scored hog, so a ramp can hold the cliff long enough to score the red
duration"). The Air's metas carry `6b8b599` in their version string. The Pro's metas read
`coreslot 0.0.0-dev`: that binary was built without version information, so its meta does not
name its commit.

`--slow-s 40` is in the metas (`slow_s`) of the Air's three ramps and of the Pro's `long1` and
`long2`; the Pro's `ramp1`, `ramp2` and `ramp8` have no `slow_s` and ran with the hog's default, 5 s
below 30 % of its speed. The Air's control has `slow_s` 0.

The detector logic is the same in all of them but for one Go change that came after, 5bb6f8a, which
moved where each job's learned share is stored. Two more changes have touched the detector since:
a machine sample is now due 1.5 s after the stored one (it was due after 2 s), and the floor is
tested on a pair of samples that cannot be compared. Neither was exercised by these records: they
hold no gap and no floor sample. The records were logged by a detector at `yellow_points` 10 and
`red_points` 30, the values before these records set the defaults.

## How the passes were taken, and what that cannot show

- Two machines, one day, one OS build each (Pro 27.0.1, Air 27.2), one control per machine (37
  samples each). Eight ramps: three on the Air and five on the Pro, of which the Pro's `ramp8`
  reached no memory cliff: its slowdown came from CPU contention by other work on the machine, so it
  is kept as a negative. Seven ramps count as cliffs (3 Air, 4 Pro).
- The records were taken when a label's normal share was learned from the warm-up's rusage, sys/(user + sys)
  over its whole process tree; the shipped code now learns it from the run's tick shares ([PROTOCOL.md](../../PROTOCOL.md)
  section 23.3). The committed `warmup_share` values are the rusage ones.
- Ten passes are committed and all ten are counted. Three earlier Pro ramps (two one-worker, one
  eight-worker) were taken before the harness learned the baseline from a warm-up and before the
  workers were processes; they are not counted and not committed.
- The thresholds moved from 10 and 30 to 20 and 40 after looking at these same records. There is no
  held-out set: the points are fitted to them, not validated on other runs or machines.
- The watchdog ran in observe mode only. The kill, the restart, the spacing and the calm wait never
  ran on a real machine.
- The control is busy loops in user mode, with the baseline learned under the same contention: it
  cannot raise a system-time share, and no pass has an I/O- or syscall-heavy job or several jobs
  with different normal shares.
- Background load was not recorded. The Pro started every pass with 21 138 to 30 664 MiB of swap in
  use (`swap_used_mib_at_start`), the Air with 3 007 to 5 515 MiB. The records show paging with nothing of ours running: the gate was open in 31 of the Air's 37 control
  samples and 2 of the Pro's 37. They do not show what else ran: the load, the other processes and
  their memory were not logged.
- Sample periods were 2.0 to 2.1 s (991 of the 1592 intervals) or 3.9 to 4.0 s (601), never between,
  and 2.6 to 3.3 s on average, because a sample was due 2 s after the stored one and a ticker that
  came a hair early waited another 2 s. A count of samples in these records is therefore 2 to 4 s
  apart, not 2 s.

To recompute the tables:

```bash
python3 calibration/analyse.py calibration/records/*
```
