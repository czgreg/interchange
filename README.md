# Leap Gateway

FeiLian (CorpLink) 转发节点上的透明分流 sidecar。员工通过飞连 VPN 接入后，流量经本节点按 geosite-cn / geoip-cn + 白名单规则自动分流：国内直连 / 海外走机场订阅节点。**员工无感，不需要额外客户端。**

## 架构（当前）

```
员工设备 (10.8.x.x)
    │ 飞连 VPN
    ▼
tun0 (FeiLian interface)
    │
    ├─ DNS 查询 (dport 53) ─────────► mihomo DNS server (fake-IP)
    │                                   返回 198.18.x.x 给国内域名 / 真实 IP 给内网
    │
    └─ TCP/UDP 非 DNS ───────────────► iptables TPROXY → mihomo :7893
                                        保留真实 srcIP        │
                                       (10.8.x.x 可见)       ├─ CN → DIRECT → ens18
                                                              ├─ 白名单 → per-terminal → 机场节点
                                                              └─ 非白名单 → DIRECT
```

## 核心组件

| 服务 | 职责 |
|---|---|
| `leap-gateway` | 控制面：订阅管理 / 白名单 / NodeScorer / 配置渲染 / REST API |
| `leap-mihomo` | 数据面：mihomo（DNS + 透明代理 + 流量分类 + 机场出站） |
| `leap-nft` | iptables TPROXY 规则 + ip rule/route（iproute.sh 管理） |

## 关键机制

**TPROXY**：`xt_TPROXY` 在 mangle PREROUTING 拦截 tun0 非 DNS 流量，转交 mihomo:7893，保留真实客户端 IP（10.8.x.x）。这是 per-terminal 路由的硬前提；TUN 模式（system/gvisor）无法保留源 IP。

**fake-IP DNS**：mihomo 接管 DNS，对白名单域名返回 198.18.x.x fake-IP；代理出站时内部映射还原真实域名。CN 域名走 CN DoH，海外域名走 proxy DoH，nameserver-policy 单查询无双发。

**per_terminal 路由**：`load_balance.per_terminal: true` 时，mihomo 规则里按客户端子网 `/32` 切片，每个终端 HRW 分配到固定出口节点。解决"ChatGPT 一个会话的多个子域名（chatgpt.com / chat.openai.com / cdn.openai.com）哈希到不同节点触发 CF 风控"。

**NodeScorer**：每 60s 对候选节点打分（RTT p50/p95/jitter + fail_rate + site probe），不合格节点热重载踢出 us-pool。Site probe 检测 `cf-mitigated: challenge` header（CF challenge 即使返回 200 也标记为失败）。

## 快速开始

```bash
# 依赖：Go 1.22+, make
git clone <this-repo>
cp configs/gateway.example.yaml gateway.yaml
# 编辑 gateway.yaml：订阅 URL + node.client_subnet / tun0_gateway_ip / egress_iface

# 首次部署（一次性 SSH 设置，之后免密）
make setup-ssh NODE=dianwei@192.168.70.89

# 部署
make deploy-89          # staging 89
make deploy-92          # production 92

# 日常
make status             # /api/status
make nodes              # /api/nodes/health
make logs               # leap-gateway 日志
```

## 关键配置（gateway.yaml）

```yaml
data_plane:
  tun:
    stack: gvisor        # REQUIRED for per_terminal (preserves real srcIP)
    tproxy_port: 7893    # TPROXY inbound port
  route:
    mode: whitelist      # whitelist | overseas
    whitelist:
      geosites: [geosite-google, geosite-openai, geosite-anthropic, ...]

load_balance:
  per_terminal: true     # pin each terminal to a single egress node

node_qualify:
  probes:
    - name: openai-api
      url: https://api.openai.com/v1/models
      check: { max_status: 401, reject_cf_challenge: true }
```

## 目录结构

```
cmd/
  gateway/        控制面进程入口
  selftest/       订阅拉取 + 渲染离线验证 CLI
  render-mihomo/  mihomo 配置渲染 CLI（调试）
internal/
  dataplane/      Controller（systemctl / health）
  mihomo/         mihomo YAML 渲染器（DNS/TUN/TPROXY/rules/groups/probes）
  nodescorer/     NodeScorer（RTT 评分 / site probe / passive stats）
  subscribe/      订阅拉取 + 解析
  leaphttp/       HTTP client（proxy-then-direct，防 fakeip 污染）
  config/         配置 schema + 默认值
  api/            REST API
deploy/node/      节点部署文件（systemd / nft.conf.tmpl / iproute.sh / install.sh）
scripts/
  deploy.sh       build + push + validate + restart
  stage.sh        全量部署 tarball
  stress.sh       单机压测（throughput / latency ceiling）
  setup-ssh.sh    一次性 SSH key + NOPASSWD sudoers
docs/
  api.md          REST API 文档（含前端接入变更日志）
  dns.md          DNS 子系统设计
```

## API 文档

[docs/api.md](docs/api.md) — 节点健康、订阅、白名单、UX 遥测，含前端接入变更日志。

## 节点

| 节点 | 子网 | 角色 | 状态 |
|---|---|---|---|
| 192.168.70.89 | 10.8.13.0/24 | 灰度 staging | TPROXY + per_terminal 验证通过 |
| 192.168.70.92 | 10.8.12.0/24 | 生产 production | 等通知后部署 |
