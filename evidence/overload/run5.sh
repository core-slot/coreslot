#!/bin/sh
# Runs acceptance run 5 (evidence/run5.py), or one phase of it.
#
#   evidence/overload/run5.sh [--dry] [PHASE [ARGS...]]
#
# With no phase: verdict 1 12 and summary. verdict continues where an interrupted run stopped, so
# re-issuing the same command resumes it. A phase given by name runs alone, for instance
# `run5.sh verdict 13 13 --as-block 5` for a re-run block.
#
# STAND defaults to evidence/overload/stand4.json, OUT to evidence/overload/run5 (which holds run
# 4's frozen needs.toml) and BASELINE, the directory whose learn.jsonl is the false-failure
# baseline, to evidence/overload/run4. --dry uses stand-dry.json, OUT and BASELINE
# ~/bench.noindex/run5-dry, learns that stand's needs with run4.py learn while OUT/needs.toml is
# missing, runs one verdict block and runs summary with --dry.
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dry=
last=12
if [ "${1:-}" = "--dry" ]; then
	shift
	dry=--dry
	last=1
	STAND=${STAND:-$here/stand-dry.json}
	OUT=${OUT:-$HOME/bench.noindex/run5-dry}
	BASELINE=${BASELINE:-$OUT}
	[ -f "$OUT/needs.toml" ] || python3 "$here/../run4.py" learn "$STAND" "$OUT"
fi
STAND=${STAND:-$here/stand4.json}
OUT=${OUT:-$here/run5}
BASELINE=${BASELINE:-$here/run4}
run() { python3 "$here/../run5.py" "$@"; }

if [ $# -gt 0 ]; then
	phase=$1
	shift
	if [ "$phase" = summary ]; then
		run summary "$STAND" "$OUT" "$BASELINE" $dry "$@"
	else
		run "$phase" "$STAND" "$OUT" "$@"
	fi
	exit
fi
run verdict "$STAND" "$OUT" 1 "$last"
run summary "$STAND" "$OUT" "$BASELINE" $dry
