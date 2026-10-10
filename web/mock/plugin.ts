// Vite plugin that answers /healthz and /api/* from mock/data.ts, for both
// `vite --mode mock` (dev) and `vite preview --mode mock` (built bundle).
// Subscription writes mutate in-memory state and sleep like a real refresh,
// so loading states and confirm flows can be exercised without a node.
import type { IncomingMessage, ServerResponse } from 'node:http';
import type { Connect, Plugin } from 'vite';
import * as data from './data';

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

function readBody(req: IncomingMessage): Promise<any> {
  return new Promise((resolve) => {
    let raw = '';
    req.on('data', (c) => (raw += c));
    req.on('end', () => {
      try {
        resolve(raw ? JSON.parse(raw) : {});
      } catch {
        resolve({});
      }
    });
  });
}

function send(res: ServerResponse, status: number, body?: unknown) {
  if (body === undefined) {
    res.statusCode = status;
    res.end();
    return;
  }
  res.statusCode = status;
  res.setHeader('Content-Type', 'application/json');
  res.end(JSON.stringify(body));
}

function handler(): Connect.NextHandleFunction {
  let subs = data.initialSubs();
  let interval = 0;
  let refreshing = false;

  return async (req, res, next) => {
    const url = new URL(req.url || '/', 'http://mock');
    const path = url.pathname;
    const method = req.method || 'GET';
    if (path !== '/healthz' && !path.startsWith('/api/')) return next();

    if (path === '/healthz') return send(res, 200, data.healthz());
    if (path === '/api/status') return send(res, 200, data.status(subs));
    if (path === '/api/proxies/active') return send(res, 200, data.active(subs));
    if (path === '/api/nodes/health') return send(res, 200, data.nodesHealth());
    if (path === '/api/pool/transitions') return send(res, 200, data.transitions());
    if (path === '/api/pool/terminal') {
      const ip = url.searchParams.get('ip');
      if (!ip) return send(res, 400, { error: 'missing ?ip= query parameter' });
      await sleep(250);
      return send(res, 200, data.terminal(ip));
    }

    if (path === '/api/subscribe/refresh-interval') {
      if (method === 'PUT') interval = Number((await readBody(req)).seconds) || 0;
      return send(res, 200, { seconds: interval });
    }
    if (path === '/api/subscribe/refresh' && method === 'POST') {
      if (refreshing) return send(res, 409, { error: 'refresh already in progress' });
      refreshing = true;
      await sleep(2500);
      refreshing = false;
      return send(res, 200, { ok: true });
    }

    if (path === '/api/subscriptions') {
      if (method === 'POST') {
        const b = await readBody(req);
        if (subs.some((s) => s.name === b.name)) return send(res, 409, { error: 'subscription already exists' });
        await sleep(1500);
        subs = [
          ...subs,
          {
            name: b.name,
            url: String(b.url).replace(/token=(\w{4})\w+(\w{4})/, 'token=$1***$2'),
            enabled: true,
            format: b.format || 'auto',
            user_agent: b.user_agent || undefined,
            nodes_count: 12,
            last_refresh: new Date().toISOString(),
          },
        ];
        return send(res, 201, subs);
      }
      return send(res, 200, subs);
    }
    const m = path.match(/^\/api\/subscriptions\/(.+)$/);
    if (m) {
      const name = decodeURIComponent(m[1]);
      if (!subs.some((s) => s.name === name)) return send(res, 404, { error: 'subscription not found' });
      await sleep(1500);
      if (method === 'DELETE') {
        subs = subs.filter((s) => s.name !== name);
        return send(res, 204);
      }
      if (method === 'PUT') {
        const b = await readBody(req);
        subs = subs.map((s) => (s.name === name ? { ...s, user_agent: b.user_agent || undefined } : s));
        return send(res, 200, subs);
      }
    }
    return send(res, 404, { error: 'mock: no handler for ' + method + ' ' + path });
  };
}

export function mockApi(): Plugin {
  return {
    name: 'interchange-mock-api',
    configureServer(server) {
      server.middlewares.use(handler());
    },
    configurePreviewServer(server) {
      server.middlewares.use(handler());
    },
  };
}
