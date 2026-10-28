#!/bin/sh
# A 30-second demo of the queue, for a terminal recording (see docs/demo/README.md).
#
# Four "agents" each start a test gate that asks for 4 cores, on a budget of 8: two run, two
# wait their turn, and `coreslot status` shows who holds what. The gates are `sleep`s, so the
# demo shows the mechanics only; the measured effect on real suites is in evidence/README.md.
# It runs on a scratch budget of its own and never touches the machine's real one.
set -eu

CORESLOT_STATE_DIR="$(mktemp -d)/state"
export CORESLOT_STATE_DIR
mkdir -p "$CORESLOT_STATE_DIR"
printf '[resources]\ncpu = 8\nmem = 0\n' >"$CORESLOT_STATE_DIR/config.toml"
trap 'rm -rf "$(dirname "$CORESLOT_STATE_DIR")"' EXIT

say() { printf '\n\033[1m$ %s\033[0m\n' "$*"; sleep 1; }

say "# a budget of 8 cores; four agents each start a gate that asks for 4"
for agent in 1 2 3 4; do
    printf '$ coreslot run --need cpu=4 --label agent-%s:tests -- sleep 8 &   # a test suite\n' "$agent"
    coreslot run --need cpu=4 --label "agent-${agent}:tests" -- sleep 8 &
    sleep 0.4
done

sleep 1
say coreslot status
coreslot status
sleep 8
say "coreslot status   # the first two finished; the next two were admitted in order"
coreslot status
wait
say "coreslot status   # all four done, nothing leaked"
coreslot status
