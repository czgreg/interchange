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

set -euo pipefail

PROXY="${PROXY:-http://127.0.0.1:11080}"
TIERS="${TIERS:-10 30 50 80 100 150}"
TIER_SECS="${TIER_SECS:-30}"
MIHOMO_PROC="${MIHOMO_PROC:-/usr/local/bin/mihomo}"
TARGETS="${TARGETS:-https://www.gstatic.com/generate_204 https://api.github.com/ https://www.google.com/generate_204 https://chatgpt.com/}"

log() { printf '[stress] %s\n' "$*" >&2; }

command -v curl >/dev/null || { echo "need curl" >&2; exit 1; }

MPID=$(pgrep -f "$MIHOMO_PROC" | head -1 || true)
[ -n "$MPID" ] || { echo "[stress] mihomo process not found ($MIHOMO_PROC)" >&2; exit 1; }
log "mihomo pid=$MPID, proxy=$PROXY"
NCPU=$(nproc 2>/dev/null || echo 1)

WORKDIR=$(mktemp -d)
trap 'rm -rf "$WORKDIR"; jobs -p | xargs -r kill 2>/dev/null || true' EXIT

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

printf '%-7s %-7s %-8s %-8s %-8s %-8s %-8s\n' \
  TIER CPU% RSS_MB REQ/S P50ms P95ms ERR%
printf '%-7s %-7s %-8s %-8s %-8s %-8s %-8s\n' \
  ------ ----- ------ ------ ------ ------ ------

for tier in $TIERS; do
  rm -f "$WORKDIR/stop"
  : >"$WORKDIR/tier.log"
  # Spawn `tier` workers, each writing to the shared tier.log (append is
  # atomic for short lines on Linux).
  for i in $(seq 1 "$tier"); do
    worker "$i" "$WORKDIR/tier.log" &
  done

  # Let the tier warm up briefly, then sample proc over the hold window.
  sleep 2
  # Average a few CPU samples across the hold window.
  cpu_sum=0; cpu_n=0; rss_last=0
  end=$(( $(date +%s) + TIER_SECS ))
  while [ "$(date +%s)" -lt "$end" ]; do
    read -r c r < <(sample_proc)
    cpu_sum=$((cpu_sum + c)); cpu_n=$((cpu_n+1)); rss_last=$r
  done

  touch "$WORKDIR/stop"
  wait 2>/dev/null || true

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
  reqps=$(awk -v t="$total" -v s="$TIER_SECS" 'BEGIN{printf "%.0f", t/s}')
  errpct=$(awk -v e="$err" -v t="$total" 'BEGIN{printf "%.1f", t>0?e*100.0/t:0}')
  printf '%-7s %-7s %-8s %-8s %-8s %-8s %-8s\n' \
    "$tier" "$cpu_avg" "$rss_last" "$reqps" "${p50:-0}" "${p95:-0}" "$errpct"

  sleep 3   # cool-down between tiers
done

log "done. Read the ceiling off the table and write capacity.{sustained,degraded}_max_users into gateway.yaml."
