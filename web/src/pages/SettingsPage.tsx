import React, { useCallback, useEffect, useState } from 'react';
import { Check, Copy, KeyRound, Save, Server } from 'lucide-react';
import { api, ServerSettings, User } from '../api';

interface Props {
  user: User;
  onToast: (msg: string) => void;
}

export const SettingsPage: React.FC<Props> = ({ user, onToast }) => {
  const [settings, setSettings] = useState<ServerSettings | null>(null);
  const [publicHost, setPublicHost] = useState('');
  const [requireKey, setRequireKey] = useState(true);
  const [alwaysRelay, setAlwaysRelay] = useState(false);
  const [relayServers, setRelayServers] = useState('');
  const [saving, setSaving] = useState(false);
  const [copiedField, setCopiedField] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const s = await api.getSettings();
      setSettings(s);
      setPublicHost(s.public_host || '');
      setRequireKey(s.require_key);
      setAlwaysRelay(s.always_use_relay);
      setRelayServers((s.relay_servers || []).join(', '));
    } catch (err) {
      onToast(`Failed to load settings: ${(err as Error).message}`);
    }
  }, [onToast]);

  useEffect(() => {
    load();
  }, [load]);

  const copyText = (field: string, value: string, label: string) => {
    navigator.clipboard.writeText(value).then(() => {
      setCopiedField(field);
      onToast(`${label} copied`);
      setTimeout(() => setCopiedField(null), 1600);
    });
  };

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    setSaving(true);
    try {
      const list = relayServers
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean);
      await api.updateSettings({
        public_host: publicHost.trim(),
        require_key: requireKey,
        always_use_relay: alwaysRelay,
        relay_servers: list,
      });
      onToast('Server settings updated');
      load();
    } catch (err) {
      onToast(`Update failed: ${(err as Error).message}`);
    } finally {
      setSaving(false);
    }
  };

  if (!settings) {
    return (
      <div className="card">
        <div className="empty-state">Loading server configuration...</div>
      </div>
    );
  }

  const host = settings.public_host || window.location.hostname || 'localhost';
  const idServer = settings.rendezvous_port === 21116 ? host : `${host}:${settings.rendezvous_port}`;
  const relayServer =
    settings.relay_servers && settings.relay_servers.length > 0
      ? settings.relay_servers[0]
      : settings.relay_port === 21117
        ? host
        : `${host}:${settings.relay_port}`;
  const apiServer = `${window.location.protocol}//${host}:${settings.http_port}`;

  return (
    <>
      <div className="card">
        <div className="card-header">
          <div>
            <h3 className="card-title">Quick RustDesk Client Setup</h3>
            <p className="card-subtitle">
              Configure any open-source RustDesk desktop or mobile client to connect through GoDesk Plane.
            </p>
          </div>
          <span className="badge badge-success">
            <KeyRound size={12} /> Ed25519 Signed
          </span>
        </div>

        <div className="card-body" style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
          <div className="form-field">
            <label className="form-label">One-Click Import Config String (Recommended)</label>
            <div className="code-copy-box">
              <code>{settings.config_string}</code>
              <button
                type="button"
                className="btn btn-primary btn-sm"
                onClick={() => copyText('cfg', settings.config_string, 'Config string')}
              >
                {copiedField === 'cfg' ? <Check size={14} /> : <Copy size={14} />} Copy
              </button>
            </div>
            <div className="form-hint">
              In RustDesk: open <strong>Settings → Network → ID/Relay Server</strong> and click the paste/import icon, or run{' '}
              <code className="mono">rustdesk --config &lt;string&gt;</code>.
            </div>
          </div>

          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(260px, 1fr))', gap: 12 }}>
            <div className="form-field">
              <label className="form-label">ID Server (Rendezvous)</label>
              <div className="code-copy-box">
                <code>{idServer}</code>
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => copyText('id', idServer, 'ID Server')}
                >
                  {copiedField === 'id' ? <Check size={14} /> : <Copy size={14} />}
                </button>
              </div>
            </div>

            <div className="form-field">
              <label className="form-label">Relay Server</label>
              <div className="code-copy-box">
                <code>{relayServer}</code>
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => copyText('relay', relayServer, 'Relay Server')}
                >
                  {copiedField === 'relay' ? <Check size={14} /> : <Copy size={14} />}
                </button>
              </div>
            </div>

            <div className="form-field">
              <label className="form-label">API Server</label>
              <div className="code-copy-box">
                <code>{apiServer}</code>
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => copyText('api', apiServer, 'API Server')}
                >
                  {copiedField === 'api' ? <Check size={14} /> : <Copy size={14} />}
                </button>
              </div>
            </div>

            <div className="form-field">
              <label className="form-label">Public Key (id_ed25519.pub)</label>
              <div className="code-copy-box">
                <code>{settings.public_key}</code>
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => copyText('key', settings.public_key, 'Public Key')}
                >
                  {copiedField === 'key' ? <Check size={14} /> : <Copy size={14} />}
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>

      <div className="charts-2col">
        <div className="card">
          <div className="card-header">
            <div>
              <h3 className="card-title">Runtime Server Policy</h3>
              <p className="card-subtitle">Changes apply immediately without restarting the server.</p>
            </div>
          </div>
          <div className="card-body">
            <form onSubmit={handleSave} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
              <div className="form-field">
                <label className="form-label">Public Hostname / IP</label>
                <input
                  className="input mono"
                  value={publicHost}
                  onChange={(e) => setPublicHost(e.target.value)}
                  placeholder="desk.example.com"
                  disabled={!user.is_admin}
                />
                <span className="form-hint">Used for generating the RustDesk client setup string.</span>
              </div>

              <div className="form-field">
                <label className="form-label">Advertised Relay Servers (comma-separated)</label>
                <input
                  className="input mono"
                  value={relayServers}
                  onChange={(e) => setRelayServers(e.target.value)}
                  placeholder={host}
                  disabled={!user.is_admin}
                />
              </div>

              <label className="toggle-row" style={{ cursor: user.is_admin ? 'pointer' : 'default' }}>
                <div>
                  <div style={{ fontWeight: 600 }}>Require Ed25519 Server Key</div>
                  <div className="form-hint">
                    Reject PunchHole and Relay requests that do not match this server&apos;s public key.
                  </div>
                </div>
                <input
                  type="checkbox"
                  checked={requireKey}
                  onChange={(e) => setRequireKey(e.target.checked)}
                  disabled={!user.is_admin}
                />
              </label>

              <label className="toggle-row" style={{ cursor: user.is_admin ? 'pointer' : 'default' }}>
                <div>
                  <div style={{ fontWeight: 600 }}>Always Use Relay</div>
                  <div className="form-hint">
                    Force all client connections through GoDesk Plane&apos;s relay for full traffic accounting.
                  </div>
                </div>
                <input
                  type="checkbox"
                  checked={alwaysRelay}
                  onChange={(e) => setAlwaysRelay(e.target.checked)}
                  disabled={!user.is_admin}
                />
              </label>

              {user.is_admin && (
                <div>
                  <button type="submit" className="btn btn-primary" disabled={saving}>
                    <Save size={14} /> {saving ? 'Saving...' : 'Save Server Policy'}
                  </button>
                </div>
              )}
            </form>
          </div>
        </div>

        <div className="card">
          <div className="card-header">
            <div>
              <h3 className="card-title">Listening Network Ports</h3>
              <p className="card-subtitle">Active RustDesk protocol listeners</p>
            </div>
            <Server size={16} />
          </div>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Service</th>
                  <th>Protocol</th>
                  <th>Port</th>
                </tr>
              </thead>
              <tbody>
                <tr>
                  <td>RustDesk API & Management UI</td>
                  <td className="mono">HTTP / TCP</td>
                  <td className="mono">{settings.http_port}</td>
                </tr>
                <tr>
                  <td>NAT Type Test & Online Status</td>
                  <td className="mono">TCP</td>
                  <td className="mono">{settings.nat_port}</td>
                </tr>
                <tr>
                  <td>Rendezvous ID & Signaling (hbbs)</td>
                  <td className="mono">TCP + UDP</td>
                  <td className="mono">{settings.rendezvous_port}</td>
                </tr>
                <tr>
                  <td>Encrypted Relay Bridge (hbbr)</td>
                  <td className="mono">TCP</td>
                  <td className="mono">{settings.relay_port}</td>
                </tr>
                <tr>
                  <td>Rendezvous WebSocket</td>
                  <td className="mono">WS / TCP</td>
                  <td className="mono">{settings.rendezvous_ws_port}</td>
                </tr>
                <tr>
                  <td>Relay WebSocket</td>
                  <td className="mono">WS / TCP</td>
                  <td className="mono">{settings.relay_ws_port}</td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      {settings.observability && (
        <div className="card">
          <div className="card-header">
            <div>
              <h3 className="card-title">Observability, OpenTelemetry &amp; Logging</h3>
              <p className="card-subtitle">
                Structured slog output with trace correlation, OTLP traces/metrics/logs export, and Prometheus scraping.
              </p>
            </div>
            <span className={`badge ${settings.observability.otel_enabled ? 'badge-success' : ''}`}>
              {settings.observability.otel_enabled ? 'OTLP Active' : 'OTLP Local Only'}
            </span>
          </div>
          <div className="card-body">
            <div className="peer-specs" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))' }}>
              <div>
                <div className="peer-spec-label">Log Level / Format</div>
                <div className="peer-spec-val mono">
                  {settings.observability.log_level} / {settings.observability.log_format}
                </div>
              </div>
              <div>
                <div className="peer-spec-label">OTel Service Name</div>
                <div className="peer-spec-val mono">{settings.observability.otel_service}</div>
              </div>
              <div>
                <div className="peer-spec-label">OTLP Endpoint ({settings.observability.otel_protocol})</div>
                <div className="peer-spec-val mono">
                  {settings.observability.otel_endpoint || 'Not configured (set OTEL_EXPORTER_OTLP_ENDPOINT)'}
                </div>
              </div>
              <div>
                <div className="peer-spec-label">Prometheus Metrics</div>
                <div className="peer-spec-val mono">
                  {settings.observability.metrics_enabled ? (
                    <a
                      href={settings.observability.metrics_endpoint}
                      target="_blank"
                      rel="noreferrer"
                      style={{ color: 'var(--accent)', textDecoration: 'underline' }}
                    >
                      {settings.observability.metrics_endpoint}
                    </a>
                  ) : (
                    'Disabled'
                  )}
                </div>
              </div>
            </div>
          </div>
        </div>
      )}
    </>
  );
};
