#!/usr/bin/env python3
"""pool-monitor.py — sample pool admission latency + pool quality on one node.

Runs ON the forwarding node (needs the loopback API). Appends one JSON object
per invocation to a JSONL file; never rewrites history, so a systemd timer can
call it every 2h and the file becomes the time series.

WHY THIS EXISTS
  We are about to change how a newly-appeared subscription node earns its way
  into the routing pool (today: a 24h trial window). To tell whether that
  change helped we need the before/after of two numbers that no existing
  endpoint reports directly:

    1. admission latency — wall time from "this node tag was first observed by
       the scorer" to "it first appeared in the routing pool".
    2. in-pool quality distribution — the composite score spread of pool
       members, which is what a user actually feels (HRW pins a terminal to
       one member for hours, so the WORST member matters more than the mean).

  Everything else here is context needed to interpret those two.

READ-ONLY: this script only GETs /api/* and writes its own state + output
files. It never touches gateway.yaml, mihomo config, or any service.

Usage:
  pool-monitor.py [--api URL] [--out FILE] [--state FILE] [--print]

Exit codes: 0 ok, 1 API unreachable (nothing appended).
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone

DEFAULT_API = "http://127.0.0.1:18080"
DEFAULT_OUT = "/var/lib/leap/pool-monitor.jsonl"
DEFAULT_STATE = "/var/lib/leap/pool-monitor-state.json"

# Mirrors internal/nodescorer/helpers.go compositeScore. Kept in sync by hand;
# the passive term is omitted because /api/nodes/health exposes passive stats
# in a different shape and the term only adds <=500 to a score whose useful
# range is ~200-5000. Documented divergence, not an oversight.
def composite(node):
    if not node.get("probe_count"):
        return None
    p95 = node.get("rtt_p95_ms") or 0
    jitter = node.get("jitter_ms") or 0
    fail = node.get("fail_rate") or 0.0
    return float(p95) + 2.0 * float(jitter) + 5000.0 * fail * fail


def get_json(api, path, timeout=8):
    req = urllib.request.Request(api.rstrip("/") + path)
    tok = os.environ.get("LEAP_TOKEN", "")
    if tok:
        req.add_header("Authorization", "Bearer " + tok)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def load_state(path):
    try:
        with open(path) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {"first_seen": {}, "first_in_pool": {}}


def save_state(path, state):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f, ensure_ascii=False)
    os.replace(tmp, path)


def percentile(values, pct):
    if not values:
        return None
    s = sorted(values)
    if len(s) == 1:
        return s[0]
    k = (len(s) - 1) * pct / 100.0
    lo = int(k)
    hi = min(lo + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--api", default=DEFAULT_API)
    ap.add_argument("--out", default=DEFAULT_OUT)
    ap.add_argument("--state", default=DEFAULT_STATE)
    ap.add_argument("--print", action="store_true", dest="do_print")
    args = ap.parse_args()

    now = datetime.now(timezone.utc)
    now_iso = now.isoformat().replace("+00:00", "Z")
    now_ts = time.time()

    try:
        status = get_json(args.api, "/api/status")
        health = get_json(args.api, "/api/nodes/health")
    except (urllib.error.URLError, urllib.error.HTTPError, OSError) as e:
        print("pool-monitor: API unreachable: %s" % e, file=sys.stderr)
        return 1

    nodes = health if isinstance(health, list) else health.get("nodes", [])
    state = load_state(args.state)
    first_seen = state.setdefault("first_seen", {})
    first_in_pool = state.setdefault("first_in_pool", {})

    # Track first observation and first admission per node tag. Tags are stable
    # (sub/name); a provider RENAMING a node looks like a new node here, which
    # is the same assumption the scorer's own EWMA FirstSeenAt makes.
    newly_admitted = []
    for n in nodes:
        tag = n["name"]
        if tag not in first_seen:
            first_seen[tag] = now_ts
        if n.get("in_pool") and tag not in first_in_pool:
            first_in_pool[tag] = now_ts
            newly_admitted.append(
                {
                    "node": tag,
                    "admission_latency_s": round(now_ts - first_seen[tag], 1),
                    "composite_at_admission": composite(n),
                    "probe_count": n.get("probe_count"),
                    "fail_rate": n.get("fail_rate"),
                }
            )

    # Nodes observed but still not admitted — the population the trial window
    # (or its replacement) is holding back. waiting_s is a LOWER bound for any
    # node already present when monitoring started.
    waiting = []
    for n in nodes:
        tag = n["name"]
        if tag in first_in_pool or n.get("in_pool"):
            continue
        waiting.append(
            {
                "node": tag,
                "waiting_s": round(now_ts - first_seen[tag], 1),
                "composite": composite(n),
                "qualified": n.get("qualified"),
                "alive": n.get("alive"),
                "probe_count": n.get("probe_count"),
                "fail_rate": n.get("fail_rate"),
            }
        )
    waiting.sort(key=lambda w: (w["composite"] is None, w["composite"] or 0))

    in_pool = [n for n in nodes if n.get("in_pool")]
    pool_comps = [c for c in (composite(n) for n in in_pool) if c is not None]
    pool_comps.sort()

    # The ceiling the proposed admission gate WOULD compute right now
    # (poolFillCeiling: median composite of measured pool members x 2.0,
    # or 0 = "no ceiling" when fewer than 3 measured members). Recorded even
    # before the gate ships so we can see what it would have done.
    ceiling = None
    if len(pool_comps) >= 3:
        mid = len(pool_comps) // 2
        med = (
            pool_comps[mid]
            if len(pool_comps) % 2 == 1
            else (pool_comps[mid - 1] + pool_comps[mid]) / 2.0
        )
        ceiling = med * 2.0

    would_admit = would_refuse = 0
    if ceiling:
        for w in waiting:
            c = w["composite"]
            if c is None:
                continue
            if c <= ceiling:
                would_admit += 1
            else:
                would_refuse += 1

    sizing = status.get("pool_sizing") or {}
    stability = status.get("pool_stability_24h") or {}

    sample = {
        "at": now_iso,
        "node_count": status.get("node_count"),
        "pool_size": status.get("pool_size"),
        "pool_total": status.get("pool_total"),
        "sizing_mode": status.get("sizing_mode"),
        "min_pool_size": sizing.get("min_pool_size"),
        "k_target": sizing.get("k_target"),
        "swap_count_24h": stability.get("swap_count"),
        "engine_ok": status.get("engine_ok"),
        "last_refresh": status.get("last_refresh"),
        "pool_quality": {
            "n_measured": len(pool_comps),
            "best": pool_comps[0] if pool_comps else None,
            "median": percentile(pool_comps, 50),
            # The number a hash-pinned user actually feels: HRW keeps one
            # terminal on one member, so the pool's worst member is somebody's
            # entire experience, not an outlier to be averaged away.
            "worst": pool_comps[-1] if pool_comps else None,
            "p95": percentile(pool_comps, 95),
            "spread": (pool_comps[-1] - pool_comps[0]) if pool_comps else None,
        },
        "pool_health": {
            "not_alive_in_pool": sum(1 for n in in_pool if not n.get("alive")),
            "max_fail_rate_in_pool": max(
                (n.get("fail_rate") or 0.0 for n in in_pool), default=None
            ),
        },
        "admission": {
            "newly_admitted": newly_admitted,
            "waiting_count": len(waiting),
            "waiting_top5": waiting[:5],
        },
        "proposed_gate": {
            "ceiling": ceiling,
            "would_admit": would_admit,
            "would_refuse": would_refuse,
        },
    }

    save_state(args.state, state)
    with open(args.out, "a") as f:
        f.write(json.dumps(sample, ensure_ascii=False) + "\n")

    if args.do_print:
        print(json.dumps(sample, ensure_ascii=False, indent=2))
    else:
        q = sample["pool_quality"]
        print(
            "pool=%s measured=%s best=%s median=%s worst=%s ceiling=%s "
            "waiting=%s admitted_now=%s"
            % (
                sample["pool_size"],
                q["n_measured"],
                None if q["best"] is None else int(q["best"]),
                None if q["median"] is None else int(q["median"]),
                None if q["worst"] is None else int(q["worst"]),
                None if ceiling is None else int(ceiling),
                len(waiting),
                len(newly_admitted),
            )
        )
    return 0


if __name__ == "__main__":
    sys.exit(main())
