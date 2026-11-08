# Telemetry files

With `[telemetry] enabled = true` coreslot keeps a local record of what the machine and each run
did in `$S/telemetry/`. It is off by default. coreslot sends nothing anywhere and opens no network
connection itself; while the record is on, a run doing container work talks to the local Docker socket, a
unix socket, to read memory ([`containers`](#containers)). `[watch] sample_log = true`, deprecated,
writes `samples.jsonl` alone and nothing else.

The directory is optional and its absence is normal: no admission reads it, and it is not part of
`FORMAT` ([`PROTOCOL.md` section 4.4](../PROTOCOL.md#44-layout)).

| file | what it holds |
|---|---|
| `samples.jsonl`, `samples.1.jsonl` | one line per machine sample |
| `events.jsonl`, `events.1.jsonl` | one line per event of a run, and the machine-wide `memory` and `containers` lines |
| `memory.stamp`, `containers.stamp` | when each machine-wide line was last claimed; no content that matters |

Each log is JSON lines. The tick that writes samples rotates `samples.jsonl` and `events.jsonl`,
each to one older generation at 256 MiB, so while anything ticks the directory stays within about
1 GiB; runs that end within 2 s and pass-throughs do not tick, and alone rotate nothing. A write
that fails costs its line. A reader skips a line it cannot parse.

The record holds labels (an auto-label copies up to two leading arguments of the command and the
repository's directory name, as the run log does), process names, which are executable file names,
and container image names. A `waiting` line's `holders` carry hold names, which embed up to 32 bytes
of a label. It holds no command line, environment or whole path, and every reason in it is a closed
code.

## Clocks

Every line has `time_ns`, the wall clock, which can step. A run's lines and the machine-wide ones
also have `up_ns`, since boot and counting sleep, and `awake_ns`, the same without sleep; a sample
has `t_ns` and `awake_ns` from the host reading. Between two lines, the sleep is the delta of `up_ns`
less the delta of `awake_ns`, and a clock step is the delta of `time_ns` less the delta of `up_ns`.
The boot clocks compare only within one boot.

## Joining the files

- A run's lines share `run`; `attempt` tells its restarts apart and `n` orders them.
- `hold` on `admitted` and `ended` is a hold of the sample lines' `jobs` and `stop`.
- `ended.record.time_ns` is the `time_ns` of the run's line in `log/runs.jsonl`; a pass-through's
  `ended` has a `record` and no line there.
- `memory` and `containers` name no run, and a sample names only holds: join them by time, on
  `up_ns` and `t_ns`.

## What a reader may conclude

- A budgeted line in the run log with no `enqueue` was not recorded.
- An `enqueue` with no terminal line (`ended`, `refused`, `cancelled`, `interrupted`, `not_started`
  or `timeout` without a run after it) was killed or lost.
- `lost` is carried by the next line of the same process, and dies with it.
- A run writes nothing more once it has seen a FORMAT it cannot read or a failed release, and a
  fail-open run with no published hold writes nothing.
- A gap in `memory` lines with no coreslot process alive is "nothing recorded", not "nothing happened".

There is no daemon: only a live coreslot process records, so with none alive nothing does.

## Switching it during a run

A run's events follow the switch as it read it at its start, even if the switch changes while it
runs. Samples follow whichever process ticks, so they can start or stop in the middle of a run.

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
| `wired`, `compressor`, `internal`, `free`, `speculative`, `purgeable`, `external` | macOS, MiB, from their own read, a moment after the paging counters: wired, compressor-held, anonymous and file-backed pages, and free ones as the host reports them, speculative pages included. Absent when the read failed |
| `pressure_level` | macOS: the kernel's memory pressure, 1 normal, 2 warning, 4 critical |
| `AnonPages`, `Shmem`, `Slab`, `SReclaimable`, `PageTables`, `Zswap` | Linux, MiB, from `/proc/meminfo`; a key the kernel does not report is absent |
| `oom_kill`, `psi_full_us` | Linux: the count of OOM kills since boot (`/proc/vmstat`) and the PSI `full` stall, in total µs since boot, where the kernel reports it |
| `candidates`, `short_mib`, `kept`, `no_round` | why a round stopped or kept whom, or why none was written; see `stopped` and `kept` |

A sample whose reading failed reports `level` `unknown`, zero readings and `headroom_ok` false.

## `events.jsonl`

One line per event of a run, written by that run. Every line has:

| key | meaning |
|---|---|
| `v` | the schema of the line, 1 |
| `e` | the event |
| `time_ns` | when it was written, ns since the epoch |
| `up_ns` | the clock since boot that counts sleep |
| `awake_ns` | the same clock without sleep |
| `lost` | lines not written, and panics of the recorder recovered, since the last line that was; a recovered panic may have cost no line |
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

Written after the run-log line, when the command ran and was reaped. If that line could not be
appended and FORMAT is not what failed, `ended` is still written, and its `record` is then the
attempt's only copy.

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

### `memory`

A machine-wide line that names no run (no `run`, `attempt`, `n` or `label`), written on macOS once
per 30 s window by whichever process is due; two processes may each write one in a window. Nothing
is written where processes cannot be swept.

| key | meaning |
|---|---|
| `vm` | the largest footprint among the Docker VM's processes (names starting `com.docker.krun`, `com.docker.virtualization` or `com.apple.Virtualization`): `name`, `footprint_mib`, `resident_mib`, `cpu_s`, `start_ns`, whose change is a VM restart; absent when none runs |
| `top` | the five largest processes of the person by footprint: `name` (the process's registered name, not its truncated command name) and `footprint_mib` |

### `containers`

A machine-wide line that names no run, written once per 30 s window (`containers.stamp`, as for
`memory`, so two processes may each write one in a window) from the supervision of a run whose own
command is container work and which is not inside another coreslot run's command, and from nothing
else: no waiter, no pass-through and no process that inherited a non-empty `CORESLOT_HOLD` or
`CORESLOT_UNBUDGETED` ever opens the socket, so a nested `--independent` container run writes no
`containers` line. Its connection would otherwise read as its parent command's container work.

It reads the Docker socket: the unix socket `DOCKER_HOST` names, or else
`~/.docker/run/docker.sock`, `~/.docker/desktop/docker.sock` and `/var/run/docker.sock`, in that
order (the system's alone when `CORESLOT_STATE_DIR` is set, which looks up no home). One request for
the list and one per container, up to 17, read-only, under one 2 s deadline, which cannot interrupt
a connect while it resolves the socket's path; the line is written at the deadline with what was
read.

| key | meaning |
|---|---|
| `containers` | up to 16 running containers: `id` (12 characters), `image` (up to 64 bytes), `usage_mib` as `docker stats` computes it, `limit_mib` |
| `containers_total` | how many are running, read or not |
| `containers_err` | absent when all was read, else a closed code: `no_socket`, `connect`, `deadline`, `http` (a status other than 200, or a response over 1 MiB), `parse`, `tcp_host` (`DOCKER_HOST` is not a unix socket; nothing is read) |

### `not_started`

The command was never started: `reason` is `exec` when it could not be started, or the name of the
signal that came between the admission and the start.

### `stopped` and `kept`

Thin markers: the numbers behind them are on the `samples.jsonl` line of the same round.
`stopped` is written when the watchdog sent the kill, or, in observe mode, when it said it would;
a command that ended by itself first has a `stopped` and then an `ended` with no `stopped_by`.
`kept` is written once for the round that kept the command as nearly finished.

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
