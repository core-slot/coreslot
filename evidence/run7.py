#!/usr/bin/env python3
"""Acceptance run 7: coreslot against fixed slot counts (a machine-wide flock and `sem -j2`) and
GNU parallel's --memsuspend, at a quiet and a loaded level of memory headroom.

usage:
  run7.py learn STAND OUT
  run7.py verdict STAND OUT PHASE MIX FIRST LAST [--as-block B] [--primer]
  run7.py pilot-check OUT
  run7.py summary OUT

STAND is overload/stand7.json: run 4's stand with FastAPI's tests under `--dist loadgroup`, and
"wide_agents", the agents of the wide mix. Every coreslot call goes through run4.cs() with the
stand's state directory; GNU parallel runs with PARALLEL_HOME=<state_dir>/../parallel-home.

The arms: K60 runs every gate under coreslot with cpu = "140%", mem = "60%" and OUT/needs.toml;
S1 and S2 hold each heavy gate on one of 1 or 2 slots of a counting semaphore, S1 a machine-wide
flock; P runs each heavy gate through GNU sem with --memsuspend 9G and -j 4; F runs every gate
when its agent reaches it. The mixes: heavy (K60, S1, S2, P on the stand's agents, at the quiet
and the loaded level), light (K60, F on A3 and A4), wide (K60, S2, F on the wide agents) and
spread (K60, S2, arrivals U(0, 30 s)), the last three at the quiet level only.

  learn    every gate of the agents and the wide agents alone, each run after a cooldown of at
           least 20 s: 6 runs in its K form (coreslot, cpu = "140%", mem = "60%"), then 3 in its
           F form (its argv with "free_n"); then `coreslot suggest --write`, copied to
           OUT/needs-pass1.toml. Then the relearn pass: 5 K-form runs per gate under that entry,
           so the window of the last 5 runs per label holds only runs at the granted widths, and
           suggest --write again, copied to OUT/needs.toml. Each run is recorded to
           OUT/learn.jsonl as run 6's learn records it, with "pass": "learn" or "relearn";
           OUT/needs.sha256 holds the SHA-256 of needs.toml and learn.jsonl.
           The first call starts a fresh state directory and records it in OUT/learn-state.json;
           a later call reuses it, never resetting it, and makes only the runs learn.jsonl lacks
           by (gate, form, pass, run). Once a relearn record exists, pass 1 and its suggest are
           never run again. Refuses with exit 2 once any pilot, main or sensitivity record exists.
  verdict  blocks FIRST..LAST of PHASE (pilot or main for the heavy, light and wide mixes; sens
           for spread) and MIX, each block's arms in a seeded order, each set after a cooldown,
           at its level, under a timeout of 600 s; appends one record per set to
           OUT/<phase>-<mix>.jsonl, skipping each (block, arm) already recorded.
  pilot-check
           from the pilot's first valid blocks: the futility check of P1's three components and
           whether P is feasible, to OUT/pilot_check.json; main runs only when its "go" is true.
  summary  from OUT/main-*.jsonl, OUT/sens-*.jsonl and OUT/pilot_check.json: P1 by
           intersection-union, then the secondary family by Holm at 0.05, and the reported
           estimates; to OUT/summary.json and OUT/summary.txt.
"""
import argparse
import glob
import json
import math
import os
import shutil
import signal
import sys
from datetime import datetime, timezone

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import acceptance  # noqa: E402  (probe, cooldown, runlog_lines)
import overload  # noqa: E402  (load)
import run4  # noqa: E402  (cs, fresh_state, write_config, checked_state, pacing, ...)
import run6  # noqa: E402  (measured, write_learn_state)

HEAVY_ARMS = ("K60", "S1", "S2", "P")
P1_ARMS = ("K60", "S1", "S2")
LIGHT_ARMS = ("K60", "F")
WIDE_ARMS = ("K60", "S2", "F")
SPREAD_ARMS = ("K60", "S2")
LEVELS = ("quiet", "loaded")
QUIET_HEADROOM_MIB = 10500  # (probe)
LOADED_HEADROOM_MIB = 8000  # (probe)
LEVEL_TOLERANCE_MIB = 512
BALLAST_CAP_MIB = 6144
QUIET_WAIT_S = 120
RESIDENCY_SHARE = 0.95
RESIDENCY_WAIT_S = 60
SET_TIMEOUT_S = 600
P_MEMSUSPEND = "9G"
P_JOBS = 4
BG_CPU_FLAG_CORES = 1.5
K_SHARE = {"cpu": "140%", "mem": "60%"}
LEARN_RUNS = {"K": 6, "F": 3}
RELEARN_RUNS = 5
LEARN_COOLDOWN_S = 20
PILOT_BLOCKS = {"heavy": 12, "light": 12, "wide": 6}
N_LEVEL = 72  # heavy blocks per level
N_LIGHT = 40
N_WIDE = 30
SPREAD_BLOCKS = 12
SPREAD_WINDOW_S = 30
ALPHA = 0.05
MARGIN = math.log(1.03)
SWAP_EVENT_MIB = 64
SWAP_EVENT_P0 = 0.03
FUTILITY_CP = 0.5
P_INFEASIBLE_TIMEOUTS = 2  # P is infeasible when more than 2 of the 12 pilot heavy P sets time out


# --- learn ------------------------------------------------------------------------------------


# --- sets -------------------------------------------------------------------------------------
