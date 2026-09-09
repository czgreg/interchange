#!/usr/bin/env bash
# watch-node-test.sh — assert watch-node.sh's filter says nothing when the pool
# is merely changing, and speaks up when it breaks.
#
# The filter's contract is "silence means nothing changed and nothing broke".
# Two shapes violated it in production on 2026-09-09 and this pins both:
#
#   growth_is_silent      — the pool getting BIGGER used to fire OUTSIDE-band.
#   reconnect_is_silent   — band state lived in awk BEGIN, which re-runs per
#                           redial, so a value already accepted re-fired after
#                           a reconnect. Modelled here by running the program
#                           twice over the same stream, as a redial does.
#   calm_is_silent        — a cumulative "every 12th swap" counter fired on a
#                           calm pool because it had no time base.
#
# Run: scripts/watch-node-test.sh
set -uo pipefail
cd "$(dirname "$0")/.."

# Extract the awk program from the script under test — no duplicated copy.
# Let bash parse the assignment itself rather than guessing at line shapes:
# the closing quote shares a line with the awk body's final brace, so any
# line-range sed is fragile. This evals ONLY the AWK_PROG=... assignment.
eval "$(awk '/^AWK_PROG=/{f=1} f{print} f && /}.$/ && !/^AWK_PROG=/{exit}' scripts/watch-node.sh)"
[ -n "$AWK_PROG" ] || { echo "FATAL: could not extract AWK_PROG"; exit 2; }

run() { awk -v MIN_POOL="${MIN_POOL:-4}" "$AWK_PROG"; }

# NOTE: $(cmd) strips trailing newlines, so "$(scored 6)$(scored 8)" collapses
# two journal records onto ONE line and awk sees a single record. Multi-record
# fixtures must be built by appending to a buffer with an explicit newline.
scored() { # qualified total -> one journal line (no trailing newline)
  printf 'time=2026-09-09T12:00:00Z level=INFO msg="nodescorer: scored" qualified=%s total=%s tier1_count=12 pool_changed=false supply_limited=false' "$1" "$2"
}

buf=""
add() { # append one record to $buf, newline-separated
  if [ -z "$buf" ]; then buf="$1"; else buf="$buf
$1"; fi
}

pass=0; fail=0
ok()   { pass=$((pass+1)); printf '  ok   %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  FAIL %s\n     %s\n' "$1" "$2"; }

expect_silent() { # name, input
  local name="$1" input="$2" out
  out="$(printf '%s\n' "$input" | run)"
  [ -z "$out" ] && ok "$name" || bad "$name" "expected silence, got: $out"
}

expect_match() { # name, input, regex
  local name="$1" input="$2" re="$3" out
  out="$(printf '%s\n' "$input" | run)"
  printf '%s' "$out" | grep -qE "$re" && ok "$name" || bad "$name" "expected /$re/, got: ${out:-<silence>}"
}

echo "watch-node filter:"

# 1. Pool growing is good news. Never interrupt for it.
buf=""; for q in 6 8 9 12; do add "$(scored "$q" 25)"; done
expect_silent "growth_is_silent (6->8->9->12)" "$buf"

# 2. A redial re-runs BEGIN. A value already accepted must not re-fire.
#    Two sequential runs over the same stream = the pool state before and
#    after a reconnect.
redial_out="$( { printf '%s\n' "$(scored 9 25)"; } | run; { printf '%s\n' "$(scored 9 25)"; } | run )"
[ -z "$redial_out" ] && ok "reconnect_is_silent (qualified=9 twice across redial)" \
  || bad "reconnect_is_silent (qualified=9 twice across redial)" "expected silence, got: $redial_out"

# 3. A calm pool oscillating inside its normal range must not accumulate into
#    an alarm. 30 alternating rounds is ~2.5h of real time.
buf=""; for i in $(seq 15); do add "$(scored 8 25)"; add "$(scored 9 25)"; done
expect_silent "calm_is_silent (30 alternating rounds)" "$buf"

# 4. The floor IS an incident.
expect_match "floor_breach_fires (qualified=3 < 4)" \
  "$(scored 3 25)" "REDUNDANCY FLOOR BREACHED"

# 5. At the floor exactly is not a breach.
expect_silent "at_floor_is_silent (qualified=4)" "$(scored 4 25)"

# 6. Real faults are always emitted — the one thing that must never be quiet.
expect_match "warn_fires" \
  'time=2026-09-09T12:00:00Z level=WARN msg="subscription unhealthy" name=foo' '^\[!\]'
expect_match "render_fail_fires" \
  'time=2026-09-09T12:00:00Z level=INFO msg="render: failed to write config"' '^\[!\]'
expect_match "panic_fires" \
  'panic: runtime error: invalid memory address' '^\[!\]'

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
