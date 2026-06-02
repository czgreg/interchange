# Leap Gateway 管理 API

控制面 HTTP API。

| 部署 | base URL |
|---|---|
| **PoC 节点（192.168.70.92）当前** | `http://192.168.70.92:18080` |
| 节点本机 | `http://127.0.0.1:18080` |

监听由 `gateway.yaml` 的 `api.listen` 控制；PoC 阶段是 `0.0.0.0:18080`，对内网开放。
所有端点均为 JSON。

## 认证

> **PoC 阶段未启用 token 鉴权** —— 任何能到达 `192.168.70.92:18080` 的内网设备都可以
> 直接调用，包括写操作（`PUT /api/whitelist`、`POST /api/subscribe/refresh`、订阅 CRUD）。
> 切生产前必须做：把 `api.listen` 收回 `127.0.0.1:18080`，并在 `gateway.yaml` 设
> `api.token: "<32+ char random>"`，前置 nginx 终端 TLS。

如果 `gateway.yaml` 里 `api.token` 非空，所有 `/api/*` 请求必须带：

```
Authorization: Bearer <token>
```

`/healthz` 永远不需要认证（用于 LB / 监控可达性探测）。

错误响应：

| 状态 | 含义 |
|---|---|
| 400 | 请求体非法 / 字段格式错（含 catalog 里没有该 rule-set tag）|
| 401 | token 缺失或不匹配（仅当 `api.token` 非空时可能出现）|
| 404 | 资源不存在（订阅名找不到） |
| 409 | 资源冲突（订阅名已存在） |
| 500 | 持久化失败 / 渲染失败 / sing-box reload 失败 |
| 502 | 上游拉取失败（PUT 白名单时按需下载 .srs 失败 —— 网络 / 上游 5xx）|

写操作（PUT/POST/DELETE）的失败语义：**先校验再落盘**。校验失败请求拒绝，
内存与磁盘均未变；落盘失败时内存已变但磁盘可能未变（重启后恢复一致）。

## 网络可达性

PoC 节点 `192.168.70.92` 的 nft `table inet leap` 在 install 时会渲染：

```
chain input {
  type filter hook input priority filter; policy accept;
  tcp dport 18080 accept            # 不限源 IP（PoC）
}
```

模板在 `deploy/node/nft.conf.tmpl`，端口由 `gateway.yaml` 的 `api.listen` 自动注入。
`leap-nft.service` 重启后规则会从模板 reload，不会丢。

---

## 端点速览

| 方法 | 路径 | 副作用 | 说明 |
|---|---|---|---|
| GET | `/healthz` | 无 | 存活检查 |
| GET | `/api/status` | 无 | 订阅 / 节点数 / sing-box 健康 |
| GET | `/api/nodes` | 无 | 当前所有出站节点（扁平化） |
| GET | `/api/proxies/active` | 无 | 节点全景：node + feilian + leap services + 当前机场 + watchdog |
| POST | `/api/proxies/select` | clash-api PUT `/proxies/out` | 手动切换 `out` selector（urltest-primary / urltest-backup / direct）；非 sticky，watchdog 仍在跑 |
| GET | `/api/whitelist` | 无 | 白名单（mode + geosites + geoips + domain_suffix + ip_cidr） |
| PUT | `/api/whitelist` | 写 yaml + 必要时拉缺失 .srs + 重启 sing-box + 异步重建展开缓存 | 整体替换白名单；catalog 里的 tag 被选中时按需下载 |
| GET | `/api/whitelist/resolved` | 无（首次冷启动会同步拉一次） | rule-set tag → 扁平 `domains[]` + `ip_cidrs[]`。飞连"极速模式"用 |
| GET | `/api/rule-sets` | 无 | 内嵌 catalog（geosite + geoip 各一组 name/url/category）+ 每项 installed/selected + 域名 / IP/CIDR 提示案例 |
| GET | `/api/geosites` | 无 | 兼容别名：仅返回 geosite 部分的 `{available, active}` |
| GET | `/api/subscriptions` | 无 | 订阅列表，URL token 自动打码 |
| POST | `/api/subscriptions` | 写 yaml + 拉订阅 + 重启 sing-box | 新增订阅 |
| PUT | `/api/subscriptions/{name}` | 写 yaml + 拉订阅 + 重启 sing-box | 改 URL |
| DELETE | `/api/subscriptions/{name}` | 写 yaml + 拉订阅 + 重启 sing-box | 删订阅 |
| POST | `/api/subscribe/refresh` | 拉订阅 + 重启 sing-box | 立即刷新 |
| GET | `/api/subscribe/refresh-interval` | 无 | 当前周期刷新间隔（秒） |
| PUT | `/api/subscribe/refresh-interval` | 写 yaml + 重置调度器 | 改间隔；`seconds=0` 关周期，只手动刷新 |

> "重启 sing-box" 实际是 `systemctl restart leap-singbox`，5–10s 中断。
> WL / 订阅写操作会原子写回 `/etc/leap/gateway.yaml`，**yaml 注释会丢失**
> （go-yaml v3 round-trip 限制）。

---

## GET /healthz

存活检查，无认证。

```bash
# 节点本机
curl -s http://127.0.0.1:18080/healthz

# 内网其它机器（PoC 直连）
curl -s http://192.168.70.92:18080/healthz
```

```json
{"ok": true}
```

---

## GET /api/status

订阅与 sing-box 健康概要。

```json
{
  "last_refresh": "2026-05-28T03:54:31.123456789Z",
  "node_count": 66,
  "subscriptions": 1,
  "singbox_ok": true
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `last_refresh` | RFC3339 时间 / 零值 | 最近一次订阅刷新；从未刷新时为 `"0001-01-01T00:00:00Z"` |
| `node_count` | int | 全部订阅解析出来的节点总数（含被 NodePattern 过滤掉的） |
| `subscriptions` | int | 当前 enabled 的订阅数（与配置里 `subscriptions[*].enabled=true` 数量一致） |
| `singbox_ok` | bool | clash-api `/version` 是否 200 |

---

## GET /api/nodes

扁平化的所有解析出来的节点。

```json
[
  {"tag": "yuyun/SG01", "type": "vless", "server": "sg.example.net", "source": "yuyun"},
  {"tag": "yuyun/US02", "type": "trojan", "server": "us.example.net", "source": "yuyun"}
]
```

| 字段 | 说明 |
|---|---|
| `tag` | 在 sing-box 里的全名，前缀是订阅名 |
| `type` | sing-box outbound type（vless / trojan / shadowsocks / ...） |
| `server` | 目标节点 hostname / IP |
| `source` | 该节点来源订阅的 `name` |

---

## GET /api/proxies/active

节点全景，**最常用**。一次拉就能看到节点本身、飞连服务、leap 服务、当前机场节点、watchdog 状态。

```json
{
  "node": {
    "hostname": "dianweiserver",
    "kernel": "5.15.0-179-generic",
    "os": "Ubuntu 22.04.5 LTS",
    "uptime_seconds": 78398,
    "interfaces": [
      {"name": "ens18", "ipv4": "192.168.70.92"},
      {"name": "tun0", "ipv4": "10.8.12.1"},
      {"name": "utun-leap", "ipv4": "172.19.0.1"}
    ],
    "client_subnet": "10.8.12.0/24",
    "egress_iface": "ens18"
  },
  "feilian": {
    "tun0_active": true,
    "vpn_active": true,
    "proxy_active": true,
    "sentry_active": true,
    "nft_chains_present": ["FEILIAN_VPN_POSTROUTING_POOL", "FEILIAN_PROXY"]
  },
  "leap": {
    "gateway_version": "f354d04",
    "singbox_version": "1.10.7",
    "services": {
      "leap-gateway": "active",
      "leap-nft": "active",
      "leap-singbox": "active"
    },
    "subscriptions_count": 1,
    "nodes_parsed": 66,
    "last_refresh": "2026-05-28T03:54:31Z"
  },
  "active_proxy": {
    "now": "yuyun/🇸🇬 新加坡01 境外中转",
    "delay_ms": 154,
    "last_check": "2026-05-28T03:54:52Z",
    "pool_size": 18,
    "pool_filter": "新加坡|美国",
    "history_len": 1,
    "reachable": true,
    "active_urltest": "urltest-primary",
    "pools": [
      {"tag": "urltest-primary", "now": "yuyun/🇸🇬 新加坡01 境外中转", "pool_size": 18, "active": true},
      {"tag": "urltest-backup",  "now": "backup/HK01",                "pool_size": 6,  "active": false}
    ]
  },
  "watchdog": {
    "enabled": true,
    "interval_seconds": 5,
    "fail_threshold": 3,
    "consecutive_fails": 0,
    "consecutive_healthy": 4,
    "last_probe": "2026-05-28T03:55:01Z",
    "current_node": "yuyun/🇸🇬 新加坡01 境外中转",
    "current_selector": "urltest-primary",
    "on_backup": false,
    "skipped_due_traffic": 12,
    "primary_recovery_streak": 0,
    "primary_recovery_threshold": 3
  }
}
```

`active_proxy`：

| 字段 | 说明 |
|---|---|
| `now` | 当前正在路由海外流量的airport节点 tag |
| `delay_ms` | 最近一次延迟测速（来自 sing-box 的 history） |
| `last_check` | 该测速的时间戳 |
| `pool_size` | 当前活跃 urltest 池里的节点数（可被 NodePattern 过滤） |
| `pool_filter` | `singbox.urltest.node_pattern` 正则 |
| `history_len` | sing-box 保留的延迟历史长度 |
| `reachable` | clash-api 是否可达（false 时其它字段是空) |
| `active_urltest` | `out` 选择器当前指向哪个池：`urltest-primary` 或 `urltest-backup`（或 `direct`） |
| `pools[]` | 每个 urltest-* 池的概要：tag / 当前选中节点 / 池大小 / 是否激活 |

`watchdog`：

| 字段 | 说明 |
|---|---|
| `enabled` | 是否启用了主动健康检查 |
| `interval_seconds` | 基础探测周期（5s 默认） |
| `fail_threshold` | 连续失败几次触发切换 |
| `consecutive_fails` | 当前连续失败计数 |
| `consecutive_healthy` | 当前连续健康计数（影响 backoff） |
| `last_probe` | 最近一次探测时间 |
| `current_node` | watchdog 看到的当前 urltest 选中节点 |
| `current_selector` | `out` 当前指向（`urltest-primary` / `urltest-backup`） |
| `on_backup` | 是否已切到备用池 |
| `skipped_due_traffic` | 因为有真实用户流量经过 `out` 而跳过的合成探测累计次数 |
| `primary_recovery_streak` | 在备用池时，主池连续健康探测计数 |
| `primary_recovery_threshold` | 需要多少次连续健康才切回主 |

---

## POST /api/proxies/select

手动把 `out` selector 切到指定池。运维场景：watchdog 自动判定还没触发但运维已经知道
该切了；或者临时 bypass 整条机场链路（`direct`）做对照测试。

请求：

```json
{"selector": "urltest-backup"}
```

合法 `selector`：

| 值 | 含义 |
|---|---|
| `urltest-primary` | 第一条 enabled 订阅的节点池（默认 / 主池） |
| `urltest-backup`  | 其余 enabled 订阅合成的节点池（仅在配 ≥ 2 条订阅时存在） |
| `direct`          | bypass 机场，海外流量直连 |

校验：

- body 缺 `selector` 或值不在上面三选一里 → 400；
- 选 `urltest-backup` 但当前部署只有 1 条订阅（没渲染备池）→ 404 `selector "urltest-backup" not registered on sing-box`；
- clash-api 出错 → 500。

成功返回 200 + 同 `GET /api/proxies/active` 的全量结构（看切换后的实际状态）。

**手动 override 不是 sticky** —— watchdog 一直在后台跑：

| 手动切到 | 机场实际状态 | watchdog 后续行为 |
|---|---|---|
| `urltest-backup` | primary 健康 | `runPrimaryRecovery` 探测主池，连续 `primary_recovery_threshold`（默认 3）次成功后 ~30m × 3 ≈ 1.5h 自动切回 primary |
| `urltest-backup` | primary 真坏 | 维持在 backup，跟自动切换结果一致 |
| `urltest-primary` | primary 真坏 | 探测连续 `fail_threshold`（默认 3）次失败 → 自动切回 backup |
| `direct` | —— | 走直连，没有 watchdog 干预（`out` 不在 urltest-* 里）；什么时候切回去也得人工 |

```bash
# 立即切到备池
curl -sX POST -H 'Content-Type: application/json' \
  -d '{"selector":"urltest-backup"}' \
  http://192.168.70.92:18080/api/proxies/select

# 临时 bypass 机场
curl -sX POST -H 'Content-Type: application/json' \
  -d '{"selector":"direct"}' \
  http://192.168.70.92:18080/api/proxies/select

# 立即切回主池（watchdog 也会自己切，但这条更快）
curl -sX POST -H 'Content-Type: application/json' \
  -d '{"selector":"urltest-primary"}' \
  http://192.168.70.92:18080/api/proxies/select
```

---

## GET /api/whitelist

```json
{
  "mode": "whitelist",
  "geosites": ["geosite-google", "geosite-youtube", "geosite-openai"],
  "geoips": [],
  "domain_suffix": ["claude.ai", "anthropic.com"],
  "ip_cidr": ["149.154.0.0/16", "91.108.0.0/16"]
}
```

| 字段 | 说明 |
|---|---|
| `mode` | `overseas` 或 `whitelist`。`overseas` = 命中 cn 的直连，其余走机场；`whitelist` = 仅命中白名单走机场，其它直连 |
| `geosites` | 启用的 geosite tag 数组（catalog 子集，从 `/api/rule-sets` 取；本地没有的会在 PUT 时按需下载） |
| `geoips` | 启用的 geoip tag 数组（同上）。catalog 里只有 ISO 国家码（`geoip-jp` / `geoip-us` / ...）。"按国家路由"用这里；硬编码 IP 段（Telegram MTProto 这种）走下面 `ip_cidr` |
| `domain_suffix` | 额外的域名后缀列表（小写 + bare domain） |
| `ip_cidr` | 额外的 IP/CIDR 列表（IPv4/IPv6 都接受；裸 IP 自动按 `/32` 或 `/128` 处理）。Telegram MTProto 这种硬编码 IP 段的应用必须走这里 |

## PUT /api/whitelist

**整体替换语义**。改一项也要先 GET 拿现状、改完整体 PUT。

请求：

```json
{
  "mode": "whitelist",
  "geosites": ["geosite-google", "geosite-anthropic"],
  "geoips": [],
  "domain_suffix": ["claude.ai", "openai.com"],
  "ip_cidr": ["149.154.0.0/16", "91.108.0.0/16"]
}
```

校验：

- `mode` 缺省 = 不动当前；显式只接受 `overseas` / `whitelist`。
- `geosites` 必须 ⊆ `/api/rule-sets` 的 `geosites.catalog[].name`，否则 400 `unknown rule-set "..."（not in catalog)`。每条要 `geosite-` 前缀。
- `geoips` 必须 ⊆ `/api/rule-sets` 的 `geoips.catalog[].name`，否则 400。每条要 `geoip-` 前缀。
- `domain_suffix` 每条必须是裸域名（含 `.`、不含 `://` `/` `空白`、首尾无 `.`）。
- `ip_cidr` 每条必须是合法 CIDR 或裸 IP（裸 IP 落盘前归一化成 `/32` 或 `/128`）。
- 重复项自动去重，空字符串忽略。

**按需拉取** —— catalog 校验通过后，对每个 tag 检查 `/etc/leap/singbox/rule-sets/<tag>.srs`
是否存在；不存在的就经 sing-box 内部 HTTP 代理（`127.0.0.1:11080`，自动经机场出网）
从 `raw.githubusercontent.com/SagerNet/sing-{geosite,geoip}/rule-set/<tag>.srs` 拉一份
落盘。任一 tag 拉取失败返回 502 + 失败 tag 名 + 上游错误，cfg 与磁盘均未变（写到 .tmp
的部分文件会清掉）。同一 tag 并发 PUT 只触发一次 HTTP 请求（per-name lock + 双检）。

成功后立即 `systemctl restart leap-singbox`（5–10s 海外业务中断），返回 200 + 新状态（同 GET 结构）。

```bash
# 加 / 删一条 domain_suffix —— 整体替换语义，要先 GET 拿现状再 PUT
TOK=...
H="Authorization: Bearer $TOK"
curl -s -H "$H" 127.0.0.1:18080/api/whitelist \
  | jq '.domain_suffix += ["new-site.com"]' \
  | curl -s -H "$H" -H "Content-Type: application/json" -X PUT \
      127.0.0.1:18080/api/whitelist -d @-

# 加一条 ip_cidr (例如某 App 自己回报的 IP 段)
curl -s -H "$H" 127.0.0.1:18080/api/whitelist \
  | jq '.ip_cidr += ["203.0.113.0/24"]' \
  | curl -s -H "$H" -H "Content-Type: application/json" -X PUT \
      127.0.0.1:18080/api/whitelist -d @-
```

---

## GET /api/whitelist/resolved

把当前白名单里所有规则解析成扁平的具体值——`domains[]` (geosites + domain_suffix
展开后去重) 和 `ip_cidrs[]` (geoips + ip_cidr 展开后去重)。专门给飞连 SaaS 控制端的
"极速模式"用：飞连终端只接受具体域名 + IP 段清单，不认 `geosite-google` /
`geoip-jp` 这种 tag，所以要把 rule-set 展开喂回去。

数据源：
- `domains`：`v2fly/domain-list-community`（这是 `sagernet/sing-geosite` 的上游），
  通过 jsdelivr CDN 拉（`cdn.jsdelivr.net`，CN 内可达），失败回退 `raw.githubusercontent.com`。
- `ip_cidrs`：节点上本地 `/etc/leap/singbox/rule-sets/<geoip-tag>.srs`，由
  `sing-box rule-set decompile` 解出 `rules[].ip_cidr` 合并去重。完全离线，
  不依赖任何上游网络。

```json
{
  "input": {
    "geosites": ["geosite-google", "geosite-openai", "geosite-github"],
    "geoips": [],
    "domain_suffix": ["claude.ai", "anthropic.com"],
    "ip_cidr": ["149.154.0.0/16", "91.108.0.0/16"]
  },
  "domains": ["0emm.com", "1e100.net", "abc.xyz", "...", "youtube.com"],
  "ip_cidrs": ["91.108.0.0/16", "149.154.0.0/16"],
  "domains_count": 1754,
  "ip_cidrs_count": 2,
  "last_built_at": "2026-05-28T09:12:06.622561162Z",
  "source": "https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/"
}
```

| 字段 | 说明 |
|---|---|
| `input.geosites` / `input.geoips` / `input.domain_suffix` / `input.ip_cidr` | 这次展开用的原始 yaml 输入（与 `/api/whitelist` 同步），便于消费方确认快照对应当前配置 |
| `domains` | 由 geosites + domain_suffix 展开。小写、字典序、去重。`include:` 递归跟、`regex:` `keyword:` 跳过、`@attribute` 剥掉、`full:` `domain:` 前缀去掉 |
| `ip_cidrs` | 由 geoips（.srs decompile）+ 字面 ip_cidr 合并。字典序、去重 |
| `domains_count` | `len(domains)` |
| `ip_cidrs_count` | `len(ip_cidrs)` |
| `last_built_at` | 本快照的构建时间（UTC，RFC3339） |
| `source` | 实际成功拉到 v2fly 数据的 base URL（geosite 失败时为空） |
| `stale` | （仅当存在）`true` 表示最近一次刷新失败、当前返回的是上一次的旧快照 |
| `last_error` | （仅当存在）最近一次失败的错误描述 |

**行为**：

- 启动时从 `/var/lib/leap/whitelist-resolved.json` 加载上次的快照（避免首启返回空），
  然后在后台跑一次 8s 后启动的 warm refresh 把快照更新到当前 yaml 的内容。
  老路径 `/var/lib/leap/whitelist-domains.json` 会作为 fallback 读取并升级到新结构。
- `PUT /api/whitelist` 写完后**异步**触发一次 refresh（不阻塞 PUT 响应）。
- 如果调用时还从来没有任何快照（首次部署刚起来、warm 还没跑完），会**同步**触发一次
  上限 30s 的 refresh；超时 / 失败 → 503 `expansion not yet available — try again in a few seconds`。
- 上游周期性失败时**不会**用空覆盖旧快照，会把旧快照标记 `stale=true` 继续返回。
- expander 进程内串行（同一时刻只跑一次 refresh），多次连发 PUT 不会打爆上游。
- geoip 展开是本地 op，几十毫秒就完成，不会影响 latency。

```bash
# PoC 直接拉（无 token）
curl -s http://192.168.70.92:18080/api/whitelist/resolved | jq .

# 飞连"极速模式"配置：分别取 domains / ip_cidrs，每行一个
curl -s http://192.168.70.92:18080/api/whitelist/resolved | jq -r '.domains[]'
curl -s http://192.168.70.92:18080/api/whitelist/resolved | jq -r '.ip_cidrs[]'

# 节点本机摘要
curl -s http://127.0.0.1:18080/api/whitelist/resolved | jq '.domains_count, .ip_cidrs_count, .last_built_at'
```

> 同样的展开逻辑也有一个 CLI 版本：`scripts/expand-whitelist.py`，但只展开 domain
> （不做 geoip decompile，因为本地没装 sing-box）。需要 IP CIDR 时直接打 API。

---

## GET /api/rule-sets

返回 leap-gateway 内嵌的 rule-set catalog（geosite + geoip 两组），每项含上游 URL、
分类、本地是否已下载（`installed`）、当前是否在白名单里启用（`selected`）。配套返回
`domain_suffix` / `ip_cidr` 的提示案例供 UI 预填表单。

```json
{
  "geosites": {
    "catalog": [
      {"name":"geosite-google","url":"https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-google.srs","category":"tech","installed":true,"selected":true},
      {"name":"geosite-anthropic","url":"https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-anthropic.srs","category":"tech","installed":true,"selected":false},
      {"name":"geosite-icloud","url":"https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-icloud.srs","category":"productivity","installed":false,"selected":false}
    ],
    "domain_suffix_examples": ["anthropic.com","claude.ai","cursor.com","figma.com","quora.com"]
  },
  "geoips": {
    "catalog": [
      {"name":"geoip-google","url":"https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/google.srs","category":"app","installed":false,"selected":false},
      {"name":"geoip-telegram","url":"https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/telegram.srs","category":"app","installed":false,"selected":false},
      {"name":"geoip-jp","url":"https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/jp.srs","category":"country","installed":false,"selected":false}
    ],
    "ip_cidr_examples": ["149.154.0.0/16","91.108.0.0/16"]
  }
}
```

| 字段 | 说明 |
|---|---|
| `geosites.catalog[].name` | 上游 sagernet/sing-geosite 仓库 rule-set 分支里 `<name>.srs` 的 tag 名 |
| `geosites.catalog[].url` | 该 .srs 的上游 URL（leap-gateway 经内部 HTTP 代理走机场拉，CN 内可达）|
| `geosites.catalog[].category` | 分组：`tech` / `social` / `streaming` / `reference` / `productivity`。其中含若干 `geosite-category-*` 聚合 tag（如 `geosite-category-ai-!cn`），一条命中数百域名 |
| `geosites.catalog[].installed` | `/etc/leap/singbox/rule-sets/<name>.srs` 是否已经在本地 |
| `geosites.catalog[].selected` | 该 tag 是否在 `whitelist.geosites` 里 |
| `geosites.domain_suffix_examples` | UI 预填 `whitelist.domain_suffix` 的示例值（不影响实际配置）|
| `geoips.catalog[].name` | 上游 MetaCubeX/meta-rules-dat 仓库 sing 分支 `geo/geoip/<stem>.srs` 的 tag 名（leap-gateway 加上 `geoip-` 前缀作为统一标识）|
| `geoips.catalog[].url` | 完整 .srs URL —— 注意上游路径里**无** `geoip-` 前缀，由 `{stem}` 替换处理 |
| `geoips.catalog[].category` | 分组：`app`（公司 / 应用自有 IP 段，如 `geoip-google`）/ `country`（ISO 2 字母国家码，如 `geoip-jp`）|
| `geoips.ip_cidr_examples` | UI 预填 `whitelist.ip_cidr` 的示例值（默认是 Telegram MTProto 的 DC 段）|

**catalog 的来源**：内嵌进 leap-gateway 二进制（`go:embed catalog.json`）。

| 上游 | 项数 | 用途 |
|---|---|---|
| `SagerNet/sing-geosite` rule-set 分支 | 62（按 `tech` / `social` / `streaming` / `reference` / `productivity` 五类分组，含若干 `geosite-category-*` 聚合 tag —— 一条命中数百域名） | 按域名路由 |
| `MetaCubeX/meta-rules-dat` sing 分支 `geo/geoip/` 目录 | 23（8 个 app tag：cloudflare / cloudfront / facebook / fastly / google / netflix / telegram / twitter；15 个 ISO 国家码） | 按 IP 路由 |

> **为什么 geoip 用 MetaCubeX 而不是 SagerNet**：sagernet/sing-geoip rule-set 分支
> **只发 ISO 国家码**，没有 app tag。MetaCubeX 这条社区线（Clash.Meta / mihomo 也用这个）
> 维护了 ~10 个常见 app 的 IP CIDR 列表，文件格式跟 sagernet 完全一致（都是 sing-box `.srs`）。

> **慎用大网段 app geoip**：`geoip-google` / `geoip-cloudflare` 命中的是整个 Google /
> Cloudflare 的 IP 范围（含 8.8.8.8 这种 DNS、所有 Cloudflare-fronted 站点等）。加进
> 白名单等于把半个互联网走机场。能用 geosite 走 DNS 路径的优先用 geosite —— DNS 解
> 析回来精确多了。geoip app tag 主要给"硬编码 IP / 不查 DNS"的应用（Telegram MTProto、
> Signal call、WireGuard endpoint 这种）。

**`geosite-cn` / `geoip-cn` 不在 catalog 里**：它们是路由 infra（命中 cn 直连），由
`gateway.yaml` 的 `singbox.route.geosite_url` / `geoip_url` 显式指定 URL，安装时由
`scripts/stage.sh` 预下到 `/etc/leap/singbox/rule-sets/`，不暴露给业务白名单选择。

PUT `/api/whitelist` 时 geosites/geoips 必须从 catalog 里挑；本地没有的会自动按需下载（见 PUT `/api/whitelist`）。

```bash
# 看完整 catalog 数量 + examples
curl -s http://192.168.70.92:18080/api/rule-sets \
  | jq '{
      geosite_count: (.geosites.catalog | length),
      geoip_count:   (.geoips.catalog   | length),
      installed_geosites: [.geosites.catalog[] | select(.installed) | .name],
      not_installed:      [.geosites.catalog[] | select(.installed | not) | .name],
      domain_examples: .geosites.domain_suffix_examples,
      ip_examples:     .geoips.ip_cidr_examples
    }'

# 拉所有"tech"分类的 tag
curl -s http://192.168.70.92:18080/api/rule-sets \
  | jq '.geosites.catalog | map(select(.category=="tech")) | map(.name)'
```

## GET /api/geosites（兼容别名）

```json
{
  "available": ["geosite-anthropic","geosite-discord","geosite-google","..."],
  "active":    ["geosite-google","geosite-openai"]
}
```

只返回 geosite 部分的 `{available, active}` —— `available` 是节点上 `geosite-*.srs` 实际
存在的 tag（不是 catalog 全集），结构与旧版一致。新代码请用 `/api/rule-sets`。

---

## GET /api/subscriptions

```json
[
  {
    "name": "yuyun",
    "url": "https://yuyun.example/sub?token=3f18***8960",
    "enabled": true,
    "format": "auto",
    "user_agent": "sing-box/1.10.7",
    "nodes_count": 66,
    "last_refresh": "2026-05-28T03:54:31Z"
  }
]
```

| 字段 | 说明 |
|---|---|
| `name` | 订阅名（在 sing-box 里也作为 tag 前缀：`<name>/...`） |
| `url` | URL，但 `token=` `key=` `password=` `auth=` `secret=` 这几个查询参数中部用 `***` 打码 |
| `enabled` | 是否启用 |
| `format` | `auto` / `clash` / `singbox` / `uri` / `sip008` |
| `user_agent` | 拉订阅时用的 UA。空表示用全局 `subscribe.user_agent`。机场对 UA 敏感时用：yuyun 给 Clash UA 返回 `proxies: []` 阉割版（要 `sing-box/1.10.7`）；ash 给 sing-box UA HTTP 500（要 `ClashforWindows/0.20.39`） |
| `nodes_count` | 上一次 refresh 该订阅解析出多少节点 |
| `last_refresh` | 全局最后一次 refresh 时间（不是单条订阅的） |

## POST /api/subscriptions

请求：

```json
{
  "name": "backup",
  "url":  "https://other-airport.example/sub?token=...",
  "format": "auto",
  "user_agent": "ClashforWindows/0.20.39"
}
```

校验：

- `name` 必填、非空。重复名 409。
- `url` 必填、scheme 必须是 `http` / `https`、host 非空。
- `format` 缺省 `auto`。
- `user_agent` 可选，缺省走全局 `subscribe.user_agent`。机场拉不到节点时优先怀疑 UA。
- 新增订阅默认 `enabled: true`。

成功：写 yaml → SetEntries → Refresh → render → restart sing-box → 201 + 同 GET 结构（含新订阅）。

> **新增第二条订阅会触发主备分流**：第一条 enabled 的为主池（`urltest-primary`），其余 enabled 的为备池（`urltest-backup`）。watchdog 会自动启用 primary→backup 故障切换。

## PUT /api/subscriptions/{name}

支持改 `url` 和 `user_agent`。请求：

```json
{
  "url": "https://yuyun.example/sub?token=NEW_TOKEN",
  "user_agent": "sing-box/1.10.7"
}
```

`user_agent` 缺省（空字符串）= 清除该订阅的 override，走全局默认。
未找到 404。校验通过后写 yaml + 全链刷新，返回 200 + 完整列表。

## DELETE /api/subscriptions/{name}

未找到 404。成功 204（无 body），背后写 yaml + 全链刷新（如果删后 enabled 为 0，refresh 会因 "no subscription produced any nodes" 报错——这种情况要先加新的再删旧的）。

---

## POST /api/subscribe/refresh

```bash
curl -s -X POST -H "$H" 127.0.0.1:18080/api/subscribe/refresh
```

成功：

```json
{"ok": true}
```

副作用：拉所有 enabled 订阅 → 重渲染 sing-box config → restart leap-singbox。
失败：500 + `{"error": "..."}`。

> 改 yaml 文件之后用这个，立即生效。订阅 CRUD 端点已经自动走过这条流程，无需再调一次。

---

## GET /api/subscribe/refresh-interval
## PUT /api/subscribe/refresh-interval

读 / 改周期刷新的间隔（秒为单位），写盘并实时切换运行时调度器，不需要重启 leap-gateway。

```bash
curl -s http://192.168.70.92:18080/api/subscribe/refresh-interval
# {"seconds":1800}

# 改成 5 分钟
curl -s -X PUT -H 'Content-Type: application/json' \
  -d '{"seconds":300}' http://192.168.70.92:18080/api/subscribe/refresh-interval
# {"seconds":300}

# 关掉周期刷新，只接受手动 POST /api/subscribe/refresh
curl -s -X PUT -H 'Content-Type: application/json' \
  -d '{"seconds":0}' http://192.168.70.92:18080/api/subscribe/refresh-interval
# {"seconds":0}
```

PUT body：

```json
{"seconds": 60}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `seconds` | int64 ≥ 0，必填 | 周期刷新间隔的秒数。`0` ⇒ 关闭周期刷新，仅 `POST /api/subscribe/refresh` 手动触发 |

校验：

- `seconds` 缺失 / 非整数 → 400；
- `seconds < 0` → 400 `seconds must be >= 0`；
- 持久化写回 `gateway.yaml` 的 `subscribe.refresh_interval`，重启后保持。

PUT 后**不会**顺手触发一次刷新（避免大量改动密集打上游）。需要立即生效就再调一次
`POST /api/subscribe/refresh`。

---

## 主备节点切换语义（watchdog v2 行为）

当 enabled 订阅 ≥ 2 时，渲染器把节点按 `<sub_name>/` 前缀拆成：
- `urltest-primary`：第一条 enabled 订阅的节点
- `urltest-backup`：其余 enabled 订阅的节点

`out` selector 默认指向 `urltest-primary`。

主→备切换：

1. watchdog 每 5s（±20% jitter）探测 `out` 当前选中的具体节点；
2. 若过去 5s 有真实用户流量经过 `out`（`/connections` 字节增量 > 0），跳过合成探测——`skipped_due_traffic` 计数 +1；
3. 连续 `fail_threshold` 次失败：
   - 有备池 → `PUT /proxies/out` 切到 `urltest-backup`
   - 单池 → 触发 `urltest-primary/delay` 强制全池重选

备→主回切：

1. 独立 goroutine 每 30 分钟探测 `urltest-primary` 当前选中节点；
2. 连续 `primary_recovery_threshold`（默认 3）次成功 → `PUT /proxies/out` 切回；
3. 任一次失败重置计数，重新等 30 分钟。

健康时 watchdog 探测周期会自适应回退：5 → 10 → 20 → 40 → 60s（任何失败 / 选择变化 / 切换都重置回 5s）。这部分参数都在 `singbox.urltest.watchdog` 段，如需调整改 yaml + 重启 leap-gateway。

---

## 节点上常用 curl 一键查

```bash
# 不配 token 时 H 留空即可
TOK=$(grep -oE 'token: "\S+"' /etc/leap/gateway.yaml | head -1 | awk -F'"' '{print $2}')
H="Authorization: Bearer $TOK"

# 节点全景（最常用）
curl -s -H "$H" 127.0.0.1:18080/api/proxies/active | python3 -m json.tool

# 当前白名单
curl -s -H "$H" 127.0.0.1:18080/api/whitelist | python3 -m json.tool

# 展开后的扁平域名 + IP 段列表（飞连"极速模式"用）
curl -s -H "$H" 127.0.0.1:18080/api/whitelist/resolved | python3 -m json.tool

# Catalog 全集 + 每项 installed/selected + 提示案例（geosite + geoip 一起拉）
curl -s -H "$H" 127.0.0.1:18080/api/rule-sets | python3 -m json.tool

# 立即刷新订阅
curl -s -X POST -H "$H" 127.0.0.1:18080/api/subscribe/refresh

# 看 / 改周期刷新间隔
curl -s -H "$H" 127.0.0.1:18080/api/subscribe/refresh-interval
curl -s -H "$H" -H "Content-Type: application/json" -X PUT \
     127.0.0.1:18080/api/subscribe/refresh-interval -d '{"seconds":600}'
```

## 外部平台调用范例（PoC 当前无 token）

```bash
LEAP=http://192.168.70.92:18080

# 健康
curl -s $LEAP/healthz

# 节点全景
curl -s $LEAP/api/proxies/active | jq .

# 加一条 domain_suffix 进白名单（先 GET 再整体 PUT）
curl -s $LEAP/api/whitelist | \
  jq '.domain_suffix += ["new-site.com"]' | \
  curl -s -H "Content-Type: application/json" \
       -X PUT $LEAP/api/whitelist -d @-

# 拉展开后的飞连"极速模式"清单（domains 一行一个；ip_cidrs 同理）
curl -s $LEAP/api/whitelist/resolved | jq -r '.domains[]'
curl -s $LEAP/api/whitelist/resolved | jq -r '.ip_cidrs[]'

# 加一条订阅（自动触发主备分流）
curl -s -H "Content-Type: application/json" \
     -X POST $LEAP/api/subscriptions \
     -d '{"name":"backup","url":"https://backup.example/sub?token=..."}'

# 立即刷新所有订阅 + 重渲染 + 重启 sing-box
curl -s -X POST $LEAP/api/subscribe/refresh
```

> 一旦切生产并启用 token，把 `LEAP=...` 后加一行 `H="Authorization: Bearer $TOK"`，每个
> curl 加 `-H "$H"` 即可。
