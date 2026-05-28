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
| 400 | 请求体非法 / 字段格式错 |
| 401 | token 缺失或不匹配（仅当 `api.token` 非空时可能出现）|
| 404 | 资源不存在（订阅名找不到） |
| 409 | 资源冲突（订阅名已存在） |
| 500 | 持久化失败 / 渲染失败 / sing-box reload 失败 |

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
| GET | `/api/whitelist` | 无 | 白名单（mode + geosites + domain_suffix） |
| PUT | `/api/whitelist` | 写 yaml + 重启 sing-box + 异步重建展开缓存 | 整体替换白名单 |
| GET | `/api/whitelist/domains` | 无（首次冷启动会同步拉一次） | 把 geosites + domain_suffix 展开成扁平域名列表（飞连"极速模式"用） |
| GET | `/api/geosites` | 无 | `available`（本地 .srs 文件）+ `active`（已启用） |
| GET | `/api/subscriptions` | 无 | 订阅列表，URL token 自动打码 |
| POST | `/api/subscriptions` | 写 yaml + 拉订阅 + 重启 sing-box | 新增订阅 |
| PUT | `/api/subscriptions/{name}` | 写 yaml + 拉订阅 + 重启 sing-box | 改 URL |
| DELETE | `/api/subscriptions/{name}` | 写 yaml + 拉订阅 + 重启 sing-box | 删订阅 |
| POST | `/api/subscribe/refresh` | 拉订阅 + 重启 sing-box | 立即刷新 |

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
    "gateway_version": "dev",
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

## GET /api/whitelist

```json
{
  "mode": "whitelist",
  "geosites": ["geosite-google", "geosite-youtube", "geosite-openai"],
  "domain_suffix": ["claude.ai", "anthropic.com"]
}
```

| 字段 | 说明 |
|---|---|
| `mode` | `overseas` 或 `whitelist`。`overseas` = 命中 cn 的直连，其余走机场；`whitelist` = 仅命中白名单走机场，其它直连 |
| `geosites` | 启用的 geosite tag 数组（必须是本地 .srs 已就位的，从 `/api/geosites` 取） |
| `domain_suffix` | 额外的域名后缀列表（小写 + bare domain） |

## PUT /api/whitelist

**整体替换语义**。改一项也要先 GET 拿现状、改完整体 PUT。

请求：

```json
{
  "mode": "whitelist",
  "geosites": ["geosite-google", "geosite-anthropic"],
  "domain_suffix": ["claude.ai", "openai.com"]
}
```

校验：

- `mode` 缺省 = 不动当前；显式只接受 `overseas` / `whitelist`。
- `geosites` 必须 ⊆ `/api/geosites` 的 `available`，否则 400。
- `domain_suffix` 每条必须是裸域名（含 `.`、不含 `://` `/` `空白`、首尾无 `.`）。
- 重复项自动去重，空字符串忽略。

成功后立即 `systemctl restart leap-singbox`（5–10s 海外业务中断），返回 200 + 新状态（同 GET 结构）。

```bash
# 加 / 删一条 domain_suffix —— 整体替换语义，要先 GET 拿现状再 PUT
TOK=...
H="Authorization: Bearer $TOK"
curl -s -H "$H" 127.0.0.1:18080/api/whitelist \
  | jq '.domain_suffix += ["new-site.com"]' \
  | curl -s -H "$H" -H "Content-Type: application/json" -X PUT \
      127.0.0.1:18080/api/whitelist -d @-
```

---

## GET /api/whitelist/domains

把当前白名单（geosites + domain_suffix）展开成**扁平的域名后缀列表**。
专门给飞连 SaaS 控制端的"极速模式"用：飞连终端只接受具体域名清单，
不认 `geosite-google` 这种 tag，所以要把 geosite 递归展开成根域名喂回去。

数据源：`v2fly/domain-list-community`（这是 `sagernet/sing-geosite` 的上游），
通过 jsdelivr CDN 拉（`cdn.jsdelivr.net`，CN 内可达），失败回退 `raw.githubusercontent.com`。

```json
{
  "geosites": ["geosite-google", "geosite-openai", "geosite-github"],
  "domain_suffix": ["claude.ai", "anthropic.com"],
  "domains": ["0emm.com", "1e100.net", "abc.xyz", "...", "youtube.com"],
  "count": 1754,
  "last_built_at": "2026-05-28T09:12:06.622561162Z",
  "source": "https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/"
}
```

| 字段 | 说明 |
|---|---|
| `geosites` | 这次展开用的 tag 列表（与 `/api/whitelist` 同步） |
| `domain_suffix` | 这次展开用的额外后缀（与 `/api/whitelist` 同步） |
| `domains` | 展开后的扁平域名列表（小写、字典序、去重）。`include:` 递归跟、`regex:` `keyword:` 跳过、`@attribute` 剥掉、`full:` `domain:` 前缀去掉 |
| `count` | `len(domains)`，方便客户端判断是否符合预期 |
| `last_built_at` | 本快照的构建时间（UTC，RFC3339） |
| `source` | 实际成功拉到数据的 base URL |
| `stale` | （仅当存在）`true` 表示最近一次刷新失败、当前返回的是上一次的旧快照 |
| `last_error` | （仅当存在）最近一次失败的错误描述 |

**行为**：

- 启动时从 `/var/lib/leap/whitelist-domains.json` 加载上次的快照（避免首启返回空），
  然后在后台跑一次 8s 后启动的 warm refresh 把快照更新到当前 yaml 的内容。
- `PUT /api/whitelist` 写完后**异步**触发一次 refresh（不阻塞 PUT 响应；
  上游拉取一般 1–3s，PoC 测下来 jsdelivr 单文件 200–500ms）。
- 如果调用时还从来没有任何快照（首次部署刚起来、warm 还没跑完），会**同步**触发一次
  上限 30s 的 refresh；超时 / 失败 → 503 `expansion not yet available — try again in a few seconds`。
- 上游周期性失败时**不会**用空覆盖旧快照，会把旧快照标记 `stale=true` 继续返回。
- expander 进程内串行（同一时刻只跑一次 refresh），多次连发 PUT 不会打爆上游。

```bash
# PoC 直接拉（无 token）
curl -s http://192.168.70.92:18080/api/whitelist/domains | jq .

# 飞连"极速模式"配置：只取 domains 字段，每行一个
curl -s http://192.168.70.92:18080/api/whitelist/domains | jq -r '.domains[]'

# 节点本机
curl -s http://127.0.0.1:18080/api/whitelist/domains | jq '.count, .last_built_at'
```

> 同样的展开逻辑也有一个 CLI 版本：`scripts/expand-whitelist.py`（默认就是去打 PoC 的这个端点；
> 也支持 `--geosites foo,bar --domain-suffix x.com` 离线跑）。两边输出一致。

---

## GET /api/geosites

```json
{
  "available": ["geosite-anthropic", "geosite-cn", "geosite-discord", ...],
  "active":    ["geosite-google", "geosite-openai"]
}
```

| 字段 | 说明 |
|---|---|
| `available` | 节点上 `/etc/leap/singbox/rule-sets/*.srs` 实际存在的 tag（去掉 `.srs` 后缀，**排除 `geosite-cn` / `geoip-cn`**——它们由路由内置使用） |
| `active` | 当前 yaml 里 `whitelist.geosites` 启用的子集 |

PUT `/api/whitelist` 时 geosites 必须从 `available` 挑。

---

## GET /api/subscriptions

```json
[
  {
    "name": "yuyun",
    "url": "https://yuyun.example/sub?token=3f18***8960",
    "enabled": true,
    "format": "auto",
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
| `nodes_count` | 上一次 refresh 该订阅解析出多少节点 |
| `last_refresh` | 全局最后一次 refresh 时间（不是单条订阅的） |

## POST /api/subscriptions

请求：

```json
{
  "name": "backup",
  "url":  "https://other-airport.example/sub?token=...",
  "format": "auto"
}
```

校验：

- `name` 必填、非空。重复名 409。
- `url` 必填、scheme 必须是 `http` / `https`、host 非空。
- `format` 缺省 `auto`。
- 新增订阅默认 `enabled: true`。

成功：写 yaml → SetEntries → Refresh → render → restart sing-box → 201 + 同 GET 结构（含新订阅）。

> **新增第二条订阅会触发主备分流**：第一条 enabled 的为主池（`urltest-primary`），其余 enabled 的为备池（`urltest-backup`）。watchdog 会自动启用 primary→backup 故障切换。

## PUT /api/subscriptions/{name}

只支持改 `url`。请求：

```json
{"url": "https://yuyun.example/sub?token=NEW_TOKEN"}
```

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

# 展开后的扁平域名列表（飞连"极速模式"用）
curl -s -H "$H" 127.0.0.1:18080/api/whitelist/domains | python3 -m json.tool

# 本地 .srs 可选清单
curl -s -H "$H" 127.0.0.1:18080/api/geosites | python3 -m json.tool

# 立即刷新订阅
curl -s -X POST -H "$H" 127.0.0.1:18080/api/subscribe/refresh
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

# 拉展开后的飞连"极速模式"域名清单（一行一个，可直接粘进飞连 SaaS）
curl -s $LEAP/api/whitelist/domains | jq -r '.domains[]'

# 加一条订阅（自动触发主备分流）
curl -s -H "Content-Type: application/json" \
     -X POST $LEAP/api/subscriptions \
     -d '{"name":"backup","url":"https://backup.example/sub?token=..."}'

# 立即刷新所有订阅 + 重渲染 + 重启 sing-box
curl -s -X POST $LEAP/api/subscribe/refresh
```

> 一旦切生产并启用 token，把 `LEAP=...` 后加一行 `H="Authorization: Bearer $TOK"`，每个
> curl 加 `-H "$H"` 即可。
