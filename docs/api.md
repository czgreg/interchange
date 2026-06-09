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
| GET | `/api/pool/state` | 手动池 yaml baseline + 当前 effective + emergency 事件 | ✓ |
| POST | `/api/pool/clear-emergency` | 还原 effective 池到 yaml baseline | ✓ |
| GET | `/api/pool/terminal?ip=X` | 查询某 terminal IP 的出口节点 + 健康 | ✓ |

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
    "measured_at":         "2026-06-07",
    "measured_with":       "stress.sh on 89+92 / mihomo v1.19.26 / 941d55f / TPROXY+per_terminal"
  }
}
```

| 字段 | 说明 |
|---|---|
| `engine_ok` | mihomo clash-api `/version` 可达 |
| `pool_qualified/total` | 当前 us-pool 成员数 / NodePattern-匹配的候选总数。post Plan-A：us-pool = 全部候选，所以稳态下两者相等；不等只发生在订阅刚增减节点 + 下一轮 scoring 之间（数秒内）。 |
| `pool_last_update` | us-pool 成员最近一次变更时间。变更频率 ≈ 订阅增减节点频率（小时-天级）。 |
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

post Plan-A 语义注意：
- `qualified` 与 `in_pool` 解耦了。`qualified` 是节点的"诊断口径"（基于探针窗口判断该节点活/通），`in_pool` 是 us-pool 的实际成员资格。**us-pool = 全部候选节点（只要 NodePattern 匹配），永远不剔除**；mihomo 的 LoadBalance + 30s 健康检查在 per-flow 层自动跳过死节点。所以 `in_pool=true qualified=false` 是正常状态（节点暂时探针失败但留在 pool 里）。
- `thresholds` 块（`MaxRTTP50Ms / MaxRTTP95Ms / MaxJitterMs / MaxFailRate`）**仅为观察用**，不再 gate us-pool。要看一个节点的真实 RTT/抖动表现就读这些字段；要看是否被 mihomo 路由就读 `in_pool`。
- 命名池（如 openai-pool）成员资格仍然由探针 gating，但用 K=3 滑动窗口 + 多数规则：节点要 ≥ 2/3 探针通过才进，要 ≥ 2/3 探针失败才掉出。单次 CF 噪声不再翻转命名池。

```json
{
  "qualified": 18, "total": 19,
  "last_pool_update": "...", "last_scored_at": "...",
  "thresholds": {"MaxRTTP50Ms":500, "MaxRTTP95Ms":800, "MaxJitterMs":300, "MaxFailRate":0.25, ...},
  "probes": {
    "openai-api": {"url":"https://api.openai.com/v1/models", "interval_sec":300, "passing":18, "total":19}
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
| `qualified` | 该节点最近 5 个 mihomo 探针里是否有任何成功。仅为诊断字段，不影响 us-pool 成员。 |
| `in_pool` | 实际是否在 us-pool 中。post Plan-A：所有 NodePattern 匹配的候选都是 true。 |
| `reason` | 不合格原因。常见值："dead"（探针全失败 + alive=False）、"no successful probes in last 5"（窗口内全失败）、"no probe history yet (benefit of doubt)"（首次进池前的过渡状态，仍计为 qualified=true）。 |
| `strikes` / `ok_rounds` | 连续不合格 / 合格的 scoring 轮数，仅诊断用。 |
| `probes` | 顶级：各 probe 全局通过率；节点级：每个 probe 的最新结果。命名池 gating 用 K=3 窗口的多数规则，不只看最新结果。 |
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
{"seconds": 0}
```

返回当前 scheduler 的间隔。post 2026-06-07 example.yaml 默认 0（关闭周期刷新，靠手动 `POST /api/subscribe/refresh` 触发）；老节点的 yaml 可能仍显式配置非零值。理由见 `configs/gateway.example.yaml` 注释。

## PUT /api/subscribe/refresh-interval

```json
{"seconds": 900}
```

0 = 关闭定时刷新。立即生效 + 持久化到 `/etc/leap/gateway.yaml`（重启后保留）。

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

## GET /api/pool/state

返回手动池（`pool_mode: manual`）的当前状态：yaml 中声明的 baseline、运行时实际生效的 effective、二者是否分离（emergency 事件导致漂移），以及 emergency 历史事件日志。

```json
{
  "mode": "manual",
  "yaml_baseline": [
    "ash/🇺🇸US-IEPL-01",
    "fishcloud/🇺🇸 美国01"
  ],
  "effective_pool": [
    "ash/🇺🇸US-IEPL-01",
    "fishcloud/🇺🇸 美国02"
  ],
  "diverged": true,
  "promote_chain": [
    "fishcloud/🇺🇸 美国02",
    "ctc-02/US-C30-02-BWH"
  ],
  "emergency_events": [
    {
      "at": "2026-06-09T08:30:12Z",
      "type": "evict",
      "node": "fishcloud/🇺🇸 美国01",
      "reason": "fail_rate >= 0.90 sustained 30m0s"
    },
    {
      "at": "2026-06-09T08:30:12Z",
      "type": "promote",
      "node": "fishcloud/🇺🇸 美国02",
      "reason": "auto-promote from emergency_promote_chain (replacing fishcloud/🇺🇸 美国01)"
    }
  ]
}
```

字段：
- `mode` — `manual` / `auto` / `""`（未配置）。后续端点的语义只在 `manual` 下完全适用。
- `yaml_baseline` — `gateway.yaml` 中 `node_qualify.pool_members` 的副本，按字典序。
- `effective_pool` — 当前真正给 perterm 用的池成员列表，按字典序。
- `diverged` — `effective_pool != yaml_baseline` 时为 true，表示 emergency 事件已经改变池组成，等 ops 介入。
- `promote_chain` — `node_qualify.emergency_promote_chain`，emergency 顶替时按顺序选下一个不在池里的候选。
- `emergency_events` — 最近 50 条事件。`type` 取值 `evict` / `promote` / `exhausted` / `clear`。

503：scorer 未启用。

---

## POST /api/pool/clear-emergency

将 `effective_pool` 还原到 `yaml_baseline`，清空 emergency 事件历史，重置每个节点的 hard-fail 计时器。等价于"运维已经处理完故障，让系统回到 yaml 声明的状态"的显式信号。

```bash
curl -X POST -H "Authorization: Bearer ..." \
  http://leap-89:18080/api/pool/clear-emergency
```

返回新的 `/api/pool/state` body 供确认。下一个 scoring round 检测到 effective 变化会触发一次 mihomo hot-reload。

要点：
- 这个端点**不会**修改 yaml 文件。yaml 是 ground truth，只有 ops 改 yaml + redeploy 才能修改 baseline。
- 如果故障节点仍在 hard-fail（fail_rate ≥ 0.9），重置后 30 分钟内仍会再次触发 emergency，**会绕回相同的 effective 状态**。先修复或换池成员再 clear。

---

## GET /api/pool/terminal

按 terminal IP 查询当前出口节点分配 + 各节点的健康数据。第三方对接、用户故障排查的主要入口。

```bash
curl -H "Authorization: Bearer ..." \
  "http://leap-89:18080/api/pool/terminal?ip=10.8.13.42"
```

```json
{
  "ip": "10.8.13.42",
  "subnet": "10.8.13.0/24",
  "in_subnet": true,
  "group_name": "fb-10.8.13.42",
  "pool_mode": "manual",
  "active_node": "primary",
  "primary": {
    "name": "fishcloud/🇺🇸 美国01",
    "alive": true,
    "health": {
      "name": "fishcloud/🇺🇸 美国01",
      "rtt_p50_ms": 246,
      "rtt_p95_ms": 252,
      "jitter_ms": 6,
      "fail_rate": 0.0,
      "probe_count": 10,
      "qualified": true,
      "in_pool": true,
      "probes": {
        "openai-api":     {"ok": true,  "status_code": 401, "latency_ms": 224},
        "anthropic-api":  {"ok": true,  "status_code": 401, "latency_ms": 292},
        "api.github.com": {"ok": false, "status_code": 403, "last_error": "status Forbidden"}
      },
      "passive": {
        "active_conns": 3,
        "closed_window": 24,
        "failed_window": 1,
        "fail_rate": 0.04,
        "sample_window_sec": 600
      }
    }
  },
  "secondary": {
    "name": "ash/🇺🇸US-IEPL-02",
    "alive": true,
    "health": { "...": "..." }
  }
}
```

字段：
- `ip` — 入参 IP。回显方便日志。
- `subnet` — 节点配置的 `client_subnet`（如 `10.8.13.0/24`）。
- `in_subnet` — `false` 表示该 IP 不在 client_subnet，会走 `MATCH,us-pool` 兜底，**没有**专属的 fb-<ip> 组。下面的 primary/secondary 字段缺省。
- `group_name` — mihomo 中对应的 fallback 组名（`fb-<ip>` 形式）。可以直接用于 mihomo clash-api 调试。
- `pool_mode` — 当前 `node_qualify.pool_mode`。
- `active_node` — `primary` / `secondary` / `""`（都不可用）。系统按 mihomo fallback 语义判断：primary alive=true 时返回 primary，否则 secondary 替补。
- `primary.name` — HRW top-1 节点名。
- `primary.alive` — mihomo url-test 的最新探针结果。
- `primary.health` — 完整 NodeHealth 嵌入：rtt_p50/p95、jitter、fail_rate、probe_count、命名探针每个目标的最近一次状态、被动连接生命周期统计（`passive`）。
- `secondary.*` — 同 primary，HRW top-2。当前实现固定每个 fb 组 2 个成员。

错误：
- 400：`?ip=` 缺失。
- 503：scorer 未启用。
- 200 + `in_subnet: false`：传入了 client_subnet 之外的 IP，正常返回但没有 per-terminal 分配信息。

第三方对接建议：
- 用户报告"访问慢"时，先调一次此端点，立刻拿到 (primary, secondary) + 它们的 probe / passive 数据，足够判定问题来自机场端还是终端端。
- `active_node == ""` 表示这个 terminal 当前两个备份都不可用。配合 `/api/pool/state` 看 emergency 事件，决定是 hot-reload 还是手动改 yaml。
- HRW 是确定性的——同一 IP + 同一池成员永远同一答案。换池成员后，只有原本被分到被换节点的 terminal 会有新分配。

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

# 当前 us-pool 成员（post Plan-A：通常 = 全部候选）
curl -s $BASE/api/nodes/health | jq '.nodes[] | select(.in_pool) | {name, rtt_p50_ms}'

# 哪些节点最近探针失败 (qualified=false ≠ 被踢出，pool 永远全员)
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

### 2026-06-09（commit `49ece0e`+）— 当前版本

**新增端点**（手动池管理 + 终端查询）：

| 端点 | 用途 |
|---|---|
| `GET /api/pool/state` | 手动池 yaml baseline / effective / emergency 事件日志 |
| `POST /api/pool/clear-emergency` | 还原 effective 池到 yaml baseline |
| `GET /api/pool/terminal?ip=X` | 按 terminal IP 查出口节点 + 各节点健康 |

**架构变化**（影响 mihomo 配置形态，REST 表面不变以下端点）：

- 默认池模式从"K-gating auto"切到"manual"——`pool_mode: manual` + 操作员维护 `pool_members:` 列表。详见 `node_qualify.pool_mode` 配置。
- `mihomo` 的 perterm sub-rule 现在指向 `fb-<ip>` fallback group，每组含 HRW 选出的 [primary, secondary]。节点死了 mihomo 自动切到 secondary（30s 内），不再依赖 leap-gateway 30min 等待。
- 已删除 `perterm-<poolname>` 子规则——同一 terminal 的所有目的地走同一 egress（CF 反检测要求）。pool_members 选节点时需自行确认 OpenAI 兼容性。

### 2026-06-06（commit `21a88d5`）

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
