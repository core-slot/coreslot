import itertools, sys
import sim
from sim import simulate, summary
sim.PAGEMODE = "all"
COND = {"run2-heavy": dict(run_id=2, mixname="full", eta=0.80),
        "run1-heavy": dict(run_id=1, mixname="full", eta=0.95),
        "run2-light": dict(run_id=2, mixname="light", eta=0.80),
        "run1-light": dict(run_id=1, mixname="light", eta=0.95)}
POL = {
 "S1": ({"kind": "S", "slots": 1, "inherit": 1}, {}),
 "a K v0.4": ({"kind": "K"}, {}),
 "b width<=1.0C": ({"kind": "K", "width_k": 1.0}, {}),
 "b width<=1.5C": ({"kind": "K", "width_k": 1.5}, {}),
 "b width<=2.0C": ({"kind": "K", "width_k": 2.0}, {}),
 "c nice w=.33 pess": ({"kind": "K"}, {"nice_w": 0.33, "senior_eta": None}),
 "c nice w=.33 opt": ({"kind": "K"}, {"nice_w": 0.33, "senior_eta": 1.0}),
 "c nice w=.1 opt": ({"kind": "K"}, {"nice_w": 0.1, "senior_eta": 1.0}),
 "d SJF": ({"kind": "K", "sjf": 1}, {}),
 "d SJF+b1.0": ({"kind": "K", "sjf": 1, "width_k": 1.0}, {}),
 "d SJF+c.1opt": ({"kind": "K", "sjf": 1}, {"nice_w": 0.1, "senior_eta": 1.0}),
 "e gain-check": ({"kind": "K", "gain": 1}, {}),
}
OBS = {"run2-heavy": ["A1", "A2", "A4", "A3"], "run1-heavy": ["A1", "A4", "A2", "A3"],
       "run2-light": ["A3", "A4"], "run1-light": ["A3", "A4"]}
for cname, c in COND.items():
    agents = ["A3", "A4"] if c["mixname"] == "light" else ["A1", "A2", "A3", "A4"]
    res = {}
    for pname, (pol, kw) in POL.items():
        ms, ks = [], []
        for order in itertools.permutations(agents):
            v, _ = simulate(c["run_id"], c["mixname"], pol, list(order), eta=c["eta"], dt=0.02, **kw)
            m, k = summary(v); ms.append(m); ks.append(k)
        vo, _ = simulate(c["run_id"], c["mixname"], pol, OBS[cname], eta=c["eta"], dt=0.02, **kw)
        mo, ko = summary(vo)
        res[pname] = (sum(ms) / len(ms), min(ms), max(ms), sum(ks) / len(ks), mo, ko)
    print("==", cname)
    s1, k = res["S1"], res["a K v0.4"]
    for p, (m, lo, hi, mk, mo, ko) in res.items():
        print(f"  {p:20s} mean {m:5.1f} [{lo:5.1f}-{hi:5.1f}] mk {mk:5.1f} | /S1 {m/s1[0]:.2f} {mk/s1[3]:.2f} | /K {m/k[0]:.2f} {mk/k[3]:.2f} || obs-order mean {mo:5.1f} mk {ko:5.1f}")
