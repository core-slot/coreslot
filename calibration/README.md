# Calibration

The watchdog stops the newest jobs when memory costs them real time. Its thresholds (yellow and red
points, red samples, the paging gate, the calm window, the reserve) are set from runs of
this harness, not from a model: it runs a memory hog under the branch's own `coreslot run` in observe
mode, logs what the watchdog sees at every sample, and scores candidate thresholds against how fast
the hog actually ran.

Needs macOS, Python 3 and Go. It reads nothing that needs privileges.

## What is here

| file | what it does |
|---|---|
| `hog.py` | Grows hot memory in steps (`--step-mib` every `--hold-s`), rewrites a byte in a random page of all it holds from `--threads` worker processes (not threads, which would contend on the interpreter lock), and logs the summed touches per second every second; the workers are its children, so the measured tree is the hog and its workers, and they die with it. Stops itself when its speed stays under 30 % of its first step's for 5 s, at `--max-gib`, or after `--seconds`. |
| `cpu_control.py` | The control: busy loops (default twice the number of cores) for `--seconds` and no memory pressure. The watchdog must stay calm here. |
| `run.sh` | One pass: builds `coreslot` from this checkout into a temporary directory, gives it a temporary state directory (`CORESLOT_STATE_DIR`) whose `config.toml` sets `[watch] mode = "observe"` and `sample_log = true`, runs the hog or the control under it, stops the workload when swap used grows 6 GiB from the start, and writes the record. Every pass is preceded by a 30 s warm-up of its workload, and a ramp is accompanied by an observer job (below). |
| `analyse.py` | Joins the watchdog's `watch.jsonl` with the workload's `work.jsonl`, puts each sample into a speed band, and prints the per-band excess, the samples where its replay of the levels at the chosen values differs from the level the watchdog logged, and a scoring table per candidate value. |
| `testdata/` | A tiny record for the tests. |
| `records/` | Where `run.sh` puts its records; the committed ones are the passes the shipped defaults rest on ([`records/README.md`](records/README.md)). |

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
shipped code learns it; a label with no history is no part of the excess, so a pass without the
warm-up would measure nothing, and a control would show no false alarm because it measured nothing.
`meta.json` gives that share as `warmup_share`. A ramp then starts an observer,
`coreslot run --label calibration-observer -- sleep ...`, in the same state directory: as the
oldest hold it is never a victim, it has no history so it is no part of the excess, and it keeps the
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
