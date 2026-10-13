# PROTOCOL.md ambiguities found by the independent Python client

Of the 42 entries, all are resolved: 40 by the revised `PROTOCOL.md`, and B1/D6 by the stray rule
that PROTOCOL §4.2/§10.4 now state. Each entry ends with its status.

Each entry is a place where `PROTOCOL.md` (format 1) is silent, ambiguous or self-contradictory
for someone implementing from it alone. Each one gives the section, the readings, and the choice
`coreslot_client.py` made. The choice is not normative: each entry asks for a `PROTOCOL.md` fix or
an explicit "both are valid".

## A. Names, seq and publication

**A1. §7.1, §11, §21.1/§21.3/§21.6: do temporaries of `gen`, `FORMAT`, `degraded` and cancel
requests use the same `seq` counter as ticket and hold names?**
§7.1 says each process numbers "its publications" with one counter, and §11 makes `gen`, `FORMAT`
and `degraded` publications through `.tmp-<pid>-<seq>`. But in §21.3 pid 7300's ticket is `000003`
and its hold is `000004`, although enqueue bumped `gen` between them. In §21.6 pid 9200 writes `gen`
(reconciliation) and then publishes ticket `000001`. Under a single counter, both examples are
impossible.
*Choice:* two counters. Ticket and hold names (and their temporaries) use one. Temporaries of
`gen`/`FORMAT`/`degraded` (in `$S`) and of cancel requests (in `cancel/`) use the other. Their
directories never overlap, so no temporary name can collide.

**Status — resolved: PROTOCOL §7.1, §21 preamble. How a process draws seq is free (both conform). Client unchanged.**

**A2. §11 step 1 / step 4: must a ticket's or hold's final `seq` equal its temporary's `seq`?**
The examples show equal values (`.tmp-0000004711-000001` → `...-000001-gate_unit`). On a final-name
`EEXIST`, §11 changes the final seq but not the temporary.
*Choice:* equal, until a final-name `EEXIST` forces the next seq.

**Status — resolved: PROTOCOL §7.1. A temporary's seq need not equal the name's. Client unchanged.**

**A3. §11 step 4: cancel request whose final name already exists.**
"On `EEXIST` the writer takes its next `seq` and retries with the new name" cannot apply. A cancel
request's name is the target ticket's id, not the canceller's seq.
*Choice:* an existing request means the ticket is already cancelled. The canceller removes its own
temporary and reports "already requested" (`cancel()` returns `False`). It is not an error.

**Status — resolved: PROTOCOL §11 step 4, §13.1. An existing request means already requested, not an error. Matches the choice.**

**A4. §11 last paragraph, §10.4 step 3: may a live writer remove its own temporary after a failed
publication?**
"Nothing else removes temporary files" (only reconciliation does). Yet a writer that hits `ENOSPC`
or `EIO` while writing the body is alive and still inside the section.
*Choice:* the writer unlinks its own temporary inside the section on failure.

**Status — resolved: PROTOCOL §11. The writer removes its own temporary on failure. On EEXIST a ticket or hold writer now removes its temporary and starts again at step 1 with a new temporary (client changed to the step-1 restart).**

**A5. §11 step 4: which errors trigger the `link`+`unlink` fallback?**
Only "unavailable or fail with `EINVAL`" is named. macOS `renamex_np(RENAME_EXCL)` returns
`ENOTSUP` on filesystems without exclusive rename. Linux returns `ENOSYS` (`renameat2` missing from
the kernel), and older libcs lack the wrapper.
*Choice:* fall back on `EINVAL`, `ENOSYS`, `ENOTSUP`/`EOPNOTSUPP` and on a missing symbol.

**Status — resolved: PROTOCOL §11 step 4. Fall back on a missing call, ENOSYS, EINVAL, ENOTSUP or EOPNOTSUPP. A failed unlink after link(2) leaves the entry published (client changed: that unlink error is no longer fatal).**

**A6. §7.1 display: the derivation is "not part of the protocol", yet both examples imply the
per-byte rule.**
*Choice:* follows the examples (per UTF-8 byte, first 32). Readers never interpret it.

**Status — resolved: PROTOCOL §7.1. Any derivation matching the grammar conforms. Client unchanged.**

## B. Filesystem policy and locations

**B1. §4.2 vs §4.4/§7.2: an entry of `queue/`, `holds/` or `cancel/` whose name matches the
grammar but is a symlink, a directory, owned by another user, or group-writable.**
§4.2 says "anything else is a state error" and applies the check "to every entry under it that the
client uses". §4.4/§7.2 say bad entries are ignored and counted. Under the first reading, any user
able to drop a symlink into `queue/` turns every client into fail-open.
*Choice:* state error for the layout's own paths (`$S`, its subdirectories, `admit.lock`, `FORMAT`,
`gen`, `degraded`, token files, `config.toml` under the override, `log/`). Other processes' tickets,
holds and cancel requests with a wrong type, owner or mode are ignored. Reconciliation skips
non-regular entries.

**Status — resolved: PROTOCOL §4.2 and §10.4 step 1. Resolution: the rule covers grammar-matching entries of queue/, holds/ and cancel/ (temporaries included) only; a stray temporary in $S itself remains a state error (client changed, test `test_stray_temporary_in_the_state_directory_is_a_state_error`). Such entries are never opened or probed: the client lstat-checks before opening and re-checks the inode after. An entry of queue/, holds/ or cancel/ whose name matches the grammar but fails the type/owner/mode checks is ignored and counted: never trusted and never removed by reconciliation, and reconciliation still completes and writes reconciled_ns. Client changed: reconciliation steps 1-3 apply the checks and count (ignored_by_reconciliation), and the election counts (ignored_entries).**

**B2. §4.2: "owned by the process's user id": real or effective uid?** §4.1 uses `getuid()` for the
home. *Choice:* `geteuid()` for ownership checks, and `getuid()` for the home, as §4.1 says.

**Status — resolved: PROTOCOL §4.2. The real uid, getuid(). Client changed.**

**B3. §4.1/§4.4: missing ancestors of `$S`.**
`~/.local/state/` often does not exist on Linux. "A client creates a directory of the layout when it
first needs it" covers `$S` and below, not its ancestors, and gives no mode or checks for them.
*Choice:* create missing ancestors with mode `0700` and check only `$S` and below.

**Status — resolved: PROTOCOL §4.4. Ancestors are created 0700 and not checked. Matches the choice.**

**B4. §5.3: checks on the real (non-override) `config.toml`.**
§4.2-style checks are required only under the override. Must `<home>/.config/coreslot/config.toml`
also be a non-symlink owned by the user? Dotfile managers make it a symlink.
*Choice:* the real configuration may be a symlink and must resolve to a regular file, with no
owner or mode check.

**Status — resolved: PROTOCOL §5.3. The real file is read through symlinks with no owner or mode check, and a dangling symlink is a missing file (the defaults). A directory, FIFO or device is an invalid configuration, detected without blocking. Client changed: O_NONBLOCK on every configuration open (real and override). It already read a dangling symlink as missing, and that is now tested.**

**B5. §4.3: what if the filesystem type cannot be determined** (statfs unavailable to a runtime)?
*Choice:* a state error. On Linux, `/proc/self/mountinfo` serves as a fallback when `statfs` cannot
be called.

**Status — resolved: PROTOCOL §4.3. Undeterminable is a state error; mountinfo MAY be used. Matches. The client also adds the four new Linux magics (vboxsf, Ceph, AFS, kAFS).**

**B6. §4.3 (Linux): `f_type` width and sign.**
`f_type` is a signed `long`, and CIFS `0xFF534D42` does not fit a signed 32-bit value.
*Choice:* compare `f_type & 0xFFFFFFFF`.

**Status — resolved: PROTOCOL §4.3. Compare the low 32 bits. Matches.**

## C. Bodies and values

**C1. §7.2: nesting depth is unbounded.**
An 8 KiB body can nest about 4 096 deep inside an unknown key. Python's default limit (about 1 000)
would call such a body malformed, while Go's `encoding/json` (limit 10 000) accepts it.
*Choice:* accept any depth that fits in 8 KiB. The protocol should bound depth or say that depth
alone never makes a body malformed.

**Status — resolved: PROTOCOL §7.2. Depth alone never makes a body malformed. Matches.**

**C2. §1/§7.2: numbers inside unknown keys.**
Integers are defined for keys "in a body". Is `1e999`, `1.5`, or a 30-digit integer in an unknown
key acceptable ("unknown keys are ignored at any depth")?
*Choice:* yes. Unknown values only need to be valid JSON, and only known keys are type-checked.
The fixture README (`internal/wire/testdata/README.md`) takes the same reading ("their values may be
any JSON"), and `ok-unknown-keys-at-any-depth` exercises it. `PROTOCOL.md` still does not say so.

**Status — resolved: PROTOCOL §7.2. Unknown values may be any JSON. Matches.**

**C3. §7.4: a hold whose `start_ns` differs from its name's start-ns.** Malformed, or is the name
"authoritative" (§7.1) and the body field only informational?
*Choice:* malformed, as a rule violation (`validate_hold(obj, name_start_ns)`). The fixture
README leaves the comparison to "the reader of the directory", which agrees.
*Related, after the §7.1 update (start-ns at most 2^64 − 1):* a hold name whose start-ns lies in
(2^63 − 1, 2^64 − 1] is a valid name, yet `start_ns` in the body must be a signed 64-bit integer.
Such a hold can never be well formed under the "equal to the name's start-ns" rule. The protocol
should give both the same bound.

**Status — resolved: PROTOCOL §7.4. A mismatch is malformed and is compared by the reader of holds/. A name above 2^63 − 1 never has a well-formed body. Matches.**

**C10. §7.2: duplicate keys spelled differently** (`"cpu"` and `"c\u0070u"` in one object). §7.2
says "a duplicate key at any depth" but not whether keys are compared before or after JSON
unescaping. The fixture `bodies/ticket/bad-duplicate-by-escaped-spelling` requires after-unescaping,
and only the fixture README states the rule ("Keys are compared after JSON unescaping").
*Choice:* compare after unescaping, which agrees with the fixture (Python's decoder compares decoded
keys). `PROTOCOL.md` should state it: a byte-level decoder would accept that body.

**Status — resolved: PROTOCOL §7.2. Keys are compared after JSON unescaping. Matches.**

**C4. §7.4: `unbudgeted_resources` duplicates or ordering, and `clamped` keys for resources that
are not in `grant`.** Not stated. *Choice:* neither is checked. The client writes sorted, distinct
names.

**Status — resolved: PROTOCOL §7.4. Readers check neither. Matches.**

**C5. §7.6: the shape of `<n>`.** Leading zeros (`01`), a sign, the value `0`, and values beyond
64 bits are not addressed.
*Choice:* `0|[1-9][0-9]*` of any length, and anything else is malformed. Any `n ≠ 1` (including
`0`) is an unknown format (token-only).

**Status — resolved: PROTOCOL §7.6. `<n>` is 1-9 ASCII digits with leading zeros allowed (`01` is 1); a sign, a 10th digit, CR or other whitespace is malformed. Client changed.**

**C6. §5.2: shapes of `"N%"` and `"-K"`.** Leading zeros (`"05%"`, `"-007"`), `"+5%"`, and
whitespace are not addressed.
*Choice:* only `[1-9][0-9]{0,2}%` (≤ 100) and `-(0|[1-9][0-9]*)`, and everything else is invalid.

**Status — resolved: PROTOCOL §5.2. N and K are 1-9 ASCII digits with leading zeros allowed, and no sign or space. Client changed.**

**C7. §7.8: the range of `gen` and overflow on a bump.**
*Choice:* any int64. A bump past `INT64_MAX` writes `0`. A malformed `gen` reads as 0 anyway.

**Status — resolved: PROTOCOL §7.8. gen is in [0, 2^63 − 1], a negative gen is malformed, and a bump from the maximum writes 0. Client changed (reader). The bump already wrapped.**

**C8. §7.9: updating a malformed `degraded` record.** `first_ns` is "the first fail-open since the
record was created", but a malformed record has no readable `first_ns`.
*Choice:* restart the record (`first_ns = now`, `count = 1`).

**Status — resolved: PROTOCOL §7.9. Start a new record. Matches.**

**C9. §6: entries of `res/<name>/` that are not canonical indices** (`007`, `5000`, `x`) and
entries of `res/` that are not resource names. Are they token files at an "index that exists"?
*Choice:* ignored, so they are not counted as locked.

**Status — resolved: PROTOCOL §6. Not token files: never probed, counted, checked or removed. Matches.**

## D. Waiting, election, gen

**D1. §10.1/§12/§13/§10.4: which removals bump `gen`?**
Enqueue, admission, the resume bump and reconciliation (when it removed something) are specified.
Removing one's own ticket after cancellation or withdrawal, removing one's own hold (§12.3), and
publishing a cancel request are not.
*Choice:* every removal of an own ticket or hold bumps `gen`. Cancel publication does not, because
the owner polls `cancel/` anyway.

**Status — resolved: PROTOCOL §7.8. A section bumps gen once per published or removed ticket or hold, and a cancel publication does not. Client changed: the fail-open hold's section now bumps gen.**

**D2. §10.3: "when it changed": `gen` only, or the whole file?**
*Choice:* either field. A reconciliation that removed nothing still triggers a rescan, which does
no harm.

**Status — resolved: PROTOCOL §10.3. A gen-field change triggers a rescan, and a reconciled_ns change MAY. Matches.**

**D3. §10.2: a resume bump that cannot enter the section.** *Choice:* retry at the next refresh.

**Status — resolved: PROTOCOL §10.2. Retried at the next refresh; it never delays a refresh. Matches.**

**D4. §10.3: a waiter whose own ticket name disappears while it still holds the descriptor**
(removed by an operator or a non-conforming client). Not addressed. Other clients no longer see it.
*Choice:* the waiter keeps waiting and tolerates `ENOENT` when it unlinks at admission. The protocol
could require an I1-style inode check on the own ticket.

**Status — resolved: PROTOCOL §10.3. Keep waiting; ENOENT counts as success. Matches.**

**D5. §10.4 step 2: what counts as "a `queue/` entry with that ticket id"** when that entry is
malformed or not a regular file? *Choice:* any name matching the ticket grammar.

**Status — resolved: PROTOCOL §10.4 step 2. Any queue/ entry with that id that is not ignored as a stray, whatever its body; a stray (wrong type, owner or mode, B1/D6) never protects a cancel request. Client changed: step 2 counts only the tickets that survive step 1.**

**D6. §10.4 step 1: entries matching the ticket or hold grammar that are not regular files.**
*Choice:* not probed and not removed.

**Status — resolved (see B1): PROTOCOL §10.4 step 1. Such entries are ignored and counted, never removed, and reconciliation completes and writes reconciled_ns. Client changed (see B1).**

**D7. §15.2: when must a waiter look for a format change?** On every poll, or only at section
entry? *Choice:* both.

**Status — resolved: PROTOCOL §8, §15.2. FORMAT is read at every section entry and MAY be read on polls. Matches.**

**D8. §15.2: a token-only client that later finds `FORMAT` back at 1, or missing.** Does it rejoin
the queue, and may it write `FORMAT`? *Choice:* it stays token-only for the rest of that request
and never writes `FORMAT`.

**Status — resolved: PROTOCOL §15.2. The mode is decided at every entry. A token-only client holding nothing that finds a format it implements, or none (which it writes), enqueues as a new request in that section. Client changed (`Ticket._step_token_only` rejoins).**

**D9. §15.2: may a token-only client create `res/` and `res/<name>/`?** It "writes nothing ...
except creating missing token files", and a token file needs its directory.
*Choice:* yes. It creates both directories and nothing else, and it opens and creates `admit.lock`
only if `admit.lock` is missing.

**Status — resolved: PROTOCOL §4.4, §15.2. Token-only may create token files, res/, res/<name>/ and a missing admit.lock, never queue/, holds/, cancel/ or log/. Matches.**

## E. Fail-open, environment, nesting

**E1. §14: does a malformed `FORMAT` count as "a directory whose `FORMAT` it does not
understand"?** The answer decides whether a fail-open run may publish its unbudgeted hold, the run
log and `degraded` there.
*Choice:* yes, nothing is written. (The client's first draft appended to `log/` here; the self-test
`test_malformed_format_fails_open_without_writing` caught it.)

**Status — resolved: PROTOCOL §14, §20. A malformed FORMAT is not understood, and nothing is written without an understood FORMAT read during the run. Client changed: the run log now requires FORMAT 1 to be present (a missing FORMAT means no line).**

**E2. §14/§17: the values of `CORESLOT_UNBUDGETED` and of `degraded.reason`.** Only `format`
(token-only) is defined. A fail-open reason vocabulary is not.
*Choice:* short tokens `config`, `state-dir`, `state-type`, `state-owner`, `state-mode`,
`filesystem`, `directory-full`, `inode`, `cpus`, `io`, `format-malformed`. The client deliberately
avoids `format` for a malformed `FORMAT`: a child could not otherwise tell a fail-open from a
token-only run.

**Status — resolved: PROTOCOL §14. The reason is informational non-empty text, and a fail-open MUST NOT use `format`. Matches (`format-malformed`).**

**E3. §18: "is set" for `CORESLOT_UNBUDGETED` and `CORESLOT_TOKEN_FDS`.** §4.1 says "set and
non-empty" for `CORESLOT_STATE_DIR`, and §18 says only "is set".
*Choice:* presence counts, even when empty. An empty `CORESLOT_TOKEN_FDS` fails to parse, which is
a refusal.

**Status — resolved: PROTOCOL §1, §18. Every CORESLOT_* variable that is empty is unset. Client changed.**

**E4. §17/§18: inherited context in an ordinary (`--independent`) run.**
Should the run strip the inherited `CORESLOT_HELD_FD`, `CORESLOT_TOKEN_FDS`,
`CORESLOT_UNBUDGETED` and `CORESLOT_GRANTED_*` before setting its own? Should it make the inherited
grant descriptors close-on-exec, so they do not reach its command at stray numbers?
*Choice:* both. Otherwise a grandchild would pass through to a grant that is not its parent's.

**Status — resolved: PROTOCOL §17. Every run that is not a pass-through strips the inherited grant variables and closes the descriptors they name that are regular files at fd 3 or above; a named standard stream or non-regular descriptor is left open. Client changed: this now applies to every non-pass-through run, not only `--independent`.**

**E5. §18: limits and failures of validation.**
There is no bound on the number of descriptors a `CORESLOT_TOKEN_FDS` range may list (`4-99999999`).
The protocol also does not say what happens when `$S` cannot be read during validation.
*Choice:* more than 513 descriptors does not validate (refusal), and an unreadable `$S` does not
validate (refusal).

**Status — resolved: PROTOCOL §18. More than 512 descriptors (counted before expanding) or an uncheckable context does not validate. Client changed (512, counted before expanding).**

**E6. §20: do pass-through and inherited-unbudgeted runs (§18 rules 1-2) log?** "Each run SHOULD
append". Only token-only is excluded explicitly. *Choice:* they do not log, because they never touch
`$S`.

**Status — resolved: PROTOCOL §18, §20, §21.9. Pass-through and inherited-unbudgeted runs append a `"nested": true` line outside the section after reading FORMAT, never write a missing one, and never rotate. Client changed.**

**E7. §12.3/§14: removing a fail-open hold after the command exits.** §12.3 covers granted holds.
*Choice:* the unbudgeted hold is removed the same way.

**Status — resolved: PROTOCOL §12.3. It applies to the unbudgeted hold too. Matches.**

**E8. I1, §7.9, §14: what a holder that finds an I1 mismatch at release writes, and where.**
I1 says a holder that finds `st_nlink == 0` or a different inode at its token's path "reports
degraded mode (section 14)". §12.1 step 6 places that check at admission, where the head has
published nothing and fails open. At release the command has already run on a real grant. When
the state directory was deleted and recreated under the run, the path now names a new directory,
and nothing says whether the holder records degraded mode in it. §14 forbids writing into a
directory whose `FORMAT` the run has not read and understood "during the run", but the run read
the old directory's, and the new one's `FORMAT` may be missing (entering its section would then
write one). §7.9 counts "fail-open runs", and this run was not one. The §12.3 hold removal would
probe and unlink a same-named entry of the new directory.
*Choice:* the holder checks I1 for its hold and every token before closing them (the hold path,
then each token path: still linked, and `(st_dev, st_ino)` of the path is the held file's). On a
mismatch it closes every descriptor as usual and then writes nothing at that path: no hold removal,
no section entry (so no `admit.lock`, `FORMAT` or `gen`), no `degraded` record and no run-log line.
The library's `Grant.close()` raises the state error (`inode`); the CLI prints it and exits with the
command's status. A fail-open run's unbudgeted hold is released the same way. The protocol should
say whether degraded mode is recorded here, and in which directory.

**Status — resolved:** PROTOCOL I1 and §12.3 now state this choice: the check runs at admission
and at release, and on a mismatch at release the holder writes nothing into the state directory.

## F. CLI surface used by the examples but not specified

**F1. §10.3/§14/§17/§19/§21: the CLI's flags and defaults.**
`--need RES=MIN..MAX` and `RES=N` appear only in examples, along with `--label`, `--timeout`,
`--on-timeout`, `--subst`, `--refusal-code` and `--independent`. "Strict mode" has no flag name.
The default need when no `--need` is given is unspecified.
*Choice:* `run [--need ...] [--label] [--priority] [--timeout S] [--strict] [--independent]
[--subst] [--refusal-code N] -- CMD`, with `RES=MIN..` and `RES=..MAX` beside `RES=MIN..MAX` and
`RES=N`. `--timeout` refuses on expiry (`--on-timeout` is not implemented). The default need is
`cpu=1..16` when the command carries a `cpu` placeholder and `cpu=1..1` otherwise, as §21 now
says; this client's first choice was `cpu=1..1` for every command.

**Status — resolved: PROTOCOL §21 preamble (informative CLI synopsis). It first made the default need `cpu=1..1`, and has since made it `cpu=1..16` for a command with a `cpu` placeholder (`cpu=1..1` otherwise) and added `RES=MIN..` and `RES=..MAX`; `--on-timeout` defaults to `fail`. Matches: this client follows the current preamble, and its `--timeout` refuses and also bounds section entry during enqueue.**

**F2. §17: placeholders naming a resource the request does not name, and `{{n:RES}}`.**
*Choice:* a usage error (exit 64), checked before queueing. `{{n:RES}}` becomes the literal
`{n:RES}`.

**Status — resolved: PROTOCOL §17. A usage error before queueing, and `{{n:RES}}` is a literal. Matches.**

**F3. §17: `CORESLOT_TOKEN_FDS` for a single token: `4` or `4-4`?** Readers must accept both.
*Choice:* `4`.

**Status — resolved: PROTOCOL §17. `4` and `4-4` both conform. Matches.**

## Skipped (optional for this client)

- **Adoption (§12.4, §21.7):** not implemented. It has no CLI or library surface in the brief.
  Validators accept `"adopted": true` holds, and admission counts their tokens like any other locked
  token.
- **`status`, `doctor`, `doctor --reset`, `doctor --clear` (§4.4, §14):** not implemented.
  `ignored_entries` counts non-matching `queue/` entries only.
- **Windows (§16):** not applicable (Unix only).
- **Run-log rotation (§20)** is implemented but not tested (it needs a 10 MiB file).
- **Pass-through (§18)** is implemented (it is cheap and avoids queueing behind a parent), with one
  positive and one refusal test.
