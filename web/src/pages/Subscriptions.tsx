import { useEffect, useState } from 'react';
import {
  Alert,
  AutoComplete,
  Badge,
  Button,
  Card,
  Empty,
  Form,
  Input,
  InputNumber,
  Message,
  Modal,
  Select,
  Space,
  Table,
  Tag,
} from '@arco-design/web-react';
import { IconDelete, IconEdit, IconPlus, IconSync } from '@arco-design/web-react/icon';
import * as api from '../api';
import { ApiError } from '../api';
import type { Snapshot } from '../snapshot';
import type { Subscription } from '../types';
import { ago, fmtSeconds, fmtTime } from '../format';

// Common values from docs/api.md "user_agent 常用值".
const UA_OPTIONS = ['mihomo/1.18.4', 'clash.meta', 'ClashforWindows/0.20.39', 'sing-box/1.10.7', 'clash-verge/1.7.7'];
const FORMATS = ['auto', 'clash', 'singbox', 'uri', 'sip008'];

// Shown in every confirm: an operator who thinks the page hung and closes it
// mid-refresh is how 2026-09-24 left new config on disk without a reload.
const SLOW_NOTE = '拉取可能需要 30 秒以上（每个订阅最多两次 fetch），期间请勿关闭或刷新页面。';

interface FormValues {
  name: string;
  url: string;
  format: string;
  user_agent?: string;
}

export default function Subscriptions({ snap, reload }: { snap: Snapshot; reload: () => Promise<void> }) {
  const subs = snap.subs.data || [];
  const [editing, setEditing] = useState<Subscription | null>(null);
  const [creating, setCreating] = useState(false);
  const [refreshing, setRefreshing] = useState(false);

  const removeSub = (s: Subscription) =>
    Modal.confirm({
      title: `删除订阅「${s.name}」？`,
      content: (
        <div>
          这会立即重新拉取剩余订阅并重载 mihomo（约 3–5 秒中断）。
          <br />
          {SLOW_NOTE}
        </div>
      ),
      okButtonProps: { status: 'danger' },
      okText: '删除并重载',
      onOk: async () => {
        try {
          await api.deleteSubscription(s.name);
          Message.success(`已删除 ${s.name}`);
          await reload();
        } catch (e) {
          Message.error('删除失败：' + (e as Error).message);
        }
      },
    });

  const refreshAll = () =>
    Modal.confirm({
      title: '立即拉取所有订阅并重载 mihomo？',
      content: (
        <div>
          重载时约 3–5 秒连接中断。
          <br />
          {SLOW_NOTE}
        </div>
      ),
      okText: '拉取并重载',
      onOk: async () => {
        setRefreshing(true);
        try {
          await api.refreshSubscriptions();
          Message.success('订阅已刷新');
          await reload();
        } catch (e) {
          if ((e as ApiError).status === 409) Message.warning('已有一次刷新在进行中，等它完成即可，无需重复触发。');
          else Message.error('刷新失败：' + (e as Error).message);
        } finally {
          setRefreshing(false);
        }
      },
    });

  return (
    <div className="page">
      <Alert
        type="warning"
        className="mb"
        title="改动会重载数据面"
        content="新增 / 修改 / 删除订阅，以及「立即拉取」都会触发 mihomo 重新加载，造成约 3–5 秒的连接中断。每个节点的订阅列表是各自独立的，不要拿另一台节点的列表覆盖过来。"
      />

      {snap.subs.err && <Alert type="error" className="mb" title="读取订阅失败" content={snap.subs.err} />}

      <Card
        bordered={false}
        className="mb"
        title="订阅"
        extra={
          <Space>
            <Button icon={<IconSync />} loading={refreshing} onClick={refreshAll}>
              {refreshing ? '拉取中…' : '立即拉取全部'}
            </Button>
            <Button type="primary" icon={<IconPlus />} onClick={() => setCreating(true)}>
              新增订阅
            </Button>
          </Space>
        }
      >
        <Table
          rowKey="name"
          data={subs}
          pagination={false}
          border={{ wrapper: false, cell: false }}
          scroll={{ x: 1000 }}
          noDataElement={<Empty description="还没有订阅。点「新增订阅」添加一个。" />}
          columns={[
            { title: '名称', dataIndex: 'name', width: 140, render: (v: string) => <b>{v}</b> },
            {
              title: 'URL（token 已脱敏）',
              dataIndex: 'url',
              render: (v: string) => <span className="mono dim url-cell">{v}</span>,
            },
            { title: '格式', dataIndex: 'format', width: 90, render: (v: string) => <Tag size="small">{v || 'auto'}</Tag> },
            {
              title: 'User-Agent',
              dataIndex: 'user_agent',
              width: 170,
              render: (v: string) => (v ? <span className="mono">{v}</span> : <span className="dim">全局默认</span>),
            },
            {
              title: '节点数',
              dataIndex: 'nodes_count',
              width: 90,
              align: 'right',
              render: (v: number) => <span className={'num ' + (!v ? 'v-bad' : '')}>{v ?? 0}</span>,
            },
            {
              title: '启用',
              dataIndex: 'enabled',
              width: 90,
              render: (v: boolean) => <Badge status={v ? 'success' : 'default'} text={v ? '启用' : '停用'} />,
            },
            {
              title: '最近刷新',
              dataIndex: 'last_refresh',
              width: 130,
              render: (v: string) => <span title={fmtTime(v)}>{ago(v) || '—'}</span>,
            },
            {
              title: '操作',
              width: 190,
              fixed: 'right',
              render: (_, s: Subscription) => (
                <Space size={0}>
                  <Button type="text" size="small" icon={<IconEdit />} onClick={() => setEditing(s)}>
                    编辑
                  </Button>
                  <Button type="text" size="small" status="danger" icon={<IconDelete />} onClick={() => removeSub(s)}>
                    删除
                  </Button>
                </Space>
              ),
            },
          ]}
        />
      </Card>

      <IntervalCard snap={snap} reload={reload} />

      <SubModal
        visible={creating || !!editing}
        editing={editing}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
        reload={reload}
      />
    </div>
  );
}

function IntervalCard({ snap, reload }: { snap: Snapshot; reload: () => Promise<void> }) {
  const current = snap.interval.data?.seconds;
  const [value, setValue] = useState<number | undefined>(current);
  const [saving, setSaving] = useState(false);
  const [dirty, setDirty] = useState(false);

  // Follow polling until the operator starts typing.
  useEffect(() => {
    if (!dirty) setValue(current);
  }, [current, dirty]);

  const save = async () => {
    if (value == null || value < 0) return;
    setSaving(true);
    try {
      await api.setRefreshInterval(value);
      Message.success(value === 0 ? '已关闭周期刷新' : `已设为每 ${fmtSeconds(value)}`);
      setDirty(false);
      await reload();
    } catch (e) {
      Message.error('保存失败：' + (e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  return (
    <Card bordered={false} title="周期刷新" extra={<span className="dim">0 = 关闭。阅后即焚类订阅二刷会失效，默认关闭</span>}>
      <Space size={12} wrap>
        <InputNumber
          min={0}
          step={60}
          value={value}
          onChange={(v) => {
            setDirty(true);
            setValue(v);
          }}
          suffix="秒"
          style={{ width: 200 }}
        />
        <Button type="primary" loading={saving} onClick={save} disabled={value === current}>
          保存
        </Button>
        <span className="dim">
          {current == null ? '' : current === 0 ? '当前关闭周期刷新（推荐）' : `当前每 ${fmtSeconds(current)} 刷新一次`}
        </span>
      </Space>
    </Card>
  );
}

function SubModal(props: {
  visible: boolean;
  editing: Subscription | null;
  onClose: () => void;
  reload: () => Promise<void>;
}) {
  const { visible, editing, onClose, reload } = props;
  const [form] = Form.useForm<FormValues>();
  const [submitting, setSubmitting] = useState(false);
  const [err, setErr] = useState('');

  useEffect(() => {
    if (!visible) return;
    setErr('');
    form.resetFields();
    // 关键：不回填脱敏 URL。回填会把 token=c85b***0f02 写进生产。
    form.setFieldsValue(
      editing
        ? { name: editing.name, url: '', format: editing.format || 'auto', user_agent: editing.user_agent || '' }
        : { name: '', url: '', format: 'auto', user_agent: '' },
    );
  }, [visible, editing, form]);

  const submit = async () => {
    let v: FormValues;
    try {
      v = await form.validate();
    } catch {
      return;
    }
    const url = v.url.trim();
    const ua = (v.user_agent || '').trim();
    setSubmitting(true);
    setErr('');
    try {
      if (editing) {
        await api.updateSubscription(editing.name, { url, user_agent: ua });
        Message.success(`已更新 ${editing.name}`);
      } else {
        await api.addSubscription({ name: v.name.trim(), url, format: v.format, user_agent: ua });
        Message.success('已新增订阅');
      }
      onClose();
      await reload();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Modal
      title={editing ? `编辑订阅：${editing.name}` : '新增订阅'}
      visible={visible}
      onCancel={onClose}
      onOk={submit}
      confirmLoading={submitting}
      okText={submitting ? '拉取并重载中…' : '保存并重载'}
      maskClosable={!submitting}
      style={{ width: 560 }}
      unmountOnExit
    >
      <Form form={form} layout="vertical" autoComplete="off">
        <Form.Item label="名称" field="name" rules={[{ required: true, message: '请填写名称' }]}>
          <Input placeholder="main" disabled={!!editing} />
        </Form.Item>
        <Form.Item
          label="订阅 URL"
          field="url"
          extra={
            editing
              ? '当前 URL 的 token 在接口里是脱敏的，无法回填。要改 URL 请粘贴完整的新 URL；本次提交会把这里的内容作为新 URL 写入。'
              : undefined
          }
          rules={[
            { required: true, message: '请粘贴完整的订阅 URL' },
            {
              validator: (value: string | undefined, cb) => {
                if (value && /\*\*\*/.test(value)) {
                  cb('这个 URL 看起来是脱敏后的（含 ***）。请粘贴完整的原始 URL。');
                } else if (value && !/^https?:\/\//i.test(value.trim())) {
                  cb('URL 需以 http:// 或 https:// 开头');
                } else {
                  cb();
                }
              },
            },
          ]}
        >
          <Input placeholder={editing ? '粘贴完整的新 URL（含 token）' : 'https://provider/subscribe?token=...'} />
        </Form.Item>
        <Form.Item label="格式" field="format" extra={editing ? 'PUT 接口不接受修改格式' : undefined}>
          <Select disabled={!!editing} options={FORMATS.map((f) => ({ label: f === 'auto' ? 'auto（推荐）' : f, value: f }))} />
        </Form.Item>
        <Form.Item label="User-Agent（可选）" field="user_agent" extra="留空使用全局默认；节点数不及预期时换一个 UA 试试">
          <AutoComplete data={UA_OPTIONS} placeholder="留空使用全局默认" />
        </Form.Item>
      </Form>
      <Alert type="info" content="提交后会立即重新拉取并重载 mihomo（约 3–5 秒中断）。" />
      {err && <Alert type="error" content={err} className="mt" />}
    </Modal>
  );
}
