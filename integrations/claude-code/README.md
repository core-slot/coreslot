# coreslot for Claude Code

A Claude Code plugin with one `PreToolUse` hook. When an agent is about to run a heavy test or
build command bare, the hook refuses it once and offers the same command back under coreslot:

```text
Claude:  go test -race ./...
hook:    coreslot: several agents share this machine's memory, and this command is one of the
         heavy ones. Run it with the `coreslot run --restart=yes --` prefix inserted, ...
         coreslot run --restart=yes -- go test -race ./...
Claude:  coreslot run --restart=yes -- go test -race ./...
```

The offered command starts when the machine has the memory for it, so four sessions that each
start a test suite do not pile four suites onto a machine that is already short; if memory still
runs out, coreslot stops the newest commands and restarts them by itself, which `--restart=yes`
allows: every command the hook recognises is a test suite or a build, safe to kill at any moment
and run again from scratch. coreslot labels the
command on its own, so the offer carries no label. It goes through your permission rules like any
command you type: the hook only refuses, it never approves anything.

## Install

1. Install coreslot itself and put it on `PATH`
   ([the main README](../../README.md#install)):

   ```sh
   brew install core-slot/tap/coreslot
   ```

2. Add the plugin, in Claude Code:

   ```text
   /plugin marketplace add core-slot/coreslot
   /plugin install coreslot@coreslot
   ```

   On Claude Code 2.1.275 or later one line does both:
   `/plugin install coreslot --marketplace core-slot/coreslot`.

There is nothing to configure: coreslot learns how much memory each command needs from its past
runs.

The hook needs `python3` (3.9 or newer) and nothing else. It does nothing on a machine where
`coreslot` is not on `PATH`.

### Without the plugin system

Clone the repository and register the hook yourself, in Claude Code's user `settings.json`
([its hooks documentation](https://code.claude.com/docs/en/hooks) says where that file lives):

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "python3 /path/to/coreslot/integrations/claude-code/hooks/coreslot_hook.py"}
        ]
      }
    ]
  }
}
```

## What counts as heavy

[`hooks/shapes.json`](hooks/shapes.json) lists the commands the hook recognises: `go test ./...`, `cargo test` / `build` / `nextest` / `clippy`, pytest with
`-n`, `jest`, `vitest` and the `npm`/`pnpm`/`yarn`/`bun` test scripts, and `bazel test` / `build`.
`make -j`, Gradle and `xcodebuild` are not in it: their targets can deploy, migrate or upload,
which must not run twice. A shape matches a command whose words begin with one of
its `programs` (a path such as `.venv/bin/pytest` may also be written `./.venv/bin/pytest`) and,
when it lists `requires`, that has a later word equal to or starting with one of them (`-j` takes
`-j8`).

To change the list, copy the file to `~/.config/coreslot/claude-code-shapes.json` and edit it;
when that file exists it replaces the defaults. The hook offers every shape with `--restart=yes`,
so add only commands that are safe to kill at any moment and run again from scratch:

```json
{
  "shapes": [
    {"programs": [["./scripts/test.sh"]]},
    {"programs": [["go", "test"]], "requires": ["./..."]}
  ]
}
```

## What it does not do

It is a nudge, not enforcement. It passes a command silently when it is not sure:

- a command it cannot split with certainty: `$(...)`, backticks, heredocs, subshells, a `{` or
  `}` anywhere, even quoted (`${...}`), `[[`, unbalanced quotes, line
  continuations, comments;
- a command whose program word is not a shape's (`ssh host go test ./...`, `env go test ./...`,
  anything already starting with `coreslot`);
- a command it already refused for the same session and agent: from then on that session and
  agent run it outside coreslot every time they send it unchanged, not just the first retry.
  The refusal says so, so an agent you told to run something bare can;
- any failure of its own, such as an unreadable shape file.

Inside `&&`, `;` and `|` chains it offers the prefix at every heavy segment and leaves every other
byte as it was. It remembers refusals in `~/.local/state/coreslot-claude-code/`, one file per
session and agent of SHA-256 digests of the commands, never the commands; the next refusal deletes
every file whose last refusal is more than seven days old.

Two things to know about a waiting command:

- The wait counts against the Bash tool's timeout. When memory is short, a suite that waited for
  it may need a longer timeout than the same suite run bare: run it in the background or raise
  the limit.
- When memory runs short, coreslot stops the newest commands and restarts them by itself, so a
  command it stopped may run twice. Run a command that must never run twice with
  `coreslot run --restart=no --`, or mark its label `no_restart` in its `needs.toml` entry.

## Other coding agents

The same rule works as an instruction for any agent that reads an instructions file:

```markdown
## Heavy commands
Several agent sessions share this machine's memory through coreslot. Run test suites and
builds that use a lot of it through coreslot: `coreslot run --restart=yes|no -- <command>`.
coreslot may kill a command when memory runs short and run it again from scratch; choose for
each command:
- `--restart=yes` when it is safe to kill at any moment and run again: tests, builds, linters,
  type checks, for example `coreslot run --restart=yes -- go test ./...`;
- `--restart=no` when it changes anything outside its own output: databases, migrations,
  deploys, pushes, sending messages, writing shared files. When unsure, choose no.
If your tool has a time limit, run it in the background or raise the limit. Never run them bare
in parallel.
```

## Tests

```sh
cd integrations/claude-code && python3 -m unittest
```
