#!/usr/bin/env bash
# scripts/redeploy-full.sh — yaml-first full reinstall to a node.
#
# Pulls /etc/leap/gateway.yaml from <host> as the staging input (so we
# never accidentally clobber the on-node config with a stale local one),
# stages a fresh tarball, ships it, runs install.sh, verifies version +
# services. Intended for full-stack reinstalls (new service files, new
# install.sh, schema migration). For pure binary swaps use `make deploy-89`
# / `scripts/deploy.sh` — they're faster and don't touch the unit files.
#
# Usage:
#   scripts/redeploy-full.sh <host>            # e.g. 192.168.70.89
#
# Env:
#   REMOTE_USER       SSH user (default dianwei)
#   GOARCH            target arch (default amd64)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

HOST="${1:-}"
[ -n "$HOST" ] || { echo "usage: $0 <host>" >&2; exit 1; }

REMOTE_USER="${REMOTE_USER:-dianwei}"
GOARCH="${GOARCH:-amd64}"
TS=$(date -u +%Y%m%d-%H%M%S)
PULLED_YAML="/tmp/gw-${HOST}-${TS}.yaml"

log()  { printf '\033[1;34m[redeploy]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[redeploy]\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m[redeploy][FAIL]\033[0m %s\n' "$*" >&2; exit 1; }

SSH="ssh -o ConnectTimeout=10 -o StrictHostKeyChecking=no $REMOTE_USER@$HOST"
SCP="scp -O -o ConnectTimeout=10 -o StrictHostKeyChecking=no"

# Worktree-dirty warning: this runs through stage.sh which also warns,
# but better to surface it before the user has waited 30s for staging.
DIRTY=$(git -C "$REPO_ROOT" status --porcelain 2>/dev/null | head -1 || true)
if [ -n "$DIRTY" ]; then
  log "WARNING: worktree dirty — staged binary will be tagged -dirty."
  log "         commit first if you want a clean version stamp on the node."
fi

# 1. Pull on-node gateway.yaml. Uses NOPASSWD `bash` allowance to read
# a root-owned file (sudo cat is not on the NOPASSWD list).
log "pulling /etc/leap/gateway.yaml from $HOST → $PULLED_YAML"
$SSH "sudo -n bash -c 'cat /etc/leap/gateway.yaml'" > "$PULLED_YAML" \
  || fail "could not pull on-node gateway.yaml from $HOST (check NOPASSWD bash + sudo perms)"
[ -s "$PULLED_YAML" ] || fail "pulled gateway.yaml is empty — bailing"
ok "pulled $(wc -l < "$PULLED_YAML") lines from $HOST"

# 2. Stage tarball using the pulled yaml.
log "staging tarball with $PULLED_YAML"
GATEWAY_YAML="$PULLED_YAML" GOARCH="$GOARCH" "$REPO_ROOT/scripts/stage.sh" >/tmp/stage-$HOST-$TS.log 2>&1 \
  || { tail -20 /tmp/stage-$HOST-$TS.log >&2; fail "stage.sh failed (full log: /tmp/stage-$HOST-$TS.log)"; }
TARBALL="$REPO_ROOT/build/leap-stage.tgz"
[ -f "$TARBALL" ] || fail "expected $TARBALL but it's not there"
ok "staged $(du -h "$TARBALL" | cut -f1)"

# 3. Ship + install.
log "pushing tarball to $HOST"
$SCP "$TARBALL" "$REMOTE_USER@$HOST:/tmp/" >/dev/null

log "running install.sh on $HOST (output → /tmp/install-${HOST}-${TS}.log on local)"
$SSH "sudo -n bash -c 'cd /tmp && rm -rf leap-stage && tar --no-same-owner -xzf leap-stage.tgz 2>/dev/null; cd leap-stage && bash install.sh > /tmp/install-${TS}.log 2>&1; echo INSTALL_EXIT=\$? >> /tmp/install-${TS}.log; tail -50 /tmp/install-${TS}.log'" \
  | tee "/tmp/install-${HOST}-${TS}.log"

EXIT=$(grep '^INSTALL_EXIT=' "/tmp/install-${HOST}-${TS}.log" | tail -1 | cut -d= -f2)
[ "$EXIT" = "0" ] || fail "install.sh exited $EXIT on $HOST — see /tmp/install-${HOST}-${TS}.log"

# 4. Verify post-install. SSH may briefly drop during gateway restart;
# retry the API curl.
log "verifying $HOST API (may take a few seconds while services restart)"
sleep 6
for i in 1 2 3; do
  RESP=$(curl -s --max-time 6 "http://$HOST:18080/api/proxies/active" 2>/dev/null || true)
  [ -n "$RESP" ] && break
  sleep 3
done
[ -n "$RESP" ] || fail "API on $HOST unreachable after install — check 'journalctl -u leap-gateway' on the node"

VER=$(printf '%s' "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['leap']['gateway_version'])" 2>/dev/null || echo UNKNOWN)
SVC=$(printf '%s' "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['leap']['services'])" 2>/dev/null || echo UNKNOWN)
ok "$HOST is on gateway_version=$VER  services=$SVC"

ok "redeploy-full done for $HOST. yaml backup on the node at /etc/leap/gateway.yaml.bak.<ts>"
