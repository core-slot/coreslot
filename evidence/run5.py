#!/usr/bin/env python3
"""Acceptance run 5: coreslot's live memory size against its static size and a semaphore, on run
4's stand with run 4's needs, in counterbalanced blocks.

usage:
  run5.py verdict STAND OUT FIRST LAST [--as-block B]
  run5.py summary STAND OUT BASELINE [--dry]

STAND is run 4's stand file (run4.py's docstring). OUT holds needs.toml, run 4's frozen needs
file, and receives results.jsonl. BASELINE is the directory whose learn.jsonl holds the gates'
alone runs in their F form, the false-failure baseline (run 4's, for the verdict).

The arms: KS runs every gate as `coreslot run --label <label> -- <argv>` with cpu = "140%",
mem = "60%" and `[memory] live = false`; KL the same with `live = true`; S holds a heavy gate on
one of 2 slots of a counting semaphore. Everything else is run4.py's: cs() and its state
directory guard, the pacing, cooldown and probe, the needs copy and hash check, the invalid rules.

  verdict  blocks FIRST..LAST of three sets in the order order_for gives the block (or block B's,
           with --as-block B), offsets from the seed run5-<block>; appends to OUT/results.jsonl,
           skipping each (block, arm) already recorded. Each record carries "static", the cpu and
           mem sizes `coreslot status --json` reports for the shares, which the decomposition
           replays.
  summary  OUT/summary.json and OUT/summary.txt from results.jsonl; exits 2 while fewer than 12
           blocks are valid and the re-run cap is not spent.
"""
import argparse
import json
import os
import signal
import statistics
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import acceptance  # noqa: E402  (probe)
import overload  # noqa: E402  (load)
import run4  # noqa: E402  (cs, run_set, order_for, warn_s, critical_s, false_failures, ...)

ARMS = ("KS", "KL", "S")
RUN4_ARM = {"F": "KS", "S": "S", "K": "KL"}  # run 4's order table, its arms renamed
SHARE = {"cpu": "140%", "mem": "60%"}
KSHARE = {"KS": {**SHARE, "live": False}, "KL": {**SHARE, "live": True}}
SLOTS = 2
NAMES = {"KS": "KS-140%-60%", "KL": "KL-140%-60%", "S": f"S{SLOTS}"}
BLOCKS, RERUN_CAP, ALPHA = run4.BLOCKS, run4.RERUN_CAP, run4.ALPHA
PRICE_WARN_S = 10  # a sixth of the 58 s median of warn-or-worse seconds F had in run 4
KEYS = ("M", "MC", "C", "HM", "HMC", "HC", "latency")


def order_for(block):
    return [RUN4_ARM[a] for a in run4.order_for(block)]


# --- verdict ----------------------------------------------------------------------------------

def static_sizes(cfg):
    """The cpu and mem sizes coreslot reports for SHARE, in tokens."""
    os.makedirs(run4.checked_state(cfg), mode=0o700, exist_ok=True)
    run4.write_config(cfg, SHARE["cpu"], SHARE["mem"])
    r = run4.cs(cfg, "status", "--json", capture_output=True, text=True)
    if r.returncode:
        sys.exit(f"run5: status --json exited {r.returncode}")
    res = json.loads(r.stdout)["resources"]
    return {"cpu": res["cpu"]["size"], "mem": res["mem"]["size"]}


def verdict(cfg, out, first, last, as_block):
    path, needs = os.path.join(out, "results.jsonl"), os.path.join(out, "needs.toml")
    if not os.path.exists(needs):
        sys.exit(f"run5: {needs} is missing")
    done = {(r["block"], r["arm"]) for r in run4.read_jsonl(path)}
    pending = [(b, arm) for b in range(first, last + 1) for arm in order_for(as_block or b)
               if (b, arm) not in done]
    if not pending:
        return
    static, baseline = static_sizes(cfg), acceptance.probe()
    for b, arm in pending:
        src = as_block or b
        run4.run_set(cfg, path, arm=arm, offsets=run4.offsets_for(cfg, f"run5-{src}"),
                     meta={"block": b, "as_block": as_block, "order": " ".join(order_for(src)),
                           "config": NAMES[arm], "static": static},
                     baseline=baseline, slots=SLOTS, kshare=KSHARE.get(arm), needs=needs)


# --- the wait decomposition -------------------------------------------------------------------

def decompose(rec, static):
    """What each run of a coreslot set waited for, replayed against the static sizes, as run 4's
    wait decomposition reconstructed it. A run queues at its gate's start, is admitted after its
    run-log line's waited_s and holds its grant for ran_s. Each stretch of its wait between two
    queue, admission or end events is classed by what the static budget lacked for its own need
    (the minimum): M its mem only, MC its mem and cpu, C its cpu only; or, when it fitted, by
    what the head of the queue (the earliest queued waiting run) lacked: HM, HMC, HC; latency
    when nothing was lacking. above_static counts the runs whose grant and the grants held at
    their admission together exceed the static mem size."""
    pool, runs = list(rec["runlog"]), []
    for g in rec["gates"]:
        dur, lab = g["end"] - g["start"], "bench:" + g["gate"]
        cands = [e for e in pool if e["label"] == lab]
        if not cands:
            continue
        e = min(cands, key=lambda e: abs(e["waited_s"] + e["ran_s"] - dur))
        pool.remove(e)
        q = g["start"]
        a = q + e["waited_s"]
        runs.append({"q": q, "a": a, "end": a + e["ran_s"], "grant": e["grant"],
                     "need": (e["need"]["cpu"][0], e["need"]["mem"][0])})
    times = sorted({x for r in runs for x in (r["q"], r["a"], r["end"])})
    totals = dict.fromkeys(KEYS, 0.0)
    for run in runs:
        for t0, t1 in zip(times, times[1:]):
            if t0 < run["q"] or t1 > run["a"] + 1e-9:
                continue
            mid = (t0 + t1) / 2
            held = [x for x in runs if x["a"] <= mid < x["end"]]
            hc = sum(x["grant"].get("cpu", 0) for x in held)
            hm = sum(x["grant"].get("mem", 0) for x in held)

            def fits(x):
                c, m = x["need"]
                return hm + m <= static["mem"], hc + c <= static["cpu"]

            fm, fc = fits(run)
            head = min((x for x in runs if x["q"] <= mid < x["a"]), key=lambda x: x["q"])
            if not fm:
                key = "M" if fc else "MC"
            elif not fc:
                key = "C"
            elif head is not run:
                hm_, hc_ = fits(head)
                key = ("HM" if hc_ else "HMC") if not hm_ else "HC" if not hc_ else "latency"
            else:
                key = "latency"
            totals[key] += t1 - t0
    above = sum(1 for run in runs
                if run["grant"].get("mem", 0) + sum(x["grant"].get("mem", 0) for x in runs
                                                    if x is not run
                                                    and x["a"] <= run["a"] < x["end"])
                > static["mem"])
    return {"totals": totals, "above_static": above}


# --- analysis ---------------------------------------------------------------------------------

def mechanism(pairs):
    """KL's summed waited_free_s below KS's, per block; a tie is not a win."""
    w = sum((kl["waited_free_s"] or 0) < (ks["waited_free_s"] or 0) for ks, kl in pairs)
    n = len(pairs)
    p = run4.sign_test(w, n) if n else 1.0
    return {"wins": w, "n": n, "p": round(p, 4), "pass": p < ALPHA}


def price(pairs, baseline):
    """KL has no critical second and no false failure in any block, and the median over blocks
    of KL's warn-or-worse seconds less KS's is at most PRICE_WARN_S."""
    diffs = [run4.warn_s(kl) - run4.warn_s(ks) for ks, kl in pairs]
    med = statistics.median(diffs) if diffs else None
    crit = sum(run4.critical_s(kl) for _, kl in pairs)
    ff = sum(run4.false_failures(kl["gates"], baseline) for _, kl in pairs)
    return {"warn_diffs_s": diffs, "warn_diff_median_s": med, "critical_s": crit,
            "false_failures": ff,
            "pass": crit == 0 and ff == 0 and med is not None and med <= PRICE_WARN_S}


def analyse(recs, baseline, dry):
    by = {}
    for r in recs:
        if r.get("block") is not None:
            by.setdefault(r["block"], {})[r["arm"]] = r
    complete = [b for b in sorted(by) if all(a in by[b] for a in ARMS)]
    valid = [b for b in complete if all(by[b][a]["valid"] for a in ARMS)][:BLOCKS]
    reruns = [b for b in by if b > BLOCKS]
    full, spent = len(valid) >= BLOCKS, len(reruns) >= RERUN_CAP
    if not full and not spent and not dry:
        print(f"run5: verdict incomplete: {len(valid)} valid blocks, {len(reruns)} of"
              f" {RERUN_CAP} re-runs; no analysis yet", file=sys.stderr)
        sys.exit(2)
    rs = {a: [by[b][a] for b in valid] for a in ARMS}

    def med(xs):
        xs = list(xs)
        return statistics.median(xs) if xs else None

    per_arm = {a: {"mean_verdict_s": med(r["mean_verdict_s"] for r in rs[a]),
                   "makespan_s": med(r["makespan_s"] for r in rs[a]),
                   "warn_s": med(run4.warn_s(r) for r in rs[a]),
                   "critical_s": med(run4.critical_s(r) for r in rs[a]),
                   "false_failures_total": sum(run4.false_failures(r["gates"], baseline)
                                               for r in rs[a])}
               for a in ARMS}
    decomposition = {}
    for a in ("KS", "KL"):
        per_arm[a]["waited_free_s"] = med(r["waited_free_s"] or 0 for r in rs[a])
        ds = [decompose(r, r["static"]) for r in rs[a]]
        decomposition[a] = {**{k: round(sum(d["totals"][k] for d in ds), 1) for k in KEYS},
                            "above_static": sum(d["above_static"] for d in ds)}
    ratios = {f"KL/{o} {key}": [round(by[b]["KL"][key] / by[b][o][key], 3) for b in valid]
              for o in ("KS", "S") for key in ("mean_verdict_s", "makespan_s")}
    s = {"blocks": valid, "reruns": sorted(reruns), "per_arm": per_arm, "ratios": ratios,
         "decomposition": decomposition, "tests": None, "outcome": None, "incomplete": None}
    if not full and not dry:
        s["incomplete"] = f"incomplete: {len(valid)} valid blocks; no test"
        return s
    pairs = list(zip(rs["KS"], rs["KL"]))
    s["tests"] = {"mechanism": mechanism(pairs), "price": price(pairs, baseline)}
    mech, pr = s["tests"]["mechanism"]["pass"], s["tests"]["price"]["pass"]
    if mech and pr:
        s["outcome"] = ["Mechanism and price pass: the live size does what it is for at an"
                        " acceptable cost."]
    else:
        s["outcome"] = ((["Mechanism fails: the live size does not remove the waits on this"
                          " machine."] if not mech else [])
                        + (["Price fails: the live size costs more memory pressure than allowed;"
                            " reported as is."] if not pr else []))
    return s


def markdown(s, recs):
    lines = ["# Acceptance run 5: summary", "",
             f"Valid blocks used: {s['blocks']}; re-runs: {s['reruns']}.", "",
             "Medians over the valid blocks (false failures as a total):", "",
             "| Arm | Mean verdict (s) | Makespan (s) | Warn+ (s) | Critical (s) | False failures"
             " | waited_free_s |", "|---|---|---|---|---|---|---|"]
    for a in ARMS:
        p = s["per_arm"][a]
        lines.append(f"| {a} | {p['mean_verdict_s']} | {p['makespan_s']} | {p['warn_s']} |"
                     f" {p['critical_s']} | {p['false_failures_total']} |"
                     f" {p.get('waited_free_s', '-')} |")
    lines += ["", "Per-block ratios (valid blocks):", ""]
    lines += [f"- {k}: {v}" for k, v in s["ratios"].items()]
    lines += ["", "Wait decomposition against the static sizes, summed over the valid blocks (s;"
              " above static: runs):", "",
              "| Arm | M | MC | C | HM | HMC | HC | latency | above static |",
              "|---|---|---|---|---|---|---|---|---|"]
    for a, d in s["decomposition"].items():
        lines.append(f"| {a} | " + " | ".join(str(d[k]) for k in KEYS + ("above_static",)) + " |")
    lines += ["", "Every verdict set:", "",
              "| Block | As block | Order | Arm | Mean verdict (s) | Makespan (s) | Warn+ (s)"
              " | Critical (s) | waited_free_s | Failed | Flags | Valid |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|"]
    for r in sorted((r for r in recs if r.get("block") is not None),
                    key=lambda r: (r["block"], r["order"].split().index(r["arm"]))):
        failed = ", ".join(f["gate"] for f in r["failures"]) or "none"
        wfree = "-" if r["waited_free_s"] is None else r["waited_free_s"]
        lines.append(f"| {r['block']} | {r['as_block'] or ''} | {r['order']} | {r['arm']} |"
                     f" {r['mean_verdict_s']} | {r['makespan_s']} | {run4.warn_s(r)} |"
                     f" {run4.critical_s(r)} | {wfree} |"
                     f" {failed} | {', '.join(r['flags'])} |"
                     f" {'yes' if r['valid'] else 'no: ' + '; '.join(r['invalid'])} |")
    lines.append("")
    if s["incomplete"]:
        return "\n".join(lines + [s["incomplete"], ""])
    m, p = s["tests"]["mechanism"], s["tests"]["price"]
    lines += ["Criteria:", "",
              f"- Mechanism (KL's summed waited_free_s below KS's, one-sided exact sign test,"
              f" alpha 0.05): {m['wins']} of {m['n']}, p = {m['p']:.4f},"
              f" {'pass' if m['pass'] else 'fail'}",
              f"- Price (KL: no critical second, no false failure, median of KL's warn+ less KS's"
              f" at most {PRICE_WARN_S} s): median {p['warn_diff_median_s']} s, critical"
              f" {p['critical_s']} s, false failures {p['false_failures']},"
              f" {'pass' if p['pass'] else 'fail'}",
              "", "Outcome:", ""] + [f"- {o}" for o in s["outcome"]] + [""]
    return "\n".join(lines)


def summary(out, baseline_dir, dry):
    recs = run4.read_jsonl(os.path.join(out, "results.jsonl"))
    s = analyse(recs, run4.gate_baseline(baseline_dir), dry)
    json.dump(s, open(os.path.join(out, "summary.json"), "w"), indent=1)
    md = markdown(s, recs)
    with open(os.path.join(out, "summary.txt"), "w") as f:
        f.write(md)
    print(md)


def main():
    ap = argparse.ArgumentParser(description=(__doc__ or "").splitlines()[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    v = sub.add_parser("verdict")
    v.add_argument("stand")
    v.add_argument("out")
    v.add_argument("first", type=int)
    v.add_argument("last", type=int)
    v.add_argument("--as-block", type=int, default=None)
    s = sub.add_parser("summary")
    s.add_argument("stand")
    s.add_argument("out")
    s.add_argument("baseline")
    s.add_argument("--dry", action="store_true")
    args = ap.parse_args()
    cfg = overload.load(args.stand)

    def interrupted(*_):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    if args.cmd == "verdict":
        verdict(cfg, args.out, args.first, args.last, args.as_block)
    else:
        if args.dry and not cfg.get("dry"):
            sys.exit("run5: --dry is for a dry stand only")
        summary(args.out, args.baseline, args.dry)


if __name__ == "__main__":
    main()
