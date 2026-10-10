import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Badge,
  Button,
  Divider,
  Dropdown,
  Layout,
  Menu,
  Popover,
  Space,
  Switch,
  Tag,
  Tooltip,
} from '@arco-design/web-react';
import {
  IconDashboard,
  IconHistory,
  IconLink,
  IconMoonFill,
  IconPoweroff,
  IconRefresh,
  IconSearch,
  IconStorage,
  IconSunFill,
  IconDown,
} from '@arco-design/web-react/icon';
import * as api from './api';
import { ApiError } from './api';
import type { Snapshot } from './snapshot';
import { emptySnapshot } from './snapshot';
import { fmtClock, fmtTime } from './format';
import Logo from './components/Logo';
import Login from './Login';
import Overview from './pages/Overview';
import Nodes from './pages/Nodes';
import Subscriptions from './pages/Subscriptions';
import Terminal from './pages/Terminal';
import TransitionsPage from './pages/Transitions';

const REFRESH_MS = 10000;

const VIEWS = [
  { key: 'overview', title: '总览', desc: '数据通路、出口与池规模', icon: <IconDashboard /> },
  { key: 'nodes', title: '节点状态', desc: '候选节点打分与站点探针', icon: <IconStorage /> },
  { key: 'subs', title: '订阅管理', desc: '机场订阅与周期刷新', icon: <IconLink /> },
  { key: 'terminal', title: '终端查询', desc: '按员工 IP 查出口 —— 报「慢 / 打不开」时的第一入口', icon: <IconSearch /> },
  { key: 'transitions', title: '池变更审计', desc: '池成员变更历史与隔离名单', icon: <IconHistory /> },
] as const;
type View = (typeof VIEWS)[number]['key'];

function viewFromHash(): View {
  const k = location.hash.replace(/^#\/?/, '');
  return (VIEWS.find((v) => v.key === k)?.key ?? 'overview') as View;
}

const prefersDark = () => window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false;

export default function App() {
  const [connected, setConnected] = useState(false);
  const [authError, setAuthError] = useState('');
  const [view, setView] = useState<View>(viewFromHash);
  const [snap, setSnap] = useState<Snapshot>(emptySnapshot);
  const [auto, setAuto] = useState(true);
  const [loading, setLoading] = useState(false);
  const [collapsed, setCollapsed] = useState(false);
  // Theme follows the OS until toggled; deliberately not persisted (the
  // page keeps nothing in web storage, see api.ts).
  const [dark, setDark] = useState(prefersDark);
  const busy = useRef(false);

  useEffect(() => {
    if (dark) document.body.setAttribute('arco-theme', 'dark');
    else document.body.removeAttribute('arco-theme');
  }, [dark]);

  useEffect(() => {
    const onHash = () => setView(viewFromHash());
    window.addEventListener('hashchange', onHash);
    return () => window.removeEventListener('hashchange', onHash);
  }, []);

  const loadAll = useCallback(async (manual: boolean) => {
    if (busy.current) return;
    busy.current = true;
    if (manual) setLoading(true);

    // Each panel fails independently: a 503 from one endpoint (e.g. the
    // scorer disabled) must not blank the whole page.
    const slot = <T,>(p: Promise<T>) =>
      p.then((data) => ({ data }), (e: Error) => ({ err: e.message, status: (e as ApiError).status }));

    const [healthz, status, active, health, subs, interval, trans] = await Promise.all([
      api.getHealthz().catch(() => null),
      slot(api.getStatus()),
      slot(api.getActive()),
      slot(api.getNodesHealth()),
      slot(api.getSubscriptions()),
      slot(api.getRefreshInterval()),
      slot(api.getTransitions()),
    ]);

    busy.current = false;
    setLoading(false);

    if ('status' in status && status.status === 401) {
      // Token rotated under us — back to the login card.
      setConnected(false);
      setAuthError(status.err);
      return;
    }
    setSnap({ healthz, status, active, health, subs, interval, trans, updatedAt: new Date() });
  }, []);

  useEffect(() => {
    if (!connected || !auto) return;
    const t = setInterval(() => loadAll(false), REFRESH_MS);
    return () => clearInterval(t);
  }, [connected, auto, loadAll]);

  const onConnected = async () => {
    setAuthError('');
    setConnected(true);
    await loadAll(true);
  };

  const onDisconnect = () => {
    api.disconnect();
    setConnected(false);
    setSnap(emptySnapshot);
  };

  const go = (key: string) => {
    location.hash = '#/' + key;
  };

  if (!connected) {
    return <Login onConnected={onConnected} initialError={authError} dark={dark} onToggleTheme={() => setDark(!dark)} />;
  }

  const meta = VIEWS.find((v) => v.key === view)!;
  const st = snap.status.data;
  const ac = snap.active.data;
  const host = ac?.node?.hostname;
  const version = ac?.leap?.gateway_version;
  const deadCount = snap.health.data?.nodes.filter((n) => !n.alive).length;

  return (
    <Layout className="shell">
      <Layout.Sider
        className="sider"
        collapsible
        collapsed={collapsed}
        onCollapse={setCollapsed}
        width={224}
        breakpoint="lg"
      >
        <div className="brand">
          <Logo size={30} />
          {!collapsed && (
            <div className="brand-text">
              <div className="brand-name">Interchange</div>
              <div className="brand-cn">立交网关 · 控制台</div>
            </div>
          )}
        </div>
        <Menu selectedKeys={[view]} onClickMenuItem={go} className="nav">
          {VIEWS.map((v) => (
            <Menu.Item key={v.key}>
              {v.icon}
              {v.title}
              {v.key === 'nodes' && !!deadCount && !collapsed && (
                <Badge count={deadCount} className="nav-badge" maxCount={99} />
              )}
            </Menu.Item>
          ))}
        </Menu>
        {!collapsed && (
          <div className="sider-foot">
            <div>控制面 {version || '—'}</div>
            <div>{ac?.leap?.engine || 'mihomo'} {ac?.leap?.engine_version || ''}</div>
          </div>
        )}
      </Layout.Sider>

      <Layout>
        <Layout.Header className="header">
          <div className="header-title">
            <h1>{meta.title}</h1>
            <span className="header-desc">{meta.desc}</span>
          </div>
          <Space size={14} className="header-actions">
            <HealthPill snap={snap} />
            <Divider type="vertical" />
            <Tooltip content={`每 ${REFRESH_MS / 1000} 秒轮询一次全部接口`}>
              <span className="auto-refresh">
                <Switch size="small" checked={auto} onChange={setAuto} />
                自动刷新
              </span>
            </Tooltip>
            <span className="updated">
              {snap.updatedAt ? '更新于 ' + fmtClock(snap.updatedAt) : ''}
            </span>
            <Button icon={<IconRefresh />} loading={loading} onClick={() => loadAll(true)}>
              刷新
            </Button>
            <Tooltip content={dark ? '切换到浅色' : '切换到深色'}>
              <Button
                shape="circle"
                icon={dark ? <IconSunFill /> : <IconMoonFill />}
                onClick={() => setDark(!dark)}
                aria-label="切换主题"
              />
            </Tooltip>
            <Dropdown
              position="br"
              droplist={
                <Menu onClickMenuItem={(k) => k === 'logout' && onDisconnect()}>
                  <Menu.Item key="node" disabled>
                    {api.currentBase() || location.origin}
                  </Menu.Item>
                  <Menu.Item key="logout">
                    <IconPoweroff /> 断开连接
                  </Menu.Item>
                </Menu>
              }
            >
              <Button type="secondary" className="node-btn">
                <span className={'dot ' + (st?.engine_ok ? 'dot-ok' : 'dot-bad')} />
                {host || '节点'}
                <IconDown />
              </Button>
            </Dropdown>
          </Space>
        </Layout.Header>

        <Layout.Content className="content">
          {view === 'overview' && <Overview snap={snap} onNavigate={go} />}
          {view === 'nodes' && <Nodes snap={snap} />}
          {view === 'subs' && <Subscriptions snap={snap} reload={() => loadAll(true)} />}
          {view === 'terminal' && <Terminal snap={snap} />}
          {view === 'transitions' && <TransitionsPage snap={snap} />}
        </Layout.Content>
      </Layout>
    </Layout>
  );
}

function HealthPill({ snap }: { snap: Snapshot }) {
  const hz = snap.healthz;
  if (snap.updatedAt === undefined) return <Tag>healthz</Tag>;
  if (!hz) return <Tag color="red">healthz 不可达</Tag>;

  const checks = hz.body?.checks || [];
  const bad = checks.filter((c) => !c.ok);
  const ok = hz.httpStatus === 200;
  const label = ok
    ? hz.body?.scope === 'process-liveness-only'
      ? 'healthz 仅存活'
      : 'healthz 通过'
    : `healthz ${hz.httpStatus}`;

  return (
    <Popover
      position="br"
      title="/healthz 数据通路断言"
      content={
        <div className="hz-pop">
          {checks.length ? (
            checks.map((c) => (
              <div key={c.name} className="hz-row">
                <Badge status={c.ok ? 'success' : 'error'} />
                <span className="mono">{c.name}</span>
              </div>
            ))
          ) : (
            <div className="dim">{hz.body?.detail || '未返回断言明细'}</div>
          )}
          {hz.body?.asserted && <div className="dim hz-at">断言于 {fmtTime(hz.body.asserted)}</div>}
          {bad.length > 0 && <div className="err">失败：{bad.map((c) => c.name).join(', ')}</div>}
        </div>
      }
    >
      <Tag color={ok ? 'green' : 'red'} className="hz-tag">
        <Badge status={ok ? 'success' : 'error'} /> {label}
      </Tag>
    </Popover>
  );
}
