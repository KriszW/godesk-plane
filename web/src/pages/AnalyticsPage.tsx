import React, { useCallback, useEffect, useState } from 'react';
import {
  Activity,
  AlertTriangle,
  ArrowUpDown,
  Clock,
  ExternalLink,
  HardDrive,
  RefreshCw,
  Wifi,
} from 'lucide-react';
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts';
import { ActiveRelayInfo, AnalyticsData, api } from '../api';
import {
  formatBytes,
  formatDuration,
  formatPeerID,
  formatRelativeTime,
  rustdeskConnectUri,
} from '../utils';

interface Props {
  onToast: (msg: string) => void;
}

const BreakdownCard: React.FC<{
  title: string;
  items: { label: string; count: number }[];
}> = ({ title, items }) => {
  const total = items.reduce((acc, i) => acc + i.count, 0) || 1;
  const sorted = [...items].sort((a, b) => b.count - a.count);
  return (
    <div className="card">
      <div className="card-header">
        <h3 className="card-title">{title}</h3>
        <span className="badge mono">{items.reduce((a, b) => a + b.count, 0)}</span>
      </div>
      <div className="card-body">
        {sorted.length === 0 ? (
          <div className="form-hint">No telemetry recorded in this window yet.</div>
        ) : (
          <div className="breakdown-list">
            {sorted.map((item) => {
              const pct = Math.round((item.count / total) * 100);
              return (
                <div className="breakdown-row" key={item.label}>
                  <div className="breakdown-head">
                    <span style={{ fontWeight: 500 }}>{item.label}</span>
                    <span className="mono" style={{ color: 'var(--ink-secondary)', fontSize: 12.5 }}>
                      {item.count} ({pct}%)
                    </span>
                  </div>
                  <div className="breakdown-bar">
                    <div className="breakdown-fill" style={{ width: `${Math.max(pct, 3)}%` }} />
                  </div>
                </div>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
};

export const AnalyticsPage: React.FC<Props> = ({ onToast }) => {
  const [range, setRange] = useState<'24h' | '7d' | '30d'>('24h');
  const [data, setData] = useState<AnalyticsData | null>(null);
  const [activeRelays, setActiveRelays] = useState<ActiveRelayInfo[]>([]);
  const [loading, setLoading] = useState(true);

  const load = useCallback(
    async (quiet = false) => {
      if (!quiet) setLoading(true);
      try {
        const res = await api.getAnalytics(range);
        setData(res.analytics);
        setActiveRelays(res.active_relays || []);
      } catch (err) {
        if (!quiet) onToast(`Failed to load analytics: ${(err as Error).message}`);
      } finally {
        if (!quiet) setLoading(false);
      }
    },
    [range, onToast],
  );

  useEffect(() => {
    load();
    const timer = setInterval(() => load(true), 12000);
    return () => clearInterval(timer);
  }, [load]);

  const onlinePct =
    data && data.total_peers > 0 ? Math.round((data.online_peers / data.total_peers) * 100) : 0;
  const relayPct =
    data && data.total_connections > 0
      ? Math.round((data.relay_connections / data.total_connections) * 100)
      : 0;

  return (
    <>
      <div className="toolbar" style={{ justifyContent: 'space-between' }}>
        <div>
          <div style={{ fontSize: 13, color: 'var(--ink-secondary)' }}>
            Real-time telemetry across connected RustDesk peers, rendezvous signaling, and relay traffic.
          </div>
        </div>
        <div className="toolbar">
          <div className="segmented">
            {(['24h', '7d', '30d'] as const).map((r) => (
              <button
                key={r}
                type="button"
                className={range === r ? 'active' : ''}
                onClick={() => setRange(r)}
              >
                {r === '24h' ? 'Last 24h' : r === '7d' ? 'Last 7d' : 'Last 30d'}
              </button>
            ))}
          </div>
          <button type="button" className="btn btn-sm" onClick={() => load()}>
            <RefreshCw size={14} /> Refresh
          </button>
        </div>
      </div>

      {loading && !data ? (
        <div className="card">
          <div className="empty-state">Loading analytics...</div>
        </div>
      ) : data ? (
        <>
          <div className="kpi-grid">
            <div className="kpi-card">
              <div className="kpi-top">
                <span>Fleet Online Rate</span>
                <Wifi size={15} />
              </div>
              <div className="kpi-value">
                {data.online_peers}
                <span style={{ fontSize: 15, fontWeight: 500, color: 'var(--ink-muted)' }}>
                  {' '}
                  / {data.total_peers}
                </span>
              </div>
              <div className="kpi-meta">{onlinePct}% of registered peers online</div>
            </div>

            <div className="kpi-card">
              <div className="kpi-top">
                <span>Connections ({range})</span>
                <Activity size={15} />
              </div>
              <div className="kpi-value">{data.total_connections}</div>
              <div className="kpi-meta">
                {data.direct_connections} direct · {data.relay_connections} relayed ({relayPct}%)
              </div>
            </div>

            <div className="kpi-card">
              <div className="kpi-top">
                <span>Relayed Traffic ({range})</span>
                <ArrowUpDown size={15} />
              </div>
              <div className="kpi-value">{formatBytes(data.total_bytes_relayed)}</div>
              <div className="kpi-meta">{activeRelays.length} active relay bridge(s) right now</div>
            </div>

            <div className="kpi-card">
              <div className="kpi-top">
                <span>Avg Session & Alerts</span>
                {data.security_warnings > 0 ? <AlertTriangle size={15} /> : <Clock size={15} />}
              </div>
              <div className="kpi-value">{formatDuration(Math.round(data.avg_duration_sec))}</div>
              <div className="kpi-meta">
                {data.total_audit_events} audit events ·{' '}
                <span style={{ color: data.security_warnings > 0 ? 'var(--warning)' : undefined }}>
                  {data.security_warnings} warning(s)
                </span>
              </div>
            </div>
          </div>

          <div className="charts-2col">
            <div className="card">
              <div className="card-header">
                <div>
                  <h3 className="card-title">Connection Activity</h3>
                  <p className="card-subtitle">Direct P2P vs Relayed sessions over time</p>
                </div>
              </div>
              <div className="card-body">
                <div className="chart-box">
                  <ResponsiveContainer width="100%" height="100%">
                    <AreaChart data={data.connections_series}>
                      <CartesianGrid strokeDasharray="3 3" stroke="var(--border-subtle)" />
                      <XAxis dataKey="bucket" tick={{ fontSize: 11, fill: 'var(--ink-muted)' }} />
                      <YAxis allowDecimals={false} tick={{ fontSize: 11, fill: 'var(--ink-muted)' }} />
                      <Tooltip
                        contentStyle={{
                          background: 'var(--bg-surface)',
                          border: '1px solid var(--border-subtle)',
                          borderRadius: 8,
                          fontSize: 12.5,
                        }}
                      />
                      <Area
                        type="monotone"
                        dataKey="direct"
                        name="Direct P2P"
                        stackId="1"
                        stroke="#059669"
                        fill="#059669"
                        fillOpacity={0.22}
                      />
                      <Area
                        type="monotone"
                        dataKey="relay"
                        name="Relayed"
                        stackId="1"
                        stroke="#2563eb"
                        fill="#2563eb"
                        fillOpacity={0.26}
                      />
                    </AreaChart>
                  </ResponsiveContainer>
                </div>
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <div>
                  <h3 className="card-title">Relay Bandwidth Volume</h3>
                  <p className="card-subtitle">Bytes transferred through hbbr relay bridge</p>
                </div>
              </div>
              <div className="card-body">
                <div className="chart-box">
                  <ResponsiveContainer width="100%" height="100%">
                    <BarChart data={data.traffic_series}>
                      <CartesianGrid strokeDasharray="3 3" stroke="var(--border-subtle)" />
                      <XAxis dataKey="bucket" tick={{ fontSize: 11, fill: 'var(--ink-muted)' }} />
                      <YAxis
                        tickFormatter={(v) => formatBytes(Number(v))}
                        tick={{ fontSize: 11, fill: 'var(--ink-muted)' }}
                        width={68}
                      />
                      <Tooltip
                        formatter={(val) => [formatBytes(Number(val)), 'Transferred']}
                        contentStyle={{
                          background: 'var(--bg-surface)',
                          border: '1px solid var(--border-subtle)',
                          borderRadius: 8,
                          fontSize: 12.5,
                        }}
                      />
                      <Bar dataKey="bytes" fill="#2563eb" radius={[4, 4, 0, 0]} />
                    </BarChart>
                  </ResponsiveContainer>
                </div>
              </div>
            </div>
          </div>

          <div className="charts-3col">
            <BreakdownCard title="Operating Systems" items={data.os_breakdown} />
            <BreakdownCard title="RustDesk Client Versions" items={data.version_breakdown} />
            <BreakdownCard title="Connection Types" items={data.conn_type_breakdown} />
          </div>

          <div className="charts-2col">
            <div className="card">
              <div className="card-header">
                <div>
                  <h3 className="card-title">Top Active Devices</h3>
                  <p className="card-subtitle">Most frequently accessed clients in this window</p>
                </div>
              </div>
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Device</th>
                      <th>OS</th>
                      <th>Sessions</th>
                      <th>Traffic</th>
                      <th style={{ textAlign: 'right' }}>Access</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.top_devices.length === 0 ? (
                      <tr>
                        <td colSpan={5} style={{ textAlign: 'center', color: 'var(--ink-muted)', padding: 28 }}>
                          No connections recorded yet.
                        </td>
                      </tr>
                    ) : (
                      data.top_devices.map((dev) => (
                        <tr key={dev.peer_id}>
                          <td>
                            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                              <span className={`status-dot ${dev.online ? 'online' : 'offline'}`} />
                              <div>
                                <div style={{ fontWeight: 600 }}>{dev.label}</div>
                                <div className="mono" style={{ fontSize: 12, color: 'var(--ink-muted)' }}>
                                  {formatPeerID(dev.peer_id)}
                                </div>
                              </div>
                            </div>
                          </td>
                          <td>{dev.os}</td>
                          <td className="mono">{dev.connections}</td>
                          <td className="mono">{formatBytes(dev.bytes)}</td>
                          <td style={{ textAlign: 'right' }}>
                            <a href={rustdeskConnectUri(dev.peer_id)} className="btn btn-sm btn-connect">
                              <ExternalLink size={12} /> Connect
                            </a>
                          </td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
            </div>

            <div className="card">
              <div className="card-header">
                <div>
                  <h3 className="card-title">Live Active Relay Bridges</h3>
                  <p className="card-subtitle">Currently streaming RustDesk relay tunnels</p>
                </div>
                <span className="badge badge-accent mono">{activeRelays.length} active</span>
              </div>
              <div className="table-wrap">
                <table>
                  <thead>
                    <tr>
                      <th>Target Peer</th>
                      <th>Endpoints</th>
                      <th>Traffic</th>
                      <th>Started</th>
                    </tr>
                  </thead>
                  <tbody>
                    {activeRelays.length === 0 ? (
                      <tr>
                        <td colSpan={4} style={{ textAlign: 'center', color: 'var(--ink-muted)', padding: 28 }}>
                          <HardDrive size={18} style={{ opacity: 0.45, marginBottom: 4 }} />
                          <div>No active relay tunnels right now.</div>
                        </td>
                      </tr>
                    ) : (
                      activeRelays.map((r) => (
                        <tr key={r.uuid}>
                          <td className="mono" style={{ fontWeight: 600 }}>
                            {r.to_id ? formatPeerID(r.to_id) : r.uuid.slice(0, 8)}
                          </td>
                          <td className="mono" style={{ fontSize: 12 }}>
                            {r.from_ip} ↔ {r.to_ip}
                          </td>
                          <td className="mono">{formatBytes(r.bytes_sent + r.bytes_recv)}</td>
                          <td style={{ fontSize: 12.5, color: 'var(--ink-secondary)' }}>
                            {formatRelativeTime(r.started_at)}
                          </td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
            </div>
          </div>

          <BreakdownCard title="NAT Type Distribution" items={data.nat_breakdown} />
        </>
      ) : null}
    </>
  );
};
