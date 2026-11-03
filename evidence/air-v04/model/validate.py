import json, os, sys
from sim import *
import sim
ETA2, ETA1, BETA, MOK = [float(x) for x in sys.argv[1:5]]
sim.PAGEMODE = sys.argv[5]
R = os.environ.get("AIR_RECORDS", os.path.dirname(os.path.dirname(os.path.abspath(__file__)))).rstrip(os.sep) + os.sep  # holds record/ and record-2/
G2L = {"vscode-typecheck": "TC", "fastapi-ruff": "RF", "fastapi-tests": "FT", "sqlalchemy-orm": "SO"}
def sets(rec):
    return [json.loads(l) for l in open(R + rec + "/sets.jsonl")]
def pytest_s(rec, tag, arm, agent, gate):
    import re
    f = f"{R}{rec}/sets/{tag}-{arm}/{agent}-{gate}.out"
    m = re.findall(r" in ([0-9.]+)s", open(f).read()[-400:])
    return float(m[-1])
rows = []
for rec, rid in (("record-2", 2), ("record", 1)):
    for d in sets(rec):
        if "stagger_s" in d:  # the counted pairs ran later, staggered: no S slot-grant time for tsc
            continue
        starts = []
        rl = {}
        for r in d.get("runlog") or []:
            rl.setdefault(r["label"], []).append(r)
        for g in d["gates"]:
            lab = G2L[g["gate"]]
            st = g["start_s"]
            if d["arm"] == "K":
                # start = gate start + waited (match run-log line by ran_s ~ duration)
                cands = rl[g["label"]]
                dur = g["end_s"] - g["start_s"]
                r = min(cands, key=lambda r: abs(r["waited_s"] + r["ran_s"] - dur))
                st += r["waited_s"]
            elif d["arm"] == "S" and lab in ("FT", "SO"):
                st = g["end_s"] - pytest_s(rec, d["tag"], "S", g["agent"], g["gate"]) - 0.7
                st = max(st, g["start_s"])
            elif d["arm"] == "S" and lab == "TC" and d["slots"] == 1 and g["agent"] == "A2":
                st = 8.65
            starts.append((g["agent"], lab, round(st, 2)))
        done = trace(rid, starts, ETA2 if rid == 2 else ETA1, BETA, MOK)
        verd = {}
        for j in done:
            verd[j.agent] = max(verd.get(j.agent, 0), j.end)
        meas = {a: v["verdict_s"] for a, v in d["agents"].items()}
        pm, pk = summary(verd); mm, mk = summary(meas)
        calib = (rid, d["tag"]) in ((2, "sem-pilot"), (2, "tune-S2"), (1, "pair-2"), (1, "pilot"), (1, "pair-3")) and d["arm"] != "F" or (rid, d["tag"]) == (2, "tune-S2")
        print(f"{rec:8s} {d['tag']:12s} {d['arm']}{d.get('slots') or ''} th{d['thermal_start']} mean meas {mm:6.1f} pred {pm:6.1f} ({pm/mm-1:+.0%})  mk meas {mk:6.1f} pred {pk:6.1f} ({pk/mk-1:+.0%}) {'CALIB' if calib else ''}")
        print("      per agent meas", {a: round(v, 1) for a, v in sorted(meas.items())}, "pred", {a: round(v, 1) for a, v in sorted(verd.items())})
