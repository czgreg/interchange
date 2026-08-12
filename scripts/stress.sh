#!/usr/bin/env bash
# scripts/stress.sh — single-node capacity ceiling finder.
#
# Ramps simulated concurrent "users" through the leap-internal mihomo HTTP
# proxy (127.0.0.1:11080 by default), sampling mihomo CPU / RSS / request
# latency at each tier. Run it ON the forwarding node (it needs the local
# proxy + access to mihomo's process stats).
#
#   sudo bash scripts/stress.sh
#
# Each "user" is a worker loop issuing back-to-back HTTPS GETs to a mix of
# the real destinations employees hit, through the proxy. This approximates
# the request rate of an active employee (page loads + API polls + websocket
# keepalives) without needing real browsers.
#
# Output: a table of tier → (CPU%, RSS MB, req/s, p50 ms, p95 ms, error%).
# Read the ceiling off it: sustained_max = last tier with CPU < ~50% and
# p95 not spiking; degraded_max = last tier that still completes requests.
# Then write those two numbers into gateway.yaml's `capacity:` block.
#
# Env overrides:
#   PROXY        proxy URL              (default http://127.0.0.1:11080)
#   TIERS        space-separated user counts (default "10 30 50 80 100 150")
#   TIER_SECS    seconds to hold each tier (default 30)
#   TARGETS      space-separated URLs   (default a built-in AI/dev mix)
#   MIHOMO_PROC  pgrep pattern          (default /usr/local/bin/mihomo)
#
# ── Single-egress mode (EGRESS=<proxy-name>) ────────────────────────────
#
# The default mode measures the WHOLE POOL: 127.0.0.1:11080 falls through to
# MATCH,us-pool, so load-balance spreads every request across all K members
# and the resulting table is an aggregate ceiling. It says nothing about what
# ONE airport node can carry — which is the number you need before lowering
# the pool size K (capacity.cap_per_node).
#
#   sudo EGRESS='🇺🇸 US-01' bash scripts/stress.sh
#   sudo EGRESS_LIST=1 bash scripts/stress.sh     # print candidate names
#
# Mechanism: spawn a SECOND, throwaway mihomo holding only that one outbound,
# on its own loopback port, and sample THAT process. The live instance is
# never touched — specifically we do NOT
#   • PUT /proxies/<selector> to pin the live selector: that drags real
#     client traffic onto the node under test, and leaves the selector
#     pinned if the script dies;
#   • rewrite /var/lib/leap/mihomo/config.yaml + reload: that interrupts
#     every live connection and races leap-gateway's own renderer.
# The child writes only into its own mktemp dir, binds only 127.0.0.1, and
# picks ports that must differ from live (11080/11081/9090/7893/53).
#
# What it still shares with production: the airport node's own bandwidth and
# the node's uplink. This is unavoidable — per-egress capacity cannot be
# measured without sending traffic through that egress. Keep TIERS modest and
# do not run it during business hours on a node carrying real users.
#
#   EGRESS        proxy name from the live config (exact, quote it)
#   EGRESS_LIST   =1 → list us-pool member names and exit
#   CHILD_PORT    child mixed-port        (default 21080)
#   LIVE_CONFIG   live rendered config    (default /var/lib/leap/mihomo/config.yaml)

set -euo pipefail

PROXY="${PROXY:-http://127.0.0.1:11080}"
TIERS="${TIERS:-10 30 50 80 100 150}"
TIER_SECS="${TIER_SECS:-30}"
MIHOMO_PROC="${MIHOMO_PROC:-/usr/local/bin/mihomo}"
EGRESS="${EGRESS:-}"
EGRESS_LIST="${EGRESS_LIST:-}"
CHILD_PORT="${CHILD_PORT:-21080}"
LIVE_CONFIG="${LIVE_CONFIG:-/var/lib/leap/mihomo/config.yaml}"
# Ports the live instance owns. The child must not bind any of these; asserted
# at startup rather than trusted, because binding 7893 or :53 would hijack the
# production data path.
LIVE_PORTS="11080 11081 9090 7893 53 18080"
# Default targets must consistently return 2xx/3xx for unauthenticated GETs —
# the script's ERR% otherwise conflates "proxy can't keep up" with "the
# upstream just doesn't accept anonymous root pings". chatgpt.com (CF
# challenge → 403) and api.github.com/ (root + datacenter IP rate-limit
# → 403) used to be defaults and inflated baseline ERR% to ~50% at tier=10.
# Four diverse orgs/CDNs that all reliably 2xx/3xx through the proxy:
#   gstatic.com/generate_204            Google CDN, 204
#   cloudflare.com/cdn-cgi/trace        Cloudflare edge, 200
#   google.com/robots.txt               Google direct, 200
#   detectportal.firefox.com/success.txt  Mozilla edge, 200
TARGETS="${TARGETS:-https://www.gstatic.com/generate_204 https://www.cloudflare.com/cdn-cgi/trace https://www.google.com/robots.txt https://detectportal.firefox.com/success.txt}"

log() { printf '[stress] %s\n' "$*" >&2; }

command -v curl >/dev/null || { echo "need curl" >&2; exit 1; }

NCPU=$(nproc 2>/dev/null || echo 1)
WORKDIR=$(mktemp -d)
CHILD_PID=""
CHILD_DIR=""

# Kill the child first, then workers. Order matters: leaving the child alive
# with an unreachable proxy would make in-flight workers spin on errors.
cleanup() {
  if [ -n "$CHILD_PID" ] && kill -0 "$CHILD_PID" 2>/dev/null; then
    log "stopping child mihomo pid=$CHILD_PID"
    kill "$CHILD_PID" 2>/dev/null || true
    for _ in 1 2 3 4 5; do
      kill -0 "$CHILD_PID" 2>/dev/null || break
      sleep 0.3
    done
    kill -9 "$CHILD_PID" 2>/dev/null || true
  fi
  for wp in "${worker_pids[@]:-}"; do
    [ -n "$wp" ] && kill "$wp" 2>/dev/null || true
  done
  jobs -p | xargs -r kill 2>/dev/null || true
  rm -rf "$WORKDIR"
  [ -n "$CHILD_DIR" ] && rm -rf "$CHILD_DIR"
  return 0
}
# HUP/INT/TERM as well as EXIT: an EXIT-only trap does not fire when the
# controlling ssh session is killed, which orphaned a child mihomo holding
# port 21080 and made the next run refuse to start.
trap cleanup EXIT HUP INT TERM
worker_pids=()

# ---- single-egress mode -------------------------------------------------

# List candidate egress names: us-pool members if the group exists, else all
# proxies. Printed one per line so the operator can copy-paste into EGRESS.
list_egress() {
  python3 - "$LIVE_CONFIG" <<'PY'
import sys, yaml
d = yaml.safe_load(open(sys.argv[1])) or {}
groups = {g.get("name"): g for g in d.get("proxy-groups") or []}
pool = groups.get("us-pool") or {}
names = pool.get("proxies") or [p.get("name") for p in d.get("proxies") or []]
for n in names:
    print(n)
PY
}

# Build a minimal config holding ONLY the named proxy. Everything that could
# collide with the live instance is explicitly off: no tun, no tproxy-port, no
# redir-port, no external-controller, and a DNS block that resolves internally
# but binds no listener (omitting dns entirely would push the proxy server's
# own hostname onto the system resolver, which on this node is the live
# mihomo — an accidental dependency on the process we are trying to isolate).
write_child_config() {
  local name="$1" out="$2" port="$3"
  python3 - "$LIVE_CONFIG" "$name" "$out" "$port" <<'PY'
import sys, yaml
live_path, name, out, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
live = yaml.safe_load(open(live_path)) or {}
proxies = live.get("proxies") or []
match = [p for p in proxies if p.get("name") == name]
if not match:
    sys.stderr.write("proxy not found in %s: %r\n" % (live_path, name))
    sys.exit(2)
cfg = {
    "mixed-port": port,
    "bind-address": "127.0.0.1",
    "allow-lan": False,
    "mode": "rule",
    "log-level": "warning",
    "ipv6": False,
    "tun": {"enable": False},
    "dns": {
        "enable": True,
        "ipv6": False,
        "enhanced-mode": "redir-host",
        "default-nameserver": ["119.29.29.29"],
        "nameserver": (live.get("dns") or {}).get("proxy-server-nameserver")
                      or ["https://doh.pub/dns-query"],
    },
    "proxies": [match[0]],
    "rules": ["MATCH,%s" % name],
}
with open(out, "w") as f:
    yaml.safe_dump(cfg, f, allow_unicode=True, sort_keys=False)
PY
}

if [ -n "$EGRESS_LIST" ]; then
  list_egress
  exit 0
fi

if [ -n "$EGRESS" ]; then
  # Refuse to reuse a live port even if the operator overrode CHILD_PORT.
  for p in $LIVE_PORTS; do
    [ "$CHILD_PORT" = "$p" ] && {
      echo "[stress] CHILD_PORT=$CHILD_PORT is a live mihomo port; pick another" >&2
      exit 1; }
  done
  if ss -ltn "sport = :$CHILD_PORT" 2>/dev/null | grep -q ":$CHILD_PORT"; then
    echo "[stress] CHILD_PORT=$CHILD_PORT already in use" >&2
    exit 1
  fi
  [ -r "$LIVE_CONFIG" ] || { echo "[stress] cannot read $LIVE_CONFIG (need sudo?)" >&2; exit 1; }

  CHILD_DIR=$(mktemp -d)
  write_child_config "$EGRESS" "$CHILD_DIR/config.yaml" "$CHILD_PORT" || {
    echo "[stress] failed to build child config; try EGRESS_LIST=1 to see valid names" >&2
    exit 1; }

  # Validate before launching, so a bad outbound fails loudly here instead of
  # showing up as 100% ERR in the table.
  "$MIHOMO_PROC" -t -d "$CHILD_DIR" -f "$CHILD_DIR/config.yaml" >"$CHILD_DIR/test.log" 2>&1 || {
    echo "[stress] child config rejected by mihomo -t:" >&2
    tail -20 "$CHILD_DIR/test.log" >&2
    exit 1; }

  "$MIHOMO_PROC" -d "$CHILD_DIR" -f "$CHILD_DIR/config.yaml" >"$CHILD_DIR/run.log" 2>&1 &
  CHILD_PID=$!

  # Wait for the port to actually accept before starting workers.
  ready=""
  for _ in $(seq 1 40); do
    kill -0 "$CHILD_PID" 2>/dev/null || break
    if ss -ltn "sport = :$CHILD_PORT" 2>/dev/null | grep -q ":$CHILD_PORT"; then
      ready=1; break
    fi
    sleep 0.25
  done
  [ -n "$ready" ] || {
    echo "[stress] child mihomo failed to listen on $CHILD_PORT:" >&2
    tail -20 "$CHILD_DIR/run.log" >&2
    exit 1; }

  PROXY="http://127.0.0.1:$CHILD_PORT"
  MPID="$CHILD_PID"
  log "single-egress mode: egress=$EGRESS child_pid=$CHILD_PID proxy=$PROXY"
  log "live instance untouched; child dir=$CHILD_DIR"
else
  MPID=$(pgrep -f "$MIHOMO_PROC" | head -1 || true)
  [ -n "$MPID" ] || { echo "[stress] mihomo process not found ($MIHOMO_PROC)" >&2; exit 1; }
  log "mihomo pid=$MPID, proxy=$PROXY (WHOLE-POOL aggregate; set EGRESS= for per-node)"
fi

# One worker: loop issuing proxied GETs until the stop-file appears. Appends
# "<curl_exit> <http_code> <time_total>" per request to its own log.
worker() {
  local id="$1" logf="$2"
  local -a urls=($TARGETS)
  local n=${#urls[@]} i=0
  while [ ! -f "$WORKDIR/stop" ]; do
    local u="${urls[$((i % n))]}"
    i=$((i+1))
    curl -s -o /dev/null -x "$PROXY" --max-time 15 \
      -w '%{http_code} %{time_total}\n' "$u" 2>/dev/null \
      >>"$logf" || echo "000 0" >>"$logf"
  done
}

# Sample mihomo cpu% over a 1s window + current RSS (MB).
sample_proc() {
  # Re-resolve MPID if the previously-cached one is gone — mihomo can be
  # restarted mid-run by leap-gateway's auto-reload after a subscription
  # refresh. Without this, late-tier rows show CPU=0 RSS=0 (sampling
  # artifact) and the operator misreads the ceiling.
  #
  # NEVER re-resolve in single-egress mode. `pgrep -f /usr/local/bin/mihomo`
  # matches the LIVE instance as well as the child, so if the child died the
  # fallback would silently start sampling production CPU and print it as the
  # per-egress ceiling — a wrong number that looks entirely plausible.
  #
  # This function runs inside `< <(...)`, i.e. a subshell, so it cannot abort
  # the script itself: `exit` here would only kill the subshell and the tier
  # loop would keep going. It emits the CHILD_DEAD sentinel instead, and
  # assert_child_alive in the main shell does the aborting.
  if [ ! -r "/proc/$MPID/stat" ]; then
    if [ -n "$EGRESS" ]; then
      echo "CHILD_DEAD 0"
      return
    fi
    MPID=$(pgrep -f "$MIHOMO_PROC" | head -1 || true)
    [ -n "$MPID" ] || { echo "0 0"; return; }
  fi
  # /proc/<pid>/stat utime+stime in clock ticks; delta over 1s / Hz / ncpu.
  local hz; hz=$(getconf CLK_TCK)
  local t1 t2
  t1=$(awk '{print $14+$15}' "/proc/$MPID/stat" 2>/dev/null || echo 0)
  sleep 1
  t2=$(awk '{print $14+$15}' "/proc/$MPID/stat" 2>/dev/null || echo 0)
  local cpu; cpu=$(awk -v a="$t1" -v b="$t2" -v hz="$hz" -v nc="$NCPU" \
    'BEGIN{ printf "%.0f", (b-a)/hz*100 }')
  local rss; rss=$(awk '/VmRSS/{print int($2/1024)}' "/proc/$MPID/status" 2>/dev/null || echo 0)
  echo "$cpu $rss"
}

# Runs in the MAIN shell, so `exit` actually terminates the run. Called with
# the raw first field of a sample_proc line.
assert_child_alive() {
  [ "$1" = "CHILD_DEAD" ] || return 0
  echo "[stress] child mihomo (pid=$MPID) died mid-run. Aborting instead of" >&2
  echo "[stress] falling back to the live instance, which would report" >&2
  echo "[stress] production CPU as this egress's ceiling. Last child log:" >&2
  tail -20 "$CHILD_DIR/run.log" >&2 2>/dev/null || true
  exit 1
}

if [ -n "$EGRESS" ]; then
  log "measuring SINGLE EGRESS: $EGRESS"
else
  log "measuring WHOLE POOL (us-pool aggregate)"
fi

printf '%-7s %-7s %-8s %-8s %-8s %-8s %-8s\n' \
  TIER CPU% RSS_MB REQ/S P50ms P95ms ERR%
printf '%-7s %-7s %-8s %-8s %-8s %-8s %-8s\n' \
  ------ ----- ------ ------ ------ ------ ------

for tier in $TIERS; do
  rm -f "$WORKDIR/stop"
  : >"$WORKDIR/tier.log"
  # Spawn `tier` workers, each writing to the shared tier.log (append is
  # atomic for short lines on Linux).
  # Collect worker PIDs explicitly. A bare `wait` would also wait on the
  # child mihomo from single-egress mode, which never exits — that hung the
  # first real run for 8 minutes at tier 5 with the tier data already
  # collected. `wait` waits for ALL background jobs, and EGRESS mode adds
  # one that runs forever, so the worker PIDs have to be named.
  worker_pids=()
  for i in $(seq 1 "$tier"); do
    worker "$i" "$WORKDIR/tier.log" &
    worker_pids+=($!)
  done

  # Let the tier warm up briefly, then sample proc over the hold window.
  sleep 2
  # Average a few CPU samples across the hold window.
  cpu_sum=0; cpu_n=0; rss_last=0
  end=$(( $(date +%s) + TIER_SECS ))
  while [ "$(date +%s)" -lt "$end" ]; do
    read -r c r < <(sample_proc)
    assert_child_alive "$c"
    cpu_sum=$((cpu_sum + c)); cpu_n=$((cpu_n+1)); rss_last=$r
  done

  touch "$WORKDIR/stop"
  # Only the workers — never a bare `wait` (see worker_pids above).
  for wp in "${worker_pids[@]}"; do
    wait "$wp" 2>/dev/null || true
  done

  # Aggregate request stats from tier.log (col1=http_code, col2=time_total s).
  cpu_avg=$((cpu_sum / (cpu_n>0?cpu_n:1)))
  total=$(wc -l <"$WORKDIR/tier.log" | tr -d ' ')
  err=$(awk '$1 !~ /^[23]/{n++} END{print n+0}' "$WORKDIR/tier.log")
  # ok latencies in ms, sorted, for percentile picking.
  awk '$1 ~ /^[23]/{printf "%.0f\n", $2*1000}' "$WORKDIR/tier.log" \
    | sort -n >"$WORKDIR/lat.sorted"
  okn=$(wc -l <"$WORKDIR/lat.sorted" | tr -d ' ')
  pick() { # pick percentile $1 from sorted lat file
    local p="$1"
    [ "$okn" -gt 0 ] || { echo 0; return; }
    local idx=$(( (okn * p + 99) / 100 ))   # ceil
    [ "$idx" -lt 1 ] && idx=1
    sed -n "${idx}p" "$WORKDIR/lat.sorted"
  }
  p50=$(pick 50); p95=$(pick 95)
  reqps=$(awk -v t="${total:-0}" -v s="$TIER_SECS" 'BEGIN{ printf "%.0f", t/s }')
  # if/else instead of a ternary in printf args — Ubuntu's mawk parse-errors
  # on `printf fmt, cond?a:b` (comma ambiguity).
  errpct=$(awk -v e="${err:-0}" -v t="${total:-0}" 'BEGIN{ if (t>0) printf "%.1f", e*100.0/t; else printf "0.0" }')
  printf '%-7s %-7s %-8s %-8s %-8s %-8s %-8s\n' \
    "$tier" "$cpu_avg" "$rss_last" "$reqps" "${p50:-0}" "${p95:-0}" "$errpct"

  sleep 3   # cool-down between tiers
done

if [ -n "$EGRESS" ]; then
  log "done (single egress: $EGRESS)."
  log "This is a PER-NODE ceiling. Do NOT write it into capacity.{sustained,degraded}_max_users"
  log "— those are whole-node numbers from the default mode. Use it to size the pool:"
  log "roughly K >= expected_concurrent_users / this_tier, then re-check with the default mode."
  log "Repeat across a few members before trusting it; airport nodes vary widely."
else
  log "done. Read the ceiling off the table and write capacity.{sustained,degraded}_max_users into gateway.yaml."
fi
