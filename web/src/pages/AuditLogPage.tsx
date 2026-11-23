import React, { useCallback, useEffect, useState } from 'react';
import {
  ChevronLeft,
  ChevronRight,
  Download,
  FileText,
  RefreshCw,
  Search,
  X,
} from 'lucide-react';
import { api, AuditLog } from '../api';
import { formatBytes, formatDuration, formatPeerID, formatTimestamp } from '../utils';

interface Props {
  onToast: (msg: string) => void;
}

const CATEGORIES = [
  { id: 'all', label: 'All Events' },
  { id: 'connection', label: 'Connections' },
  { id: 'relay', label: 'Relay' },
  { id: 'file', label: 'File Transfers' },
  { id: 'peer', label: 'Peers' },
  { id: 'security', label: 'Security' },
  { id: 'admin', label: 'Admin' },
  { id: 'alarm', label: 'Alarms' },
];

export const AuditLogPage: React.FC<Props> = ({ onToast }) => {
  const [logs, setLogs] = useState<AuditLog[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const pageSize = 30;
  const [category, setCategory] = useState('all');
  const [severity, setSeverity] = useState('all');
  const [search, setSearch] = useState('');
  const [loading, setLoading] = useState(true);
  const [selectedLog, setSelectedLog] = useState<AuditLog | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const params: Record<string, string> = {
        page: String(page),
        page_size: String(pageSize),
      };
      if (category !== 'all') params.category = category;
      if (severity !== 'all') params.severity = severity;
      if (search.trim()) params.search = search.trim();
      const res = await api.getAudit(params);
      setLogs(res.logs || []);
      setTotal(res.total || 0);
    } catch (err) {
      onToast(`Failed to load audit log: ${(err as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, [page, category, severity, search, onToast]);

  useEffect(() => {
    load();
  }, [load]);

  const totalPages = Math.max(1, Math.ceil(total / pageSize));

  const exportParams = () => {
    const p: Record<string, string> = {};
    if (category !== 'all') p.category = category;
    if (severity !== 'all') p.severity = severity;
    if (search.trim()) p.search = search.trim();
    return p;
  };

  const severityBadgeClass = (sev: string) => {
    if (sev === 'error') return 'badge-danger';
    if (sev === 'warn') return 'badge-warning';
    return 'badge-accent';
  };

  return (
    <>
      <div className="card">
        <div className="card-header" style={{ flexDirection: 'column', alignItems: 'stretch', gap: 12 }}>
          <div style={{ display: 'flex', gap: 6, flexWrap: 'wrap' }}>
            {CATEGORIES.map((c) => (
              <button
                key={c.id}
                type="button"
                className={`btn btn-sm ${category === c.id ? 'btn-primary' : ''}`}
                onClick={() => {
                  setCategory(c.id);
                  setPage(1);
                }}
              >
                {c.label}
              </button>
            ))}
          </div>

          <div className="toolbar">
            <div className="input-wrap">
              <Search size={15} />
              <input
                id="audit-search"
                className="input"
                placeholder="Search peer ID, IP address, user, action, or summary..."
                value={search}
                onChange={(e) => {
                  setSearch(e.target.value);
                  setPage(1);
                }}
              />
            </div>

            <select
              className="select"
              value={severity}
              onChange={(e) => {
                setSeverity(e.target.value);
                setPage(1);
              }}
            >
              <option value="all">All Severities</option>
              <option value="info">Info</option>
              <option value="warn">Warning</option>
              <option value="error">Error</option>
            </select>

            <button type="button" className="btn btn-sm" onClick={load}>
              <RefreshCw size={14} /> Refresh
            </button>

            <a
              href={api.exportAuditUrl(exportParams(), 'csv')}
              className="btn btn-sm"
              download="godesk-audit-log.csv"
            >
              <Download size={14} /> CSV
            </a>

            <a
              href={api.exportAuditUrl(exportParams(), 'json')}
              className="btn btn-sm"
              download="godesk-audit-log.json"
            >
              <Download size={14} /> JSON
            </a>
          </div>
        </div>

        {loading && logs.length === 0 ? (
          <div className="empty-state">Loading audit events...</div>
        ) : logs.length === 0 ? (
          <div className="empty-state">
            <FileText size={32} strokeWidth={1.5} />
            <div style={{ fontWeight: 600, color: 'var(--ink-primary)' }}>No audit records found</div>
            <div style={{ fontSize: 13 }}>
              Server events, RustDesk client connections, relay sessions, and file transfers appear here automatically.
            </div>
          </div>
        ) : (
          <>
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Timestamp</th>
                    <th>Category</th>
                    <th>Action</th>
                    <th>Actor / Target</th>
                    <th>Source IP</th>
                    <th>Metrics</th>
                    <th>Summary</th>
                  </tr>
                </thead>
                <tbody>
                  {logs.map((l) => (
                    <tr
                      key={l.id}
                      style={{ cursor: 'pointer' }}
                      onClick={() => setSelectedLog(l)}
                    >
                      <td className="mono" style={{ fontSize: 12, whiteSpace: 'nowrap', color: 'var(--ink-secondary)' }}>
                        {formatTimestamp(l.created_at)}
                      </td>
                      <td>
                        <span className={`badge ${severityBadgeClass(l.severity)}`}>
                          {l.category}
                        </span>
                      </td>
                      <td className="mono" style={{ fontSize: 12.5, fontWeight: 500 }}>
                        {l.action}
                      </td>
                      <td className="mono" style={{ fontSize: 12 }}>
                        {l.actor_name || l.actor_id ? (
                          <span>{l.actor_name || formatPeerID(l.actor_id)}</span>
                        ) : null}
                        {(l.actor_name || l.actor_id) && (l.target_name || l.target_id) ? ' → ' : ''}
                        {l.target_name || l.target_id ? (
                          <strong>{l.target_name || formatPeerID(l.target_id)}</strong>
                        ) : null}
                        {!(l.actor_name || l.actor_id || l.target_name || l.target_id) ? '—' : null}
                      </td>
                      <td className="mono" style={{ fontSize: 12 }}>
                        {l.source_ip || '—'}
                      </td>
                      <td className="mono" style={{ fontSize: 12, whiteSpace: 'nowrap', color: 'var(--ink-secondary)' }}>
                        {l.bytes_transferred > 0 ? formatBytes(l.bytes_transferred) : ''}
                        {l.bytes_transferred > 0 && l.duration_sec > 0 ? ' · ' : ''}
                        {l.duration_sec > 0 ? formatDuration(l.duration_sec) : ''}
                        {!l.bytes_transferred && !l.duration_sec ? (l.conn_type || '—') : ''}
                      </td>
                      <td style={{ maxWidth: 380, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                        {l.summary}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <div
              style={{
                padding: '12px 20px',
                borderTop: '1px solid var(--border-subtle)',
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                fontSize: 13,
                color: 'var(--ink-secondary)',
              }}
            >
              <span>
                Showing {(page - 1) * pageSize + 1}–{Math.min(page * pageSize, total)} of{' '}
                <strong>{total}</strong> events
              </span>
              <div style={{ display: 'flex', gap: 6, alignItems: 'center' }}>
                <button
                  type="button"
                  className="btn btn-sm"
                  disabled={page <= 1}
                  onClick={() => setPage((p) => Math.max(1, p - 1))}
                >
                  <ChevronLeft size={14} /> Prev
                </button>
                <span className="mono" style={{ fontSize: 12.5, padding: '0 6px' }}>
                  {page} / {totalPages}
                </span>
                <button
                  type="button"
                  className="btn btn-sm"
                  disabled={page >= totalPages}
                  onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                >
                  Next <ChevronRight size={14} />
                </button>
              </div>
            </div>
          </>
        )}
      </div>

      {/* Event Metadata Drawer */}
      {selectedLog && (
        <div className="drawer-backdrop" onClick={() => setSelectedLog(null)}>
          <div className="drawer" onClick={(e) => e.stopPropagation()}>
            <div className="drawer-header">
              <div>
                <div style={{ fontWeight: 700, fontSize: 15 }}>Audit Event #{selectedLog.id}</div>
                <div className="mono" style={{ fontSize: 12, color: 'var(--ink-muted)' }}>
                  {formatTimestamp(selectedLog.created_at)}
                </div>
              </div>
              <button
                type="button"
                className="btn btn-ghost btn-icon"
                onClick={() => setSelectedLog(null)}
              >
                <X size={18} />
              </button>
            </div>
            <div className="drawer-body">
              <div style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <span className={`badge ${severityBadgeClass(selectedLog.severity)}`}>
                  {selectedLog.severity.toUpperCase()}
                </span>
                <span className="badge">{selectedLog.category}</span>
                <span className="mono" style={{ fontSize: 12.5, fontWeight: 600 }}>
                  {selectedLog.action}
                </span>
              </div>

              <div style={{ fontSize: 14, fontWeight: 500 }}>{selectedLog.summary}</div>

              <div className="peer-specs" style={{ gridTemplateColumns: '1fr 1fr' }}>
                <div>
                  <div className="peer-spec-label">Actor</div>
                  <div className="peer-spec-val mono">
                    {selectedLog.actor_name || selectedLog.actor_id || '—'}
                  </div>
                </div>
                <div>
                  <div className="peer-spec-label">Target</div>
                  <div className="peer-spec-val mono">
                    {selectedLog.target_name || selectedLog.target_id || '—'}
                  </div>
                </div>
                <div>
                  <div className="peer-spec-label">Source IP</div>
                  <div className="peer-spec-val mono">{selectedLog.source_ip || '—'}</div>
                </div>
                <div>
                  <div className="peer-spec-label">Connection Type</div>
                  <div className="peer-spec-val mono">{selectedLog.conn_type || '—'}</div>
                </div>
                <div>
                  <div className="peer-spec-label">Duration</div>
                  <div className="peer-spec-val mono">{formatDuration(selectedLog.duration_sec)}</div>
                </div>
                <div>
                  <div className="peer-spec-label">Bytes Transferred</div>
                  <div className="peer-spec-val mono">
                    {formatBytes(selectedLog.bytes_transferred)}
                  </div>
                </div>
                {selectedLog.session_id && (
                  <div style={{ gridColumn: '1 / -1' }}>
                    <div className="peer-spec-label">Session / Relay UUID</div>
                    <div className="peer-spec-val mono">{selectedLog.session_id}</div>
                  </div>
                )}
              </div>

              <div>
                <div className="form-label" style={{ marginBottom: 6 }}>
                  Raw Event Metadata
                </div>
                <pre className="json-pre">
                  {JSON.stringify(selectedLog.metadata || {}, null, 2)}
                </pre>
              </div>
            </div>
          </div>
        </div>
      )}
    </>
  );
};
