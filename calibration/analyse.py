#!/usr/bin/env python3
"""Score candidate watchdog thresholds on recorded calibration runs.

usage: analyse.py RECORD_DIR... [--yellow-points N] [--red-points N] [--red-samples N]
                                [--gate-pages-s N]

A record directory holds watch.jsonl (the watchdog's sample log), work.jsonl (the hog's or the
contention control's own log) and, optionally, meta.json (machine, OS, threads, and the share a
warm-up run taught the watchdog as the hog's normal S0). A directory
with none of these is searched for record directories one level down.

Every sample of the watchdog that falls inside a run is joined to the workload's speed over the
sample's period and put into a band:
  full      the hog ran at 90 % or more of its first step's speed
  slowing   50 to 90 %
  slow      30 to 50 %, or under 30 % with no paging (a cache-bound hog slows without any)
  cliff     under 30 % with paging, or decompressions at CLIFF_DECOMP_S or more per second
  control   a sample of the contention control, where nothing may fire
Gaps (level unknown) are skipped. The tables then count, per band, the samples a candidate would
fire on, replaying the yellow and red rules over each run as the detector applies them; the replay
at the chosen values is checked against the level the detector logged, and every sample where they
differ is printed. calm_s is scored from the logged levels alone. The floor is not calibrated.
"""
import argparse
import json
import os
import statistics
import sys

CLIFF_DECOMP_S = 100_000
CLIFF_SPEED = 0.30
PAGING_EVIDENCE_S = 1000
BANDS = ("full", "slowing", "slow", "cliff", "control")
PRE_CLIFF = ("full", "slowing", "slow", "control")
MAX_PERIOD_NS = 6_000_000_000

INTERIM = {"yellow_points": 10, "red_points": 30, "red_samples": 10, "gate_pages_s": 1000}
CLEAR_SAMPLES = 15  # samples in a row below yellow_points that clear yellow
CANDIDATES = {
    "yellow_points": (5, 10, 15, 20, 30),
    "red_points": (10, 15, 20, 30, 40, 50),
    "red_samples": (3, 5, 10, 15, 20),
    "gate_pages_s": (100, 300, 1000, 3000, 10000),
    "reserve_delta_mib": (-2048, -1024, 0, 1024, 2048),
    "calm_s": (15, 30, 60, 90),
}


def read_jsonl(path):
    """The JSON objects of a file; a blank or torn line (a killed writer's last) is skipped."""
    rows = []
    try:
        f = open(path)
    except FileNotFoundError:
        return rows
    with f:
        for line in f:
            try:
                row = json.loads(line)
            except ValueError:
                continue
            if isinstance(row, dict):
                rows.append(row)
    return rows


class Record:
    def __init__(self, path):
        self.path = path
        self.name = os.path.basename(os.path.normpath(path))
        self.watch = read_jsonl(os.path.join(path, "watch.jsonl"))
        self.work = read_jsonl(os.path.join(path, "work.jsonl"))
        self.meta = {}
        try:
            with open(os.path.join(path, "meta.json")) as f:
                self.meta = json.load(f)
        except (FileNotFoundError, ValueError):
            pass
        self.is_control = not any(e.get("event") == "rate" for e in self.work)
        self.stop_ns = max((e["t_ns"] for e in self.work if e.get("event") == "stop"), default=None)
        self.samples, self.baseline = join(self)

    def s0(self):
        share = self.meta.get("warmup_share")
        return f", S0 learned {share:.3f} by a warm-up" if share is not None else ""

    def label(self):
        m = self.meta
        bits = [m.get(k) for k in ("machine", "os") if m.get(k)]
        if m.get("threads"):
            bits.append(f"{m['threads']} threads")
        return self.name + (" (" + ", ".join(map(str, bits)) + ")" if bits else "")


def band_of(speed, paging_s):
    if paging_s >= CLIFF_DECOMP_S or (speed < CLIFF_SPEED and paging_s >= PAGING_EVIDENCE_S):
        return "cliff"
    if speed >= 0.9:
        return "full"
    if speed >= 0.5:
        return "slowing"
    return "slow"


def join(rec):
    """The record's in-run watchdog samples with a band each, and the hog's reference speed."""
    start = min((e["t_ns"] for e in rec.work if e.get("event") == "start"), default=None)
    if start is None:
        return [], None
    stop = rec.stop_ns if rec.stop_ns is not None else max(e["t_ns"] for e in rec.work)
    rates = [e for e in rec.work if e.get("event") == "rate" and e.get("hot_mib", 0) > 0]
    baseline = None
    if rates:
        lowest = min(e["hot_mib"] for e in rates)
        first = [e["touches_s"] for e in rates if e["hot_mib"] == lowest]
        baseline = statistics.median(first)
    out, prev = [], None
    for w in rec.watch:
        t = w["t_ns"]
        period = min(t - prev, MAX_PERIOD_NS) if prev is not None else 2_000_000_000
        prev = t
        if w.get("level") == "unknown":
            continue
        paging = (w.get("decomp_s") or 0) + (w.get("swapins_s") or 0)
        s = {"t_ns": t, "E": w["excess"], "paging": paging, "level": w["level"], "room_mib": w.get("room_mib", 0),
             "hot_mib": None, "speed": None}
        if rec.is_control:
            if not start < t <= stop + period:
                continue
            s["band"] = "control"
        else:
            window = [e for e in rates if t - period < e["t_ns"] <= t]
            if not window or not baseline:
                continue
            s["speed"] = statistics.mean(e["touches_s"] for e in window) / baseline
            s["hot_mib"] = window[-1]["hot_mib"]
            s["band"] = band_of(s["speed"], paging)
        out.append(s)
    return out, baseline


def gate_open(s, gate_pages_s):
    return s["paging"] >= gate_pages_s


def percentile(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(q * len(xs)))]


def band_table(samples, gate_pages_s):
    lines = [f"{'band':8} {'n':>4} {'E med':>7} {'E p90':>7} {'E max':>7} {'gate open':>9} {'room med':>9}"]
    for b in BANDS:
        xs = [s for s in samples if s["band"] == b]
        if not xs:
            continue
        e = [s["E"] for s in xs]
        opened = sum(gate_open(s, gate_pages_s) for s in xs) / len(xs)
        lines.append(f"{b:8} {len(xs):4d} {statistics.median(e):7.1f} {percentile(e, 0.9):7.1f} {max(e):7.1f} "
                     f"{opened:8.0%} {statistics.median(s['room_mib'] for s in xs):9.0f}")
    return lines


def replay(seq, c):
    """The level of each sample at the values c, as the detector computes it from E and the gate
    (the floor left out): yellow latches on 2 hot samples of the last 3 and clears after
    CLEAR_SAMPLES in a row below yellow_points; red is the leaky bucket, saturated at red_samples.
    A gap (level unknown) stays unknown, counts as a sample that is not hot and keeps the rest."""
    out, hot, yellow, below, bucket = [], [], False, 0, 0.0
    for s in seq:
        if s.get("level") == "unknown":
            hot = (hot + [False])[-3:]
            out.append("unknown")
            continue
        gate = gate_open(s, c["gate_pages_s"])
        hot = (hot + [gate and s["E"] >= c["yellow_points"]])[-3:]
        if gate and s["E"] >= c["red_points"]:
            bucket = min(bucket + 1, c["red_samples"])
        else:
            bucket = max(0.0, bucket - 0.5)
        if sum(hot) >= 2:
            yellow, below = True, 0
        elif s["E"] >= c["yellow_points"]:
            below = 0
        elif yellow:
            below += 1
            yellow = below < CLEAR_SAMPLES
        out.append("red" if bucket >= c["red_samples"] else "yellow" if yellow else "green")
    return out


def first_red(seq, c):
    """The first sample the replay at c makes red, or None."""
    return next((s for s, level in zip(seq, replay(seq, c)) if level == "red"), None)


def mismatches(rec, c):
    """The logged samples whose level the replay at c gives otherwise, as (t_s, replayed, logged);
    a floor sample is skipped, since the replay leaves the floor out."""
    seq = [{"t_ns": w["t_ns"], "E": w["excess"], "paging": (w.get("decomp_s") or 0) + (w.get("swapins_s") or 0),
            "level": w.get("level")} for w in rec.watch]
    t0 = seq[0]["t_ns"] if seq else 0
    return [((s["t_ns"] - t0) / 1e9, got, s["level"]) for s, got in zip(seq, replay(seq, c))
            if s["level"] != "floor" and got != s["level"]]


def calm_after_stop(rec):
    """From the levels the detector logged after the hog's stop: seconds until the level first turned
    green, and seconds it then stayed green (to the next level that was not, or the record's end)."""
    if rec.stop_ns is None:
        return None
    post = [w for w in rec.watch if w["t_ns"] > rec.stop_ns and w.get("level") != "unknown"]
    i = next((i for i, w in enumerate(post) if w["level"] == "green"), None)
    if i is None:
        return None
    end = next((w["t_ns"] for w in post[i:] if w["level"] != "green"), post[-1]["t_ns"])
    return (post[i]["t_ns"] - rec.stop_ns) / 1e9, (end - post[i]["t_ns"]) / 1e9


def room_negative(seq, delta):
    """The first sample where admission room would be negative with the reserve raised by delta MiB."""
    return next((s for s in seq if s["room_mib"] - delta < 0), None)


def by_band(samples):
    counts = {b: 0 for b in BANDS}
    for s in samples:
        counts[s["band"]] += 1
    return counts


def verdict(records, fired):
    """fired maps a record to the samples it fires on: where it fires in the cliff band and where not."""
    with_cliff = [r for r in records if any(s["band"] == "cliff" for s in r.samples)]
    hit = [r for r in with_cliff if any(s["band"] == "cliff" for s in fired(r))]
    early = [f"{r.name}:{b}" for r in records for b in PRE_CLIFF if any(s["band"] == b for s in fired(r))]
    text = f"cliff {len(hit)}/{len(with_cliff)} ramps"
    return text + ("; pre-cliff in " + ", ".join(early) if early else "; no pre-cliff")


def table(title, rows, header):
    return ["", title, header] + rows


def report(records, chosen=None):
    chosen = {**INTERIM, **(chosen or {})}
    gate = chosen["gate_pages_s"]
    out = []
    for r in records:
        out.append(f"== {r.label()}: {len(r.samples)} samples in the run, "
                   f"{'control' if r.is_control else f'reference {r.baseline:.0f} touches/s{r.s0()}'}")
        out += band_table(r.samples, gate)
        calm = calm_after_stop(r)
        if not r.is_control:
            out.append("after the stop: " + ("never green" if calm is None else
                       f"green after {calm[0]:.0f} s, then green for {calm[1]:.0f} s"))
        diff = mismatches(r, chosen)
        out.append(f"replay against the logged level: {len(diff)} mismatches")
        out += [f"  {t:7.0f} s: replay {got}, logged {logged}" for t, got, logged in diff[:20]]
        out.append("")
    pooled = [s for r in records for s in r.samples]
    out.append(f"== all {len(records)} runs")
    out += band_table(pooled, gate)

    head = f"{'value':>8} " + " ".join(f"{b:>8}" for b in BANDS) + "  verdict"

    def counts_row(value, fired):
        counts = by_band([s for r in records for s in fired(r)])
        return f"{value:8} " + " ".join(f"{counts[b]:8d}" for b in BANDS) + "  " + verdict(records, fired)

    def held(r, v):
        c = {**chosen, "yellow_points": v}
        return [s for s, level in zip(r.samples, replay(r.samples, c)) if level != "green"]

    out += table(f"yellow_points: samples held yellow or red (gate_pages_s={gate})",
                 [counts_row(v, lambda r, v=v: held(r, v)) for v in CANDIDATES["yellow_points"]], head)
    out += table(f"red_points: samples with E at or over the value and the gate open (gate_pages_s={gate})",
                 [counts_row(v, lambda r, v=v: [s for s in r.samples if s["E"] >= v and gate_open(s, gate)])
                  for v in CANDIDATES["red_points"]], head)

    def red_row(v):
        marks = []
        for r in records:
            s = first_red(r.samples, {**chosen, "red_samples": v})
            marks.append(f"{r.name}:{s['band'] if s else 'never'}")
        return f"{v:8} " + "  ".join(marks)

    out += table(f"red_samples: band of the first red per run (red_points={chosen['red_points']})",
                 [red_row(v) for v in CANDIDATES["red_samples"]], "   value")
    out += table("gate_pages_s: share of each band's samples with the gate open",
                 [f"{v:8} " + " ".join(f"{sum(gate_open(s, v) for s in pooled if s['band'] == b) / max(1, sum(s['band'] == b for s in pooled)):8.0%}"
                                        for b in BANDS) for v in CANDIDATES["gate_pages_s"]], head.split("  verdict")[0])
    out += table("reserve raised by value (MiB): samples where room would be negative",
                 [counts_row(v, lambda r, v=v: [s for s in r.samples if s["room_mib"] - v < 0])
                  for v in CANDIDATES["reserve_delta_mib"]], head)
    ramps = [r for r in records if not r.is_control]
    calm = [calm_after_stop(r) for r in ramps]
    out += table("calm_s: ramps whose logged level, once green after the stop, stayed green for the value",
                 [f"{v:8} {sum(x is not None and x[1] >= v for x in calm)}/{len(ramps)}" for v in CANDIDATES["calm_s"]],
                 "   value")
    return "\n".join(out)


def find_records(paths):
    found = []
    for p in paths:
        if os.path.exists(os.path.join(p, "watch.jsonl")):
            found.append(p)
        else:
            found += sorted(os.path.join(p, d) for d in os.listdir(p) if os.path.exists(os.path.join(p, d, "watch.jsonl")))
    return found


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("records", nargs="+")
    for k, v in INTERIM.items():
        ap.add_argument("--" + k.replace("_", "-"), type=int, default=v)
    a = ap.parse_args(argv)
    paths = find_records(a.records)
    if not paths:
        ap.error("no record directory with a watch.jsonl found")
    print(report([Record(p) for p in paths], {k: getattr(a, k) for k in INTERIM}))


if __name__ == "__main__":
    main(sys.argv[1:])
