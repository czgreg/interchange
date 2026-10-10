/* Control-plane API client.
 *
 * 设计约束（来自 docs/api.md，改动前先读那份文档）：
 *  1. GET /api/subscriptions 返回的 url 是**脱敏**的（token=c85b***0f02）。
 *     PUT 又要求 url 非空。所以编辑表单绝不回填脱敏 URL —— 那会把
 *     星号写进生产配置。要改 URL 必须粘贴完整新 URL（见 pages/Subscriptions）。
 *  2. 订阅的增删改、以及 POST /api/subscribe/refresh 都会重载 mihomo
 *     （约 3–5 秒中断），所以这些操作一律先确认。请求本身要慢得多：
 *     每个订阅最多两次 fetch（UA 回退探测），实测冷刷新 17–30s。确认
 *     文案必须说出这个时长 —— 操作者以为卡死而掐断连接，正是
 *     2026-09-24 那次配置落盘但 reload 没跑的起因。
 *  3. token 只放在这个模块的内存变量里。不进 web storage —— 这个页面
 *     可能开在共享的运维机上，而 token 能读到带凭据的订阅 URL。
 *
 * Endpoint paths are written as single-quoted literals on purpose:
 * internal/api/ui_test.go extracts them and asserts each one is registered
 * in api.go, so a renamed route fails `go test` instead of 404-ing here.
 */
import type {
  Active,
  Healthz,
  NodesHealth,
  RefreshInterval,
  Status,
  Subscription,
  TerminalAssignment,
  Transitions,
} from './types';

let base = '';
let token = '';

export function connect(nextBase: string, nextToken: string) {
  base = nextBase.trim().replace(/\/+$/, '');
  token = nextToken;
}

export function disconnect() {
  base = '';
  token = '';
}

export function currentBase() {
  return base;
}

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

async function request<T>(path: string, opts: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {};
  if (token) headers.Authorization = 'Bearer ' + token;
  if (opts.body) headers['Content-Type'] = 'application/json';

  const res = await fetch(base + path, { ...opts, headers });
  if (res.status === 401) throw new ApiError(401, '401 未授权 —— token 不正确');
  if (res.status === 204) return null as T;

  const text = await res.text();
  if (!res.ok) {
    // Errors are either http.Error plain text or writeJSON {"error": "..."}.
    let msg = text.split('\n')[0].trim() || res.statusText;
    try {
      const body = JSON.parse(text);
      if (body && typeof body.error === 'string') msg = body.error;
    } catch {
      /* plain text */
    }
    throw new ApiError(res.status, `${res.status} ${msg}`);
  }
  if (!text) return null as T;
  return JSON.parse(text) as T;
}

const json = (body: unknown): RequestInit => ({ body: JSON.stringify(body) });

/** /healthz is unauthenticated and answers 503 with a body when an assertion fails. */
export async function getHealthz(): Promise<{ httpStatus: number; body: Healthz | null }> {
  const res = await fetch(base + '/healthz');
  let body: Healthz | null = null;
  try {
    body = await res.json();
  } catch {
    /* non-JSON: older binary */
  }
  return { httpStatus: res.status, body };
}

export const getStatus = () => request<Status>('/api/status');
export const getActive = () => request<Active>('/api/proxies/active');
export const getNodesHealth = () => request<NodesHealth>('/api/nodes/health');
export const getSubscriptions = () => request<Subscription[]>('/api/subscriptions');
export const getRefreshInterval = () => request<RefreshInterval>('/api/subscribe/refresh-interval');
export const getTransitions = (limit = 100) =>
  request<Transitions>('/api/pool/transitions?limit=' + limit);
export const getTerminal = (ip: string) =>
  request<TerminalAssignment>('/api/pool/terminal?ip=' + encodeURIComponent(ip));

export const addSubscription = (sub: { name: string; url: string; format: string; user_agent: string }) =>
  request<Subscription[]>('/api/subscriptions', { method: 'POST', ...json(sub) });

export const updateSubscription = (name: string, sub: { url: string; user_agent: string }) =>
  request<unknown>('/api/subscriptions/' + encodeURIComponent(name), { method: 'PUT', ...json(sub) });

export const deleteSubscription = (name: string) =>
  request<null>('/api/subscriptions/' + encodeURIComponent(name), { method: 'DELETE' });

export const refreshSubscriptions = () =>
  request<{ ok: boolean }>('/api/subscribe/refresh', { method: 'POST' });

export const setRefreshInterval = (seconds: number) =>
  request<RefreshInterval>('/api/subscribe/refresh-interval', { method: 'PUT', ...json({ seconds }) });
