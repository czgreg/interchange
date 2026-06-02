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

# 5. Rule-set .srs blobs. Pre-fetched into the tarball so sing-box can load
#    geosite-cn / geoip-cn (route infra) and any tags already selected in
#    gateway.yaml.whitelist.{geosites,geoips} as type=local at startup —
#    sidesteps the GFW path AND the startup race where parallel remote
#    downloads compete with urltest's first measurement.
#
#    Catalog tags NOT referenced by gateway.yaml are NOT pre-fetched; the
#    runtime API (PUT /api/whitelist) downloads them on-demand from the
#    embedded catalog when an operator selects them.
#
#    Per-URL cache in .cache/rule-sets/. Delete that dir to force re-fetch.
RULESETS_CACHE="$SINGBOX_CACHE/rule-sets"
mkdir -p "$RULESETS_CACHE" "$STAGE_DIR/rule-sets"

# Always pre-fetch the two infra rule-sets used by the route classifier.
RULESET_URLS=(
  "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs"
  "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs"
)

# Tags requested by gateway.yaml as full URLs — legacy form, kept for yaml
# that still has explicit url: fields under singbox.route.geosite_url etc.
while IFS= read -r u; do
  RULESET_URLS+=("$u")
done < <(grep -oE 'https://[^"[:space:]]+\.srs' "$GATEWAY_YAML" | sort -u)

# Tags from gateway.yaml's whitelist (post-migration: geosites: [tag, ...]
# and geoips: [tag, ...] — bare strings, no urls). Pull both and let the
# prefix branch decide the upstream.
WL_TAGS=()
while IFS= read -r tag; do
  WL_TAGS+=("$tag")
done < <(awk '
  /^[[:space:]]*geosites:/  { mode="geo";   next }
  /^[[:space:]]*geoips:/    { mode="geo";   next }
  /^[a-zA-Z]/                { mode=""; next }
  mode=="geo" && /^[[:space:]]*-[[:space:]]/ {
    sub(/^[[:space:]]*-[[:space:]]*/, "")
    sub(/[[:space:]]*#.*/, "")
    gsub(/"|'\''/, "")
    if ($0 ~ /^(geosite|geoip)-/) print $0
  }
' "$GATEWAY_YAML")

# Resolve each WL tag to a URL by prefix.
for tag in "${WL_TAGS[@]}"; do
  case "$tag" in
    geosite-*) RULESET_URLS+=("https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/${tag}.srs") ;;
    geoip-*)   RULESET_URLS+=("https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/${tag}.srs")   ;;
    *)         log "  skip unknown tag prefix: $tag" ;;
  esac
done

# Dedupe (bash 3.2 has no associative arrays — use a sentinel string).
DEDUPED_URLS=()
SEEN=$'\n'
for u in "${RULESET_URLS[@]}"; do
  case "$SEEN" in
    *$'\n'"$u"$'\n'*) ;;
    *)
      DEDUPED_URLS+=("$u")
      SEEN="$SEEN$u"$'\n'
      ;;
  esac
done

log "fetching ${#DEDUPED_URLS[@]} rule-set .srs files (cn infra + gateway.yaml whitelist)"
fetched=0
skipped=0
for url in "${DEDUPED_URLS[@]}"; do
  fname="$(basename "$url")"
  cached="$RULESETS_CACHE/$fname"
  if [ ! -s "$cached" ]; then
    if ! curl -fsSL --retry 2 --max-time 30 "$url" -o "$cached.tmp"; then
      log "  skip $fname (fetch failed)"
      rm -f "$cached.tmp"
      skipped=$((skipped+1))
      continue
    fi
    mv "$cached.tmp" "$cached"
  fi
  cp "$cached" "$STAGE_DIR/rule-sets/$fname"
  fetched=$((fetched+1))
done
log "rule-sets staged: $fetched ok, $skipped skipped ($(du -sh "$STAGE_DIR/rule-sets" | cut -f1))"

# 6. Tarball.
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
