#!/bin/bash
# leap-gateway sidecar installer for FeiLian forwarding nodes.
#
# Run as root on the FeiLian forwarding node (e.g. dianwei@192.168.70.92 with sudo).
# Idempotent — safe to re-run after edits.
#
# Inputs (must be present in working directory or fetched):
#   - leap-gateway       Linux binary (built with `GOOS=linux go build ./cmd/gateway`)
#   - mihomo             1.19.x binary (the data plane)
#   - sing-box           1.10.x binary (CLI helper for whitelistexpand
#                        rule-set decompile — NOT run as a service)
#   - geoip.metadb       MaxMind DB for mihomo's fakeip mode
#   - gateway.yaml       Filled-in config (subscription URL + node values)
#
# Companion files (same directory as this script):
#   leap-mihomo.service / leap-gateway.service / leap-nft.service /
#   nft.conf.tmpl / iproute.sh / logrotate-leap.conf
#
# Engine is mihomo. The legacy sing-box-as-data-plane path was removed
# in 2026-06; sing-box is still installed as a CLI helper because
# whitelistexpand shells out to `sing-box rule-set decompile` for geoip
# expansion (mihomo lacks an equivalent command).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MIHOMO_VERSION="${MIHOMO_VERSION:-1.19.26}"
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
  # Note: precheck runs BEFORE leap-gateway is installed, so we can't use
  # --print-env here. The sed extractor accepts both quoted ("10.8.13.1")
  # and unquoted (10.8.13.1) yaml values; trims trailing comments.
  if [ -f "$SCRIPT_DIR/gateway.yaml" ]; then
    expected_gw=$(sed -nE 's/^[[:space:]]*tun0_gateway_ip:[[:space:]]*"?([^"#[:space:]]+)"?.*/\1/p' "$SCRIPT_DIR/gateway.yaml")
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
                              | grep -v -E 'mihomo' >/dev/null; then
      ss -tulnp 'sport = :53' >&2
      fail "$expected_gw:53 already bound by something other than mihomo"
    fi
    log "preflight: $expected_gw:53 free"
  fi

  # api.listen must be free. FeiLian's feilian-sentry typically owns
  # 127.0.0.1:8080, so configs default to 18080; double-check whatever the
  # operator picked. Same sed pattern as tun0_gateway_ip — accepts quoted
  # or unquoted yaml.
  if [ -f "$SCRIPT_DIR/gateway.yaml" ]; then
    api_addr=$(sed -nE 's/^[[:space:]]*listen:[[:space:]]*"?([^"#[:space:]]+)"?.*/\1/p' "$SCRIPT_DIR/gateway.yaml" | head -1)
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
# 2. Install mihomo data-plane binary

install_mihomo() {
  if [ -x "$SCRIPT_DIR/mihomo" ]; then
    install -m 0755 "$SCRIPT_DIR/mihomo" /usr/local/bin/mihomo
    log "mihomo installed from $SCRIPT_DIR/mihomo"
  elif [ -x /usr/local/bin/mihomo ]; then
    log "mihomo already installed at /usr/local/bin/mihomo"
  else
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
  fi
  # Capture full output then take first line in shell — avoids SIGPIPE
  # from `head -1` closing the pipe early under `set -o pipefail` (mihomo
  # -v emits 2 lines, the second triggers SIGPIPE → pipefail propagates
  # exit 141 and the install aborts).
  v=$(/usr/local/bin/mihomo -v 2>&1)
  v=${v%%$'\n'*}
  log "mihomo: $v"
}

# ---------------------------------------------------------------------------
# 3. Install sing-box CLI binary (helper, NOT a service).
# Required because whitelistexpand shells out to `sing-box rule-set
# decompile` for geoip expansion — mihomo doesn't have an equivalent
# decompile command.

install_singbox_cli() {
  if [ -x "$SCRIPT_DIR/sing-box" ]; then
    install -m 0755 "$SCRIPT_DIR/sing-box" /usr/local/bin/sing-box
    log "sing-box CLI installed from $SCRIPT_DIR/sing-box"
  elif [ -x /usr/local/bin/sing-box ]; then
    log "sing-box CLI already installed at /usr/local/bin/sing-box"
  else
    log "downloading sing-box CLI ${SINGBOX_VERSION}"
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
      || fail "could not download sing-box CLI; place a binary at $SCRIPT_DIR/sing-box and rerun"
    tar -xzf "$tmp/sb.tgz" -C "$tmp"
    install -m 0755 "$tmp/$pkg/sing-box" /usr/local/bin/sing-box
    rm -rf "$tmp"
  fi
  # Same SIGPIPE-safe pattern as mihomo above (sing-box version also
  # emits multiple lines).
  v=$(/usr/local/bin/sing-box version 2>&1)
  v=${v%%$'\n'*}
  log "sing-box CLI: $v"
}

# ---------------------------------------------------------------------------
# 4. Install leap-gateway binary + config

install_leap() {
  [ -x "$SCRIPT_DIR/leap-gateway" ] \
    || fail "missing leap-gateway binary in $SCRIPT_DIR (build with GOOS=linux go build ./cmd/gateway)"
  install -m 0755 "$SCRIPT_DIR/leap-gateway" /usr/local/bin/leap-gateway
  log "leap-gateway installed"

  # Working dirs.
  install -d -m 0755 /etc/leap /var/lib/leap
  install -d -m 0755 /var/lib/leap/mihomo /var/lib/leap/mihomo/rule-sets

  if [ -f "$SCRIPT_DIR/gateway.yaml" ]; then
    # Auto-backup whatever is currently at /etc/leap/gateway.yaml before
    # overwriting. Cheap insurance — the staged yaml may have schema /
    # field drift from the on-node copy (saw this on 92's 2026-06-07
    # mihomo cutover), and operators were doing manual `sudo cp` for
    # rollback. Now it's automatic. Backups are 0640 root:root, never
    # pruned automatically — operators clean up when they need the disk.
    if [ -f /etc/leap/gateway.yaml ]; then
      bak=/etc/leap/gateway.yaml.bak.$(date -u +%Y%m%d-%H%M%S)
      install -m 0640 /etc/leap/gateway.yaml "$bak"
      log "backed up existing gateway.yaml → $bak"
    fi
    install -m 0640 "$SCRIPT_DIR/gateway.yaml" /etc/leap/gateway.yaml
    log "gateway.yaml installed (edit /etc/leap/gateway.yaml + restart leap-gateway to update)"
  elif [ ! -f /etc/leap/gateway.yaml ]; then
    fail "no gateway.yaml provided in $SCRIPT_DIR and none exists at /etc/leap/gateway.yaml"
  else
    log "keeping existing /etc/leap/gateway.yaml (no gateway.yaml in $SCRIPT_DIR)"
  fi

  # Generate /etc/leap/env — runtime variables sourced by iproute.sh so it
  # can set up TPROXY rules with the correct subnet and port without needing
  # to parse YAML itself.
  #
  # Use `leap-gateway --print-env` instead of grep/awk: YAML parsing in shell
  # is fragile (indent, quoting, comments all affect text matching). The
  # binary parses with the same logic as the runtime, so there are no
  # mismatches. install.sh runs AFTER the binary is installed (step 4), so
  # the binary is available.
  if ! /usr/local/bin/leap-gateway --config /etc/leap/gateway.yaml --print-env > /etc/leap/env 2>&1; then
    fail "leap-gateway --print-env failed — check gateway.yaml"
  fi
  chmod 0644 /etc/leap/env
  # shellcheck disable=SC1091
  source /etc/leap/env
  log "env file written: subnet=${LEAP_CLIENT_SUBNET:-?} tproxy_port=${LEAP_TPROXY_PORT:-0}"

  # rule-sets — baked into the tarball by stage.sh as .mrs files.
  if [ -d "$SCRIPT_DIR/rule-sets" ] && compgen -G "$SCRIPT_DIR/rule-sets/*.mrs" >/dev/null; then
    install -m 0644 "$SCRIPT_DIR"/rule-sets/*.mrs /var/lib/leap/mihomo/rule-sets/
    n=$(find "$SCRIPT_DIR/rule-sets" -name '*.mrs' | wc -l | tr -d ' ')
    log "rule-sets installed ($n .mrs in /var/lib/leap/mihomo/rule-sets/)"
  fi

  # mihomo's geoip.metadb (used by mihomo's dns fallback-filter geoip:CN
  # check). If staged, drop in the working dir; otherwise leave for mihomo
  # to download on first start (works only when leap-gateway has already
  # routed mihomo's HTTP fetches through itself, chicken-and-egg).
  if [ -f "$SCRIPT_DIR/geoip.metadb" ]; then
    install -m 0644 "$SCRIPT_DIR/geoip.metadb" /var/lib/leap/mihomo/geoip.metadb
    log "geoip.metadb installed for mihomo"
  fi

  # Sanity: rule-sets must be present.
  compgen -G "/var/lib/leap/mihomo/rule-sets/*.mrs" >/dev/null \
    || fail "no .mrs files in /var/lib/leap/mihomo/rule-sets/ — re-run scripts/stage.sh"
  [ -f /var/lib/leap/mihomo/geoip.metadb ] \
    || log "WARNING: mihomo geoip.metadb missing; mihomo will fail to start until it downloads one"
}

# ---------------------------------------------------------------------------
# 5. Render nft + install policy-routing helper

install_nft() {
  # Source the env file written by install_leap (line 205, via --print-env).
  # YAML-aware values from the binary's own parser; never grep YAML again
  # for the same fields — install_leap's env and install_nft's nft.conf
  # MUST agree, and re-parsing risks divergence on indent/quote edge cases.
  # shellcheck disable=SC1091
  source /etc/leap/env
  [ -n "${LEAP_CLIENT_SUBNET:-}" ] || fail "LEAP_CLIENT_SUBNET missing in /etc/leap/env (re-run install_leap)"
  [ -n "${LEAP_API_PORT:-}" ]      || fail "LEAP_API_PORT missing in /etc/leap/env (re-run install_leap)"
  sed -e "s|@CLIENT_SUBNET@|$LEAP_CLIENT_SUBNET|g" \
      -e "s|@API_PORT@|$LEAP_API_PORT|g" \
      "$SCRIPT_DIR/nft.conf.tmpl" \
    > /etc/leap/nft.conf
  log "nft.conf rendered (client_subnet=$LEAP_CLIENT_SUBNET, api_port=$LEAP_API_PORT)"

  install -m 0755 "$SCRIPT_DIR/iproute.sh" /etc/leap/iproute.sh
  log "iproute.sh installed"
}

# ---------------------------------------------------------------------------
# 6. Install systemd units + logrotate, enable, start

install_units() {
  # leap-mihomo / leap-gateway / leap-nft. Old leap-singbox.service was
  # removed; if it exists from a pre-2026-06 install, disable + remove it.
  if systemctl is-enabled --quiet leap-singbox.service 2>/dev/null; then
    log "removing legacy leap-singbox.service (sing-box engine retired)"
    systemctl disable --now leap-singbox.service || true
    rm -f /etc/systemd/system/leap-singbox.service
  fi
  for unit in leap-mihomo.service leap-gateway.service leap-nft.service; do
    [ -f "$SCRIPT_DIR/$unit" ] || continue
    install -m 0644 "$SCRIPT_DIR/$unit" "/etc/systemd/system/$unit"
  done
  install -m 0644 "$SCRIPT_DIR/logrotate-leap.conf" /etc/logrotate.d/leap
  systemctl daemon-reload
  log "systemd units installed"
}

# ---------------------------------------------------------------------------
# 7. Pre-render mihomo config so the first start finds a valid file.
# Without this, mihomo's parse fails on first boot and the unit fail-
# restarts for ~5s until leap-gateway has had a chance to write the
# bootstrap config — harmless but noisy in logs.

prerender_config() {
  /usr/local/bin/leap-gateway --config /etc/leap/gateway.yaml --render-once \
    || fail "leap-gateway --render-once failed; check gateway.yaml"
  log "bootstrap mihomo config rendered at /var/lib/leap/mihomo/config.yaml"
  # mihomo -t: validate the rendered YAML before accepting it.
  # Catches config bugs (invalid rule-provider paths, bad proxy syntax) that
  # render-once won't catch — render-once only verifies the renderer produces
  # valid Go structs, not that mihomo itself can parse the output.
  /usr/local/bin/mihomo -d /var/lib/leap/mihomo -t \
    || fail "mihomo -t failed on rendered config — check gateway.yaml and rule-sets"
  log "mihomo -t passed"
}

start_services() {
  log "enabling + starting leap-mihomo + leap-gateway"
  # Use enable + restart (not enable --now). `enable --now` is a no-op for
  # services already running — so a re-install with new binaries / new
  # gateway.yaml would silently keep the old processes alive. Explicit
  # restart ensures the units pick up whatever this run just installed.
  systemctl daemon-reload
  systemctl enable leap-mihomo.service leap-gateway.service leap-nft.service
  systemctl restart leap-mihomo.service
  # leap-nft has PartOf=leap-mihomo.service so it auto-restarts when
  # mihomo does, but be explicit in case the unit file dependency changed
  # this run (daemon-reload + restart picks up new After=/BindsTo=).
  systemctl restart leap-nft.service
  systemctl restart leap-gateway.service

  systemctl --no-pager status leap-mihomo.service leap-gateway.service leap-nft.service \
    | head -30 || true
}

# ---------------------------------------------------------------------------
main() {
  if [ "$SKIP_PRECHECK" = "0" ]; then
    precheck
  else
    log "SKIP_PRECHECK=1 — skipping preflight"
  fi
  install_mihomo
  install_singbox_cli
  install_leap
  install_nft
  install_units
  prerender_config
  start_services
  log "DONE — verify with 'systemctl status leap-*' and 'journalctl -fu leap-mihomo'"
}

main "$@"
