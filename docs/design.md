# Leap Gateway — 设计方案 v3 (FeiLian Forwarding Node Sidecar)

> 旁挂在飞连转发节点上的透明分流 sidecar：员工经飞连接入 → 这台节点上的 sing-box 按 geosite-cn / geoip-cn 自动判定 direct / proxy → 直连出 ens18 / 翻墙出机场订阅节点。员工无感。

> v2 → v3 变化：v2 假设一台全新的 VPS 跑 WireGuard 入站 + sing-box 单进程；v3 改成在飞连 (CorpLink) **转发节点**上做 sidecar，飞连本身的 WireGuard inbound / nft 链 / SaaS 推送的配置全部不动。所有员工流量不需要重新接入新 VPN，飞连 SaaS 控制端只改一个 push DNS 字段。

## 1. 目标

- 员工**不需要装额外客户端**，沿用飞连。
- 国内域名 / 国内 IP 段：直连出站，速度与终端直接上网一致。
- 墙外目的地：自动经订阅机场节点出站，员工无感。
- DNS 全程不被 GFW 污染或抢答，分流决策基于域名而非 IP（避免 CDN 误判）。
- **不动飞连本身**——任何对飞连进程 / 配置文件 / `FEILIAN_*` nft 链的修改都会被飞连 SaaS 周期性下发覆盖。

## 2. 转发节点 vs SaaS 控制节点

飞连 (CorpLink, 字节/Volcengine 托管) 是 SaaS 模式：

- **SaaS 控制节点** `https://*.feilian.cn`：托管在云端，负责：员工身份 / 客户端配置 / 各转发节点的 ACL / push DNS / NAT 池配置。
- **转发节点**（如生产 192.168.70.246 / 测试节点）：本地服务器，跑 `feilian-vpn` / `feilian-vpn-proxy` / `feilian-vpn-sentry` / `feilian-tun@tun0`，从 SaaS 拉配置（`/opt/feilian/vpn/conf/`）。员工 WireGuard 客户端拨进这里，流量从这里出。

**Leap Gateway 部署在转发节点上做 sidecar**，与飞连进程并存：

| 资源 | 飞连占用 | leap 占用 |
|---|---|---|
| 文件系统 | `/opt/feilian/` | `/etc/leap/` `/usr/local/bin/` `/var/lib/leap/` |
| systemd | `feilian-*.service` | `leap-*.service` |
| nft table | `table ip nat` (`FEILIAN_*` chains) | `table inet leap`（独立 family / 独立名字） |
| 网络接口 | `tun0` (10.8.x.0/24) | `utun-leap`（sing-box TUN inbound） |
| 监听端口 | `127.0.0.53:53` (resolved), `100.74.x:53` (netbird), Caddy `49987-49989` | `<tun0_gw>:53` (sing-box DNS), `127.0.0.1:9090` (clash-api) |

灰度方式：用户在飞连 SaaS 新加一台测试转发节点（独立客户端池如 `10.8.11.0/24`），整个 PoC 期生产 246 不动。

## 3. 总体架构

```
员工设备                            飞连转发节点 (新增的 PoC)
   │                                ┌────────────────────────────────────────┐
   │ 飞连客户端 (CorpLink)          │                                        │
   │ push DNS = <tun0_gw> / 223.5.5.5                                        │
   ├──飞连 WireGuard──────────────►│ tun0 (10.8.11.0/24) ←──不动             │
   │                                │   │                                    │
   │ AllowedIPs = 全局              │   │ leap-prerouting (table inet leap)  │
   │                                │   │   iif tun0, src 10.8.11.0/24       │
   │                                │   │     udp dport 53 → accept          │
   │                                │   │     others       → mark 0x42       │
   │                                │   ▼                                    │
   │                                │  ip rule fwmark 0x42 → table 100       │
   │                                │  table 100: default dev utun-leap      │
   │                                │   │                                    │
   │                                │   ▼                                    │
   │                                │ ┌──── sing-box (新) ───────────────┐   │
   │                                │ │ DNS server <tun0_gw>:53          │   │
   │                                │ │   fake-IP 198.18.0.0/15          │   │
   │                                │ │ TUN inbound utun-leap            │   │
   │                                │ │   sniff = true                   │   │
   │                                │ │ rule_set: geosite-cn, geoip-cn   │   │
   │                                │ │ outbound:                        │   │
   │                                │ │   - direct (默认/CN)             │   │
   │                                │ │   - selector "out" → urltest(机场)│  │
   │                                │ └──┬───────────────────────────────┘   │
   │                                │    │                                   │
   │                                │    ├─ direct ─► ens18 ──► 国内 / 内网  │
   │                                │    └─ proxy  ─► ens18 ──► 机场 ─► 墙外 │
   │                                │                                        │
   │                                │ 飞连原有 (并存，不交叉)                │
   │                                │   FEILIAN_VPN_POSTROUTING_POOL         │
   │                                │     (10.8.11.0/24 → MASQUERADE) ─ 旁置  │
   │                                │   FEILIAN_PROXY                        │
   │                                │     (daddr=tun0_gw 80/443 → Caddy)     │
   │                                └────────────────────────────────────────┘
```

要点：

- **入站**：飞连负责，sing-box 不参与员工握手。
- **流量接管**：`table inet leap` 在 prerouting hook (priority `mangle`/-150) 上把 `iif tun0 + 10.8.11.0/24` 的非 53 流量打 fwmark 0x42；ip rule 把这条 fwmark 的流量送到自定义路由表 100；表 100 的 default 走 `utun-leap`；sing-box TUN inbound 接收。
- **DNS**：`udp dport 53` 在 leap-prerouting 直接 accept（不打标），随后在 sing-box DNS server 上由飞连默认路由命中——sing-box 监听 `<tun0_gw>:53`。
- **分流决策**：sing-box `route` 按 `geosite-cn` / `geoip-cn` rule_set 判，命中 cn → direct；否则 → selector `out` → urltest 选机场节点。
- **出站协议**：sing-box outbound 由控制面拉机场订阅、解析（clash/singbox/uri/sip008）、渲染。
- **观测**：sing-box clash-api `127.0.0.1:9090`；leap 控制面 `/healthz` `/api/status` `/api/nodes`。

## 4. 飞连原有链不动的理由 + 共存性

| 原有资源 | 我们碰不碰 | 解释 |
|---|---|---|
| `feilian-vpn.service` 等 4 个 unit | 不碰 | 飞连主进程，SaaS 下发的 systemd 文件 |
| `/opt/feilian/vpn/conf/*` | 不碰 | 文件时间戳显示 SaaS 周期同步 |
| `table ip nat` 里的 `FEILIAN_*` 链 | 不碰 | 飞连负责员工出口 MASQUERADE 和 80/443 L7 hook |
| `tun0` 接口 | 不碰，但**依赖**它存在 | sing-box TUN 是另一个独立接口 utun-leap |
| systemd-resolved (`127.0.0.53:53`) | 不碰 | 节点本机用 |
| netbird (`100.74.x:53`) | 不碰 | mesh DNS |

**为什么不会 nft 冲突**：飞连用 `table ip nat`（IPv4 nat family），我们用 `table inet leap`（IPv4+IPv6 inet family）；不同 family 的链互不可见，`nft flush table inet leap` 只动我们的，`nft flush table ip nat` 也只动飞连的。

**hook 优先级顺序**：
- prerouting `mangle` -150（我们的 leap-prerouting，打 fwmark）
- prerouting `dstnat` -100（飞连的 PREROUTING / FEILIAN_PROXY，做 REDIRECT）

我们在 dstnat 之前打 fwmark，policy routing 把流量送进 utun-leap，飞连 dstnat 拿到的还是原包但已经"带着 mark + 走 table 100"，REDIRECT 不会触发——因为 sing-box 早把它从 forwarding 路径接走了。

**`FEILIAN_PROXY` 的 80/443 REDIRECT 不冲突**：这条链匹配 `daddr=tun0_gw`（员工连"网关本身"，比如 captive portal 或飞连内置门户）。员工正常上网时 daddr 是远端目标，不命中 FEILIAN_PROXY。

## 5. 分流规则

sing-box `route.rules` 按"先匹配先生效"评估：

```jsonc
"route": {
  "rules": [
    // 1. DNS 流量自己回环到 sing-box 内置 DNS 处理
    { "protocol": "dns", "outbound": "dns-out" },
    // 2. 私网/lan：直连
    { "ip_is_private": true, "outbound": "direct" },
    // 3. CN 域名 / IP：直连
    { "rule_set": ["geosite-cn", "geoip-cn"], "outbound": "direct" }
  ],
  "final": "out",
  "auto_detect_interface": true
}
```

`final: "out"` → selector，selector 默认 `urltest` → 节点池里延迟最低的机场节点。

`rule_set` 用 sing-box 官方 `geosite-cn.srs` / `geoip-cn.srs`，远程加载，每周自动更新。

判定优先级"先域名后 IP"在 fake-IP 模式下尤其关键：fake-IP 给客户端的 IP 永远在 198.18.0.0/15，sing-box TUN 反查回原域名后按域名分流，**绕过 GFW 污染、CDN 边缘错配两个老问题**。

## 6. DNS 子系统 — fake-IP

详见 [dns.md](dns.md)。要点：

- 客户端 push DNS = `<tun0_gw>` (sing-box DNS server)，备 = `223.5.5.5`（容灾）。
- sing-box DNS 收到查询：CN 域名 → 国内 DoH（doh.pub）→ 真 IP；非 CN 域名 → 返回 fake-IP `198.18.x.x`。
- 客户端用 fake-IP 发起连接 → sing-box TUN 收到 → 反查回原域名 → route 按域名分流 → 命中代理 → sing-box 把"原域名 + 端口"传给机场节点（节点端做远端解析，**不在国内做出境 CDN 解析**）。
- 上游 DoH 用 IP literal 或国内 DoH，避免 bootstrap chicken-and-egg。

## 7. 控制面（已有，复用）

`cmd/gateway/main.go` + `internal/{config,subscribe,singbox,api}/`：

```go
mgr := subscribe.NewManagerWithFetch(cfg.Subscriptions, cfg.Subscribe.HTTPTimeout, cfg.Subscribe.UserAgent)
renderer := singbox.NewRenderer(cfg.SingBox)
sbCtl := singbox.NewController(cfg.SingBox.ClashAPI)
go ticker { mgr.Refresh(ctx) → renderer.Write(allOutbounds) → sbCtl.Reload() }
```

P2 阶段渲染器（`internal/singbox/renderer.go`）从"裸 outbound 列表"扩展到完整 sing-box config（DNS / inbounds / route / experimental）；config (`internal/config/config.go`) 加 `node` / `singbox.tun` / `singbox.dns` / `singbox.route` 子段。

订阅源（`internal/config.Subscriptions []SubscriptionEntry`）已经是 yaml 数组，URL 任何时候改 yaml + `POST /api/subscribe/refresh` 即可，多订阅原生支持。

## 8. 部署形态

```
/etc/leap/
├── gateway.yaml         # leap 控制面配置
├── singbox/
│   └── config.json      # 由控制面渲染
└── nft.conf             # table inet leap + ip rule + ip route 模板

/usr/local/bin/
├── sing-box             # pin 到 1.10+ 版本
└── leap-gateway

/var/lib/leap/
├── geoip-cn.srs         # 周期更新
├── geosite-cn.srs
└── sb-cache.db          # fake-IP 持久化（independent_cache）
```

systemd unit：

| Unit | 角色 | 依赖 |
|---|---|---|
| `leap-singbox.service` | sing-box 数据面 | `BindsTo=feilian-tun@tun0.service`（tun0 起来才能监听 `<tun0_gw>:53`） |
| `leap-gateway.service` | 控制面（订阅 / 渲染 / `systemctl restart leap-singbox` 触发 reload） | `Wants=leap-singbox.service` |
| `leap-nft.service` (oneshot) | 注 `table inet leap` + ip rule + ip route | `BindsTo=feilian-tun@tun0` + `Wants=leap-singbox` + `PartOf=leap-singbox`（sing-box 重启时跟着 restart 把内核回收的 utun-leap 路由重新注入） |

详见 [deploy.md](deploy.md)。

## 9. 选型决策记录

- **不再装 WireGuard**：飞连已经是 WireGuard 二次开发，再装一份新 wg0 就和飞连同级冲突；现在改成"飞连 inbound 完整保留 + sing-box 处理 forward 流量"。
- **sing-box 不选 mihomo / dae**：
  - mihomo 协议覆盖类似但 fake-IP + DNS 路由 + clash-api 在 sing-box 上更稳定。
  - dae 用 eBPF 性能好，但 OS 内核要求高（5.17+），飞连节点 5.15 不达标。
- **fake-IP 不选 redir-host**：redir-host 只能基于 IP 决策，CDN 误判频发。
- **不再需要 BGP**：员工流量已经经过飞连转发节点，不需要在网络层 hijack。
- **DNS 上游必须 DoH/DoT**：节点实测 Cloudflare DoT (`1.1.1.1:853`) 直通；腾讯 doh.pub 对 google.com 返回真实 IP（不污染）。Google DoT (`dns.google:853`) 被墙，跳过。

## 10. 安全考量

- **客户端身份**：飞连负责，sing-box 不存员工身份。
- **管理 API**：默认绑 `127.0.0.1`，强制 token 鉴权（外部用 nginx 反代 + mTLS）。
- **订阅 URL 含 token**：磁盘加密 + 日志脱敏（控制面已实现）。
- **sing-box clash-api**：绑 `127.0.0.1`，仅控制面访问。
- **审计日志**：sing-box `cache_file` 保存 fakeip 映射；可选记录 `(client_ip, domain, outbound)` 到追溯日志。

## 11. 不在范围

- 用户级/部门级分流（不同员工走不同节点池）—— 后续工程项。
- 多机房高可用 / Anycast —— PoC 单节点。
- 流量整形 / 限速 —— 暂不需要。
- DPI 审计 —— 不做。
- 切到生产 192.168.70.246 —— 在 PoC 通过后单独评估，不在本期范围。

## 12. 风险与缓解 Register

| ID | 风险 | 影响 | 缓解 |
|---|---|---|---|
| H1 | 飞连客户端 DNS push 不是系统级覆盖（per-app DNS 或 split-DNS） | 大量查询绕过我们的 DNS，fake-IP 失效 | Stage 0 Pre-Implementation Validation 实测；不通则切 sing-box 真解析 + TUN sniff |
| H2 | 飞连 SaaS 周期性重置节点（推送 / 重启 / nft flush） | 我们的服务和规则被冲掉 | systemd `Restart=always`；leap-nft.service 检测到丢失自动重注；独立 `table inet leap` 至少不和飞连冲突 |
| M3 | 改 `rp_filter` 等内核参数副作用 | 飞连 L4 proxy 异常 | 改前实测飞连业务，改 sysctl 加 dropin 文件可回滚；首选 `rp_filter=2`（loose）兼容 fwmark routing |
| M4 | 飞连 SaaS 频繁重启 tun0 | 员工每次断网 30s（leap 跟着 BindsTo 重启） | 监控重启频率；如果太频繁用 `Wants=` 替代 `BindsTo=` |
| M5 | 机场节点全员被关联封锁 | 海外业务静默失败（fallback direct 上不去） | `/api/health` 暴露存活节点数；接监控告警 |
| L1 | fake-IP 198.18.0.0/15 段冲突 | 客户端可能误连真业务 IP | 部署前 `nmap` 扫这段确认无冲突 |
| L2 | clash-api 跨版本兼容 | 控制面 API 可能 break | 在部署脚本里 pin sing-box 版本，写进 docs |
| L3 | 客户端旧 fake-IP 缓存 | sing-box 重启后旧映射不在 | `cache_file.store_fakeip: true` 落盘；不可恢复时 fall through 到 selector |

H1 / H2 是高风险，必须在写代码之前用 [deploy.md](deploy.md) 的 **Pre-Implementation Validation** 章节做 24-48h 实测。

## 13. 里程碑

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P1** ✅ | 控制面骨架 + 订阅拉取/解析 + sing-box 渲染（裸版本） | `cmd/selftest --url <yuyun> --render /tmp/sb.json` 可渲染 |
| **P2** | 渲染器扩 fake-IP DNS / TUN inbound / route rule_set / selector outbound；测试节点观察 spike (H1/H2)；本地 lxc lab；测试节点完整部署 | 测试机走 PoC 节点，`curl https://www.google.com` 通且 outbound=urltest，`curl https://www.baidu.com` 通且 outbound=direct |
| **P3** | 监控告警（机场节点存活 / 飞连 SaaS 推送频率 / 分流准确率）；24-48h 稳定性观察 | 1 周内 leap-singbox uptime ≥ 99% |
| **P4** | 决定是否切到生产 246 | 由用户拍板，超出本期范围 |
