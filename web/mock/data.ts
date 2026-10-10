// A fictional node for `npm run mock` / `npm run preview` and the README
// screenshots. Shapes mirror real /api responses (captured from production
// and anonymised): provider names, node names, IPs and hostnames are made up,
// egress uses TEST-NET-3 (203.0.113.0/24).
import type {
  Active,
  Healthz,
  NodeHealth,
  NodesHealth,
  ProbeResult,
  Status,
  Subscription,
  TerminalAssignment,
  Transition,
} from '../src/types';

const PROBES = ['openai-api', 'anthropic-api', 'accounts.google.com', 'github'];
const SUBNET = '10.8.11.0/24';

const iso = (msAgo: number) => new Date(Date.now() - msAgo).toISOString();
const MIN = 60_000;
const HOUR = 60 * MIN;

interface Seed {
  name: string;
  p50: number;
  p95: number;
  fail: number;
  alive?: boolean;
  qualified?: boolean;
  inPool?: boolean;
  conns?: number;
  probeFail?: Partial<Record<string, ProbeResult>>;
  reason?: string;
}

const SEEDS: Seed[] = [
  { name: 'northwind/US-LAX-BGP-A', p50: 212, p95: 248, fail: 0, conns: 6 },
  { name: 'northwind/US-LAX-BGP-B', p50: 219, p95: 262, fail: 0, conns: 4 },
  { name: 'northwind/US-LAX-BGP-C', p50: 226, p95: 301, fail: 0, conns: 5 },
  { name: 'northwind/US-SJC-BGP-D', p50: 231, p95: 344, fail: 0, conns: 3 },
  { name: 'northwind/US-SJC-BGP-E', p50: 244, p95: 392, fail: 0, conns: 7 },
  { name: 'skyline/🇺🇸 US-West 01 中转', p50: 251, p95: 266, fail: 0, conns: 4 },
  { name: 'skyline/🇺🇸 US-West 02 中转', p50: 258, p95: 281, fail: 0.1, conns: 2 },
  { name: 'atlas/美国 IEPL 01 | 专线', p50: 263, p95: 512, fail: 0, conns: 5 },
  { name: 'atlas/美国 IEPL 02 | 专线', p50: 270, p95: 655, fail: 0.2, conns: 3 },
  {
    name: 'atlas/美国 HE.2x', p50: 288, p95: 334, fail: 0, conns: 2,
    probeFail: { github: { ok: false, status_code: 403, latency_ms: 812, last_error: 'status Forbidden' } },
  },
  { name: 'atlas/美国 HF.2x', p50: 301, p95: 470, fail: 0, conns: 1 },
  { name: 'atlas/美国 HJ.2x', p50: 315, p95: 892, fail: 0.3, conns: 2 },
  {
    name: 'skyline/🇺🇸 US-East 01', p50: 0, p95: 0, fail: 1, alive: false, qualified: false, inPool: false,
    reason: 'no successful probes in last 5',
  },
  {
    name: 'skyline/🇺🇸 US-East 02 IPv6', p50: 0, p95: 0, fail: 1, alive: false, qualified: false, inPool: false,
    reason: 'no successful probes in last 5', probeFail: { '*': undefined },
  },
  {
    name: 'atlas/美国 HA.6x | 专线', p50: 254, p95: 262, fail: 0.7, alive: false, qualified: false, inPool: false,
    conns: 0, reason: 'no successful probes in last 5',
  },
  {
    name: 'atlas/美国 HB.6x | 专线', p50: 623, p95: 2120, fail: 0.7, alive: false, qualified: false, inPool: false,
    conns: 0, reason: 'no successful probes in last 5',
    probeFail: {
      'openai-api': { ok: false, status_code: 403, cf_mitigated: true, latency_ms: 1840, last_error: 'cloudflare challenge' },
    },
  },
  {
    name: 'atlas/美国 HC.2x', p50: 0, p95: 0, fail: 1, alive: false, qualified: false, inPool: false,
    reason: 'no successful probes in last 5',
  },
];

function probesFor(s: Seed): Record<string, ProbeResult> | undefined {
  if (s.probeFail && '*' in s.probeFail) return undefined;
  const out: Record<string, ProbeResult> = {};
  PROBES.forEach((p, i) => {
    out[p] = s.probeFail?.[p] ?? {
      ok: true,
      status_code: p.endsWith('-api') ? 401 : 200,
      cf_mitigated: false,
      latency_ms: Math.round((s.p50 || 300) * (1.2 + i * 0.6)),
      last_checked_at: iso(2 * MIN + i * 1000),
    };
  });
  return out;
}

export function nodes(): NodeHealth[] {
  return SEEDS.map((s) => {
    const sub = s.name.split('/')[0];
    const conns = s.conns ?? 0;
    return {
      name: s.name,
      sub,
      alive: s.alive ?? true,
      rtt_last_ms: s.p50 ? s.p50 + 12 : 0,
      rtt_p50_ms: s.p50,
      rtt_p95_ms: s.p95,
      jitter_ms: s.p95 - s.p50,
      fail_rate: s.fail,
      probe_count: 10,
      throughput_bps: conns * 61,
      qualified: s.qualified ?? true,
      in_pool: s.inPool ?? true,
      strikes: s.qualified === false ? 3 : 0,
      ok_rounds: s.qualified === false ? 0 : 6 + (s.p50 % 17),
      reason: s.reason,
      probes: probesFor(s),
      passive:
        s.conns != null
          ? {
              active_conns: conns,
              closed_window: conns * 7 + 3,
              failed_window: s.fail > 0.5 ? 4 : 0,
              fail_rate: s.fail > 0.5 ? 0.57 : 0,
              sample_window_sec: 600,
            }
          : undefined,
    };
  });
}

export function nodesHealth(): NodesHealth {
  const ns = nodes();
  return {
    qualified: ns.filter((n) => n.qualified).length,
    total: ns.length,
    last_pool_update: iso(4 * MIN),
    last_scored_at: iso(4 * MIN),
    thresholds: {
      MaxRTTP50Ms: 500,
      MaxRTTP95Ms: 800,
      MaxJitterMs: 300,
      MaxFailRate: 0.25,
      Probes: [
        { Name: 'openai-api', URL: 'https://api.openai.com/v1/models' },
        { Name: 'anthropic-api', URL: 'https://api.anthropic.com/v1/models' },
        { Name: 'accounts.google.com', URL: 'https://accounts.google.com/' },
        { Name: 'github', URL: 'https://github.com/favicon.ico' },
      ],
    },
    pool_mode: 'auto',
    nodes: ns,
  };
}

const poolMembers = () => nodes().filter((n) => n.in_pool);

export function status(subs: Subscription[]): Status {
  const ns = nodes();
  return {
    engine_ok: true,
    last_refresh: iso(52 * MIN),
    node_count: subs.reduce((a, s) => a + (s.nodes_count || 0), 0),
    subscriptions: subs.length,
    pool_size: poolMembers().length,
    pool_eligible: ns.filter((n) => n.qualified).length,
    pool_total: ns.length,
    pool_last_update: iso(4 * MIN),
    sizing_mode: 'eligibility',
    pool_sizing: { k_target: 12, k_current: 12, t_active: 50, tier1_count: 12, supply_limited: false },
    pool_stability_24h: {
      window_hours: 24,
      swap_count: 9,
      rollback_count: 1,
      unique_nodes_in: 5,
      unique_nodes_out: 6,
      oldest_at: iso(23 * HOUR),
    },
    capacity: {
      sustained_max_users: 50,
      degraded_max_users: 150,
      measured_at: '2026-06-07',
      measured_with: 'stress.sh / mihomo v1.19.26 / TPROXY+per_terminal; upstream-bandwidth-bound',
    },
  };
}

export function healthz(): Healthz {
  return {
    ok: true,
    asserted: iso(12_000),
    checks: [
      { name: 'ip_rule_fwmark', ok: true, detail: 'fwmark 0x44 lookup 101 present' },
      { name: 'route_table_local', ok: true, detail: 'table 101 has local default' },
      { name: 'prerouting_jump', ok: true, detail: `PREROUTING -i tun0 -s ${SUBNET} -j LEAP_TPROXY present` },
      { name: 'tproxy_flow', ok: true, detail: '187/189 live connections are TProxy' },
    ],
  };
}

export function active(subs: Subscription[]): Active {
  return {
    node: {
      hostname: 'leap-node-a',
      kernel: '5.15.0-179-generic',
      os: 'Ubuntu 22.04.5 LTS',
      uptime_seconds: 41 * 86400 + 5 * 3600,
      client_subnet: SUBNET,
      egress_iface: 'ens18',
    },
    feilian: { tun0_active: true, vpn_active: true, proxy_active: true, sentry_active: true },
    leap: {
      gateway_version: '96cfcb5',
      engine: 'mihomo',
      engine_version: 'v1.19.26',
      services: { 'leap-gateway': 'active', 'leap-mihomo': 'active', 'leap-nft': 'active' },
      subscriptions_count: subs.length,
      nodes_parsed: subs.reduce((a, s) => a + (s.nodes_count || 0), 0),
      last_refresh: iso(52 * MIN),
    },
    active_proxy: {
      reachable: true,
      active_group: 'us-pool',
      pool_filter: '美国|🇺🇸|\\bUS',
      pool_mode: 'auto',
      last_swap_at: iso(4 * MIN),
      pool_size: poolMembers().length,
      pools: [
        {
          tag: 'us-pool',
          pool_size: poolMembers().length,
          active: true,
          egress: {
            ip: '203.0.113.42',
            country: 'US',
            region: 'California',
            city: 'Los Angeles',
            org: 'AS64500 Example Transit, Inc.',
            checked_at: iso(90_000),
          },
          nodes: poolMembers().map((n) => ({
            tag: n.name,
            delay_ms: (n.rtt_last_ms || 0) + 8,
            last_check: iso(20_000),
            fail_rate: n.fail_rate ?? 0,
          })),
        },
      ],
    },
  };
}

export const initialSubs = (): Subscription[] => [
  {
    name: 'northwind',
    url: 'https://sub.northwind.example/api/v1/client/subscribe?token=7f3a***c91e',
    enabled: true,
    format: 'auto',
    user_agent: 'clash.meta',
    nodes_count: 35,
    last_refresh: iso(52 * MIN),
  },
  {
    name: 'skyline',
    url: 'https://198.51.100.7:8443/s/skyline?token=c85b***0f02',
    enabled: true,
    format: 'auto',
    user_agent: 'sing-box/1.10.7',
    nodes_count: 45,
    last_refresh: iso(52 * MIN),
  },
  {
    name: 'atlas',
    url: 'https://atlas-cdn.example.net/link/Qm9***xYz?clash=3',
    enabled: true,
    format: 'clash',
    nodes_count: 46,
    last_refresh: iso(52 * MIN),
  },
];

export function transitions() {
  const t = (h: number, type: string, added: string[], removed: string[], reason: string, source = 'scorer'): Transition => ({
    at: iso(h * HOUR),
    type,
    added,
    removed,
    pool_after: poolMembers().map((n) => n.name),
    reason,
    source,
  });
  // Oldest first, like the real endpoint.
  const list: Transition[] = [
    t(22.5, 'auto_swap', ['atlas/美国 HJ.2x'], ['skyline/🇺🇸 US-East 01'], 'eligibility-set membership change'),
    t(19.2, 'auto_swap', ['skyline/🇺🇸 US-East 01'], [], 'eligibility-set membership change'),
    t(16.8, 'emergency_evict', [], ['atlas/美国 HC.2x'], 'fail_rate >= 0.90 sustained 30m0s'),
    t(12.1, 'auto_swap', ['atlas/美国 HF.2x'], ['skyline/🇺🇸 US-East 02 IPv6'], 'eligibility-set membership change'),
    t(9.4, 'auto_swap', ['atlas/美国 HB.6x | 专线'], [], 'eligibility-set membership change'),
    t(9.1, 'rollback', [], ['atlas/美国 HB.6x | 专线'], 'operator rollback (1 step(s))', 'operator_api'),
    t(5.6, 'auto_swap', ['northwind/US-SJC-BGP-E'], ['skyline/🇺🇸 US-East 01'], 'eligibility-set membership change'),
    t(2.3, 'auto_swap', ['atlas/美国 IEPL 02 | 专线'], [], 'eligibility-set membership change'),
    t(0.07, 'auto_swap', [], ['atlas/美国 HA.6x | 专线', 'atlas/美国 HB.6x | 专线'], 'eligibility-set membership change'),
  ];
  return {
    transitions: list,
    quarantine: { 'atlas/美国 HB.6x | 专线': iso(-50 * MIN) },
  };
}

// Deterministic stand-in for HRW: same IP → same pair, like the real renderer.
export function terminal(ip: string): TerminalAssignment {
  const inSubnet = ip.startsWith('10.8.11.');
  if (!inSubnet) {
    return {
      ip,
      subnet: SUBNET,
      in_subnet: false,
      pool_mode: 'auto',
      hint: 'ip is outside client_subnet — traffic from this source falls through to MATCH,us-pool, not a per-terminal fb group',
    };
  }
  const pool = nodes().filter((n) => n.in_pool);
  const h = ip.split('.').reduce((a, o) => (a * 31 + Number(o)) >>> 0, 7);
  const p = pool[h % pool.length];
  const s = pool[(h >>> 3) % pool.length] === p ? pool[(h + 1) % pool.length] : pool[(h >>> 3) % pool.length];
  return {
    ip,
    subnet: SUBNET,
    in_subnet: true,
    group_name: 'fb-' + ip,
    pool_mode: 'auto',
    active_node: p.alive ? 'primary' : s.alive ? 'secondary' : '',
    primary: { name: p.name, alive: p.alive, health: p },
    secondary: { name: s.name, alive: s.alive, health: s },
  };
}
