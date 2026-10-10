import { useMemo, useState } from 'react';
import {
  Alert,
  Badge,
  Card,
  Checkbox,
  Empty,
  Input,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
} from '@arco-design/web-react';
import type { TableColumnProps } from '@arco-design/web-react';
import { IconSearch } from '@arco-design/web-react/icon';
import type { Snapshot } from '../snapshot';
import type { NodeHealth } from '../types';
import { ago, fmtMs, fmtRate, fmtTime, probeLabel, splitNode } from '../format';
import ProbeTags from '../components/ProbeTags';

const num = (v?: number) => (v ? v : Infinity); // 0 = no measurement → sort last

export default function Nodes({ snap }: { snap: Snapshot }) {
  const [q, setQ] = useState('');
  const [poolOnly, setPoolOnly] = useState(false);
  const [sub, setSub] = useState<string | undefined>();
  const health = snap.health.data;
  const nodes = health?.nodes || [];
  const th = health?.thresholds || {};

  const subs = useMemo(() => Array.from(new Set(nodes.map((n) => n.sub || '').filter(Boolean))).sort(), [nodes]);

  // Site-probe pass rate across all candidates — answers "is OpenAI
  // rejecting our egress IPs" without scanning every row.
  const probeSummary = useMemo(() => {
    const m: Record<string, { pass: number; total: number }> = {};
    nodes.forEach((n) =>
      Object.entries(n.probes || {}).forEach(([name, p]) => {
        m[name] = m[name] || { pass: 0, total: 0 };
        m[name].total++;
        if (p.ok) m[name].pass++;
      }),
    );
    return Object.entries(m).sort(([a], [b]) => a.localeCompare(b));
  }, [nodes]);

  const list = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return nodes
      .filter((n) => !poolOnly || n.in_pool)
      .filter((n) => !sub || n.sub === sub)
      .filter((n) => !needle || (n.name + ' ' + (n.sub || '')).toLowerCase().includes(needle))
      .slice()
      // In-pool first, then p95 ascending — the order an operator scans in.
      .sort((a, b) => (a.in_pool !== b.in_pool ? (a.in_pool ? -1 : 1) : num(a.rtt_p95_ms) - num(b.rtt_p95_ms)));
  }, [nodes, q, poolOnly, sub]);

  const columns: TableColumnProps<NodeHealth>[] = [
    {
      title: '节点',
      dataIndex: 'name',
      fixed: 'left',
      width: 270,
      render: (_, n) => (
        <div className="node-cell">
          <Badge status={n.alive ? 'success' : 'error'} />
          <div>
            <div className="node-name">{splitNode(n.name).node}</div>
            <Space size={4} className="node-meta">
              <span className="node-sub">{n.sub || '—'}</span>
              {n.in_pool ? <Tag color="arcoblue" size="small">在池</Tag> : <Tag size="small">不在池</Tag>}
              {n.qualified ? (
                <Tag color="green" size="small">合格</Tag>
              ) : (
                <Tooltip content={n.reason || '无原因'}>
                  <Tag color="orangered" size="small" className="dotted-tag">不合格</Tag>
                </Tooltip>
              )}
            </Space>
          </div>
        </div>
      ),
    },
    {
      title: 'p50',
      dataIndex: 'rtt_p50_ms',
      align: 'right',
      width: 96,
      sorter: (a, b) => num(a.rtt_p50_ms) - num(b.rtt_p50_ms),
      render: (v: number) => <span className="num">{fmtMs(v)}</span>,
    },
    {
      title: 'p95',
      dataIndex: 'rtt_p95_ms',
      align: 'right',
      width: 96,
      sorter: (a, b) => num(a.rtt_p95_ms) - num(b.rtt_p95_ms),
      render: (v: number) => (
        <span className={'num ' + (v > (th.MaxRTTP95Ms || 800) ? 'v-warn' : '')}>{fmtMs(v)}</span>
      ),
    },
    {
      title: '抖动',
      dataIndex: 'jitter_ms',
      align: 'right',
      width: 96,
      sorter: (a, b) => num(a.jitter_ms) - num(b.jitter_ms),
      render: (v: number) => (
        <span className={'num ' + (v > (th.MaxJitterMs || 300) ? 'v-warn' : '')}>{fmtMs(v)}</span>
      ),
    },
    {
      title: '失败率',
      dataIndex: 'fail_rate',
      align: 'right',
      width: 104,
      sorter: (a, b) => (a.fail_rate ?? 0) - (b.fail_rate ?? 0),
      render: (v: number) => (
        <span className={'num ' + (v >= 0.5 ? 'v-bad' : v > 0.1 ? 'v-warn' : '')}>{fmtRate(v)}</span>
      ),
    },
    {
      title: '连接数',
      align: 'right',
      width: 104,
      sorter: (a, b) => (a.passive?.active_conns ?? 0) - (b.passive?.active_conns ?? 0),
      render: (_, n) => <span className="num">{n.passive ? n.passive.active_conns : '—'}</span>,
    },
    {
      title: '站点探针',
      width: 300,
      render: (_, n) => <ProbeTags probes={n.probes} />,
    },
  ];

  if (snap.health.err) {
    return (
      <div className="page">
        <Alert
          type="warning"
          title="无法读取节点评分"
          content={`${snap.health.err}（node_qualify.enabled=false 时该接口返回 503）`}
        />
      </div>
    );
  }

  return (
    <div className="page">
      <Card bordered={false} className="mb summary-card">
        <div className="summary">
          <div className="summary-item">
            <span className="dim">合格</span>
            <b>
              {health?.qualified ?? '—'} <span className="dim">/ {health?.total ?? '—'}</span>
            </b>
          </div>
          <div className="summary-item">
            <span className="dim">最近打分</span>
            <b>{ago(health?.last_scored_at) || '—'}</b>
            <span className="dim small">{fmtTime(health?.last_scored_at)}</span>
          </div>
          <div className="summary-item">
            <span className="dim">观察阈值（不再 gate 池成员）</span>
            <Space size={4} wrap>
              <Tag size="small">p50 &lt; {th.MaxRTTP50Ms ?? '—'}ms</Tag>
              <Tag size="small">p95 &lt; {th.MaxRTTP95Ms ?? '—'}ms</Tag>
              <Tag size="small">抖动 &lt; {th.MaxJitterMs ?? '—'}ms</Tag>
              <Tag size="small">失败率 &lt; {th.MaxFailRate ?? '—'}</Tag>
            </Space>
          </div>
          <div className="summary-item">
            <span className="dim">站点探针通过率</span>
            <Space size={6} wrap>
              {probeSummary.length ? (
                probeSummary.map(([name, s]) => (
                  <Tooltip key={name} content={th.Probes?.find((p) => p.Name === name)?.URL || name}>
                    <Tag size="small" color={s.pass === s.total ? 'green' : s.pass === 0 ? 'red' : 'orange'}>
                      {probeLabel(name)} {s.pass}/{s.total}
                    </Tag>
                  </Tooltip>
                ))
              ) : (
                <span className="dim">—</span>
              )}
            </Space>
          </div>
        </div>
      </Card>

      <Card
        bordered={false}
        title="节点评分"
        extra={
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<IconSearch />}
              placeholder="按节点名 / 订阅筛选"
              value={q}
              onChange={setQ}
              style={{ width: 240 }}
            />
            <Select
              allowClear
              placeholder="全部订阅"
              value={sub}
              onChange={setSub}
              style={{ width: 160 }}
              options={subs.map((s) => ({ label: s, value: s }))}
            />
            <Checkbox checked={poolOnly} onChange={setPoolOnly}>
              只看在池
            </Checkbox>
          </Space>
        }
      >
        <Table
          rowKey="name"
          columns={columns}
          data={list}
          pagination={false}
          scroll={{ x: 1100 }}
          border={{ wrapper: false, cell: false }}
          noDataElement={<Empty description="没有匹配的节点" />}
          expandedRowRender={(n) => <NodeDetail n={n} />}
        />
      </Card>
    </div>
  );
}

function NodeDetail({ n }: { n: NodeHealth }) {
  const probes = Object.entries(n.probes || {}).sort(([a], [b]) => a.localeCompare(b));
  const facts: [string, string | number][] = [
    ['最近一次 RTT', fmtMs(n.rtt_last_ms)],
    ['探针样本数', n.probe_count ?? '—'],
    ['连续不合格轮数', n.strikes ?? '—'],
    ['连续合格轮数', n.ok_rounds ?? '—'],
    ['吞吐', n.throughput_bps != null ? `${n.throughput_bps} B/s` : '—'],
    [
      '被动失败率',
      n.passive
        ? `${fmtRate(n.passive.fail_rate)}（${n.passive.failed_window}/${n.passive.closed_window}，${n.passive.sample_window_sec / 60} 分钟窗口）`
        : '无连接',
    ],
  ];
  return (
    <div className="node-detail">
      {n.reason && <Alert type="warning" content={'不合格原因：' + n.reason} className="mb" />}
      <div className="facts">
        {facts.map(([k, v]) => (
          <div key={k} className="fact">
            <div className="dim">{k}</div>
            <div>{v}</div>
          </div>
        ))}
      </div>
      {probes.length > 0 && (
        <table className="probe-table">
          <thead>
            <tr>
              <th>探针</th>
              <th>结果</th>
              <th>HTTP</th>
              <th>耗时</th>
              <th>CF 挑战</th>
              <th>检查时间</th>
              <th>错误</th>
            </tr>
          </thead>
          <tbody>
            {probes.map(([name, p]) => (
              <tr key={name}>
                <td className="mono">{name}</td>
                <td>{p.ok ? <Tag color="green" size="small">通过</Tag> : <Tag color="red" size="small">失败</Tag>}</td>
                <td>{p.status_code || '—'}</td>
                <td>{fmtMs(p.latency_ms)}</td>
                <td>{p.cf_mitigated ? <Tag color="orange" size="small">命中</Tag> : '—'}</td>
                <td>{fmtTime(p.last_checked_at)}</td>
                <td className="dim">{p.last_error || ''}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
