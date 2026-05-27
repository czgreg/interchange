#!/bin/bash
# leap-gateway sidecar installer for FeiLian forwarding nodes.
#
# Run as root on the FeiLian forwarding node (e.g. dianwei@192.168.70.92 with sudo).
# Idempotent — safe to re-run after edits.
#
# Inputs (must be present in working directory or fetched):
#   - leap-gateway       Linux binary (built with `GOOS=linux go build ./cmd/gateway`)
#   - sing-box           1.10.x binary (downloaded by --bootstrap-singbox if missing)
#   - gateway.yaml       Filled-in config (subscription URL + node.{client_subnet,tun0_gateway_ip,egress_iface})
#
# Companion files (same directory as this script):
#   leap-singbox.service / leap-gateway.service / leap-nft.service / nft.conf.tmpl /
#   iproute.sh / logrotate-leap.conf

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SINGBOX_VERSION="${SINGBOX_VERSION:-1.10.7}"
SKIP_PRECHECK="${SKIP_PRECHECK:-0}"

log()  { printf '[install] %s\n' "$*"; }
fail() { printf '[install][FAIL] %s\n' "$*" >&2; exit 1; }

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
                              | grep -v 'sing-box' >/dev/null; then
      ss -tulnp 'sport = :53' >&2
      fail "$expected_gw:53 already bound by something other than sing-box"
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
# 2. Install sing-box

install_singbox() {
  if [ -x "$SCRIPT_DIR/sing-box" ]; then
    install -m 0755 "$SCRIPT_DIR/sing-box" /usr/local/bin/sing-box
    log "sing-box installed from $SCRIPT_DIR/sing-box"
  elif command -v sing-box >/dev/null 2>&1 && [ "$(command -v sing-box)" = "/usr/local/bin/sing-box" ]; then
    log "sing-box already installed at /usr/local/bin/sing-box"
  else
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
  fi
  v=$(/usr/local/bin/sing-box version | head -1)
  case "$v" in
    *"version 1.10."*) log "sing-box: $v" ;;
    *) fail "sing-box version $v is not 1.10.x — set SINGBOX_VERSION=1.10.7 and rerun" ;;
  esac
}

# ---------------------------------------------------------------------------
# 3. Install leap-gateway binary + config

install_leap() {
  [ -x "$SCRIPT_DIR/leap-gateway" ] \
    || fail "missing leap-gateway binary in $SCRIPT_DIR (build with GOOS=linux go build ./cmd/gateway)"
  install -m 0755 "$SCRIPT_DIR/leap-gateway" /usr/local/bin/leap-gateway
  log "leap-gateway installed"

  install -d -m 0755 /etc/leap /etc/leap/singbox /var/lib/leap

  if [ -f "$SCRIPT_DIR/gateway.yaml" ]; then
    install -m 0640 "$SCRIPT_DIR/gateway.yaml" /etc/leap/gateway.yaml
    log "gateway.yaml installed (edit /etc/leap/gateway.yaml + restart leap-gateway to update)"
  elif [ ! -f /etc/leap/gateway.yaml ]; then
    fail "no gateway.yaml provided in $SCRIPT_DIR and none exists at /etc/leap/gateway.yaml"
  else
    log "keeping existing /etc/leap/gateway.yaml (no gateway.yaml in $SCRIPT_DIR)"
  fi
}

# ---------------------------------------------------------------------------
# 4. Render nft + install policy-routing helper

install_nft() {
  client_subnet=$(grep -E '^\s*client_subnet:' /etc/leap/gateway.yaml | awk -F'"' '{print $2}')
  [ -n "$client_subnet" ] || fail "node.client_subnet not set in /etc/leap/gateway.yaml"
  sed "s|@CLIENT_SUBNET@|$client_subnet|g" "$SCRIPT_DIR/nft.conf.tmpl" \
    > /etc/leap/nft.conf
  log "nft.conf rendered (client_subnet=$client_subnet)"

  install -m 0755 "$SCRIPT_DIR/iproute.sh" /etc/leap/iproute.sh
  log "iproute.sh installed"
}

# ---------------------------------------------------------------------------
# 5. Install systemd units + logrotate, enable, start

install_units() {
  for unit in leap-singbox.service leap-gateway.service leap-nft.service; do
    install -m 0644 "$SCRIPT_DIR/$unit" "/etc/systemd/system/$unit"
  done
  install -m 0644 "$SCRIPT_DIR/logrotate-leap.conf" /etc/logrotate.d/leap
  systemctl daemon-reload
  log "systemd units installed"
}

# 6. Pre-render the sing-box config so leap-singbox.service's first start
# finds a valid file. Without this, sing-box check (ExecStartPre) fails on
# first boot and the unit fail-restarts for ~5s until leap-gateway has had
# a chance to write the bootstrap config — harmless but noisy in logs.
prerender_config() {
  /usr/local/bin/leap-gateway --config /etc/leap/gateway.yaml --render-once \
    || fail "leap-gateway --render-once failed; check gateway.yaml"
  log "bootstrap singbox config rendered at /etc/leap/singbox/config.json"
}

start_services() {
  # Start order: leap-singbox -> leap-nft (waits for utun-leap) -> leap-gateway.
  # systemd handles ordering via After= / Requires=, but enabling all three is
  # explicit so they auto-start on reboot.
  systemctl enable --now leap-singbox.service leap-gateway.service leap-nft.service
  systemctl --no-pager status leap-singbox.service leap-gateway.service leap-nft.service \
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
  install_leap
  install_nft
  install_units
  prerender_config
  start_services
  log "DONE — verify with 'systemctl status leap-*' and 'journalctl -fu leap-singbox'"
}

main "$@"
