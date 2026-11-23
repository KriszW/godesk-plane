import React, { useCallback, useEffect, useMemo, useState } from 'react';
import {
  Check,
  ChevronDown,
  Copy,
  Edit3,
  ExternalLink,
  LayoutGrid,
  List,
  Monitor,
  Plus,
  RefreshCw,
  Search,
  Server,
  Shield,
  Tag as TagIcon,
  Terminal,
  Trash2,
  X,
} from 'lucide-react';
import { api, AuditLog, Peer, Tag } from '../api';
import { WebClientModal } from '../components/WebClientModal';
import {
  argbToHex,
  formatPeerID,
  formatRelativeTime,
  formatTimestamp,
  hexToArgb,
  normalizeOSLabel,
  rustdeskConnectUri,
} from '../utils';

interface Props {
  onToast: (msg: string) => void;
  onOnlineCountChange?: (online: number, total: number) => void;
}

const TAG_SWATCHES = ['#2563eb', '#059669', '#d97706', '#dc2626', '#0891b2', '#4f46e5', '#475569'];

export const AddressBookPage: React.FC<Props> = ({ onToast, onOnlineCountChange }) => {
  const [peers, setPeers] = useState<Peer[]>([]);
  const [tags, setTags] = useState<Tag[]>([]);
  const [loading, setLoading] = useState(true);
  const [search, setSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState<'all' | 'online' | 'offline'>('all');
  const [osFilter, setOsFilter] = useState('all');
  const [tagFilter, setTagFilter] = useState('all');
  const [viewMode, setViewMode] = useState<'table' | 'grid'>('table');
  const [copiedId, setCopiedId] = useState<string | null>(null);
  const [webClientPeer, setWebClientPeer] = useState<Peer | null>(null);
  const [openMenuPeerId, setOpenMenuPeerId] = useState<string | null>(null);

  useEffect(() => {
    if (!openMenuPeerId) return;
    const close = () => setOpenMenuPeerId(null);
    window.addEventListener('click', close);
    return () => window.removeEventListener('click', close);
  }, [openMenuPeerId]);

  // Selected device drawer
  const [selectedPeer, setSelectedPeer] = useState<Peer | null>(null);
  const [peerAudits, setPeerAudits] = useState<AuditLog[]>([]);
  const [editAlias, setEditAlias] = useState('');
  const [editGroup, setEditGroup] = useState('');
  const [editNote, setEditNote] = useState('');
  const [editTags, setEditTags] = useState<string[]>([]);
  const [savingEdit, setSavingEdit] = useState(false);

  // Add Device modal
  const [showAddModal, setShowAddModal] = useState(false);
  const [newPeer, setNewPeer] = useState({
    id: '',
    alias: '',
    hostname: '',
    username: '',
    os: '',
    group_name: 'Default',
    note: '',
    tags: '' as string,
  });

  // Manage Tags modal
  const [showTagsModal, setShowTagsModal] = useState(false);
  const [newTagName, setNewTagName] = useState('');
  const [newTagColor, setNewTagColor] = useState(TAG_SWATCHES[0]);

  const loadPeers = useCallback(async (quiet = false) => {
    if (!quiet) setLoading(true);
    try {
      const res = await api.getPeers();
      setPeers(res.peers || []);
      setTags(res.tags || []);
      const online = (res.peers || []).filter((p) => p.online).length;
      onOnlineCountChange?.(online, (res.peers || []).length);
    } catch (e) {
      if (!quiet) onToast(`Failed to load address book: ${(e as Error).message}`);
    } finally {
      if (!quiet) setLoading(false);
    }
  }, [onOnlineCountChange, onToast]);

  useEffect(() => {
    loadPeers();
    const timer = setInterval(() => loadPeers(true), 10000);
    return () => clearInterval(timer);
  }, [loadPeers]);

  const tagColorMap = useMemo(() => {
    const map: Record<string, string> = {};
    for (const t of tags) {
      map[t.name] = argbToHex(t.color || 0xff2563eb);
    }
    return map;
  }, [tags]);

  const filteredPeers = useMemo(() => {
    const q = search.trim().toLowerCase();
    return peers.filter((p) => {
      if (statusFilter === 'online' && !p.online) return false;
      if (statusFilter === 'offline' && p.online) return false;
      if (osFilter !== 'all' && normalizeOSLabel(p.os).toLowerCase() !== osFilter) return false;
      if (tagFilter !== 'all' && !(p.tags || []).includes(tagFilter)) return false;
      if (!q) return true;
      const hay = [
        p.id,
        p.alias,
        p.hostname,
        p.username,
        p.ip,
        p.os,
        p.note,
        p.group_name,
        ...(p.tags || []),
      ]
        .join(' ')
        .toLowerCase();
      return hay.includes(q);
    });
  }, [peers, search, statusFilter, osFilter, tagFilter]);

  const onlineCount = useMemo(() => peers.filter((p) => p.online).length, [peers]);
  const groupCount = useMemo(
    () => new Set(peers.map((p) => p.group_name || 'Default')).size,
    [peers],
  );

  const handleCopy = (text: string, label: string, key?: string) => {
    navigator.clipboard.writeText(text).then(() => {
      onToast(`${label} copied to clipboard`);
      if (key) {
        setCopiedId(key);
        setTimeout(() => setCopiedId(null), 1600);
      }
    });
  };

  const handleConnect = (peer: Peer, forceRelay = false) => {
    const uri = rustdeskConnectUri(peer.id, forceRelay);
    window.location.href = uri;
    onToast(
      `Launching RustDesk for ${peer.alias || peer.hostname || formatPeerID(peer.id)}${forceRelay ? ' (Relay)' : ''}`,
    );
  };

  const openDrawer = async (peer: Peer) => {
    setSelectedPeer(peer);
    setEditAlias(peer.alias || '');
    setEditGroup(peer.group_name || 'Default');
    setEditNote(peer.note || '');
    setEditTags(peer.tags || []);
    setPeerAudits([]);
    try {
      const res = await api.getPeer(peer.id);
      setSelectedPeer(res.peer);
      setPeerAudits(res.audits || []);
    } catch {
      // keep initial peer data
    }
  };

  const handleSavePeer = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!selectedPeer) return;
    setSavingEdit(true);
    try {
      const updated = await api.updatePeer(selectedPeer.id, {
        alias: editAlias.trim(),
        group_name: editGroup.trim() || 'Default',
        note: editNote.trim(),
        tags: editTags,
      });
      setSelectedPeer(updated);
      onToast('Device details saved');
      loadPeers(true);
    } catch (err) {
      onToast(`Save failed: ${(err as Error).message}`);
    } finally {
      setSavingEdit(false);
    }
  };

  const handleDeletePeer = async (peer: Peer) => {
    if (!confirm(`Remove device "${peer.alias || peer.hostname || peer.id}" from GoDesk Plane?`)) {
      return;
    }
    try {
      await api.deletePeer(peer.id);
      if (selectedPeer?.id === peer.id) setSelectedPeer(null);
      onToast(`Removed peer ${formatPeerID(peer.id)}`);
      loadPeers(true);
    } catch (err) {
      onToast(`Delete failed: ${(err as Error).message}`);
    }
  };

  const handleAddPeer = async (e: React.FormEvent) => {
    e.preventDefault();
    const parsedTags = newPeer.tags
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean);
    try {
      await api.addPeer({
        id: newPeer.id.replace(/\s+/g, ''),
        alias: newPeer.alias.trim(),
        hostname: newPeer.hostname.trim(),
        username: newPeer.username.trim(),
        os: newPeer.os.trim(),
        group_name: newPeer.group_name.trim() || 'Default',
        note: newPeer.note.trim(),
        tags: parsedTags,
      });
      setShowAddModal(false);
      setNewPeer({
        id: '',
        alias: '',
        hostname: '',
        username: '',
        os: '',
        group_name: 'Default',
        note: '',
        tags: '',
      });
      onToast('Device added to address book');
      loadPeers(true);
    } catch (err) {
      onToast(`Failed to add device: ${(err as Error).message}`);
    }
  };

  const handleCreateTag = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!newTagName.trim()) return;
    try {
      await api.createTag(newTagName.trim(), hexToArgb(newTagColor));
      setNewTagName('');
      onToast(`Tag "${newTagName.trim()}" saved`);
      loadPeers(true);
    } catch (err) {
      onToast(`Failed to save tag: ${(err as Error).message}`);
    }
  };

  const handleDeleteTag = async (name: string) => {
    try {
      await api.deleteTag(name);
      onToast(`Deleted tag "${name}"`);
      loadPeers(true);
    } catch (err) {
      onToast(`Failed to delete tag: ${(err as Error).message}`);
    }
  };

  const toggleEditTag = (name: string) => {
    setEditTags((prev) =>
      prev.includes(name) ? prev.filter((t) => t !== name) : [...prev, name],
    );
  };

  return (
    <>
      <div className="kpi-grid">
        <div className="kpi-card">
          <div className="kpi-top">
            <span>Online Clients</span>
            <span className="status-dot online" />
          </div>
          <div className="kpi-value" style={{ color: 'var(--success)' }}>
            {onlineCount}
          </div>
          <div className="kpi-meta">Ready for one-click remote access</div>
        </div>
        <div className="kpi-card">
          <div className="kpi-top">
            <span>Total Address Book</span>
            <Monitor size={15} />
          </div>
          <div className="kpi-value">{peers.length}</div>
          <div className="kpi-meta">{peers.length - onlineCount} currently offline</div>
        </div>
        <div className="kpi-card">
          <div className="kpi-top">
            <span>Device Groups</span>
            <Server size={15} />
          </div>
          <div className="kpi-value">{groupCount}</div>
          <div className="kpi-meta">Synced with RustDesk clients</div>
        </div>
        <div className="kpi-card">
          <div className="kpi-top">
            <span>Tags Defined</span>
            <TagIcon size={15} />
          </div>
          <div className="kpi-value">{tags.length}</div>
          <div className="kpi-meta">Filterable across fleet</div>
        </div>
      </div>

      <div className="card">
        <div className="card-header">
          <div className="toolbar" style={{ flex: 1 }}>
            <div className="input-wrap">
              <Search size={15} />
              <input
                id="addressbook-search"
                className="input"
                placeholder="Search by Peer ID, alias, hostname, IP, user, or tag..."
                value={search}
                onChange={(e) => setSearch(e.target.value)}
              />
            </div>

            <div className="segmented" role="group" aria-label="Status filter">
              {(['all', 'online', 'offline'] as const).map((st) => (
                <button
                  key={st}
                  type="button"
                  className={statusFilter === st ? 'active' : ''}
                  onClick={() => setStatusFilter(st)}
                >
                  {st === 'all' ? 'All' : st === 'online' ? 'Online' : 'Offline'}
                </button>
              ))}
            </div>

            <select
              className="select"
              aria-label="Filter by OS"
              value={osFilter}
              onChange={(e) => setOsFilter(e.target.value)}
            >
              <option value="all">All OS</option>
              <option value="linux">Linux</option>
              <option value="windows">Windows</option>
              <option value="macos">macOS</option>
              <option value="android">Android</option>
              <option value="ios">iOS</option>
            </select>

            {tags.length > 0 && (
              <select
                className="select"
                aria-label="Filter by Tag"
                value={tagFilter}
                onChange={(e) => setTagFilter(e.target.value)}
              >
                <option value="all">All Tags</option>
                {tags.map((t) => (
                  <option key={t.name} value={t.name}>
                    {t.name}
                  </option>
                ))}
              </select>
            )}
          </div>

          <div className="toolbar">
            <div className="segmented">
              <button
                type="button"
                className={viewMode === 'table' ? 'active' : ''}
                onClick={() => setViewMode('table')}
                title="Table view"
              >
                <List size={14} />
              </button>
              <button
                type="button"
                className={viewMode === 'grid' ? 'active' : ''}
                onClick={() => setViewMode('grid')}
                title="Card view"
              >
                <LayoutGrid size={14} />
              </button>
            </div>
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => setShowTagsModal(true)}
            >
              <TagIcon size={14} /> Tags
            </button>
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => loadPeers()}
              title="Refresh devices"
            >
              <RefreshCw size={14} />
            </button>
            <button
              id="add-device-btn"
              type="button"
              className="btn btn-primary btn-sm"
              onClick={() => setShowAddModal(true)}
            >
              <Plus size={14} /> Add Device
            </button>
          </div>
        </div>

        {loading && peers.length === 0 ? (
          <div className="empty-state">Loading connected devices...</div>
        ) : filteredPeers.length === 0 ? (
          <div className="empty-state">
            <Monitor size={34} strokeWidth={1.5} />
            <div style={{ fontWeight: 600, color: 'var(--ink-primary)' }}>
              {peers.length === 0 ? 'No RustDesk clients registered yet' : 'No matching devices'}
            </div>
            <div style={{ maxWidth: 420, fontSize: 13 }}>
              {peers.length === 0
                ? 'Point any RustDesk client to this server using the Server & Setup page, or add a Peer ID manually.'
                : 'Try adjusting your search query or filters.'}
            </div>
          </div>
        ) : viewMode === 'table' ? (
          <div className="table-wrap" style={{ overflow: 'visible' }}>
            <table>
              <thead>
                <tr>
                  <th>Status</th>
                  <th>Device / Peer ID</th>
                  <th>System</th>
                  <th>Network & Version</th>
                  <th>Tags & Group</th>
                  <th>Last Seen</th>
                  <th style={{ textAlign: 'right' }}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {filteredPeers.map((p) => {
                  const displayName = p.alias || p.hostname || 'Unnamed Device';
                  const menuOpen = openMenuPeerId === p.id;
                  return (
                    <tr key={p.id}>
                      <td>
                        <span className={`badge ${p.online ? 'badge-success' : ''}`}>
                          <span className={`status-dot ${p.online ? 'online' : 'offline'}`} />
                          {p.online ? 'Online' : 'Offline'}
                        </span>
                      </td>
                      <td>
                        <div style={{ fontWeight: 600 }}>{displayName}</div>
                        <div className="peer-id-row">
                          <span>{formatPeerID(p.id)}</span>
                          <button
                            type="button"
                            className="btn btn-ghost btn-sm"
                            style={{ padding: '2px 4px' }}
                            onClick={() => handleCopy(p.id, 'Peer ID', p.id)}
                            title="Copy Peer ID"
                          >
                            {copiedId === p.id ? <Check size={12} /> : <Copy size={12} />}
                          </button>
                        </div>
                      </td>
                      <td>
                        <div style={{ fontWeight: 500 }}>{normalizeOSLabel(p.os)}</div>
                        <div style={{ fontSize: 12, color: 'var(--ink-muted)' }}>
                          {[p.username, p.memory].filter(Boolean).join(' · ') || '—'}
                        </div>
                      </td>
                      <td>
                        <div className="mono" style={{ fontSize: 12.5 }}>
                          {p.ip || '—'}
                        </div>
                        <div style={{ fontSize: 12, color: 'var(--ink-muted)' }}>
                          {p.version ? `v${p.version}` : 'No client info'}
                          {p.nat_type && p.nat_type !== 'UNKNOWN' ? ` · ${p.nat_type}` : ''}
                        </div>
                      </td>
                      <td>
                        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 5 }}>
                          {p.group_name && p.group_name !== 'Default' && (
                            <span className="badge">{p.group_name}</span>
                          )}
                          {(p.tags || []).map((t) => (
                            <span key={t} className="tag-chip">
                              <span
                                className="tag-dot"
                                style={{ background: tagColorMap[t] || 'var(--accent)' }}
                              />
                              {t}
                            </span>
                          ))}
                          {(!p.tags || p.tags.length === 0) &&
                            (!p.group_name || p.group_name === 'Default') && (
                              <span style={{ color: 'var(--ink-muted)', fontSize: 12 }}>—</span>
                            )}
                        </div>
                      </td>
                      <td style={{ whiteSpace: 'nowrap', fontSize: 12.5, color: 'var(--ink-secondary)' }}>
                        {formatRelativeTime(p.last_seen_at)}
                      </td>
                      <td>
                        <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 6, alignItems: 'center' }}>
                          <button
                            type="button"
                            className="btn btn-sm btn-icon"
                            onClick={() => openDrawer(p)}
                            title="Inspect & Edit"
                          >
                            <Edit3 size={14} />
                          </button>
                          <button
                            type="button"
                            className="btn btn-sm btn-icon btn-danger"
                            onClick={() => handleDeletePeer(p)}
                            title="Remove Device"
                          >
                            <Trash2 size={14} />
                          </button>
                          <div
                            className="split-btn-wrap"
                            onClick={(e) => e.stopPropagation()}
                          >
                            <button
                              type="button"
                              className="btn btn-connect btn-sm split-btn-main"
                              onClick={() => {
                                setOpenMenuPeerId(null);
                                setWebClientPeer(p);
                              }}
                              title="Open in RustDesk Web Client"
                            >
                              <Monitor size={13} /> Open
                            </button>
                            <button
                              type="button"
                              className="btn btn-connect btn-sm split-btn-toggle"
                              aria-label="More open options"
                              aria-expanded={menuOpen}
                              onClick={() => setOpenMenuPeerId(menuOpen ? null : p.id)}
                            >
                              <ChevronDown size={13} />
                            </button>
                            {menuOpen && (
                              <div className="split-menu" role="menu">
                                <a
                                  href={rustdeskConnectUri(p.id)}
                                  className="split-menu-item"
                                  role="menuitem"
                                  onClick={(e) => {
                                    e.preventDefault();
                                    setOpenMenuPeerId(null);
                                    handleConnect(p, false);
                                  }}
                                >
                                  <ExternalLink size={13} /> RustDesk
                                </a>
                                <a
                                  href={rustdeskConnectUri(p.id, true)}
                                  className="split-menu-item"
                                  role="menuitem"
                                  onClick={(e) => {
                                    e.preventDefault();
                                    setOpenMenuPeerId(null);
                                    handleConnect(p, true);
                                  }}
                                >
                                  <Shield size={13} /> RustDesk (Relay)
                                </a>
                                <button
                                  type="button"
                                  className="split-menu-item"
                                  role="menuitem"
                                  onClick={() => {
                                    setOpenMenuPeerId(null);
                                    handleCopy(`rustdesk --connect ${p.id}`, 'CLI command');
                                  }}
                                >
                                  <Terminal size={13} /> Copy CLI
                                </button>
                              </div>
                            )}
                          </div>
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <div className="card-body">
            <div className="peer-grid">
              {filteredPeers.map((p) => {
                const displayName = p.alias || p.hostname || 'Unnamed Device';
                const menuOpen = openMenuPeerId === p.id;
                return (
                  <div className="peer-card" key={p.id}>
                    <div className="peer-card-head">
                      <div>
                        <div className="peer-title">{displayName}</div>
                        <div className="peer-id-row">
                          <span>{formatPeerID(p.id)}</span>
                          <button
                            type="button"
                            className="btn btn-ghost btn-sm"
                            style={{ padding: 2 }}
                            onClick={() => handleCopy(p.id, 'Peer ID', p.id)}
                          >
                            {copiedId === p.id ? <Check size={12} /> : <Copy size={12} />}
                          </button>
                        </div>
                      </div>
                      <span className={`badge ${p.online ? 'badge-success' : ''}`}>
                        <span className={`status-dot ${p.online ? 'online' : 'offline'}`} />
                        {p.online ? 'Online' : 'Offline'}
                      </span>
                    </div>

                    <div className="peer-specs">
                      <div>
                        <div className="peer-spec-label">OS / User</div>
                        <div className="peer-spec-val">
                          {normalizeOSLabel(p.os)} {p.username ? `(${p.username})` : ''}
                        </div>
                      </div>
                      <div>
                        <div className="peer-spec-label">IP Address</div>
                        <div className="peer-spec-val mono">{p.ip || '—'}</div>
                      </div>
                      <div>
                        <div className="peer-spec-label">Hardware</div>
                        <div className="peer-spec-val">{p.memory || p.cpu || '—'}</div>
                      </div>
                      <div>
                        <div className="peer-spec-label">Last Seen</div>
                        <div className="peer-spec-val">{formatRelativeTime(p.last_seen_at)}</div>
                      </div>
                    </div>

                    {p.tags && p.tags.length > 0 && (
                      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 5 }}>
                        {p.tags.map((t) => (
                          <span key={t} className="tag-chip">
                            <span
                              className="tag-dot"
                              style={{ background: tagColorMap[t] || 'var(--accent)' }}
                            />
                            {t}
                          </span>
                        ))}
                      </div>
                    )}

                    <div className="peer-card-foot">
                      <button
                        type="button"
                        className="btn btn-sm"
                        onClick={() => openDrawer(p)}
                      >
                        Details
                      </button>
                      <div
                        className="split-btn-wrap"
                        style={{ flex: 1, justifyContent: 'flex-end' }}
                        onClick={(e) => e.stopPropagation()}
                      >
                        <button
                          type="button"
                          className="btn btn-connect btn-sm split-btn-main"
                          style={{ flex: 1 }}
                          onClick={() => {
                            setOpenMenuPeerId(null);
                            setWebClientPeer(p);
                          }}
                        >
                          <Monitor size={13} /> Open
                        </button>
                        <button
                          type="button"
                          className="btn btn-connect btn-sm split-btn-toggle"
                          aria-label="More open options"
                          aria-expanded={menuOpen}
                          onClick={() => setOpenMenuPeerId(menuOpen ? null : p.id)}
                        >
                          <ChevronDown size={13} />
                        </button>
                        {menuOpen && (
                          <div className="split-menu" role="menu">
                            <a
                              href={rustdeskConnectUri(p.id)}
                              className="split-menu-item"
                              role="menuitem"
                              onClick={(e) => {
                                e.preventDefault();
                                setOpenMenuPeerId(null);
                                handleConnect(p, false);
                              }}
                            >
                              <ExternalLink size={13} /> RustDesk
                            </a>
                            <a
                              href={rustdeskConnectUri(p.id, true)}
                              className="split-menu-item"
                              role="menuitem"
                              onClick={(e) => {
                                e.preventDefault();
                                setOpenMenuPeerId(null);
                                handleConnect(p, true);
                              }}
                            >
                              <Shield size={13} /> RustDesk (Relay)
                            </a>
                            <button
                              type="button"
                              className="split-menu-item"
                              role="menuitem"
                              onClick={() => {
                                setOpenMenuPeerId(null);
                                handleCopy(`rustdesk --connect ${p.id}`, 'CLI command');
                              }}
                            >
                              <Terminal size={13} /> Copy CLI
                            </button>
                          </div>
                        )}
                      </div>
                    </div>
                  </div>
                );
              })}
            </div>
          </div>
        )}
      </div>

      {webClientPeer && (
        <WebClientModal
          peer={webClientPeer}
          onClose={() => setWebClientPeer(null)}
          onToast={onToast}
        />
      )}

      {/* Device Details & Edit Slide-over Drawer */}
      {selectedPeer && (
        <div className="drawer-backdrop" onClick={() => setSelectedPeer(null)}>
          <div className="drawer" onClick={(e) => e.stopPropagation()}>
            <div className="drawer-header">
              <div>
                <div style={{ fontWeight: 700, fontSize: 16 }}>
                  {selectedPeer.alias || selectedPeer.hostname || formatPeerID(selectedPeer.id)}
                </div>
                <div className="mono" style={{ fontSize: 12.5, color: 'var(--ink-muted)' }}>
                  ID: {formatPeerID(selectedPeer.id)}
                </div>
              </div>
              <button
                type="button"
                className="btn btn-ghost btn-icon"
                onClick={() => setSelectedPeer(null)}
              >
                <X size={18} />
              </button>
            </div>

            <div className="drawer-body">
              <div style={{ display: 'flex', gap: 8, flexWrap: 'wrap' }}>
                <button
                  type="button"
                  className="btn btn-connect"
                  style={{ flex: 1 }}
                  onClick={() => {
                    const target = selectedPeer;
                    setSelectedPeer(null);
                    setWebClientPeer(target);
                  }}
                >
                  <Monitor size={14} /> Open Web Client
                </button>
                <button
                  type="button"
                  className="btn"
                  onClick={() => handleConnect(selectedPeer, false)}
                  title="Open in native RustDesk app"
                >
                  <ExternalLink size={14} /> RustDesk
                </button>
                <button
                  type="button"
                  className="btn"
                  onClick={() =>
                    handleCopy(`rustdesk --connect ${selectedPeer.id}`, 'CLI command')
                  }
                  title="Copy CLI connect command"
                >
                  <Terminal size={14} /> CLI
                </button>
              </div>

              <div className="peer-specs" style={{ gridTemplateColumns: '1fr 1fr' }}>
                <div>
                  <div className="peer-spec-label">Status</div>
                  <div className="peer-spec-val">
                    {selectedPeer.online ? 'Online' : 'Offline'} ({formatRelativeTime(selectedPeer.last_seen_at)})
                  </div>
                </div>
                <div>
                  <div className="peer-spec-label">IP Address</div>
                  <div className="peer-spec-val mono">{selectedPeer.ip || '—'}</div>
                </div>
                <div>
                  <div className="peer-spec-label">Hostname</div>
                  <div className="peer-spec-val">{selectedPeer.hostname || '—'}</div>
                </div>
                <div>
                  <div className="peer-spec-label">OS User</div>
                  <div className="peer-spec-val">{selectedPeer.username || '—'}</div>
                </div>
                <div style={{ gridColumn: '1 / -1' }}>
                  <div className="peer-spec-label">Operating System</div>
                  <div className="peer-spec-val">{selectedPeer.os || '—'}</div>
                </div>
                <div style={{ gridColumn: '1 / -1' }}>
                  <div className="peer-spec-label">CPU</div>
                  <div className="peer-spec-val">{selectedPeer.cpu || '—'}</div>
                </div>
                <div>
                  <div className="peer-spec-label">Memory</div>
                  <div className="peer-spec-val">{selectedPeer.memory || '—'}</div>
                </div>
                <div>
                  <div className="peer-spec-label">RustDesk Version / NAT</div>
                  <div className="peer-spec-val">
                    {selectedPeer.version || '—'} / {selectedPeer.nat_type || 'UNKNOWN'}
                  </div>
                </div>
              </div>

              <form onSubmit={handleSavePeer} style={{ display: 'flex', flexDirection: 'column', gap: 12 }}>
                <div className="form-field">
                  <label className="form-label">Alias</label>
                  <input
                    className="input"
                    value={editAlias}
                    onChange={(e) => setEditAlias(e.target.value)}
                    placeholder="e.g. Prod Build Runner 01"
                  />
                </div>
                <div className="form-field">
                  <label className="form-label">Group</label>
                  <input
                    className="input"
                    value={editGroup}
                    onChange={(e) => setEditGroup(e.target.value)}
                    placeholder="Default"
                  />
                </div>
                <div className="form-field">
                  <label className="form-label">Tags</label>
                  <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6 }}>
                    {tags.length === 0 ? (
                      <span className="form-hint">Create tags using the Tags button above.</span>
                    ) : (
                      tags.map((t) => {
                        const active = editTags.includes(t.name);
                        return (
                          <button
                            key={t.name}
                            type="button"
                            className={`tag-chip`}
                            style={{
                              cursor: 'pointer',
                              background: active ? 'var(--accent-soft)' : undefined,
                              color: active ? 'var(--accent)' : undefined,
                              borderColor: active ? 'var(--accent)' : undefined,
                            }}
                            onClick={() => toggleEditTag(t.name)}
                          >
                            <span
                              className="tag-dot"
                              style={{ background: tagColorMap[t.name] || 'var(--accent)' }}
                            />
                            {t.name}
                          </button>
                        );
                      })
                    )}
                  </div>
                </div>
                <div className="form-field">
                  <label className="form-label">Notes</label>
                  <textarea
                    className="textarea"
                    rows={3}
                    value={editNote}
                    onChange={(e) => setEditNote(e.target.value)}
                    placeholder="Location, owner, maintenance notes..."
                  />
                </div>
                <div style={{ display: 'flex', gap: 8 }}>
                  <button type="submit" className="btn btn-primary" disabled={savingEdit}>
                    {savingEdit ? 'Saving...' : 'Save Changes'}
                  </button>
                  <button
                    type="button"
                    className="btn btn-danger"
                    onClick={() => handleDeletePeer(selectedPeer)}
                  >
                    <Trash2 size={14} /> Delete
                  </button>
                </div>
              </form>

              <div style={{ borderTop: '1px solid var(--border-subtle)', paddingTop: 14 }}>
                <div style={{ fontWeight: 600, marginBottom: 8 }}>Recent Activity</div>
                {peerAudits.length === 0 ? (
                  <div className="form-hint">No audit events recorded for this peer yet.</div>
                ) : (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
                    {peerAudits.slice(0, 10).map((a) => (
                      <div
                        key={a.id}
                        style={{
                          padding: '8px 10px',
                          background: 'var(--bg-subtle)',
                          borderRadius: 6,
                          fontSize: 12.5,
                        }}
                      >
                        <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8 }}>
                          <span style={{ fontWeight: 600 }}>{a.action}</span>
                          <span className="mono" style={{ fontSize: 11, color: 'var(--ink-muted)' }}>
                            {formatTimestamp(a.created_at)}
                          </span>
                        </div>
                        <div style={{ color: 'var(--ink-secondary)', marginTop: 2 }}>{a.summary}</div>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            </div>
          </div>
        </div>
      )}

      {/* Add Device Modal */}
      {showAddModal && (
        <div className="backdrop" onClick={() => setShowAddModal(false)}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-header">
              <strong>Add Device to Address Book</strong>
              <button
                type="button"
                className="btn btn-ghost btn-icon"
                onClick={() => setShowAddModal(false)}
              >
                <X size={16} />
              </button>
            </div>
            <form onSubmit={handleAddPeer}>
              <div className="modal-body">
                <div className="form-field">
                  <label className="form-label">RustDesk Peer ID *</label>
                  <input
                    className="input mono"
                    required
                    placeholder="e.g. 1357317218"
                    value={newPeer.id}
                    onChange={(e) => setNewPeer({ ...newPeer, id: e.target.value })}
                  />
                </div>
                <div className="form-field">
                  <label className="form-label">Alias</label>
                  <input
                    className="input"
                    placeholder="Friendly display name"
                    value={newPeer.alias}
                    onChange={(e) => setNewPeer({ ...newPeer, alias: e.target.value })}
                  />
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
                  <div className="form-field">
                    <label className="form-label">Hostname</label>
                    <input
                      className="input"
                      placeholder="workstation-01"
                      value={newPeer.hostname}
                      onChange={(e) => setNewPeer({ ...newPeer, hostname: e.target.value })}
                    />
                  </div>
                  <div className="form-field">
                    <label className="form-label">OS</label>
                    <input
                      className="input"
                      placeholder="Linux / Windows / macOS"
                      value={newPeer.os}
                      onChange={(e) => setNewPeer({ ...newPeer, os: e.target.value })}
                    />
                  </div>
                </div>
                <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
                  <div className="form-field">
                    <label className="form-label">Group</label>
                    <input
                      className="input"
                      value={newPeer.group_name}
                      onChange={(e) => setNewPeer({ ...newPeer, group_name: e.target.value })}
                    />
                  </div>
                  <div className="form-field">
                    <label className="form-label">Tags (comma separated)</label>
                    <input
                      className="input"
                      placeholder="prod, finance"
                      value={newPeer.tags}
                      onChange={(e) => setNewPeer({ ...newPeer, tags: e.target.value })}
                    />
                  </div>
                </div>
                <div className="form-field">
                  <label className="form-label">Notes</label>
                  <textarea
                    className="textarea"
                    rows={2}
                    value={newPeer.note}
                    onChange={(e) => setNewPeer({ ...newPeer, note: e.target.value })}
                  />
                </div>
              </div>
              <div className="modal-footer">
                <button type="button" className="btn" onClick={() => setShowAddModal(false)}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary">
                  Add Device
                </button>
              </div>
            </form>
          </div>
        </div>
      )}

      {/* Manage Tags Modal */}
      {showTagsModal && (
        <div className="backdrop" onClick={() => setShowTagsModal(false)}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-header">
              <strong>Manage Address Book Tags</strong>
              <button
                type="button"
                className="btn btn-ghost btn-icon"
                onClick={() => setShowTagsModal(false)}
              >
                <X size={16} />
              </button>
            </div>
            <div className="modal-body">
              <form onSubmit={handleCreateTag} style={{ display: 'flex', gap: 8, alignItems: 'center' }}>
                <input
                  className="input"
                  placeholder="New tag name (e.g. production)"
                  value={newTagName}
                  onChange={(e) => setNewTagName(e.target.value)}
                />
                <div style={{ display: 'flex', gap: 4 }}>
                  {TAG_SWATCHES.map((hex) => (
                    <button
                      key={hex}
                      type="button"
                      onClick={() => setNewTagColor(hex)}
                      style={{
                        width: 22,
                        height: 22,
                        borderRadius: '50%',
                        background: hex,
                        border: newTagColor === hex ? '2px solid var(--ink-primary)' : '2px solid transparent',
                        cursor: 'pointer',
                      }}
                    />
                  ))}
                </div>
                <button type="submit" className="btn btn-primary btn-sm">
                  Add
                </button>
              </form>

              <div style={{ display: 'flex', flexDirection: 'column', gap: 6, marginTop: 8 }}>
                {tags.length === 0 ? (
                  <div className="form-hint">No tags created yet.</div>
                ) : (
                  tags.map((t) => (
                    <div
                      key={t.name}
                      style={{
                        display: 'flex',
                        alignItems: 'center',
                        justifyContent: 'space-between',
                        padding: '8px 12px',
                        borderRadius: 6,
                        background: 'var(--bg-subtle)',
                      }}
                    >
                      <span className="tag-chip" style={{ border: 'none', background: 'transparent', padding: 0 }}>
                        <span
                          className="tag-dot"
                          style={{ background: argbToHex(t.color || 0xff2563eb), width: 10, height: 10 }}
                        />
                        <strong>{t.name}</strong>
                      </span>
                      <button
                        type="button"
                        className="btn btn-ghost btn-sm"
                        onClick={() => handleDeleteTag(t.name)}
                      >
                        <Trash2 size={14} />
                      </button>
                    </div>
                  ))
                )}
              </div>
            </div>
            <div className="modal-footer">
              <button type="button" className="btn" onClick={() => setShowTagsModal(false)}>
                Done
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  );
};
