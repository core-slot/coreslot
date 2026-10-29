# Running a command on another machine under its budget

A recipe for sending a heavy command to a second, stronger machine with
Crabbox so that it waits for, and runs inside, *that*
machine's coreslot budget. coreslot has no remote code and gains none: the budget is a directory on
the machine where the command runs, so the remote part lives entirely in Crabbox and `ssh`. This
page only shows how to put the two together. Crabbox is an external tool with its own
documentation; check its `--help` for the flags used here before relying on them.

## When this fits

- You have a machine with more cores than the one you work on, reachable over `ssh`, and a command
  that decides something: a test suite, a lint or type check, a gate.
- Several agents or sessions may send work to that machine at once.

In one measurement, a CPU-heavy test suite of about six minutes took about 55 % less wall time on a
14-core machine than on the 10-core laptop it was started from. Crabbox added about 8.5 s over a
plain `rsync` and `ssh`. The first sync of a fresh workspace seeds a git checkout on the remote once
(about 77 s in that measurement); later syncs of the same workspace took a few seconds.

## Prerequisites

- **On the remote**, as the user Crabbox connects as: coreslot installed and configured
  ([README](../README.md#install), [configuration](../README.md#configuration)). Check it over
  `ssh` with `coreslot doctor`, and set the budget in that user's `config.toml` if the default
  (`cpu` at 140 % of the CPU count) is not what you want on that machine. The budget is per user:
  agents that connect as one user share it, and with local sessions of that user too, because the
  state directory comes from the password database
  ([`PROTOCOL.md` section 4.1](../PROTOCOL.md#41-location)). Another user on the same host has a
  budget of their own.
- **On the caller**: Crabbox, and an `ssh` key the remote accepts without a prompt.
- **A toolchain on the remote**: Crabbox syncs your git checkout to the host, so the host needs
  only what the command itself uses (compilers, interpreters, test dependencies).

## The command

Put `coreslot run` in front of the heavy command, on the remote side of Crabbox:

```sh
export CRABBOX_STATIC_ID=agent-1   # one lease id per agent, see below

crabbox run --provider ssh --target macos --static-host build-host --shell -- '
  export PATH="$PATH:/opt/homebrew/bin:$HOME/.local/bin"
  coreslot run --strict --need cpu=8 --label gate:unit -- make test
'
```

What happens: Crabbox syncs the working tree to `build-host`, starts the shell command there, and
`coreslot run` waits in the queue of `build-host`'s budget until 8 cpu tokens are free, runs `make
test`, and releases the tokens when it exits. Everything in [Choosing a
range](../README.md#choosing-a-range) applies unchanged, `--need cpu=4..12` and `{n}` included.

- **Export `PATH` in the remote command.** A non-login `ssh` shell often lacks the directories
  `coreslot` and your toolchain live in (`/opt/homebrew/bin`, `~/.local/bin`). Without it
  `coreslot` is not found and the command exits 127.
- **`--strict`** makes the remote refuse (exit 75) instead of running unbudgeted when its state
  directory or configuration cannot be used. Without it a broken remote budget fails open: the
  command runs at once, with a diagnostic on stderr, which is the everything-at-once case again
  ([README](../README.md#exit-status)).
- **`--timeout D`** bounds the wait (`--timeout 10m`), `--no-wait` refuses at once when the request
  does not fit; both exit 75, so the caller can try another host.

## One lease id per agent

Crabbox keeps a lease per static host. With its default id a second agent is refused while the
first holds the lease (`lease … is claimed by repo …; use --reclaim`, exit 2). Give each agent its
own `CRABBOX_STATIC_ID`: both then run at the same time, each under the host's coreslot budget. In
that measurement two agents with distinct ids ran concurrently; one id per agent also keeps that
agent's warm workspace, so a later run does not seed the checkout again.

## Choosing a host

`coreslot status --json` over `ssh` answers in about 0.3 s and says how much of a host's budget is
free and who waits (the document has `"schema": 1`):

```sh
ssh build-host 'export PATH="$PATH:/opt/homebrew/bin:$HOME/.local/bin"; coreslot status --json' |
  jq '{cpu_free: .resources.cpu.free,
       mem_free_mib: .resources.mem.free_mib,
       waiting: [.tickets[] | select(.state == "waiting")] | length}'
```

A host admits a request of `cpu=8` now when `cpu_free` is at least 8 and `waiting` is 0. A new
request queues behind the tickets already waiting: the head blocks the waiters behind it, except
that a waiter among the first 4 may pass it within what every minimum ahead leaves free
([`PROTOCOL.md` section 10.3](../PROTOCOL.md#103-waiting-and-election)). So with tickets waiting,
free tokens do not mean you are next. `status` exits 3 while the host's runs fail open (it still
prints the report), so look at `config_error` and `degraded` too before trusting a host.

## Limits

- **Verdicts, not builds.** Crabbox syncs the tree out only. Files the command writes stay on the
  remote. Use this for commands whose result is the exit status and the output (tests, checks), not
  for a build whose artifacts you need back.
- **Sync preconditions.** The checkout must be on a branch, not a detached `HEAD`, and its `origin`
  must be reachable from the remote; otherwise Crabbox's sync fails (exit 67) before anything runs.
- **The remote's budget is the one that holds.** Your local coreslot sees nothing of the remote run:
  a local `coreslot run` around the `crabbox run` command would reserve local cores for a command
  that uses none of them, so leave it out unless something else on your machine must be bounded.
- **Exit status.** `coreslot run` exits with its command's status. Its own statuses are 75
  (refused), 64 (usage), 70 (internal) and 126 / 127 (the command is not executable / not found);
  Crabbox has its own (2 and 67 above). A caller that must tell a refusal from a failing test uses
  `--refusal-code N` to move 75 to a status the command does not return. How Crabbox relays the
  remote status is Crabbox's to document.
- **Other users' commands.** coreslot is cooperative admission control, not enforcement
  ([README](../README.md#what-it-is-not)). Commands that do not go through `coreslot run` on the
  remote are not counted.

## Why the remote needs coreslot too

A Crabbox static host has no core or memory budget of its own. Several agents offloading to one
host without coreslot there all run at once, which is the oversubscription coreslot exists to
prevent ([README](../README.md)); moving the work to a bigger machine only moves the problem.
Distinct lease ids let the agents in together, and `coreslot run` on the host is what then makes
them take turns.
