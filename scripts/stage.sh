#!/usr/bin/env bash
# scripts/stage.sh — produce a deployable tarball for a FeiLian forwarding
# node sidecar install.
#
# Outputs $OUT_DIR/leap-stage.tgz containing:
#   leap-gateway          (linux/amd64 binary built from this checkout)
#   sing-box              (1.10.7 linux/amd64, downloaded if not cached)
#   gateway.yaml          (operator-supplied; defaults to ./gateway.yaml)
#   <deploy/node/* files> (install.sh, *.service, nft.conf.tmpl, iproute.sh, logrotate-leap.conf)
#
# Usage on operator workstation:
#   cp configs/gateway.example.yaml gateway.yaml   # then edit subscription URL + node values
#   scripts/stage.sh
#   scp build/leap-stage.tgz dianwei@<node>:/tmp/
#   ssh dianwei@<node> 'cd /tmp && tar -xzf leap-stage.tgz && sudo bash leap-stage/install.sh'
#
# Env overrides:
#   GATEWAY_YAML       path to gateway.yaml (default: ./gateway.yaml)
#   SINGBOX_VERSION    sing-box version (default: 1.10.7)
#   SINGBOX_CACHE      cache dir for downloaded sing-box (default: ./.cache)
#   OUT_DIR            output directory (default: ./build)
#   GOARCH             target arch (default: amd64; set arm64 for ARM nodes)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_YAML="${GATEWAY_YAML:-./gateway.yaml}"
SINGBOX_VERSION="${SINGBOX_VERSION:-1.10.7}"
SINGBOX_CACHE="${SINGBOX_CACHE:-./.cache}"
OUT_DIR="${OUT_DIR:-./build}"
GOARCH="${GOARCH:-amd64}"

log() { printf '[stage] %s\n' "$*"; }
fail() { printf '[stage][FAIL] %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
[ -f "$GATEWAY_YAML" ] || fail "gateway.yaml not found at $GATEWAY_YAML — copy configs/gateway.example.yaml and fill in subscription URL + node values"

mkdir -p "$OUT_DIR" "$SINGBOX_CACHE"
STAGE_DIR="$OUT_DIR/leap-stage"
rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR"

# 1. Build leap-gateway for the target platform.
log "building leap-gateway (linux/$GOARCH)"
GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go build -ldflags='-s -w' \
  -o "$STAGE_DIR/leap-gateway" ./cmd/gateway
ls -lh "$STAGE_DIR/leap-gateway" | awk '{print "[stage] leap-gateway", $5}'

# 2. Fetch sing-box 1.10.x; cache so repeat runs are offline.
SB_PKG="sing-box-${SINGBOX_VERSION}-linux-${GOARCH}"
SB_TGZ="$SINGBOX_CACHE/${SB_PKG}.tar.gz"
if [ ! -f "$SB_TGZ" ]; then
  url="https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/${SB_PKG}.tar.gz"
  log "downloading $url"
  curl -fsSL "$url" -o "$SB_TGZ" || fail "could not download sing-box"
fi
tmp=$(mktemp -d)
tar -xzf "$SB_TGZ" -C "$tmp"
cp "$tmp/$SB_PKG/sing-box" "$STAGE_DIR/sing-box"
chmod 0755 "$STAGE_DIR/sing-box"
rm -rf "$tmp"
ls -lh "$STAGE_DIR/sing-box" | awk '{print "[stage] sing-box", $5}'

# 3. Operator gateway.yaml.
cp "$GATEWAY_YAML" "$STAGE_DIR/gateway.yaml"
log "gateway.yaml copied from $GATEWAY_YAML"

# 4. deploy/node/* — install.sh, *.service, nft.conf.tmpl, iproute.sh, logrotate-leap.conf.
cp deploy/node/install.sh \
   deploy/node/iproute.sh \
   deploy/node/nft.conf.tmpl \
   deploy/node/logrotate-leap.conf \
   deploy/node/leap-singbox.service \
   deploy/node/leap-gateway.service \
   deploy/node/leap-nft.service \
   "$STAGE_DIR/"
chmod 0755 "$STAGE_DIR/install.sh" "$STAGE_DIR/iproute.sh"
log "deploy/node/* files staged"

# 5. Tarball.
TARBALL="$OUT_DIR/leap-stage.tgz"
tar -czf "$TARBALL" -C "$OUT_DIR" leap-stage
log "wrote $TARBALL ($(du -h "$TARBALL" | cut -f1))"

cat <<EOF
[stage] DONE.

Next steps:
  scp $TARBALL dianwei@<node>:/tmp/
  ssh dianwei@<node>
  $ cd /tmp && tar -xzf leap-stage.tgz
  $ sudo bash leap-stage/install.sh
EOF
