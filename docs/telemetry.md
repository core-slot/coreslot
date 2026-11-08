# Telemetry files

With `[telemetry] enabled = true` coreslot keeps a local record of what the machine and each run
did in `$S/telemetry/`. Nothing leaves the machine. `sample_log = true` alone writes
`samples.jsonl` and nothing else.

Each file is JSON lines. The tick that writes samples rotates `samples.jsonl` and `events.jsonl`,
each to one older generation (`samples.1.jsonl`, `events.1.jsonl`), at 256 MiB. A write that fails
costs its line. A reader skips a line it cannot parse.

## `samples.jsonl`

One line per machine sample, written by whichever run takes the sample.

| key | meaning |
|---|---|
| `v` | the schema of the line, 1 |
| `time_ns` | when the sample was taken, ns since the epoch |
| `t_ns` | the host reading's clock, ns since boot, counting sleep; the detector's times are on it |
| `awake_ns` | the same clock without sleep |
| `level` | the detector's level: `green`, `unknown`, `yellow`, `red`, `floor` |
| `excess` | the stall excess E, in points |
| `bucket` | the red bucket |
| `gate` | whether the paging gate was open |
| `decomp_s` | decompressions per second |
| `swapins_s` | swap-ins per second |
| `psi_some_us_s` | PSI `some` in microseconds per second; null where the platform has no PSI |
| `swap_used_mib` | swap in use |
| `swap_free_mib` | swap free |
| `vm_free_mib` | free space on the volume that holds the swap, on macOS; 0 elsewhere |
| `room_mib` | the admission room; 0 when `headroom_ok` is false |
| `live_holds` | every live hold, running or not; `jobs` lists only those that ticked recently |
| `headroom_mib` | the headroom admission read |
| `headroom_ok` | whether `headroom_mib` is a reading; when false, its 0 is not one |
| `reserve_mib` | the reserve admission used |
| `jobs` | the running, freshly ticked holds: `hold`, `label`, `arrival_ns`, `need_mib`, `footprint_mib`, the time deltas `d_user_ns`, `d_sys_ns`, `d_page_wait_ns`, `share` (null when the job reported no time), `normal_share`, `learned`, `victim`, `cpu_ns`, `typical_cpu_s`, `typical_s`, `stall_ewma` |
| `stop` | present on the sample that wrote a stop round: the holds it lists |
| `candidates`, `short_mib`, `kept`, `no_round` | why a round stopped or kept whom, or why none was written; see `stopped` and `kept` |

A sample whose reading failed reports `level` `unknown`, zero readings and `headroom_ok` false.

## `events.jsonl`

One line per event of a run, written by that run. A run's events follow the switch as it read it
at its start, even if the switch changes while it runs. Samples follow whichever process ticks.
Every line has:

| key | meaning |
|---|---|
| `v` | the schema of the line, 1 |
| `e` | the event |
| `time_ns` | when it was written, ns since the epoch |
| `up_ns` | the clock since boot that counts sleep; slept = delta `up_ns` less delta `awake_ns` |
| `awake_ns` | the same clock without sleep |
| `lost` | lines not written since the last line that was |
| `run` | the run, the same across its attempts |
| `attempt` | the attempt, from 1; above 1 is a restart |
| `n` | the run's line number |
| `label` | the run's label |

### `enqueue`

Written once the ticket is published; an enqueue that fails writes nothing.

| key | meaning |
|---|---|
| `ticket` | the ticket's name in `queue/` |
| `need_mib` | what it asks for |
| `need_source` | where the need came from: `flag`, `pin`, `learned`, `cold`, `container`, `restart_peak` |
| `need_before_mib` | with `restart_peak`: the need before the peak replaced it |
| `real_memory` | a container request, judged against the real free memory |
| `typical_s` | the label's typical run time, 0 unknown |
| `oversized` | the need is above what the machine has above its reserve |
| `timeout_s` | `--timeout`, 0 for none |
| `on_timeout` | `run` or `fail` |
| `version` | coreslot's version |
| `mem_mib` | the machine's memory |
| `mode` | `[watch] mode`, `enforce` or `observe`; absent when the configuration cannot be read |
| `reserve_mib` | the reserve; absent likewise |
| `watch_config` | the effective `[watch]` thresholds: `yellow_points`, `red_points`, `red_samples`, `gate_pages_s`, `floor_mib`, `calm_s`, `post_kill_s`, `spacing_s`; absent likewise |
| `nested_miss` | why an inherited `CORESLOT_HOLD` did not count: `malformed`, `not_ancestor`, `dead`, `unresolved`; absent without one |
| `parent_hold` | for `--independent` inside a live parent: the parent's hold |

### `waiting`

Written with each line a person reads: at the start of the wait and every 30 s after, from the
snapshot that line reads. A failed snapshot leaves out the keys from `headroom_mib` on.

| key | meaning |
|---|---|
| `reason` | what the last step found: `behind_ticket`, `section_busy`, `room_short`, `behind_head`, `watch_level`, `watch_spacing`, `restart_stale`, `restart_level`, `restart_calm`, `restart_margin` |
| `reason_s` | seconds waited on each reason since the last line |
| `evals` | admission attempts so far |
| `max_gap_ms` | the longest time between two steps since the last line |
| `ahead` | the ticket examined ahead of it; absent for the head |
| `no_pass` | why it may not pass the head: `no_typical`, `headroom_unknown`, `no_holds`, `wait_unknown`, `watch_unreadable`, `too_long`; absent otherwise |
| `w_s` | the expected wait a pass was judged on, when computed |
| `calm_left_s` | with `restart_calm`: seconds of calm still needed |
| `headroom_mib`, `headroom_ok`, `reserve_mib` | as on a sample |
| `room_mib` | the room the person's line shows: at least 0; for a container waiter the real free memory |
| `level` | the detector's level; absent with no sample |
| `read_age_ms` | the age of that sample; absent with no sample |
| `holders` | up to 16 live holds, those with the most left to grow into first: `hold`, `need_mib`, `footprint_mib`, `child_exited`, `owner_exited`, `sample_age_ms` (absent when never sampled) |

### `admitted`

| key | meaning |
|---|---|
| `hold` | the hold's name |
| `waited_s` | the attempt's wait, the run log's `waited_s` |
| `reason_s` | seconds per reason since the last `waiting` |
| `last_reason` | the reason of the last step before the admission; empty for a run admitted at once |
| `headroom_mib`, `headroom_ok`, `reserve_mib`, `room_mib` | what the decision read |
| `watch_gate` | what the watch state could tell: `checked`, `stale`, `unknown`, `no_holds` |
| `w_s` | the expected wait, when computed |

### A wait that ends without a start

Each of these has `waited_s`, `reason_s` (seconds per reason since the last `waiting`) and
`last_reason`, as `admitted` has them. A restart's lines have `attempt` above 1. An enqueue that
fails writes none of them.

| event | added keys | meaning |
|---|---|---|
| `interrupted` | `signal`, the signal's name | a signal ended the wait, with or without a grant in hand |
| `cancelled` | | `coreslot cancel` ended the wait |
| `refused` | `reason`, `no_wait` or `strict` | `--no-wait` was not admitted at once, or `--strict` met a state error; not written when FORMAT is what could not be read |
| `timeout` | `on_timeout`, as on `enqueue` | `--timeout` ran out; a restart's wait never runs outside the budget, whatever this says |

### `ended`

Written after the run-log line, when the command ran and was reaped.

| key | meaning |
|---|---|
| `hold` | the hold's name; absent without one |
| `record` | the run-log line of the attempt, with `fail_open` emptied |
| `fail_open` | why the run was outside the budget: `timeout`, `state_error`, `parent_unbudgeted`; absent otherwise |
| `signal` | the signal that killed the command, by name; an `exit 137` of the command's own is not one |
| `stopped_by` | `watchdog_red` or `watchdog_floor` when a stop round killed it, `outside` when a SIGKILL did that coreslot did not send; absent otherwise |
| `mean_footprint_mib` | the mean of the tree's 1 s readings, in whole MiB; absent for a run with no reading |

### `outside` and `live`

`outside` is written by a run that holds no ticket when it starts its command, or whose ticket is
gone: a pass-through inside a parent's reservation, a fail-open run with its unbudgeted hold
published, and a timed-out run with `--on-timeout run`. It is followed by `ended`, except for a
pass-through killed with its parent's group by a stop round.

| key | meaning |
|---|---|
| `kind` | `pass_through`, `fail_open`, `timeout_run` |
| `reason` | `parent_hold`, `parent_unbudgeted`, `state_error`, `timeout`; a closed code, never the reason's text |
| `need_mib` | the run's need |
| `parent` | a pass-through's parent hold; absent otherwise |

`live` is written every 30 s by a run outside the budget (not by a pass-through, whose memory is
its parent's), which no sample sees: `footprint_mib` is its tree's footprint at that listing.

### When nothing more is written

A recorder is made only once FORMAT was read and understood: in the section of a successful
enqueue or of a published unbudgeted hold, or by a pass-through itself. A run that then sees a
FORMAT it cannot read (at a hold update, a tick, the run-log append, or a pass-through's check
before its last line), or a failed append or release, writes nothing more, even if FORMAT comes
back, and hands its command no `CORESLOT_TELEMETRY`. A fail-open run with no published hold writes
nothing.

### `not_started`

The command was never started: `reason` is `exec` when it could not be started, or the name of the
signal that came between the admission and the start.

### `stopped` and `kept`

Thin markers: the numbers behind them are on the `samples.jsonl` line of the same round.
`stopped` is written when the watchdog killed the command, or, in observe mode, when it said it
would; `kept` once for the round that kept the command as nearly finished.

| key | meaning |
|---|---|
| `round` | `red` or `floor`; a keep is always `red` |
| `observe` | the round was written in observe mode, which kills nothing |
| `level` | the detector's level when the run read the round |
| `list_pos` | the hold's place in the round's list; absent on `kept`, which the round spared |
| `excess` | the stall excess E, in points |

A sample that wrote a stop round also has `candidates` (the running holds that were victims),
`short_mib` (what the round had to free) and `kept` (the holds that round spared). A red or floor
sample that wrote none has `no_round`: `post_kill` (the last round is inside `post_kill_s`),
`listed_alive` (a hold it listed still runs as a victim) or `nobody_stoppable` (a round that had
no one to list).
