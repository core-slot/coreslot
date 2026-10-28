#!/bin/sh
# Runs acceptance run 8 (evidence/run8.py), or one phase of it.
#
#   evidence/overload/run8.sh [--dry] [PHASE [ARGS...]]
#
# Before learn and verdict, and alone as `run8.sh preflight`, it checks the machine: it holds
# caffeinate for its own life, refuses (exit 2) on battery power, and appends the Spotlight and
# software-update settings, the background agents still running (names only), the date, uptime
# and swap to OUT/preflight.log. A real run also refuses while Spotlight indexing is on or the
# automatic update check is on; a dry run records them and goes on.
#
# With no phase: preflight, learn while OUT/needs.sha256 is missing, then the pilot of every
# cell (`verdict pilot CELL 1 10`, the first with --primer); then it prints the next step,
# pilot-check, which the operator runs and reads before any main block. power is not in this
# path, it needs the exploratory probe's directory, but it refuses once a pilot record exists:
# run `run8.sh power PROBE_OUT` after learn and before this pilot (the pilot refuses without
# OUT/power.json). learn and verdict continue where an interrupted run stopped, so re-issuing
# the same command resumes it. A phase given by name runs alone, for instance
# `run8.sh verdict main trace-quiet 1 40`. pilot-check and summary take OUT only; power takes
# PROBE_OUT then OUT; anything else takes STAND and OUT.
#
# STAND defaults to evidence/overload/stand8.json and OUT to evidence/overload/run8. --dry uses
# stand-dry.json and OUT ~/bench.noindex/run8-dry, and with no phase runs the whole pipeline at
# 2 blocks a phase: learn, the probe of every cell into OUT itself, power from OUT, the pilot,
# pilot-check, the main blocks when it says go, and summary.
#
# The run takes days, so launch it detached from the terminal that starts it:
#
#   nohup evidence/overload/run8.sh > ~/bench.noindex/run8.log 2>&1 &
set -eu
here=$(cd "$(dirname "$0")" && pwd)
dry=
if [ "${1:-}" = "--dry" ]; then
	shift
	dry=1
	STAND=${STAND:-$here/stand-dry.json}
	OUT=${OUT:-$HOME/bench.noindex/run8-dry}
fi
STAND=${STAND:-$here/stand8.json}
OUT=${OUT:-$here/run8}
CELLS="trace-quiet trace-loaded heavy-quiet heavy-loaded wide-quiet batch"
blocks() { # PHASE LAST [--primer]: blocks 1..LAST of every cell, the primer on the first only
	for c in $CELLS; do
		run verdict "$STAND" "$OUT" "$1" "$c" 1 "$2" ${3:+"$3"}
		set -- "$1" "$2"
	done
}
run() { python3 "${RUN8_PY:-$here/../run8.py}" "$@"; }
refuse() {
	echo "run8: $1" >&2
	exit 2
}

preflight() {
	caffeinate -dimsu -w $$ >/dev/null 2>&1 &
	batt=$(pmset -g batt || true)
	case $batt in *"AC Power"*) ;; *) refuse "the machine is not on AC power" ;; esac
	index=$(mdutil -s / 2>&1 || true)
	update=$(softwareupdate --schedule 2>&1 || true)
	mkdir -p "$OUT"
	{
		date -u
		uptime || true
		sysctl vm.swapusage || true
		echo "$batt"
		echo "spotlight: $index"
		echo "software update: $update"
		echo "background agents:"
		launchctl list | awk 'NR > 1 && $3 !~ /^com\.apple\./ { print $3 }' || true
		pgrep -lf 'go |pytest|node|docker|Code Helper|claude' |
			awk '{ n = split($2, p, "/"); print $1, p[n] }' || true
	} >>"$OUT/preflight.log"
	[ -n "$dry" ] && return
	case $index in *"Indexing disabled"*) ;; *) refuse "Spotlight indexing is on; mdutil -a -i off" ;; esac
	case $update in *"turned off"*) ;; *) refuse "the automatic update check is on; softwareupdate --schedule off" ;; esac
}

if [ $# -gt 0 ]; then
	phase=$1
	shift
	case $phase in
	preflight) preflight ;;
	pilot-check | summary) run "$phase" "$OUT" "$@" ;;
	power)
		probe=${1:?power takes PROBE_OUT}
		shift
		run power "$probe" "$OUT" "$@"
		;;
	learn | verdict)
		preflight
		run "$phase" "$STAND" "$OUT" "$@"
		;;
	*) run "$phase" "$STAND" "$OUT" "$@" ;;
	esac
	exit
fi
preflight
[ -f "$OUT/needs.sha256" ] || run learn "$STAND" "$OUT"
if [ -n "$dry" ]; then
	blocks probe 2
	[ -f "$OUT/power.json" ] || run power "$OUT" "$OUT"
	blocks pilot 2 --primer
	run pilot-check "$OUT"
	if grep -q '"go": true' "$OUT/pilot_check.json"; then
		for c in $CELLS; do # a cell power dropped or the check found futile refuses with 2
			run verdict "$STAND" "$OUT" main "$c" 1 2 || [ $? -eq 2 ]
		done
	fi
	run summary "$OUT"
	exit
fi
[ -f "$OUT/power.json" ] || refuse "no OUT/power.json: run $0 power PROBE_OUT first (it refuses once a pilot exists)"
blocks pilot 10 --primer
echo "run8: the pilot is recorded; next: $0 pilot-check, read OUT/pilot_check.json, and only on"
echo "run8: \"go\": true run the main blocks at power.json's n"
