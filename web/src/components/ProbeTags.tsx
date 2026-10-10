import { Tag, Tooltip } from '@arco-design/web-react';
import type { ProbeResult } from '../types';
import { fmtMs, fmtTime, probeLabel } from '../format';

// One tag per site probe; hover shows status / CF challenge / last error.
export default function ProbeTags({ probes }: { probes?: Record<string, ProbeResult> }) {
  const entries = Object.entries(probes || {}).sort(([a], [b]) => a.localeCompare(b));
  if (!entries.length) return <span className="dim">—</span>;
  return (
    <span className="probe-tags">
      {entries.map(([name, p]) => {
        const detail = [
          p.status_code ? `HTTP ${p.status_code}` : '',
          p.latency_ms ? fmtMs(p.latency_ms) : '',
          p.cf_mitigated ? 'Cloudflare 挑战' : '',
          p.last_error || '',
        ].filter(Boolean);
        return (
          <Tooltip
            key={name}
            content={
              <div>
                <div className="tip-title">{name}</div>
                <div>{detail.join(' · ') || '无详情'}</div>
                {p.last_checked_at && <div className="tip-dim">{fmtTime(p.last_checked_at)}</div>}
              </div>
            }
          >
            <Tag size="small" color={p.ok ? 'green' : 'red'} bordered>
              {probeLabel(name)}
            </Tag>
          </Tooltip>
        );
      })}
    </span>
  );
}
