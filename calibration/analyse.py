#!/usr/bin/env python3
"""Score candidate watchdog thresholds on recorded calibration runs.

usage: analyse.py RECORD_DIR... [--yellow-points N] [--red-points N] [--red-samples N]
                                [--gate-pages-s N] [--floor-mib N]

A record directory holds watch.jsonl (the watchdog's sample log), work.jsonl (the hog's or the
contention control's own log) and, optionally, meta.json (machine, OS, threads). A directory with
none of these is searched for record directories one level down.

Every sample of the watchdog that falls inside a run is joined to the workload's speed over the
sample's period and put into a band:
  full      the hog ran at 90 % or more of its first step's speed
  slowing   50 to 90 %
  slow      30 to 50 %, or under 30 % with no paging (a cache-bound hog slows without any)
  cliff     under 30 % with paging, or decompressions at CLIFF_DECOMP_S or more per second
  control   a sample of the contention control, where nothing may fire
Gaps (level unknown) are skipped. The tables then count, per band, the samples a candidate would
fire on, and replay the yellow and red rules over each run.
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

INTERIM = {"yellow_points": 10, "red_points": 30, "red_samples": 10, "gate_pages_s": 1000, "floor_mib": 2048}
CANDIDATES = {
    "yellow_points": (5, 10, 15, 20, 30),
    "red_points": (10, 15, 20, 30, 40, 50),
    "red_samples": (3, 5, 10, 15, 20),
    "gate_pages_s": (100, 300, 1000, 3000, 10000),
    "floor_mib": (1024, 2048, 4096, 8192),
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
             "swap_used_mib": w.get("swap_used_mib", 0), "headroom_mib": w.get("swap_free_mib", 0) + w.get("vm_free_mib", 0),
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


def yellow_samples(seq, points, gate_pages_s):
    """The samples the yellow rule holds yellow on: E over the points with the gate open in 2 of the
    last 3 samples, cleared after 15 samples below the points."""
    hits, below, active, held = [], 0, False, []
    for s in seq:
        hit = s["E"] >= points and gate_open(s, gate_pages_s)
        hits = (hits + [hit])[-3:]
        if not active and sum(hits) >= 2:
            active, below = True, 0
        elif active:
            below = below + 1 if s["E"] < points else 0
            if below >= 15:
                active, hits = False, []
        if active:
            held.append(s)
    return held


def first_red(seq, points, samples_needed, gate_pages_s):
    """The sample at which the leaky bucket reaches the red samples, or None."""
    bucket = 0.0
    for s in seq:
        hit = s["E"] >= points and gate_open(s, gate_pages_s)
        bucket = bucket + 1 if hit else max(0.0, bucket - 0.5)
        if bucket >= samples_needed:
            return s
    return None


def floor_samples(seq, floor_mib):
    """Samples where headroom is under the floor while swap used grew 256 MiB over the last 20 s."""
    fired = []
    for i, s in enumerate(seq):
        window = [x for x in seq[: i + 1] if s["t_ns"] - x["t_ns"] <= 20_000_000_000]
        if s["headroom_mib"] < floor_mib and s["swap_used_mib"] - window[0]["swap_used_mib"] >= 256:
            fired.append(s)
    return fired


def room_negative(seq, delta):
    """The first sample where admission room would be negative with the reserve raised by delta MiB."""
    return next((s for s in seq if s["room_mib"] - delta < 0), None)


def recovery_s(rec, calm_points=5, run=5):
    """Seconds from the hog's stop to the start of the first run of `run` samples under calm_points."""
    if rec.stop_ns is None:
        return None
    post = [w for w in rec.watch if w["t_ns"] > rec.stop_ns and w.get("level") != "unknown"]
    for i in range(len(post) - run + 1):
        if all(w["excess"] < calm_points for w in post[i: i + run]):
            return (post[i]["t_ns"] - rec.stop_ns) / 1e9
    return None


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
                   f"{'control' if r.is_control else f'reference {r.baseline:.0f} touches/s'}")
        out += band_table(r.samples, gate)
        rec_s = recovery_s(r)
        if not r.is_control:
            out.append(f"after the stop: calm in {'never' if rec_s is None else f'{rec_s:.0f} s'}")
        out.append("")
    pooled = [s for r in records for s in r.samples]
    out.append(f"== all {len(records)} runs")
    out += band_table(pooled, gate)

    head = f"{'value':>8} " + " ".join(f"{b:>8}" for b in BANDS) + "  verdict"

    def counts_row(value, fired):
        counts = by_band([s for r in records for s in fired(r)])
        return f"{value:8} " + " ".join(f"{counts[b]:8d}" for b in BANDS) + "  " + verdict(records, fired)

    out += table(f"yellow_points: samples held yellow (gate_pages_s={gate})",
                 [counts_row(v, lambda r, v=v: yellow_samples(r.samples, v, gate)) for v in CANDIDATES["yellow_points"]], head)
    out += table(f"red_points: samples with E at or over the value and the gate open (gate_pages_s={gate})",
                 [counts_row(v, lambda r, v=v: [s for s in r.samples if s["E"] >= v and gate_open(s, gate)])
                  for v in CANDIDATES["red_points"]], head)

    def red_row(v):
        marks = []
        for r in records:
            s = first_red(r.samples, chosen["red_points"], v, gate)
            marks.append(f"{r.name}:{s['band'] if s else 'never'}")
        return f"{v:8} " + "  ".join(marks)

    out += table(f"red_samples: band of the first red per run (red_points={chosen['red_points']})",
                 [red_row(v) for v in CANDIDATES["red_samples"]], "   value")
    out += table("gate_pages_s: share of each band's samples with the gate open",
                 [f"{v:8} " + " ".join(f"{sum(gate_open(s, v) for s in pooled if s['band'] == b) / max(1, sum(s['band'] == b for s in pooled)):8.0%}"
                                        for b in BANDS) for v in CANDIDATES["gate_pages_s"]], head.split("  verdict")[0])
    out += table("floor_mib: samples where the floor trips",
                 [counts_row(v, lambda r, v=v: floor_samples(r.samples, v)) for v in CANDIDATES["floor_mib"]], head)
    out += table("reserve raised by value (MiB): samples where room would be negative",
                 [counts_row(v, lambda r, v=v: [s for s in r.samples if s["room_mib"] - v < 0])
                  for v in CANDIDATES["reserve_delta_mib"]], head)
    ramps = [r for r in records if not r.is_control]
    rec = [recovery_s(r) for r in ramps]
    out += table("calm_s: ramps calm within the value after the hog stopped",
                 [f"{v:8} {sum(x is not None and x <= v for x in rec)}/{len(ramps)}" for v in CANDIDATES["calm_s"]], "   value")
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
