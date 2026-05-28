#!/usr/bin/env python3
"""
expand-whitelist.py — 把 leap-gateway 的白名单展开成域名后缀列表。

用途：飞连 SaaS 控制端的"极速模式"需要明确告诉飞连终端"哪些域名走 VPN"。
我们节点 sing-box 的白名单是 geosite tag + domain_suffix，要让飞连和节点保持
一致，必须把 geosite 展开成具体域名喂给飞连。

数据源：v2fly/domain-list-community（这是 sagernet/sing-geosite 的上游）。
每个 geosite-X 对应仓库里 data/X 一个纯文本文件，带 include: 递归引用。

输出：每行一个根域名 / 子域名，可直接粘贴进飞连"极速模式"的域名白名单。

用法：
  scripts/expand-whitelist.py                          # 默认从 PoC API 拉白名单
  scripts/expand-whitelist.py --api 192.168.70.92:18080
  scripts/expand-whitelist.py --geosites google,openai --domain-suffix claude.ai

退出码 != 0 时打印错误到 stderr。
"""

import argparse
import json
import sys
import urllib.request
from urllib.error import HTTPError, URLError

V2FLY_BASES = [
    # jsdelivr CDN — reachable from mainland China (the operator workstation
    # may not have a working proxy when running this script).
    "https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/",
    # raw.githubusercontent fallback for non-CN runs / when jsdelivr stale.
    "https://raw.githubusercontent.com/v2fly/domain-list-community/master/data/",
]
DEFAULT_API = "192.168.70.92:18080"


def fetch_v2fly(name: str) -> str:
    last_err = None
    for base in V2FLY_BASES:
        url = base + name
        try:
            with urllib.request.urlopen(url, timeout=10) as r:
                return r.read().decode("utf-8", errors="replace")
        except (HTTPError, URLError) as e:
            last_err = f"{url}: {e}"
            continue
    raise RuntimeError(f"all sources failed; last: {last_err}")


def fetch_url(url: str) -> str:
    try:
        with urllib.request.urlopen(url, timeout=10) as r:
            return r.read().decode("utf-8", errors="replace")
    except (HTTPError, URLError) as e:
        raise RuntimeError(f"fetch {url}: {e}")


def parse_geosite(name: str, seen: set, out: set) -> None:
    """
    Recursive parse of one geosite category. Walks include: links,
    drops regex:/keyword:/comments, strips @attributes, normalizes prefixes.
    """
    if name in seen:
        return
    seen.add(name)

    body = fetch_v2fly(name)
    for raw in body.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue

        # Split off @attributes (e.g. "@cn", "@ads") — keep the entry; sing-box
        # loads everything by default, so we mirror that.
        head = line.split("@", 1)[0].strip()
        if not head:
            continue

        if head.startswith("include:"):
            child = head[len("include:") :].strip()
            if child:
                parse_geosite(child, seen, out)
            continue

        if head.startswith("regex:"):
            # FeiLian's domain whitelist doesn't support regex; ad-hoc cases
            # need to be added manually.
            continue
        if head.startswith("keyword:"):
            # Same — keyword match isn't a domain suffix.
            continue

        if head.startswith("full:"):
            domain = head[len("full:") :].strip()
        elif head.startswith("domain:"):
            domain = head[len("domain:") :].strip()
        else:
            domain = head

        if domain:
            out.add(domain.lower())


def fetch_whitelist_api(api_addr: str) -> dict:
    url = f"http://{api_addr}/api/whitelist"
    body = fetch_url(url)
    return json.loads(body)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--api", default=DEFAULT_API,
                    help=f"leap-gateway API addr (default: {DEFAULT_API})")
    ap.add_argument("--geosites",
                    help="override: comma-separated geosite tags, e.g. google,openai")
    ap.add_argument("--domain-suffix",
                    help="override: comma-separated extra domains to append")
    ap.add_argument("--csv", action="store_true",
                    help="emit comma-separated single line instead of one per line")
    ap.add_argument("--show-stats", action="store_true",
                    help="print summary (#categories, #domains) to stderr")
    args = ap.parse_args()

    if args.geosites or args.domain_suffix:
        geosites = [g.strip() for g in (args.geosites or "").split(",") if g.strip()]
        suffix = [s.strip() for s in (args.domain_suffix or "").split(",") if s.strip()]
    else:
        try:
            wl = fetch_whitelist_api(args.api)
        except Exception as e:
            print(f"error: cannot read whitelist from {args.api}: {e}", file=sys.stderr)
            return 1
        # geosite tags from the API come as "geosite-google"; the v2fly source
        # uses just "google", so strip the prefix.
        geosites = [g[len("geosite-"):] if g.startswith("geosite-") else g
                    for g in wl.get("geosites", [])]
        suffix = wl.get("domain_suffix", [])

    if not geosites and not suffix:
        print("error: empty whitelist (no geosites + no domain_suffix)", file=sys.stderr)
        return 1

    seen: set = set()
    domains: set = set()
    for g in geosites:
        try:
            parse_geosite(g, seen, domains)
        except Exception as e:
            print(f"warn: geosite '{g}' failed: {e}", file=sys.stderr)

    for s in suffix:
        domains.add(s.lower())

    sorted_domains = sorted(domains)
    if args.csv:
        print(",".join(sorted_domains))
    else:
        for d in sorted_domains:
            print(d)

    if args.show_stats:
        print(f"# categories expanded: {len(geosites)}", file=sys.stderr)
        print(f"# domains (deduped):   {len(sorted_domains)}", file=sys.stderr)

    return 0


if __name__ == "__main__":
    sys.exit(main())
