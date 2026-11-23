export function formatPeerID(id: string): string {
  const clean = id.replace(/\s+/g, '');
  if (/^\d{9,12}$/.test(clean)) {
    return clean.replace(/\B(?=(\d{3})+(?!\d))/g, ' ');
  }
  return clean;
}

export function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let val = bytes;
  let idx = 0;
  while (val >= 1024 && idx < units.length - 1) {
    val /= 1024;
    idx++;
  }
  return `${val < 10 && idx > 0 ? val.toFixed(2) : val.toFixed(1)} ${units[idx]}`;
}

export function formatDuration(seconds: number): string {
  if (!seconds || seconds <= 0) return '0s';
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.floor(seconds % 60);
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

export function formatRelativeTime(ms: number): string {
  if (!ms || ms <= 0) return 'Never';
  const diffSec = Math.max(0, Math.floor((Date.now() - ms) / 1000));
  if (diffSec < 15) return 'Just now';
  if (diffSec < 60) return `${diffSec}s ago`;
  const m = Math.floor(diffSec / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  const d = Math.floor(h / 24);
  if (d < 30) return `${d}d ago`;
  return new Date(ms).toLocaleDateString();
}

export function formatTimestamp(ms: number): string {
  if (!ms) return '-';
  return new Date(ms).toLocaleString(undefined, {
    month: 'short',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  });
}

export function argbToHex(argb: number): string {
  const rgb = argb & 0xffffff;
  return '#' + rgb.toString(16).padStart(6, '0');
}

export function hexToArgb(hex: string): number {
  const clean = hex.replace('#', '');
  const rgb = parseInt(clean, 16) & 0xffffff;
  return (0xff000000 | rgb) >>> 0;
}

export function rustdeskConnectUri(peerID: string, forceRelay = false): string {
  const clean = peerID.replace(/\s+/g, '');
  return forceRelay ? `rustdesk://${clean}/r` : `rustdesk://${clean}`;
}

export function normalizeOSLabel(os: string): string {
  const s = (os || '').toLowerCase();
  if (!s) return 'Unknown';
  if (s.includes('win')) return 'Windows';
  if (s.includes('mac') || s.includes('darwin')) return 'macOS';
  if (s.includes('android')) return 'Android';
  if (s.includes('ios')) return 'iOS';
  if (s.includes('linux') || s.includes('ubuntu') || s.includes('debian') || s.includes('fedora'))
    return 'Linux';
  return os;
}
