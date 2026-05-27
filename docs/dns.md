# DNS 污染对抗 — fake-IP 实战版

> 本文展开 [design.md §6](./design.md#6-dns-子系统--fake-ip) 的 DNS 部分，覆盖每种污染形态、对应防御、节点上的实测数据，以及 H1（DNS push 语义）的应急退路。

## 1. 污染形态清单

GFW（及国内运营商）对 DNS 的干预，按攻击面分四类：

| # | 攻击形态 | 触发场景 | 我们的暴露面 |
|---|---|---|---|
| A | **UDP 53 抢答** | 客户端用明文 53 查询，GFW 比真上游早返回伪造 A 记录 | 任何 UDP 53 上游 |
| B | **DNS 中间人篡改** | 经过国内 ISP 的 DNS 查询被改写（多见于 DoT 阻断后退到 53） | UDP 53 + 部分 DoT |
| C | **CNAME 链污染** | 即使 A 记录对，CNAME 链中某一跳被改写 | 所有 53/DoT，部分 DoH |
| D | **CDN 边缘错配** | 国内 DNS 给国际 CDN 域名返回的边缘 IP 不可达或带宽差 | 任何在国内做出境 CDN 解析 |

补充一个非 DNS 但常被混淆的攻击面：

| # | 攻击形态 | 触发场景 | 防御位置 |
|---|---|---|---|
| E | **TLS SNI 阻断** | DNS 解析 OK 后，HTTPS 握手时 GFW 看到 SNI=被墙域名 → RST | 必须由代理出站承担，不在 DNS 层解决 |

## 2. 节点出口实测数据 (2026-05-27, 192.168.70.246)

> 这一节是这一期飞连转发节点环境的独家数据，写代码 / 选上游 DoH 时直接引用。

### 2.1 节点直连墙外的 DoT/DoH 联通性

| 上游 | 协议 | 节点直连结果 | 备注 |
|---|---|---|---|
| `1.1.1.1:853` | DoT | ✓ succeeded | Cloudflare DoT 实测可达，**首选海外 DoT** |
| `1.1.1.1:443` | DoH | ✗ 5s timeout | HTTPS 路径被限制（443 GFW 看 SNI = `cloudflare-dns.com` 阻断概率高） |
| `dns.google:853` | DoT | ✗ timeout (`8.8.8.8` / `8.8.4.4`) | 完全被墙 |
| `120.53.53.53:853` (`dot.pub`) | DoT | ✓ succeeded | 腾讯 DoT，国内 DoT |
| `https://doh.pub/dns-query` | DoH | ✓ 0.34s | 腾讯 DoH，国内首选 |
| `https://dns.alidns.com/dns-query` | DoH | ✓ 0.22s | 阿里 DoH，国内备选 |

### 2.2 腾讯 doh.pub 对境外站点不污染（重要）

实测 `https://doh.pub/dns-query?name=google.com&type=A` 从节点出去返回 `142.250.196.206` —— 是真实的 Google IP，不是污染答案。这意味着：

- **国内域名**：用 doh.pub 解析，拿国内 CDN 边缘 IP（直连最优）。
- **境外域名**：在 fake-IP 模式下不需要真实解析（fake-IP 直接给客户端 198.18.x.x，sing-box 把原域名传给机场节点解析），但**作为 fallback** 也能用 doh.pub 拿到能用的真实 IP。

但**不能依赖**腾讯永远不污染——这是腾讯当前策略，未来可能变。所以默认 sing-box 配置里把 Cloudflare DoT (`1.1.1.1:853`) 列在前面，doh.pub 当 fallback。

### 2.3 节点本身上墙的限制

节点本身的 ens18 出口默认走机房网关（`192.168.70.1`），出公网时受到 GFW 限制：
- 大部分海外 HTTPS（443）有 SNI 阻断风险。
- 海外 DoT (853) 部分可达（Cloudflare），部分不可达（Google）。

**结论**：墙外目的的访问**必须经机场订阅出口**，sing-box 节点本身不能裸出 ens18 解析海外。这是 v3 架构和 v2 架构的根本区别——v2 假设 VPN 服务器在境内但没有强 GFW 限制，v3 实测下来发现飞连节点环境就是有限制。

## 3. 单点防御不够，得叠层

任何一种单点防御都有漏洞：
- 只用 DoH 上游：解决 A/B 但不解决 D（仍在国内解析 → 拿到错的 CDN 边缘）。
- 只用海外 DNS：解决 A/B/D 但慢，且对国内站不友好。
- 只用 fake-IP：解决 A/B/D，但客户端被注入恶意 hosts 时仍会走旧 IP。

我们的方案 = 五层组合：

```
1. fake-IP            ── 客户端层面把真实 IP 隔离掉
2. 上游 DoH/DoT       ── 抗 A/B/C 的链路加密
3. 域名分流           ── 决定到底用哪个上游
4. 代理端解析         ── 出境流量的 DNS 也在境外做
5. 飞连 push DNS      ── OS 系统级 DNS 指向 sing-box
```

## 4. fake-IP 是怎么把污染挡住的

传统模式（redir-host）：
```
客户端 → DNS 查 github.com → 得到真实 IP（已被污染）→ 客户端连 IP → 走分流（按 IP 判，错）
```

fake-IP 模式：
```
客户端 → DNS 查 github.com → sing-box 给 198.18.0.1（虚拟） → 客户端连 198.18.0.1
                                                                ↓
                                       sing-box TUN 拦截 → 反查回域名 github.com
                                                                ↓
                                         按域名走 geosite-cn 判定 → 不命中 cn → out (selector)
                                                                ↓
                                         域名 github.com 直接送给机场节点
                                                                ↓
                                                  机场节点（境外）做真实 DNS 解析
                                                                ↓
                                                  连境外的 GitHub 边缘 → 通
```

关键点：
- **客户端 DNS 缓存的是 fake-IP，永远不会缓存被污染的真实 IP**。
- **真实 DNS 解析推迟到出站时刻，且发生在境外**（如果走代理）或境内 DoH（如果直连）。
- **分流粒度是域名**，不是 IP，CDN 域名永远不会被错判。

## 5. 飞连 push DNS 的语义和 H1 风险

### 5.1 期望行为

飞连 SaaS 控制端 → push DNS 字段填 `<tun0_gw>` (主) / `223.5.5.5` (备) → 飞连客户端拿到这两条 → 写进客户端系统 resolv.conf（macOS：`scutil --dns` 可见；Linux：`/etc/resolv.conf` 通常被覆盖）→ OS 把所有 DNS 查询发往主 DNS。

我们填到飞连 SaaS 的**只是一个 IP**：`<tun0_gw>` 比如 `10.8.11.1`。**不要填 198.18.x.x**——`198.18.0.0/15` 是 sing-box 内部的 fake-IP 池，**不是 DNS server 地址**。客户端和飞连控制端都看不到也不需要知道这个池。

### 5.2 H1：DNS push 真的是系统级覆盖吗？

CorpLink 是 WireGuard 的二次开发，DNS 下发可能有三种实现：

- **(A)** 标准 wg `DNS=...` → 写进客户端系统 resolv.conf（plan 假设的就是这种）。
- **(B)** 应用内 DNS → 只对走 tun 的连接生效。
- **(C)** Per-app DNS → 仅指定应用走 push DNS，其他走系统 DNS。

**Mac 上 `scutil --dns` 列着 push DNS** 只能证明不是 (C)，但**仍可能是 (B)**——浏览器 DoH、特定 SDK 内嵌 DNS 都可能绕过。

如果是 (B)，DNS-based 分流被绕过的部分会被 sing-box 退到**按 IP geoip-cn** 兜底分流，**仍能跑**，只是 CDN 边界精度下降。

### 5.3 H1 验证方法（Stage 0 必跑）

测试机接入 PoC 节点后跑：

```bash
sudo tcpdump -i any -n 'port 53 and not host <push_dns> and not host <push_dns_backup>' -w leak.pcap
```

期间正常使用浏览器（Chrome auto-DoH 是常见漏出来源）、Telegram、Bonjour 等。1h 后：

```bash
tcpdump -nr leak.pcap | wc -l       # 漏出查询数
tcpdump -nr leak.pcap | awk '{print $NF}' | sort -u | head    # 漏到哪些上游
```

通过条件：漏出 < 5% 总查询。否则切 5.4 应急方案。

### 5.4 应急：H1 不通时退到真解析 + sniff

如果 H1 验证发现飞连 DNS push 不是系统级，fake-IP 路径覆盖率不够，切到：

- sing-box `dns.fakeip.enabled = false`，不返回假 IP。
- DNS 上游照常解析，sing-box 把真 IP 给客户端。
- TUN inbound `sniff = true`，从 TLS ClientHello 拿 SNI 当域名，按 SNI 走 geosite 分流。
- 完全没 SNI 的（比如普通 HTTP，但越来越罕见）退到 geoip-cn 按 IP 分流。

代价：精度比 fake-IP 差，CDN 边界域名（同一 IP 服务多个站点）误判会增加。

## 6. 具体配置（sing-box 1.10+，渲染器输出）

```jsonc
"dns": {
  "servers": [
    // 上游 1：海外 DoT（首选） — 节点直连可达
    { "tag": "remote",
      "address": "tls://1.1.1.1:853",
      "address_resolver": "local",
      "strategy": "ipv4_only",
      "detour": "out" },

    // 上游 2：国内 DoH，detour=direct → 走本机直连到腾讯 DoH
    { "tag": "local",
      "address": "https://doh.pub/dns-query",
      "address_resolver": "local-bootstrap",
      "strategy": "ipv4_only",
      "detour": "direct" },

    // 上游 3：fake-IP 池
    { "tag": "fakeip", "address": "fakeip" },

    // bootstrap：用 IP literal，避免 chicken-and-egg
    { "tag": "local-bootstrap",
      "address": "udp://119.29.29.29",
      "detour": "direct" }
  ],

  "rules": [
    // 1. 出站本身（机场节点解析等）走 local，避免回环
    { "outbound": "any", "server": "local" },
    // 2. 直连域名 → 国内 DoH（拿国内 CDN 边缘）
    { "rule_set": ["geosite-cn"], "server": "local" },
    // 3. 其它 A/AAAA → fake-IP
    { "query_type": ["A", "AAAA"], "server": "fakeip" }
  ],

  "fakeip": {
    "enabled": true,
    "inet4_range": "198.18.0.0/15"
  },

  "strategy": "ipv4_only",
  "independent_cache": true,
  "final": "remote"
}
```

要点：

- `strategy: ipv4_only`：M2 阶段统一压到 v4，IPv6 的 fake-IP 段更容易冲突真实服务，等观测稳定再放开。
- `independent_cache: true`：让 fake-IP 表和 remote/local 真实解析的 cache 分开，避免缓存毒化跨上游传染。
- `local-bootstrap`：用 IP literal `119.29.29.29`，sing-box 启动时不需要先解析域名才能联通 DNS 上游。

## 7. 引导问题（chicken-and-egg）

sing-box 启动时要解析 DoH/DoT 服务器自己的域名。两条路：

**路 A：上游全部用 IP literal 做 host**
- `tls://1.1.1.1:853` — Cloudflare 证书覆盖 IP
- `https://120.53.53.53/dns-query` — 腾讯证书覆盖 IP
- 完全无引导依赖。**首选**。

**路 B：用域名 host + 显式 bootstrap server**
- `address: https://doh.pub/dns-query`
- `address_resolver: local-bootstrap`（指向 IP literal 的 UDP 上游，仅做引导用）
- 域名变更时翻车风险低（腾讯/阿里 DNS 域名不太会变）

我们用路 A 做 remote、路 B 做 local（doh.pub 用域名，bootstrap 通过 119.29.29.29 解析一次就 cache）。

## 8. 验证清单

部署到测试节点后用这套自测，每一步过了再交付：

### 8.1 fake-IP 工作

测试节点本机：
```bash
dig @<tun0_gw> github.com
# 期望：ANSWER 段一条 198.18.0.X 的 A 记录

dig @<tun0_gw> www.taobao.com
# 期望：国内 IP（命中 server=local）
```

客户端（已连飞连 PoC 节点）：
```bash
# Mac
dig github.com
# 期望：同样 198.18.0.X
scutil --dns | grep nameserver
# 期望：第一条是 <tun0_gw>
```

如果客户端拿到真实 IP，说明 H1 风险触发——飞连 DNS push 不是系统级或被绕过。看 5.3 抓 leak。

### 8.2 国内 DoH 工作

```bash
# 测试节点
dig @<tun0_gw> www.taobao.com
# 期望：国内 IP（114.x / 124.x / 等），命中 server=local

# 抓包确认 53 没出去，DoH 走了 443
sudo tcpdump -i any 'port 53 and host <doh.pub IP>' -c 3   # 期望：没流量
sudo tcpdump -i any 'host <doh.pub IP> and tcp' -c 3       # 期望：TLS 流量
```

### 8.3 出境 DoT 工作

```bash
sudo tcpdump -i ens18 'host 1.1.1.1 and port 853' -c 5
# 期望：sing-box 启动后会发 ClientHello 到 1.1.1.1:853
```

### 8.4 抗污染抽查

挑几个被墙域名做盲测（在测试机上）：

```bash
for d in www.google.com github.com www.youtube.com twitter.com; do
  echo "=== $d ==="
  dig +short $d                # 应是 198.18.x
  curl -sI https://$d -o /dev/null -w "%{http_code} %{time_total}s\n"
done
```

每一行都要 200/204/30x，不能 connection-reset 或 timeout。

### 8.5 抗 SNI 阻断（不属于 DNS 层但顺带）

```bash
# 测试节点本机直连测试（不该通过）
curl --resolve www.google.com:443:142.251.42.4 https://www.google.com -I
# 期望：connection reset / timeout（GFW SNI 拦截）

# 经机场（应该通）
curl --proxy socks5://127.0.0.1:1080 https://www.google.com -I
# 期望：200
```

## 9. 故障 → 处置矩阵

| 现象 | 最可能原因 | 处置 |
|---|---|---|
| 客户端 dig 拿到真实 IP（不是 198.18.x） | 飞连 DNS push 没生效 / 被绕过（H1） | 5.3 抓 leak.pcap；漏出严重就切 5.4 真解析 + sniff |
| 客户端拿到 198.18.x 但访问不通 | sing-box TUN 路由没起来；或对应 outbound 红 | 节点 `ip rule`、`ip route show table 100`；clash-api 节点延迟 |
| 国内站点慢、解析延迟高 | 国内 DoH 上游故障 | sing-box 日志看 server=local 错误；切到备份（阿里 DoH `https://dns.alidns.com/dns-query`） |
| 国内域名走了代理（不该走） | geosite-cn 没下载到 / 过期 | `route.rule_set update_interval` 缩短；或控制面强制刷一次 |
| 国外站点偶发解析失败 | 机场节点抖动 | sing-box `dns.servers[].rate_limit`；增加备用上游 `tls://9.9.9.9:853`（Quad9） |
| 客户端 DNS 泄漏到飞连默认 | push DNS 字段没改 / 飞连客户端没重连 | 飞连 SaaS 后台再确认；让员工 disconnect/reconnect |

## 10. 进阶：自建 DNS 黑白名单

geosite-cn 列表是社区维护的近似值，对企业自有域名 / 私有 SaaS / 客户内网回环域名往往覆盖不到。

控制面提供两个口子：

**白名单"必须直连"** — 写到 `geosite-cn-extra.srs`：
```
auth.your-corp.com
git.your-corp.com
*.intranet.your-corp.com
```

**黑名单"必须代理"** — 写到 `geosite-proxy-extra.srs`：
```
docs.google.com
mail.google.com
```

控制面定时把这两个文件编译成 `.srs` 并 reload sing-box（用 sing-box `rule-set` 子命令）。

## 11. 不在本文范围

- DNS 加密的协议选型（DoT vs DoH vs DoQ）— design.md 已定 DoT 首选 + DoH 备选。
- DNSSEC — 国内 ISP 普遍不支持，且 fake-IP 模式下没意义。
- 跨境合规审计 — 由控制面 audit log 输出 `(client, domain, outbound)`，落库后处理。
