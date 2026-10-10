import { useMemo } from 'react';
import { Alert, Card, Empty, Grid, Space, Statistic, Table, Tag } from '@arco-design/web-react';
import type { Snapshot } from '../snapshot';
import type { Transition } from '../types';
import { ago, fmtTime, splitNode } from '../format';

const { Row, Col } = Grid;

const TYPE_META: Record<string, { label: string; color: string }> = {
  auto_swap: { label: '自动换池', color: 'arcoblue' },
  emergency_evict: { label: '紧急剔除', color: 'red' },
  emergency_swap: { label: '紧急顶替', color: 'orangered' },
  rollback: { label: '回滚', color: 'orange' },
};
const SOURCE_LABEL: Record<string, string> = {
  scorer: '系统打分',
  operator_api: '操作员 API',
  operator_yaml: '操作员 yaml',
};

function NodeTags({ list, color }: { list?: string[]; color: string }) {
  if (!list?.length) return <span className="dim">—</span>;
  return (
    <Space size={4} wrap>
      {list.map((n) => (
        <Tag key={n} size="small" color={color} title={n}>
          {splitNode(n).node}
        </Tag>
      ))}
    </Space>
  );
}

export default function TransitionsPage({ snap }: { snap: Snapshot }) {
  const data = snap.trans.data;
  const stab = snap.status.data?.pool_stability_24h;
  const list = useMemo(() => (data?.transitions || []).slice().reverse(), [data]);
  const quarantine = Object.entries(data?.quarantine || {});
  const types = Array.from(new Set(list.map((t) => t.type)));

  if (snap.trans.err) {
    return (
      <div className="page">
        <Alert type="warning" title="无法读取池变更" content={`${snap.trans.err}（scorer 未启用时返回 503）`} />
      </div>
    );
  }

  return (
    <div className="page">
      <Row gutter={16} className="mb">
        {[
          { title: '24h 池变更', value: stab?.swap_count, suffix: '次' },
          { title: '24h 回滚', value: stab?.rollback_count, suffix: '次' },
          { title: '24h 移入节点', value: stab?.unique_nodes_in, suffix: '个' },
          { title: '24h 移出节点', value: stab?.unique_nodes_out, suffix: '个' },
        ].map((s) => (
          <Col key={s.title} xs={12} lg={6}>
            <Card bordered={false} className="mini-stat">
              <Statistic title={s.title} value={s.value ?? '—'} suffix={s.suffix} groupSeparator />
            </Card>
          </Col>
        ))}
      </Row>

      {quarantine.length > 0 && (
        <Alert
          type="warning"
          className="mb"
          title={`隔离中 ${quarantine.length} 个节点`}
          content={
            <Space size={8} wrap>
              {quarantine.map(([name, until]) => (
                <span key={name}>
                  <span className="mono">{name}</span> <span className="dim">至 {fmtTime(until)}</span>
                </span>
              ))}
            </Space>
          }
        />
      )}

      <Card bordered={false} title="池组成变更" extra={<span className="dim">频繁变更说明打分在抖动 · 最近 {list.length} 条</span>}>
        <Table
          rowKey={(t: Transition) => t.at + t.type}
          data={list}
          border={{ wrapper: false, cell: false }}
          pagination={{ pageSize: 15, sizeCanChange: false, hideOnSinglePage: true }}
          noDataElement={<Empty description="暂无池变更记录 —— 稳定是好事。" />}
          expandedRowRender={(t) =>
            t.pool_after?.length ? (
              <div className="pool-diff">
                <div className="dim">变更后池（{t.pool_after.length}）</div>
                <NodeTags list={t.pool_after} color="gray" />
              </div>
            ) : null
          }
          columns={[
            {
              title: '时间',
              dataIndex: 'at',
              width: 190,
              render: (v: string) => (
                <div>
                  <div>{fmtTime(v)}</div>
                  <div className="dim small">{ago(v)}</div>
                </div>
              ),
            },
            {
              title: '类型',
              dataIndex: 'type',
              width: 120,
              filters: types.map((t) => ({ text: TYPE_META[t]?.label || t, value: t })),
              onFilter: (value, row) => row.type === value,
              render: (v: string) => <Tag color={TYPE_META[v]?.color}>{TYPE_META[v]?.label || v}</Tag>,
            },
            {
              title: '来源',
              dataIndex: 'source',
              width: 110,
              render: (v: string) => <span className="dim">{SOURCE_LABEL[v] || v || '—'}</span>,
            },
            { title: '加入', dataIndex: 'added', render: (v: string[]) => <NodeTags list={v} color="green" /> },
            { title: '移出', dataIndex: 'removed', render: (v: string[]) => <NodeTags list={v} color="red" /> },
            { title: '原因', dataIndex: 'reason', width: 260, render: (v: string) => <span className="dim">{v}</span> },
          ]}
        />
      </Card>
    </div>
  );
}
