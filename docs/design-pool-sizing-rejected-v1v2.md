# 设计：收敛式池大小（Convergent Pool Sizing）— design Z

状态：**草案 v3（design Z），六路评审后定稿。生产未改动一行。**
日期：2026-08-31
作者：运维 / nodescorer 重设计
目标读者：nodescorer 维护者、92 运维

> **版本史（诚实记录，防止再犯）**
> - **v1**：相对中位数质量带 + 新多目的地信号。**否决** —— 相对带对整池
>   相关退化结构性失明；死区在实时数据上不空（BGP_C 2.51x），分布是
>   连续斜坡而非双峰；median-of-medians 把唯一真故障（accounts.google
>   全队 5–10s）盖住。
> - **v2**：绝对慢轴 Schmitt 质量门。**否决** —— 门读的是 compositeScore
>   长 EWMA（不是 p95），在这个信号上 US-01（在池、吞吐全队最高、此刻
>   健康）long-EWMA=3215，与"唯一真坏"的 US-03（3355）只差 4%，无法
>   分离；任何滤掉 US-03 的绝对阈值会同时误杀 US-01。§4 的干净空档只
>   存在于**瞬时** p95 单帧（±7500 轮间噪声）。
> - **v3（本版，Z）**：不再试图用现有信号做"质量分级自动剔除"——两轮
>   实测证明现有 gstatic 信号做不到。改为**消灭抖动**（删强制回填 + 修
>   fast-evict + 边界迟滞），让池稳定收敛到系统一贯认定的稳定档；
>   "自动识别慢节点"降级为一个**依赖新信号**的独立后续项目。

---

## 0. 一句话（诚实版）

**能现在做到、且解决你诉求的**：删掉"无条件回填到 K"、把 K 降为上限、
修 fast-evict 的无迟滞乒乓、给池内↔池外边界加一道 swap 迟滞。池子从此
稳定收敛、停止每天 ~50 次换槽，不发版、不手挑成员。

**现有信号做不到、必须诚实说清的**：用 gstatic 信号**可靠地自动区分
"真降级的节点"和"健康但历史抖动被积分进来的节点"**——两轮评审在实时
数据上证明分不开（US-01 与 US-03 在 24h composite 上仅差 4%）。真正能
区分的是**按目的地的真实延迟**（如 BGP_E/US-01 那条 accounts.google
5–9s 腿），那个信号现在不存在。所以"节点变差自动降 K"这个能力,**需要
先建新信号**,是独立项目,不在本设计承诺内。

---

## 1. 病因（六路评审一致确认，保留）

生产 92 自 2026-07-04 起每天 ~50 次换槽（实时 `swap_count_24h` 39–70）。
根因是结构性的，三个机制互相打架：

1. **`computeK`（`helpers.go:173-217`）把 K 当强制填满目标。** 实时
   K=8，qualified=10。
2. **fast-evict（`scorer.go:703-722`）无迟滞、无绝对下限。** 门限 = 池内
   short-EWMA 中位数 × 2.0。边缘节点一抖就被剔。
3. **fill 后处理（`scorer.go:799-826`）无条件回填到 kTarget。** 换上另一个
   节点，下一轮再换回来。`readmit_strikes: 1`（实测）意味着被剔节点下一轮
   就够格回收。

评审实测：48h 内 44 次 fast-evict **全部**是 ash 三兄弟（US-01/US-03/
US-04），add/remove 计数完美对称，纯乒乓；这条路径占 **54% 抖动，完全
不经过 `swap_threshold`**。实时 transitions 抓到 07:58 池子掉到 6、08:03
回 8、08:13 把 BGP_B 换出、US-04（p95=3089）换入、08:18 又换回——好节点
之间也在乒乓。

### 1.1 为什么"调参 / 静态 k_max:7"都不够
- 调 `swap_threshold` 只作用于 promote（~42%），对 54% 的 fast-evict 无效。
- 静态 `k_max:7` 能止今天的乒乓,但把问题平移成"好节点数一漂移就复发",
  被迫追版。仍可作**临时止血带**（§7），不作终态。

### 1.2 什么必须留
Plan A 边界：liveness（30s）→ mihomo，分级/慢决策（5min）→ scorer。本设计
整个活在 5min 慢轴，不回退它。catastrophic 快路径（fail_rate ≥ 0.9 →
`Qualified=false`）保留。

### 1.3 两个被否机制的教训
- **相对中位数带**对整池相关退化失明（5/7 好节点同属 Hutao，它们一起慢
  时带跟着上移）。
- **绝对门读 composite 长 EWMA**：composite = `p95 + 2·jitter +
  5000·failRate²`（`helpers.go:141-165`，`scorer.go:525` 喂入），jitter 项
  让 Hutao（jitter 519–785）虚高 +1000~1570；ash 三兄弟历史积分后挤在
  3215–3355（4%）不可分。**代码里根本没有 raw-p95 的长 EWMA 序列。**
  → 现有任何标量绝对门都无法分离 US-03 与 US-01。

---

## 2. 目标与非目标

**目标**
1. 池大小自动收敛到稳定档、不再强制 8、停止乒乓，不手挑成员。
2. 单节点 catastrophic 失败仍被立即剔除（现有快路径）。
3. 池成员稳定（每个终端出口 IP 稳定数小时以上，保匿名性）。

**非目标（诚实划清）**
- **不承诺"用现有信号自动识别并剔除慢节点"** —— 做不到（§0/§1.3）。
  这需要先建按目的地真实延迟信号，是独立后续项目（§8）。
- 不做 v1 相对带、不做 v2 绝对 Schmitt 剔除门、不做 M-轮 dwell（评审
  证明在 24h EWMA 上冗余：20 分钟只动 0.4%）。
- 不换选池信号（gstatic 长 EWMA 排序够用于"选哪几个"）。
- 不碰 `subscriptions:` / manual 模式 / mihomo 数据面。
- 不动未实测的 `cap_per_node`（`stress.sh EGRESS=` 单出口模式从未跑过）。

---

## 3. 设计（design Z：消灭抖动，四个改动）

### 3.1 删除无条件回填 + K 降为上限（核心）
- 删除 fill 后处理里 `if len(newPoolSet) < kTarget` 的填充块
  （`scorer.go:799-826`）。`computeK` 输出 `K_ceiling` **只做上限**。
- 保留：`min_pool_size` 保命补齐（`scorer.go:828+`）；over-ceiling trim
  （`scorer.go:872+`）当"≤ K_ceiling" clamp。
- `k_max` 回 12 当安全上限。`t_active`/`cap_per_node` 当上限用够宽松即可。
- **硬不变量 `min_pool_size ≥ 2`**（低于 2，per_terminal top-2 渲染静默
  失效，`perterminal.go:83,112`，匿名性消失）。默认 3。

> **注意（评审 F2）**：单删这一条**不收敛**。fast-evict 仍会把节点剔到
> 池外(pool 8→7)，`readmit_strikes:1` 让它下一轮回收(7→8)，变成 8↔7
> 每两轮震一次。所以 §3.2 是**同批必须**的，不是可选项。

### 3.2 修 fast-evict：加迟滞 + 绝对下限（与 3.1 同批，非可选）
现状 `scorer.go:703-722` 无迟滞、瞬时剔除。改为：
- **复用现有 `evict_strikes`（实测=2）做迟滞**：short-EWMA 越线需**连续
  `evict_strikes` 轮**才剔除，而非瞬时。单轮抖动不再触发换槽。
- **对称 readmit**：`readmit_strikes` 保持（=1 偏激进，建议随本次调到 2–3，
  让回收也要连续几轮，杜绝 8↔7 抖）。
- 这条直接掐掉 54% 的乒乓触发器。fast-evict 仍在（保留"某节点持续显著
  差于池中位数就该走"的能力），但要求持续性,不再对单帧噪声反应。

### 3.3 边界 swap 迟滞：止住好节点之间的残余乒乓（评审 G1）
删掉强制回填后，qualified(10) > K_ceiling(8) 仍需**排序砍到 8**。排序键
是长 EWMA-composite，5 个 Hutao 挤在 1275–1412（**137 点**），而
`swap_threshold_score=100 < 137`——闸门拦不住簇内重排，好节点之间继续
乒乓（实测 08:03→08:18 就是）。改为：
- **挑战者要顶掉在池成员，其长 EWMA 需优于"最差在池成员"至少一个
  margin，且该 margin > 簇内 spread**。把 `swap_threshold_score` 从 100
  提到 **≥ 200**（覆盖 137 的 Hutao 簇内跨度，留余量），或等价地给
  "挑战者顶替在位者"加一个连续 N 轮 dwell。
- 效果：在位的好节点获得**在位优势**,近似节点之间不再每轮重排。这正是
  你要的"收敛"——池子锁定在一组稳定的好节点上，而不是在等价节点间抖。

> 评审提醒：排序键（gstatic 长 EWMA-composite）与真实用户延迟相关性弱
> （FINDING-final-ranking 报 ρ≈−0.26）。这**更**是给在位者加迟滞的理由
> ——既然排序键本身不可靠，就别每轮按它重排,冻结在位者更稳。

### 3.4 前置修复（独立真 bug，无论如何都做）
- **`poolSet` 重启恢复**：`loadState`（`persist.go:62-101`）恢复
  `effectivePool`/`ewma`/`transitions`/`state`，**从不恢复 `poolSet`**。
  每次重启 bootstrap 轮走无护栏路径、迟滞状态丢失。v3 快照增
  `PoolSet []string`（JSON 向后兼容，老文件缺键→nil，不崩）。冷启时用
  当前信号新算一次、不信任盘上陈旧 `strikes`（现网死节点 strikes 高达
  2396–12014）。
- **`ProbeResult` TTL**：3–4 个死节点探针数据 195–253h stale 仍报 4/4 ok，
  污染 qualified 判定。加 TTL，过期视为无测量。
- **接 Lark webhook**：节点 `notifications.lark.webhook_url` 当前为空,
  无任何告警通道。上线前必接（§3.5 依赖它）。

### 3.5 相关退化的兜底（现有信号能做到的部分）
整池相关退化（如整个 Hutao 订阅变慢）**靠换节点解决不了**，归运维：
- `min_pool_size` 保命网从"最好的现有节点"补齐（即便都变慢）——宁可用
  慢节点不可无池。**保命网只保证 size 稳定，不做质量门**（评审 G2：net
  按 EWMA 确定性挑最好，size 不抖；但它 admit 的节点是 `in_pool &&
  !qualified`,合法）。
- 触发告警：qualified 数贴近 min_pool、或池中位数长 EWMA 突破一个**仅
  用于告警、不用于剔除**的绝对 sanity 线（这条线可以用 p95 瞬时值,因为
  只发通知不做决策,不怕它噪声）。**不 eviction-gate。**

### 3.6 观测语义 + 配置灰度
- `/api/status` 增 `in_qualified_count` 与 `pool_size`；`supply_limited`
  重定义为 `in_qualified_count < K_ceiling`（评审集成审计 F1）。
- 新增 `node_qualify.sizing_mode: convergent | legacy`，默认 legacy；
  **显式枚举校验**，非法值 fail-closed 到 legacy（yaml 非严格解析，打错
  字会静默跑错 sizer）。仅在 `case "auto"` 分支内切换（manual +
  emergency_promote_chain 不受影响，`scorer.go:560,578-584` 隔离干净）。
- §3.5 的够格集必须继承现有过滤器 `!inQuarantine && (!inTrial ||
  currentlyInPool)`（`scorer.go:655-668`），否则 `POST /api/pool/rollback`
  隔离会被下一轮推翻。

---

## 4. 这套改动实测能收敛到什么

实时（2026-08-31）gstatic p95 + 长 EWMA-composite：

```
7 个稳定档(2 fishcloud + 5 Hutao):  长EWMA 387–1412
3 个 ash(US-01/US-04/US-03):        长EWMA 3215/3348/3355  ← 系统一贯的边缘档
```

- 系统几周来稳定地把 **US-01/US-03/US-04 三个 ash 当边缘档**（44/44 次
  fast-evict 全是它们）。这个 7-vs-3 的分裂**正是你说的"8 个里质量差距
  大、该滤掉差的"**——你是对的,稳定池就是那 7 个非 ash。
- **改动后的稳态**：K_ceiling=8 时，排序取前 8 = 7 个稳定档 + 1 个最好的
  ash（当前 US-01）。fast-evict 迟滞 + 边界迟滞让这第 8 个**稳定锁定**在
  一个 ash 上，不再乒乓。**一个稳定的第 8 名 > 乒乓的第 8 名**——前者给
  那 ~13% 终端稳定出口 IP（保匿名），后者两者皆失。
- 若你要**只留 7 个**（把整个 ash 档排除），当前信号**无法自动、可靠地
  做到**（§0）——只能靠临时 `k_max:7`（止血带，会随供应漂移失效）或等
  新信号项目（§8）。这是诚实的边界。

容量校验：实测需求 14–21 活跃终端；K=7 → 7×实测单节点容量(≈6，`stress.sh
EGRESS=` 未跑,待测)≈42，够。K=8 更宽裕。

---

## 5. 自愈行为（正面回答"节点变差是否要再发版下调"）

**能自愈的**：单节点 catastrophic 失败（fail_rate ≥ 0.9）→ 现有快路径
立即剔除、池收敛、不回填、不发版。恢复 → 排序自然回收。

**不能自愈的（诚实）**：单节点**渐变慢**（没到 catastrophic、但真的变差）
→ 现有 gstatic 信号无法与"健康但抖动"区分（§1.3），系统**不会**自动降 K。
要获得这个能力,必须先建按目的地真实延迟信号（§8）。在那之前,渐变慢
节点的处理仍是**运维介入**（看告警 + 手动 `k_max` 或 pool_members 应急）。

所以对你最初的问题：**"节点变差自动离池、不用发版"这个理想,一半能做到
（catastrophic）、一半做不到（渐变慢，缺信号）。** 我不假装 Z 全做到了。
Z 保证的是**不再乒乓、稳定收敛**;完整自愈是 §8。

---

## 6. 影子模式验证（切换前必做）
新逻辑并行只记日志 ≥24h，不改实际成员。通过标准：稳态池大小稳定
（预期 8，或运维选择 k_max:7 时为 7），**零乒乓**；注入单节点持续退化，
验证连续 `evict_strikes` 轮离池、不回填、无 8↔7 抖；冷启（空 poolSet）
与老状态文件缺 PoolSet 两条路径都不塌到 min_pool。仅影子达标才切
`sizing_mode: convergent`。

---

## 7. 分阶段上线

| 阶段 | 内容 | 代码? | 回滚 |
|---|---|---|---|
| 0 | 接 Lark webhook；`cp /usr/local/bin/leap-gateway{,.bak-preplan}` + 备份 on-node yaml | 否 | — |
| 1 | `ProbeResult` TTL + `poolSet` 恢复 + `openai-pool` 删除 | 是 | 换回二进制 |
| 2 | 删无条件回填 + K 上限 + fast-evict 迟滞 + 边界 swap 迟滞（§3.1-3.3，`sizing_mode` flag 默认 legacy，**影子模式** §6） | 是 | flag 切回 legacy |
| 3 | 影子达标切 `sizing_mode: convergent`；`swap_threshold_score` 提到 ≥200；`readmit_strikes` 调到 2–3 | 配置 | flag/值切回 |

**临时止血带（可选，解耦）**：阶段 1–3 完成前想立刻压住抖动，可先上
`k_max: 7`（`deploy.sh 92 --config`，热重载不重启 mihomo，一次性 ~13%
终端重映射）。止血带非治疗（§1.1），convergent 生效后回退 12。走不走
看你更在意"今天就停"还是"少一次终端重映射"。

---

## 8. 后续独立项目：真实质量信号（本设计不承诺、不阻塞）
要获得"自动识别并剔除渐变慢节点"的能力，需先解决现有信号缺陷：
- **保留多目的地延迟历史**（现在每 (node,probe) 只留一份 + 3 深布尔，
  无延迟序列）。
- **修采样**：site 探针串行、每轮最坏 402–560s > 300s 轮次，样本时间
  不可比；需并行化（多 `probe-out` selector）。
- **聚合选择**：median-of-medians 会盖住单腿真故障（accounts.google
  全队 5–10s）；要能识别"活着但某目的地慢"。这是 v1 栽的地方，需专门
  设计，不是简单换个聚合函数。

此项目完成后，§3 的排序键和告警可换到真实信号上，"渐变慢自动剔除"
才成立。届时 `k_max` 可永久留 12。

---

## 9. 六路评审结论落点（存档）
- **核心正确**：删强制回填 + K 上限（`scorer.go:799-826` 确实会 re-admit，
  删它是正解）；拒绝相对带改用绝对底线思路；poolSet 恢复 / TTL / flag+
  shadow / min_pool≥2 都是独立真修复。
- **两轮否决的机制**：v1 相对带（对相关退化失明）、v2 绝对 Schmitt 剔除门
  （composite 信号上 US-01↔US-03 不可分，误杀健康高吞吐节点）。
- **本版据评审修正**：把复杂度从"新质量门"转移到评审指出的真正残余
  ——fast-evict 迟滞（F2）+ 边界 swap 迟滞（G1）；砍掉 M-dwell（在 24h
  EWMA 上冗余,C1）；绝对门降为**仅告警 floor**,不做剔除（F1/C2/C3）;
  诚实标注"渐变慢自动剔除"需新信号,不在本设计承诺内。
