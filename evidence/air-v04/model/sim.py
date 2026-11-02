"""Fluid processor-sharing model of the air-v04 stand, plus admission policies.

Assumptions (all explicit):
- C = 10 homogeneous cores; P/E asymmetry is NOT identifiable from the records (alone widths 4.9..8.9
  already use E cores), so it is folded into eta.
- Job i alone: duration T_i, width w_i (cores), work = 1. Allocation: weighted water-filling of C,
  each job capped at w_i (weight 1 by default = equal share per job, which matches the measured
  ~4.6-5.1 cores each when two pytest -n auto overlap).
- rate_i = (a_i / w_i) / T_i * eff / pi
  eff = eta when sum of widths of running jobs > C (oversubscribed), else 1.
  pi  = paging penalty = 1 + beta * max(0, summem / M_ok - 1).
- speed: per-run multiplier on T (run-1 machine state slower).
"""
import itertools, math

C = 10.0
PAGEMODE = "all"

def labels(run):
    # alone T (s), width, mem GiB, typical_s (p90 ran_s ceil, coreslot's TypicalS)
    if run == 2:   # record-2 learn medians
        return {"TC": (8.49, 4.91, 8.37, 9), "RF": (0.05, 0.6, 0.02, 1),
                "FT": (13.55, 7.80, 2.44, 14), "SO": (22.21, 8.89, 2.14, 23)}
    # record-1: alone runs inside K sets (pilot-K, pair-3-K: thermal 'fair'); learn was disturbed
    return {"TC": (10.0, 4.85, 8.5, 19), "RF": (0.05, 0.6, 0.02, 1),
            "FT": (16.8, 8.0, 2.44, 18), "SO": (27.7, 8.8, 2.14, 43)}

def mix(name):
    if name == "light":
        return {"A3": ["RF", "FT"], "A4": ["SO"]}
    return {"A1": ["TC"], "A2": ["TC"], "A3": ["RF", "FT"], "A4": ["SO"]}


class Job:
    def __init__(s, agent, lab, spec, arr):
        s.agent, s.lab = agent, lab
        s.T, s.w, s.mem, s.typ = spec
        s.arr, s.start, s.end, s.left, s.weight, s.senior = arr, None, None, 1.0, 1.0, True


def alloc(run, eta, beta, m_ok, c=C, senior_eta=None):
    if not run:
        return {}
    a, free, act = {}, c, list(run)
    while act:
        W = sum(j.weight for j in act)
        capped = [j for j in act if free * j.weight / W >= j.w]
        if not capped:
            for j in act:
                a[j] = free * j.weight / W
            break
        for j in capped:
            a[j] = j.w
            free -= j.w
            act.remove(j)
    over = sum(j.w for j in run) > c + 1e-9
    eff = eta if over else 1.0
    mem = sum(j.mem for j in run)
    pi = 1 + beta * max(0.0, mem / m_ok - 1)
    niced = any(j.weight < 1 for j in run)
    def e(j):
        if over and senior_eta is not None and niced and j.weight == 1:
            return senior_eta
        return eff
    if PAGEMODE == "big":
        return {j: (a[j] / j.w) / j.T * e(j) / (pi if j.mem > 4 else 1.0) for j in run}
    return {j: (a[j] / j.w) / j.T * e(j) / pi for j in run}


def simulate(run_id, mixname, policy, order, eta=1.0, beta=0.0, m_ok=8.5, room0=6.4,
             nice_w=None, senior_eta=None, dt=0.005, speed=1.0, tmax=2000):
    """policy: dict(kind=F|S|K, slots, width_k, sjf, gain). order: agent arrival order at t=0
    (ties broken by this order; under S it is the flock race winner order)."""
    L = {k: (v[0] * speed, v[1], v[2], v[3]) for k, v in labels(run_id).items()}
    chains = mix(mixname)
    pos = {}
    queue, running, done = [], [], []
    t = 0.0
    for i, ag in enumerate(order):
        pos[ag] = 0
        j = Job(ag, chains[ag][0], L[chains[ag][0]], i * 1e-6)
        queue.append(j)

    def room():
        return room0 - sum(j.mem for j in running)

    def W_for(head):
        # coreslot passes(): running sorted by remaining typical; free their need until head fits
        r, w = room(), 0
        for h in sorted(running, key=lambda h: max(0, h.typ - (t - h.start))):
            if r >= head.mem:
                break
            r, w = r + h.mem, max(0, h.typ - (t - h.start))
        return w if r >= head.mem else None

    def cpu_ok(j):
        k = policy.get("width_k")
        if k is None or not running:
            return True
        return sum(x.w for x in running) + j.w <= k * C + 1e-9

    def gain_ok(j):
        # admit alongside running jobs only if the PS+eta model predicts the overlap lowers
        # the summed completion time of {running, j} against j starting after them all
        if not policy.get("gain") or not running:
            return True
        def finish(js, startdelay):
            js = [(x, x.left) for x in js]
            tt, rem, ends = 0.0, {x: l for x, l in js}, {}
            act = [x for x, _ in js if startdelay.get(x, 0) == 0]
            pend = [x for x, _ in js if startdelay.get(x, 0) > 0]
            while act or pend:
                if not act:
                    act, pend = pend, []
                r = alloc(act, eta, beta, m_ok)
                tt += 0.01
                for x in list(act):
                    rem[x] -= r[x] * 0.01
                    if rem[x] <= 0:
                        ends[x] = tt
                        act.remove(x)
            return sum(ends.values())
        both = finish(running + [j], {})
        serial = finish(running + [j], {j: 1})
        return both <= serial

    def order_key(j):
        if policy.get("sjf"):
            return (j.typ, j.arr)
        return (j.arr,)

    dirty = True
    while queue or running:
        # admissions only at events (an arrival or an end): nothing else changes the inputs
        kind = policy["kind"]
        changed = dirty
        dirty = False
        while changed:
            changed = False
            q = sorted(queue, key=order_key)
            for idx, j in enumerate(q):
                ok = False
                if kind == "F":
                    ok = True
                elif kind == "S":
                    ok = len(running) < policy["slots"]
                else:  # K family
                    if not running:
                        ok = idx == 0
                    elif j.mem <= room() and cpu_ok(j) and gain_ok(j):
                        if idx == 0:
                            ok = True
                        else:
                            Wh = W_for(q[0])
                            ok = Wh is not None and j.typ <= Wh
                if ok:
                    queue.remove(j)
                    j.start = t
                    if nice_w is not None and running:
                        j.weight, j.senior = nice_w, False
                    running.append(j)
                    changed = True
                    break
        if not running:
            break
        r = alloc(running, eta, beta, m_ok, senior_eta=senior_eta)
        t += dt
        for j in list(running):
            j.left -= r[j] * dt
            if j.left <= 0:
                j.end = t
                dirty = True
                running.remove(j)
                done.append(j)
                # senior status passes on: the oldest running keeps weight 1
                pos[j.agent] += 1
                ch = chains[j.agent]
                if pos[j.agent] < len(ch):
                    lab = ch[pos[j.agent]]
                    queue.append(Job(j.agent, lab, L[lab], j.arr if policy.get("inherit") else t))
        if t > tmax:
            raise RuntimeError("tmax")
    verd = {}
    for j in done:
        verd[j.agent] = max(verd.get(j.agent, 0), j.end)
    return verd, done


def summary(verd):
    return sum(verd.values()) / len(verd), max(verd.values())


def trace(run_id, starts, eta, beta=0.0, m_ok=8.5, dt=0.005, speed=1.0):
    """Replay: each (agent, label, t_start) starts at its observed start; returns end per job."""
    L = {k: (v[0] * speed, v[1], v[2], v[3]) for k, v in labels(run_id).items()}
    pend = sorted([Job(a, l, L[l], ts) for a, l, ts in starts], key=lambda j: j.arr)
    running, done, t = [], [], 0.0
    while pend or running:
        while pend and pend[0].arr <= t + 1e-9:
            j = pend.pop(0); j.start = t; running.append(j)
        if running:
            r = alloc(running, eta, beta, m_ok)
            for j in list(running):
                j.left -= r[j] * dt
                if j.left <= 0:
                    j.end = t + dt; running.remove(j); done.append(j)
        t += dt
    return done
