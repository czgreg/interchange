import { useState } from 'react';
import { Alert, Badge, Card, Descriptions, Empty, Grid, Input, Space, Tag } from '@arco-design/web-react';
import { IconSearch } from '@arco-design/web-react/icon';
import * as api from '../api';
import type { Snapshot } from '../snapshot';
import type { TerminalAssignment, TerminalNode } from '../types';
import { fmtMs, fmtRate, splitNode } from '../format';
import ProbeTags from '../components/ProbeTags';

const { Row, Col } = Grid;
const IP_RE = /^\d{1,3}(\.\d{1,3}){3}$/;

export default function Terminal({ snap }: { snap: Snapshot }) {
  const [ip, setIp] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [result, setResult] = useState<TerminalAssignment | null>(null);
  const subnet = snap.active.data?.node?.client_subnet;

  const lookup = async (raw: string) => {
    const v = raw.trim();
    if (!IP_RE.test(v)) {
      setErr('请输入 IPv4 地址，例如 10.8.11.42');
      return;
    }
    setBusy(true);
    setErr('');
    try {
      setResult(await api.getTerminal(v));
    } catch (e) {
      setResult(null);
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="page">
      <Card bordered={false} className="mb terminal-search">
        <div className="terminal-head">
          <div>
            <h3>按终端 IP 查出口</h3>
            <p className="dim">
              输入员工设备的飞连地址，直接看到它的 primary / secondary 出口及其健康数据，判断问题出在机场端还是终端端。
              {subnet && (
                <>
                  {' '}本节点客户端网段 <span className="mono">{subnet}</span>。
                </>
              )}
            </p>
          </div>
          <Input.Search
            size="large"
            searchButton="查询"
            prefix={<IconSearch />}
            placeholder={subnet ? subnet.replace(/\.0\/\d+$/, '.42') : '10.8.11.42'}
            value={ip}
            onChange={setIp}
            onSearch={lookup}
            loading={busy}
            allowClear
            className="terminal-input"
          />
        </div>
        {err && <Alert type="error" content={err} className="mt" />}
      </Card>

      {result && !result.in_subnet && (
        <Alert
          type="warning"
          title={`${result.ip} 不在本节点的 client_subnet（${result.subnet || '未配置'}）内`}
          content="这个 IP 没有专属的 fb-<ip> 组，流量会走 MATCH,us-pool 兜底。"
        />
      )}

      {result && result.in_subnet && (
        <>
          <Card bordered={false} className="mb">
            <Descriptions
              column={{ xs: 1, md: 3, xl: 5 }}
              layout="vertical"
              data={[
                { label: '终端 IP', value: <span className="mono big">{result.ip}</span> },
                { label: '所属子网', value: <span className="mono">{result.subnet || '—'}</span> },
                { label: 'mihomo 组名', value: <span className="mono">{result.group_name || '—'}</span> },
                { label: '池模式', value: result.pool_mode || '—' },
                {
                  label: '当前出口',
                  value: result.active_node ? (
                    <Tag color="green">{result.active_node.toUpperCase()}</Tag>
                  ) : (
                    <Tag color="red">两个备份都不可用</Tag>
                  ),
                },
              ]}
            />
            {result.hint && <Alert type={result.active_node ? 'info' : 'error'} content={result.hint} className="mt" />}
          </Card>
          <Row gutter={16}>
            <Col xs={24} lg={12}>
              <EgressCard label="PRIMARY" sub="HRW top-1" node={result.primary} active={result.active_node === 'primary'} />
            </Col>
            <Col xs={24} lg={12}>
              <EgressCard label="SECONDARY" sub="HRW top-2 · mihomo fallback" node={result.secondary} active={result.active_node === 'secondary'} />
            </Col>
          </Row>
        </>
      )}

      {!result && !err && (
        <Card bordered={false}>
          <Empty description="输入终端 IP 开始查询。HRW 分配是确定性的：同一 IP + 同一池成员永远同一答案。" />
        </Card>
      )}
    </div>
  );
}

function EgressCard({ label, sub, node, active }: { label: string; sub: string; node?: TerminalNode; active: boolean }) {
  const h = node?.health;
  const pv = h?.passive;
  return (
    <Card bordered={false} className={'egress-node mb' + (active ? ' is-active' : '')}>
      <div className="egress-node-head">
        <Space>
          <span className="role">{label}</span>
          <span className="dim">{sub}</span>
        </Space>
        {active && <Tag color="green">当前生效</Tag>}
      </div>
      {!node?.name ? (
        <Empty description="未分配" />
      ) : (
        <>
          <div className="egress-node-name">
            <Badge status={node.alive ? 'success' : 'error'} />
            <span>{splitNode(node.name).node}</span>
            <span className="node-sub">{splitNode(node.name).sub}</span>
          </div>
          <div className="metrics">
            <Metric label="存活" value={node.alive ? '活' : '死'} bad={!node.alive} />
            <Metric label="p50" value={fmtMs(h?.rtt_p50_ms)} />
            <Metric label="p95" value={fmtMs(h?.rtt_p95_ms)} warn={(h?.rtt_p95_ms || 0) > 800} />
            <Metric label="抖动" value={fmtMs(h?.jitter_ms)} />
            <Metric label="失败率" value={fmtRate(h?.fail_rate)} bad={(h?.fail_rate || 0) >= 0.5} />
          </div>
          <Descriptions
            column={1}
            size="small"
            data={[
              {
                label: '被动连接',
                value: pv
                  ? `${pv.active_conns} 活跃 · 失败率 ${fmtRate(pv.fail_rate)}（${pv.failed_window}/${pv.closed_window}）`
                  : '无连接',
              },
              { label: '站点探针', value: <ProbeTags probes={h?.probes} /> },
            ]}
          />
        </>
      )}
    </Card>
  );
}

function Metric({ label, value, bad, warn }: { label: string; value: string; bad?: boolean; warn?: boolean }) {
  return (
    <div className="metric">
      <div className="dim">{label}</div>
      <div className={'metric-v ' + (bad ? 'v-bad' : warn ? 'v-warn' : '')}>{value}</div>
    </div>
  );
}
