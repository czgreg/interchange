# 候选集闭环 — 新订阅节点永远不被评分

**状态**：**已修复**（未部署，等 Hutao 冷却期）。收益经实测仅 +1 节点，
与准入质量门同批上线（互为前置条件）。
**发现日**：2026-09-08（92 生产）

---

## 1. 现象

两个**有效**订阅（soonlink、沃迪加速）的 11 个美国节点从未被 scorer 评分过一次。

```
gateway.yaml 订阅: fishcloud, ash, soonlink, 沃迪加速, Hutao
匹配 node_pattern 的美国节点: 24 / 179
mihomo /proxies 里存在: 24  ← 协议解析、渲染全部正常
mihomo us-pool 组成员:   13  ← 缺 soonlink 2 + 沃迪加速 9
/api/nodes/health:       13
nodescorer-state.json:   soonlink 0 条, 沃迪加速 0 条
```

排除的假设：**不是协议不支持**。24 个节点在 `/proxies` 里类型全部正确
（soonlink 2×Shadowsocks、沃迪加速 9×Hysteria2、ash 9×Vless、fishcloud 4×）。
解析、渲染、mihomo 加载三层都正常。

---

## 2. 根因：候选集自举闭环

`score()`（`scorer.go:450-462`）构造候选集只有两个来源：

1. `proxies["us-pool"]["all"]` —— 当前 us-pool 组成员
2. `s.state` 里已存在且仍在 `/proxies` 的 tag

**它从不扫描 `/proxies` 全集匹配 `node_pattern`。**

而 us-pool 的成员由渲染器根据 scorer 自己上一轮给的 `qualifiedOverride` 写入
（`groups.go:236-249`），`usPoolAll`（`scorer.go:1119`）传入的是已过滤的
`candidates`。于是：

```
候选集 ← us-pool ← qualifiedOverride ← 候选集
```

闭环内**没有任何入口**让 scorer 未见过的节点进来：要进候选集必须已在
us-pool，要进 us-pool 必须已被评分。

### 2.1 为什么重启也修不了（关键，曾被误判）

`usPoolMembers`（`groups.go:250-255`）确实有 pattern 回退：

```go
if len(pool) == 0 {
    pool = filterByPattern(tags, r.cfg.URLTest.NodePattern)
```

`qualifiedOverride` 仅在内存、不持久化，所以进程启动时为空 —— 看似每次重启
都会重新 pattern 发现。**但实际不会**：`main.go` 在 `config.yaml` 已存在时
跳过 bootstrap 写入（保护 last-known-good，本身正确），mihomo 用**上次渲染的
config.yaml** 启动，`us-pool` 里已有成员。scorer 首轮从 mihomo 读到这些成员，
`qualifiedOverride ∩ present` 非空，pattern 回退**永不触发**。

生产证据（同一 PID 全程，即无重启）：

```
PID 1862814 贯穿 07:50–08:10
08:03:24  沃迪加速首次出现  subscriptions=4 nodes=139   ← API 加的订阅
08:06:08  scored total=9   ← 没进来
08:11…08:41 连续 8 轮 total=9  ← 3 小时全部没进来
11:01:09  重启 (PID 761368)
11:01:19  scored total=18  ← 仍然没有 soonlink/沃迪
```

订阅在 **08:03 经 API 添加**，比 11:01 的重启早 3 小时；**11:01 的重启同样
没能让它们进来**。因此「清空 `qualifiedOverride` 即可」的 3 行方案是无效的
——mihomo 的 us-pool 仍有成员，scorer 下一轮照样从那里自举。

---

## 3. b1ff1df 的误诊（必须纠正的记录）

闭环源于 `b1ff1df`（2026-06-05）刻意移除了 regexp 发现。其 commit message
声称的 bug #1：

> filterCandidates used NodePattern regexp (美国|🇺🇸|\bUS) to filter
> AllOutbounds. The \bUS part produced no matches in Go's RE2 engine for
> ctc-02/US-C30-* names: \b doesn't match between non-ASCII-adjacent chars

**实测该结论为假。** 用生产原样 pattern 编译运行 Go regexp：

```
ctc-02/US-C30-01     → true      ← 它指名失败的那一族
ctc-02/🇺🇸US-C30-01   → true
🇺🇸US-01             → true
美国US                → true
沃迪加速/美国HB.2x      → true
全部 24 个生产美国节点   → true
ash/🇭🇰HK-01 等非美国   → false   ← 正确排除
aUS                  → false     ← 正确（ASCII 词边界语义）
```

Go RE2 的 `\b` 是 ASCII 词边界，`🇺🇸`/`美国`/`·`/`-` 均为非词字符，
故 `\bUS` 正常匹配。

它真正修对的是 **bug #2**（us-pool 收缩到 qualified 子集后，读
`us-pool.all` 作为候选源导致候选集饿死），而 state-union 那半个修复解决了它。
**移除 regexp 发现是基于错误理论的附带损害，正是本闭环的来源。**

`scorer.go:441-444` 那段注释仍在复述这个错误理论，**在主动误导后来的维护者**，
修复时必须一并改掉。

附带：`filterCandidates`（`scorer.go:1734`）**零调用者**，是 `b1ff1df` 之后
残留的死代码。

---

## 4. 实测收益：+1 节点，不是 11

对 11 个"闲置"节点逐一 delay 探测：

```
soonlink/🇺🇸 美国        delay 653   ← 唯一可用
soonlink/🇺🇸 美国 B      error
沃迪加速/美国HB.2x        error
沃迪加速/美国HA.6x | 专线   error
沃迪加速/美国HJ.2x        error      （9 个全 error）
沃迪加速/香港家宽H.2x      error      ← 对照组，也挂
```

**整个沃迪加速订阅（45 节点）不可用**，非仅美国节点，与其每次刷新都触发
UA 回退（`primary_outcome=zero_nodes`，仅 `sing-box/1.10.7` 返回内容）一致。
渲染出的 hysteria2 配置结构正常，故为**机场侧账号/配额/凭据问题**。

所以"9 个 Hysteria2 含 4 个专线优质线路"是伪需求 —— 死端点上的营销字符串。
修复本 bug 的真实收益 = `soonlink/🇺🇸 美国` 一个节点。

soonlink 本身抓取完全正常（18 节点、5 个订阅里唯一不需要 UA 覆盖的），
但 18 个里仅 2 个匹配美国 pattern，其余 16 个是港澳台日韩新马泰印英法土澳波俄。
它是"全球混合"型订阅，对美国出口容量的贡献天然有限。

---

## 5. 修复方案与两个 BLOCKER（评审结论）

正确形状：给候选集**增加**第三个来源（union，绝不能是 replace）——
`filterCandidates(s.subscribe(), s.nodePattern, proxies)`。union 语义保证
`b1ff1df` 的 bug #2 不回归，且超集永不产生导致 bug #1 的空集。

选 `AllOutbounds` 而非扫 `/proxies` key 的三个理由：
`/proxies` 含组和内建（`us-pool`/`out`/`pin`/`probe-out`/`fb-<ip>`/`DIRECT`…）；
与渲染器的 `nodeTags(outbounds)` 保持同一事实源；现有测试 `subscribe` 返回 nil、
pattern 为空，故 CI 中为 no-op 不会静默改写既有预期。

### BLOCKER 1 — 发现了但不触发重渲染，节点永远拿不到测量

`poolChanged`（`scorer.go:969`）只比较**路由集**，而 hot-reload 是唯一把
`usPoolAll` 写进 mihomo us-pool 的地方。路由集不变的轮次 → 不渲染 →
us-pool 仍是 13 → mihomo 不探测那 11 个 → `ProbeCount` 永远 0 →
准入门永久拒绝。**把"从未被评分"换成"被评分、永久不合格、每轮打一条拒绝
日志"，看起来健康实际更糟。**

修法：记住上一次**意图**渲染的 us-pool 集合，纳入重载触发条件：

```go
// 字段: lastUsPoolRendered map[string]bool（s.mu 保护，不持久化）
usPoolChanged := !poolSetsEqual(s.lastUsPoolRendered, candidateSet)
if (poolChanged || usPoolChanged) && !poolChangePending { … }
```

不要改用「mihomo 现状 vs 期望」做触发器：`usPoolMembers` 会与
`nodeTags(AllOutbounds)` 求交，一个在 `/proxies` 但不在 `AllOutbounds` 的
候选将永远无法出现在渲染组里，导致每轮都热重载。与自己的上次意图比较才是
幂等的。`HotReloadMinInterval` 仍然节流。

### BLOCKER 2 — 池低于 floor 时未测量节点以不确定顺序进入路由集

cold-start last resort（`scorer.go:1288`）只要求
`Qualified && !eligible && !inQuarantine`，11 个未测量节点全部通过；
按 `nodeLongEWMA` 排序而它们全为 `+Inf`，`sort.Slice` **非稳定**，
选中哪些依赖 pdqsort 实现细节。随后 incumbency（`scorer.go:~1350`
把在位优先于 EWMA）把这个任意选择锁死：round N 随机选中者，
round N+1 即使测出 p95=5000 也作为"在位填充者"被保留，而兄弟节点测出 200。

修法：cold-start 层显式按 `(!incumbent, name)` 排序确定化；
measured floor-fill 层允许显著更优的 EWMA 击败 incumbency
（复用既有 `SwapThresholdScore`，不新增常量）。

### 其他评审要点

- **us-pool 不只是探测集**：`out (select)` 首项是 us-pool，
  `perTerminalSlices` 以 `MATCH,us-pool` 收尾，故扩大它会让
  **fallback 流量**立即哈希到全部 24 个成员（在网段内终端走
  `fb-<ip>` 不受影响）。暴露窗约一轮。`scorer.go:1113` 的注释需修正。
  若要更稳可分批发现（每轮 N 个）。
- 拒绝日志应在**原因变化时**打印，而非每轮每节点（否则 ~3k 行/天）。
- `usPool == nil` 时 `score()` 直接 return（`scorer.go:445-449`）应改为
  warn 后继续 —— 有了 pattern 发现，来源 1 变为可选，可消除相邻的自锁死状态。
- 影响 `pool_mode: manual`（89）：被静默记为 "missing from candidates" 的
  `pool_members` 条目会在修复后首轮开始承载流量。
- `tier1_count` 会跳增 11（含 benefit-of-doubt 节点），监控基线需重设。

### 必须补的测试

现有 eligibility 测试**无法**捕获这两个 BLOCKER：`mockClashAPI` 把每个节点
都放进 `us-pool.all`，"在 /proxies 但不在 us-pool" 这一状态未被表达。
需要新增 fixture：节点在 `/proxies` + `AllOutbounds` + 匹配 pattern 但
**不在** `us-pool.all`，断言：①出现在 `snap.Nodes`；②即使 `newPoolSet` 未变
也触发渲染且 `lastUsPool` 含它；③`ProbeCount==0` 且池在 floor 之上时
**不在** `routingMembers`；④低于 floor 时 cold-start 选择在两轮相同输入下确定。

---

## 6. 实现记录（原「为什么暂不修」）

三处协同改动已全部落地，5 个新测试（4 个来自评审要求 + 1 个补充）：

- **发现**：`score()` 第三个候选来源 `filterCandidates(s.subscribe(), …)`，
  union 语义。同时 `us-pool` 缺失不再 fatal（改为 warn 后继续）。
- **BLOCKER 1**：新增 `lastUsPoolRendered` 字段，`reloadNeeded = poolChanged
  || usPoolChanged`，与自己上次**意图**比较（非与 mihomo 现状比较）保证幂等。
- **BLOCKER 2**：cold-start 排序改为 `(incumbent, ewma, name)` 全序。
- 拒绝日志改为**原因变化时**才打（新增 `nodeState.lastRefusal`）。
- 修正 `filterCandidates` 的过期文档注释（它并不过滤 node-bearing）。
- 修正 `usPoolAll` 注释，明确 us-pool **同时承载 fallback 流量**
  （`out` 首项 + `MATCH,us-pool`），扩大它会让未测量节点接触 fallback 流量约一轮。

**变异测试验证**（逐个回退修复看测试是否失败）：

| 变异 | 结果 |
|---|---|
| 去掉 discovery 源 | 3 个测试失败 ✅ |
| `reloadNeeded` 只看 `poolChanged` | BLOCKER 1 测试失败 ✅ |
| cold-start 退回裸 EWMA 排序 | **测试仍通过** ⚠️ |

第三项的诚实结论：实测 Go pdqsort 对"比较器恒为 false"的全等切片**不改动
顺序**（n=8/13/20/40 均验证），故 `cold` 的顺序就是构建它的顺序 ——
即 name-sorted `candidates`，本来就是确定的。所以 BLOCKER 2 的"不确定排序"
表述**不准确**，显式 tiebreak 是防御未来改动，不是修一个当下可复现的 bug。

BLOCKER 2 真正的风险是评审提的**第二点**：任意但稳定的 cold-start 选择被
incumbency 钉死。已补 `TestDiscovery_ColdStartPickReleasedOnceMeasured`
验证：一个 cold-start 选中后测出 p95=5000 的节点，在兄弟节点测出 150/160
后**会被释放**出路由集，floor 仍满足。这条此前只是推断，现在有测试。
2. **验证会假阳性**：池低于 floor 时 floor 路径每轮都动，`poolChanged` 恰好为真，
   生产观察会显示"修好了"，而稳态缺陷（BLOCKER 1）被掩盖。必须等池稳定在
   floor 之上才能真正验证。
3. **需与准入质量门一起上线**：质量门是让本修复可安全上线的前提（否则 11 个
   未测量节点凭 benefit-of-doubt 直接进路由集）；反之若 BLOCKER 1 未修，
   质量门会退化为永久拒绝。二者互为条件。

---

## 7. 顺带发现（各自独立立项）

1. **`swap_threshold_score: 100` 是 92 上的死配置**（eligibility 下不读取），
   而代码默认为 400。读 yaml 者会得出错误结论。
2. **`refresh_on_startup` + 订阅无缓存**：节点列表仅在内存
   （`parser.go:27`），重启必须重新拉取全部订阅；部分失败时会用残缺结果
   **覆盖**已渲染的 config（`api.go:333-345` 仅告警不阻止）；全部失败会渲染
   DIRECT-only。**这是能把生产打空的单点。** 且 `fetcher` 丢弃 `retry-after`，
   每次重启都会重撞机场限流（Hutao 403 即此）。
3. **`scripts/deploy.sh:133`** 在 macOS bash 3.2 下 `AUTH=()` 空数组展开触发
   unbound variable，导致部署实际成功但验证步骤误报失败。
4. **state 从不清理已删除订阅的节点**：49 条中 16 条属于早已移除的
   狗狗加速/ctc-02。当前无害（不在 `/proxies` 故被过滤），但 tag 若被复用，
   僵尸的 EWMA 与 strike 计数会挂到不同物理节点上。
   修复前置条件：`persistedState` 需新增 `last_seen`（现无时间戳），
   门槛取「连续缺席 ≥7 天」而非小时级 —— 否则 Hutao 这类 23.5h 中断期间
   会误删并丢失 `FirstSeenAt` 与双窗 EWMA。**必须与 state+ewma 一起删**，
   避免留下孤儿 EWMA。

---

## 8. 更新记录

- 2026-09-08：初版。两份独立交叉评审（over-engineering / correctness）结论相反，
  以生产日志 PID 与时间线证据判定 correctness 正确：闭环无出口、重启不可破。
  实测 11 节点仅 1 可用，修正了原判断的收益预估。
