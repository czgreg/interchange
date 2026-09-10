#!/usr/bin/env bash
# scripts/deploy.sh — build leap-gateway and deploy to one or more forwarding nodes.
#
# Usage:
#   scripts/deploy.sh <host> [<host> ...]          # deploy to given nodes
#   scripts/deploy.sh 192.168.70.92                # deploy to 92 (production)
#
# Prerequisites:
#   - SSH key auth set up (ssh-copy-id dianwei@<host>) — no password prompts.
#   - Gateway.yaml already on the node at /etc/leap/gateway.yaml.
#     This script only updates the binary; use --update-config to also push
#     a local gateway.yaml.
#
# Flags (all optional):
#   --config <file>   also push this gateway.yaml to /etc/leap/gateway.yaml
#                     (backs up the existing one first)
#   --skip-build      use the last build/leap-gateway binary (skip recompile)
#   --dry-run         show what would happen, don't actually deploy
#
# Environment:
#   GOARCH=arm64      cross-compile for ARM nodes (default amd64)
#   REMOTE_USER       SSH user (default dianwei)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GOARCH="${GOARCH:-amd64}"
REMOTE_USER="${REMOTE_USER:-dianwei}"
CONFIG_FILE=""
SKIP_BUILD=0
DRY_RUN=0
HOSTS=()

# Parse args
while [[ $# -gt 0 ]]; do
  case "$1" in
    --config) CONFIG_FILE="$2"; shift 2 ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -*) echo "Unknown flag: $1" >&2; exit 1 ;;
    *) HOSTS+=("$1"); shift ;;
  esac
done
[ ${#HOSTS[@]} -gt 0 ] || { echo "Usage: $0 [flags] <host> [<host> ...]" >&2; exit 1; }

log()  { printf '\033[1;34m[deploy]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[deploy]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[deploy][FAIL]\033[0m %s\n' "$*" >&2; exit 1; }
run()  { [ "$DRY_RUN" -eq 1 ] && { echo "  DRY-RUN: $*"; return 0; }; "$@"; }

# ---------------------------------------------------------------------------
# 1. Build
# ---------------------------------------------------------------------------
BIN="$REPO_ROOT/build/leap-gateway-linux-${GOARCH}"
mkdir -p "$REPO_ROOT/build"

if [ "$SKIP_BUILD" -eq 0 ]; then
  VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
  log "building leap-gateway (linux/$GOARCH, version=$VERSION)"
  run env GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go build \
    -ldflags="-s -w -X main.Version=$VERSION" \
    -o "$BIN" ./cmd/gateway
  ok "build ok — $(ls -lh "$BIN" | awk '{print $5}') — $VERSION"
else
  [ -f "$BIN" ] || fail "no pre-built binary at $BIN; re-run without --skip-build"
  log "using pre-built $BIN (--skip-build)"
fi

# ---------------------------------------------------------------------------
# 2. Deploy to each host
# ---------------------------------------------------------------------------
for HOST in "${HOSTS[@]}"; do
  log "deploying to $REMOTE_USER@$HOST ..."
  SSH="ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=no $REMOTE_USER@$HOST"
  SCP_TO="scp -O -o ConnectTimeout=10 -o StrictHostKeyChecking=no"

  # Connectivity check
  run $SSH "echo CONN_OK" > /dev/null \
    || fail "$HOST: SSH connection failed — have you run ssh-copy-id for this host?"

  # Optionally push gateway.yaml
  if [ -n "$CONFIG_FILE" ]; then
    [ -f "$CONFIG_FILE" ] || fail "config file not found: $CONFIG_FILE"
    # Validate config locally before sending
    log "  validating $CONFIG_FILE with local binary..."
    TMPDIR_VALIDATE=$(mktemp -d)
    cp "$CONFIG_FILE" "$TMPDIR_VALIDATE/gw.yaml"
    sed -i '' \
      "s|/var/lib/leap/mihomo/config.yaml|$TMPDIR_VALIDATE/config.yaml|;
       s|/var/lib/leap/mihomo/rule-sets|$TMPDIR_VALIDATE/rs|" \
      "$TMPDIR_VALIDATE/gw.yaml" 2>/dev/null || true
    mkdir -p "$TMPDIR_VALIDATE/rs"
    if ! go run ./cmd/gateway --config "$TMPDIR_VALIDATE/gw.yaml" --render-once > /dev/null 2>&1; then
      rm -rf "$TMPDIR_VALIDATE"
      fail "$HOST: config validation failed — fix $CONFIG_FILE before deploying"
    fi
    rm -rf "$TMPDIR_VALIDATE"
    ok "  config validation passed"

    log "  pushing gateway.yaml → /etc/leap/gateway.yaml"
    REMOTE_BAK="/etc/leap/gateway.yaml.bak.$(date +%Y%m%d-%H%M%S)"
    run $SSH "sudo bash -c 'cp /etc/leap/gateway.yaml $REMOTE_BAK 2>/dev/null || true'"
    run $SCP_TO "$CONFIG_FILE" "$REMOTE_USER@$HOST:/tmp/gateway.yaml.new"
    run $SSH "sudo bash -c 'install -m 0640 /tmp/gateway.yaml.new /etc/leap/gateway.yaml'"
    ok "  config pushed (backup: $REMOTE_BAK)"
  fi

  # Push binary
  log "  pushing binary → /tmp/leap-gateway.new"
  run $SCP_TO "$BIN" "$REMOTE_USER@$HOST:/tmp/leap-gateway.new"

  # Validate on node using --validate (temp dir, never writes production config)
  log "  validating with --validate on node ..."
  run $SSH "sudo bash -c '/tmp/leap-gateway.new --config /etc/leap/gateway.yaml --validate'" \
    || fail "$HOST: --validate failed on node — binary NOT installed"
  ok "  validation passed"

  # Atomic install + restart
  log "  installing + restarting ..."
  run $SSH "sudo bash -c 'install -m 0755 /tmp/leap-gateway.new /usr/local/bin/leap-gateway && systemctl restart leap-gateway'"

  # Verify
  log "  verifying ..."
  sleep 3
  # LEAP_TOKEN, when exported, authenticates against api.token. Unset →
  # no header, correct for nodes that leave api.token empty.
  #
  # Auth header as a single optional arg rather than an array: under `set -u`,
  # bash <4.4 (macOS's system /bin/bash is 3.2) treats "${AUTH[@]}" on an
  # EMPTY array as an unbound-variable error, not "expands to nothing". That
  # aborted verification after an install had already succeeded — the deploy
  # looked failed while production was actually fine. Same bug and same fix
  # as scripts/redeploy-full.sh:88-92; this copy was missed then, and bit on
  # 2026-09-09 deploying 557ba03 to 92.
  AUTHOPT=""
  [ -n "${LEAP_TOKEN:-}" ] && AUTHOPT="Authorization: Bearer $LEAP_TOKEN"
  if [ -n "$AUTHOPT" ]; then
    DEPLOYED_VER=$(curl -s -m6 -H "$AUTHOPT" "http://$HOST:18080/api/proxies/active" \
      | python3 -c "import sys,json;print(json.load(sys.stdin)['leap']['gateway_version'])" 2>/dev/null || echo "API_UNREACHABLE")
  else
    DEPLOYED_VER=$(curl -s -m6 "http://$HOST:18080/api/proxies/active" \
      | python3 -c "import sys,json;print(json.load(sys.stdin)['leap']['gateway_version'])" 2>/dev/null || echo "API_UNREACHABLE")
  fi
  if [ "$DEPLOYED_VER" = "API_UNREACHABLE" ]; then
    fail "$HOST: API unreachable after restart — check 'journalctl -u leap-gateway'"
  fi
  ok "  $HOST running version $DEPLOYED_VER"

  run $SSH "sudo bash -c 'rm -f /tmp/leap-gateway.new /tmp/gateway.yaml.new'"
done

ok "deploy complete: ${HOSTS[*]}"
