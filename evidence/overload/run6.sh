#!/bin/sh
# Runs acceptance run 6 (evidence/run6.py), or one phase of it.
#
#   evidence/overload/run6.sh [--dry] [PHASE [ARGS...]]
#
# With no phase: learn while OUT/needs.sha256 is missing, then the pilot, verdict pilot heavy 1 12
# and verdict pilot light 1 12, each with --primer. learn and verdict continue where an interrupted
# run stopped, so re-issuing the same command resumes it. A phase given by name runs alone, for
# instance
# `run6.sh verdict main heavy 1 30 --primer` or `run6.sh verdict main heavy 31 31 --as-block 5`
# for a re-run block.
#
# STAND defaults to evidence/overload/stand4.json and OUT to evidence/overload/run6. --dry uses
# stand-dry.json and OUT ~/bench.noindex/run6-dry, and with no phase runs one block of each mix
# after learn: pilot heavy and light, sens spread and cold.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dry=
if [ "${1:-}" = "--dry" ]; then
	shift
	dry=1
	STAND=${STAND:-$here/stand-dry.json}
	OUT=${OUT:-$HOME/bench.noindex/run6-dry}
fi
STAND=${STAND:-$here/stand4.json}
OUT=${OUT:-$here/run6}
run() { python3 "$here/../run6.py" "$@"; }

if [ $# -gt 0 ]; then
	phase=$1
	shift
	run "$phase" "$STAND" "$OUT" "$@"
	exit
fi
[ -f "$OUT/needs.sha256" ] || run learn "$STAND" "$OUT"
if [ -n "$dry" ]; then
	run verdict "$STAND" "$OUT" pilot heavy 1 1 --primer
	run verdict "$STAND" "$OUT" pilot light 1 1
	run verdict "$STAND" "$OUT" sens spread 1 1
	run verdict "$STAND" "$OUT" sens cold 1 1
	exit
fi
run verdict "$STAND" "$OUT" pilot heavy 1 12 --primer
run verdict "$STAND" "$OUT" pilot light 1 12 --primer
