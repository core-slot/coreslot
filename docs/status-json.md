# `status --json`

The document `coreslot status --json` prints. Its `"schema"` rule, what a consumer must ignore and
what raises the number are in [compatibility.md](compatibility.md#status---json). Every key is
listed below, one table per object. Times named `_ns` are Unix nanoseconds by the wall clock; `_s`
is seconds; `_mib` is MiB. Integers may be large: a reader tolerates any non-negative value (a
signed `room_mib` excepted, see `mem`). Labels are carried exactly as stored, JSON-encoded, so
escape one before printing it to a terminal.

## Top level

| key | type | meaning |
|---|---|---|
| `schema` | int | the document's schema number, 1 |
| `state_dir` | string | the state directory read |
| `state_dir_exists` | bool | false on a machine where nothing has run yet: everything else is then empty |
| `format` | int | the state directory's `FORMAT`; 0 when it is absent, unreadable or malformed |
| `format_present` | bool | `FORMAT` was read, well-formed or malformed |
| `format_understood` | bool | this client implements `format`; an absent `FORMAT` counts as understood |
| `format_error` | string | set when `FORMAT` is malformed or unreadable, otherwise empty |
| `gen` | int | the queue's generation counter |
| `reconciled_ns` | int | when the state was last reconciled |
| `config_error` | string | set when the configuration or the machine's memory could not be read, otherwise empty |
| `degraded` | object or null | the degraded-mode record; null when none is recorded |
| `section` | object | the admission section |
| `mem` | object or null | the memory budget; null when the configuration or the machine's free memory cannot be read |
| `watch` | object | the watchdog |
| `holds` | array | the live holds, in name order |
| `tickets` | array | the queue, in queue order |
| `dead_holds` | array of string | names of holds whose lock is free, awaiting reconciliation |
| `cancel_requests` | array of string | pending cancel requests (ticket ids) |
| `ignored` | object | per directory (`queue`, `holds`, `cancel`), the number of entries that do not match the grammar or whose body is malformed |
| `observed_s` | number | how long `--observe` watched the queue; 0 without it |

## `degraded`

| key | type | meaning |
|---|---|---|
| `reason` | string | why the runs fail open |
| `first_ns` | int | when degraded mode was first recorded |
| `count` | int | how many times it was recorded |
| `malformed` | bool | the record's body could not be read; degraded mode is still recorded |

## `section`

| key | type | meaning |
|---|---|---|
| `held` | bool | some client is inside the admission section now |
| `holder` | object or null | null while the section is free |

## `section.holder`

From the section's informational body. `pid` and `entered_ns` are 0 when the body is unreadable.

| key | type | meaning |
|---|---|---|
| `pid` | int | the process inside the section |
| `entered_ns` | int | when it entered |
| `held_for_s` | number | how long it has been inside |
| `alive` | bool or null | whether that process is alive (signal 0); null when `pid` is 0. A holder that is not alive is a stale record |

## `mem`

The memory budget. `free_mib` and `room_mib` are the room a new job may take: the headroom, less
`reserve_mib` (what admission keeps free), less the live holds' unrealised needs. `reserved_mib` is a
different number, the sum of the live holds' needs. `room_mib` is unclamped and negative when the
holds reserve more than the machine offers; `free_mib` is the same room clamped at 0. A reader of
telemetry's `waiting` line sees that line clamp its own `room_mib` at 0.

| key | type | meaning |
|---|---|---|
| `reserved_mib` | int | the sum of the live holds' needs |
| `free_mib` | int | the room, never below 0 |
| `machine_mib` | int | the machine's memory |
| `reserve_mib` | int | what admission keeps free |
| `headroom_mib` | int | the headroom admission read |
| `room_mib` | int | `headroom_mib` less `reserve_mib` less the holds' unrealised needs, below 0 when over-reserved |

## `watch`

The watchdog, from the newest shared sample.

| key | type | meaning |
|---|---|---|
| `level` | string | the memory level of the newest sample; empty without a sample taken in the last 6 s |
| `excess_points` | number | E, in points, at the newest sample |
| `protecting` | bool | whether the watchdog is protecting the machine now |
| `reason` | string | why it is not, empty when it is |
| `next_victim` | string | the hold the next stop round lists first; empty when none |
| `learning` | int | running jobs whose label has no normal share yet |

## `holds[]`

| key | type | meaning |
|---|---|---|
| `name` | string | the hold's file name in `holds/` |
| `label` | string | the holder's label |
| `pid` | int | the owner's pid (the `coreslot run` process) |
| `start_ns` | int | when the hold was admitted |
| `held_for_s` | number | how long it has been held |
| `arrival_ns` | int | when its run first arrived in the queue |
| `need_mib` | int | what the hold reserves; 0 for a run outside the budget |
| `footprint_mib` | int | what its run last measured |
| `stops` | int | how many times it was stopped |
| `owner_exited` | bool | the owner exited while a descendant that inherited the hold's descriptor keeps the reservation |
| `container` | bool | present and true for container work, whose containers' memory is not measured |
| `no_cpu_s` | int | present once its tree has used next to no CPU for 30 minutes: for how long, in s |
| `typical_s` | int | the label's typical run time; 0 when unknown |
| `left_s` | int or null | what remains of `typical_s`, never below 0; null when `typical_s` is 0 |
| `stoppable` | bool | where a reader may offer to stop the run |

`left_s` of 0 on a hold that is still live means it has outlived its typical run time.

`stoppable` is a convenience, not a control: it is true where the hold's body marks it a victim
(its run is restartable, its command has a process group of its own and is not container work), its
command has not exited and `status` read the process table and saw the owner alive. It refuses
nobody anything, and when `status` cannot be sure it is false: an unreadable process table makes
every `stoppable` false.

## `tickets[]`

| key | type | meaning |
|---|---|---|
| `name` | string | the ticket's file name in `queue/`, the value telemetry's `enqueue.ticket` carries |
| `id` | string | the ticket's first three name fields: what `cancel` names |
| `label` | string | the requester's label |
| `pid` | int | the requester's pid |
| `need_mib` | int | the memory it asks for |
| `created_ns` | int | when it was published |
| `waited_s` | number | how long it has waited |
| `position` | int | 1 is the head; 0 for a dead ticket |
| `state` | string | `waiting`, `stalled` or `dead` |
| `stalled` | bool or null | null unless `--observe` ran |
| `expected_wait_s` | int or null | the first live ticket's expected wait, see below |

`expected_wait_s` is the expected wait the passing rule judges others by
([`PROTOCOL.md` section 10.3](../PROTOCOL.md#103-waiting-and-election)). It is given on the first
live ticket in queue order only; every later ticket and every dead ticket has null. It is null as
well when the rule leaves the wait unknown: the headroom is unread while a hold is live, the live
holds with a typical run time cannot cover the need, or the ticket is a restart whose watch state
cannot be read. It is null for every ticket while `mem` is null because the configuration or the
machine's memory could not be read (`config_error` is set), even with no hold live. Otherwise it is
0 when the ticket fits now and when no hold is live, where the idle rule
starts the head whatever the numbers say. A restart's number is at least the calm it still needs.

A number, or a 0, is not a start time. A hold may overrun its usual time; a memory gate may hold the
head; a container request is judged against real free memory rather than `room_mib`; and under
`--observe` the number stays on the first live ticket even when `status` shows it stalled.
