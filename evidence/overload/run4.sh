#!/bin/sh
# Runs acceptance run 4 (evidence/run4.py), or one phase of it.
#
#   evidence/overload/run4.sh [--dry] [PHASE [ARGS...]]
#
# With no phase: learn (skipped once OUT/needs.toml exists, since tuning and the verdict use the
# needs it froze), tune, verdict 1 12, summary and figures. tune and verdict continue where an
# interrupted run stopped, so re-issuing the same command resumes it. A phase given by name runs
# alone, for instance `run4.sh verdict 13 13 --as-block 5` for a re-run block.
#
# STAND defaults to evidence/overload/stand4.json and OUT to evidence/overload/run4. --dry uses
# stand-dry.json, OUT ~/bench.noindex/run4-dry and one verdict block, and runs summary with --dry.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dry=
last=12
if [ "${1:-}" = "--dry" ]; then
	shift
	dry=--dry
	last=1
	STAND=${STAND:-$here/stand-dry.json}
	OUT=${OUT:-$HOME/bench.noindex/run4-dry}
fi
STAND=${STAND:-$here/stand4.json}
OUT=${OUT:-$here/run4}
run() { python3 "$here/../run4.py" "$@"; }

if [ $# -gt 0 ]; then
	phase=$1
	shift
	if [ "$phase" = summary ]; then
		run summary "$STAND" "$OUT" $dry "$@"
	else
		run "$phase" "$STAND" "$OUT" "$@"
	fi
	exit
fi
[ -f "$OUT/needs.toml" ] || run learn "$STAND" "$OUT"
run tune "$STAND" "$OUT"
run verdict "$STAND" "$OUT" 1 "$last"
run summary "$STAND" "$OUT" $dry
run figures "$STAND" "$OUT"
