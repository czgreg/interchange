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
# Baseline recorded at 557ba03 (2026-09-09 06:26Z), pre-obfs-fix: qualified
# oscillated 2<->3 with excursions to 4, supply_limited flipped with it,
# pool_stability_24h.swap_count=86. The band self-widens as it observes, so
# a genuine improvement (>=4 sustained) or regression (<=1) reports as
# OUTSIDE-band once and then becomes the new normal.
set -uo pipefail

HOST="192.168.70.92"
SINCE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --since) SINCE="${2:-}"; shift 2 ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) HOST="$1"; shift ;;
  esac
done

AWK_PROG='
BEGIN { lo = 2; hi = 3; flaps = 0; seen = 0 }
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
    if (q > hi || q < lo) {
      print "[SUPPLY] qualified=" q " total=" tot " supply_limited=" sl "  << OUTSIDE band " lo "-" hi
      fflush()
      if (q > hi) hi = q
      if (q < lo) lo = q
    } else if (seen && q != lastq) {
      flaps++
      if (flaps % 12 == 0) {
        print "[flap] " flaps " in-band swaps (band " lo "-" hi ", now qualified=" q " total=" tot ")"; fflush()
      }
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

SSH="ssh -o ConnectTimeout=10 -o ServerAliveInterval=30 -o StrictHostKeyChecking=no"

if [ -n "$SINCE" ]; then
  $SSH "dianwei@$HOST" \
    "sudo journalctl -u leap-gateway --since '$SINCE' --no-pager 2>/dev/null" \
    | awk "$AWK_PROG"
else
  # -n0 so we start at "now" rather than replaying the tail.
  $SSH "dianwei@$HOST" \
    "sudo journalctl -u leap-gateway -f -n0 --no-pager 2>/dev/null" \
    | awk "$AWK_PROG"
fi
