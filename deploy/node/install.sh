#!/bin/bash
# leap-gateway sidecar installer for FeiLian forwarding nodes.
#
# Run as root on the FeiLian forwarding node (e.g. dianwei@192.168.70.92 with sudo).
# Idempotent — safe to re-run after edits.
#
# Inputs (must be present in working directory or fetched):
#   - leap-gateway       Linux binary (built with `GOOS=linux go build ./cmd/gateway`)
#   - sing-box           1.10.x binary (downloaded by --bootstrap-singbox if missing)
#   - mihomo             1.19.x binary (optional; required only if engine=mihomo)
#   - gateway.yaml       Filled-in config (subscription URL + node values + optional engine)
#
# Companion files (same directory as this script):
#   leap-singbox.service / leap-mihomo.service / leap-gateway.service /
#   leap-nft.service / nft.conf.tmpl / iproute.sh / logrotate-leap.conf
#
# Engine selection is read from gateway.yaml's `singbox.engine` field
# (default "sing-box"; valid: "sing-box" | "mihomo"). Both engines' binaries
# and unit files are installed; only the active one is enabled. Switching
# engines later: edit /etc/leap/gateway.yaml singbox.engine, run install.sh
# again — it stops the old engine, enables the new one, restarts leap-nft.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SINGBOX_VERSION="${SINGBOX_VERSION:-1.10.7}"
MIHOMO_VERSION="${MIHOMO_VERSION:-1.19.26}"
SKIP_PRECHECK="${SKIP_PRECHECK:-0}"

log()  { printf '[install] %s\n' "$*"; }
fail() { printf '[install][FAIL] %s\n' "$*" >&2; exit 1; }

# Read the active engine from gateway.yaml. Defaults to sing-box for the
# preflight stage when /etc/leap/gateway.yaml doesn't exist yet (first
# install). Read both from $SCRIPT_DIR (incoming) and /etc/leap (existing)
# in that order so re-runs after operator edits pick up the new value.
detect_engine() {
  local f
  for f in "$SCRIPT_DIR/gateway.yaml" /etc/leap/gateway.yaml; do
    [ -f "$f" ] || continue
    local e
    e=$(awk '/^[[:space:]]*engine:[[:space:]]/{
      gsub(/^[[:space:]]*engine:[[:space:]]*/, "")
      gsub(/[[:space:]"'\''#].*$/, "")
      print; exit
    }' "$f")
    if [ -n "$e" ]; then
      echo "$e"
      return
    fi
  done
  echo "sing-box"
}

ENGINE=$(detect_engine)
log "active engine: $ENGINE"
case "$ENGINE" in
  sing-box|mihomo) ;;
  *) fail "invalid engine '$ENGINE' in gateway.yaml — must be sing-box | mihomo" ;;
esac

# ---------------------------------------------------------------------------
# 0. Root check
[ "$EUID" -eq 0 ] || fail "must run as root (sudo)"

# ---------------------------------------------------------------------------
# 1. Pre-flight checks (Stage 0 verified these on dianwei@192.168.70.92, but
#    re-check on every install in case node state drifted)

precheck() {
  log "preflight: kernel $(uname -r)"
  log "preflight: $(. /etc/os-release && echo "$PRETTY_NAME")"

  # FeiLian must be running.
  systemctl is-active --quiet feilian-tun@tun0.service \
    || fail "feilian-tun@tun0 not active — is this a FeiLian forwarding node?"
  log "preflight: feilian-tun@tun0 active"

  # tun0 must exist with the IP that matches gateway.yaml.
  if [ -f "$SCRIPT_DIR/gateway.yaml" ]; then
    expected_gw=$(grep -E '^\s*tun0_gateway_ip:' "$SCRIPT_DIR/gateway.yaml" | awk -F'"' '{print $2}')
    actual_gw=$(ip -4 -o addr show tun0 2>/dev/null | awk '{print $4}' | cut -d/ -f1)
    [ -z "$expected_gw" ] && fail "gateway.yaml missing node.tun0_gateway_ip"
    [ "$expected_gw" = "$actual_gw" ] \
      || fail "node.tun0_gateway_ip=$expected_gw but tun0 has $actual_gw"
    log "preflight: tun0=$actual_gw matches gateway.yaml"
  fi

  # rp_filter must be loose for fwmark policy routing.
  rp=$(sysctl -n net.ipv4.conf.all.rp_filter)
  case "$rp" in
    0|2) log "preflight: rp_filter=$rp (loose, OK)" ;;
    *)   fail "rp_filter=$rp; need 0 or 2 for fwmark routing. Adjust /etc/sysctl.d/" ;;
  esac

  # IP forwarding must be on (FeiLian needs it anyway, but verify).
  fwd=$(sysctl -n net.ipv4.ip_forward)
  [ "$fwd" = "1" ] || fail "ip_forward=$fwd, need 1"
  log "preflight: ip_forward=1"

  # The DNS port we want to bind must be free.
  if [ -n "${expected_gw:-}" ]; then
    if ss -tulnp 2>/dev/null | grep -E "(${expected_gw}|0\.0\.0\.0):53\b" \
                              | grep -v -E 'sing-box|mihomo' >/dev/null; then
      ss -tulnp 'sport = :53' >&2
      fail "$expected_gw:53 already bound by something other than the proxy engine"
    fi
    log "preflight: $expected_gw:53 free"
  fi

  # api.listen must be free. FeiLian's feilian-sentry typically owns
  # 127.0.0.1:8080, so configs default to 18080; double-check whatever the
  # operator picked.
  if [ -f "$SCRIPT_DIR/gateway.yaml" ]; then
    api_addr=$(grep -E '^\s*listen:' "$SCRIPT_DIR/gateway.yaml" | head -1 | awk -F'"' '{print $2}')
    if [ -n "$api_addr" ]; then
      api_port=${api_addr##*:}
      if ss -tulnp 2>/dev/null | grep -E ":${api_port}\b" | grep -v leap-gateway >/dev/null; then
        ss -tulnp "sport = :${api_port}" >&2
        fail "api.listen ${api_addr} already bound by another process — pick a free port in gateway.yaml"
      fi
      log "preflight: api.listen $api_addr free"
    fi
  fi

  # Cannot collide with FeiLian's table ip nat.
  if nft list table ip nat >/dev/null 2>&1 \
     && ! nft list table ip nat | grep -q FEILIAN_; then
    log "preflight: WARNING — table ip nat exists without FEILIAN_ chains. Is this really a FeiLian node?"
  fi

  log "preflight: all good"
}

# ---------------------------------------------------------------------------
# 2. Install proxy-engine binary (sing-box and/or mihomo)
#
# Both binaries can coexist on disk. Only the active one (per ENGINE)
# is installed when its package is missing; the other is best-effort.
# Service unit selection at start_services time decides which is enabled.

install_singbox() {
  if [ -x "$SCRIPT_DIR/sing-box" ]; then
    install -m 0755 "$SCRIPT_DIR/sing-box" /usr/local/bin/sing-box
    log "sing-box installed from $SCRIPT_DIR/sing-box"
  elif [ -x /usr/local/bin/sing-box ]; then
    log "sing-box already installed at /usr/local/bin/sing-box"
  elif [ "$ENGINE" = "sing-box" ]; then
    log "downloading sing-box ${SINGBOX_VERSION} (set SINGBOX_VERSION to override)"
    arch=$(uname -m)
    case "$arch" in
      x86_64)  go_arch=amd64 ;;
      aarch64) go_arch=arm64 ;;
      *) fail "unsupported arch: $arch" ;;
    esac
    tmp=$(mktemp -d)
    pkg="sing-box-${SINGBOX_VERSION}-linux-${go_arch}"
    url="https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/${pkg}.tar.gz"
    log "fetching $url"
    curl -fsSL "$url" -o "$tmp/sb.tgz" \
      || fail "could not download sing-box; place a 1.10.x binary at $SCRIPT_DIR/sing-box and rerun"
    tar -xzf "$tmp/sb.tgz" -C "$tmp"
    install -m 0755 "$tmp/$pkg/sing-box" /usr/local/bin/sing-box
    rm -rf "$tmp"
  else
    log "sing-box not present; engine=$ENGINE so we don't need it now"
    return
  fi
  v=$(/usr/local/bin/sing-box version | head -1)
  case "$v" in
    *"version 1.10."*) log "sing-box: $v" ;;
    *) [ "$ENGINE" = "sing-box" ] && fail "sing-box version $v is not 1.10.x — set SINGBOX_VERSION=1.10.7 and rerun" || log "(non-active) sing-box: $v" ;;
  esac
}

install_mihomo() {
  if [ -x "$SCRIPT_DIR/mihomo" ]; then
    install -m 0755 "$SCRIPT_DIR/mihomo" /usr/local/bin/mihomo
    log "mihomo installed from $SCRIPT_DIR/mihomo"
  elif [ -x /usr/local/bin/mihomo ]; then
    log "mihomo already installed at /usr/local/bin/mihomo"
  elif [ "$ENGINE" = "mihomo" ]; then
    log "downloading mihomo ${MIHOMO_VERSION} compatible build (set MIHOMO_VERSION to override)"
    arch=$(uname -m)
    case "$arch" in
      x86_64)  go_arch=amd64 ;;
      aarch64) go_arch=arm64 ;;
      *) fail "unsupported arch: $arch" ;;
    esac
    # Use the "compatible" variant — works on CPUs without AMD64-v3 (avx2),
    # which several FeiLian forwarding-node hardware classes lack.
    pkg="mihomo-linux-${go_arch}-compatible-v${MIHOMO_VERSION}"
    url="https://github.com/MetaCubeX/mihomo/releases/download/v${MIHOMO_VERSION}/${pkg}.gz"
    tmp=$(mktemp -d)
    log "fetching $url"
    curl -fsSL "$url" -o "$tmp/m.gz" \
      || fail "could not download mihomo; place a binary at $SCRIPT_DIR/mihomo and rerun"
    gunzip "$tmp/m.gz"
    install -m 0755 "$tmp/m" /usr/local/bin/mihomo
    rm -rf "$tmp"
  else
    log "mihomo not present; engine=$ENGINE so we don't need it now"
    return
  fi
  v=$(/usr/local/bin/mihomo -v 2>&1 | head -1)
  log "mihomo: $v"
}

# ---------------------------------------------------------------------------
# 3. Install leap-gateway binary + config

install_leap() {
  [ -x "$SCRIPT_DIR/leap-gateway" ] \
    || fail "missing leap-gateway binary in $SCRIPT_DIR (build with GOOS=linux go build ./cmd/gateway)"
  install -m 0755 "$SCRIPT_DIR/leap-gateway" /usr/local/bin/leap-gateway
  log "leap-gateway installed"

  # Create dirs for BOTH engines so a future engine swap doesn't need a
  # second install.sh pass. Engine-irrelevant dirs grouped first.
  install -d -m 0755 /etc/leap /var/lib/leap
  install -d -m 0755 /etc/leap/singbox /etc/leap/singbox/rule-sets
  install -d -m 0755 /var/lib/leap/mihomo /var/lib/leap/mihomo/rule-sets

  if [ -f "$SCRIPT_DIR/gateway.yaml" ]; then
    install -m 0640 "$SCRIPT_DIR/gateway.yaml" /etc/leap/gateway.yaml
    log "gateway.yaml installed (edit /etc/leap/gateway.yaml + restart leap-gateway to update)"
  elif [ ! -f /etc/leap/gateway.yaml ]; then
    fail "no gateway.yaml provided in $SCRIPT_DIR and none exists at /etc/leap/gateway.yaml"
  else
    log "keeping existing /etc/leap/gateway.yaml (no gateway.yaml in $SCRIPT_DIR)"
  fi

  # rule-sets — baked into the tarball by stage.sh. The active engine reads
  # from its own directory:
  #   sing-box → /etc/leap/singbox/rule-sets/<tag>.srs
  #   mihomo   → /var/lib/leap/mihomo/rule-sets/<tag>.mrs
  # Install whichever is present in $SCRIPT_DIR/rule-sets/.
  if [ -d "$SCRIPT_DIR/rule-sets" ]; then
    if compgen -G "$SCRIPT_DIR/rule-sets/*.srs" >/dev/null; then
      install -m 0644 "$SCRIPT_DIR"/rule-sets/*.srs /etc/leap/singbox/rule-sets/
      n=$(find "$SCRIPT_DIR/rule-sets" -name '*.srs' | wc -l | tr -d ' ')
      log "rule-sets installed ($n .srs in /etc/leap/singbox/rule-sets/)"
    fi
    if compgen -G "$SCRIPT_DIR/rule-sets/*.mrs" >/dev/null; then
      install -m 0644 "$SCRIPT_DIR"/rule-sets/*.mrs /var/lib/leap/mihomo/rule-sets/
      n=$(find "$SCRIPT_DIR/rule-sets" -name '*.mrs' | wc -l | tr -d ' ')
      log "rule-sets installed ($n .mrs in /var/lib/leap/mihomo/rule-sets/)"
    fi
  fi

  # mihomo's geoip.metadb (used by mihomo's dns fallback-filter geoip:CN
  # check). If staged, drop in the working dir; otherwise leave for mihomo
  # to download on first start (works only when leap-gateway has already
  # routed mihomo's HTTP fetches through itself, chicken-and-egg).
  if [ -f "$SCRIPT_DIR/geoip.metadb" ]; then
    install -m 0644 "$SCRIPT_DIR/geoip.metadb" /var/lib/leap/mihomo/geoip.metadb
    log "geoip.metadb installed for mihomo"
  fi

  # Engine sanity: the active engine must have its required deliverables.
  case "$ENGINE" in
    sing-box)
      compgen -G "/etc/leap/singbox/rule-sets/*.srs" >/dev/null \
        || fail "engine=sing-box but no .srs files in /etc/leap/singbox/rule-sets/ — re-run scripts/stage.sh"
      ;;
    mihomo)
      compgen -G "/var/lib/leap/mihomo/rule-sets/*.mrs" >/dev/null \
        || fail "engine=mihomo but no .mrs files in /var/lib/leap/mihomo/rule-sets/ — re-run scripts/stage.sh"
      [ -f /var/lib/leap/mihomo/geoip.metadb ] \
        || log "WARNING: mihomo geoip.metadb missing; mihomo will fail to start until it downloads one"
      ;;
  esac
}

# ---------------------------------------------------------------------------
# 4. Render nft + install policy-routing helper

install_nft() {
  client_subnet=$(grep -E '^\s*client_subnet:' /etc/leap/gateway.yaml | awk -F'"' '{print $2}')
  [ -n "$client_subnet" ] || fail "node.client_subnet not set in /etc/leap/gateway.yaml"
  api_listen=$(grep -E '^\s*listen:' /etc/leap/gateway.yaml | head -1 | awk -F'"' '{print $2}')
  api_port=${api_listen##*:}
  [ -n "$api_port" ] || api_port=18080
  sed -e "s|@CLIENT_SUBNET@|$client_subnet|g" \
      -e "s|@API_PORT@|$api_port|g" \
      "$SCRIPT_DIR/nft.conf.tmpl" \
    > /etc/leap/nft.conf
  log "nft.conf rendered (client_subnet=$client_subnet, api_port=$api_port)"

  install -m 0755 "$SCRIPT_DIR/iproute.sh" /etc/leap/iproute.sh
  log "iproute.sh installed"
}

# ---------------------------------------------------------------------------
# 5. Install systemd units + logrotate, enable, start

install_units() {
  # Install ALL data-plane unit files; only the active engine's gets
  # enabled at start_services. Lets cutover happen via gateway.yaml edit
  # + install.sh re-run, no manual cp+sed.
  for unit in leap-singbox.service leap-mihomo.service leap-gateway.service leap-nft.service; do
    [ -f "$SCRIPT_DIR/$unit" ] || continue
    install -m 0644 "$SCRIPT_DIR/$unit" "/etc/systemd/system/$unit"
  done
  install -m 0644 "$SCRIPT_DIR/logrotate-leap.conf" /etc/logrotate.d/leap
  systemctl daemon-reload
  log "systemd units installed"
}

# 6. Pre-render the data-plane config so the active engine's first start
# finds a valid file. Without this, sing-box's check (ExecStartPre) /
# mihomo's parse fails on first boot and the unit fail-restarts for ~5s
# until leap-gateway has had a chance to write the bootstrap config —
# harmless but noisy in logs.
prerender_config() {
  /usr/local/bin/leap-gateway --config /etc/leap/gateway.yaml --render-once \
    || fail "leap-gateway --render-once failed; check gateway.yaml"
  case "$ENGINE" in
    sing-box) log "bootstrap sing-box config rendered at /etc/leap/singbox/config.json" ;;
    mihomo)   log "bootstrap mihomo config rendered at /var/lib/leap/mihomo/config.yaml" ;;
  esac
}

start_services() {
  # Two engine units exist; only one is enabled per ENGINE. Re-running this
  # function with a different ENGINE value (operator edited gateway.yaml +
  # ran install.sh again) cleanly switches: stop+disable inactive, start
  # active, restart leap-nft so its PartOf attachment recreates utun-leap
  # routes against the new engine.
  local active inactive
  case "$ENGINE" in
    sing-box) active=leap-singbox.service; inactive=leap-mihomo.service ;;
    mihomo)   active=leap-mihomo.service;  inactive=leap-singbox.service ;;
  esac

  if systemctl is-enabled --quiet "$inactive" 2>/dev/null; then
    log "switching engine: stopping + disabling $inactive"
    systemctl disable --now "$inactive" || true
  fi

  log "enabling + starting $active"
  systemctl enable --now "$active"
  systemctl enable --now leap-gateway.service

  # leap-nft must come up AFTER active engine creates utun-leap (its
  # ExecStartPre waits up to 60s for that), and restart whenever the engine
  # restarts (PartOf chain). Always (re)start it last.
  systemctl enable leap-nft.service
  systemctl restart leap-nft.service

  systemctl --no-pager status "$active" leap-gateway.service leap-nft.service \
    | head -30 || true
}

# ---------------------------------------------------------------------------
main() {
  if [ "$SKIP_PRECHECK" = "0" ]; then
    precheck
  else
    log "SKIP_PRECHECK=1 — skipping preflight"
  fi
  install_singbox
  install_mihomo
  install_leap
  install_nft
  install_units
  prerender_config
  start_services
  log "DONE — verify with 'systemctl status leap-*' and 'journalctl -fu leap-${ENGINE}'"
}

main "$@"
