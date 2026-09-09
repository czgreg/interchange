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
# rather than replayed as live. Replaying would re-emit events already judged,
# and the 30-minute inspection reads the journal in full anyway.
#
# An ISOLATED redial is not news and must not interrupt. Measured 2026-09-09
# 11:36/12:03/12:47/13:21Z: 4 drops in 105min, every one self-healed in 15s,
# zero real faults in the same span — the watch was reporting only its own
# plumbing. Root cause is the network path, not the node: 92's sshd runs
# clientaliveinterval=0 so it never probes the client, the path crosses at
# least one router (Mac gw 192.168.100.1, seen at the node as 192.168.102.117,
# node on 192.168.70.0/24), and `journalctl -f` is silent between 5-minute
# scoring rounds, so an intermediate NAT/firewall reaps the idle flow. sshd
# logs `session opened` for each redial with no matching `session closed` for
# the dead one, which is the signature of a network-layer drop rather than a
# clean teardown. Raising ServerAliveCountMax would only slow DETECTION (now
# 90s); it cannot stop a middlebox from reaping the flow.
#
# So: stay silent for a redial that restores the stream, and speak up for the
# two states that actually mean lost coverage —
#   1. the redial itself fails (stream ends again in under RAPID_SECS), or
#   2. RAPID_MAX drops inside RAPID_WINDOW, i.e. a flapping path rather than
#      a one-off blip.
# The gap is still recorded to the output file either way, so a hole in
# coverage stays inspectable; only the interrupt is suppressed.
RAPID_SECS="${RAPID_SECS:-45}"      # a stream dying this fast did not come back
RAPID_WINDOW="${RAPID_WINDOW:-900}" # 15min
RAPID_MAX="${RAPID_MAX:-3}"         # drops in the window before it is worth reporting

# drop_verdict LIVED RC NOW DROPS... -> sets VERDICT_OUT and DROPS_OUT.
# A function of its arguments only (no clock, no reads of prior state) so
# scripts/watch-node-test.sh can drive every branch without waiting on a real
# ssh drop.
#
#   VERDICT_OUT — the line to emit, or empty for "stay quiet".
#   DROPS_OUT   — the pruned drop list the caller should keep.
#
# MUST NOT be called inside $( ) — that runs it in a subshell and neither
# out-variable reaches the caller. The first version returned the verdict on
# stdout, which forced the caller into command substitution and made
# DROPS_OUT unreadable; under `set -u` the read aborted the whole monitor on
# the first real ssh drop. Both results are out-variables now precisely so
# there is nothing to capture and no subshell to lose them in.
drop_verdict() {
  local lived="$1" rc="$2" now="$3"; shift 3
  local t pruned="" n=0
  for t in "$@"; do
    [ $(( now - t )) -lt "$RAPID_WINDOW" ] && { pruned="$pruned $t"; n=$(( n + 1 )); }
  done
  pruned="$pruned $now"; n=$(( n + 1 ))
  DROPS_OUT="$pruned"
  VERDICT_OUT=""
  if [ "$lived" -lt "$RAPID_SECS" ]; then
    VERDICT_OUT="[!] STREAM WILL NOT HOLD — died after ${lived}s (rc=$rc); coverage is NOT continuous"
  elif [ "$n" -ge "$RAPID_MAX" ]; then
    VERDICT_OUT="[!] PATH FLAPPING — $n drops in the last $(( RAPID_WINDOW / 60 ))min (latest rc=$rc, held ${lived}s)"
    DROPS_OUT=""   # reported; start a fresh window rather than re-firing per drop
  fi
  # else: isolated blip — VERDICT_OUT stays empty, deliberately.
}

# Sourced by the test harness, which wants the functions but not the stream.
[ -n "${WATCH_NODE_LIB:-}" ] && return 0

drops=""             # space-separated epoch seconds of recent drops
DROPS_OUT=""         # set by drop_verdict; defined here so `set -u` holds
VERDICT_OUT=""
while :; do
  started=$(date +%s)
  $SSH "dianwei@$HOST" \
    "sudo journalctl -u leap-gateway -f -n0 --no-pager 2>/dev/null" \
    | awk -v MIN_POOL="$MIN_POOL" "$AWK_PROG"
  rc=$?
  now=$(date +%s)
  lived=$(( now - started ))

  drop_verdict "$lived" "$rc" "$now" $drops   # no $( ) — see the note above
  drops="$DROPS_OUT"
  if [ -n "$VERDICT_OUT" ]; then
    echo "$VERDICT_OUT"
  else
    # Isolated blip. Recorded for the coverage trail, deliberately not an alert:
    # stderr lands in the output file without raising a notification.
    echo "[gap] redial at $(date -u +%H:%M:%SZ) after ${lived}s (rc=$rc) — 15s hole, not replayed" >&2
  fi
  sleep 15
done
