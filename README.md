# Leap Gateway

旁挂在飞连 (CorpLink) **转发节点**上的透明分流 sidecar：员工经飞连接入 → 这台节点上的 leap-gateway 按 geosite-cn / geoip-cn 自动判定 → 国内目的直连出 ens18 / 国外目的经机场订阅节点出墙。**员工无感**，飞连 SaaS 控制端只需要在 push DNS 字段里填我们这台节点上 sing-box DNS 的监听 IP。

详细方案见 [docs/design.md](docs/design.md)、[docs/dns.md](docs/dns.md)、[docs/deploy.md](docs/deploy.md)。

## 环境约束（重要）

- 这个项目部署在**飞连 SaaS 控制端管辖的转发节点**上，不是独立 VPN 服务器。
- 飞连 SaaS（`https://*.feilian.cn`，字节/Volcengine 托管）会周期性向转发节点下发配置（`proxy.conf` / `tun0.conf` / nft `FEILIAN_*` 链等）。
- **不动飞连本身**：所有 leap 文件落 `/etc/leap/`、`/usr/local/bin/`、`/var/lib/leap/`，nft 用独立 `table inet leap`，systemd unit 独立 (`leap-*.service`)。
- 灰度方式：用户在飞连 SaaS 新加一台测试转发节点，跟生产隔离，PoC 期员工流量不动。

## 架构概览

```
员工终端 ──飞连客户端──► 飞连 tun0 ──┬─ geosite-cn / geoip-cn 命中 → direct → 出 ens18
                                     └─ 其它                       → urltest 选订阅节点 → 出墙
```

DNS 用 sing-box fake-IP 模式：客户端拿到 198.18.x.x，sing-box TUN 反查域名后按 geosite-cn 分流。详见 [docs/dns.md](docs/dns.md)。

## 快速开始（控制面，本地）

```bash
# 1. 准备配置
cp configs/gateway.example.yaml configs/gateway.yaml
# 编辑 configs/gateway.yaml，填订阅 URL + 节点参数（client_subnet / tun0_gateway_ip / egress_iface）

# 2. 本地开发跑一次
go test ./...
go build ./...
go run ./cmd/gateway --config configs/gateway.yaml

# 3. 验证订阅 + 渲染
curl -X POST http://127.0.0.1:18080/api/subscribe/refresh
sing-box check -c /etc/leap/singbox/config.json
```

完整测试节点部署（leap-singbox + 独立 nft + 飞连 SaaS DNS push）走 [docs/deploy.md](docs/deploy.md)。

## 自测

```bash
# 全单测
go test ./...

# 订阅 / 解析端到端：拉取 → 解析 → 过滤伪节点 → 渲染 sing-box config
go run ./cmd/selftest --url "https://your.airport/subscribe?token=..." \
  --ua "sing-box/1.8.0" --render /tmp/sb.json
sing-box check -c /tmp/sb.json

# 离线 replay（用之前抓下来的 body 验证解析变化）
go run ./cmd/selftest --file /tmp/saved-body.json --format singbox
```

## 管理 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | 存活检查 |
| GET | `/api/status` | 订阅状态、节点数、sing-box 健康 |
| GET | `/api/nodes` | 当前所有出站节点 |
| GET | `/api/proxies/active` | 节点全景：node + feilian + leap services + 当前机场 + watchdog |
| GET | `/api/whitelist` | 当前白名单 (mode / geosites / domain_suffix) |
| PUT | `/api/whitelist` | 整体替换白名单（geosites 必须 ⊆ available；写 yaml + 重启 sing-box） |
| GET | `/api/geosites` | `available` (本地 .srs 文件) + `active` (yaml 里启用的) |
| GET | `/api/subscriptions` | 订阅列表，URL token 自动打码 |
| POST | `/api/subscriptions` | 新增 `{name, url}` |
| PUT | `/api/subscriptions/{name}` | 改 URL |
| DELETE | `/api/subscriptions/{name}` | 删 |
| POST | `/api/subscribe/refresh` | 立即拉取 + 渲染 + 重载 sing-box |

如果配置了 `api.token`，所有 `/api/*` 请求需带 `Authorization: Bearer <token>`。

WL / 订阅的写操作都会原子写回 `/etc/leap/gateway.yaml` 并 `systemctl restart leap-singbox`（5-10s 中断）。yaml 的注释会丢失（go-yaml v3 round-trip 限制），介意请只读不写。

### 常用 curl 一键查

```bash
TOK=$(grep -oE 'token: "\S+"' /etc/leap/gateway.yaml | head -1 | awk -F'"' '{print $2}')
H="Authorization: Bearer $TOK"   # 没配 token 就别传 -H

# 节点全景：飞连服务、leap 服务、机场、watchdog
curl -s -H "$H" 127.0.0.1:18080/api/proxies/active | python3 -m json.tool

# 看现在白名单里有什么
curl -s -H "$H" 127.0.0.1:18080/api/whitelist | python3 -m json.tool

# 看本地有哪些 geosite 可选 (PUT WL 时必须从这里挑)
curl -s -H "$H" 127.0.0.1:18080/api/geosites | python3 -m json.tool

# 加 / 删一条 domain_suffix —— 整体替换语义，要先 GET 拿到现状再 PUT
curl -s -H "$H" 127.0.0.1:18080/api/whitelist | jq '.domain_suffix += ["new-site.com"]' \
  | curl -s -H "$H" -H "Content-Type: application/json" -X PUT 127.0.0.1:18080/api/whitelist -d @-
```

## 节点运维速查（在部署节点上跑）

> 端口约定：leap-gateway 控制面 `127.0.0.1:18080`，sing-box clash-api `127.0.0.1:9090`，sing-box DNS = `<tun0_gateway_ip>:53`（PoC 节点 = `10.8.12.1`）。

### 看现在选了哪个机场

```bash
# 现在 urltest 选中的机场节点 + 最近一次延迟
curl -s 127.0.0.1:9090/proxies/urltest \
  | python3 -c 'import sys,json; d=json.load(sys.stdin); h=d.get("history",[]); print("now:", d.get("now")); print("last delay:", h[-1] if h else None)'

# 列出 urltest 池里的节点（只含按 node_pattern 过滤后的，比如新加坡+美国）
curl -s 127.0.0.1:9090/proxies/urltest | python3 -c 'import sys,json; print(*json.load(sys.stdin).get("all",[]), sep="\n")'

# 列出订阅里所有机场节点（含被过滤掉的）—— 先看 urltest 之外哪些 outbound 也被解析进来
curl -s 127.0.0.1:9090/proxies | python3 -c 'import sys,json; d=json.load(sys.stdin); print(*[k for k in d.get("proxies",{}) if k.startswith("yuyun/") or k.startswith("airport/")], sep="\n")'

# 按地区统计可用节点数（在 urltest 池里）
curl -s 127.0.0.1:9090/proxies/urltest | python3 -c '
import sys,json,re
nodes = json.load(sys.stdin).get("all",[])
buckets = {}
for n in nodes:
    short = n.split("/",1)[1] if "/" in n else n
    m = re.match(r"(\S+\s*\S+?)\s", short)
    key = m.group(1).strip() if m else short[:14]
    buckets[key] = buckets.get(key,0) + 1
print("total:", len(nodes))
for k,v in sorted(buckets.items(), key=lambda x:-x[1]): print("  %3d %s" % (v,k))
'

# 触发一次延迟测速（强制 urltest 重新选节点 —— watchdog 失败超阈值时也是调用这个接口）
curl -s --max-time 30 'http://127.0.0.1:9090/proxies/urltest/delay?url=https%3A%2F%2Fwww.gstatic.com%2Fgenerate_204&timeout=5000'
```

### 调整选节点策略

`/etc/leap/gateway.yaml` 的 `singbox.urltest` 段控制选节点行为：

```yaml
singbox:
  urltest:
    interval:    3m                              # sing-box 全量重测周期
    tolerance:   50                              # 切换迟滞 (ms)
    probe_url:   "https://www.gstatic.com/generate_204"  # 测速 URL，必须翻墙
    node_pattern: "新加坡|美国"                    # 入池节点的 tag 正则；空 = 全部
    watchdog:
      enabled:        true
      interval:       5s                         # 主动探测当前节点的频率
      timeout:        3s
      fail_threshold: 3                          # 连续失败几次后强制重选
```

改完跑 `sudo systemctl restart leap-gateway` 即可生效（leap-gateway 会重渲染 sing-box config 并 systemctl restart leap-singbox）。watchdog 只对失败/状态变化打日志，稳态下安静。看 watchdog 是否在干活：

```bash
sudo journalctl -u leap-gateway --since '10 minutes ago' --no-pager | grep watchdog
# 期望见到 "watchdog started" 一次 + 选节点变化时一次 "selection changed"
```

### 看实时分流（哪些访问走机场，哪些直连）

```bash
# 当前活跃连接，按目标 + 出口链路汇总
curl -s 127.0.0.1:9090/connections | python3 -c '
import sys,json
d = json.load(sys.stdin)
seen = {}
for c in d.get("connections",[]):
    md = c.get("metadata",{})
    host = md.get("host") or md.get("destinationIP")
    chain = tuple(c.get("chains",[]))
    seen[(host,chain)] = seen.get((host,chain),0)+1
for (host,chain),n in sorted(seen.items()):
    print("%3d  %-30s  %s" % (n, host, list(chain)))
'
# chain=["direct"]                       → 国内 direct 出 ens18
# chain=["yuyun/...", "urltest", "out"] → 海外 经机场翻墙
```

### 触发订阅刷新

```bash
# 改完 /etc/leap/gateway.yaml 后立即生效（会重启 sing-box，5-10s 中断）
curl -s -X POST http://127.0.0.1:18080/api/subscribe/refresh

# 看 leap-gateway 实际拉到了多少节点
journalctl -u leap-gateway -n 20 --no-pager | grep refresh
```

### 服务状态 / 日志

```bash
sudo systemctl status leap-singbox leap-gateway leap-nft
sudo journalctl -fu leap-singbox          # sing-box 数据面实时
sudo journalctl -fu leap-gateway          # 订阅刷新 / 渲染日志
```

### DNS 自测（从节点本机）

```bash
dig +short @10.8.12.1 baidu.com    # 期望: 真 IP (111.63.x / 153.3.x)
dig +short @10.8.12.1 google.com   # 期望: fake-IP (198.18.x.x)
```

### 路由 / nft 健康

```bash
sudo nft list table inet leap | head -10              # 期望 3 条规则在 prerouting
ip rule | grep 0x42                                   # 期望: 100: from all fwmark 0x42 lookup 100
ip route show table 100                               # 期望: default dev utun-leap

# 飞连原 nft 链没动
sudo nft list table ip nat | grep -E 'FEILIAN_(PROXY|VPN_POSTROUTING)'
```

### 紧急停机（员工自动 fallback 到备 DNS 223.5.5.5）

```bash
sudo systemctl stop leap-singbox leap-gateway leap-nft
# 国内站继续正常；海外站超时但不全断。改回飞连 SaaS 主 DNS 为 223.5.5.5 更保险
```

### 升级 / 重新部署（在操作机上）

```bash
# 改完代码 / gateway.yaml 后重新打包 + 推送 + 装
./scripts/stage.sh
scp build/leap-stage.tgz dianwei@<node>:/tmp/
ssh dianwei@<node> 'cd /tmp && tar -xzf leap-stage.tgz && sudo bash leap-stage/install.sh'
# install.sh 幂等，会原地覆盖 binary 并 daemon-reload + 重启服务
```

## 项目结构

```
cmd/
  gateway/          控制面 main 入口
  selftest/         订阅 / 渲染自测 CLI
internal/
  config/           配置加载
  subscribe/        订阅拉取与解析（Clash / sing-box / V2Ray / SS）
  singbox/          sing-box 配置渲染与进程管理（systemctl restart 触发 reload）
  api/              HTTP 管理 API
configs/            配置模板
deploy/             节点侧部署脚本（systemd unit + nft 模板）
docs/
  design.md         整体架构与决策、风险登记
  dns.md            fake-IP 策略与 GFW 污染对抗
  deploy.md         转发节点 sidecar 部署手册（含 Pre-Implementation Validation）
```

## 里程碑

- **P1（已完成）**：订阅拉取 + sing-box 渲染骨架 + 控制面 API + 自测
- **P2（进行中）**：渲染器扩 fake-IP DNS / TUN inbound / route rule_set / selector outbound；测试节点观察 spike (H1/H2)；本地 lxc lab 验证；测试节点完整部署
- **P3**：稳定性观察 + 监控告警（机场节点存活、飞连 SaaS 推送频率）
- **P4**：决定是否切到生产 246（不在本期范围）
