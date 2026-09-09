#!/usr/bin/env bash
# watch-node.sh — stream a leap-gateway node's journal as filtered events.
#
#   scripts/watch-node.sh [host] [--since <systemd-time>]
#
# Default host 192.168.70.92 (production). With --since it replays history
# and exits; without it, follows live and never exits (Ctrl-C to stop).
#
# Filter design rule: SILENCE MUST MEAN "nothing changed and nothing broke",
# never "the filter forgot to look". Every failure signature is always
# emitted. Only the known steady-state oscillation is aggregated, because
# per-occurrence reporting of a flap that fires every scoring round drowns
# the events that matter.
#
# Supply alerting is a FLOOR ONLY: qualified < MIN_POOL fires, growth never
# does. Two earlier shapes were both wrong and both cost real inspection
# rounds:
#
#   - A self-widening band (seed 6-8, then 2-3 before that) reported the
#     pool GROWING as OUTSIDE-band. Pool growth is good news and must never
#     interrupt. Worse, the band lived in awk's BEGIN, which re-runs on every
#     reconnect (the awk is inside the while loop below) — so every reconnect
#     reset the band to its seed and re-fired alerts on values already judged
#     normal before the drop. Observed 2026-09-09 12:03Z: qualified=9 was
#     accepted at 10:35Z, then re-fired as OUTSIDE after the 12:03Z redial.
#
#   - A cumulative flap counter printing every 12th in-band swap. It was not
#     a rate, so "12" carried no time base (12 swaps over 3.5h ≈ 0.29/round,
#     i.e. the pool was CALM) yet read as an alarm. It also counted changes
#     in the qualified COUNT, so an equal one-in-one-out swap — the exact
#     thing it existed to catch — was invisible to it. Deleted rather than
#     fixed: the 30-minute inspection reads `pool updated + hot-reloaded`
#     straight from the journal, which is the authoritative churn measure.
#
# MIN_POOL mirrors node_qualify.pool_sizing.min_pool_size on the node. Below
# it the redundancy floor is breached, which is a real incident.
set -uo pipefail

HOST="192.168.70.92"
SINCE=""
# Mirrors node_qualify.pool_sizing.min_pool_size on the node. Kept as a plain
# constant rather than read from the API: the watch must alert on the floor
# even when the API is the thing that is down.
MIN_POOL="${MIN_POOL:-4}"
while [ $# -gt 0 ]; do
  case "$1" in
    --since) SINCE="${2:-}"; shift 2 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) HOST="$1"; shift ;;
  esac
done

AWK_PROG='
BEGIN { seen = 0 }
{
  line = $0
  if (line ~ /level=(WARN|ERROR|FATAL)/ ||
      line ~ /subscription unhealthy/ ||
      line ~ /obfs has unexpected shape/ ||
      line ~ /obfs block has no type/ ||
      line ~ /panic|fatal error|signal SIGSEGV/ ||
      line ~ /systemd.*(Stopping|Stopped|Started|Failed|Deactivated)/ ||
      line ~ /msg="render:|msg="reload:|render failed|reload failed/) {
    print "[!] " line; fflush(); next
  }
  if (line ~ /nodescorer: scored/) {
    q = ""; tot = ""; sl = ""
    if (match(line, /qualified=[0-9]+/))      q   = substr(line, RSTART+10, RLENGTH-10) + 0
    if (match(line, /total=[0-9]+/))          tot = substr(line, RSTART+6,  RLENGTH-6) + 0
    if (match(line, /supply_limited=[a-z]+/)) sl  = substr(line, RSTART+15, RLENGTH-15)
    if (seen && sl != lastsl) {
      print "[SUPPLY] supply_limited " lastsl " -> " sl "  (qualified=" q " total=" tot ")"; fflush()
    }
    if (q < MIN_POOL) {
      print "[SUPPLY] qualified=" q " < min_pool_size " MIN_POOL " — REDUNDANCY FLOOR BREACHED (total=" tot " supply_limited=" sl ")"
      fflush()
    }
    if (seen && tot != lasttot) {
      print "[SUPPLY] scorer-visible total " lasttot " -> " tot "  (qualified=" q ")"; fflush()
    }
    lastq = q; lasttot = tot; lastsl = sl; seen = 1
    next
  }
  if (line ~ /subscription ok/) {
    name = ""; n = ""
    if (match(line, /name=[^ ]+/))   name = substr(line, RSTART+5, RLENGTH-5)
    if (match(line, /nodes=[0-9]+/)) n    = substr(line, RSTART+6, RLENGTH-6)
    if (subs[name] != n) {
      print "[sub] " name " nodes=" n " (was " (subs[name]=="" ? "-" : subs[name]) ")"; fflush()
      subs[name] = n
    }
    next
  }
}'

SSH="ssh -o ConnectTimeout=10 -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -o StrictHostKeyChecking=no"

if [ -n "$SINCE" ]; then
  $SSH "dianwei@$HOST" \
    "sudo journalctl -u leap-gateway --since '$SINCE' --no-pager 2>/dev/null" \
    | awk -v MIN_POOL="$MIN_POOL" "$AWK_PROG"
  exit 0
fi

# Follow mode reconnects. A single dropped ssh session used to kill the watch
# outright ("Timeout, server not responding", exit 255, 2026-09-09 08:46Z) —
# and a dead monitor is indistinguishable from a quiet one, which is the worst
# failure mode for something whose whole job is to notice. ServerAliveInterval
# alone did NOT prevent it: keepalives detect a dead peer, they don't re-dial.
#
# Each reconnect restarts journalctl with -n0, so the gap's events are missed
# rather than replayed as live. That is deliberate: replaying would re-fire
# stale supply/flap events and corrupt the band. The RECONNECT line marks the
# gap so a hole in coverage is visible instead of silent.
while :; do
  $SSH "dianwei@$HOST" \
    "sudo journalctl -u leap-gateway -f -n0 --no-pager 2>/dev/null" \
    | awk -v MIN_POOL="$MIN_POOL" "$AWK_PROG"
  rc=$?
  echo "[RECONNECT] stream ended (rc=$rc) at $(date -u +%H:%M:%SZ) — re-dialing in 15s; events during the gap are NOT replayed"
  sleep 15
done
