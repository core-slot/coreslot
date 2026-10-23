"""The figures of acceptance run 6, plain SVG from the standard library, drawn by `run6.py figures`
from summary.json and the main records. Every arm keeps the colour ARM_COLOUR gives it."""
import math
import os
import statistics

import run4
import stats6

INK, MUTED, GRID, SURFACE = run4.INK, run4.MUTED, run4.GRID, run4.SURFACE
ARM_COLOUR = {"K60": "#2a78d6", "F": "#eb6834", "S1": "#1baf7a", "S2": "#eda100",
              "S3": "#e87ba4", "K67": "#008300", "KH": "#6250d6"}
W, L, R = 600, 64, 24  # the width and the plot's left and right margins
MARGIN = math.log(1.03)


def arms_of(blocks):
    """The arms the blocks hold, in ARM_COLOUR's order."""
    held = next(iter(blocks.values()))
    return [a for a in ARM_COLOUR if a in held]


def level_of(sets):
    return next(iter(sets.values()))["level_mib"]


def sstar(sets, s_star):
    return s_star[level_of(sets)]


def ticks(lo, hi, n=5):
    """Round values from at most lo to at least hi, about n steps apart."""
    if hi <= lo:
        lo, hi = lo - 0.5, hi + 0.5
    raw = (hi - lo) / n
    step = 10 ** math.floor(math.log10(raw))
    step *= next(m for m in (1, 2, 5, 10) if step * m >= raw)
    first = math.floor(lo / step) * step
    return [round(first + i * step, 10) for i in range(math.ceil((hi - first) / step - 1e-9) + 1)]


def iqr(xs):
    return statistics.quantiles(xs, n=4)[0::2] if len(xs) > 1 else [xs[0], xs[0]]


def gmr_bca(d):
    """The geometric mean ratio of log differences and its BCa 90 % interval."""
    lo, hi = stats6.bca(d)
    return math.exp(statistics.fmean(d)), [math.exp(lo), math.exp(hi)]


def swatch(shape, x, y, colour):
    if shape == "dot":
        return f'<circle cx="{x + 7}" cy="{y}" r="5" fill="{colour}"/>'
    if shape == "band":
        return f'<rect x="{x}" y="{y - 5}" width="14" height="10" fill="{colour}"/>'
    if shape == "bar":
        return f'<line x1="{x + 7}" x2="{x + 7}" y1="{y - 6}" y2="{y + 6}" stroke="{colour}" ' \
               'stroke-width="3"/>'
    dash = ' stroke-dasharray="4 3"' if shape == "dash" else ""
    return f'<line x1="{x}" x2="{x + 14}" y1="{y}" y2="{y}" stroke="{colour}" stroke-width="2"' \
           f'{dash}/>'


def head(title, items):
    """The title and a legend of (label, colour, shape) items; returns the body and the first y
    below them."""
    body, x, y = [f'<text x="{L}" y="18" fill="{INK}">{title}</text>'], L, 40
    for label, colour, shape in items:
        span = 20 + 7 * len(label)
        if x + span > W - R:
            x, y = L, y + 18
        body.append(swatch(shape, x, y, colour))
        body.append(f'<text x="{x + 20}" y="{y + 4}" fill="{INK}">{label}</text>')
        x += span + 14
    return body, y + 22


def frame(body, box, xr, yt, xlabel, ylabel, xt=()):
    """Grid lines and tick labels on y, tick labels xt on x and both axis titles for the box
    (left, top, right, bottom) spanning xr and yt; returns the x and y scales."""
    l, t, r, b = box
    x = (lambda v: l + (v - xr[0]) / ((xr[1] - xr[0]) or 1) * (r - l))
    y = (lambda v: b - (v - yt[0]) / (yt[-1] - yt[0]) * (b - t))
    for v in yt:
        body.append(f'<line x1="{l}" x2="{r}" y1="{y(v):.1f}" y2="{y(v):.1f}" stroke="{GRID}"/>'
                    f'<text x="{l - 6}" y="{y(v) + 4:.1f}" text-anchor="end" '
                    f'fill="{MUTED}">{v:g}</text>')
    for v in xt:
        body.append(f'<text x="{x(v):.1f}" y="{b + 16}" text-anchor="middle" '
                    f'fill="{MUTED}">{v:g}</text>')
    if xlabel:
        body.append(f'<text x="{(l + r) / 2:.0f}" y="{b + 34}" text-anchor="middle" '
                    f'fill="{MUTED}">{xlabel}</text>')
    body.append(f'<text transform="translate(14 {(t + b) / 2:.0f}) rotate(-90)" '
                f'text-anchor="middle" fill="{MUTED}">{ylabel}</text>')
    return x, y


def block_axis(body, blocks, x, b):
    """Every k-th block number under the plot, at most about 15 of them."""
    k = math.ceil(len(blocks) / 15)
    for i, bl in enumerate(blocks):
        if i % k == 0:
            body.append(f'<text x="{x(i):.1f}" y="{b + 16}" text-anchor="middle" '
                        f'fill="{MUTED}">{bl}</text>')


def f1_timeline(heavy, s_star, kcf):
    """run4's swimlane of the block whose ln K*cf/S* is nearest the median."""
    d = sorted((math.log(heavy[b][kcf[b]]["mean_verdict_s"]
                         / heavy[b][sstar(heavy[b], s_star)]["mean_verdict_s"]), b) for b in heavy)
    mid = d[(len(d) - 1) // 2][1]
    sets = heavy[mid]
    agents = sorted({g["agent"] for s in sets.values() for g in s["gates"]})
    return mid, run4.timeline_svg(mid, sets, next(iter(sets.values()))["order"].split(), agents)


def f3_paired(heavy, s_star, kcf):
    blocks = list(heavy)
    series = [("K*cf / S*", [math.log(heavy[b][kcf[b]]["mean_verdict_s"]
                                      / heavy[b][sstar(heavy[b], s_star)]["mean_verdict_s"])
                             for b in blocks]),
              ("K60 / F", [math.log(heavy[b]["K60"]["mean_verdict_s"]
                                    / heavy[b]["F"]["mean_verdict_s"]) for b in blocks])]
    body, top = head("Per heavy block: ln of the ratio of mean time to verdict",
                     [("one block", INK, "dot"), ("±ln 1.03 margin", GRID, "band"),
                      ("BCa 90 % interval of the mean", INK, "bar")])
    ph, gap = 150, 50
    for k, (name, d) in enumerate(series):
        t = top + k * (ph + gap)
        lo, hi = stats6.bca(d)
        yt = ticks(min(d + [lo, -MARGIN]), max(d + [hi, MARGIN]))
        box = (L, t, W - R - 40, t + ph)
        x, y = frame(body, box, (-0.5, len(d) - 0.5), yt, "block" if k else "", "ln ratio")
        body.append(f'<rect x="{L}" y="{y(MARGIN):.1f}" width="{box[2] - L}" '
                    f'height="{y(-MARGIN) - y(MARGIN):.1f}" fill="{GRID}" opacity="0.8"/>'
                    f'<line x1="{L}" x2="{box[2]}" y1="{y(0):.1f}" y2="{y(0):.1f}" '
                    f'stroke="{INK}" stroke-dasharray="4 3"/>'
                    f'<text x="{L + 4}" y="{t + 12}" fill="{INK}">ln({name})</text>')
        for i, v in enumerate(d):
            body.append(f'<circle cx="{x(i):.1f}" cy="{y(v):.1f}" r="4" fill="{INK}" '
                        f'stroke="{SURFACE}" stroke-width="2"><title>block {blocks[i]}: ln({name})'
                        f' {v:.4f}</title></circle>')
        bx = box[2] + 20
        body.append(f'<line x1="{bx}" x2="{bx}" y1="{y(hi):.1f}" y2="{y(lo):.1f}" stroke="{INK}" '
                    f'stroke-width="3"><title>BCa 90 %: {lo:.4f} to {hi:.4f}</title></line>'
                    f'<circle cx="{bx}" cy="{y(statistics.fmean(d)):.1f}" r="4" fill="{INK}"/>')
        block_axis(body, blocks, x, box[3])
    return run4.svg(W, top + 2 * ph + gap + 44, body)


def f4_pareto(heavy, level):
    bs = [b for b in heavy if level_of(heavy[b]) == level]
    pts = {}
    for a in arms_of(heavy):
        xs =[heavy[b][a]["mean_verdict_s"] for b in bs]
        ys = [run4.warn_s(heavy[b][a]) for b in bs]
        pts[a] = (statistics.fmean(xs), iqr(xs), statistics.fmean(ys), iqr(ys))
    body, t = head(f"Ballast {level} MiB: time to verdict against memory pressure, mean and IQR",
                   [(a, ARM_COLOUR[a], "dot") for a in pts])
    xt = ticks(min(p[1][0] for p in pts.values()), max(p[1][1] for p in pts.values()))
    yt = ticks(min(p[3][0] for p in pts.values()), max(p[3][1] for p in pts.values()))
    box = (L, t, W - R, t + 300)
    x, y = frame(body, box, (xt[0], xt[-1]), yt, "mean time to verdict per set (s)",
                 "warn-or-worse pressure per set (s)", xt)
    placed = []
    for a, (mx, (x1, x3), my, (y1, y3)) in pts.items():
        c, lx, ly = ARM_COLOUR[a], x(mx) + 7, y(my) - 7
        while any(abs(lx - px) < 34 and abs(ly - py) < 13 for px, py in placed):
            ly -= 13  # lift a label off one already drawn
        placed.append((lx, ly))
        body.append(f'<line x1="{x(x1):.1f}" x2="{x(x3):.1f}" y1="{y(my):.1f}" y2="{y(my):.1f}" '
                    f'stroke="{c}" stroke-width="2"/><line x1="{x(mx):.1f}" x2="{x(mx):.1f}" '
                    f'y1="{y(y1):.1f}" y2="{y(y3):.1f}" stroke="{c}" stroke-width="2"/>'
                    f'<circle cx="{x(mx):.1f}" cy="{y(my):.1f}" r="5" fill="{c}" stroke="{SURFACE}"'
                    f' stroke-width="2"><title>{a}: {mx:.1f} s to verdict, {my:.1f} s warn+'
                    f'</title></circle><text x="{lx:.1f}" y="{ly:.1f}" fill="{INK}">{a}</text>')
    return run4.svg(W, box[3] + 44, body)


def f5_regret(heavy, s_star, regret):
    arms = ["S1", "S2", "S3", "F", "K60"]
    levels = sorted({level_of(s) for s in heavy.values()})
    vals = {}
    for lv in levels:
        for a in arms[:-1]:
            e = regret[lv][a]
            vals[lv, a] = (e["gmr"], e["bca90"]) if e else None
        bs = [b for b in heavy if level_of(heavy[b]) == lv]
        d = [math.log(heavy[b]["K60"]["mean_verdict_s"]
                      / heavy[b][sstar(heavy[b], s_star)]["mean_verdict_s"]) for b in bs]
        vals[lv, "K60"] = gmr_bca(d) if d else None
    body, t = head("Regret: each arm's mean time to verdict over S*'s per ballast level",
                   [(a, ARM_COLOUR[a], "dot") for a in arms] + [("BCa 90 %", MUTED, "line")])
    span = [v for g, iv in filter(None, vals.values()) for v in [g] + iv if v is not None]
    yt = ticks(min(span + [1.0]), max(span + [1.0]))
    box = (L, t, W - R, t + 280)
    x, y = frame(body, box, (-0.5, len(levels) - 0.5), yt, "ballast (MiB)",
                 "ratio to S* (geometric mean)")
    for i, lv in enumerate(levels):
        body.append(f'<text x="{x(i):.1f}" y="{box[3] + 16}" text-anchor="middle" '
                    f'fill="{MUTED}">{lv} (S* = {s_star[lv]})</text>')
        for j, a in enumerate(arms):
            v = vals[lv, a]
            if not v or v[0] is None:
                continue
            g, (lo, hi) = v
            cx, c = x(i - 0.32 + 0.16 * j), ARM_COLOUR[a]
            if lo is not None and hi is not None:
                body.append(f'<line x1="{cx:.1f}" x2="{cx:.1f}" y1="{y(hi):.1f}" y2="{y(lo):.1f}" '
                            f'stroke="{c}" stroke-width="2"/>')
            body.append(f'<circle cx="{cx:.1f}" cy="{y(g):.1f}" r="5" fill="{c}" '
                        f'stroke="{SURFACE}" stroke-width="2"><title>{lv} MiB, {a}: {g:.4f}'
                        '</title></circle>')
    body.append(f'<line x1="{L}" x2="{W - R}" y1="{y(1):.1f}" y2="{y(1):.1f}" stroke="{INK}" '
                'stroke-dasharray="4 3"/>')
    return run4.svg(W, box[3] + 44, body)


def f6_ecdf(heavy):
    arms = arms_of(heavy)
    vals = {a: sorted(v for s in heavy.values() for v in s[a]["verdict_s"].values())
            for a in arms}
    body, t = head("Time to verdict per agent, pooled over the heavy blocks",
                   [(a, ARM_COLOUR[a], "line") for a in arms])
    xt = ticks(0, max(max(v) for v in vals.values()))
    box = (L, t, W - R, t + 300)
    x, y = frame(body, box, (xt[0], xt[-1]), ticks(0, 1), "time to verdict per agent (s)",
                 "fraction of verdicts", xt)
    for a, v in vals.items():
        pts, prev = [f"{x(xt[0]):.1f},{y(0):.1f}"], 0.0
        for i, s in enumerate(v):
            pts += [f"{x(s):.1f},{y(prev):.1f}", f"{x(s):.1f},{y((i + 1) / len(v)):.1f}"]
            prev = (i + 1) / len(v)
        pts.append(f"{x(xt[-1]):.1f},{y(1):.1f}")
        body.append(f'<polyline points="{" ".join(pts)}" fill="none" stroke="{ARM_COLOUR[a]}" '
                    f'stroke-width="2"><title>{a}: median {statistics.median(v):g} s</title>'
                    '</polyline>')
    return run4.svg(W, box[3] + 44, body)


def f7_light(light):
    blocks = list(light)
    d = [light[b]["K60"]["mean_verdict_s"] / light[b]["F"]["mean_verdict_s"] for b in blocks]
    body, t = head("Light mix: K60 / F mean time to verdict per block",
                   [("one block", ARM_COLOUR["K60"], "dot"), ("K60 1, 3, 5 % slower", MUTED,
                                                              "dash")])
    yt = ticks(min(d + [1.0]), max(d + [1.05]))
    box = (L, t, W - R - 40, t + 260)
    x, y = frame(body, box, (-0.5, len(d) - 0.5), yt, "light block", "K60 / F (ratio)")
    body.append(f'<line x1="{L}" x2="{box[2]}" y1="{y(1):.1f}" y2="{y(1):.1f}" stroke="{INK}"/>')
    for p in (1, 3, 5):
        v = 1 + p / 100
        body.append(f'<line x1="{L}" x2="{box[2]}" y1="{y(v):.1f}" y2="{y(v):.1f}" '
                    f'stroke="{MUTED}" stroke-dasharray="4 3"/><text x="{box[2] + 6}" '
                    f'y="{y(v) + 4:.1f}" fill="{MUTED}">+{p} %</text>')
    for i, v in enumerate(d):
        body.append(f'<circle cx="{x(i):.1f}" cy="{y(v):.1f}" r="4" fill="{ARM_COLOUR["K60"]}" '
                    f'stroke="{SURFACE}" stroke-width="2"><title>block {blocks[i]}: K60 / F'
                    f' {v:.4f}</title></circle>')
    block_axis(body, blocks, x, box[3])
    return run4.svg(W, box[3] + 44, body)


def write(out, summary, heavy, light):
    """Writes the figures into out; heavy and light are block -> arm -> set for the blocks the
    summary used."""
    s_star = {int(lv): a for lv, a in summary["oracles"]["S"].items()}
    kcf = {int(b): a for b, a in summary["oracles"]["Kcf"].items()}
    regret = {int(lv): r for lv, r in summary["reported"]["regret"].items()}
    mid, timeline = f1_timeline(heavy, s_star, kcf)
    figs = {"f1-timeline.svg": timeline, "f3-paired.svg": f3_paired(heavy, s_star, kcf),
            "f5-regret.svg": f5_regret(heavy, s_star, regret), "f6-ecdf.svg": f6_ecdf(heavy),
            "f7-light.svg": f7_light(light)}
    for lv in sorted({level_of(s) for s in heavy.values()}):
        figs[f"f4-pareto-{lv}.svg"] = f4_pareto(heavy, lv)
    for name, text in figs.items():
        with open(os.path.join(out, name), "w", encoding="utf-8") as f:
            f.write(text)
    return sorted(figs), mid
