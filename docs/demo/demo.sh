#!/bin/sh
# A 20-second demo of the queue, for a terminal recording (see docs/demo/README.md).
#
# Four "agents" each start a test gate that asks for 60 % of this machine's memory, so only one fits
# at a time: one runs, three wait their turn in order, and `coreslot status` shows who is where.
# The gates are `sleep`s, so the demo shows the mechanics only.
# It runs on a scratch state directory of its own and never touches the machine's real one.
set -eu

CORESLOT_STATE_DIR="$(mktemp -d)/state"
export CORESLOT_STATE_DIR
mkdir -p "$CORESLOT_STATE_DIR"
trap 'rm -rf "$(dirname "$CORESLOT_STATE_DIR")"' EXIT

if [ "$(uname)" = Darwin ]; then
    total_mib=$(sysctl -n hw.memsize | awk '{ print int($1 / 1048576) }')
else
    total_mib=$(awk '/^MemTotal:/ { print int($2 / 1024) }' /proc/meminfo)
fi
need="$((total_mib * 6 / 10))M"

say() { printf '\n\033[1m$ %s\033[0m\n' "$*"; sleep 1; }

say "# four agents each start a gate that asks for 60 % of the machine's memory"
for agent in 1 2 3 4; do
    printf '$ coreslot run --mem %s -- sleep 4 &   # agent %s: a test suite\n' "$need" "$agent"
    coreslot run --mem "$need" -- sleep 4 &
    sleep 0.4
done

sleep 1
say coreslot status
coreslot status
sleep 4
say "coreslot status   # the first finished; the next in line was admitted"
coreslot status
wait
say "coreslot status   # all four done, nothing leaked"
coreslot status
