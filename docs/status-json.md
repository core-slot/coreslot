# `status --json`

The document `coreslot status --json` prints. Its `"schema"` rule, what a consumer must ignore and
what raises the number are in [compatibility.md](compatibility.md#status---json). Every key is
listed below, one table per object. Times named `_ns` are Unix nanoseconds by the wall clock; `_s`
is seconds; `_mib` is MiB. Integers may be large: a reader tolerates any non-negative value (a
signed `room_mib` excepted, see `mem`), and a nonzero `_ns` value exceeds 2^53, so a reader that
parses numbers as doubles loses their last digits. Labels are carried exactly as stored, JSON-encoded, so
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

The memory budget. `free_mib` and `room_mib` are the room a new job may take: the headroom (the
memory the kernel can hand out now without paging,
[`PROTOCOL.md` section 5.1](../PROTOCOL.md#51-the-machines-memory)), less `reserve_mib` (what
admission keeps free), less what each live hold reserves and its run has not used yet
(`need_mib − footprint_mib`, never below 0). `reserved_mib` is a different number, the sum of the
live holds' needs. `room_mib` is unclamped: it is negative when the reserve and the holds take more
than the headroom, with no hold live too when the headroom is below the reserve. `free_mib` is the
same room clamped at 0. A reader of
telemetry's `waiting` line sees that line clamp its own `room_mib` at 0.

| key | type | meaning |
|---|---|---|
| `reserved_mib` | int | the sum of the live holds' needs |
| `free_mib` | int | the room, never below 0 |
| `machine_mib` | int | the machine's memory |
| `reserve_mib` | int | what admission keeps free |
| `headroom_mib` | int | the headroom admission read |
| `room_mib` | int | `headroom_mib` less `reserve_mib` less the holds' unused needs; may be below 0 |

## `watch`

The watchdog, from the newest shared sample.

| key | type | meaning |
|---|---|---|
| `level` | string | the memory level of the newest sample: `green`, `yellow`, `red`, `floor` or `unknown` ([`PROTOCOL.md` section 23.4](../PROTOCOL.md#234-levels)); empty without a sample taken in the last 6 s |
| `excess_points` | number | the machine's excess `E`, in percentage points, at the newest sample ([`PROTOCOL.md` section 23.3](../PROTOCOL.md#233-the-stall-measure)) |
| `protecting` | bool | whether the watchdog is protecting the machine now |
| `reason` | string | why it is not, empty when it is |
| `next_victim` | string | the hold the next stop round lists first; empty when none |
| `learning` | int | running jobs whose label has no normal share yet |

## `holds[]`

| key | type | meaning |
|---|---|---|
| `name` | string | the hold's file name in `holds/` |
| `label` | string | the holder's label |
| `pid` | int | the owner's pid as the hold names it: the `coreslot run` process that published it |
| `start_ns` | int | when the hold was admitted |
| `held_for_s` | number | how long it has been held |
| `arrival_ns` | int | when its run first arrived in the queue |
| `need_mib` | int | what the hold reserves; 0 for a run outside the budget |
| `footprint_mib` | int | what its run last measured |
| `stops` | int | how many times it was stopped |
| `owner_exited` | bool | the owner exited while a descendant that inherited the hold's descriptor keeps the reservation; false also when the process table could not be read |
| `container` | bool | present and true for container work, whose containers' memory is not measured |
| `no_cpu_s` | int | present once its tree has used next to no CPU for 30 minutes: for how long, in s |
| `typical_s` | int | the label's typical run time; 0 when unknown |
| `left_s` | int or null | what remains of `typical_s`, never below 0; null when `typical_s` is 0 |
| `stoppable` | bool | where a reader may offer to stop the run |

`left_s` of 0 on a hold that is still live means it has outlived its typical run time.

A live hold whose body cannot be parsed, or names another pid or start than its file name, is shown
from its name: `pid` and `start_ns` from the name, `need_mib` the cold start need
([`PROTOCOL.md` section 5.1](../PROTOCOL.md#51-the-machines-memory); 1 while `config_error` is set),
and every other key empty, 0, false or null. It is not counted in `ignored`.

`stoppable` is a convenience, not a control: it prevents nothing. It is true where the hold's body
marks it a victim (its run is restartable, its command has a process group of its own and is not
container work), its command has not exited, and `status` read the process table and found a
process `pid` that started no later than `start_ns`, with a second's slack. It does not show that
this process is a `coreslot run`, that it holds the hold's lock, or that it is still the same
process when a signal goes out. A `pid` means something only on the host that printed it, so a `status --json` read
over `ssh` ([remote.md](remote.md)) names processes on that host. When `status` cannot be sure,
`stoppable` is false: an unreadable process table makes every `stoppable` false.

A reader that stops a run whose hold is `stoppable` reads `status --json` again just before and
matches the hold by `name`, then sends `SIGTERM` to `pid` and nothing else: `coreslot run` relays it
to its command's own process group, which a `stoppable` run always has. A `SIGKILL` ends
`coreslot run` alone; its command keeps running and keeps the reservation (`owner_exited` then
shows it). A run stopped this way is not restarted, though `stoppable` asks for a restartable run:
only the watchdog's own stops restart it.

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

`expected_wait_s` is the head's expected wait `W` of the passing rule, the rule by which a waiter
with a typical run time may start ahead of the head when it is expected to end within `W`
([`PROTOCOL.md` section 10.3](../PROTOCOL.md#103-waiting-and-election)). It is given on the first
live ticket in queue order only; every later ticket and every dead ticket has null. It is null as
well when the rule leaves the wait unknown: the headroom is unread while a hold is live, the live
holds with a typical run time cannot cover the need, or the ticket is a restart whose watch state
cannot be read. It is null for every ticket while `config_error` is set, even with no hold live. A
`mem` of null with `config_error` empty only means the headroom is unread: the first live ticket's
wait is then 0 when no hold is live and null when one is. Otherwise it is 0 when the ticket fits
now, when the holds that would cover it have outlived their typical run time, and when no hold is
live, where the idle rule starts the head whatever the numbers say
([`PROTOCOL.md` section 12.1](../PROTOCOL.md#121-steps)). While a hold is live, a restart's number
is at least the calm it still needs.

A number, or a 0, is not a start time. A hold may overrun its usual time; a memory gate may hold the
head; a container request is judged against real free memory rather than `room_mib`; and under
`--observe` the number stays on the first live ticket even when `status` shows it stalled.
