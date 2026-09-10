#!/usr/bin/env bash
# scripts/stage.sh — produce a deployable tarball for a FeiLian forwarding
# node sidecar install.
#
# Outputs $OUT_DIR/leap-stage.tgz containing:
#   leap-gateway          (linux/amd64, built from this checkout, version-stamped)
#   mihomo                (1.19.x linux/amd64, the data plane)
#   sing-box              (1.10.x linux/amd64, CLI-only — used by
#                          whitelistexpand for `rule-set decompile`,
#                          NOT run as a service)
#   geoip.metadb          (mihomo's MaxMind DB; required by geoip routing rules)
#   gateway.yaml          (operator-supplied; defaults to ./gateway.yaml)
#   <deploy/node/* files> (install.sh, *.service, nft.conf.tmpl,
#                          iproute.sh, logrotate-leap.conf)
#   rule-sets/*.mrs       (cn infra + tags referenced by gateway.yaml's
#                          whitelist, fetched from MetaCubeX /meta/ branch)
#
# Engine is always mihomo; the sing-box-based renderer was removed
# in 2026-06. /usr/local/bin/sing-box is still shipped because the
# gateway shells out to it for `.srs` decompile (mihomo lacks an
# equivalent of `rule-set decompile`).
#
# Usage on operator workstation:
#   cp configs/gateway.example.yaml gateway.yaml   # then edit subscription URL + node values
#   scripts/stage.sh
#   scp build/leap-stage.tgz dianwei@<node>:/tmp/
#   ssh dianwei@<node> 'cd /tmp && tar -xzf leap-stage.tgz && sudo bash leap-stage/install.sh'
#
# Env overrides:
#   GATEWAY_YAML       path to gateway.yaml (default: ./gateway.yaml)
#   MIHOMO_VERSION     mihomo version       (default: 1.19.26)
#   SINGBOX_VERSION    sing-box CLI version (default: 1.10.7)
#   STAGE_CACHE        cache dir for downloaded binaries (default: ./.cache)
#   OUT_DIR            output directory (default: ./build)
#   GOARCH             target arch (default: amd64; set arm64 for ARM nodes)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GATEWAY_YAML="${GATEWAY_YAML:-./gateway.yaml}"
MIHOMO_VERSION="${MIHOMO_VERSION:-1.19.26}"
SINGBOX_VERSION="${SINGBOX_VERSION:-1.10.7}"
STAGE_CACHE="${STAGE_CACHE:-${SINGBOX_CACHE:-./.cache}}"
OUT_DIR="${OUT_DIR:-./build}"
GOARCH="${GOARCH:-amd64}"

log() { printf '[stage] %s\n' "$*"; }
fail() { printf '[stage][FAIL] %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Without an explicit yaml the staged tarball would be useless. The
# preferred flow for an existing node is `make redeploy-92`
# (or `scripts/redeploy-full.sh <host>`) — that pulls
# /etc/leap/gateway.yaml from the node and feeds it to this script via
# GATEWAY_YAML, so the staged tarball never lies about what the node will
# end up running. Only fall back to ./gateway.yaml + plain `make stage`
# when bootstrapping a fresh node that has no on-disk config yet.
[ -f "$GATEWAY_YAML" ] || fail "gateway.yaml not found at $GATEWAY_YAML.
       For an existing node, prefer:  make redeploy-92  /  scripts/redeploy-full.sh <host>
       For a fresh node, copy:        configs/gateway.example.yaml → ./gateway.yaml
                                       and fill in subscription URL + node values."

mkdir -p "$OUT_DIR" "$STAGE_CACHE"
STAGE_DIR="$OUT_DIR/leap-stage"
rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR"

# 1. Build leap-gateway for the target platform.
# Version is `git describe --tags --always --dirty`: tag if present, short
# sha otherwise, with `-dirty` suffix when the worktree has uncommitted
# changes. Injected into main.Version so /api/proxies/active.leap
# .gateway_version reports it — `dev` means the binary was built off-tree,
# which is exactly the case we want to flag during incident triage.
VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
case "$VERSION" in
  *-dirty)
    log "WARNING: worktree dirty — version=$VERSION will lie about what's deployed."
    log "         commit your changes first if this is going to a node you care about."
    ;;
esac
log "building leap-gateway (linux/$GOARCH, version=$VERSION)"
GOOS=linux GOARCH="$GOARCH" CGO_ENABLED=0 go build \
  -ldflags="-s -w -X main.Version=$VERSION" \
  -o "$STAGE_DIR/leap-gateway" ./cmd/gateway
ls -lh "$STAGE_DIR/leap-gateway" | awk '{print "[stage] leap-gateway", $5}'

# 2. mihomo data-plane binary. Use the "compatible" build (non-AMD64-v3)
# so it runs on FeiLian forwarding-node hardware classes that lack AVX2.
M_PKG="mihomo-linux-${GOARCH}-compatible-v${MIHOMO_VERSION}"
M_GZ="$STAGE_CACHE/${M_PKG}.gz"
if [ ! -f "$M_GZ" ]; then
  url="https://github.com/MetaCubeX/mihomo/releases/download/v${MIHOMO_VERSION}/${M_PKG}.gz"
  log "downloading $url"
  curl -fsSL "$url" -o "$M_GZ" || fail "could not download mihomo"
fi
gunzip -c "$M_GZ" > "$STAGE_DIR/mihomo"
chmod 0755 "$STAGE_DIR/mihomo"
ls -lh "$STAGE_DIR/mihomo" | awk '{print "[stage] mihomo", $5}'

# geoip.metadb — mihomo's geoip-cn routing rule requires this MaxMind DB;
# without it mihomo refuses to load geoip rule-sets.
METADB_FILE="$STAGE_CACHE/geoip.metadb"
if [ ! -s "$METADB_FILE" ]; then
  url="https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb"
  log "downloading $url"
  curl -fsSL "$url" -o "$METADB_FILE" || fail "could not download geoip.metadb"
fi
cp "$METADB_FILE" "$STAGE_DIR/geoip.metadb"
ls -lh "$STAGE_DIR/geoip.metadb" | awk '{print "[stage] geoip.metadb", $5}'

# 3. sing-box CLI binary (helper, not a service). Required because
# whitelistexpand shells out to `sing-box rule-set decompile` for geoip
# expansion — mihomo doesn't have an equivalent decompile command.
SB_PKG="sing-box-${SINGBOX_VERSION}-linux-${GOARCH}"
SB_TGZ="$STAGE_CACHE/${SB_PKG}.tar.gz"
if [ ! -f "$SB_TGZ" ]; then
  url="https://github.com/SagerNet/sing-box/releases/download/v${SINGBOX_VERSION}/${SB_PKG}.tar.gz"
  log "downloading $url"
  curl -fsSL "$url" -o "$SB_TGZ" || fail "could not download sing-box CLI"
fi
tmp=$(mktemp -d)
tar -xzf "$SB_TGZ" -C "$tmp"
cp "$tmp/$SB_PKG/sing-box" "$STAGE_DIR/sing-box"
chmod 0755 "$STAGE_DIR/sing-box"
rm -rf "$tmp"
ls -lh "$STAGE_DIR/sing-box" | awk '{print "[stage] sing-box (CLI)", $5}'

# 4. Operator gateway.yaml.
# ⛔ subscriptions[] in this yaml is operator-owned and per-node-specific.
# If this stage came from `redeploy-full.sh` (yaml pulled from a node),
# the subscriptions block is going right back to that node — DO NOT edit
# entries between pull and push. See CLAUDE.md.
cp "$GATEWAY_YAML" "$STAGE_DIR/gateway.yaml"
log "gateway.yaml copied from $GATEWAY_YAML"

# 5. deploy/node/* — install.sh, *.service, nft.conf.tmpl, iproute.sh,
# logrotate-leap.conf. leap-singbox.service was removed in the 2026-06
# sing-box engine cleanup; only mihomo + gateway + nft units ship now.
cp deploy/node/install.sh \
   deploy/node/iproute.sh \
   deploy/node/nft.conf.tmpl \
   deploy/node/logrotate-leap.conf \
   deploy/node/leap-mihomo.service \
   deploy/node/leap-gateway.service \
   deploy/node/leap-nft.service \
   "$STAGE_DIR/"
chmod 0755 "$STAGE_DIR/install.sh" "$STAGE_DIR/iproute.sh"
log "deploy/node/* files staged"

# 6. Rule-set .mrs blobs. Pre-fetched into the tarball so mihomo can load
# geosite-cn / geoip-cn (route infra) and any tags already selected in
# gateway.yaml.whitelist.{geosites,geoips} as type=file at startup.
# Tags NOT referenced by gateway.yaml are fetched on-demand by the runtime
# API (PUT /api/whitelist) when an operator selects them.
#
# Per-URL cache in $STAGE_CACHE/rule-sets/. Delete that dir to force
# re-fetch.
RULESETS_CACHE="$STAGE_CACHE/rule-sets"
mkdir -p "$RULESETS_CACHE" "$STAGE_DIR/rule-sets"

# Always pre-fetch the two infra rule-sets used by the route classifier.
# MetaCubeX URL paths use stem-only filenames; we save to <tag>.mrs locally
# to match the renderer's rule_set path lookup.
RULESET_ENTRIES=(
  "geosite-cn.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/cn.mrs"
  "geoip-cn.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/cn.mrs"
)

# Tags requested by gateway.yaml as full URLs — legacy form, kept for yaml
# that still has explicit url: fields under data_plane.route.geosite_url etc.
# Strip any extension and rewrite under /meta/ + .mrs.
while IFS= read -r u; do
  case "$u" in
    *"/geo/geosite/"*)
      stem=$(basename "$u" .srs); stem=${stem%.mrs}
      RULESET_ENTRIES+=("geosite-${stem}.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/${stem}.mrs")
      ;;
    *"/geo/geoip/"*)
      stem=$(basename "$u" .srs); stem=${stem%.mrs}
      RULESET_ENTRIES+=("geoip-${stem}.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/${stem}.mrs")
      ;;
    *) RULESET_ENTRIES+=("$(basename "$u")|$u") ;;
  esac
done < <(grep -oE 'https://[^"[:space:]]+\.(srs|mrs)' "$GATEWAY_YAML" | sort -u)

# Tags from gateway.yaml's whitelist (geosites: [tag, ...] and geoips:
# [tag, ...] — bare strings, no urls). Pull both and let the prefix
# branch decide the upstream.
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

for tag in "${WL_TAGS[@]:-}"; do
  case "$tag" in
    geosite-*) stem="${tag#geosite-}"
               RULESET_ENTRIES+=("${tag}.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/${stem}.mrs") ;;
    geoip-*)   stem="${tag#geoip-}"
               RULESET_ENTRIES+=("${tag}.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/${stem}.mrs") ;;
    *)         log "  skip unknown tag prefix: $tag" ;;
  esac
done

# Tags from gateway.yaml pools[].rule_sets — pools need their .mrs files
# just as much as whitelist entries. If stage.sh skips them, mihomo fails
# on startup with "file not found" for the rule-provider.
POOL_TAGS=()
while IFS= read -r tag; do
  POOL_TAGS+=("$tag")
done < <(awk '
  /^pools:/                     { inpools=1; next }
  inpools && /^[^[:space:]]/    { inpools=0 }
  inpools && /rule_sets:/        { inrs=1; next }
  inpools && /^[[:space:]]*[a-zA-Z_-]+:/ && !/rule_sets:/ { inrs=0 }
  inrs && /^[[:space:]]*-[[:space:]]/ {
    sub(/^[[:space:]]*-[[:space:]]*/, "")
    sub(/[[:space:]]*#.*/, "")
    gsub(/"|'"'"'/, "")
    if ($0 ~ /^(geosite|geoip)-/) print $0
  }
' "$GATEWAY_YAML")

for tag in "${POOL_TAGS[@]:-}"; do
  case "$tag" in
    geosite-*) stem="${tag#geosite-}"
               RULESET_ENTRIES+=("${tag}.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/${stem}.mrs") ;;
    geoip-*)   stem="${tag#geoip-}"
               RULESET_ENTRIES+=("${tag}.mrs|https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geoip/${stem}.mrs") ;;
  esac
done

# Dedupe by save_name|url tuple (bash 3.2 has no associative arrays).
DEDUPED_ENTRIES=()
SEEN=$'\n'
for e in "${RULESET_ENTRIES[@]:-}"; do
  case "$SEEN" in
    *$'\n'"$e"$'\n'*) ;;
    *)
      DEDUPED_ENTRIES+=("$e")
      SEEN="$SEEN$e"$'\n'
      ;;
  esac
done

log "fetching ${#DEDUPED_ENTRIES[@]} rule-set .mrs files (cn infra + gateway.yaml whitelist)"
fetched=0
skipped=0
for entry in "${DEDUPED_ENTRIES[@]:-}"; do
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

# 7. Tarball.
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
