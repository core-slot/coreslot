#!/usr/bin/env python3
"""Draw the README's benchmark chart from acceptance run 6's committed summary.

    python3 docs/img/overload_chart.py   # writes docs/img/overload-run6.svg

Every number on the chart is read from `evidence/overload/run6/summary.json`; nothing is typed in.
Standard library only.
"""

from __future__ import annotations

import json
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SUMMARY = ROOT / "evidence" / "overload" / "run6" / "summary.json"
OUT = Path(__file__).resolve().parent / "overload-run6.svg"

# The arms of the heavy mix, top to bottom, with the names the chart gives them.
ARMS = [
    ("F", "Everything at once"),
    ("S3", "Semaphore, 3 slots"),
    ("S2", "Semaphore, 2 slots"),
    ("S1", "Semaphore, 1 slot"),
    ("K60", "coreslot, learned needs"),
]

W, LEFT, BAR_X, BAR_W, NOTE_X = 960, 24, 220, 320, 620
TOP, ROW, BAR_H = 96, 44, 22


def main() -> None:
    per_arm = json.loads(SUMMARY.read_text(encoding="utf-8"))["reported"]["per_arm"]["heavy"]
    longest = max(per_arm[arm]["mean_verdict_s"] for arm, _ in ARMS)
    rows = []
    for i, (arm, name) in enumerate(ARMS):
        data = per_arm[arm]
        seconds = data["mean_verdict_s"]
        swap_gib = data["swapout_bytes"]["mean"] / 2**30
        failures = data["false_failures"]
        y = TOP + i * ROW
        width = BAR_W * seconds / longest
        cls = "accent" if arm.startswith("K") else "bar"
        swap = f"{swap_gib:.1f} GiB swapped" if swap_gib >= 0.05 else "no swap"
        rows.append(
            f'<text class="name" x="{LEFT}" y="{y + 16}">{name}</text>'
            f'<rect class="{cls}" x="{BAR_X}" y="{y}" width="{width:.1f}" height="{BAR_H}" rx="4"/>'
            f'<text class="value" x="{BAR_X + width + 8:.1f}" y="{y + 16}">{seconds:.1f} s</text>'
            f'<text class="note" x="{NOTE_X}" y="{y + 16}">{swap}, '
            f'{failures["k"]} false failures in {failures["n"]} runs</text>'
        )
    height = TOP + len(ARMS) * ROW + 40
    svg = f"""<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {height}" width="{W}" height="{height}" role="img" aria-labelledby="t d">
<title id="t">Mean time to verdict of four agents' gates, acceptance run 6</title>
<desc id="d">{'; '.join(f"{name}: {per_arm[arm]['mean_verdict_s']:.1f} s" for arm, name in ARMS)}</desc>
<style>
  .bg {{ fill: #fcfcfb; }} .bar {{ fill: #b9b8b2; }} .accent {{ fill: #2a78d6; }}
  text {{ font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif; }}
  .title {{ font-size: 18px; font-weight: 600; fill: #0b0b0b; }}
  .sub, .note, .foot {{ font-size: 13px; fill: #52514e; }}
  .name, .value {{ font-size: 14px; fill: #0b0b0b; }} .value {{ font-weight: 600; }}
  @media (prefers-color-scheme: dark) {{
    .bg {{ fill: #1a1a19; }} .bar {{ fill: #5c5b56; }} .accent {{ fill: #3987e5; }}
    .title, .name, .value {{ fill: #f0efec; }} .sub, .note, .foot {{ fill: #c3c2b7; }}
  }}
</style>
<rect class="bg" width="{W}" height="{height}" rx="8"/>
<text class="title" x="{LEFT}" y="36">Four agents' type checks, lints and test suites on one 16 GB, 10-core laptop</text>
<text class="sub" x="{LEFT}" y="60">Mean time from an agent's start to its last verdict, 120 counterbalanced blocks; shorter is better</text>
{''.join(rows)}
<text class="foot" x="{LEFT}" y="{height - 16}">coreslot acceptance run 6, 2026-10-23: records, analysis and threats to validity in evidence/README.md</text>
</svg>
"""
    OUT.write_text(svg, encoding="utf-8")


if __name__ == "__main__":
    main()
