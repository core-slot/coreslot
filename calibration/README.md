# Calibration

The watchdog stops the newest jobs when memory costs them real time. This harness runs a memory hog
under the branch's own `coreslot run` in observe mode, logs what the watchdog sees at every sample,
and scores candidate thresholds against how fast the hog actually ran. The yellow and red points
were chosen on its records; the paging gate is a round value the records cannot tell from its
neighbours, and no other default is calibrated ([what each rests
on](#what-the-defaults-rest-on)).

Needs macOS, Python 3 and Go. It reads nothing that needs privileges.

## What is here

| file | what it does |
|---|---|
| `hog.py` | Grows hot memory in steps (`--step-mib` every `--hold-s`), rewrites a byte in a random page of all it holds from `--threads` worker processes (not threads, which would contend on the interpreter lock), and logs the summed touches per second every second; the workers are its children, so the measured tree is the hog and its workers, and they die with it. Stops itself when its speed stays under 30 % of its first step's for 5 s, at `--max-gib`, or after `--seconds`. |
| `cpu_control.py` | The control: busy loops (default twice the number of cores) for `--seconds` and no memory pressure. The watchdog must stay calm here. |
| `run.sh` | One pass: builds `coreslot` from this checkout into a temporary directory, gives it a temporary state directory (`CORESLOT_STATE_DIR`) whose `config.toml` sets `[watch] mode = "observe"` and `sample_log = true`, runs the hog or the control under it, stops the workload when swap used grows 6 GiB from the start, and writes the record. Every pass is preceded by a 30 s warm-up of its workload, and a ramp is accompanied by an observer job (below). |
| `analyse.py` | Joins the watchdog's `watch.jsonl` with the workload's `work.jsonl`, puts each sample into a speed band, and prints the per-band excess, the samples where its replay of the levels at the chosen values differs from the level the watchdog logged, and a scoring table per candidate value. |
| `cost.py` | The watchdog's own CPU cost: N concurrent `coreslot run --restart=yes -- sleep S`, each run's own CPU time over the window, as a share of one core ([below](#the-watchdogs-own-cost)). |
| `testdata/` | A tiny record for the tests. |
| `records/` | Where `run.sh` puts its records; the committed ones are the passes the yellow and red points were chosen on, and one measurement of the watchdog's own cost ([`records/README.md`](records/README.md)). |

## Running it

Quit other work first: the numbers are only as quiet as the machine. One pass at a time; `run.sh`
refuses to start while a process of an earlier pass is alive, and says which.

```bash
calibration/run.sh --dry ramp --threads 8     # print what would run; starts nothing
calibration/run.sh control                    # 60 s of busy loops, after a 30 s warm-up of them
calibration/run.sh ramp                       # a ramp with one worker
calibration/run.sh ramp --threads 8           # the many-worker check
calibration/run.sh ramp --after-s 120         # observe 120 s after the hog stops (default 90)
calibration/run.sh ramp --slow-s 40           # hold the cliff for 40 s (--slow-pct N: the speed floor, default 30)
python3 calibration/analyse.py calibration/records
```

Every pass starts with a warm-up: the workload runs for 30 s under the same label, in the same state
directory (the hog at 0.5 GiB, or the busy loops), and ends normally. The run log then holds one
counted run of the label, so the pass is scored against the label's learned normal share, the way the
shipped code learns it (the median `tick_share` of the label's runs); a label with no history is no part of the excess, so a pass without the
warm-up would measure nothing, and a control would show no false alarm because it measured nothing.
`meta.json` gives that share as `warmup_share`. A ramp then starts an observer,
`coreslot run --restart=yes --label calibration-observer -- sleep ...`, in the same state
directory: as the oldest hold it is never a victim, it has no history so it is no part of the excess, and it keeps the
sampling going for `--after-s` seconds after the hog stops, which is what `calm_s` is scored on.

A pass stops itself by the hog's own rules and, as a second guard from outside, when swap used has
grown 6 GiB from the start of the pass (a Mac may already carry tens of GiB of swap, so the limit is
relative) or after two hours. Every process a pass starts is stopped by the pid it recorded, at the
end and on INT, TERM or HUP, and the busy loops leave on their own when their parent dies. If
anything is left, `run.sh` prints the pids; stop them by pid.

A record directory holds `watch.jsonl` (one JSON line per machine sample), `work.jsonl`, and
`meta.json` (machine model, OS version, core count, memory, worker count as `threads`, the warm-up's
share, `after_s`, swap at the start, the `coreslot` build). Commit the records you take with the analysis you draw from them.

## Reading the analysis

Each watchdog sample inside a run gets the hog's mean speed over the sample's period, relative to
the median of its first step, and falls in a band:

| band | meaning |
|---|---|
| `full` | 90 % of the reference speed or more |
| `slowing` | 50 to 90 % |
| `slow` | 30 to 50 %, or under 30 % with no paging |
| `cliff` | under 30 % with paging, or decompressions at 100 000/s or more |
| `control` | a sample of the busy-loop control |

Slowdown alone is not the cliff: a single thread touching a large heap slows from its own cache
misses with no paging at all, so a slow sample counts as cliff only with the kernel paging.

The tables then show, per candidate value, how many samples of each band it would fire on and, in
the verdict column, in how many cliff bands it fires and in which pre-cliff or control bands it
also fires. A value is justified by a property two candidates score differently on, for example:
over the committed records `red_points = 40` fires in no full-speed or slowing sample, while 30
fires in 5 slowing samples of one Mac. The yellow and red tables replay the detector's rules; each
record's section lists every sample where that replay, at the chosen values, differs from the level
the watchdog logged, so a replay that drifts from the code shows. The committed records were logged
by a detector at `yellow_points` 10 and `red_points` 30, so the replay at the shipped 20 and 40
differs from them where those values matter; `--yellow-points 10 --red-points 30` replays each with
no mismatch. `red_samples` names the band in which
each run first turns red; `calm_s` counts the ramps whose logged level, once green after the hog
stopped, stayed green for that long. The floor is not calibrated.

Notes for reading numbers:

- Every pass uses a fresh state directory. The label's history is the warm-up's one run and the
  stall baseline is its measured share; `analyse.py` names that share for each run, control included.
- `calm_s` can only be established as far as the observer watched: green for N seconds needs the time
  to turn green plus N inside `--after-s`.
- A machine that is busy for other reasons shows paging in the `full` band; the gate and the cliff
  both read the same counters. Repeat a pass that looks noisy rather than averaging it in.
- Linux is not calibrated.

```bash
python3 -m unittest discover calibration
```

## What the defaults rest on

Counts are samples of the ten committed passes ([`records/README.md`](records/README.md) says how
they were taken and what they cannot show): 339 full-speed, 608 slowing, 10 slow and 202 cliff
samples of eight ramps, and 74 samples of two controls. The points were chosen on these same
records, with no held-out set: they are fitted to them, not validated on others.

| default | status | what the records show |
|---|---|---|
| `yellow_points = 20` | chosen on the records | Held yellow or red by 1 of 339 full-speed samples, 31 of 608 slowing, 9 of 10 slow, 194 of 202 cliff and 0 of 74 control ones. The one full-speed sample is an Air sample (E 9.6), yellow at 10, 20 and 30 alike. At 10 and 15 one Pro full-speed sample (E 7.0) is held too; at 20 none is. 20 and 30 differ only in the slowing (31 and 15), slow (9 and 7) and cliff (194 and 193) samples held, so 20 over 30 is a judgement. |
| `red_points = 40` | chosen on the records | With the gate open, E reaches it in 0 of 339 full-speed, 0 of 608 slowing, 4 of 10 slow, 185 of 202 cliff and 0 of 74 control samples; 30 reaches it in 5 slowing samples of the Pro. Seven of the eight ramps have such cliff samples; the eighth, the Pro's `ramp8`, reached no memory cliff. |
| `red_samples = 10` | not calibrated | The reason is the rule that red needs danger held for tens of seconds: 10 samples at a 2 s period are about 20 s. The records allow 3 to 15: replays at 3, 5, 10 and 15 fire first in a cliff sample and in none of the 74 control samples (3 also fires in the Pro's `ramp1` cliff, which lasted 12 s); at 20 two of the three Air ramps never turn red. At 10, red came 20 to 52 s after the first cliff sample in the six ramps that turned red (Air 28 to 52 s, Pro 20 to 30 s); contiguous cliff stretches of about 24 s (Air `ramp8`) and 22 s (Pro `long1`) carry no red. |
| `gate_pages_s = 1000` | chosen, not calibrated | The records cannot discriminate it: `analyse.py` puts a sample in the cliff band only when paging is at least 1000 per second, so the gate is open in every cliff sample by construction. Replays at 1000, 3000 and 10 000 give the same levels in all ten passes. The gate is open in 22 % of full-speed, 37 % of slowing and 45 % of control samples at 1000 (84 % of the Air's control, 5 % of the Pro's), and in 5 %, 17 % and 0 % at 10 000. |
| `calm_s = 30` | not calibrated | Seven of the eight ramps turned green 45 to 58 s after the hog stopped, the eighth (the Pro's `ramp8`) after 3 s, and all stayed green to the end of the record (at least 92 s). The 45 to 58 s is the detector's own clearing (15 samples below yellow, the bucket draining) with only the observer job left, so E is 0 by construction: it says nothing about how long memory takes to recover. No `calm_s` from 15 to 90 is discriminated. |
| `floor = 2G`, 256 MiB in 20 s | not calibrated | A round value. No sample of the ten passes has the level floor (1602 samples: 1249 green, 222 yellow, 131 red), and a pass stops when swap used has grown 6 GiB. |
| `spacing_s = 3`, `post_kill_s = 15` | not calibrated | Every pass ran in observe mode: the kill, the restart, the spacing and the calm wait never ran on a real machine. |
| busy floor, 10 % of a period | not calibrated | Chosen so that idle polling jobs, mostly system time, carry no excess; nothing in the records tests it. |
| `reserve = 1G` | not calibrated | The room at the cliff was 0.9 to 1.0 GiB at the median on the Air and 3.5 to 4.0 GiB on the Pro, and no in-run sample had a negative room at 1 GiB. With 1 GiB more it would have been negative in 11 full-speed, 16 slowing and 62 cliff samples, all the Air's; with 2 GiB more in every sample of the Air's control. That bounds the reserve; it does not choose it. |

Linux is not calibrated at all.

## The watchdog's own cost

The watchdog is meant to cost under 1 % of one core per run. `cost.py` starts N
`coreslot run --restart=yes --mem 64M -- sleep 120`, for N = 1, 4 and 8 in turn, waits until every run has started its command, reads each
`coreslot` process's CPU time with `ps` (the process's own: its `sleep` is another process), and
divides the difference over a window in which all N tick by the window's wall time.

```bash
go build -o /tmp/coreslot ./cmd/coreslot
python3 calibration/cost.py /tmp/coreslot --seconds 120 --runs 1 4 8
```

Measured on the Air (`Mac16,12`, 10 cores, not idle: its load average was about 3) on 2026-11-02, in
the record `records/Mac16,12-27.2-2026-11-02-cost` (`cost.jsonl` has every run's reading, `meta.json`
the checkout it was built from), over windows of 114 s in which all N runs ticked:

| concurrent runs | CPU per run, share of one core |
|---|---|
| 1 | 0.29 % |
| 4 | 0.17 % to 0.24 % |
| 8 | 0.11 % to 0.32 % |

The highest is 0.32 % of one core, under the 1 % it is meant to stay below. `ps` reports CPU time to 10 ms, which is
0.009 % of a 114 s window. The window leaves out each run's first 3 s and its waiting in the queue;
one machine, one build and one sitting, so it bounds the cost on this Mac and says nothing of other
machines.
