# coreslot for Claude Code

A Claude Code plugin with one `PreToolUse` hook. When an agent is about to run a heavy test or
build command bare, the hook refuses it once and offers the same command back under coreslot:

```text
Claude:  go test -race ./...
hook:    coreslot: this machine shares its cores through a budget, and this command is one of
         its heavy ones. Run it with the `coreslot run --label ... --` prefix inserted, ...
         coreslot run --label go-test -- go test -race ./...
Claude:  coreslot run --label go-test -- go test -race ./...
```

The offered command waits for its share of the machine's core budget, so four sessions that each
start a test suite take turns instead of running four suites on the same cores. It goes through
your permission rules like any command you type: the hook only refuses, it never approves
anything.

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

3. Tell coreslot how many cores each label keeps busy, in `~/.config/coreslot/needs.toml`. A
   range from 1 is a safe start for commands that size themselves to the machine; after a few
   runs, `coreslot suggest --write` replaces each entry with what the runs used:

   ```toml
   [needs.go-test]
   cpu = "1.."

   [needs.cargo]
   cpu = "1.."

   [needs.pytest]
   cpu = "1.."
   ```

   Without an entry a label asks for one core, which keeps the queue honest only for commands
   that really use one; coreslot prints a line after each run that used more, saying what would
   fit.

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

[`hooks/shapes.json`](hooks/shapes.json) lists the commands the hook recognises, each with the
label it offers: `go test ./...`, `cargo test` / `build` / `nextest` / `clippy`, pytest with
`-n`, `jest`, `vitest` and the `npm`/`pnpm`/`yarn`/`bun` test scripts, `make -j`, Gradle,
`xcodebuild` and `bazel test` / `build`. A shape matches a command whose words begin with one of
its `programs` (a path such as `.venv/bin/pytest` may also be written `./.venv/bin/pytest`) and,
when it lists `requires`, that has a later word equal to or starting with one of them (`-j` takes
`-j8`).

To change the list, copy the file to `~/.config/coreslot/claude-code-shapes.json` and edit it;
when that file exists it replaces the defaults:

```json
{
  "shapes": [
    {"label": "unit", "programs": [["./scripts/test.sh"]]},
    {"label": "go-test", "programs": [["go", "test"]], "requires": ["./..."]}
  ]
}
```

A label is letters, digits and `.`, `_`, `:`, `-`, up to 64 characters, so that it can be written
into a command line unquoted.

## What it does not do

It is a nudge, not enforcement. It passes a command silently when it is not sure:

- a command it cannot split with certainty: `$(...)`, backticks, heredocs, subshells, braces,
  unbalanced quotes, line continuations, comments;
- a command whose program word is not a shape's (`ssh host go test ./...`, `env go test ./...`,
  anything already starting with `coreslot`);
- a command it already refused for the same session and agent: sent again unchanged, it runs
  outside the budget. The refusal says so, so an agent you told to run something bare can;
- any failure of its own, such as an unreadable shape file.

Inside `&&`, `;` and `|` chains it offers the prefix at every heavy segment and leaves every other
byte as it was. It remembers refusals for seven days in `~/.local/state/coreslot-claude-code/`, as
SHA-256 digests of the command, never the command.

Two things to know about a waiting command:

- The wait counts against the Bash tool's timeout. When the machine is busy, a suite that waited
  its turn may need a longer timeout than the same suite run bare.
- coreslot demotes long, large runs by itself (see "Choosing a range" in the main README). For a
  label whose tests have wall-clock deadlines, add `preemptible = false` to its entry.

## Other coding agents

The same rule works as an instruction for any agent that reads an instructions file:

```markdown
## Heavy commands
This machine shares its cores between several agent sessions through coreslot. Run test suites
and builds that use many cores through it: `coreslot run --label <tool> -- <command>`, for
example `coreslot run --label go-test -- go test ./...`. Never run them bare in parallel.
```

## Tests

```sh
cd integrations/claude-code && python3 -m unittest
```
