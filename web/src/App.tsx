import React, { useCallback, useEffect, useState } from 'react';
import {
  BarChart3,
  BookUser,
  FileClock,
  LogOut,
  Menu,
  Moon,
  Settings,
  Sun,
  Users,
} from 'lucide-react';
import { api, getStoredToken, setStoredToken, User } from './api';
import { AddressBookPage } from './pages/AddressBookPage';
import { AnalyticsPage } from './pages/AnalyticsPage';
import { AuditLogPage } from './pages/AuditLogPage';
import { SettingsPage } from './pages/SettingsPage';
import { UsersPage } from './pages/UsersPage';

type Tab = 'addressbook' | 'analytics' | 'audit' | 'settings' | 'users';

const TAB_META: Record<Tab, { title: string; label: string }> = {
  addressbook: { title: 'Address Book & Connected Clients', label: 'Address Book' },
  analytics: { title: 'Fleet & Session Analytics', label: 'Analytics' },
  audit: { title: 'Audit Log & Security Trail', label: 'Audit Log' },
  settings: { title: 'Server & Client Setup', label: 'Server & Setup' },
  users: { title: 'Users & Access Control', label: 'Users' },
};

export const App: React.FC = () => {
  const [user, setUser] = useState<User | null>(null);
  const [checkingAuth, setCheckingAuth] = useState(true);
  const [tab, setTab] = useState<Tab>('addressbook');
  const [onlineCounts, setOnlineCounts] = useState<{ online: number; total: number }>({
    online: 0,
    total: 0,
  });
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  const [toast, setToast] = useState<string | null>(null);
  const [theme, setTheme] = useState<'light' | 'dark'>(() =>
    localStorage.getItem('godesk_theme') === 'light' ? 'light' : 'dark',
  );

  // Login form state
  const [username, setUsername] = useState('admin');
  const [password, setPassword] = useState('');
  const [loginErr, setLoginErr] = useState('');
  const [loggingIn, setLoggingIn] = useState(false);

  useEffect(() => {
    document.documentElement.setAttribute('data-theme', theme);
    localStorage.setItem('godesk_theme', theme);
  }, [theme]);

  const showToast = useCallback((msg: string) => {
    setToast(msg);
  }, []);

  const handleOnlineCountChange = useCallback((online: number, total: number) => {
    setOnlineCounts((prev) =>
      prev.online === online && prev.total === total ? prev : { online, total },
    );
  }, []);

  useEffect(() => {
    if (!toast) return;
    const t = setTimeout(() => setToast(null), 2800);
    return () => clearTimeout(t);
  }, [toast]);

  useEffect(() => {
    const tok = getStoredToken();
    if (!tok) {
      setCheckingAuth(false);
      return;
    }
    api
      .me()
      .then((res) => setUser(res.user))
      .catch(() => setStoredToken(''))
      .finally(() => setCheckingAuth(false));
  }, []);

  const handleLogin = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoginErr('');
    setLoggingIn(true);
    try {
      const res = await api.login(username.trim(), password);
      setStoredToken(res.token);
      setUser(res.user);
      setPassword('');
    } catch (err) {
      setLoginErr((err as Error).message || 'Authentication failed');
    } finally {
      setLoggingIn(false);
    }
  };

  const handleLogout = async () => {
    try {
      await api.logout();
    } catch {
      // ignore
    }
    setStoredToken('');
    setUser(null);
  };

  if (checkingAuth) {
    return (
      <div className="login-page">
        <div style={{ color: 'var(--ink-muted)' }}>Loading GoDesk Plane...</div>
      </div>
    );
  }

  if (!user) {
    return (
      <div className="login-page">
        <div className="login-card">
          <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <div className="brand-mark" style={{ width: 40, height: 40, fontSize: 17 }}>
              GD
            </div>
            <div>
              <h1 style={{ margin: 0, fontSize: 19, fontWeight: 700, letterSpacing: '-0.02em' }}>
                GoDesk Plane
              </h1>
              <div style={{ fontSize: 12.5, color: 'var(--ink-muted)' }}>
                Self-Hosted RustDesk Control Plane
              </div>
            </div>
          </div>

          {loginErr && <div className="error-banner">{loginErr}</div>}

          <form onSubmit={handleLogin} style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
            <div className="form-field">
              <label className="form-label" htmlFor="login-user">
                Username
              </label>
              <input
                id="login-user"
                className="input"
                autoComplete="username"
                required
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </div>
            <div className="form-field">
              <label className="form-label" htmlFor="login-pass">
                Password
              </label>
              <input
                id="login-pass"
                className="input"
                type="password"
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
            <button
              id="login-submit"
              type="submit"
              className="btn btn-primary"
              style={{ padding: '10px 14px', fontWeight: 600 }}
              disabled={loggingIn}
            >
              {loggingIn ? 'Signing in...' : 'Sign In'}
            </button>
          </form>

          <div style={{ fontSize: 12, color: 'var(--ink-muted)', textAlign: 'center' }}>
            Default initial account: <span className="mono">admin</span> /{' '}
            <span className="mono">godesk-admin</span> (override via env vars)
          </div>
        </div>
      </div>
    );
  }

  const navigate = (t: Tab) => {
    setTab(t);
    setMobileNavOpen(false);
  };

  return (
    <div className="app-shell">
      <aside className={`sidebar ${mobileNavOpen ? 'open' : ''}`}>
        <div className="brand">
          <div className="brand-mark">GD</div>
          <div className="brand-text">
            <span className="brand-name">GoDesk Plane</span>
            <span className="brand-sub">RustDesk Server</span>
          </div>
        </div>

        <nav className="nav-section" aria-label="Primary navigation">
          <div className="nav-label">Fleet</div>
          <button
            type="button"
            className={`nav-item ${tab === 'addressbook' ? 'active' : ''}`}
            onClick={() => navigate('addressbook')}
          >
            <BookUser size={17} />
            <span>Address Book</span>
            <span className="nav-badge">
              {onlineCounts.online}/{onlineCounts.total}
            </span>
          </button>

          <button
            type="button"
            className={`nav-item ${tab === 'analytics' ? 'active' : ''}`}
            onClick={() => navigate('analytics')}
          >
            <BarChart3 size={17} />
            <span>Analytics</span>
          </button>

          <button
            type="button"
            className={`nav-item ${tab === 'audit' ? 'active' : ''}`}
            onClick={() => navigate('audit')}
          >
            <FileClock size={17} />
            <span>Audit Log</span>
          </button>

          <div className="nav-label">Administration</div>
          <button
            type="button"
            className={`nav-item ${tab === 'settings' ? 'active' : ''}`}
            onClick={() => navigate('settings')}
          >
            <Settings size={17} />
            <span>Server & Setup</span>
          </button>

          {user.is_admin && (
            <button
              type="button"
              className={`nav-item ${tab === 'users' ? 'active' : ''}`}
              onClick={() => navigate('users')}
            >
              <Users size={17} />
              <span>Users</span>
            </button>
          )}
        </nav>

        <div className="sidebar-footer">
          <div className="user-pill">
            <div className="user-info">
              <div className="user-name">{user.username}</div>
              <div className="user-role">{user.is_admin ? 'Administrator' : 'Operator'}</div>
            </div>
            <button
              type="button"
              className="btn btn-ghost btn-icon"
              onClick={handleLogout}
              title="Sign out"
            >
              <LogOut size={15} />
            </button>
          </div>
        </div>
      </aside>

      {mobileNavOpen && (
        <div
          className="backdrop"
          style={{ zIndex: 25 }}
          onClick={() => setMobileNavOpen(false)}
        />
      )}

      <div className="main-area">
        <header className="topbar">
          <div className="topbar-left">
            <button
              type="button"
              className="btn btn-ghost btn-icon mobile-menu-btn"
              onClick={() => setMobileNavOpen((v) => !v)}
              aria-label="Toggle navigation"
            >
              <Menu size={18} />
            </button>
            <h1 className="topbar-title">{TAB_META[tab].title}</h1>
          </div>

          <div className="topbar-right">
            <button
              type="button"
              className="btn btn-sm"
              onClick={() => setTheme((t) => (t === 'light' ? 'dark' : 'light'))}
              title="Toggle theme"
            >
              {theme === 'light' ? <Moon size={14} /> : <Sun size={14} />}
              <span>{theme === 'light' ? 'Dark' : 'Light'}</span>
            </button>
          </div>
        </header>

        <main className="content">
          {tab === 'addressbook' && (
            <AddressBookPage
              onToast={showToast}
              onOnlineCountChange={handleOnlineCountChange}
            />
          )}
          {tab === 'analytics' && <AnalyticsPage onToast={showToast} />}
          {tab === 'audit' && <AuditLogPage onToast={showToast} />}
          {tab === 'settings' && <SettingsPage user={user} onToast={showToast} />}
          {tab === 'users' && user.is_admin && (
            <UsersPage currentUser={user} onToast={showToast} />
          )}
        </main>
      </div>

      {toast && <div className="toast">{toast}</div>}
    </div>
  );
};
