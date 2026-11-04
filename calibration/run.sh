#!/usr/bin/env bash
# Run one calibration pass of the watchdog and record what it saw.
#
#   run.sh [--dry] ramp    [--threads N] [--step-mib N] [--hold-s N] [--max-gib N] [--slow-s N] [--slow-pct N] [--after-s N] [--out DIR]
#   run.sh [--dry] control [--procs N] [--seconds N] [--out DIR]
#
# The branch's own coreslot is built into a temporary directory and run against a temporary state
# directory (CORESLOT_STATE_DIR) whose config.toml puts the watchdog in observe mode with the sample
# log on: it measures and logs, it never stops anything. `ramp` runs the hog under `coreslot run`;
# `control` runs the busy loops. The record (watch.jsonl, work.jsonl, meta.json) lands in --out,
# by default records/<UTC time>-<kind>. --dry prints what would run and starts nothing.
# Either pass first runs its workload for 30 s under the same label (the hog at 0.5 GiB), so the label
# has a run log and the pass is scored against its learned normal share (a label with no history is
# never counted or stopped). A ramp's observer job then keeps the sampling going for --after-s
# (default 90) seconds after the hog stops. --slow-s and --slow-pct are the hog's own stop rule (its
# defaults, 5 s below 30 %) and reach the scored hog only: a longer --slow-s holds the cliff longer.
set -u

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)
py=${PYTHON:-python3}
swap_stop_mib=6144 # swap used may grow this far from the start of the pass
warm_s=30          # the warm-up that gives the label its one counted run

usage() {
	sed -n '2,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2
	exit 64
}

dry=0 kind="" threads=1 step_mib=512 hold_s=15 max_gib="" procs="" seconds=60 out="" after_s=90 slow_s=5 slow_pct=30 # the hog's defaults
while [ $# -gt 0 ]; do
	case $1 in
	--dry) dry=1 ;;
	ramp | control)
		[ -z "$kind" ] || usage
		kind=$1
		;;
	--threads | --step-mib | --hold-s | --max-gib | --slow-s | --slow-pct | --after-s | --procs | --seconds | --out)
		[ $# -ge 2 ] || usage
		case $1 in
		--threads) threads=$2 ;;
		--step-mib) step_mib=$2 ;;
		--hold-s) hold_s=$2 ;;
		--max-gib) max_gib=$2 ;;
		--slow-s) slow_s=$2 ;;
		--slow-pct) slow_pct=$2 ;;
		--after-s) after_s=$2 ;;
		--procs) procs=$2 ;;
		--seconds) seconds=$2 ;;
		--out) out=$2 ;;
		esac
		shift
		;;
	*) usage ;;
	esac
	shift
done
[ -n "$kind" ] || usage

swap_used_mib() {
	sysctl -n vm.swapusage 2>/dev/null | awk '{ for (i = 1; i <= NF; i++) if ($i == "used") {
		v = $(i + 2); u = substr(v, length(v)); n = substr(v, 1, length(v) - 1)
		printf "%d\n", n * (u == "G" ? 1024 : u == "K" ? 1 / 1024 : 1) } }'
}

# Processes of an earlier pass: the workloads and their workers by their script, coreslot by the label this script gives it.
earlier_runs() {
	ps -axo pid=,command= | awk -v me="$$" '
		$1 != me && (($2 ~ /[Pp]ython/ && /calibration\/(hog|cpu_control)\.py/) ||
			($2 ~ /coreslot$/ && /--label calibration-/)) { print }'
}

# kill_pid PID WANT: stop the process this script started, if it is still that process.
# shellcheck disable=SC2329 # cleanup calls it, and only the traps run cleanup
kill_pid() {
	local pid=$1 want=$2
	[ -n "$pid" ] && kill -0 "$pid" 2>/dev/null || return 0
	case $(ps -p "$pid" -o command= 2>/dev/null) in *"$want"*) ;; *) return 0 ;; esac
	kill -TERM "$pid" 2>/dev/null
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.5
	done
	kill -KILL "$pid" 2>/dev/null
}

[ -n "$out" ] || out="$here/records/$(date -u +%Y%m%dT%H%M%SZ)-$kind"

earlier=$(earlier_runs)
if [ -n "$earlier" ]; then
	printf 'a calibration process from an earlier pass is still alive; stop it by pid first:\n%s\n' "$earlier" >&2
	[ "$dry" = 1 ] || exit 3
fi

tmp=/tmp/coreslot-calibration.XXXXXX # a dry run only prints it
[ "$dry" = 1 ] || tmp=$(mktemp -d "${TMPDIR:-/tmp}/coreslot-calibration.XXXXXX") || exit 1

if [ "$kind" = ramp ]; then
	work=hog.py
	work_args=(--log "$out/work.jsonl" --pidfile "$tmp/work.pid" --threads "$threads" --step-mib "$step_mib" --hold-s "$hold_s" --slow-s "$slow_s" --slow-pct "$slow_pct")
	[ -z "$max_gib" ] || work_args+=(--max-gib "$max_gib")
	warm_args=(--log "$tmp/warmup.jsonl" --pidfile "$tmp/work.pid" --threads "$threads" --step-mib 512 --hold-s 3600 --seconds "$warm_s")
	warm_mem=512M
	need="${step_mib}M"
	max_s=7200
else
	work=cpu_control.py
	work_args=(--log "$out/work.jsonl" --pidfile "$tmp/work.pid" --seconds "$seconds")
	warm_args=(--log "$tmp/warmup.jsonl" --pidfile "$tmp/work.pid" --seconds "$warm_s")
	[ -z "$procs" ] || { work_args+=(--procs "$procs"); warm_args+=(--procs "$procs"); }
	warm_mem=64M
	need=64M
	max_s=$((${seconds%.*} + 60))
fi
build_cmd=(go build -o "$tmp/coreslot" ./cmd/coreslot)
coreslot_run=(env "CORESLOT_STATE_DIR=$tmp/state" "$tmp/coreslot" run --restart=yes)
run_cmd=("${coreslot_run[@]}" --label "calibration-$kind" --mem "$need" -- "$py" -I "$here/$work" "${work_args[@]}")
# The warm-up gives the label one counted run, so the pass is scored against the label's own normal
# share. The observer is the first and oldest hold, never a victim, and keeps the sampling going.
warm_cmd=("${coreslot_run[@]}" --label "calibration-$kind" --mem "$warm_mem" -- "$py" -I "$here/$work" "${warm_args[@]}")
observer_cmd=("${coreslot_run[@]}" --label calibration-observer --mem 64M -- sleep $((max_s + after_s + 60)))

echo "state dir: $tmp/state, config.toml: [watch] mode = \"observe\", sample_log = true"
echo "build:     (cd $repo && $(printf "%q " "${build_cmd[@]}" | sed "s/ $//"))"
echo "warm-up:   $(printf "%q " "${warm_cmd[@]}" | sed "s/ $//")"
if [ "$kind" = ramp ]; then
	echo "observer:  $(printf "%q " "${observer_cmd[@]}" | sed "s/ $//"), stopped $after_s s after the workload"
fi
echo "run:       $(printf "%q " "${run_cmd[@]}" | sed "s/ $//")"
echo "guard:     stop the workload by the pid in $tmp/work.pid after $max_s s or when swap used grows by $swap_stop_mib MiB; also at exit and on INT, TERM, HUP"
echo "record:    $out (watch.jsonl from $tmp/state/log/watch.jsonl, work.jsonl, meta.json)"
[ "$dry" = 1 ] && exit 0

cpid="" opid="" status="" reason="" warm_share=null
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
swap0=$(swap_used_mib)

# shellcheck disable=SC2329 # runs from the traps below, and kill_pid with it
cleanup() {
	trap - EXIT
	trap '' INT TERM HUP # a second signal must not cut the cleanup short
	kill_pid "$(cat "$tmp/work.pid" 2>/dev/null)" "$work"
	kill_pid "$cpid" coreslot
	kill_pid "$opid" coreslot
	if [ -d "$out" ]; then
		[ ! -f "$tmp/state/log/watch.jsonl" ] || cp "$tmp/state/log/watch.jsonl" "$out/watch.jsonl"
		printf '{"kind":"%s","machine":"%s","os":"%s","ncpu":%s,"mem_bytes":%s,"threads":%s,"warmup_share":%s,"slow_s":%s,"slow_pct":%s,"after_s":%s,"swap_used_mib_at_start":%s,"coreslot":"%s","started":"%s","exit":"%s","stopped_by":"%s"}\n' \
			"$kind" "$(sysctl -n hw.model 2>/dev/null || uname -m)" "$(sw_vers -productVersion 2>/dev/null || uname -sr)" \
			"$(sysctl -n hw.ncpu 2>/dev/null || echo 0)" "$(sysctl -n hw.memsize 2>/dev/null || echo 0)" \
			"$([ "$kind" = ramp ] && echo "$threads" || echo 0)" "$warm_share" \
			"$([ "$kind" = ramp ] && echo "$slow_s" || echo 0)" "$([ "$kind" = ramp ] && echo "$slow_pct" || echo 0)" \
			"$([ "$kind" = ramp ] && echo "$after_s" || echo 0)" "${swap0:-0}" \
			"$("$tmp/coreslot" version 2>/dev/null)" "$started" "$status" "$reason" >"$out/meta.json"
		echo "record: $out"
	fi
	rm -rf "$tmp"
	left=$(earlier_runs)
	[ -z "$left" ] || printf 'warning: calibration processes are still alive; stop them by pid:\n%s\n' "$left" >&2
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

mkdir -p "$out" "$tmp/state" || exit 1
chmod 700 "$tmp/state"
printf '[watch]\nmode = "observe"\nsample_log = true\n' >"$tmp/state/config.toml"
(cd "$repo" && "${build_cmd[@]}") || exit 1

echo "warm-up: $warm_s s"
"${warm_cmd[@]}" &
cpid=$!
wait "$cpid"
cpid=""
# the share the shipped code reads back: the tick_share of the one counted run
warm_share=$("$py" -I -c 'import json, sys
for line in open(sys.argv[1]):
    r = json.loads(line)
    if r.get("label") == sys.argv[2] and not r.get("fail_open") and not r.get("stops") and r.get("tick_share") is not None:
        print(r["tick_share"])
        break
else:
    print("null")' "$tmp/state/log/runs.jsonl" "calibration-$kind" 2>/dev/null || echo null)
if [ "$warm_share" = null ]; then
	echo "the warm-up left no counted run with a tick_share in the run log" >&2
	exit 1
fi
echo "warm-up share: $warm_share"
rm -f "$tmp/state/log/watch.jsonl" # the record starts with the scored pass
if [ "$kind" = ramp ]; then
	"${observer_cmd[@]}" &
	opid=$!
	for _ in $(seq 30); do
		[ ! -f "$tmp/state/log/watch.jsonl" ] || break
		sleep 0.5
	done
	[ -f "$tmp/state/log/watch.jsonl" ] || { echo "the observer took no sample" >&2; exit 1; }
fi

"${run_cmd[@]}" &
cpid=$!
end=$((SECONDS + max_s))
while kill -0 "$cpid" 2>/dev/null; do
	sleep 1
	now=$(swap_used_mib)
	if [ -n "$now" ] && [ -n "$swap0" ] && [ $((now - swap0)) -gt "$swap_stop_mib" ]; then
		reason=swap
		break
	fi
	if [ "$SECONDS" -ge "$end" ]; then
		reason="time"
		break
	fi
done
if [ -n "$reason" ]; then
	echo "stopping the workload: $reason"
	kill_pid "$(cat "$tmp/work.pid" 2>/dev/null)" "$work"
	wait "$cpid"
else
	wait "$cpid"
	status=$?
fi
cpid=""
if [ -n "$opid" ]; then
	echo "observing for $after_s s after the stop"
	until_s=$((SECONDS + after_s))
	while [ "$SECONDS" -lt "$until_s" ]; do sleep 1; done # not one sleep: a trap waits for it to end
fi
exit 0
