/* Interchange 立交网关 — 控制面前端。原生 JS，无构建步骤、无依赖。
 *
 * 设计约束（来自 docs/api.md，改动前先读那份文档）：
 *  1. GET /api/subscriptions 返回的 url 是**脱敏**的（token=c85b***0f02）。
 *     PUT 又要求 url 非空。所以编辑表单绝不回填脱敏 URL —— 那会把
 *     星号写进生产配置。要改 URL 必须粘贴完整新 URL。
 *  2. 订阅的增删改、以及 POST /api/subscribe/refresh 都会重载 mihomo
 *     （约 3–5 秒中断），所以这些操作一律先确认。请求本身要慢得多：
 *     每个订阅最多两次 fetch（UA 回退探测），实测冷刷新 17–30s。确认
 *     文案必须说出这个时长 —— 操作者以为卡死而掐断连接，正是
 *     2026-09-24 那次配置落盘但 reload 没跑的起因。
 *  3. token 只放在内存。不进 localStorage —— 这个页面可能开在共享的
 *     运维机上，而 token 能读到带凭据的订阅 URL。
 */
'use strict';

const state = {
  base: '',
  token: '',
  timer: null,
  view: 'overview',
  nodes: [],           // 最近一次 /api/nodes/health 的 nodes[]
  subs: [],            // 最近一次 /api/subscriptions
  editing: null,       // 正在编辑的订阅名；null = 新增
  clientSubnet: '',    // 从 /api/pool/terminal 或 status 推断，用于占位符
  busy: false,
};

const REFRESH_MS = 10000;
const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

/* ---------------------------------------------------------------- utils */

function esc(v) {
  return String(v == null ? '' : v).replace(/[&<>"']/g,
    (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function toast(msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast' + (kind ? ' toast-' + kind : '');
  el.textContent = msg;
  $('#toasts').append(el);
  setTimeout(() => el.remove(), kind === 'bad' ? 7000 : 3800);
}

function fmtMs(v) {
  if (v == null || v === 0) return '—';
  return v >= 1000 ? (v / 1000).toFixed(2) + 's' : Math.round(v) + 'ms';
}

function fmtRate(v) {
  if (v == null || v < 0) return '—';
  return (v * 100).toFixed(0) + '%';
}

function fmtTime(s) {
  if (!s) return '—';
  const d = new Date(s);
  if (isNaN(d)) return s;
  const pad = (n) => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ` +
         `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function ago(s) {
  if (!s) return '';
  const secs = Math.floor((Date.now() - new Date(s).getTime()) / 1000);
  if (isNaN(secs) || secs < 0) return '';
  if (secs < 60) return secs + ' 秒前';
  if (secs < 3600) return Math.floor(secs / 60) + ' 分钟前';
  if (secs < 86400) return Math.floor(secs / 3600) + ' 小时前';
  return Math.floor(secs / 86400) + ' 天前';
}

/* ------------------------------------------------------------------ api */

async function api(path, opts = {}) {
  const headers = Object.assign({}, opts.headers);
  if (state.token) headers.Authorization = 'Bearer ' + state.token;
  if (opts.body) headers['Content-Type'] = 'application/json';

  const res = await fetch(state.base + path, Object.assign({}, opts, { headers }));
  if (res.status === 401) throw new Error('401 未授权 —— token 不正确');
  if (res.status === 204) return null;

  const text = await res.text();
  if (!res.ok) {
    // 后端错误多为 http.Error 的纯文本，取首行即可。
    throw new Error(`${res.status} ${text.split('\n')[0].trim() || res.statusText}`);
  }
  if (!text) return null;
  try { return JSON.parse(text); } catch { return text; }
}

/* ------------------------------------------------------------- 渲染：总览 */

function renderStatus(st, active, health) {
  const cards = [];
  const push = (label, value, note, cls) => cards.push(
    `<div class="card"><div class="card-label">${esc(label)}</div>
     <div class="card-value ${cls || ''}">${value}</div>
     <div class="card-note">${esc(note || '')}</div></div>`);

  const engineOK = st && st.engine_ok;
  push('数据面', engineOK ? '正常' : '异常',
       engineOK ? 'mihomo clash-api 可达' : 'clash-api 不可达',
       engineOK ? 'is-ok' : 'is-bad');

  if (st) {
    push('在池节点', st.pool_size != null ? st.pool_size : '—',
         `合格 ${st.pool_eligible != null ? st.pool_eligible : '—'} / 候选 ${st.pool_total != null ? st.pool_total : '—'}`);
    push('解析到节点', st.node_count != null ? st.node_count : '—',
         `${st.subscriptions != null ? st.subscriptions : 0} 个订阅`);
    push('池最近变更', ago(st.pool_last_update) || '—', fmtTime(st.pool_last_update));
    push('订阅最近刷新', ago(st.last_refresh) || '—', fmtTime(st.last_refresh));
  }

  if (health && health.nodes) {
    const dead = health.nodes.filter((n) => !n.alive).length;
    push('探针不通', dead, `共 ${health.nodes.length} 个候选`, dead ? 'is-bad' : 'is-ok');
  }

  $('#statusCards').innerHTML = cards.join('');

  // ---- 数据面明细
  const rows = [];
  const kv = (k, v) => rows.push(`<div class="kv"><span class="kv-k">${esc(k)}</span><span class="kv-v">${v}</span></div>`);
  if (active) {
    const leap = active.leap || {};
    const fl = active.feilian || {};
    const ap = active.active_proxy || {};
    const svc = leap.services || {};
    kv('主机名', esc((active.node || {}).hostname || '—'));
    kv('系统', esc((active.node || {}).os || '—'));
    kv('控制面版本', `<span class="mono">${esc(leap.gateway_version || '—')}</span>`);
    kv('数据面', `${esc(leap.engine || '—')} <span class="v-dim">${esc(leap.engine_version || '')}</span>`);
    kv('飞连 tun0', fl.tun0_active ? '<span class="dot dot-ok"></span>active'
                                  : '<span class="dot dot-bad"></span>inactive');
    Object.keys(svc).forEach((name) => {
      const ok = svc[name] === 'active';
      kv(name, `<span class="dot ${ok ? 'dot-ok' : 'dot-bad'}"></span>${esc(svc[name])}`);
    });
    kv('当前出站组', `<span class="mono">${esc(ap.active_group || '—')}</span>`);
    kv('池模式', esc(ap.pool_mode || '—'));
    kv('节点筛选正则', `<span class="mono">${esc(ap.pool_filter || '—')}</span>`);
    const eg = ((ap.pools || [])[0] || {}).egress;
    if (eg) kv('出口 IP', `<span class="mono">${esc(eg.ip || '—')}</span> ${esc(eg.country || '')}`);
    $('#dpHint').textContent = ap.last_swap_at ? '池最近变更 ' + ago(ap.last_swap_at) : '';
  }
  $('#dataplane').innerHTML = rows.join('') || '<p class="hint">无数据</p>';

  // ---- 容量
  const cap = st && st.capacity;
  if (cap) {
    $('#capacity').innerHTML = [
      ['稳定承载', (cap.sustained_max_users ?? '—') + ' 人'],
      ['降级承载', (cap.degraded_max_users ?? '—') + ' 人'],
      ['测量时间', cap.measured_at || '—'],
      ['测量条件', cap.measured_with || '—'],
    ].map(([k, v]) => `<div class="kv"><span class="kv-k">${esc(k)}</span><span class="kv-v">${esc(v)}</span></div>`).join('');
  } else {
    $('#capacity').innerHTML = '<p class="hint">未配置 capacity。跑 scripts/stress.sh 后写入 gateway.yaml。</p>';
  }
}

/* ------------------------------------------------------------- 渲染：节点 */

function renderNodes() {
  const q = $('#nodeFilter').value.trim().toLowerCase();
  const poolOnly = $('#poolOnly').checked;
  let list = state.nodes;
  if (poolOnly) list = list.filter((n) => n.in_pool);
  if (q) list = list.filter((n) => (n.name + ' ' + (n.sub || '')).toLowerCase().includes(q));

  // 排序：在池优先，其次 p95 升序（0/缺失排最后）。
  list = list.slice().sort((a, b) => {
    if (!!a.in_pool !== !!b.in_pool) return a.in_pool ? -1 : 1;
    const pa = a.rtt_p95_ms || Infinity, pb = b.rtt_p95_ms || Infinity;
    return pa - pb;
  });

  const tbody = $('#nodesTable tbody');
  if (!list.length) {
    tbody.innerHTML = '<tr><td colspan="12" class="empty">没有匹配的节点</td></tr>';
    return;
  }

  tbody.innerHTML = list.map((n) => {
    const probes = Object.entries(n.probes || {}).map(([name, p]) => {
      const ok = p && p.ok;
      const cf = p && p.cf_mitigated;
      const cls = ok ? 'pill-ok' : 'pill-bad';
      const title = [name, p && p.status_code ? 'status ' + p.status_code : '',
                     cf ? 'CF challenge' : '', p && p.last_error ? p.last_error : '']
                    .filter(Boolean).join(' · ');
      return `<span class="pill ${cls}" title="${esc(title)}">${esc(name.replace(/-api$/, ''))}</span>`;
    }).join(' ');

    const failCls = n.fail_rate >= 0.5 ? 'v-bad' : (n.fail_rate > 0.1 ? 'v-warn' : '');
    const p95Cls = n.rtt_p95_ms > 800 ? 'v-warn' : '';

    return `<tr>
      <td class="mono">${esc(n.name)}</td>
      <td class="v-dim">${esc(n.sub || '—')}</td>
      <td><span class="dot ${n.alive ? 'dot-ok' : 'dot-bad'}"></span>${n.alive ? '活' : '死'}</td>
      <td class="num">${fmtMs(n.rtt_p50_ms)}</td>
      <td class="num ${p95Cls}">${fmtMs(n.rtt_p95_ms)}</td>
      <td class="num">${fmtMs(n.jitter_ms)}</td>
      <td class="num ${failCls}">${fmtRate(n.fail_rate)}</td>
      <td class="num">${n.probe_count != null ? n.probe_count : '—'}</td>
      <td>${n.in_pool ? '<span class="pill pill-ok">在池</span>' : '<span class="pill">—</span>'}</td>
      <td>${n.qualified ? '<span class="pill pill-ok">合格</span>' : '<span class="pill pill-warn">不合格</span>'}</td>
      <td>${probes || '<span class="v-dim">—</span>'}</td>
      <td class="wrap v-dim">${esc(n.reason || '')}</td>
    </tr>`;
  }).join('');
}

/* ------------------------------------------------------------- 渲染：订阅 */

function renderSubs() {
  const tbody = $('#subsTable tbody');
  if (!state.subs.length) {
    tbody.innerHTML = '<tr><td colspan="7" class="empty">还没有订阅。点「新增订阅」添加一个。</td></tr>';
    return;
  }
  tbody.innerHTML = state.subs.map((s) => `<tr>
    <td>${esc(s.name)}</td>
    <td class="mono wrap v-dim">${esc(s.url)}</td>
    <td>${esc(s.format || 'auto')}</td>
    <td class="v-dim">${esc(s.user_agent || '默认')}</td>
    <td class="num">${s.nodes_count != null ? s.nodes_count : 0}</td>
    <td>${s.enabled ? '<span class="pill pill-ok">启用</span>' : '<span class="pill">停用</span>'}</td>
    <td>
      <button class="btn btn-sm" data-edit="${esc(s.name)}" type="button">编辑</button>
      <button class="btn btn-sm btn-danger" data-del="${esc(s.name)}" type="button">删除</button>
    </td>
  </tr>`).join('');
}

/* --------------------------------------------------------- 渲染：池变更 */

function renderTransitions(data) {
  const q = (data && data.quarantine) || {};
  const names = Object.keys(q);
  $('#quarantineBox').innerHTML = names.length
    ? `<div class="warnbox"><strong>隔离中</strong>：${names.map((n) =>
        `${esc(n)} <span class="v-dim">(至 ${esc(fmtTime(q[n]))})</span>`).join('、')}</div>`
    : '';

  const list = (data && data.transitions) || [];
  const tbody = $('#transTable tbody');
  if (!list.length) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty">暂无池变更记录 —— 稳定是好事。</td></tr>';
    return;
  }
  tbody.innerHTML = list.slice().reverse().map((t) => {
    const kindCls = t.type === 'rollback' ? 'pill-warn'
                  : (t.type && t.type.startsWith('emergency') ? 'pill-bad' : '');
    return `<tr>
      <td>${esc(fmtTime(t.at))}<div class="hint">${esc(ago(t.at))}</div></td>
      <td><span class="pill ${kindCls}">${esc(t.type || '—')}</span></td>
      <td class="v-dim">${esc(t.source || '—')}</td>
      <td class="mono wrap">${(t.added || []).map(esc).join('<br>') || '<span class="v-dim">—</span>'}</td>
      <td class="mono wrap">${(t.removed || []).map(esc).join('<br>') || '<span class="v-dim">—</span>'}</td>
      <td class="wrap v-dim">${esc(t.reason || '')}</td>
    </tr>`;
  }).join('');
}

/* --------------------------------------------------------- 渲染：终端查询 */

function egressCard(role, label, obj, isActive) {
  if (!obj || !obj.name) {
    return `<div class="egress"><div class="role">${esc(label)}</div>
            <h3 class="v-dim">未分配</h3></div>`;
  }
  const h = obj.health || {};
  const probes = Object.entries(h.probes || {}).map(([name, p]) =>
    `<span class="pill ${p && p.ok ? 'pill-ok' : 'pill-bad'}" title="${esc(name)}">${esc(name.replace(/-api$/, ''))}</span>`
  ).join(' ');
  const pv = h.passive;
  return `<div class="egress ${isActive ? 'is-active' : ''}">
    <div class="role">${esc(label)}${isActive ? ' · 当前生效' : ''}</div>
    <h3 class="mono">${esc(obj.name)}</h3>
    <div class="kv"><span class="kv-k">存活</span><span class="kv-v">
      <span class="dot ${obj.alive ? 'dot-ok' : 'dot-bad'}"></span>${obj.alive ? '活' : '死'}</span></div>
    <div class="kv"><span class="kv-k">p50 / p95</span><span class="kv-v">${fmtMs(h.rtt_p50_ms)} / ${fmtMs(h.rtt_p95_ms)}</span></div>
    <div class="kv"><span class="kv-k">抖动 / 失败率</span><span class="kv-v">${fmtMs(h.jitter_ms)} / ${fmtRate(h.fail_rate)}</span></div>
    ${pv ? `<div class="kv"><span class="kv-k">被动失败率</span><span class="kv-v">${fmtRate(pv.fail_rate)}
      <span class="v-dim">(${pv.active_conns} 活跃连接)</span></span></div>` : ''}
    <div class="probe-list">${probes || '<span class="v-dim">无站点探针</span>'}</div>
  </div>`;
}

function renderTerminal(d) {
  if (!d.in_subnet) {
    $('#terminalResult').innerHTML = `<div class="warnbox" style="margin-top:14px">
      <strong>${esc(d.ip)}</strong> 不在本节点的 client_subnet
      (<span class="mono">${esc(d.subnet || '未配置')}</span>) 内。
      这个 IP 没有专属的 fb-&lt;ip&gt; 组，会走 <span class="mono">MATCH,us-pool</span> 兜底。
    </div>`;
    return;
  }
  const active = d.active_node;
  $('#terminalResult').innerHTML = `
    <div class="kv-grid" style="margin-top:14px">
      <div class="kv"><span class="kv-k">终端 IP</span><span class="kv-v mono">${esc(d.ip)}</span></div>
      <div class="kv"><span class="kv-k">所属子网</span><span class="kv-v mono">${esc(d.subnet || '—')}</span></div>
      <div class="kv"><span class="kv-k">mihomo 组名</span><span class="kv-v mono">${esc(d.group_name || '—')}</span></div>
      <div class="kv"><span class="kv-k">池模式</span><span class="kv-v">${esc(d.pool_mode || '—')}</span></div>
      <div class="kv"><span class="kv-k">当前出口</span><span class="kv-v">${
        active ? esc(active) : '<span class="v-bad">两个备份都不可用</span>'}</span></div>
    </div>
    <div class="egress-pair">
      ${egressCard('primary', 'PRIMARY（HRW top-1）', d.primary, active === 'primary')}
      ${egressCard('secondary', 'SECONDARY（HRW top-2）', d.secondary, active === 'secondary')}
    </div>`;
}

/* --------------------------------------------------------------- 数据加载 */

async function loadAll(manual) {
  if (state.busy) return;
  state.busy = true;
  if (manual) $('#refreshBtn').classList.add('spin');

  // healthz 未鉴权，单独取，失败不影响其他面板。
  try {
    const res = await fetch(state.base + '/healthz');
    const pill = $('#healthPill');
    pill.className = 'pill ' + (res.ok ? 'pill-ok' : 'pill-bad');
    pill.textContent = res.ok ? 'healthz 通过' : 'healthz ' + res.status;
    let body = null;
    try { body = await res.json(); } catch { /* 非 JSON 忽略 */ }
    if (body && body.checks) {
      const bad = Object.entries(body.checks).filter(([, v]) => v && v.ok === false).map(([k]) => k);
      pill.title = bad.length ? '失败断言：' + bad.join(', ') : '全部数据通路断言通过';
    }
  } catch {
    const pill = $('#healthPill');
    pill.className = 'pill pill-bad';
    pill.textContent = 'healthz 不可达';
  }

  // 各面板独立失败：一个 503（如 scorer 未启用）不该让整页空白。
  const [st, active, health, subs, interval, trans] = await Promise.all([
    api('/api/status').catch((e) => ({ __err: e.message })),
    api('/api/proxies/active').catch(() => null),
    api('/api/nodes/health').catch((e) => ({ __err: e.message })),
    api('/api/subscriptions').catch((e) => ({ __err: e.message })),
    api('/api/subscribe/refresh-interval').catch(() => null),
    api('/api/pool/transitions?limit=50').catch((e) => ({ __err: e.message })),
  ]);

  if (st && st.__err) {
    toast('读取状态失败：' + st.__err, 'bad');
    $('#nodeLine').textContent = '连接异常';
  } else {
    const host = (active && active.node && active.node.hostname) || '';
    const ver = (active && active.leap && active.leap.gateway_version) || '';
    $('#nodeLine').textContent =
      `${state.base || '同源'}${host ? ' · ' + host : ''}${ver ? ' · ' + ver : ''} · 更新于 ${fmtTime(new Date().toISOString())}`;
    renderStatus(st, active, health && !health.__err ? health : null);
  }

  if (health && health.__err) {
    $('#nodesHint').textContent = '无法读取节点评分：' + health.__err +
      '（node_qualify.enabled=false 时该接口返回 503）';
    $('#nodesTable tbody').innerHTML = '<tr><td colspan="12" class="empty">无节点评分数据</td></tr>';
  } else if (health) {
    state.nodes = health.nodes || [];
    const th = health.thresholds || {};
    $('#nodesHint').textContent =
      `合格 ${health.qualified}/${health.total} · 最近打分 ${fmtTime(health.last_scored_at)} · ` +
      `观察阈值 p50<${th.MaxRTTP50Ms}ms p95<${th.MaxRTTP95Ms}ms 抖动<${th.MaxJitterMs}ms 失败率<${th.MaxFailRate}`;
    renderNodes();
  }

  if (subs && subs.__err) {
    toast('读取订阅失败：' + subs.__err, 'bad');
  } else if (Array.isArray(subs)) {
    state.subs = subs;
    renderSubs();
  }

  if (interval && typeof interval.seconds === 'number') {
    if (document.activeElement !== $('#intervalInput')) $('#intervalInput').value = interval.seconds;
    $('#intervalHint').textContent = interval.seconds === 0
      ? '当前关闭周期刷新（推荐）' : `当前每 ${interval.seconds} 秒刷新一次`;
  }

  if (trans && !trans.__err) renderTransitions(trans);

  state.busy = false;
  $('#refreshBtn').classList.remove('spin');
}

function startTimer() {
  if (state.timer) clearInterval(state.timer);
  if ($('#autoRefresh').checked) state.timer = setInterval(() => loadAll(false), REFRESH_MS);
}

/* ------------------------------------------------------------------ 事件 */

// 登录
$('#authForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  state.base = $('#baseInput').value.trim().replace(/\/+$/, '');
  state.token = $('#tokenInput').value;
  $('#authErr').hidden = true;
  try {
    await api('/api/status');           // 探一次，确认地址 + token 可用
    $('#authModal').classList.remove('modal-open');
    $('#tokenInput').value = '';        // 不留在 DOM 里
    await loadAll(true);
    startTimer();
  } catch (err) {
    $('#authErr').textContent = err.message;
    $('#authErr').hidden = false;
  }
});

// 标签页
$$('.tab').forEach((tab) => tab.addEventListener('click', () => {
  $$('.tab').forEach((t) => t.classList.toggle('tab-active', t === tab));
  state.view = tab.dataset.view;
  $$('.view').forEach((v) => v.classList.toggle('view-active', v.dataset.view === state.view));
}));

$('#refreshBtn').addEventListener('click', () => loadAll(true));
$('#autoRefresh').addEventListener('change', startTimer);
$('#nodeFilter').addEventListener('input', renderNodes);
$('#poolOnly').addEventListener('change', renderNodes);

// 订阅：新增
$('#addSubBtn').addEventListener('click', () => {
  state.editing = null;
  $('#subTitle').textContent = '新增订阅';
  $('#subName').value = '';
  $('#subName').disabled = false;
  $('#subUrl').value = '';
  $('#subUrl').placeholder = 'https://provider/subscribe?token=...';
  $('#subFormat').value = 'auto';
  $('#subFormat').disabled = false;
  $('#subUA').value = '';
  $('#subUrlHint').hidden = true;
  $('#subErr').hidden = true;
  $('#subModal').classList.add('modal-open');
  $('#subName').focus();
});

// 订阅：编辑 / 删除（事件委托）
$('#subsTable').addEventListener('click', async (e) => {
  const editName = e.target.dataset.edit;
  const delName = e.target.dataset.del;

  if (editName) {
    const sub = state.subs.find((s) => s.name === editName);
    state.editing = editName;
    $('#subTitle').textContent = '编辑订阅：' + editName;
    $('#subName').value = editName;
    $('#subName').disabled = true;          // PUT 用路径里的 name，不可改
    // 关键：不回填脱敏 URL。回填会把 token=c85b***0f02 写进生产。
    $('#subUrl').value = '';
    $('#subUrl').placeholder = '粘贴完整的新 URL（含 token）';
    $('#subFormat').value = sub && sub.format ? sub.format : 'auto';
    $('#subFormat').disabled = true;        // PUT 不接受 format
    $('#subUA').value = (sub && sub.user_agent) || '';
    $('#subUrlHint').hidden = false;
    $('#subErr').hidden = true;
    $('#subModal').classList.add('modal-open');
    $('#subUrl').focus();
    return;
  }

  if (delName) {
    if (!confirm(`删除订阅「${delName}」？\n\n` +
                 '这会立即重新拉取剩余订阅并重载 mihomo（约 3–5 秒中断）。\n' +
                 '拉取可能需要 30 秒以上，期间请勿关闭或刷新页面。')) return;
    e.target.disabled = true;
    try {
      await api('/api/subscriptions/' + encodeURIComponent(delName), { method: 'DELETE' });
      toast(`已删除 ${delName}`, 'ok');
      await loadAll(true);
    } catch (err) {
      toast('删除失败：' + err.message, 'bad');
      e.target.disabled = false;
    }
  }
});

$('#subCancel').addEventListener('click', () => $('#subModal').classList.remove('modal-open'));

// 订阅：提交
$('#subForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const url = $('#subUrl').value.trim();
  if (/\*\*\*/.test(url)) {
    $('#subErr').textContent = '这个 URL 看起来是脱敏后的（含 ***）。请粘贴完整的原始 URL。';
    $('#subErr').hidden = false;
    return;
  }
  const ua = $('#subUA').value.trim();
  $('#subErr').hidden = true;
  $('#subSubmit').disabled = true;
  try {
    if (state.editing) {
      await api('/api/subscriptions/' + encodeURIComponent(state.editing),
                { method: 'PUT', body: JSON.stringify({ url, user_agent: ua }) });
      toast(`已更新 ${state.editing}`, 'ok');
    } else {
      await api('/api/subscriptions', {
        method: 'POST',
        body: JSON.stringify({
          name: $('#subName').value.trim(),
          url,
          format: $('#subFormat').value,
          user_agent: ua,
        }),
      });
      toast('已新增订阅', 'ok');
    }
    $('#subModal').classList.remove('modal-open');
    await loadAll(true);
  } catch (err) {
    $('#subErr').textContent = err.message;
    $('#subErr').hidden = false;
  } finally {
    $('#subSubmit').disabled = false;
  }
});

// 立即拉取全部
$('#refreshSubsBtn').addEventListener('click', async (e) => {
  if (!confirm('立即拉取所有订阅并重载 mihomo？\n\n' +
              '重载时约 3–5 秒连接中断。拉取本身可能需要 30 秒以上\n' +
              '（每个订阅最多两次 fetch），期间请勿关闭或刷新页面。')) return;
  e.target.disabled = true;
  e.target.textContent = '拉取中…';
  try {
    await api('/api/subscribe/refresh', { method: 'POST' });
    toast('订阅已刷新', 'ok');
    await loadAll(true);
  } catch (err) {
    toast('刷新失败：' + err.message, 'bad');
  } finally {
    e.target.disabled = false;
    e.target.textContent = '立即拉取全部';
  }
});

// 周期刷新间隔
$('#intervalForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const seconds = parseInt($('#intervalInput').value, 10);
  if (isNaN(seconds) || seconds < 0) return;
  try {
    await api('/api/subscribe/refresh-interval',
              { method: 'PUT', body: JSON.stringify({ seconds }) });
    toast(seconds === 0 ? '已关闭周期刷新' : `已设为每 ${seconds} 秒`, 'ok');
    await loadAll(false);
  } catch (err) {
    toast('保存失败：' + err.message, 'bad');
  }
});

// 终端查询
$('#terminalForm').addEventListener('submit', async (e) => {
  e.preventDefault();
  const ip = $('#terminalIp').value.trim();
  $('#terminalResult').innerHTML = '<p class="hint">查询中…</p>';
  try {
    renderTerminal(await api('/api/pool/terminal?ip=' + encodeURIComponent(ip)));
  } catch (err) {
    $('#terminalResult').innerHTML = `<p class="err">${esc(err.message)}</p>`;
  }
});

/* ------------------------------------------------------------------ 启动 */

(function init() {
  // 同源默认：UI 由控制面自己伺服时，base 留空即可。
  const sameOrigin = location.protocol.startsWith('http');
  $('#baseInput').value = sameOrigin ? location.origin : '';
  $('#baseInput').focus();
})();
