import type { ReactNode } from 'react';
import {
  Alert,
  Badge,
  Card,
  Descriptions,
  Empty,
  Grid,
  Progress,
  Table,
  Tag,
  Tooltip,
} from '@arco-design/web-react';
import {
  IconCheckCircleFill,
  IconCloseCircleFill,
  IconThunderbolt,
  IconStorage,
  IconSwap,
  IconLink,
  IconExclamationCircle,
} from '@arco-design/web-react/icon';
import type { Snapshot } from '../snapshot';
import type { PoolMember } from '../types';
import { ago, fmtMs, fmtRate, fmtTime, fmtUptime, splitNode } from '../format';

const { Row, Col } = Grid;

function StatCard(props: {
  icon: ReactNode;
  tone: 'blue' | 'green' | 'red' | 'orange' | 'purple' | 'cyan';
  title: string;
  value: ReactNode;
  suffix?: ReactNode;
  note?: ReactNode;
  onClick?: () => void;
}) {
  return (
    <Card className={'stat' + (props.onClick ? ' stat-link' : '')} bordered={false} onClick={props.onClick}>
      <div className={'stat-icon tone-' + props.tone}>{props.icon}</div>
      <div className="stat-body">
        <div className="stat-title">{props.title}</div>
        <div className="stat-value">
          {props.value}
          {props.suffix && <span className="stat-suffix">{props.suffix}</span>}
        </div>
        <div className="stat-note">{props.note}</div>
      </div>
    </Card>
  );
}

export default function Overview({ snap, onNavigate }: { snap: Snapshot; onNavigate: (k: string) => void }) {
  const st = snap.status.data;
  const ac = snap.active.data;
  const health = snap.health.data;
  const hz = snap.healthz?.body;
  const pool = ac?.active_proxy?.pools?.find((p) => p.active) || ac?.active_proxy?.pools?.[0];
  const eg = pool?.egress;
  const stab = st?.pool_stability_24h;
  const dead = health ? health.nodes.filter((n) => !n.alive).length : undefined;

  return (
    <div className="page">
      {snap.status.err && <Alert type="error" title="读取 /api/status 失败" content={snap.status.err} className="mb" />}

      <Row gutter={16} className="stats">
        <Col xs={24} sm={12} xl={8} xxl={4}>
          <StatCard
            icon={<IconThunderbolt />}
            tone={st?.engine_ok ? 'green' : 'red'}
            title="数据面"
            value={st ? (st.engine_ok ? '正常' : '异常') : '—'}
            note={st?.engine_ok ? 'mihomo clash-api 可达' : 'clash-api 不可达'}
          />
        </Col>
        <Col xs={24} sm={12} xl={8} xxl={4}>
          <StatCard
            icon={<IconStorage />}
            tone="blue"
            title="在池节点"
            value={st?.pool_size ?? '—'}
            suffix={st?.pool_total != null ? ` / ${st.pool_total}` : undefined}
            note={`活着且可达 ${st?.pool_eligible ?? '—'} · 候选 ${st?.pool_total ?? '—'}`}
            onClick={() => onNavigate('nodes')}
          />
        </Col>
        <Col xs={24} sm={12} xl={8} xxl={4}>
          <StatCard
            icon={<IconLink />}
            tone="purple"
            title="解析到节点"
            value={st?.node_count ?? '—'}
            note={`${st?.subscriptions ?? 0} 个订阅 · 刷新于 ${ago(st?.last_refresh) || '—'}`}
            onClick={() => onNavigate('subs')}
          />
        </Col>
        <Col xs={24} sm={12} xl={8} xxl={4}>
          <StatCard
            icon={<IconSwap />}
            tone="orange"
            title="24h 池变更"
            value={stab?.swap_count ?? '—'}
            suffix=" 次"
            note={`回滚 ${stab?.rollback_count ?? 0} · 最近 ${ago(st?.pool_last_update) || '—'}`}
            onClick={() => onNavigate('transitions')}
          />
        </Col>
        <Col xs={24} sm={12} xl={8} xxl={4}>
          <StatCard
            icon={<IconExclamationCircle />}
            tone={dead ? 'red' : 'green'}
            title="探针不通"
            value={dead ?? '—'}
            suffix={health ? ` / ${health.nodes.length}` : undefined}
            note={health ? `合格 ${health.qualified} / ${health.total}` : snap.health.err ? '评分接口不可用' : ''}
            onClick={() => onNavigate('nodes')}
          />
        </Col>
        <Col xs={24} sm={12} xl={8} xxl={4}>
          <StatCard
            icon={<IconCheckCircleFill />}
            tone={snap.healthz?.httpStatus === 200 ? 'cyan' : 'red'}
            title="数据通路断言"
            value={hz?.checks ? `${hz.checks.filter((c) => c.ok).length}` : snap.healthz ? (hz?.ok ? '通过' : '失败') : '—'}
            suffix={hz?.checks ? ` / ${hz.checks.length}` : undefined}
            note={hz?.asserted ? '断言于 ' + ago(hz.asserted) : hz?.scope === 'process-liveness-only' ? '未配置 TPROXY 断言' : ''}
          />
        </Col>
      </Row>

      <Row gutter={16}>
        <Col xs={24} xl={15}>
          <Card title="数据通路断言" extra={<span className="dim">GET /healthz · 未鉴权</span>} bordered={false} className="mb">
            {hz?.checks?.length ? (
              <div className="checks">
                {hz.checks.map((c) => (
                  <div key={c.name} className={'check ' + (c.ok ? 'is-ok' : 'is-bad')}>
                    {c.ok ? <IconCheckCircleFill className="check-icon" /> : <IconCloseCircleFill className="check-icon" />}
                    <div>
                      <div className="mono check-name">{c.name}</div>
                      <div className="dim">{c.detail}</div>
                    </div>
                  </div>
                ))}
              </div>
            ) : (
              <Empty description={hz?.detail || (snap.healthz === null ? '/healthz 不可达' : '无断言数据')} />
            )}
          </Card>

          <Card
            title="在池节点"
            extra={
              <span className="dim">
                {ac?.active_proxy?.active_group || '—'} · 最近变更 {ago(ac?.active_proxy?.last_swap_at) || '—'}
              </span>
            }
            bordered={false}
            className="mb"
          >
            <Table
              rowKey="tag"
              size="small"
              pagination={false}
              border={false}
              data={pool?.nodes || []}
              noDataElement={<Empty description="无池成员数据" />}
              columns={[
                {
                  title: '节点',
                  dataIndex: 'tag',
                  render: (t: string) => {
                    const { sub, node } = splitNode(t);
                    return (
                      <span>
                        <span className="node-name">{node}</span>
                        {sub && <span className="node-sub">{sub}</span>}
                      </span>
                    );
                  },
                },
                {
                  title: 'url-test 延迟',
                  dataIndex: 'delay_ms',
                  width: 220,
                  render: (v: number) => <DelayBar ms={v} />,
                },
                {
                  title: '探针失败率',
                  dataIndex: 'fail_rate',
                  width: 120,
                  align: 'right',
                  render: (v: number, r: PoolMember) => (
                    <span className={v >= 0.5 ? 'v-bad' : v > 0.1 ? 'v-warn' : ''}>{fmtRate(r.fail_rate)}</span>
                  ),
                },
              ]}
            />
          </Card>
        </Col>

        <Col xs={24} xl={9}>
          <Card title="当前出口" bordered={false} className="mb egress-card">
            {eg?.ip ? (
              <>
                <div className="egress-ip mono">{eg.ip}</div>
                <div className="egress-geo">
                  {[eg.country, eg.region, eg.city].filter(Boolean).join(' · ')}
                </div>
                <div className="dim">{eg.org}</div>
                <Descriptions
                  column={1}
                  size="small"
                  className="egress-desc"
                  data={[
                    { label: '出站组', value: <span className="mono">{ac?.active_proxy?.active_group || '—'}</span> },
                    { label: '池模式', value: ac?.active_proxy?.pool_mode || '—' },
                    { label: '节点筛选', value: <span className="mono">{ac?.active_proxy?.pool_filter || '—'}</span> },
                    { label: '探测于', value: ago(eg.checked_at) || '—' },
                  ]}
                />
              </>
            ) : (
              <Empty description="出口信息不可用" />
            )}
          </Card>

          <Card title="池规模" bordered={false} className="mb">
            {st?.pool_total ? (
              <>
                <div className="pool-progress">
                  <Progress
                    type="circle"
                    size="large"
                    percent={Math.round(((st.pool_size || 0) / st.pool_total) * 100)}
                    formatText={() => (
                      <span className="pool-ring">
                        <b>{st.pool_size}</b>
                        <span>/ {st.pool_total}</span>
                      </span>
                    )}
                  />
                  <Descriptions
                    column={1}
                    size="small"
                    data={[
                      { label: '选池模式', value: <Tag size="small">{st.sizing_mode || '—'}</Tag> },
                      { label: '在池', value: st.pool_size ?? '—' },
                      {
                        label: (
                          <Tooltip content="仅通过 liveness 门；不扣除准入质量门，所以常大于在池数">
                            <span className="dotted">活着且可达</span>
                          </Tooltip>
                        ),
                        value: st.pool_eligible ?? '—',
                      },
                      {
                        label: '供给受限',
                        value: st.pool_sizing?.supply_limited ? <Tag color="red" size="small">是</Tag> : '否',
                      },
                    ]}
                  />
                </div>
                {stab && (
                  <div className="stab">
                    24h 内移入 <b>{stab.unique_nodes_in}</b> 个、移出 <b>{stab.unique_nodes_out}</b> 个节点，
                    共 <b>{stab.swap_count}</b> 次变更。频繁变更说明打分在抖动。
                  </div>
                )}
              </>
            ) : (
              <Empty description="未启用 NodeScorer" />
            )}
          </Card>
        </Col>
      </Row>

      <Row gutter={16}>
        <Col xs={24} xl={15}>
          <Card title="节点与服务" bordered={false} className="mb">
            {ac ? (
              <Descriptions
                column={{ xs: 1, sm: 1, md: 2, lg: 2, xl: 2, xxl: 3 }}
                size="medium"
                labelStyle={{ width: 120 }}
                data={[
                  { label: '主机名', value: ac.node?.hostname || '—' },
                  { label: '系统', value: ac.node?.os || '—' },
                  { label: '内核', value: <span className="mono">{ac.node?.kernel || '—'}</span> },
                  { label: '运行时长', value: fmtUptime(ac.node?.uptime_seconds) },
                  { label: '控制面版本', value: <span className="mono">{ac.leap?.gateway_version || '—'}</span> },
                  { label: '数据面', value: `${ac.leap?.engine || '—'} ${ac.leap?.engine_version || ''}` },
                  { label: '客户端网段', value: <span className="mono">{ac.node?.client_subnet || '—'}</span> },
                  { label: '出口网卡', value: <span className="mono">{ac.node?.egress_iface || '—'}</span> },
                  {
                    label: '飞连 tun0',
                    value: <Badge status={ac.feilian?.tun0_active ? 'success' : 'error'} text={ac.feilian?.tun0_active ? 'active' : 'inactive'} />,
                  },
                  ...Object.entries(ac.leap?.services || {}).map(([name, state]) => ({
                    label: name,
                    value: <Badge status={state === 'active' ? 'success' : 'error'} text={state} />,
                  })),
                ]}
              />
            ) : (
              <Empty description={snap.active.err || '无数据'} />
            )}
          </Card>
        </Col>
        <Col xs={24} xl={9}>
          <Card title="容量" extra={<span className="dim">离线压测上限，非实时测量</span>} bordered={false} className="mb">
            {st?.capacity ? (
              <>
                <div className="capacity">
                  <div>
                    <div className="cap-num">{st.capacity.sustained_max_users}</div>
                    <div className="dim">稳定承载（人）</div>
                  </div>
                  <div>
                    <div className="cap-num cap-warn">{st.capacity.degraded_max_users}</div>
                    <div className="dim">降级承载（人）</div>
                  </div>
                </div>
                <div className="dim cap-meta">
                  测于 {st.capacity.measured_at || '—'} · {st.capacity.measured_with || ''}
                </div>
              </>
            ) : (
              <Empty description="未配置 capacity。跑 scripts/stress.sh 后写入 gateway.yaml。" />
            )}
          </Card>
          <Card title="时间线" bordered={false} className="mb">
            <Descriptions
              column={1}
              size="small"
              data={[
                { label: '订阅最近刷新', value: `${fmtTime(st?.last_refresh)}  ${ago(st?.last_refresh)}` },
                { label: '池最近变更', value: `${fmtTime(st?.pool_last_update)}  ${ago(st?.pool_last_update)}` },
                { label: '最近打分', value: `${fmtTime(health?.last_scored_at)}  ${ago(health?.last_scored_at)}` },
              ]}
            />
          </Card>
        </Col>
      </Row>
    </div>
  );
}

// Horizontal bar scaled to 1s; red beyond the 800ms p95 observation threshold.
function DelayBar({ ms }: { ms: number }) {
  if (!ms) return <span className="dim">—</span>;
  const pct = Math.min(100, (ms / 1000) * 100);
  const color = ms > 800 ? 'rgb(var(--red-6))' : ms > 400 ? 'rgb(var(--orange-6))' : 'rgb(var(--green-6))';
  return (
    <div className="delay">
      <Progress percent={pct} showText={false} color={color} size="small" />
      <span className="delay-num">{fmtMs(ms)}</span>
    </div>
  );
}
