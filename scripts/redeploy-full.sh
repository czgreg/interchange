#!/usr/bin/env bash
# scripts/redeploy-full.sh — yaml-first full reinstall to a node.
#
# Pulls /etc/leap/gateway.yaml from <host> as the staging input (so we
# never accidentally clobber the on-node config with a stale local one),
# stages a fresh tarball, ships it, runs install.sh, verifies version +
# services. Intended for full-stack reinstalls (new service files, new
# install.sh, schema migration). For pure binary swaps use `make deploy-92`
# / `scripts/deploy.sh` — they're faster and don't touch the unit files.
#
# Usage:
#   scripts/redeploy-full.sh <host>            # e.g. 192.168.70.92
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

# ⛔ HARD RULE printed at the top of every sync — see CLAUDE.md.
# Subscriptions in the pulled yaml go back to the node verbatim. Do NOT
# add/remove/rename/reorder entries between pull and push. Each node's
# subscription set is operator-owned and intentionally different per node.
printf '\033[1;33m[redeploy] ⛔ subscriptions[] is operator-owned — NEVER edit between pull and push\033[0m\n'

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
# LEAP_TOKEN, when exported, authenticates against api.token. Unset → no
# header, correct for nodes that leave api.token empty.
# Auth header as a single optional arg rather than an array: under `set -u`,
# bash <4.4 (macOS's system /bin/bash is 3.2) treats "${AUTH[@]}" on an
# empty array as an unbound-variable error, not "expands to nothing" — hit
# in production on 2026-08-12, aborting verification after every install
# had already succeeded.
AUTHOPT=""
[ -n "${LEAP_TOKEN:-}" ] && AUTHOPT="Authorization: Bearer $LEAP_TOKEN"
for i in 1 2 3; do
  if [ -n "$AUTHOPT" ]; then
    RESP=$(curl -s --max-time 6 -H "$AUTHOPT" "http://$HOST:18080/api/proxies/active" 2>/dev/null || true)
  else
    RESP=$(curl -s --max-time 6 "http://$HOST:18080/api/proxies/active" 2>/dev/null || true)
  fi
  [ -n "$RESP" ] && break
  sleep 3
done
[ -n "$RESP" ] || fail "API on $HOST unreachable after install — check 'journalctl -u leap-gateway' on the node"

VER=$(printf '%s' "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['leap']['gateway_version'])" 2>/dev/null || echo UNKNOWN)
SVC=$(printf '%s' "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['leap']['services'])" 2>/dev/null || echo UNKNOWN)
ok "$HOST is on gateway_version=$VER  services=$SVC"

ok "redeploy-full done for $HOST. yaml backup on the node at /etc/leap/gateway.yaml.bak.<ts>"
