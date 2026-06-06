# Leap Gateway 管理 API

控制面 HTTP API —— 覆盖节点健康、订阅管理、白名单维护、UX 遥测四大功能域。

| 节点 | base URL |
|---|---|
| 89（生产 PoC，mihomo） | `http://192.168.70.89:18080` |
| 节点本机调用 | `http://127.0.0.1:18080` |

监听由 `gateway.yaml` 的 `api.listen` 控制；生产前应收回 `127.0.0.1:18080` 并在前置 nginx 终端 TLS。

> **数据面引擎**：mihomo（load-balance + consistent-hashing 多活）。sing-box-as-engine 路径在 2026-06 清理时整段删除。`/usr/local/bin/sing-box` CLI 二进制仍随部署安装，仅给 `whitelistexpand` 做 `.srs` decompile 用，不作为数据面跑。

---

## 认证

`gateway.yaml` 设 `api.token: "<token>"` 后，除 `/healthz` 和 `POST /api/ux-telemetry` 外，所有端点需要：

```
Authorization: Bearer <token>
```

`/healthz` 不需要认证（LB / 监控探测用）。`POST /api/ux-telemetry` 不需要认证（员工浏览器批量上报，加 token 成本过高）。

---

## 通用错误码

| 状态 | 含义 |
|---|---|
| 400 | 请求体非法 / 字段格式错 / catalog 里无该 rule-set tag |
| 401 | token 缺失或不匹配 |
| 404 | 资源不存在（如订阅名找不到） |
| 409 | 资源冲突（订阅名已存在） |
| 500 | 持久化 / 渲染 / 数据面 reload 失败 |
| 502 | 上游按需拉取 .srs/.mrs 失败 |
| 503 | 功能未启用（如访问 `/api/nodes/health` 但 `node_qualify.enabled=false`） |

---

## 端点速览

| 方法 | 路径 | 功能 | 认证 |
|---|---|---|---|
| GET | `/healthz` | LB 可达性探测 | 无 |
| GET | `/api/status` | 节点总体状态 | ✓ |
| GET | `/api/nodes` | 全量已解析节点列表 | ✓ |
| GET | `/api/nodes/health` | 节点健康评分 + 动态入池状态（mihomo） | ✓ |
| GET | `/api/proxies/active` | 数据面当前活跃代理详情 + 节点 + 出口 IP | ✓ |
| POST | `/api/proxies/select` | 手动指定活跃节点 | ✓ |
| GET | `/api/subscriptions` | 订阅列表 | ✓ |
| POST | `/api/subscriptions` | 新增订阅 | ✓ |
| PUT | `/api/subscriptions/{name}` | 修改订阅 URL / UA | ✓ |
| DELETE | `/api/subscriptions/{name}` | 删除订阅 | ✓ |
| POST | `/api/subscribe/refresh` | 立即触发订阅刷新 | ✓ |
| GET | `/api/subscribe/refresh-interval` | 查看刷新间隔 | ✓ |
| PUT | `/api/subscribe/refresh-interval` | 修改刷新间隔 | ✓ |
| GET | `/api/whitelist` | 白名单当前配置 | ✓ |
| PUT | `/api/whitelist` | 整体替换白名单 | ✓ |
| GET | `/api/whitelist/resolved` | 展开为域名 + IP 清单（飞连极速模式） | ✓ |
| GET | `/api/rule-sets` | rule-set catalog（geosite + geoip） | ✓ |
| GET | `/api/geosites` | 同 /api/rule-sets（兼容别名） | ✓ |
| POST | `/api/ux-telemetry` | 客户端上报 UX 事件 | 无 |
| GET | `/api/ux-telemetry` | 查询原始事件 | ✓ |
| GET | `/api/ux-telemetry/summary` | 按域名聚合（p50/p95 TTFB、错误率） | ✓ |

---

## GET /healthz

LB / 监控可达性探测。

```
HTTP 200
{"ok":true}
```

---

## GET /api/status

节点总体快照。

```json
{
  "last_refresh":     "2026-06-06T01:00:00Z",
  "node_count":       155,
  "subscriptions":    5,
  "engine_ok":        true,
  "pool_qualified":   17,
  "pool_total":       26,
  "pool_last_update": "2026-06-06T00:55:32Z"
}
```

| 字段 | 说明 |
|---|---|
| `node_count` | 所有已解析的代理节点总数（跨全部订阅） |
| `subscriptions` | 当前已配置的订阅数 |
| `engine_ok` | 当前数据面（mihomo）clash-api `/version` 可达 |
| `pool_qualified` / `pool_total` | NodeScorer 当前合格 / 候选总数（仅 mihomo 引擎，未启用 NodeScorer 时省略） |
| `pool_last_update` | 最近一次 us-pool 成员变更的时间 |

> 注：`pool_qualified` / `pool_total` / `pool_last_update` 三个字段仅在 `node_qualify.enabled=true`（默认）时返回，关掉就缺失。

---

## GET /api/nodes

返回所有已解析节点的扁平列表，含 tag、协议类型、server 域名、来源订阅。

```json
[
  {"tag": "ctc-02/US-C30-01-DMIT", "type": "vless", "server": "us01.c30.example.xyz", "source": "ctc-02"},
  {"tag": "ash/🇺🇸US-IEPL-01",      "type": "trojan", "server": "t7m2.example.org",   "source": "ash"}
]
```

**注**：此端点输出全量（含非 US 节点）。配合 `node_qualify.node_pattern` 筛选实际入池的节点。

---

## GET /api/nodes/health

**仅 mihomo 引擎**。NodeScorer 的动态评分结果：每 `node_qualify.scoring_interval`（默认 60s）对 us-pool 候选集打一次分，按 RTT 稳定性 + 探测失败率 + 被动吞吐决定是否入池，变化时热重载 mihomo（无重启）。

返回 `503 {"error":"node health scoring not active ..."}` 的情况：
- `node_qualify.enabled=false`（默认会自动开启，除非显式关闭）

```json
{
  "qualified": 17,
  "total": 26,
  "last_pool_update": "2026-06-05T12:33:25Z",
  "last_scored_at":   "2026-06-05T12:36:25Z",
  "thresholds": {
    "MaxRTTP50Ms": 500,
    "MaxRTTP95Ms": 800,
    "MaxJitterMs": 300,
    "MaxFailRate": 0.25,
    "MinProbes":   2,
    "EvictStrikes":   2,
    "ReadmitStrikes": 1
  },
  "nodes": [
    {
      "name":           "ctc-02/US-C30-01-DMIT",
      "sub":            "ctc-02",
      "alive":          true,
      "rtt_last_ms":    154,
      "rtt_p50_ms":     157,
      "rtt_p95_ms":     169,
      "jitter_ms":      12,
      "fail_rate":      0.0,
      "probe_count":    8,
      "throughput_bps": 0,
      "qualified":      true,
      "in_pool":        true,
      "strikes":        0,
      "ok_rounds":      9,
      "reason":         ""
    },
    {
      "name":       "ash/🇺🇸US-IEPL-03",
      "sub":        "ash",
      "alive":      true,
      "rtt_p50_ms": 198,
      "jitter_ms":  511,
      "fail_rate":  0.0,
      "qualified":  false,
      "in_pool":    false,
      "strikes":    2,
      "reason":     "jitter=511ms > 300ms"
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `qualified` / `total` | 当前入池数 / 候选集总数 |
| `last_pool_update` | 最近一次 us-pool 成员变化的时间 |
| `rtt_p50_ms` | 最近 8 次探测的中位延迟 |
| `rtt_p95_ms` | 95 分位延迟（防尖刺） |
| `jitter_ms` | p95 - p50（稳定性指标） |
| `fail_rate` | delay=0（探测失败）占比 |
| `throughput_bps` | 过去 60s 被动推算的真实吞吐（无活跃流量时为 0） |
| `in_pool` | 当前是否在 us-pool 中承载流量 |
| `strikes` | 连续不达标轮次（达到 EvictStrikes 才真正踢出） |
| `ok_rounds` | 连续达标轮次 |
| `reason` | 不入池原因（空 = 入池） |

**评分规则**：

- `fail_rate > MaxFailRate` → 剔除
- `rtt_p50_ms > MaxRTTP50Ms` → 剔除
- `rtt_p95_ms > MaxRTTP95Ms` → 剔除
- `jitter_ms > MaxJitterMs` → 剔除
- `probe_count < MinProbes` → 暂时保留（历史不足，给 benefit of doubt）
- 连续 `EvictStrikes` 次不达标 → 从 us-pool 移除 + 热重载
- 恢复后连续 `ReadmitStrikes` 次达标 → 重新加入 + 热重载

---

## GET /api/proxies/active

数据面活跃状态的完整快照，含节点、池子、出口 IP 等信息。

```json
{
  "node": { "hostname": "...", "ipv4": "192.168.70.89", ... },
  "feilian": { "tun0_active": true, "vpn_active": true, ... },
  "leap": {
    "gateway_version": "<commit-sha>",
    "engine":          "mihomo",
    "engine_version":  "v1.19.26",
    "services": {
      "leap-gateway": "active",
      "leap-mihomo":  "active",
      "leap-nft":     "active"
    },
    "subscriptions_count": 5,
    "nodes_parsed": 155,
    "last_refresh": "2026-06-06T00:55:32Z"
  },
  "active_proxy": {
    "reachable":     true,
    "active_urltest": "us-pool",
    "pools": [
      {
        "tag":       "us-pool",
        "pool_size": 17,
        "active":    true,
        "nodes": [
          {"tag": "ctc-02/US-C30-01-DMIT", "delay_ms": 154, "last_check": "..."}
        ]
      }
    ]
  }
}
```

**字段说明**：
- `leap.engine`：永远是 `"mihomo"`（sing-box-as-engine 已删除）
- `leap.engine_version`：mihomo 二进制的版本字符串（`mihomo -v` 第一行的 `vX.Y.Z`）。二进制不存在或不响应时为空
- `leap.services`：`leap-gateway` / `leap-mihomo` / `leap-nft` 三个 unit 的 systemd 状态
- `active_proxy.active_urltest`：mihomo 的活跃池子，正常情况下是 `us-pool`（load-balance 组）；运维手动 PUT 切到 `pin` 时会是 `pin`

---

## POST /api/proxies/select

手动把某个 Selector 组指向它的某个成员（用于运维调试 / 应急切换）。

```json
// 请求体（两个字段必填）
{ "selector": "out", "name": "us-pool" }
```

**典型用法**（mihomo）：

| selector | name | 效果 |
|---|---|---|
| `out` | `us-pool` | 走 load-balance 多活池（默认行为） |
| `out` | `pin` | 走人工钉住的单节点（`pin` 选哪个节点见下一行） |
| `out` | `DIRECT` | 应急绕过代理，全部直连 |
| `pin` | `<node-tag>` | 把 pin 选择器固定到某个节点（之后 `out=pin` 才有意义） |

**响应**：成功返回 `200` + `/api/proxies/active` 当前快照（让运维直接看到切换后的状态）。失败：

| 状态 | 含义 |
|---|---|
| 400 | 请求体缺字段；或 selector 类型不是 Selector（URLTest/LoadBalance 不能手动 PUT） |
| 404 | selector 在数据面上不存在；或 name 不在该 selector 的 `all` 列表里（response 体会列出合法成员） |
| 500 | clash-api 不可达 |

**override 持久性**：NodeScorer 只管 us-pool 的 *成员*（哪些节点能承载流量），不会去动 selector 指针。手动 `out` / `pin` 切换会一直保持，直到下次运维改动或数据面重启。

---

## GET /api/subscriptions

返回所有订阅，含 UA、节点数、上次刷新时间。token 值被脱敏（中间 `***`）。

```json
[
  {
    "name":         "ctc-02",
    "url":          "https://47.x.x.x:9999/...?token=c85b***0f02",
    "enabled":      true,
    "format":       "auto",
    "user_agent":   "sing-box/1.10.7",
    "nodes_count":  6,
    "last_refresh": "2026-06-05T12:28:25Z"
  },
  {
    "name":       "ash",
    "url":        "https://kejalrnx.671234.xyz/...?token=728a***5b95",
    "enabled":    true,
    "user_agent": "clash.meta/v1.19.26",
    "nodes_count": 34
  }
]
```

---

## POST /api/subscriptions

新增订阅。立即触发一次 refresh + 数据面 reload（约 5-10s 海外业务中断）。

```json
// 请求体
{
  "name":       "yuyun",
  "url":        "https://yuyun.mhlnf.cn/yuyunsvip?token=...",
  "format":     "auto",
  "user_agent": "clash.meta/v1.19.26"
}
```

- `format`：`auto`（默认） | `clash` | `singbox` | `uri` | `sip008`
- `user_agent`：大多数机场用 `clash.meta/v1.19.26` 可拿到最多节点；`ClashforWindows/0.20.39` 兼容性最广但节点数较少；`sing-box/1.10.x` 适用于原生返回 sing-box JSON 的机场

成功返回 201 + 当前完整订阅列表（同 GET /api/subscriptions）。

---

## PUT /api/subscriptions/{name}

修改已有订阅的 URL 或 UA。请求体必须包含完整 URL（非脱敏版）。

```json
{
  "url":        "https://kejalrnx.671234.xyz/...?token=<真实token>",
  "user_agent": "clash.meta/v1.19.26"
}
```

成功返回 200 + 当前完整订阅列表。

---

## DELETE /api/subscriptions/{name}

删除订阅，立即触发 refresh + reload。成功返回 204。

---

## POST /api/subscribe/refresh

立即触发一次完整的 订阅拉取 → 渲染 → 数据面 reload 流程。

```json
// 响应
{"ok": true}
```

**注**：订阅 fetch 经由数据面 HTTP 代理（`127.0.0.1:11080`）路由，避免 fakeip 污染 host 解析器。数据面宕机时自动回退直连。

---

## GET /api/subscribe/refresh-interval

```json
{"refresh_interval": "30m0s"}
```

---

## PUT /api/subscribe/refresh-interval

```json
// 请求体
{"refresh_interval": "15m"}
```

接受 Go duration 格式：`30m`、`1h`、`0`（禁用定期刷新）。

---

## GET /api/whitelist

返回当前白名单配置（含 DNS fake-ip-skip 列表）。

```json
{
  "mode": "whitelist",
  "geosites": [
    "geosite-google", "geosite-anthropic", "geosite-openai",
    "geosite-github", "geosite-cloudflare", "geosite-category-ai-!cn"
  ],
  "geoips": ["geoip-google", "geoip-telegram"],
  "domain_suffix": ["ipinfo.io", "ip.me", "claude.ai"],
  "ip_cidr": ["149.154.0.0/16"],
  "fake_ip_skip": ["+.paigod.work", "+.feilian.cn"]
}
```

**mode 说明**：

| mode | 未命中白名单的流量 | 适用场景 |
|---|---|---|
| `whitelist` | → DIRECT（走直连，GFW 可能拦截） | 节省机场流量，员工只有 WL 内的站需翻墙 |
| `overseas` | → out（走代理） | 透明翻墙，所有海外站走机场 |

---

## PUT /api/whitelist

整体替换白名单（语义：PUT，非 PATCH；先 GET 拿当前值再修改再 PUT）。

```json
{
  "mode": "whitelist",
  "geosites": ["geosite-google", "geosite-anthropic", "geosite-category-ai-!cn"],
  "geoips":   ["geoip-telegram"],
  "domain_suffix": ["claude.ai", "cursor.com"],
  "ip_cidr":  ["149.154.0.0/16", "91.108.0.0/16"],
  "fake_ip_skip": ["+.paigod.work", "+.feilian.cn"]
}
```

**字段说明**：

| 字段 | 层次 | 作用 |
|---|---|---|
| `geosites` | 路由层 | 命中的域名集合 → 走代理（RULE-SET） |
| `geoips` | 路由层 | 命中的 IP 段 → 走代理（适合 Telegram MTProto 等硬编码 IP 场景） |
| `domain_suffix` | 路由层 | 精确域名后缀 → 走代理（补充 geosite 没覆盖的单条域名） |
| `ip_cidr` | 路由层 | 精确 IP 段 → 走代理 |
| `fake_ip_skip` | **DNS 层** | 这些域名返回**真实 IP**（不 fakeip）。用于内网服务：不在 geosite-cn 但解析到 CN/LAN IP，不加就会收到 198.18.x.x 导致直连失败 |

**`fake_ip_skip` 格式**：接受三种写法，服务端自动规范化为 `+.` 前缀：

| 输入 | 存储形式 |
|---|---|
| `paigod.work` | `+.paigod.work` |
| `.feilian.cn` | `+.feilian.cn` |
| `+.company.io` | `+.company.io`（不变） |

**其他字段校验**：
- `geosites` / `geoips` 中的每个 tag 必须在 `/api/rule-sets` 的 catalog 里
- 不在磁盘上的 tag 会按需从 MetaCubeX 拉取（`.mrs`），失败返回 502
- `domain_suffix`：裸域名，无 `://`
- `ip_cidr`：有效 CIDR 或裸 IP（自动补 /32 / /128）

成功触发 **数据面 reload**：当前 `Controller.Reload` 走 `systemctl restart leap-mihomo.service`，约 3-5s 中断。NodeScorer 调整 us-pool 成员是另一条路径，走 mihomo clash-api `PUT /configs?force=true` 热重载，不中断；但白名单/订阅类的整体重新渲染当前不走热重载。

```bash
# 加一条 domain_suffix：先 GET，jq 追加，再 PUT
curl -s http://127.0.0.1:18080/api/whitelist \
  | jq '.domain_suffix += ["new-ai-tool.com"]' \
  | curl -s -H "Content-Type: application/json" -X PUT \
      http://127.0.0.1:18080/api/whitelist -d @-
```

---

## GET /api/whitelist/resolved

将白名单展开为扁平的域名列表 + IP CIDR 列表，供飞连 SaaS「极速模式」消费。

```json
{
  "input": {
    "geosites":      ["geosite-google", "geosite-anthropic"],
    "geoips":        ["geoip-telegram"],
    "domain_suffix": ["claude.ai"],
    "ip_cidr":       []
  },
  "domains":        ["0emm.com", "1e100.net", "anthropic.com", "claude.ai", "..."],
  "ip_cidrs":       ["91.108.0.0/16", "149.154.0.0/16", "..."],
  "domains_count":  2196,
  "ip_cidrs_count": 6694,
  "last_built_at":  "2026-06-05T12:15:16Z",
  "source":         "https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/"
}
```

**数据来源**：
- `domains`：v2fly/domain-list-community（经 jsdelivr CDN，CN 内可达；失败回退 raw.githubusercontent）
- `ip_cidrs`：mihomo 的 `.mrs` 由 leap-gateway 在首次需要时按需从 MetaCubeX `/sing/` 分支拉对应 `.srs` 缓存到 `/var/lib/leap/whitelist-srs-cache/`，再调 `sing-box rule-set decompile`（mihomo 没有 decompile 命令，节点上保留 sing-box CLI 二进制专门干这件事）

**刷新时机**：`PUT /api/whitelist` 后异步触发；不阻塞 PUT 响应。快照有效期内多次 GET 返回缓存。

```bash
# 飞连极速模式集成：取域名和 IP 各一份
curl -s http://127.0.0.1:18080/api/whitelist/resolved | jq -r '.domains[]'
curl -s http://127.0.0.1:18080/api/whitelist/resolved | jq -r '.ip_cidrs[]'
```

---

## GET /api/rule-sets

返回内嵌 catalog 中所有可选的 geosite + geoip tag，含 URL、分类、本地是否已安装、当前是否已启用。

```json
{
  "geosites": {
    "catalog": [
      {
        "name":      "geosite-google",
        "url":       "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geosite/google.srs",
        "category":  "tech",
        "installed": true,
        "selected":  true
      },
      {
        "name":      "geosite-category-ai-!cn",
        "url":       "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geosite/category-ai-!cn.srs",
        "category":  "tech",
        "installed": true,
        "selected":  true
      }
    ],
    "domain_suffix_examples": ["anthropic.com", "claude.ai", "cursor.com"]
  },
  "geoips": {
    "catalog": [
      {
        "name":      "geoip-google",
        "url":       "https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/sing/geo/geoip/google.srs",
        "category":  "app",
        "installed": false,
        "selected":  false
      }
    ],
    "ip_cidr_examples": ["149.154.0.0/16", "91.108.0.0/16"]
  }
}
```

**上游**：MetaCubeX/meta-rules-dat（每日 06:30 CST 自动构建，Loyalsoldier-enhanced）。catalog 里 URL 写的是 `/sing/.../*.srs`；mihomo 引擎的 ruleset manager 在按需拉取时会自动改写为 `/meta/.../*.mrs`（`internal/rulesets.WithEngine("mihomo")`）。`geosite-cn` / `geoip-cn` 不在 catalog 里（路由基建，由 `gateway.yaml` 固定 URL 控制）。

---

## GET /api/geosites

`GET /api/rule-sets` 的兼容别名，仅返回 geosites 部分（老版本客户端使用）。

---

## POST /api/ux-telemetry

**不需要认证**。员工浏览器扩展 / IDE 插件向 gateway 上报 UX 指标。接受单个 event 对象或 event 数组。

```json
// 单个
{"domain":"claude.ai","event_type":"ttfb","duration_ms":280,"client_id":"emp-007"}

// 批量（推荐）
[
  {"domain":"claude.ai","event_type":"ttfb","duration_ms":280,"client_id":"emp-007"},
  {"domain":"chatgpt.com","event_type":"request_error","duration_ms":5000,"detail":"connect timeout"}
]
```

| 字段 | 说明 |
|---|---|
| `domain` | 目标域名，必填，会被 lowercase |
| `event_type` | `ttfb` / `page_load` / `ws_disconnect` / `stream_stall` / `request_error` |
| `duration_ms` | 数值语义随 event_type 变：ttfb=首字节延迟；ws_disconnect=掉线前存活时间；request_error=超时前等待时间 |
| `client_id` | 可选，员工匿名 UUID（客户端本地生成并复用） |
| `detail` | 可选，自由文本，最长 256 字节 |

```json
// 响应 202
{"accepted": 2}
```

**环形缓冲区**：内存保存最近 5000 条事件（约数小时的繁忙流量），重启清零。

---

## GET /api/ux-telemetry

查询原始事件（newest-first）。

**Query params**：
- `limit`：返回条数，默认 100，最大 1000
- `since_sec`：只看最近 N 秒内的事件，默认 3600

```json
{
  "count": 3,
  "events": [
    {
      "time":        "2026-06-05T12:28:19Z",
      "domain":      "anthropic.com",
      "event_type":  "ttfb",
      "duration_ms": 190,
      "client_id":   "emp-007"
    },
    {
      "time":        "2026-06-05T12:28:19Z",
      "domain":      "claude.ai",
      "event_type":  "ws_disconnect",
      "duration_ms": 12000,
      "client_id":   "emp-008",
      "detail":      "code 1006"
    }
  ]
}
```

---

## GET /api/ux-telemetry/summary

按域名聚合统计，适合 ops 看板。

**Query param**：
- `window_sec`：统计时间窗口（秒），默认 3600

```json
{
  "window_sec": 3600,
  "domains": [
    {
      "domain":       "claude.ai",
      "event_count":  47,
      "ttfb_p50_ms":  285,
      "ttfb_p95_ms":  520,
      "error_rate":   0.04
    },
    {
      "domain":      "chatgpt.com",
      "event_count": 12,
      "ttfb_p50_ms": 310,
      "ttfb_p95_ms": 680,
      "error_rate":  0.08
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `ttfb_p50_ms` | 首字节延迟中位值（仅统计 event_type=ttfb） |
| `ttfb_p95_ms` | 首字节延迟 95 分位 |
| `error_rate` | request_error 事件占比（0~1） |

---

## 常用操作速查

```bash
BASE=http://192.168.70.89:18080

# 整体状态
curl -s $BASE/api/status | jq .

# 查看动态节点评分
curl -s $BASE/api/nodes/health | jq '{qualified,total,last_scored_at}'

# 哪些节点在池子里 / 谁被踢出
curl -s $BASE/api/nodes/health | jq '.nodes[] | select(.in_pool) | .name'
curl -s $BASE/api/nodes/health | jq '.nodes[] | select(.qualified|not) | {name,reason}'

# 活跃代理详情（出口 IP、RTT、池子）
curl -s $BASE/api/proxies/active | jq '{active_proxy:.active_proxy}' 

# 添加订阅
curl -s -X POST -H 'Content-Type: application/json' $BASE/api/subscriptions \
  -d '{"name":"new-airport","url":"https://...","user_agent":"clash.meta/v1.19.26"}'

# 加一个域名进白名单
# 加一条 domain_suffix（境外站走代理）
curl -s $BASE/api/whitelist \
  | jq '.domain_suffix += ["new-ai-tool.com"]' \
  | curl -s -H 'Content-Type: application/json' -X PUT $BASE/api/whitelist -d @-

# 加内网域名到 fake_ip_skip（不走 fakeip，返回真实 IP）
curl -s $BASE/api/whitelist | jq '.fake_ip_skip += ["+.paigod.work"]' \
  | curl -s -H 'Content-Type: application/json' -X PUT $BASE/api/whitelist -d @-

# 查看 UX 遥测摘要（过去 1h）
curl -s "$BASE/api/ux-telemetry/summary" | jq '.domains[:5]'

# 飞连极速模式：取域名列表
curl -s $BASE/api/whitelist/resolved | jq -r '.domains[]' | head -20
```

---

## 控制面建议（管理 UI 方案）

当前 gateway 提供了足够的 API 来支持一个完整的 **Web 控制台**。如果要做，以下是推荐的页面和数据来源：

### 首页 Dashboard

| 卡片 | 数据来源 | 刷新频率 |
|---|---|---|
| 节点总数 / qualified 数 | `/api/status` + `/api/nodes/health` | 30s |
| 当前活跃出口 IP | `/api/proxies/active.active_proxy` | 30s |
| 最近 1h UX 错误率 TOP 3 域名 | `/api/ux-telemetry/summary` | 60s |
| 订阅 fetch 状态（最后刷新时间 + 节点数变化） | `/api/subscriptions` | 60s |

### 节点健康页

实时表格，每列：`name / sub / p50 / p95 / jitter / fail_rate / throughput / in_pool / strikes / reason`。

- 颜色标记：green=in_pool, yellow=qualified but high strikes, red=not qualified
- 顶部指标：qualified/total + 最近 pool 变化时间
- 数据来源：`GET /api/nodes/health`，30s 轮询

### 白名单管理页

- 左侧：已选 rule-set tags（checkbox 树，按 tech/social/streaming/reference/productivity 分组）
- 右侧：domain_suffix 和 ip_cidr 自由输入框
- 底部：当前 mode 切换（whitelist / overseas）
- 数据：`GET /api/rule-sets` 渲染 catalog，`GET /api/whitelist` 回填选中态，`PUT /api/whitelist` 保存
- 飞连集成：「复制极速模式域名/IP」按钮，调 `/api/whitelist/resolved`

### 订阅管理页

- 表格：name / url(脱敏) / ua / nodes_count / last_refresh / 操作按钮
- 新增对话框：name/url/ua 三字段
- 修改对话框：同上（编辑 url/ua）
- 「立即刷新」按钮：`POST /api/subscribe/refresh`
- 数据：全 CRUD 走 `/api/subscriptions*`

### UX 遥测页

- 折线图：过去 24h 各主要域名的 ttfb_p50 趋势（需要定时采样 `/api/ux-telemetry/summary` 保存到本地 timeseries）
- 错误率热力表：域名 × 时段
- 原始事件日志：`GET /api/ux-telemetry?limit=100&since_sec=3600`
- **需要客户端侧配套**（浏览器扩展 / IDE 插件）向 `POST /api/ux-telemetry` 上报；无上报则页面无数据

### 分发实测（开发者工具页）

- 输入 N 个目标域名 → 并发请求（走 11080 proxy）→ 分析 chain，展示每个 dst 落到哪个节点
- 数据：前端发请求 + 抓 `/connections` 差分
- 价值：快速验证负载分散是否正常

---

## 接入变更日志（控制平台 / 前端集成）

按发布周期组织。**只列对外暴露的 API/JSON 字段变化**，内部代码重构不在此处记录。

### 2026-06-06 — sing-box 引擎退役 + schema 清理（commit `bf6e610`）

**Breaking — 字段移除**：

| 端点 | 字段 | 处理 |
|---|---|---|
| `/api/proxies/active` | `watchdog`（顶级对象，含 `enabled / current_node / consecutive_healthy`） | **整段删除**。前端如有"看门狗状态"卡片，移除即可——mihomo 的 load-balance 自带 per-flow 健康检查，原 watchdog 在 mihomo 模式下已经 dead code |
| `/api/proxies/active` | `leap.services.leap-singbox` | **不再返回**。services 现在只列 `leap-gateway` / `leap-mihomo` / `leap-nft`。前端遍历这个 map 即可，不要 hardcode key |

**前端 grep 替换清单**：
- `data.watchdog`、`response.watchdog.*` → 全部删除引用
- `services["leap-singbox"]` → 不再返回；如果前端要统计"sing-box 是否在跑"，**永远是不在跑**，可直接删该 UI 元素

**新增字段**（schema 定义已经合入，但运行时返回需等下面"即将发布"的 1.1 / 1.3 上线）：
- `/api/nodes/health` 将在 1.3 落地后增加 `probes` 顶级对象 + 每节点 `probes` 字段（见下文 schema 预览）
- `/api/proxies/active` 将在 1.1 落地后在 `pools[]` 数组里出现 `openai-pool` 等命名池

### 2026-06-05 — 引擎相关字段重命名（commit `bf0b469`）

之前几次小迭代的字段改名总账（如果还没跟上）：

| 端点 | 旧字段 | 新字段 | 备注 |
|---|---|---|---|
| `/api/status` | `singbox_ok` | `engine_ok` | 含义不变（数据面 clash-api 可达） |
| `/api/proxies/active` | `leap.singbox_version` | `leap.engine_version` + 新增 `leap.engine` | 旧字段返回的是 `/usr/local/bin/sing-box --version` 输出（误导性，mihomo 模式下报的是 sing-box CLI 工具版本，不是数据面）；新字段 `engine_version` 报真正在跑的引擎版本，`engine` 报引擎名（恒为 `"mihomo"`） |

**前端 grep 替换清单**：
- `singbox_ok` → `engine_ok`
- `singbox_version` → `engine_version`（语义变更：现在是 mihomo 版本而不是 sing-box CLI 版本）
- 显示"引擎"字段：直接用 `engine`（恒等于 `"mihomo"`，可省略整段 UI 也可）

### 即将发布（schema 已定，运行时未上）

#### 1.3 — `/api/nodes/health` 增加 site-specific probes

每个节点新增 `probes` 字段，报告对各 probe 目标的可达性：

```json
{
  "qualified": 17, "total": 22,
  "last_pool_update": "...", "last_scored_at": "...",
  "thresholds": { ... 现有 ... },

  // 新顶级字段：各 probe 全局摘要，前端可做总览卡片
  "probes": {
    "chatgpt.com":         {"url": "https://chatgpt.com/",        "interval_sec": 300, "passing": 8,  "total": 22},
    "claude.ai":           {"url": "https://claude.ai/",          "interval_sec": 300, "passing": 14, "total": 22},
    "accounts.google.com": {"url": "https://accounts.google.com/","interval_sec": 300, "passing": 20, "total": 22},
    "api.github.com":      {"url": "https://api.github.com/",     "interval_sec": 300, "passing": 19, "total": 22}
  },

  "nodes": [
    {
      "name": "ctc-02/US-C30-01-DMIT", "sub": "ctc-02",
      "rtt_p50_ms": 154, "rtt_p95_ms": 165, "jitter_ms": 5,
      "fail_rate": 0.0, "in_pool": true, "qualified": true,
      "strikes": 0, "ok_rounds": 12, "reason": "",

      // 新字段：每节点的 probes 结果（key = 全名 hostname）
      "probes": {
        "chatgpt.com": {
          "ok":              true,
          "status_code":     200,
          "cf_mitigated":    false,
          "latency_ms":      280,
          "last_checked_at": "2026-06-06T12:35:42Z"
        },
        "claude.ai":           { "ok": true, "status_code": 200, "cf_mitigated": false, "latency_ms": 312, "last_checked_at": "..." },
        "accounts.google.com": { "ok": true, "status_code": 200, "cf_mitigated": false, "latency_ms": 198, "last_checked_at": "..." },
        "api.github.com":      { "ok": true, "status_code": 200, "cf_mitigated": false, "latency_ms": 234, "last_checked_at": "..." }
      }
    },
    {
      "name": "cyberguard/🇺🇸美国 01 | 1X | CMI 01",
      "rtt_p50_ms": 356,
      "in_pool": true,
      "probes": {
        "chatgpt.com": {
          "ok":              false,
          "status_code":     200,    // CF 给了 200 但
          "cf_mitigated":    true,   //   header 里有 cf-mitigated: challenge → 算失败
          "latency_ms":      850,
          "last_checked_at": "...",
          "last_error":      "cloudflare challenge (cf-mitigated: challenge)"
        },
        "claude.ai":     { "ok": true,  "status_code": 200, "latency_ms": 720 },
        "accounts.google.com": { "ok": true, "status_code": 200, "latency_ms": 380 },
        "api.github.com":      { "ok": false, "status_code": 0, "latency_ms": 10003,
                                 "last_error": "context deadline exceeded" }
      }
    }
  ]
}
```

字段语义参考：

| 字段 | 含义 |
|---|---|
| `probes.<host>.url` / `interval_sec` | 该 probe 的元信息 |
| `probes.<host>.passing` / `total` | 集群层面通过 / 总数 |
| `nodes[].probes.<host>.ok` | 综合 status_code + cf_mitigated 判定的"是否合格" |
| `nodes[].probes.<host>.status_code` | HTTP 状态码（0 = 没拿到响应） |
| `nodes[].probes.<host>.cf_mitigated` | CF challenge header 命中（`cf-mitigated: challenge`） |
| `nodes[].probes.<host>.latency_ms` | 探测耗时；失败时是真等了多久（如 timeout 10s = 10003ms），不是 0 |
| `nodes[].probes.<host>.last_checked_at` | RFC3339 时间戳 |
| `nodes[].probes.<host>.last_error` | 失败原因（空 = 没失败过） |

**前端可视化建议**：节点表格加 4 列状态点（每个 probe 一列），绿/灰/红，hover 显示 latency + last_error。

#### 1.1 — `/api/proxies/active.pools[]` 增加命名池

部署 `pools` 配置后（如 `openai-pool`），`active_proxy.pools` 数组会出现额外条目：

```json
"active_proxy": {
  "active_urltest": "us-pool",
  "pools": [
    { "tag": "us-pool",     "pool_size": 17, "active": true,  "nodes": [...] },
    { "tag": "openai-pool", "pool_size": 4,  "active": false, "nodes": [...] }
  ]
}
```

新池子的 `active=false` 是正常状态——只有 `out` selector 当前指向的池子 `active=true`。`openai-pool` 的成员只在 ChatGPT/Anthropic 这种 rule-set 命中时被路由进来，不出现在 `active_proxy.now`。

**前端可视化建议**：池子列表分两列展示（默认池 vs 命名池），不要把 `openai-pool.active=false` 展示成"故障"。

### 抽象层稳定性约定

下面这些约定不会随版本变化：

- **JSON key 命名规则**：`snake_case`，host 全名作 key 时含点（如 `"chatgpt.com"`），数值字段用 `_ms` / `_sec` / `_bps` 后缀标单位
- **时间戳**：所有 `*_at` / `last_*` 字段都是 RFC3339（带时区）
- **数组顺序**：`/api/nodes/health.nodes` 排序为"qualified 优先，再按 RTT 升序"；`/api/proxies/active.pools` 顺序为"`out` selector 的 `all` 列表顺序"
- **可选字段**：返回里没有的 key 用 `?:` 处理，不要假设永远存在（如 `pools[].egress` 在 cold start 时缺失）
- **breaking change 走这一节**：本文档每次 commit 都会在"接入变更日志"加一行；前端 CI 可以 grep `## 接入变更日志` 后的最新条目确认

---

*最后更新：2026-06-06，基于 mihomo-only 清理后版本（删除 sing-box-as-engine + watchdog；`data_plane:` / `node_qualify:` / `pools:` 三个顶级 block）*
