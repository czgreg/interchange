#!/usr/bin/env bash
# scripts/stage.sh — produce a deployable tarball for a FeiLian forwarding
# node sidecar install.
#
# Outputs $OUT_DIR/leap-stage.tgz containing:
#   leap-gateway          (linux/amd64 binary built from this checkout)
#   sing-box              (1.10.x linux/amd64; staged when ENGINE=sing-box or for fallback)
#   mihomo                (1.19.x linux/amd64; staged when ENGINE=mihomo)
#   geoip.metadb          (mihomo's MaxMind DB; needed when ENGINE=mihomo)
#   gateway.yaml          (operator-supplied; defaults to ./gateway.yaml)
#   <deploy/node/* files> (install.sh, *.service, nft.conf.tmpl, iproute.sh, logrotate-leap.conf)
#   rule-sets/*.srs       (when ENGINE=sing-box)
#   rule-sets/*.mrs       (when ENGINE=mihomo — same MetaCubeX repo, /meta/ branch)
#
# Engine is read from the staged gateway.yaml's `singbox.engine` field
# (default "sing-box"). To stage a mihomo-targeting tarball: set
# `singbox.engine: mihomo` in your gateway.yaml.
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
#   MIHOMO_VERSION     mihomo version (default: 1.19.26)
#   SINGBOX_CACHE      cache dir for downloaded engine binaries (default: ./.cache)
#   OUT_DIR            output directory (default: ./build)
#   GOARCH             target arch (default: amd64; set arm64 for ARM nodes)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_YAML="${GATEWAY_YAML:-./gateway.yaml}"
SINGBOX_VERSION="${SINGBOX_VERSION:-1.10.7}"
MIHOMO_VERSION="${MIHOMO_VERSION:-1.19.26}"
SINGBOX_CACHE="${SINGBOX_CACHE:-./.cache}"
OUT_DIR="${OUT_DIR:-./build}"
GOARCH="${GOARCH:-amd64}"

log() { printf '[stage] %s\n' "$*"; }
fail() { printf '[stage][FAIL] %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
[ -f "$GATEWAY_YAML" ] || fail "gateway.yaml not found at $GATEWAY_YAML — copy configs/gateway.example.yaml and fill in subscription URL + node values"

# Detect engine from gateway.yaml. Stage diff for sing-box vs mihomo:
#   binary: sing-box vs mihomo
#   rule-sets: .srs (MetaCubeX /sing/) vs .mrs (MetaCubeX /meta/)
#   extras: geoip.metadb only for mihomo
ENGINE=$(awk '/^[[:space:]]*engine:[[:space:]]/{
  gsub(/^[[:space:]]*engine:[[:space:]]*/, "")
  gsub(/[[:space:]"'\''#].*$/, "")
  print; exit
}' "$GATEWAY_YAML")
[ -n "$ENGINE" ] || ENGINE="sing-box"
case "$ENGINE" in
  sing-box|mihomo) log "engine: $ENGINE" ;;
  *) fail "invalid engine '$ENGINE' in $GATEWAY_YAML — must be sing-box | mihomo" ;;
esac

mkdir -p "$OUT_DIR" "$SINGBOX_CACHE"
STAGE_DIR="$OUT_DIR/leap-stage"
rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR"

# 1. Build leap-gateway for the target platform.
log "building leap-gateway (linux/$GOARCH)"
GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go build -ldflags='-s -w' \
  -o "$STAGE_DIR/leap-gateway" ./cmd/gateway
ls -lh "$STAGE_DIR/leap-gateway" | awk '{print "[stage] leap-gateway", $5}'

# 2. Fetch the active engine's binary; cache so repeat runs are offline.
#    Both engines are stagable so a future engine swap on-node is
#    install.sh edit + re-run, but for tarball size we only fetch the
#    active one (other can be downloaded by install.sh on demand).
case "$ENGINE" in
  sing-box)
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
    ;;
  mihomo)
    # Use compatible (non-AMD64-v3) build — works on FeiLian forwarding-
    # node hardware classes that lack AVX2.
    M_PKG="mihomo-linux-${GOARCH}-compatible-v${MIHOMO_VERSION}"
    M_GZ="$SINGBOX_CACHE/${M_PKG}.gz"
    if [ ! -f "$M_GZ" ]; then
      url="https://github.com/MetaCubeX/mihomo/releases/download/v${MIHOMO_VERSION}/${M_PKG}.gz"
      log "downloading $url"
      curl -fsSL "$url" -o "$M_GZ" || fail "could not download mihomo"
    fi
    gunzip -c "$M_GZ" > "$STAGE_DIR/mihomo"
    chmod 0755 "$STAGE_DIR/mihomo"
    ls -lh "$STAGE_DIR/mihomo" | awk '{print "[stage] mihomo", $5}'

    # geoip.metadb — mihomo's dns.fallback-filter geoip:CN check requires
    # this; without it, mihomo refuses to load when fakeip is on. Fetch
    # from MetaCubeX's parallel asset publication.
    METADB_FILE="$SINGBOX_CACHE/geoip.metadb"
    if [ ! -s "$METADB_FILE" ]; then
      url="https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb"
      log "downloading $url"
      curl -fsSL "$url" -o "$METADB_FILE" || fail "could not download geoip.metadb"
    fi
    cp "$METADB_FILE" "$STAGE_DIR/geoip.metadb"
    ls -lh "$STAGE_DIR/geoip.metadb" | awk '{print "[stage] geoip.metadb", $5}'
    ;;
esac

# 3. Operator gateway.yaml.
cp "$GATEWAY_YAML" "$STAGE_DIR/gateway.yaml"
log "gateway.yaml copied from $GATEWAY_YAML"

# 4. deploy/node/* — install.sh, *.service, nft.conf.tmpl, iproute.sh, logrotate-leap.conf.
cp deploy/node/install.sh \
   deploy/node/iproute.sh \
   deploy/node/nft.conf.tmpl \
   deploy/node/logrotate-leap.conf \
   deploy/node/leap-singbox.service \
   deploy/node/leap-mihomo.service \
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

# Rule-set entries are "<save_name>|<url>" tuples. MetaCubeX URL paths use
# stem-only filenames (no "geosite-" / "geoip-" prefix), but the renderer
# loads each rule-set by tag — so we save to <tag>.<ext> on disk where
# <ext> matches the engine's format (sing-box → .srs, mihomo → .mrs).
case "$ENGINE" in
  sing-box) FMT_BRANCH="sing"; FMT_EXT="srs" ;;
  mihomo)   FMT_BRANCH="meta"; FMT_EXT="mrs" ;;
esac
log "rule-set format: .$FMT_EXT (MetaCubeX /$FMT_BRANCH/ branch)"

# Always pre-fetch the two infra rule-sets used by the route classifier.
RULESET_ENTRIES=(
  "geosite-cn.${FMT_EXT}|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/${FMT_BRANCH}/geo/geosite/cn.${FMT_EXT}"
  "geoip-cn.${FMT_EXT}|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/${FMT_BRANCH}/geo/geoip/cn.${FMT_EXT}"
)

# Tags requested by gateway.yaml as full URLs — legacy form, kept for yaml
# that still has explicit url: fields under singbox.route.geosite_url etc.
# Strip the engine-specific extension and rewrite via FMT_BRANCH/FMT_EXT.
while IFS= read -r u; do
  case "$u" in
    *"/geo/geosite/"*)
      stem=$(basename "$u" .srs); stem=${stem%.mrs}
      RULESET_ENTRIES+=("geosite-${stem}.${FMT_EXT}|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/${FMT_BRANCH}/geo/geosite/${stem}.${FMT_EXT}")
      ;;
    *"/geo/geoip/"*)
      stem=$(basename "$u" .srs); stem=${stem%.mrs}
      RULESET_ENTRIES+=("geoip-${stem}.${FMT_EXT}|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/${FMT_BRANCH}/geo/geoip/${stem}.${FMT_EXT}")
      ;;
    *) RULESET_ENTRIES+=("$(basename "$u")|$u") ;;
  esac
done < <(grep -oE 'https://[^"[:space:]]+\.(srs|mrs)' "$GATEWAY_YAML" | sort -u)

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

# Resolve each WL tag to a URL by prefix. MetaCubeX paths use stem-only
# filenames; we save to <tag>.<ext> locally to match the renderer's
# rule_set path lookup.
for tag in "${WL_TAGS[@]}"; do
  case "$tag" in
    geosite-*) stem="${tag#geosite-}"
               RULESET_ENTRIES+=("${tag}.${FMT_EXT}|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/${FMT_BRANCH}/geo/geosite/${stem}.${FMT_EXT}") ;;
    geoip-*)   stem="${tag#geoip-}"
               RULESET_ENTRIES+=("${tag}.${FMT_EXT}|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/${FMT_BRANCH}/geo/geoip/${stem}.${FMT_EXT}") ;;
    *)         log "  skip unknown tag prefix: $tag" ;;
  esac
done

# Dedupe by save_name|url tuple (bash 3.2 has no associative arrays).
DEDUPED_ENTRIES=()
SEEN=$'\n'
for e in "${RULESET_ENTRIES[@]}"; do
  case "$SEEN" in
    *$'\n'"$e"$'\n'*) ;;
    *)
      DEDUPED_ENTRIES+=("$e")
      SEEN="$SEEN$e"$'\n'
      ;;
  esac
done

log "fetching ${#DEDUPED_ENTRIES[@]} rule-set .${FMT_EXT} files (cn infra + gateway.yaml whitelist)"
fetched=0
skipped=0
for entry in "${DEDUPED_ENTRIES[@]}"; do
  fname="${entry%%|*}"
  url="${entry#*|}"
  cached="$RULESETS_CACHE/$fname"
  if [ ! -s "$cached" ]; then
    if ! curl -fsSL --retry 2 --max-time 30 "$url" -o "$cached.tmp"; then
      log "  skip $fname (fetch failed: $url)"
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
