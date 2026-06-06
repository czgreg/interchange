# Leap Gateway API

控制面 HTTP API。base URL：`http://<node-ip>:18080`（端口由 `api.listen` 控制）。

---

## 认证

`gateway.yaml` 配置 `api.token` 后，除 `/healthz` 外所有端点需要：

```
Authorization: Bearer <token>
```

token 为空时不鉴权。

---

## 端点速览

| 方法 | 路径 | 功能 | 认证 |
|---|---|---|---|
| GET | `/healthz` | 可达性探测 | 无 |
| GET | `/api/status` | 节点总体状态 + 容量 | ✓ |
| GET | `/api/nodes` | 全量节点列表 | ✓ |
| GET | `/api/nodes/health` | NodeScorer 评分 + probe + passive stats | ✓ |
| GET | `/api/proxies/active` | 数据面活跃状态快照 | ✓ |
| POST | `/api/proxies/select` | 手动切换 selector 成员 | ✓ |
| GET | `/api/subscriptions` | 订阅列表 | ✓ |
| POST | `/api/subscriptions` | 新增订阅 | ✓ |
| PUT | `/api/subscriptions/{name}` | 修改订阅 | ✓ |
| DELETE | `/api/subscriptions/{name}` | 删除订阅 | ✓ |
| POST | `/api/subscribe/refresh` | 立即刷新订阅 | ✓ |
| GET | `/api/subscribe/refresh-interval` | 查看刷新间隔 | ✓ |
| PUT | `/api/subscribe/refresh-interval` | 修改刷新间隔 | ✓ |
| GET | `/api/whitelist` | 白名单配置 | ✓ |
| PUT | `/api/whitelist` | 整体替换白名单 | ✓ |
| GET | `/api/whitelist/resolved` | 展开为域名 + CIDR 列表 | ✓ |
| GET | `/api/rule-sets` | 可用 geosite/geoip catalog | ✓ |
| GET | `/api/geosites` | 同 /api/rule-sets（兼容别名） | ✓ |

---

## GET /healthz

```json
{"ok": true}
```

---

## GET /api/status

```json
{
  "engine_ok":       true,
  "last_refresh":    "2026-06-06T12:00:00Z",
  "node_count":      136,
  "subscriptions":   4,
  "pool_qualified":  19,
  "pool_total":      22,
  "pool_last_update":"2026-06-06T11:58:00Z",
  "capacity": {
    "sustained_max_users": 50,
    "degraded_max_users":  150,
    "measured_at":         "2026-06-06",
    "measured_with":       "stress.sh / mihomo v1.19.26"
  }
}
```

| 字段 | 说明 |
|---|---|
| `engine_ok` | mihomo clash-api `/version` 可达 |
| `pool_qualified/total` | NodeScorer 当前合格/候选节点数 |
| `pool_last_update` | us-pool 成员最近一次变更时间 |
| `capacity` | 离线压测得出的单机上限，未配置时不返回 |

---

## GET /api/nodes

全量节点列表（含非 US 节点）。

```json
[
  {"tag": "ctc-02/US-C30-01", "type": "vless", "server": "us01.example.com", "source": "ctc-02"}
]
```

---

## GET /api/nodes/health

NodeScorer 评分快照。503 = `node_qualify.enabled=false`。

```json
{
  "qualified": 19, "total": 22,
  "last_pool_update": "...", "last_scored_at": "...",
  "thresholds": {"MaxRTTP50Ms":500, "MaxRTTP95Ms":800, "MaxJitterMs":300, "MaxFailRate":0.25, ...},
  "probes": {
    "openai-api": {"url":"https://api.openai.com/v1/models", "interval_sec":300, "passing":19, "total":22}
  },
  "nodes": [
    {
      "name": "ctc-02/US-C30-01", "sub": "ctc-02",
      "alive": true,
      "rtt_p50_ms": 155, "rtt_p95_ms": 165, "jitter_ms": 10,
      "fail_rate": 0.0, "throughput_bps": 0,
      "qualified": true, "in_pool": true,
      "strikes": 0, "ok_rounds": 12, "reason": "",
      "probes": {
        "openai-api": {
          "ok": true, "status_code": 401, "cf_mitigated": false,
          "latency_ms": 320, "last_checked_at": "2026-06-06T12:00:00Z"
        }
      },
      "passive": {
        "active_conns": 3, "closed_window": 12, "failed_window": 0,
        "fail_rate": 0.0, "sample_window_sec": 600
      }
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `probes` | 顶级：各 probe 全局通过率；节点级：每个 probe 的最新结果 |
| `probes[].cf_mitigated` | CF challenge header 命中（status 200 但实为 challenge）→ ok=false |
| `probes[].latency_ms` | 真实等待时长，失败时为实际超时值（如 10003ms），不是 0 |
| `passive` | 从 `/connections` 被动推导，纯本地无机场流量；无连接时不返回 |
| `passive.fail_rate` | 关闭时传输 < 2KB 的连接占比（可能 RST / dead egress） |

---

## GET /api/proxies/active

数据面活跃状态快照。

```json
{
  "node": {"hostname": "dianwei", "kernel": "...", "os": "Ubuntu 22.04", "uptime_seconds": 86400},
  "feilian": {"tun0_active": true, "vpn_active": true},
  "leap": {
    "gateway_version": "0d7432d",
    "engine": "mihomo",
    "engine_version": "v1.19.26",
    "services": {"leap-gateway": "active", "leap-mihomo": "active", "leap-nft": "active"},
    "subscriptions_count": 4,
    "nodes_parsed": 136,
    "last_refresh": "2026-06-06T12:00:00Z"
  },
  "active_proxy": {
    "reachable": true,
    "active_urltest": "us-pool",
    "pool_filter": "美国|🇺🇸|\\bUS",
    "pools": [
      {
        "tag": "us-pool", "pool_size": 19, "active": true,
        "egress": {"ip": "12.34.56.78", "country": "US", "asn": "AS???"},
        "nodes": [
          {"tag": "ctc-02/US-C30-01", "delay_ms": 155, "last_check": "..."}
        ]
      }
    ]
  }
}
```

---

## POST /api/proxies/select

手动切换某个 Selector 组到指定成员。

```json
{"selector": "out", "name": "pin"}
{"selector": "pin", "name": "ctc-02/US-C30-01"}
{"selector": "out", "name": "DIRECT"}
```

成功返回 200 + `/api/proxies/active` 当前快照。失败：400（selector 类型不是 Select）、404（selector 或 name 不存在）。

override 不 sticky：NodeScorer 只管 us-pool 成员，不回写 selector 指针。

---

## GET /api/subscriptions

```json
[
  {
    "name": "ctc-02",
    "url": "https://47.x.x.x:9999/...?token=c85b***0f02",
    "enabled": true,
    "format": "auto",
    "user_agent": "sing-box/1.10.7",
    "nodes_count": 6,
    "last_refresh": "2026-06-06T12:00:00Z"
  }
]
```

## POST /api/subscriptions

```json
{"name": "new-sub", "url": "https://...", "format": "auto", "user_agent": "clash.meta/v1.19.26"}
```

成功 201 + 完整列表。立即触发 refresh + reload（~3-5s 中断）。

## PUT /api/subscriptions/{name}

```json
{"url": "https://...", "user_agent": "clash.meta/v1.19.26"}
```

## DELETE /api/subscriptions/{name}

204。立即触发 refresh + reload。

---

## POST /api/subscribe/refresh

立即拉取所有订阅 → 渲染 → 重启 mihomo。

```json
{"ok": true}
```

## GET /api/subscribe/refresh-interval

```json
{"seconds": 1800}
```

## PUT /api/subscribe/refresh-interval

```json
{"seconds": 900}
```

0 = 关闭定时刷新。

---

## GET /api/whitelist

```json
{
  "mode": "whitelist",
  "geosites": ["geosite-google", "geosite-openai"],
  "geoips": ["geoip-telegram"],
  "domain_suffix": ["ipinfo.io"],
  "ip_cidr": ["149.154.0.0/16"],
  "fake_ip_skip": ["+.paigod.work"]
}
```

`mode`: `whitelist`（只有命中列表的海外域名走机场）| `overseas`（所有非 CN 走机场）

## PUT /api/whitelist

整体替换（PUT 语义，不是 PATCH）。body 同 GET 结构。

`fake_ip_skip` 写法：`paigod.work` / `.paigod.work` / `+.paigod.work` 均可，统一规范化为 `+.` 前缀。

`geosites` / `geoips` 中每个 tag 必须在 `/api/rule-sets` catalog 中；不在磁盘则按需从 MetaCubeX 拉取（.mrs），失败 502。

成功触发 mihomo 重启（~3-5s 中断）。

```bash
# 追加一条 domain_suffix
curl -s http://127.0.0.1:18080/api/whitelist \
  | jq '.domain_suffix += ["new-tool.com"]' \
  | curl -s -X PUT -H 'Content-Type: application/json' \
      http://127.0.0.1:18080/api/whitelist -d @-
```

## GET /api/whitelist/resolved

展开为扁平域名 + IP CIDR 列表，供飞连"极速模式"消费。

```json
{
  "input": {"geosites": [...], "geoips": [...], "domain_suffix": [...], "ip_cidr": []},
  "domains": ["0emm.com", "1e100.net", "..."],
  "ip_cidrs": ["149.154.0.0/16", "..."],
  "domains_count": 2460,
  "ip_cidrs_count": 6694,
  "last_built_at": "2026-06-06T12:00:00Z",
  "source": "https://cdn.jsdelivr.net/gh/v2fly/domain-list-community@master/data/"
}
```

---

## GET /api/rule-sets

可用的 geosite + geoip tag catalog（含安装状态）。

```json
{
  "geosites": {
    "catalog": [
      {"name":"geosite-google", "category":"tech", "installed":true, "selected":true}
    ]
  },
  "geoips": {
    "catalog": [
      {"name":"geoip-telegram", "category":"app", "installed":true, "selected":true}
    ]
  }
}
```

`geosite-cn` / `geoip-cn` 是路由基建，不在 catalog 中。

`GET /api/geosites` 是此端点的兼容别名，只返回 geosites 部分。

---

## 错误码

| 状态 | 含义 |
|---|---|
| 400 | 请求体非法 / 字段格式错 / tag 不在 catalog |
| 401 | token 缺失或不匹配 |
| 404 | 资源不存在 |
| 409 | 资源冲突（订阅名重复） |
| 500 | 持久化 / 渲染 / reload 失败 |
| 502 | 按需拉取 .mrs 失败 |
| 503 | 功能未启用（NodeScorer 未激活） |

---

## 常用速查

```bash
BASE=http://192.168.70.89:18080

# 状态
curl -s $BASE/api/status | jq .

# 节点评分（含 probe 结果）
curl -s $BASE/api/nodes/health | jq '{qualified, total, last_scored_at}'

# 哪些节点在 pool / 被踢出原因
curl -s $BASE/api/nodes/health | jq '.nodes[] | select(.in_pool) | {name, rtt_p50_ms}'
curl -s $BASE/api/nodes/health | jq '.nodes[] | select(.qualified|not) | {name, reason}'

# probe 结果：哪些节点能过 OpenAI
curl -s $BASE/api/nodes/health | jq '.nodes[] | {name, ok: .probes["openai-api"].ok}'

# 切 out → pin（手动指定出口）
curl -s -X POST -H 'Content-Type: application/json' $BASE/api/proxies/select \
  -d '{"selector":"out","name":"pin"}'
curl -s -X POST -H 'Content-Type: application/json' $BASE/api/proxies/select \
  -d '{"selector":"pin","name":"ctc-02/US-C30-01"}'

# 恢复 load-balance
curl -s -X POST -H 'Content-Type: application/json' $BASE/api/proxies/select \
  -d '{"selector":"out","name":"us-pool"}'

# 立即刷新订阅
curl -s -X POST $BASE/api/subscribe/refresh | jq .

# 追加 fake_ip_skip 内网域名
curl -s $BASE/api/whitelist \
  | jq '.fake_ip_skip += ["+.corp.example.com"]' \
  | curl -s -X PUT -H 'Content-Type: application/json' $BASE/api/whitelist -d @-

# 极速模式域名列表
curl -s $BASE/api/whitelist/resolved | jq -r '.domains[]' | head -20
```

---

## 接入变更日志（前端 / 控制平台）

### 2026-06-06（commit `21a88d5`）— 当前版本

**Breaking**：

| 端点 / 路径 | 变化 |
|---|---|
| `POST /api/ux/event`、`GET /api/ux/events`、`GET /api/ux/stats` | **已删除**（commit `9bb5ee9`，整个 ux-telemetry 子系统下线，前端勿再上报） |

**配置相关（影响部署，但 REST 表面不变）**：

- `data_plane.tproxy_port` 现在是**全局必填**（>0）。`--validate` 会拒绝 `tproxy_port: 0`，TUN-only 模式不再支持（之前只在 `load_balance.per_terminal=true` 时强制）。
- 部署链增加 `--validate`：`mihomo -t` 在临时目录跑，永远不写生产 `config.yaml`。
- install.sh 增加 `--print-env` 生成 `/etc/leap/env`，nft 模板和 `iproute.sh` 都从这里读，不再 grep YAML。

### 2026-06-06（commit `0d7432d`）

**新增字段**：

| 端点 | 字段 | 说明 |
|---|---|---|
| `/api/nodes/health` | `probes`（顶级 + 节点级） | site-specific probe 结果；`cf_mitigated` 检测 CF challenge；`latency_ms` 失败时为真实等待时长 |
| `/api/nodes/health` | `nodes[].passive` | 被动连接统计：`active_conns / closed_window / failed_window / fail_rate / sample_window_sec` |
| `/api/status` | `capacity`（条件返回） | 压测天花板：`sustained/degraded_max_users / measured_at / measured_with` |

**Breaking（上轮已通知）**：

| 字段 | 变化 |
|---|---|
| `singbox_ok` | → `engine_ok` |
| `leap.singbox_version` | → `leap.engine_version` + 新增 `leap.engine`（恒为 `"mihomo"`） |
| `active_proxy.watchdog` | **已删除** |
| `leap.services.leap-singbox` | **不再返回**（services 只剩 gateway/mihomo/nft） |

**约定**：
- 字段命名 `snake_case`；时间戳 RFC3339；数值后缀标单位（`_ms` / `_sec` / `_bps`）
- 可选字段缺失时不返回（用 `?.` / nullish 处理）
- breaking change 一律在此节记录
