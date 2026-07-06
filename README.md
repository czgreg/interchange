# Leap Gateway

FeiLian (CorpLink) 转发节点上的透明分流 sidecar。员工通过飞连 VPN 接入后，流量经本节点按 geosite-cn / geoip-cn + 白名单规则自动分流：国内直连 / 海外走机场订阅节点。**员工无感，不需要额外客户端。**

## 架构

```
员工设备 (10.8.x.x)
    │ 飞连 VPN
    ▼
tun0 (FeiLian interface)
    │
    ├─ DNS 查询 (dport 53) ─────────► mihomo DNS server (redir-host)
    │                                   返回真实 IP（CN/内网 via CN DoH，海外 via proxy DoH）
    │
    └─ TCP/UDP 非 DNS ───────────────► iptables TPROXY → mihomo :7893
                                        保留真实 srcIP        │
                                       (10.8.x.x 可见)       ├─ CN → DIRECT → ens18
                                                              ├─ 白名单 → fb-<ip> 组 → 机场节点
                                                              └─ 非白名单 → DIRECT
```

## 核心组件

| 服务 | 职责 |
|---|---|
| `leap-gateway` | 控制面：订阅管理 / 白名单 / NodeScorer / 池决策 / 配置渲染 / Lark 通知 / REST API |
| `leap-mihomo` | 数据面：mihomo（DNS + 透明代理 + 流量分类 + 机场出站） |
| `leap-nft` | iptables TPROXY 规则 + ip rule/route（iproute.sh 管理） |

## 关键机制

**TPROXY**：`xt_TPROXY` 在 mangle PREROUTING 拦截 tun0 非 DNS 流量，转交 mihomo:7893，保留真实客户端 IP（10.8.x.x）。这是 per-terminal 路由的硬前提；TUN 模式（system/gvisor）无法保留源 IP。

**redir-host DNS**（2026-07-06 起，替代 fake-IP）：mihomo 接管 DNS，所有域名返回真实 IP，不做全局劫持。路由不依赖 DNS——sniffer 从 TLS SNI / HTTP Host / QUIC 提取域名（含 pure-IP 流量），geosite 规则据此分类。CN 域名走 CN DoH，海外域名走 proxy DoH（经 us-pool 解析、绕过 GFW 污染），nameserver-policy 单查询无双发。切换动机：fake-IP 的"默认劫持 + 名单豁免"模型下，任何漏配的域名（内网服务、机场节点 server 域名）都会拿到 198.18.x.x 假 IP 后超时——2026-07-06 因订阅换商漏配 `quandao.com` 导致全节点探测 `alive=False`。redir-host 从根上消除该故障模式：节点 server 域名与内网域名自动拿真实 IP，零维护。

**Per-terminal fallback group (fb-\<ip\>)**：`load_balance.per_terminal: true` 时，每个客户端 IP 在 mihomo 配置里有专属 `fallback` proxy-group 命名 `fb-<ip>`，含 HRW top-2 [primary, secondary]。SRC-IP-CIDR 规则把该 IP 的流量路由到这个 fallback 组——mihomo 内置的 alive-bit 失败自动切到 secondary（30s 内），不需要 leap-gateway 干预。一个终端的所有目的地共享同一 egress（CF / OpenAI 反检测要求）。

**NodeScorer + K-gating**：每 5 min 对所有候选节点打分（`p95 + 2×jitter + 5000×fail²`），按 long EWMA (24h 半衰期) 排名选 top-K 进池。新节点 24h trial 期不能晋升；rollback 后 1h 隔离不被回填；非池→池的换血需绝对 score 差 > `swap_threshold_score` (默认 100) 防抖。`fail_rate ≥ 0.9` 是 catastrophic gate（直接 `Qualified=false`，10 min 内自然剔出）。

**通知（Lark webhook）**：池组成变化（auto_swap/rollback）、紧急事件（emergency_evict）走 HMAC-SHA256 签名的 Lark webhook。本地 JSONL 日志兜底（webhook 挂了仍能查历史）。dedup（5 min 同 node|type）+ 限速（info 单节点每小时 4 条）+ 聚合（30 s 多事件合并）。Bootstrap 重启不发 Lark（噪音抑制）。

## 快速开始

```bash
# 依赖：Go 1.22+, make
git clone <this-repo>
cp configs/gateway.example.yaml gateway.yaml
# 编辑 gateway.yaml：订阅 URL + node.client_subnet / tun0_gateway_ip / egress_iface

# 首次部署（一次性 SSH 设置，之后免密）
make setup-ssh NODE=dianwei@192.168.70.89

# 部署
make deploy-89          # 灰度 89
make deploy-92          # 生产 92

# 配置 Lark 通知（可选）
curl -X POST -H "Content-Type: application/json" \
  -d '{"webhook_url":"https://open.feishu.cn/open-apis/bot/v2/hook/xxx",
       "secret":"<lark-bot-secret>", "signature_required":true}' \
  http://192.168.70.89:18080/api/notifications/lark

# 日常
make status             # /api/status — engine/池/容量/24h 稳定度
make nodes              # /api/nodes/health
make logs               # leap-gateway 日志
```

## 关键配置（gateway.yaml）

```yaml
data_plane:
  tun:
    stack: system        # system or gvisor — does NOT affect TPROXY source IP
  tproxy_port: 7893      # TPROXY inbound port — REQUIRED (>0)
  route:
    mode: whitelist      # whitelist | overseas
    whitelist:
      geosites: [geosite-google, geosite-openai, geosite-anthropic, ...]

load_balance:
  per_terminal: true     # 每 terminal 一个 fb-<ip> fallback 组

node_qualify:
  pool_mode: auto                # auto | manual
  swap_threshold_score: 100      # 候选 long EWMA 须比池内最差好 ≥100 才换
  pool_sizing:
    t_active: 50                 # 操作员手填：7 天活跃 terminal 数
    cap_per_node: 10             # 单节点稳定承载 terminal 数
    # K = clamp(⌈T × 1.5 / cap⌉, K_min(T), 12) → 50/10 → K=8
  probes:                        # CF-aware site probes
    - name: openai-api
      url: https://api.openai.com/v1/models
      check: { max_status: 401, reject_cf_challenge: true }

notifications:
  enabled: true
  lark:
    webhook_url: ""              # 通过 API 设置，yaml 不存 secret
    signature_required: true
    secret_file: /var/lib/leap/lark-secret
  dedup_window: 5m
  per_node_rate_limit_per_hour: 4
```

## 目录结构

```
cmd/
  gateway/        控制面进程入口
  selftest/       订阅拉取 + 渲染离线验证 CLI
  render-mihomo/  mihomo 配置渲染 CLI（调试）
internal/
  dataplane/      Controller（systemctl / health）
  mihomo/         mihomo YAML 渲染器（DNS/TUN/TPROXY/rules/groups/per-terminal fallback）
  nodescorer/     评分 + K-gating + EWMA + transition 审计 + rollback + emergency
  notify/         Lark webhook 客户端 + dedup/限速/聚合 + 本地 JSONL 兜底
  subscribe/      订阅拉取 + 解析（clash YAML + sing-box JSON 两种格式）
  leaphttp/       HTTP client（proxy-then-direct，海外出站经池过墙）
  config/         配置 schema + 默认值
  api/            REST API
deploy/node/      节点部署文件（systemd / nft.conf.tmpl / iproute.sh / install.sh）
scripts/
  deploy.sh       build + push + validate + restart
  stage.sh        全量部署 tarball
  stress.sh       单机压测
  setup-ssh.sh    一次性 SSH key + NOPASSWD sudoers
docs/
  api.md          REST API 文档（25 个端点 + 前端接入变更日志）
```

## API 文档

[docs/api.md](docs/api.md) — 25 个 REST 端点，按职责分组：
- **状态**：`/api/status`、`/api/nodes/health`、`/api/proxies/active`
- **订阅 & 白名单**：`/api/subscriptions/*`、`/api/whitelist*`、`/api/rule-sets`
- **池管理**：`/api/pool/terminal`（按 IP 查路由）、`/api/pool/transitions`（审计）、`/api/pool/rollback`
- **通知**：`/api/notifications/*`

含前端接入变更日志，每次 breaking change 标记。

## 流量路径

### 走本地（不进机场）

| 类型 | 判断依据 | 出口 |
|---|---|---|
| DNS 查询 | nft `dport 53 accept`，不走 TPROXY | mihomo DNS server 本机处理 |
| 国内域名 / IP | `geosite-cn` / `geoip-cn` 命中 | `DIRECT` → ens18 直出 |
| 内网域名 | `fake_ip_skip_suffixes` → nameserver-policy CN DoH | 拿真实 IP + DIRECT 出站，直连内网 |
| 非白名单海外域名 | whitelist 模式 `MATCH,DIRECT` | 直出 ens18（GFW 可能拦） |

### 走机场

| 类型 | 判断依据 | 路径 |
|---|---|---|
| 白名单海外域名 | `geosite-google/openai/anthropic/...` | fb-\<ip\> primary → 出墙 |
| 白名单 IP/CIDR | `geoip-telegram` 等 | us-pool → 出墙 |
| overseas 模式所有非 CN | `MATCH,out` | us-pool consistent-hash → 出墙 |

### 完整包路径

```
员工设备 (10.8.13.x)
    │ 飞连 WireGuard
    ▼
tun0 (FeiLian VPN interface)
    │
    ├── DNS (dport 53) ─────────────────────► mihomo DNS server :53
    │   nft accept，不走 TPROXY               redir-host 返回真实 IP
    │                                         CN/内网 → CN DoH (doh.pub)
    │                                         海外域名 → proxy DoH (1.1.1.1 via 机场)
    │
    └── 其他 TCP/UDP ─────────────────────── iptables TPROXY ──► mihomo :7893
        fwmark 0x44 → table 101 → lo          保留真实 srcIP (10.8.13.x)
                                                       │
                          ┌────────────────────────────┤
                          │                            │
                   geosite-cn 命中             白名单域名命中
                   (国内流量)                  (海外白名单)
                          │                            │
                          ▼                            ▼
                    DIRECT → ens18          SRC-IP-CIDR,10.8.13.5/32,fb-10.8.13.5
                    延迟 = 直连              fb-10.8.13.5: fallback [primary, secondary]
                                                         │ alive=true → primary
                                                         │ alive=false → secondary（30s 内自动切）
                                                         ▼
                                                   机场节点 → 目标网站
```

### 关键保证

- **Per-terminal 一致 egress**：同一终端的 ChatGPT 所有子域名（chatgpt.com / chat.openai.com / cdn.openai.com）全走 fb-\<ip\> 组的同一节点。fallback 组 [primary, secondary] 由 HRW 决定，节点死了自动切，但仍只用一个出口——不触发 CF / OpenAI 的 fan-out 检测。
- **CN 零损耗**：`geosite-cn / geoip-cn` 最前匹配，命中即 DIRECT，不经机场，延迟与直连一致。
- **DNS 防污染**：走机场的海外域名经 proxy DoH 出墙解析，绕过 GFW UDP 53 抢答；redir-host 下路由由 sniffer 从 TLS SNI 提取域名决定，不依赖 DNS 应答 IP。
- **池组成稳定**：双窗 EWMA（4h 短窗 / 24h 长窗）+ swap_threshold + 24h trial 三层防抖；正常情况下日级别才有池变更，与"住宅用户网络"模式一致。

## 节点

| 节点 | 子网 | 角色 | 当前状态 |
|---|---|---|---|
| 192.168.70.89 | 10.8.13.0/24 | 灰度 staging | auto 模式 + Lark 通知，K=8 |
| 192.168.70.92 | 10.8.12.0/24 | 生产 production | auto 模式（Lark 待配），K=8 |

两节点同步运行同一版本（git HEAD），yaml 各自有 4 个订阅源（节点级独立，禁止跨节点同步——见 CLAUDE.md 规则）。
