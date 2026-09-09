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


# ---------------------------------------------------------------------------
# Reconnect policy. An isolated redial must NOT alert (4 of them in 105min on
# 2026-09-09 produced 4 interrupts and 0 real faults); lost coverage must.
# drop_verdict is a pure function of its args so no real ssh drop is needed.
# ---------------------------------------------------------------------------
echo
echo "reconnect policy:"
# Refuse to source a script without the lib guard: without it the source
# falls through into the real ssh follow loop and the test hangs forever
# (hit on 2026-09-09 while reverse-verifying against the pre-guard version,
# which also orphaned an ssh child that had to be killed by hand).
grep -q 'WATCH_NODE_LIB' scripts/watch-node.sh || {
  echo "  SKIP reconnect policy — scripts/watch-node.sh has no WATCH_NODE_LIB guard;"
  echo "       sourcing it would start the real follow loop. Not sourcing."
  printf '\n%d passed, %d failed\n' "$pass" "$fail"
  [ "$fail" -eq 0 ]; exit $?
}
WATCH_NODE_LIB=1 source scripts/watch-node.sh
NOW=1000000

# Call drop_verdict the way the follow loop does: directly in this shell, then
# read the out-variables. NOT inside $( ) — the earlier harness captured stdout
# instead, which is why it passed while production died on line 174.
v_silent() { # name, lived, rc, drops...
  local name="$1" lived="$2" rc="$3"; shift 3
  VERDICT_OUT="<unset>"
  drop_verdict "$lived" "$rc" "$NOW" "$@"
  [ -z "$VERDICT_OUT" ] && ok "$name" || bad "$name" "expected silence, got: $VERDICT_OUT"
}
v_match() { # name, regex, lived, rc, drops...
  local name="$1" re="$2" lived="$3" rc="$4"; shift 4
  VERDICT_OUT="<unset>"
  drop_verdict "$lived" "$rc" "$NOW" "$@"
  printf '%s' "$VERDICT_OUT" | grep -qE "$re" && ok "$name" \
    || bad "$name" "expected /$re/, got: ${VERDICT_OUT:-<silence>}"
}

# 1. The measured production case: one drop after ~27min of healthy stream.
v_silent "isolated_redial_is_silent (held 1620s)" 1620 255

# 2. Two drops in the window is still not worth an interrupt (RAPID_MAX=3).
v_silent "two_drops_in_window_silent" 1620 255 $(( NOW - 600 ))

# 3. Three in the window IS a flapping path.
v_match "three_drops_fires" "PATH FLAPPING" 1620 255 $(( NOW - 600 )) $(( NOW - 300 ))

# 4. Drops older than the window must not accumulate toward the threshold.
v_silent "stale_drops_pruned (2 outside 900s)" 1620 255 $(( NOW - 5000 )) $(( NOW - 4000 ))

# 5. A stream that dies immediately means the redial did not really work.
v_match "stream_will_not_hold_fires (died in 3s)" "STREAM WILL NOT HOLD" 3 255

# 6. After reporting a flap the window resets, so the next isolated drop is
#    quiet again instead of re-firing on every subsequent drop.
drop_verdict 1620 255 "$NOW" $(( NOW - 600 )) $(( NOW - 300 ))
[ -z "$DROPS_OUT" ] && ok "flap_report_resets_window" \
  || bad "flap_report_resets_window" "expected empty DROPS_OUT, got: $DROPS_OUT"

# 7. The regression that took the monitor down on 2026-09-09: the follow loop
#    calls drop_verdict and then reads BOTH out-variables in the same shell. The
#    first version returned the verdict on stdout, so the caller had to wrap it
#    in $( ) — a subshell — and the DROPS_OUT read that followed aborted under
#    `set -u` with "line 174: DROPS_OUT: unbound variable", killing the monitor
#    on its first real ssh drop. Every test above happened to sidestep the
#    combination: v_silent/v_match captured stdout without reading DROPS_OUT,
#    and case 6 read DROPS_OUT without capturing. This asserts the real pairing.
unset VERDICT_OUT DROPS_OUT
loop_drops=""
drop_verdict 3 255 "$NOW" $loop_drops
loop_verdict="${VERDICT_OUT-<unset: still returns via stdout>}"
loop_drops="${DROPS_OUT-<unset: lost in a subshell>}"
case "$loop_verdict$loop_drops" in
  *unset*) bad "caller_pattern_propagates_both" \
             "verdict=[$loop_verdict] drops=[$loop_drops]" ;;
  *) case "$loop_verdict" in
       *"STREAM WILL NOT HOLD"*)
         [ -n "$loop_drops" ] && ok "caller_pattern_propagates_both" \
           || bad "caller_pattern_propagates_both" "DROPS_OUT empty, drop not recorded" ;;
       *) bad "caller_pattern_propagates_both" "wrong verdict: [$loop_verdict]" ;;
     esac ;;
esac

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
