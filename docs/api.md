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
| GET | `/api/pool/terminal?ip=X` | 查询某 terminal IP 的出口节点 + 健康 | ✓ |
| GET | `/api/pool/transitions?limit=N` | 池组成变更审计日志 + 当前隔离名单 | ✓ |
| POST | `/api/pool/rollback` | 回滚最近 N 次池变更 + 隔离被回滚节点 | ✓ |
| GET | `/api/notifications/status` | 通知系统配置快照（不含 secret） | ✓ |
| POST | `/api/notifications/lark` | 设置/更新 Lark webhook url + secret | ✓ |
| DELETE | `/api/notifications/lark` | 移除 Lark webhook（停止外发，本地日志保留） | ✓ |
| POST | `/api/notifications/test` | 同步发一条测试消息验证 webhook | ✓ |
| GET | `/api/notifications/recent?limit=N` | 本地 JSONL 日志最近 N 条 | ✓ |

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
  "pool_size":       19,
  "pool_eligible":   19,
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
| `pool_size` | 实际承载流量的 us-pool 成员数（InPool，即 per-terminal HRW 抽取的集合大小）。 |
| `pool_eligible` | 通过准入门的节点数（tier1）。eligibility 模式下稳态等于 `pool_size`；legacy K-gating 下 `pool_size` 被 K 截断、`pool_eligible` 可更大。（旧字段 `pool_qualified` 名为"合格数"实为 InPool，已拆成这两个诚实字段。） |
| `pool_total` | NodePattern-匹配的候选总数。 |
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

数据面活跃状态快照。15 秒内判断是否需要介入的主要运维视图。

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
    "active_group": "us-pool",
    "pool_filter": "美国|🇺🇸|\\bUS",
    "pool_mode": "auto",
    "last_swap_at": "2026-06-06T11:58:00Z",
    "pools": [
      {
        "tag": "us-pool", "pool_size": 8, "active": true,
        "egress": {"ip": "12.34.56.78", "country": "US", "asn": "AS???"},
        "nodes": [
          {"tag": "ctc-02/US-C30-01", "delay_ms": 155, "last_check": "...", "fail_rate": 0.0}
        ]
      }
    ]
  }
}
```

| 字段 | 说明 |
|---|---|
| `active_group` | `out` selector 当前指向的 group（`us-pool` / `pin` / `DIRECT`） |
| `pool_mode` | `node_qualify.pool_mode`：`auto`（scorer 自动换池）/ `manual`（手动维护）|
| `last_swap_at` | 池成员最近一次变更时间。频繁变化说明 scorer 在抖动，配合 `/api/pool/transitions` 查原因 |
| `pool_size` | routing pool 成员数（K-gating top-K）。K-gating 未启用时等于所有合格候选数 |
| `nodes[]` | 当前 routing pool 成员（K-gating top-K 子集）。scorer 不活跃时降级为全量 us-pool 成员 |
| `nodes[].fail_rate` | 来自 NodeScorer 的探针失败率（0.0–1.0）。scorer 不可用时为 -1 |
| `nodes[].delay_ms` | mihomo url-test 最近一次延迟（ms）。0 表示尚无测量 |

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

### user_agent 常用值

不同机场按 UA 判断客户端版本，UA 不对会返回警告占位节点而非真实节点。省略时用全局默认（`ClashforWindows/0.20.39`），部分面板会将其判定为版本过旧。

| user_agent | 适用场景 |
|---|---|
| `mihomo/1.18.4` | 狗狗加速等按 mihomo 版本放行的面板；通常返回节点数最多 |
| `clash.meta` | 多数 Clash 系面板的通用兼容值 |
| `ClashforWindows/0.20.39` | 全局默认；老面板兼容性好，新面板可能拦截 |
| `sing-box/1.10.7` | 仅限明确要求 sing-box 格式的机场；clash 系面板通常返回 0 节点 |
| `clash-verge/1.7.7` | Clash Verge 系面板 |

排查节点数不及预期时，用不同 UA 各拉一次对比返回的 `name:` 行数，取节点数最多的值。

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

`fake_ip_skip` 写法：`paigod.work` / `.paigod.work` / `+.paigod.work` 均可，统一规范化为 `+.` 前缀。列出的内网域名后缀会走国内 DoH 解析。

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
  "pool_mode": "auto",
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

## GET /api/pool/transitions

返回池组成变更审计日志（K-gating 自动 swap、emergency evict/promote、operator rollback）+ 当前因 rollback 被隔离的节点列表。

```bash
curl -H "Authorization: Bearer ..." \
  "http://leap-89:18080/api/pool/transitions?limit=20"
```

```json
{
  "transitions": [
    {
      "at": "2026-06-09T13:42:11Z",
      "type": "auto_swap",
      "added": ["fishcloud/🇺🇸 美国02"],
      "removed": ["ctc-02/US-C30-04-SNAP"],
      "pool_before": ["ash/...01", "ctc-02/04-SNAP", "..."],
      "pool_after": ["ash/...01", "fishcloud/02", "..."],
      "reason": "K-gating composite-score swap",
      "source": "scorer"
    },
    {
      "at": "2026-06-09T13:55:30Z",
      "type": "rollback",
      "added": ["ctc-02/US-C30-04-SNAP"],
      "removed": ["fishcloud/🇺🇸 美国02"],
      "reason": "operator rollback (1 step(s))",
      "source": "operator_api"
    }
  ],
  "quarantine": {
    "fishcloud/🇺🇸 美国02": "2026-06-09T14:55:30Z"
  }
}
```

字段：
- `transitions[].type`：`auto_swap`（K-gating 决策）/ `emergency_evict` / `emergency_swap`（emergency_promote_chain 顶替）/ `rollback`（操作员回滚）
- `transitions[].source`：`scorer`（系统）/ `operator_api`（API 调用）/ `operator_yaml`（yaml 编辑后 reconcile）
- `transitions[].added` / `removed`：相对前一状态的差集，按字典序
- `pool_before` / `pool_after`：完整快照
- `quarantine`：节点 → 隔离截止时间。被 rollback 移出的节点在隔离期内 K-gating 不会重新选中

`limit` 默认 50，最大 500。日志保留最近 200 条（capped），超出会被滚动覆盖。

503：scorer 未启用。

---

## POST /api/pool/rollback

回滚最近 N 次池变更，并把"刚被加入"的节点放进 rollback 隔离区。下一次 K-gating round 不会重新选中这些节点（直到隔离期满）——给操作员留排查 + 改 yaml + 决定是否延长隔离的时间窗口。

```bash
curl -X POST -H "Authorization: Bearer ..." -H "Content-Type: application/json" \
  -d '{"steps": 1, "quarantine_seconds": 3600}' \
  http://leap-89:18080/api/pool/rollback
```

请求体（全部可选）：
- `steps`：回滚步数。默认 1，最大 = 当前 transitions 长度
- `quarantine_seconds`：被回滚节点的隔离时长。默认 3600 (1h)，最大 7×24×3600 (7d)

成功响应：
```json
{
  "steps_requested": 1,
  "steps_applied": 1,
  "reverted_transition_types": ["auto_swap"],
  "new_pool": ["ash/...01", "ash/...02", "ash/...03", "ctc-02/04-SNAP", ...],
  "quarantined": ["fishcloud/🇺🇸 美国02"],
  "quarantine_until": "2026-06-09T14:55:30Z"
}
```

失败：
- 400：`pool_mode=manual`（manual 模式应使用 `/api/pool/clear-emergency`）
- 400：JSON 格式错误

注意：
- 回滚自身也会被记入 transitions（type=`rollback`），但不可被进一步 rollback——避免回滚链条混乱
- 回滚后立刻触发 mihomo hot-reload，无需等下一个 5min scoring round
- 隔离期到期后，节点再次按正常 K-gating 评分；如果它仍是 top-K，会被重新选中。**隔离不是永久排除**——永久排除请改 yaml + redeploy
- 多次连续 rollback：每次回滚都是基于"当前 transitions 中最末一个非 rollback 项"，所以连续调用确实能往前再走一步

---

## 错误码

返回通知系统当前配置 + 健康状态。永远不返回 secret 本身（哪怕已配置），只返回 `secret_configured: true/false`。

```json
{
  "enabled": true,
  "webhook_configured": true,
  "signature_required": true,
  "secret_configured": true,
  "local_log_path": "/var/lib/leap/notifications.log",
  "dedup_window_sec": 300,
  "aggregation_window_sec": 30,
  "per_node_rate_limit_per_hour": 4,
  "retry_attempts": 3
}
```

字段：
- `enabled` — `notifications.enabled` 开关
- `webhook_configured` / `signature_required` / `secret_configured` — Lark 子系统的可用性
- `dedup_window_sec` — 同 (node, event_type) 在窗口内合并为一条
- `aggregation_window_sec` — 多个 info 事件在窗口内打包成一条 Lark 消息
- `per_node_rate_limit_per_hour` — 单节点每小时最多 N 条 info 通知（urgent 不限）
- `retry_attempts` — webhook 失败时指数退避重试次数

---

## POST /api/notifications/lark

设置/更新 Lark webhook 配置。`webhook_url` 写入 `gateway.yaml` 的 `notifications.lark.webhook_url`；`secret` 写入独立文件（默认 `/var/lib/leap/lark-secret`，0600 权限），yaml 不存 secret。

```bash
curl -X POST -H "Authorization: Bearer ..." -H "Content-Type: application/json" \
  -d '{
    "webhook_url": "https://open.feishu.cn/open-apis/bot/v2/hook/xxx",
    "secret": "your-signing-secret",
    "signature_required": true
  }' \
  http://leap-89:18080/api/notifications/lark
```

字段：
- `webhook_url`（必填）— Lark 自定义机器人 webhook URL
- `secret` — Lark 机器人的签名密钥；当 `signature_required=true` 时必填
- `signature_required`（可选，默认 `true`）— 启用 Lark HMAC-SHA256 签名校验。**生产强烈建议保持 true**——webhook URL 一旦泄露，没签名就能被任何人伪造消息

返回：post-config 的 `Status` body，同 `/api/notifications/status`。

副作用：
- yaml 持久化（webhook_url + signature_required，不含 secret）
- secret 文件原子写入（tmp + rename）
- Notifier 在线重建 Lark client，下一条事件就用新配置发

错误：
- 400 `webhook_url required` — 空 URL
- 400 `secret required when signature_required=true` — 缺 secret
- 500 — 持久化失败

---

## DELETE /api/notifications/lark

移除 Lark webhook 配置。secret 文件删除，yaml 中 `webhook_url` 清空。事件继续写到本地 JSONL 日志，**不再外发**——`notifications.enabled` 状态保持原样。

```bash
curl -X DELETE -H "Authorization: Bearer ..." \
  http://leap-89:18080/api/notifications/lark
```

返回：post-deletion 的 `Status` body，可见 `webhook_configured: false`。

---

## POST /api/notifications/test

同步发一条测试消息。绕过 dedup / rate-limit / aggregation——一定尝试发送，便于操作员验证 webhook + secret 配置正确性。

```bash
curl -X POST -H "Authorization: Bearer ..." \
  http://leap-89:18080/api/notifications/test
```

成功：
```json
{"ok": true}
```

失败（502 Bad Gateway）：
```json
{"ok": false, "error": "lark response not OK: {\"code\":19021,\"msg\":\"sign match fail or timestamp is not within one hour from current time\"}"}
```

错误信息会原样从 Lark API 透传过来，便于诊断签名错、URL 错、token 失效等问题。

---

## GET /api/notifications/recent

返回本地 JSONL 日志最近 N 条记录。即使 webhook 挂掉、Lark 不可达，记录也在；ops 通过这个端点可以重建"系统过去发生了什么"。

```bash
curl -H "Authorization: Bearer ..." \
  "http://leap-89:18080/api/notifications/recent?limit=20"
```

```json
{
  "limit": 20,
  "entries": [
    {
      "at": "2026-06-09T13:42:11Z",
      "outcome": "delivered",
      "severity": "urgent",
      "type": "scorer_evict",
      "node": "fishcloud/🇺🇸 美国01",
      "subject": "pool member auto-evicted: fishcloud/🇺🇸 美国01",
      "body": "fail_rate >= 0.90 sustained 30m0s"
    },
    {
      "at": "2026-06-09T13:42:11Z",
      "outcome": "webhook_failed",
      "severity": "info",
      "type": "scorer_promote",
      "node": "fishcloud/🇺🇸 美国02",
      "subject": "promoted from chain: fishcloud/🇺🇸 美国02",
      "extra": {"err": "Post \"https://...\": dial tcp: lookup open.feishu.cn: no such host"}
    }
  ]
}
```

`outcome` 取值：
- `queued` — 进入 dispatcher 队列等待处理
- `delivered` — webhook 成功
- `webhook_failed` — 重试耗尽仍失败（保留 extra.err）
- `dropped_dedup_or_rate_limit` — 被 dedup 或 rate-limit 抑制
- `dropped_queue_full` — dispatcher 队列满（少见）
- `test_attempt` / `test_delivered` / `test_failed` — `/api/notifications/test` 触发

`limit` 默认 50，最大 1000。

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

### 2026-07-04（active 接口运维增强）

**Breaking**：

| 字段 | 变化 |
|---|---|
| `active_proxy.active_urltest` | → `active_proxy.active_group`（语义更准确，原字段删除） |
| `active_proxy.history_len` | **已删除**（无运维价值） |
| `pools[].pool_size` | 语义变更：原为 mihomo `all` 全量大小，现为 routing pool（top-K）实际大小 |
| `pools[].nodes[]` | 语义变更：原返回全量 us-pool 成员，现只返回 routing pool 成员（scorer 不活跃时降级为全量） |

**新增字段**：

| 字段 | 说明 |
|---|---|
| `active_proxy.pool_mode` | `"auto"` / `"manual"`，决定运维操作方向 |
| `active_proxy.last_swap_at` | 池成员最近一次变更时间（RFC3339） |
| `pools[].nodes[].fail_rate` | NodeScorer 探针失败率（0.0–1.0）；scorer 不可用时为 -1 |

### 2026-06-10（清理 manual 模式遗留）— 当前版本

**Breaking — 删除 3 个端点**：

| 端点 | 删除理由 |
|---|---|
| `GET /api/geosites` | 与 `/api/rule-sets` 同数据，纯遗留兼容别名 |
| `GET /api/pool/state` | 只在 `pool_mode: manual` 下有意义；auto 模式下输出是 stale 数据 |
| `POST /api/pool/clear-emergency` | 同上，manual 专用；auto 下应该用 `/api/pool/rollback` |

替代查询（auto 模式）：
- 当前池成员 → `/api/nodes/health` 过滤 `in_pool: true`
- 池规模 K → `/api/status.pool_sizing`
- 池历史变更 → `/api/pool/transitions`

### 2026-06-09 深夜（B+ v2 — EWMA + trial + 稳定度）

**评分系统升级**（影响 auto 模式池决策，REST 表面新增字段）：

- `pool_mode: auto` 的池决策不再基于"当前轮单次 compositeScore"，而是基于双窗 EWMA：
  - **短窗（4h 半衰期）** — 驱动 evict 决策。新出现的降级在 ~2 小时内反映到分数。
  - **长窗（24h 半衰期）** — 驱动 promote 决策。"刚恢复"的节点要等长窗忘掉旧坏期才会被晋升，避免抖动。
- **24h trial 窗口**：新订阅引入的节点在 24h 内不被 EWMA-promote 路径选中（除非已在池中或 emergency 路径触发）。
- **绝对交换阈值** `swap_threshold_score`（默认 100）：候选要晋升必须比池内最差成员的长窗 EWMA 好 ≥ 100 分。降到 0 = 任何改进都换。
- **池稳定度 24h** — `/api/status` 新增 `pool_stability_24h` 字段：

```json
"pool_stability_24h": {
  "window_hours":     24,
  "swap_count":       2,
  "rollback_count":   0,
  "unique_nodes_in":  3,
  "unique_nodes_out": 3,
  "oldest_at":        "2026-06-09T01:13:00Z"
}
```

ops 看一眼这个就知道"系统过去一天动了多少"。频繁高 swap_count 是 thrashing 信号；持续 0 是池稳定。

**配置默认变化**（向后兼容，原有 yaml 不需要改）：

```yaml
node_qualify:
  swap_threshold_score: 100   # 默认 100；写 0 回退到旧"top-K 直接换"
```

**状态文件 v3 schema 兼容扩展**（增加 `ewma` 字段）。v1/v2 文件自动升级，无操作。

### 2026-06-09 夜（池审计 + 回滚）

**新增端点**（B+ issue 6 闭环）：

| 端点 | 用途 |
|---|---|
| `GET /api/pool/transitions` | 池变更审计日志 + 当前隔离名单 |
| `POST /api/pool/rollback` | 回滚最近 N 次变更，隔离被回滚的节点 |

**机制**：
- 每次池组成变化（auto K-gating swap / emergency evict-promote / 操作员 rollback）都记入 transitions（capped 200 条），含 type / added / removed / pool_before / pool_after / reason / source
- `rollback` 反向执行 transitions（重新加回被移除的节点，移除被加入的节点），并把刚被移除的节点放进 quarantine
- quarantine 期间（默认 1h，可配 ≤7d），K-gating 选 top-K 时跳过这些节点——避免回滚被立即覆盖
- 状态 file 升级 v2 → v3（自动兼容旧 v1/v2 文件）

### 2026-06-09 晚（Lark 通知）

**新增端点**（Lark 通知 + 池审计预备）：

| 端点 | 用途 |
|---|---|
| `GET /api/notifications/status` | 通知系统配置 |
| `POST /api/notifications/lark` | 设置 webhook url + secret（HMAC 签名） |
| `DELETE /api/notifications/lark` | 移除 webhook |
| `POST /api/notifications/test` | 同步测试发送 |
| `GET /api/notifications/recent` | 本地 JSONL 日志最近 N 条 |

**通知机制**：
- 严重档：紧急 (urgent，绕过 dedup/限速)、信息 (info)、完成 (done)
- dedup：5min 内同 (node, type) 合并；info 单节点每小时最多 4 条；urgent 不限
- aggregation：30s 内多事件合并为一条 Lark 消息（urgent 不参与）
- 重试：失败时指数退避 1s/2s/4s，3 次后落本地日志
- 安全：默认 HMAC-SHA256 签名，secret 持久化在 0600 权限文件，yaml 不存 secret

**配置关键字段**（`notifications:` 块）：
```yaml
notifications:
  enabled: true
  lark:
    webhook_url: ""             # 通过 API 设置
    signature_required: true    # 默认 true，强烈推荐
    secret_file: /var/lib/leap/lark-secret
  local_log_path: /var/lib/leap/notifications.log
  dedup_window: 5m
  aggregation_window: 30s
  per_node_rate_limit_per_hour: 4
  retry_attempts: 3
```

### 2026-06-09 上午（commit `49ece0e`+）

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
