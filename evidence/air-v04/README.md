# Acceptance run on a 16 GB MacBook Air

Four coding agents run their gates at the same moment on one laptop, once with nothing in front of
them (F) and once with each command under `coreslot run --label <label> --` (K), in alternating
pairs. The question is whether the memory guard keeps the machine out of memory pressure without
costing the agents their time or their verdicts. A watchdog demo then checks, once, that a real
stop and restart happens on the shipped rules and that every stopped job still completes.

The stand is public code at pinned commits:

| agent | gate | source | alone |
|---|---|---|---|
| A1, A2 | `npm run --silent typecheck-client` | VS Code 1.142.0 (`d956e8a`) | about 8.3 GiB, 10 s |
| A3 | `ruff check fastapi tests`, then `pytest -n auto --dist loadgroup` | FastAPI 0.156.2 (`44cc742`) | about 3 GiB, 32 s |
| A4 | `pytest -n auto test/orm` | SQLAlchemy rel_2_1_5 (`4fbf72c`) | about 3 GiB, 30 s |

## The phases

1. **learn** (about 5 min): each gate runs alone under coreslot three times, in the run's own state
   directory, so that every label has a learned need and a normal share before the first K set.
   These runs are also the false-failure baseline: a gate whose alone runs all passed is stable,
   and a non-zero exit of a stable gate in a set is a false failure.
2. **pilot** (about 4 min): one F set and one K set, exploratory. The analysis prints it apart and
   no test uses it. After it, the owner writes the criterion into `criterion.md` beside this file
   and commits it before the pairs start; the analysis computes the candidate statistics and does
   not read the criterion.
3. **pairs** (about 20 min): five pairs in ABBA order, F K K F F K K F F K. Every set starts the
   four agents at once, after a cooldown that waits until the memory pressure level is normal and
   swap used has held still (within 64 MiB) for 5 s, at least 15 s and at most 120 s; the record
   says how long it took and whether it gave up.
4. **demo** (about 6 min, n = 1, a functional check): below.

Per set the record holds each agent's start, end, time to verdict and exit; the makespan; the
memory pressure level and swap used every second (`sysctl kern.memorystatus_vm_pressure_level`, 1
normal, 2 warn, 4 critical; `vm.swapusage`); the `vm_stat` swapouts and pageouts over the set; the
thermal state at its start; and for K every run-log line the set added (`waited_s`, `ran_s`,
`mem_peak_kib`, `stops`), each command's stderr with coreslot's own lines, and the watchdog's
samples.

### The demo

`calibration/hog.py` holds a fixed amount of hot memory and rewrites random pages of it. The demo
first teaches the label `demo-hog` a small need: a 256 MiB hog runs alone for 30 s, so the run log
holds one counted run with a memory peak near 280 MiB and a `tick_share`, the label's normal share
(retried up to three times if no tick had the paging gate closed). Then three hogs start a second
apart under `coreslot run --label demo-hog`, each mapping 3/8 of the machine's memory at once
(6 GiB on 16 GiB) and holding it for 90 s, with the hog's own slowness stop turned off. Admission
lets all three in on the learned 280 MiB need; together they hold 18 GiB of hot memory on a 16 GiB
machine, so every touch of a compressed page decompresses it, the paging gate opens and the hogs'
share of time lost to memory rises far above their learned normal share. That is the excess the
red level counts: once it has held at or over `red_points` for about `red_samples` samples, a red
round stops the newest hog, never the oldest, and another round after `post_kill_s` takes the next
one if the machine is still red. A stopped hog re-queues at its first arrival with its peak as its
need, and comes back when memory is calm and has room for it, which on this machine is when the
hog before it has finished. All thresholds are the shipped defaults; the state directory's
`config.toml` only turns on the sample log. From outside, the demo stops everything when swap used
grows 6 GiB from its start or after 600 s, and the record says which.

## Reproducing it

Quit other work first. Build the stand once (git, node with npm, uv and the network; the measured
runs use no network), and build coreslot from the checkout under test outside the repository:

```bash
STAND=/path/to/stand.noindex          # the .noindex suffix keeps Spotlight out
evidence/air-v04/setup.sh "$STAND"
go build -o /tmp/air-v04/coreslot ./cmd/coreslot
run() { python3 evidence/air-v04/run.py --coreslot /tmp/air-v04/coreslot --stand "$STAND" --out evidence/air-v04/record "$@"; }
```

Then, in this order:

```bash
run --dry pairs                      # any phase with --dry prints what it would run and starts nothing
run learn                            # about 5 min
run set F --tag pilot                # about 2 min
run set K --tag pilot                # about 2 min
python3 evidence/air-v04/analyse.py evidence/air-v04/record
# write evidence/air-v04/criterion.md by hand and commit it
run pairs                            # about 20 min; --first N --last N runs part of it
run demo                             # about 6 min; --hog-gib G sets the hogs' size
python3 evidence/air-v04/analyse.py evidence/air-v04/record
```

`run.py` refuses to start while a process it started for an earlier set is alive (it lists them
by pid), refuses a K set while the budget holds a live hold or a waiting ticket, and refuses a
stand that is not at its pins. Every process it starts is stopped by its process group at exit
and on INT, TERM or HUP. An interrupted set leaves its directory without a record line; delete
that directory to run the set again. `--smoke` swaps the gates for short Python allocations and
skips the pin check, to try the harness without loading the machine.

## Files

| file | what it holds |
|---|---|
| `setup.sh` | builds the stand at its pins |
| `run.py` | the phases above; stdlib only |
| `analyse.py` | the per-pair table (warn s, critical s, swap growth, makespan, mean time to verdict, false failures, and for K the summed wait and the stops), one-sided sign tests of K against F for warn seconds, false failures and mean time to verdict, the pilot apart, and the demo's verdict |
| `test_air.py` | unit tests of the analysis and of the pure parts of `run.py` (`python3 -m unittest` here) |
| `record/meta.jsonl` | one line per invocation: machine model, macOS version, memory, `coreslot version` and its SHA-256, the commit of this repository, the stand's commits |
| `record/learn.jsonl`, `record/learn/` | each alone run, with its run-log line, and its output |
| `record/sets.jsonl`, `record/sets/<tag>-<arm>/` | one line per set, and each gate's output and stderr (`watch.jsonl` for K) |
| `record/demo/` | `demo.json`, the hogs' own logs, each run's stderr and `watch.jsonl` |
| `record/state/`, `record/demo/state/`, `record/live.json` | the run's coreslot state and the processes it started; not committed |
