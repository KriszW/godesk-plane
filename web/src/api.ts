export interface User {
  id: number;
  username: string;
  email: string;
  is_admin: boolean;
  status: number;
  created_at: number;
  updated_at: number;
}

export interface Peer {
  id: string;
  uuid?: string;
  has_pk: boolean;
  ip: string;
  socket_addr?: string;
  hostname: string;
  username: string;
  os: string;
  cpu: string;
  memory: string;
  version: string;
  alias: string;
  note: string;
  group_name: string;
  tags: string[];
  nat_type: string;
  in_address_book: boolean;
  online: boolean;
  last_reg_at: number;
  last_heartbeat_at: number;
  last_seen_at: number;
  created_at: number;
  updated_at: number;
}

export interface Tag {
  name: string;
  color: number;
  created_at: number;
}

export interface ActiveRelayInfo {
  uuid: string;
  from_ip: string;
  to_ip: string;
  to_id: string;
  conn_type: string;
  bytes_sent: number;
  bytes_recv: number;
  started_at: number;
}

export interface Session {
  id: number;
  session_key?: string;
  relay_uuid?: string;
  from_id: string;
  from_name: string;
  from_ip: string;
  to_id: string;
  to_ip: string;
  conn_type: string;
  is_relay: boolean;
  status: string;
  bytes_sent: number;
  bytes_recv: number;
  duration_sec: number;
  started_at: number;
  ended_at: number;
}

export interface AuditLog {
  id: number;
  category: string;
  action: string;
  severity: string;
  actor_id: string;
  actor_name: string;
  target_id: string;
  target_name: string;
  source_ip: string;
  conn_id: number;
  session_id: string;
  conn_type: string;
  duration_sec: number;
  bytes_transferred: number;
  summary: string;
  metadata?: Record<string, unknown>;
  created_at: number;
}

export interface AnalyticsData {
  range: string;
  total_peers: number;
  online_peers: number;
  offline_peers: number;
  active_sessions: number;
  total_connections: number;
  relay_connections: number;
  direct_connections: number;
  total_bytes_relayed: number;
  avg_duration_sec: number;
  total_audit_events: number;
  security_warnings: number;
  connections_series: { bucket: string; total: number; relay: number; direct: number }[];
  traffic_series: { bucket: string; bytes: number }[];
  os_breakdown: { label: string; count: number }[];
  version_breakdown: { label: string; count: number }[];
  nat_breakdown: { label: string; count: number }[];
  conn_type_breakdown: { label: string; count: number }[];
  top_devices: {
    peer_id: string;
    label: string;
    os: string;
    online: boolean;
    connections: number;
    bytes: number;
  }[];
}

export interface ServerSettings {
  public_host: string;
  public_key: string;
  require_key: boolean;
  always_use_relay: boolean;
  relay_servers: string[];
  http_port: number;
  nat_port: number;
  rendezvous_port: number;
  relay_port: number;
  rendezvous_ws_port: number;
  relay_ws_port: number;
  config_string: string;
  observability?: {
    log_level: string;
    log_format: string;
    otel_enabled: boolean;
    otel_endpoint: string;
    otel_protocol: string;
    otel_service: string;
    metrics_enabled: boolean;
    metrics_endpoint: string;
  };
}

const TOKEN_KEY = 'godesk_plane_token';

export function getStoredToken(): string {
  return localStorage.getItem(TOKEN_KEY) || '';
}

export function setStoredToken(token: string) {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token);
  } else {
    localStorage.removeItem(TOKEN_KEY);
  }
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const token = getStoredToken();
  const headers = new Headers(init.headers);
  if (token) {
    headers.set('Authorization', `Bearer ${token}`);
  }
  if (init.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  const resp = await fetch(path, { ...init, headers });
  if (resp.status === 401) {
    setStoredToken('');
    throw new Error('UNAUTHORIZED');
  }
  const text = await resp.text();
  let parsed: unknown = null;
  if (text) {
    try {
      parsed = JSON.parse(text);
    } catch {
      parsed = text;
    }
  }
  if (!resp.ok) {
    const errMsg =
      typeof parsed === 'object' && parsed && 'error' in parsed
        ? String((parsed as { error: unknown }).error)
        : `HTTP ${resp.status}`;
    throw new Error(errMsg);
  }
  return parsed as T;
}

export const api = {
  login: (username: string, password: string) =>
    request<{ token: string; user: User }>('/admin/api/login', {
      method: 'POST',
      body: JSON.stringify({ username, password }),
    }),
  logout: () => request('/admin/api/logout', { method: 'POST' }),
  me: () => request<{ user: User }>('/admin/api/me'),

  getPeers: (params: Record<string, string> = {}) => {
    const qs = new URLSearchParams(params).toString();
    return request<{ peers: Peer[]; total: number; tags: Tag[] }>(
      `/admin/api/peers${qs ? '?' + qs : ''}`,
    );
  },
  getPeer: (id: string) =>
    request<{ peer: Peer; audits: AuditLog[] }>(`/admin/api/peers/${encodeURIComponent(id)}`),
  addPeer: (body: {
    id: string;
    alias?: string;
    hostname?: string;
    username?: string;
    os?: string;
    note?: string;
    group_name?: string;
    tags?: string[];
  }) => request<Peer>('/admin/api/peers', { method: 'POST', body: JSON.stringify(body) }),
  updatePeer: (
    id: string,
    body: { alias: string; note: string; group_name: string; tags: string[] },
  ) =>
    request<Peer>(`/admin/api/peers/${encodeURIComponent(id)}`, {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
  deletePeer: (id: string) =>
    request(`/admin/api/peers/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  createTag: (name: string, color: number) =>
    request('/admin/api/tags', { method: 'POST', body: JSON.stringify({ name, color }) }),
  deleteTag: (name: string) =>
    request(`/admin/api/tags/${encodeURIComponent(name)}`, { method: 'DELETE' }),

  getAnalytics: (range: string) =>
    request<{ analytics: AnalyticsData; active_relays: ActiveRelayInfo[] }>(
      `/admin/api/analytics?range=${encodeURIComponent(range)}`,
    ),
  getSessions: (limit = 50) =>
    request<{ sessions: Session[]; active_relays: ActiveRelayInfo[] }>(
      `/admin/api/sessions?limit=${limit}`,
    ),

  getAudit: (params: Record<string, string>) => {
    const qs = new URLSearchParams(params).toString();
    return request<{ logs: AuditLog[]; total: number; page: number; page_size: number }>(
      `/admin/api/audit?${qs}`,
    );
  },
  exportAuditUrl: (params: Record<string, string>, format: 'csv' | 'json') => {
    const qs = new URLSearchParams({ ...params, format, token: getStoredToken() }).toString();
    return `/admin/api/audit/export?${qs}`;
  },

  getUsers: () => request<{ users: User[] }>('/admin/api/users'),
  createUser: (body: { username: string; email: string; password: string; is_admin: boolean }) =>
    request<User>('/admin/api/users', { method: 'POST', body: JSON.stringify(body) }),
  updateUser: (
    id: number,
    body: { email: string; is_admin: boolean; status: number; password?: string },
  ) => request<User>(`/admin/api/users/${id}`, { method: 'PUT', body: JSON.stringify(body) }),
  deleteUser: (id: number) => request(`/admin/api/users/${id}`, { method: 'DELETE' }),

  getSettings: () => request<ServerSettings>('/admin/api/settings'),
  updateSettings: (body: {
    public_host: string;
    require_key: boolean;
    always_use_relay: boolean;
    relay_servers: string[];
  }) =>
    request<{ status: string; config_string: string }>('/admin/api/settings', {
      method: 'PUT',
      body: JSON.stringify(body),
    }),
};
