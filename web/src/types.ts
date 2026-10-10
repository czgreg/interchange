// Response shapes of the control-plane API. Source of truth is the Go
// handlers in internal/api/ and docs/api.md — every field the UI reads is
// optional unless the handler always sets it, because older nodes and
// scorer-less deployments omit whole blocks.

export interface HealthCheck {
  name: string;
  ok: boolean;
  detail?: string;
}

export interface Healthz {
  ok: boolean;
  scope?: string; // "process-liveness-only" when no asserter is configured
  detail?: string;
  asserted?: string;
  checks?: HealthCheck[];
}

export interface PoolSizing {
  k_target: number;
  k_current: number;
  t_active: number;
  tier1_count: number;
  supply_limited: boolean;
}

export interface PoolStability {
  window_hours: number;
  swap_count: number;
  rollback_count: number;
  unique_nodes_in: number;
  unique_nodes_out: number;
  oldest_at?: string;
}

export interface Capacity {
  sustained_max_users: number;
  degraded_max_users: number;
  measured_at?: string;
  measured_with?: string;
}

export interface Status {
  engine_ok: boolean;
  last_refresh?: string;
  node_count?: number;
  subscriptions?: number;
  pool_size?: number;
  pool_eligible?: number;
  pool_total?: number;
  pool_last_update?: string;
  sizing_mode?: string;
  pool_sizing?: PoolSizing;
  pool_stability_24h?: PoolStability;
  capacity?: Capacity;
}

export interface Egress {
  ip?: string;
  country?: string;
  region?: string;
  city?: string;
  org?: string;
  checked_at?: string;
}

export interface PoolMember {
  tag: string;
  delay_ms: number;
  last_check?: string;
  fail_rate: number; // -1 when the scorer is unavailable
}

export interface ActivePool {
  tag: string;
  pool_size: number;
  active: boolean;
  egress?: Egress;
  nodes?: PoolMember[];
}

export interface Active {
  node?: {
    hostname?: string;
    kernel?: string;
    os?: string;
    uptime_seconds?: number;
    interfaces?: { name: string; ipv4?: string }[];
    client_subnet?: string;
    egress_iface?: string;
  };
  feilian?: {
    tun0_active?: boolean;
    vpn_active?: boolean;
    proxy_active?: boolean;
    sentry_active?: boolean;
  };
  leap?: {
    gateway_version?: string;
    engine?: string;
    engine_version?: string;
    services?: Record<string, string>;
    subscriptions_count?: number;
    nodes_parsed?: number;
    last_refresh?: string;
  };
  active_proxy?: {
    reachable?: boolean;
    active_group?: string;
    pool_filter?: string;
    pool_mode?: string;
    last_swap_at?: string;
    pool_size?: number;
    pools?: ActivePool[];
  };
}

export interface ProbeResult {
  ok: boolean;
  status_code?: number;
  cf_mitigated?: boolean;
  latency_ms?: number;
  last_checked_at?: string;
  last_error?: string;
}

export interface Passive {
  active_conns: number;
  closed_window: number;
  failed_window: number;
  fail_rate: number;
  sample_window_sec: number;
}

export interface NodeHealth {
  name: string;
  sub?: string;
  alive: boolean;
  rtt_last_ms?: number;
  rtt_p50_ms?: number;
  rtt_p95_ms?: number;
  jitter_ms?: number;
  fail_rate?: number;
  probe_count?: number;
  throughput_bps?: number;
  qualified: boolean;
  in_pool: boolean;
  strikes?: number;
  ok_rounds?: number;
  reason?: string;
  probes?: Record<string, ProbeResult>;
  passive?: Passive;
}

export interface NodesHealth {
  qualified: number;
  total: number;
  last_pool_update?: string;
  last_scored_at?: string;
  thresholds?: {
    MaxRTTP50Ms?: number;
    MaxRTTP95Ms?: number;
    MaxJitterMs?: number;
    MaxFailRate?: number;
    Probes?: { Name: string; URL: string }[];
  };
  pool_mode?: string;
  nodes: NodeHealth[];
}

export interface Subscription {
  name: string;
  url: string; // masked: token=c85b***0f02 — never write this back
  enabled: boolean;
  format?: string;
  user_agent?: string;
  nodes_count?: number;
  last_refresh?: string;
}

export interface RefreshInterval {
  seconds: number;
}

export interface Transition {
  at: string;
  type: string; // auto_swap | emergency_evict | emergency_swap | rollback
  added?: string[];
  removed?: string[];
  pool_before?: string[];
  pool_after?: string[];
  reason?: string;
  source?: string; // scorer | operator_api | operator_yaml
}

export interface Transitions {
  transitions: Transition[];
  quarantine?: Record<string, string>;
}

export interface TerminalNode {
  name: string;
  alive: boolean;
  health?: NodeHealth;
}

export interface TerminalAssignment {
  ip: string;
  subnet: string;
  in_subnet: boolean;
  primary?: TerminalNode;
  secondary?: TerminalNode;
  active_node?: '' | 'primary' | 'secondary';
  group_name?: string;
  pool_mode: string;
  hint?: string;
}
