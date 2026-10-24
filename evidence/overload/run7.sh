#!/bin/sh
# Runs acceptance run 7 (evidence/run7.py), or one phase of it.
#
#   evidence/overload/run7.sh [--dry] [PHASE [ARGS...]]
#
# With no phase: learn while OUT/needs.sha256 is missing, then the pilot: verdict pilot heavy 1 12
# with --primer, verdict pilot light 1 12 and verdict pilot wide 1 6; then it prints the next
# step, pilot-check, which the operator runs and reads before any main block. learn and verdict
# continue where an interrupted run stopped, so re-issuing the same command resumes it. A phase
# given by name runs alone, for instance `run7.sh verdict main heavy 1 144` or
# `run7.sh verdict main heavy 145 145 --as-block 5` for a re-run block. pilot-check takes OUT
# only, `run7.sh pilot-check`; summary takes OUT and, once the drop past 32 h is declared,
# --dropped: `run7.sh summary` or `run7.sh summary --dropped`.
#
# STAND defaults to evidence/overload/stand7.json and OUT to evidence/overload/run7. --dry uses
# stand-dry.json and OUT ~/bench.noindex/run7-dry, and with no phase runs one block of each mix
# after learn: pilot heavy 1-2 (one at each level), pilot light and wide, sens spread.
#
# The run takes about a day, so launch it detached from the terminal that starts it:
#
#   nohup evidence/overload/run7.sh > ~/bench.noindex/run7.log 2>&1 &
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dry=
if [ "${1:-}" = "--dry" ]; then
	shift
	dry=1
	STAND=${STAND:-$here/stand-dry.json}
	OUT=${OUT:-$HOME/bench.noindex/run7-dry}
fi
STAND=${STAND:-$here/stand7.json}
OUT=${OUT:-$here/run7}
run() { python3 "$here/../run7.py" "$@"; }

if [ $# -gt 0 ]; then
	phase=$1
	shift
	case $phase in
	pilot-check | summary) run "$phase" "$OUT" "$@" ;;
	*) run "$phase" "$STAND" "$OUT" "$@" ;;
	esac
	exit
fi
[ -f "$OUT/needs.sha256" ] || run learn "$STAND" "$OUT"
if [ -n "$dry" ]; then
	run verdict "$STAND" "$OUT" pilot heavy 1 2 --primer
	run verdict "$STAND" "$OUT" pilot light 1 1
	run verdict "$STAND" "$OUT" pilot wide 1 1
	run verdict "$STAND" "$OUT" sens spread 1 1
	exit
fi
run verdict "$STAND" "$OUT" pilot heavy 1 12 --primer
run verdict "$STAND" "$OUT" pilot light 1 12
run verdict "$STAND" "$OUT" pilot wide 1 6
echo "run7: the pilot is recorded; next: $0 pilot-check, read OUT/pilot_check.json, and only on"
echo "run7: \"go\": true run the main blocks (verdict main heavy 1 144, light 1 40, wide 1 30,"
echo "run7: sens spread 1 12)"
