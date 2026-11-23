import React, { useCallback, useEffect, useState } from 'react';
import { Plus, Shield, Trash2, User as UserIcon, X } from 'lucide-react';
import { api, User } from '../api';
import { formatTimestamp } from '../utils';

interface Props {
  currentUser: User;
  onToast: (msg: string) => void;
}

export const UsersPage: React.FC<Props> = ({ currentUser, onToast }) => {
  const [users, setUsers] = useState<User[]>([]);
  const [loading, setLoading] = useState(true);
  const [showCreate, setShowCreate] = useState(false);
  const [form, setForm] = useState({ username: '', email: '', password: '', is_admin: false });

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const res = await api.getUsers();
      setUsers(res.users || []);
    } catch (err) {
      onToast(`Failed to load users: ${(err as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, [onToast]);

  useEffect(() => {
    load();
  }, [load]);

  const handleCreate = async (e: React.FormEvent) => {
    e.preventDefault();
    try {
      await api.createUser(form);
      setShowCreate(false);
      setForm({ username: '', email: '', password: '', is_admin: false });
      onToast(`User "${form.username}" created`);
      load();
    } catch (err) {
      onToast(`Create failed: ${(err as Error).message}`);
    }
  };

  const toggleStatus = async (u: User) => {
    try {
      await api.updateUser(u.id, {
        email: u.email,
        is_admin: u.is_admin,
        status: u.status === 1 ? 0 : 1,
      });
      onToast(`Updated status for ${u.username}`);
      load();
    } catch (err) {
      onToast(`Update failed: ${(err as Error).message}`);
    }
  };

  const handleDelete = async (u: User) => {
    if (!confirm(`Delete user "${u.username}"?`)) return;
    try {
      await api.deleteUser(u.id);
      onToast(`Deleted user ${u.username}`);
      load();
    } catch (err) {
      onToast(`Delete failed: ${(err as Error).message}`);
    }
  };

  return (
    <>
      <div className="card">
        <div className="card-header">
          <div>
            <h3 className="card-title">Accounts & RustDesk Client Sign-In</h3>
            <p className="card-subtitle">
              Users can sign into both this Management Console and the native RustDesk client to sync their Address Book.
            </p>
          </div>
          <button type="button" className="btn btn-primary btn-sm" onClick={() => setShowCreate(true)}>
            <Plus size={14} /> New User
          </button>
        </div>

        {loading && users.length === 0 ? (
          <div className="empty-state">Loading users...</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Username</th>
                  <th>Email</th>
                  <th>Role</th>
                  <th>Status</th>
                  <th>Created</th>
                  <th style={{ textAlign: 'right' }}>Actions</th>
                </tr>
              </thead>
              <tbody>
                {users.map((u) => (
                  <tr key={u.id}>
                    <td style={{ fontWeight: 600 }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                        {u.is_admin ? <Shield size={14} color="var(--accent)" /> : <UserIcon size={14} />}
                        {u.username}
                        {u.id === currentUser.id && <span className="badge">You</span>}
                      </div>
                    </td>
                    <td style={{ color: 'var(--ink-secondary)' }}>{u.email || '—'}</td>
                    <td>
                      <span className={`badge ${u.is_admin ? 'badge-accent' : ''}`}>
                        {u.is_admin ? 'Admin' : 'Operator'}
                      </span>
                    </td>
                    <td>
                      <span className={`badge ${u.status === 1 ? 'badge-success' : 'badge-danger'}`}>
                        {u.status === 1 ? 'Active' : 'Disabled'}
                      </span>
                    </td>
                    <td className="mono" style={{ fontSize: 12, color: 'var(--ink-secondary)' }}>
                      {formatTimestamp(u.created_at)}
                    </td>
                    <td style={{ textAlign: 'right' }}>
                      <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 6 }}>
                        {u.id !== currentUser.id && (
                          <>
                            <button type="button" className="btn btn-sm" onClick={() => toggleStatus(u)}>
                              {u.status === 1 ? 'Disable' : 'Enable'}
                            </button>
                            <button
                              type="button"
                              className="btn btn-sm btn-icon btn-danger"
                              onClick={() => handleDelete(u)}
                            >
                              <Trash2 size={14} />
                            </button>
                          </>
                        )}
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {showCreate && (
        <div className="backdrop" onClick={() => setShowCreate(false)}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="modal-header">
              <strong>Create User Account</strong>
              <button
                type="button"
                className="btn btn-ghost btn-icon"
                onClick={() => setShowCreate(false)}
              >
                <X size={16} />
              </button>
            </div>
            <form onSubmit={handleCreate}>
              <div className="modal-body">
                <div className="form-field">
                  <label className="form-label">Username *</label>
                  <input
                    className="input"
                    required
                    value={form.username}
                    onChange={(e) => setForm({ ...form, username: e.target.value })}
                  />
                </div>
                <div className="form-field">
                  <label className="form-label">Email</label>
                  <input
                    className="input"
                    type="email"
                    value={form.email}
                    onChange={(e) => setForm({ ...form, email: e.target.value })}
                  />
                </div>
                <div className="form-field">
                  <label className="form-label">Password *</label>
                  <input
                    className="input"
                    type="password"
                    required
                    minLength={6}
                    value={form.password}
                    onChange={(e) => setForm({ ...form, password: e.target.value })}
                  />
                </div>
                <label className="toggle-row" style={{ cursor: 'pointer' }}>
                  <div>
                    <div style={{ fontWeight: 600 }}>Administrator Role</div>
                    <div className="form-hint">Can manage users and change server security policy.</div>
                  </div>
                  <input
                    type="checkbox"
                    checked={form.is_admin}
                    onChange={(e) => setForm({ ...form, is_admin: e.target.checked })}
                  />
                </label>
              </div>
              <div className="modal-footer">
                <button type="button" className="btn" onClick={() => setShowCreate(false)}>
                  Cancel
                </button>
                <button type="submit" className="btn btn-primary">
                  Create Account
                </button>
              </div>
            </form>
          </div>
        </div>
      )}
    </>
  );
};
