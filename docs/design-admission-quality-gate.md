# 准入质量门 — 用绝对阈值替换 24h trial 窗口

**状态**：已交叉评审，待实现。**不在 Hutao 冷却期内部署**（见 §7）。
**范围**：仅 `sizing_mode: eligibility` 的准入路径。legacy K-gated 路径不动。

---

## 1. 触发问题

运维提出："新增订阅节点入池要 24 小时，是否过长？"

实测路径（92 生产，`sizing_mode: eligibility`）：

| 阶段 | 耗时 | 位置 |
|---|---|---|
| 订阅刷新发现节点 | 手动触发（`refresh_interval: 0s`） | — |
| 进 `us-pool`（探测集，不载流量） | 立即 | `scorer.go:1119` |
| 攒够 url-test 历史 | 30s | `health_check_interval` |
| scorer 评一轮 | ≤5m | `scoring_interval` |
| **trial 窗口** | **24h** | `ewma.go:56` |
| 热重载节流 | ≤90s | `hot_reload_min_interval` |

前四道约 5–7 分钟，24 小时全在 trial 上。唯一旁路是池跌破
`min_pool_size` 时的 floor / cold-start bypass（`scorer.go:1299`），
92 的 floor=4、池常态 ≥6，该旁路不开。

**前提修正**：trial 不是"从节点出现算 24h"。`FirstSeenAt` 在
`updateNodeEWMA` 里设置，而 `score()` 只对 `ProbeCount>0` 的节点调用它
（`scorer.go:530-533`）。所以时钟**从首次被测量那一轮开始**，不是从订阅
刷新开始。方向一致，但不要在别处复述成"节点出现后 24h"。

---

## 2. 根因：不是"太慢"，是"准入没有质量门"

`trialDuration` 的原始理由（`ewma.go:20-27`）是防止全新订阅的未知节点
"靠首轮探针的运气把 top-K 扫掉"。**这个担忧的前提是存在排名竞争。**

eligibility 模式里没有排名、没有 top-K、没有 fast-evict、没有 swap 门
（`scorer.go:1131-1137` 自述）。新节点进池**不排挤任何人**，只是让 HRW
目标集多一个元素。所以 trial 防的失效模式在生产模式下不存在——它是从
legacy 路径继承的遗留耦合。

但 trial 现在**兼任了另一个它没被设计承担的职责**。eligibility 的准入门
`Qualified` 是存活门不是质量门：

- `recentOk > 0`：最近 5 次探针有一次成功即可（`scorer.go:1481-1489`）
- catastrophic：仅 `fail_rate >= 0.9` 才判不合格（`scorer.go:1497`）
- **RTT / jitter 完全不设限**

而 eligibility 又没有 fast-evict。合起来：

> 一个 fail=0.8、p95=4000ms 的节点通过准入，且此后无任何机制能移除它。

所以 24h trial 是目前唯一挡在"新出现的坏节点"和 1/N 用户之间的东西
（HRW 黏性让被分到的用户整段会话待在那）。

**正确的问题框架不是"让准入更快"，而是"让准入正确，正确之后快是免费
的"。** 这是本方案的全部依据。

---

## 3. 方案

`computeEligibleSetLocked`（`scorer.go:1218`）删掉 `inTrial` 调用，
新纳入改为绝对阈值门：

```
incumbent（s.poolSet[name]） → 照旧准入，不设质量门
新纳入                        → ProbeCount >= MinProbes
                              && RTTP95Ms <= MaxRTTP95Ms
                              && JitterMs <= MaxJitterMs
                              && FailRate <= MaxFailRate
```

`!inQuarantine` 和 `!deadEvicted` 两个既有过滤**不动**。
floor / cold-start 旁路**完全不动**——低于 `min_pool_size` 时可用性必须
压过质量，这部分已经正确且 load-bearing。

### 3.1 为什么用这三个已有阈值

`MaxRTTP95Ms=800`、`MaxJitterMs=300`、`MaxFailRate=0.25` 在 92 上配置着、
在 `/api/status` 里暴露着、启动时打日志打得像在生效（`scorer.go:378-385`）
——**但代码里除了日志行没有任何地方读它们**。

它们在 Plan-A 被禁用，理由是 30s 探针粒度下用作**持续成员判定**会每轮
翻转（`scorer.go:1453-1460` 记录了这个 churn）。**那个反对理由不适用于
一次性准入。** churn 来自反复评判在池成员；准入是单向门，判一次。

用它们：零新配置、零新常量、零新概念，三个死配置变成 load-bearing，
语义诚实——"我们不会把用户路由到比这更差的出口"。

### 3.2 为什么老成员必须豁免

给在池成员加质量门 = 重新引入 Plan-A 的每轮翻转 churn。
**准入是单向门，这个不对称就是全部设计。** 出口问题见 §5。

### 3.3 与 design-eligibility-set-selection.md 的调和

那份文档 §3.1:105 把 `poolFillCeiling` 列在 eligibility **删除**清单里，
§3.2:125-126 写"**不做相对质量门**"，并指名相对中位数带是 v1 的死因
（对相关性退化盲视）、绝对 composite 门是 v2 的死因（误杀健康节点）。

本方案**遵守**该结论，不违反：

- 不引入相对门。评审期曾提议复用 `poolFillCeiling`（池 composite 中位数
  ×2.0）做准入，**已否决**，理由记录在 §6。
- 不引入 composite 绝对门。v2 死于用 composite 单值误杀健康节点；这里用
  的是三个**分量各自**的既有阈值，且**只在准入时判一次**，不参与持续成员
  判定，不排序。v1/v2 死因都在"持续判定 + 相对/合成单值"，本方案两者都
  不沾。
- §3.2 那句"不新建 RTT 门"针对的是**资格门（持续）**。本方案不改
  `scoreNode` 的 `Qualified`，只加一道**准入门（一次性）**。二者是不同
  的决策点，文档原文的 `E = Qualified ∩ !inQuarantine ∩ (!inTrial || 已在池)`
  里那个 `(!inTrial || 已在池)` 正是被替换的项——替换的是**同一个位置的
  同一个职责**，不是新增一层。

实现时同步在 `design-eligibility-set-selection.md` §3.1/§3.2 加一段指向
本文档的说明，否则下一个维护者会把这段当死代码删掉。

---

## 4. 预期效果（按 92 实测数据）

采样时点池内 6 成员：composite 202/212/213/217/279/280，
p95 198–261、jitter 2–10、fail 0 —— **全部轻松通过新门**。

| 节点类型 | 现状 | 新方案 |
|---|---|---|
| 好新节点 p95≈250 jitter≈10 fail=0 | 24h 后入池 | **5–7 分钟入池** |
| 中等坏 fail=0.5 p95=3000 | **24h 后照样入池** | **永久拒绝** |
| fail=1.0（实测 7 个） | catastrophic 拒 | 双重拒绝 |
| p95=1242 的 Hutao BGP_D 式 | 24h 后入池 | 拒绝（超 800） |

新门**同时更快也更严**：好节点快 200 倍，坏节点从"等一天照样进"变成
"永不进"。

---

## 5. 本方案不解决什么（必须明说）

**出口问题依然未解决。** 这道门只拦新的坏节点。已入池后退化的老成员仍然
只能靠 `dead_evict_rounds`（需 `alive=false`）或 catastrophic
（需 `fail>=0.9`）移除。

**ops 文档 U1 里被钉在慢节点上的用户正是老成员**，本方案不改善他们的处境。
不要把本方案当作 U1 闭环。U1 需要 fast-evict 或质量加权 HRW，那会重新引入
churn，是独立议题，需要独立评审。

**相关退化仍靠既有三件套**（liveness 门 + 保命网 + 告警），本方案不触碰。

---

## 6. 已否决的替代方案

| 方案 | 否决理由 |
|---|---|
| 只把 24h 调成 2h | 没解决任何问题：好节点仍等 2h，坏节点 2h 后照样进。把已知的洞开得更快。 |
| 复用 `poolFillCeiling`（相对 median×2） | **方向反了**：池健康时最严（最不需要），池腐化时最松（最需要）。池全 3000ms → ceiling 6000 → 门形同虚设。**且有棘轮**：ceiling 由池中位数导出，准入只纳入优于中位数的节点 → 中位数单向下降 → ceiling 单向收紧 → 一个 composite 300 的好节点最终被永久拒绝，而 279/280 的老成员照样带流量，准入结果依赖到达顺序。另违反 §3.3 的文档结论。 |
| 给 eligibility 加 fast-evict | 重新引入 eligibility 存在的全部意义所要消除的 churn。准入门在入口解决同一问题，不动稳态，风险小一个量级。 |
| `trial_duration` 提成配置项 | 配置膨胀。eligibility 不再读它，legacy 不是生产模式，无人要求调。为自己正在离开的路径加旋钮 = speculative generality。const 保留原样。 |
| per-destination 质量信号 | ops 文档 loop #5 已判定"per-dest bad legs 不转化成用户失败"，优先级已降。 |
| 给新订阅开快速通道 | 伪需求。系统不需要知道"这是新订阅"，只需知道"这节点够不够好"。 |
| 每次准入决策打 info 日志 | 18 节点 × 5 分钟 = 噪音。只记**拒绝**。 |

---

## 7. 部署约束

**不要在 Hutao 冷却期内部署。** 2026-09-07 该订阅返回 HTTP 403 +
`retry-after: 84491`（23.5h，至 ~2026-09-08 10:35Z），35 节点从数据面消失，
池 13→6。重启会再撞 403。

顺序：代码落地 → 冷却期过 → 确认池已恢复 → 再部署。

---

## 8. 顺带发现（独立修，不进本次改动）

1. **`swap_threshold_score: 100` 是 92 上的死配置**（eligibility 下不读），
   而代码默认 400。读 yaml 的人会得出错误结论。
2. **`refresh_on_startup` + 无订阅缓存**：节点列表只在内存
   （`parser.go:27`），重启必须重新拉订阅；拉取部分失败会用残缺结果覆盖
   已渲染的 mihomo config（`api.go:334-340` 仅告警不阻止）。全订阅同时失败
   会渲染出 DIRECT-only。这是能把生产打空的单点。
3. **`fetcher` 丢弃 `retry-after`**：每次重启都会重撞机场限流。
4. **`scripts/deploy.sh:133` 在 macOS bash 3.2 下 `AUTH=()` 空数组展开报
   unbound variable**，导致部署成功但验证步骤误报失败。

---

## 9. 更新记录

- 2026-09-07：初版。基于两份独立交叉评审（correctness: SHIP WITH CHANGES；
  first-principles: ADOPT SIMPLIFIED）收敛。原提案的相对 ceiling 方案被
  两份评审独立否决，改用既有绝对阈值。
