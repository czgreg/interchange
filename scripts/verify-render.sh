#!/usr/bin/env bash
# scripts/verify-render.sh — local schema gate for the sing-box renderer.
#
# Renders a fixture config + runs `sing-box check` against it. Catches the
# kind of bug that took down .89 last week (vmess grpc transport `path` vs
# `service_name`): the config rendered fine but sing-box rejected it on
# startup. We want that failure to surface in `make verify-render`, not
# 30s after deploy when leap-singbox enters a restart loop.
#
# Behavior:
#   - Reuses the cached sing-box binary in .cache/ when possible.
#   - Auto-downloads the host-platform sing-box 1.10.7 release otherwise.
#   - Runs `go test -run TestRenderedConfigPassesSingBoxCheck ./internal/singbox/`.
#
# Env:
#   SINGBOX_VERSION   pin (default: 1.10.7 — must match what stage.sh + nodes use)
#   SINGBOX_CACHE     binary cache dir (default: ./.cache)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

VERSION="${SINGBOX_VERSION:-1.10.7}"
CACHE="${SINGBOX_CACHE:-./.cache}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *)      echo "verify-render: unsupported host OS $(uname -s)" >&2; exit 2 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64)  arch=amd64 ;;
  *)             echo "verify-render: unsupported arch $(uname -m)" >&2; exit 2 ;;
esac

PKG="sing-box-${VERSION}-${os}-${arch}"
TGZ="${CACHE}/${PKG}.tar.gz"
BIN="${CACHE}/${PKG}/sing-box"

mkdir -p "$CACHE"
if [ ! -x "$BIN" ]; then
  if [ ! -f "$TGZ" ]; then
    url="https://github.com/SagerNet/sing-box/releases/download/v${VERSION}/${PKG}.tar.gz"
    echo "[verify-render] downloading $url"
    curl -fsSL "$url" -o "$TGZ"
  fi
  tar -xzf "$TGZ" -C "$CACHE"
fi

ver=$("$BIN" version | head -1)
echo "[verify-render] using $BIN ($ver)"

SING_BOX_BIN="$BIN" go test -count=1 -run TestRenderedConfigPassesSingBoxCheck ./internal/singbox/
