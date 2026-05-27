# Leap Gateway — 部署手册

> 在飞连转发节点上 sidecar 部署 leap-gateway（v3 架构）。本期落地目标是**一台新增的测试转发节点**做 PoC，不动生产 192.168.70.246。

## 0. 部署目标和前置条件

### 部署目标

- **不是**全新机器装 WireGuard 网关。
- 是在**飞连 SaaS 控制端新增的一台转发节点**上，跟飞连进程并存，旁路接管员工的 forward 流量做透明分流。
- 整个 PoC 期生产 246 不动。

### 前置条件

| 项 | 要求 |
|---|---|
| 飞连 SaaS 后台 | 用户能新增一台转发节点 + 给 sudo 权限 + 后续能改 push DNS 字段 |
| 节点 OS | Ubuntu 22.04+，kernel ≥ 5.15（飞连兼容版本） |
| 节点资源 | 2C/4G 起步，员工 ≤50 人；磁盘 ≥10G |
| 节点出口 | 公网可达，能走 1.1.1.1:853 (Cloudflare DoT) |
| 节点客户端池 | 飞连 SaaS 分配的 CIDR（生产 246 是 `10.8.8.0/24`，PoC 节点会是不同段，比如 `10.8.11.0/24`） |
| 系统包 | `iproute2`, `nftables`, `tcpdump`, `inotify-tools`, `curl`, `dig` |
| 节点权限 | sudo / root |
| 机场订阅 | 已测过的可用订阅 URL |

### 你已经填到飞连 SaaS 的字段

新增转发节点时飞连 SaaS 会问你两件事：

| 字段 | 推荐值 | 说明 |
|---|---|---|
| **DNS 地址** | 先填 `223.5.5.5`（占位），Stage 2 改成 `<tun0_gw>` (主) / `223.5.5.5` (备) | "通过 VPN 节点下发的 DNS 地址" — 即客户端 push DNS。先用占位避免在 leap 没装上之前员工断网 |
| **NAT 转换** | **启用** | 飞连用 `FEILIAN_VPN_POSTROUTING_POOL` MASQUERADE。我们 sing-box 在 hijack 后会用节点本机 IP 重新建连，所以 NAT 启不启用不影响我们这一层；启用 NAT 不要求公司内网为 10.8.x.0/24 加路由 |

## 1. Pre-Implementation Validation（Stage 0，必须先跑）

写代码 / 部署 leap 之前先做 24-48h 的小规模验证，否则可能整个 PR1+PR2 白做。

| 风险 | 验证方法 | 通过条件 |
|---|---|---|
| **H1** 飞连客户端 DNS push 是否系统级覆盖 | 测试机接入 PoC 节点后，`tcpdump -i any 'port 53 and not host <push_dns>'` 跑 1h，统计漏出查询 | 漏出 < 5% 总查询；否则 fake-IP 路径不可行，必须切到"sing-box 真解析 + sniff"模式 |
| **H2** 飞连 SaaS 是否会重置我们部署的内容 | PoC 节点装 `inotifywait /etc /usr/local/bin /var/lib/leap` + `auditctl -w /etc/nftables -p wa`，跑 24h | 我们的目录 / nftables 没被外部进程触碰 |
| **M3** rp_filter 能否改 | `sysctl net.ipv4.conf.all.rp_filter` 看当前值；改成 2 后跑 30min 业务检查飞连功能正常 | 改完飞连 L4 proxy 仍然工作，员工流量正常 MASQUERADE 出 ens18 |
| **M4** 飞连 SaaS 推送频率 | `journalctl -u feilian-tun@tun0 --since=72h \| grep -iE 'start\|restart\|stop'` 数重启次数 | < 2 次/天；否则把 `BindsTo=` 改成 `Wants=` 自检 tun0 |
| **M3 补充** ICMP / IPv6 | 测试机 `ping 8.8.8.8`、`ping6 2606:4700:4700::1111`，看包是否被 sing-box 接管 | TCP/UDP 工作即视为通过，ICMP/v6 失败可写进 README 当限制 |

只要 H1 / H2 任一不通，**plan 必须重新设计**：
- H1 不通 → 退到 sing-box 真解析 + TUN sniff（详见 [dns.md §5.4](dns.md#54-应急h1-不通时退到真解析--sniff)）。
- H2 不通 → 加 `leap-watchdog` 进程定期重申 nft + ip rule + sysctl。

### 1.1 H1 验证执行步骤

在 PoC 节点上飞连 SaaS 把 push DNS 配成临时占位（如 `223.5.5.5`）后：

```bash
# 测试机（已连飞连 PoC 节点）
PUSH_DNS=223.5.5.5     # 飞连 SaaS 后台填的那个值
sudo tcpdump -i any -n "port 53 and not host $PUSH_DNS" -w /tmp/leak.pcap &
TCPDUMP_PID=$!

# 正常使用 1h：浏览器（含触发 DoH 的 Chrome）、收邮件、看视频、Telegram、IDE...
sleep 3600

sudo kill $TCPDUMP_PID
tcpdump -nr /tmp/leak.pcap | wc -l
tcpdump -nr /tmp/leak.pcap | awk '{for(i=1;i<=NF;i++) if($i~/^[0-9.]+\.53$/) print $i}' | sort -u | head
```

漏出比例 = 漏出查询数 / (漏出 + 走 push DNS 的查询数)。后者抓：

```bash
sudo tcpdump -i any -n "port 53 and host $PUSH_DNS" -G 3600 -W 1 -w /tmp/normal.pcap
tcpdump -nr /tmp/normal.pcap | wc -l
```

### 1.2 H2 验证执行步骤

在 PoC 节点上：

```bash
sudo apt install inotify-tools auditd
sudo inotifywait -mr -e create,delete,modify,attrib \
  /etc /usr/local/bin /var/lib /etc/systemd/system \
  > /tmp/inotify.log 2>&1 &
sudo auditctl -w /etc/nftables.conf -p wa -k leap-h2
sudo auditctl -w /etc/leap -p wa -k leap-h2          # 等 leap 装上再加
journalctl -fu feilian-tun@tun0 > /tmp/feilian-tun.log 2>&1 &

# 24h 不操作节点
sleep 86400

# 看是否有外部修改
grep -E 'feilian|/etc/leap|/usr/local/bin' /tmp/inotify.log | head
ausearch -k leap-h2 | head
```

### 1.3 M3 / M4 快速跑

```bash
# 现在的 rp_filter
sudo sysctl net.ipv4.conf.all.rp_filter

# 现在 nft 基线（保存以后对比用）
sudo nft list ruleset > /tmp/nft.before.txt

# 飞连 tun 重启频率
journalctl -u feilian-tun@tun0 --since=72h | grep -iE 'start|restart|stop' | wc -l
```

## 2. 部署（一键）

Stage 2 之后部署被打包成 `scripts/stage.sh` + `deploy/node/install.sh` 两步，整个过程在节点上不需要任何手工 systemd / nft / iproute 操作。

### 2.1 操作机：构建 staging tarball

```bash
cd ~/code/leap-gateway

# 1. 复制配置模板，填好订阅 URL 和飞连 SaaS 给 PoC 节点的 CIDR / 网关 IP / 出口网卡
cp configs/gateway.example.yaml gateway.yaml
$EDITOR gateway.yaml      # subscriptions[*].url、node.{client_subnet,tun0_gateway_ip,egress_iface}

# 2. 一条命令出可部署 tarball（build leap-gateway + 下载 sing-box 1.10.7 + 打包 deploy/node/）
./scripts/stage.sh
# → ./build/leap-stage.tgz （约 14 MB）
```

`scripts/stage.sh` 干的事：
- `GOOS=linux GOARCH=amd64 go build` 出静态 binary（要交叉到 ARM 节点：`GOARCH=arm64 ./scripts/stage.sh`）。
- 下载 `sing-box-1.10.7-linux-amd64.tar.gz` 到 `./.cache/`（再次跑用缓存）。
- 把 binary + 你填好的 `gateway.yaml` + `deploy/node/{install.sh,iproute.sh,nft.conf.tmpl,logrotate-leap.conf,*.service}` 平铺到 `./build/leap-stage/`，再 tar。

### 2.2 节点：解包 + 安装

```bash
scp ./build/leap-stage.tgz dianwei@<node>:/tmp/
ssh dianwei@<node>
$ cd /tmp && tar -xzf leap-stage.tgz
$ sudo bash leap-stage/install.sh
```

`install.sh` 会按顺序做：
1. **preflight** — 校验 `feilian-tun@tun0` active、`tun0` IP 跟 `gateway.yaml` 的 `tun0_gateway_ip` 对得上、`rp_filter` 是 0/2、`ip_forward=1`、`<gw>:53` 没人占、`api.listen` 端口没冲突、FeiLian 自己的 nft 链还在。
2. 装 `/usr/local/bin/sing-box`、`/usr/local/bin/leap-gateway`、`/etc/leap/gateway.yaml`、`/etc/leap/iproute.sh`、渲染 `/etc/leap/nft.conf`（占位 `@CLIENT_SUBNET@` 替换成 `gateway.yaml` 里的值）。
3. 装 systemd unit + logrotate，`daemon-reload`。
4. **`leap-gateway --render-once`** 预渲染 sing-box 配置到 `/etc/leap/singbox/config.json`（避免 leap-singbox 第一次启动因找不到 config 失败重启）。
5. `systemctl enable --now leap-singbox leap-gateway leap-nft`，三个一起起。

跑完正常应该看见：

```
[install] preflight: feilian-tun@tun0 active
[install] preflight: tun0=10.8.12.1 matches gateway.yaml
[install] preflight: rp_filter=2 (loose, OK)
[install] preflight: ip_forward=1
[install] preflight: 10.8.12.1:53 free
[install] preflight: api.listen 127.0.0.1:18080 free
[install] preflight: all good
[install] sing-box: sing-box version 1.10.7
[install] leap-gateway installed
[install] gateway.yaml installed
[install] nft.conf rendered (client_subnet=10.8.12.0/24)
[install] iproute.sh installed
[install] systemd units installed
[install] bootstrap singbox config rendered at /etc/leap/singbox/config.json
... systemctl status output ...
[install] DONE
```

幂等：再跑一次只是覆盖 binary 和 config。改完 `gateway.yaml` 想 push 上去：直接 `scp` 新 yaml 进 `/etc/leap/gateway.yaml` 然后 `systemctl restart leap-gateway`，不用整个重装。

### 2.3 sysctl（可选 — 如果 M3 验证发现 rp_filter 不是 0/2）

PoC 节点 (Ubuntu 22.04 飞连镜像) 默认就是 `rp_filter=2`。如果是别的环境 preflight 报 rp_filter 错，先停下确认 M3 验证通过，再写 dropin：

```bash
sudo tee /etc/sysctl.d/99-leap.conf <<EOF
# fwmark policy routing 要求 loose RPF
net.ipv4.conf.all.rp_filter=2
net.ipv4.conf.default.rp_filter=2
net.ipv4.ip_forward=1
EOF
sudo sysctl --system
```

> ⚠️ 改 `rp_filter` **必须先做 M3 验证**——飞连 L4 proxy 的回包靠 RPF 通过，rp_filter 改坏可能导致飞连业务整个挂掉。

## 3. systemd / nft 架构（Stage 2 落地版）

> 这一节解释装好以后系统是怎么自洽的，看完知道发生异常时去哪儿排查。具体 unit 文件在 `deploy/node/*.service`，install.sh 会原样装到 `/etc/systemd/system/`。

### 3.1 三个 unit 的关系

```
feilian-tun@tun0.service          (飞连原生，tun0 起来才有客户端)
        ▲ BindsTo=
        │
leap-singbox.service              ← sing-box 数据面：utun-leap、DNS:53、clash-api:9090
        ▲ Wants= + PartOf=        ← PartOf 让 sing-box 一重启 leap-nft 跟着 restart
        │
leap-nft.service (oneshot)        ← nft table inet leap + ip rule + ip route table 100

leap-gateway.service              ← 控制面：拉订阅 + 渲 sing-box config + 触发 reload
   After= Wants= leap-singbox     ← 启动顺序，不强绑生命周期
```

### 3.2 leap-gateway 怎么做 reload

`leap-gateway` 拉到新订阅后：
1. 调 renderer 把节点列表渲染成完整 sing-box config 写到 `/etc/leap/singbox/config.json`。
2. **`exec.Command("systemctl", "restart", "leap-singbox.service")`** — sing-box 重启读新 config。

> 注意：sing-box 1.10.x 的 clash-api `PUT /configs?force=true` **是 no-op stub**（用 `-c file` 启动时它返回 204 但不真热重载）。所以 leap-gateway 用 systemctl restart 而不是 clash-api。clash-api 只用于查询（`/proxies`、`/connections`、`/version` 等）。

### 3.3 sing-box 重启时 nft 怎么自动复位

sing-box 重启 → 内核销毁 + 重建 `utun-leap` → **内核会顺手把 `default dev utun-leap` 路由删掉**。如果 leap-nft 不重跑，table 100 就空了，fwmark 0x42 流量没出口。

解决方式：`leap-nft.service` 加 `PartOf=leap-singbox.service`。systemd 在 leap-singbox 重启时会一起 restart leap-nft：
- ExecStop：`nft delete table inet leap` + `iproute.sh down`
- (sing-box 此刻在 stop → start，utun-leap 重建)
- ExecStart：`ExecStartPre` 等 utun-leap 出现（最长 30s）→ `nft -f /etc/leap/nft.conf` + `iproute.sh up`

整条链 5-10s 自愈。期间客户端访问海外站会断一次，国内站走 direct 不受影响。

### 3.4 端口

- `127.0.0.1:8080` 被 **`feilian-sentry`** 占了（飞连原生 sidecar，UI/状态用），所以 `leap-gateway` API 默认 `127.0.0.1:18080`。
- `127.0.0.1:9090` 是 sing-box clash-api。
- `<tun0_gw>:53` UDP/TCP 是 sing-box 的 fake-IP DNS server，飞连 SaaS push 给客户端的 DNS 就指这里。

`install.sh` preflight 都会校验，遇到冲突会 fail 并打印占用的进程。

## 6. 飞连 SaaS 改 push DNS

leap 服务起来 + 测试节点本机 dig 验证通过之后，在飞连 SaaS 后台改 PoC 节点的 push DNS：

| 字段 | 值 |
|---|---|
| 主 DNS | `<tun0_gateway_ip>` （PoC 节点 = `10.8.12.1`） |
| 备 DNS | `223.5.5.5` |

主备两条都让客户端看到，OS 层故障容灾——leap 挂掉时系统自动落到 223.5.5.5。

测试机 disconnect/reconnect 飞连后：

```bash
# Mac
scutil --dns | grep nameserver | head -3
# 期望看到指向 10.8.12.1 的 resolver（飞连客户端通过 utun8 上的 127.0.0.1:53 代理转发）
```

## 7. 验证

### 7.1 节点本机自测

```bash
# DNS 走 fake-IP
dig @10.8.12.1 google.com
# 期望：198.18.x.x

# DNS 走 local DoH
dig @10.8.12.1 baidu.com
# 期望：国内真 IP（111.63.x / 153.3.x 等）

# 路由表
ip rule | grep 0x42
# 期望：100:	from all fwmark 0x42 lookup 100
ip route show table 100
# 期望：default dev utun-leap scope link

# nft 独立 table
sudo nft list table inet leap
# 期望：prerouting chain，三条规则（DNS udp accept / DNS tcp accept / 其他 mark 0x42）

# 飞连原 nft 没动
sudo nft list table ip nat | grep -E 'FEILIAN_(PROXY|VPN_POSTROUTING)'
# 期望：原样在

# sing-box 健康
curl -s 127.0.0.1:9090/version
curl -s 127.0.0.1:9090/proxies | jq '.proxies | keys | length'
# 期望：proxies 数量 = 订阅节点数 + selector/urltest/direct/dns-out/block 等内置

# leap 健康
curl -s 127.0.0.1:18080/healthz
curl -s 127.0.0.1:18080/api/status | jq .
```

### 7.2 测试机端到端（PoC 实测命令）

```bash
# DNS 直接打 sing-box（不经飞连客户端代理）
dig +short @10.8.12.1 baidu.com    # 期望 111.63.x（真 IP）
dig +short @10.8.12.1 google.com   # 期望 198.18.x.x（fake-IP）

# 翻墙路径 — 应该经 urltest 出机场节点
curl -sv -o /dev/null -w "code=%{http_code} ip=%{remote_ip} time=%{time_total}s\n" https://www.google.com
# 期望：code=200 / ip=198.18.x.x（fake）/ time<2s

# 看实际出口
curl -s https://api.myip.com
# 期望：机场节点国家（PoC 拿到 yuyun 新加坡 → SG）

# 国内 → 直连
curl -sv -o /dev/null -w "code=%{http_code} ip=%{remote_ip} time=%{time_total}s\n" https://www.baidu.com
# 期望：code=200 / ip=153.3.x（真 IP）
```

### 7.3 sing-box clash-api 看分流

```bash
ssh -L 9090:127.0.0.1:9090 ppio@<poc-node>
# 浏览器访问 http://127.0.0.1:9090/ui （需要 yacd / metacubexd 等前端）
# 或直接抽样：
curl -s 127.0.0.1:9090/connections | jq '.connections[] | {host, network, rule, outbound}' | head -20
# 期望：访问 google.com 的连接 outbound=urltest 或具体节点；访问 baidu.com 的连接 outbound=direct
```

### 7.4 故障演练

```bash
sudo systemctl stop leap-singbox
# 测试机继续访问 baidu.com → 通（fallback 到 223.5.5.5）
# 访问 google.com → 超时（不在 GFW 后没有代理路径）
sudo systemctl start leap-singbox
# 等 30s，全部恢复
```

### 7.5 隔离验证

```bash
# 飞连原有完全没动
sudo systemctl status feilian-vpn feilian-vpn-proxy feilian-vpn-sentry feilian-tun@tun0 | head -20
sudo nft list table ip nat | diff - /tmp/nft.before.txt | grep -v leap
# 仅有的差异应是飞连自己的计数器，不是我们注入的链
```

## 8. 升级与回滚

订阅刷新已在控制面里跑（默认 30min）；改完 `gateway.yaml` 后立即触发：

```bash
# 触发 leap-gateway 重新拉订阅 + 重渲 sing-box config + systemctl restart leap-singbox
curl -X POST 127.0.0.1:18080/api/subscribe/refresh
# 5-10s 后流量恢复（期间海外站会断一次，国内站走 direct 不影响）
```

> 注意：reload 路径走的是 `systemctl restart leap-singbox`，**不是** clash-api 热重载（sing-box 1.10.x 那个端点是空的）。所以一次 refresh 会让 sing-box 真重启一次。

升级 sing-box 二进制：

```bash
# 1. 在操作机上把新版本 sing-box 直接放进 staging 重打包
SINGBOX_VERSION=1.10.X ./scripts/stage.sh
scp ./build/leap-stage.tgz dianwei@<node>:/tmp/

# 2. 节点上重新解包 + 跑 install.sh（幂等覆盖 binary，systemctl 自动 restart）
ssh dianwei@<node>
$ cd /tmp && tar -xzf leap-stage.tgz && sudo bash leap-stage/install.sh
journalctl -u leap-singbox -n 50
```

回滚 = 把上一次的 staging tarball 重新 `install.sh` 一遍。**强烈建议升级前在 lxc / dev 节点先 `sing-box check` 验证。**

紧急停机（不影响员工，员工系统自动 fallback 到 223.5.5.5）：

```bash
sudo systemctl stop leap-singbox leap-gateway leap-nft
# 飞连 SaaS 后台可选把主 DNS 改回 223.5.5.5（保险，避免客户端缓存的 fake-IP 在 leap 起回来前误连）
```

## 9. 排障速查

| 现象 | 排查 |
|---|---|
| 客户端 dig 拿到真实 IP（不是 198.18.x） | 飞连 DNS push 没生效或被绕过（H1）。看 [dns.md §5](dns.md#5-飞连-push-dns-的语义和-h1-风险) |
| 客户端拿到 198.18.x 但访问不通 | `ip rule \| grep 0x42`、`ip route show table 100` 不为空；`sudo nft list table inet leap` 看 chain；clash-api `/proxies/urltest/delay` 测节点延迟 |
| 海外流量走了 direct (从节点 ens18 出而不是机场) | clash-api `/proxies` 看 urltest.all 是否为空（订阅没拉到节点）；`journalctl -u leap-gateway \| grep refresh`；手动 `curl -X POST 127.0.0.1:18080/api/subscribe/refresh` 看错误 |
| `ip route show table 100` 空但 nft / ip rule 在 | sing-box 重启过但 leap-nft 没跟着 restart——检查 `leap-nft.service` 是否有 `PartOf=leap-singbox.service`；`systemctl restart leap-nft` 手动恢复 |
| 国内站点慢 | sing-box 日志看 `server=local` DoH 错误；备份 DoH 换阿里 (`https://dns.alidns.com/dns-query`) |
| 节点全部红色 / urltest 无可用节点 | 订阅 token 过期；`curl https://yuyun.../...` 直接验订阅可达；clash-api `/proxies` 看延迟 |
| 飞连 L4 proxy 异常 | `rp_filter=2` 是不是改坏了；`sudo sysctl net.ipv4.conf.all.rp_filter`；改回 1 看是否恢复 |
| `leap-gateway` API bind 失败 (`address already in use`) | 18080 被别的进程占了——`ss -tulnp 'sport = :18080'`；改 `gateway.yaml api.listen` 到别的端口（**避免 8080**，feilian-sentry 占着） |
| `table inet leap` 突然不见了（H2 触发） | 看 `journalctl -u leap-nft`；可能是飞连 SaaS push 触发 `nft flush ruleset`。需要加 watchdog 定期重申 |
| `feilian-tun@tun0` 频繁重启（M4 触发） | `journalctl -u feilian-tun@tun0`；`leap-singbox.service` 的 `BindsTo=` 改成 `Wants=` |

## 10. Stage 推进顺序

| Stage | 做什么 | 在哪做 | 状态 |
|---|---|---|---|
| **Stage 0** | §1 Pre-Implementation Validation (H1/H2/M3/M4) | PoC 节点 + 测试机 | ✅ 完成 (2026-05-27, H1=0 leak / H2=no external touch / M3=rp_filter 默认 2 / M4=0 restarts in 72h) |
| **Stage 1** | PR1 渲染器 + PR2 部署脚本 | 本机 (lxc lab 跳过——sing-box check 静态校验已覆盖) | ✅ 完成 |
| **Stage 2** | §2-§7 完整部署 + 端到端验证 | PoC 节点 (192.168.70.92) + 测试机 | ✅ 完成 (2026-05-27, 节点上 sing-box urltest=66 nodes / 测试机 google.com 经新加坡 SG 出口 / baidu.com 直连真 IP) |
| **Stage 3** | 决定是否切到生产 246 | — | ⏳ 待用户拍板。Stage 2 稳定 1-2 周后启动；**不在本期范围** |
