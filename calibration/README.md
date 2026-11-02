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
| `hog.py` | Grows hot memory in steps (`--step-mib` every `--hold-s`), rewrites a byte in a random page of all it holds from `--threads` threads, and logs its touches per second every second; it starts no process, so the measured tree is the hog alone. Stops itself when its speed stays under 30 % of its first step's for 5 s, or at `--max-gib`. |
| `cpu_control.py` | The control: busy loops (default twice the number of cores) for `--seconds` and no memory pressure. The watchdog must stay calm here. |
| `run.sh` | One pass: builds `coreslot` from this checkout into a temporary directory, gives it a temporary state directory (`CORESLOT_STATE_DIR`) whose `config.toml` sets `[watch] mode = "observe"` and `sample_log = true`, runs the hog or the control under it, stops the workload when swap used grows 6 GiB from the start, and writes the record. |
| `analyse.py` | Joins the watchdog's `watch.jsonl` with the workload's `work.jsonl`, puts each sample into a speed band, and prints the per-band excess, the samples where its replay of the levels at the chosen values differs from the level the watchdog logged, and a scoring table per candidate value. |
| `testdata/` | A tiny record for the tests. |
| `records/` | Where `run.sh` puts its records. |

## Running it

Quit other work first: the numbers are only as quiet as the machine. One pass at a time; `run.sh`
refuses to start while a process of an earlier pass is alive, and says which.

```bash
calibration/run.sh --dry ramp --threads 8     # print what would run; starts nothing
calibration/run.sh control                    # 60 s of busy loops
calibration/run.sh ramp                       # a ramp with one thread
calibration/run.sh ramp --threads 8           # the many-threaded check
python3 calibration/analyse.py calibration/records
```

A pass stops itself by the hog's own rules and, as a second guard from outside, when swap used has
grown 6 GiB from the start of the pass (a Mac may already carry tens of GiB of swap, so the limit is
relative) or after two hours. Every process a pass starts is stopped by the pid it recorded, at the
end and on INT, TERM or HUP, and the busy loops leave on their own when their parent dies. If
anything is left, `run.sh` prints the pids; stop them by pid.

A record directory holds `watch.jsonl` (one JSON line per machine sample), `work.jsonl`, and
`meta.json` (machine model, OS version, core count, memory, threads, swap at the start, the
`coreslot` build). Commit the records you take with the analysis you draw from them.

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
`red_points = 30` fires in every ramp's cliff band and in no pre-cliff band; 20 also fires in the
`slow` band of one ramp. The yellow and red tables replay the detector's rules; each record's
section lists every sample where that replay, at the chosen values, differs from the level the
watchdog logged, so a replay that drifts from the code shows. `red_samples` names the band in which
each run first turns red; `calm_s` counts the ramps whose logged level, once green after the hog
stopped, stayed green for that long. The floor is not calibrated.

Notes for reading numbers:

- Every pass uses a fresh state directory, so the label has no history and the stall baseline is the
  unknown-command default. That is what a command seen for the first time gets.
- A machine that is busy for other reasons shows paging in the `full` band; the gate and the cliff
  both read the same counters. Repeat a pass that looks noisy rather than averaging it in.
- Linux is not calibrated.

```bash
python3 -m unittest discover calibration
```
