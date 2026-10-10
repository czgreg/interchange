import { useState } from 'react';
import { Alert, Button, Form, Input } from '@arco-design/web-react';
import { IconLock, IconMoonFill, IconSunFill, IconLink } from '@arco-design/web-react/icon';
import * as api from './api';
import Logo from './components/Logo';

interface Props {
  onConnected: () => void;
  initialError?: string;
  dark: boolean;
  onToggleTheme: () => void;
}

export default function Login({ onConnected, initialError, dark, onToggleTheme }: Props) {
  const [form] = Form.useForm();
  const [err, setErr] = useState(initialError || '');
  const [busy, setBusy] = useState(false);

  // Same-origin default: when the control plane serves this page itself,
  // the API lives at location.origin.
  const defaultBase = location.protocol.startsWith('http') ? location.origin : '';

  const submit = async (values: { base: string; token?: string }) => {
    setBusy(true);
    setErr('');
    api.connect(values.base || '', values.token || '');
    try {
      await api.getStatus(); // probe once: address + token both usable
      form.setFieldValue('token', ''); // do not leave it in the DOM
      onConnected();
    } catch (e) {
      api.disconnect();
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login">
      <Button
        className="login-theme"
        shape="circle"
        icon={dark ? <IconSunFill /> : <IconMoonFill />}
        onClick={onToggleTheme}
        aria-label="切换主题"
      />
      <div className="login-hero">
        <Logo size={56} />
        <h1>Interchange</h1>
        <p className="login-cn">立交网关 · 控制台</p>
        <p className="login-tag">
          在飞连转发节点上，让内网和海外同时可达 —— 员工零配置、零客户端、零感知。
        </p>
        <ul className="login-points">
          <li>节点打分与站点探针，实时可见</li>
          <li>订阅增删改，一处完成</li>
          <li>按终端 IP 秒查出口</li>
        </ul>
      </div>

      <div className="login-card">
        <h2>连接到节点</h2>
        <p className="login-hint">
          若 <code>api.token</code> 为空可留空。token 只保存在当前标签页内存里，不写入任何浏览器存储，刷新页面需要重新输入。
        </p>
        <Form
          form={form}
          layout="vertical"
          onSubmit={submit}
          initialValues={{ base: defaultBase, token: '' }}
          autoComplete="off"
        >
          <Form.Item label="控制面地址" field="base" extra="默认取当前页面同源">
            <Input prefix={<IconLink />} placeholder="http://<node-ip>:18080" />
          </Form.Item>
          <Form.Item label="API Token（可选）" field="token">
            <Input.Password prefix={<IconLock />} placeholder="api.token" autoComplete="current-password" />
          </Form.Item>
          {err && <Alert type="error" content={err} className="login-err" />}
          <Button type="primary" htmlType="submit" long loading={busy} size="large">
            连接
          </Button>
        </Form>
      </div>
    </div>
  );
}
