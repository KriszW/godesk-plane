import React, { useCallback, useEffect, useRef, useState } from 'react';
import {
  ClipboardCopy,
  KeyRound,
  Maximize2,
  Minimize2,
  Monitor,
  Power,
  RefreshCw,
  ShieldCheck,
  X,
} from 'lucide-react';
import { getStoredToken, Peer } from '../api';
import { formatPeerID } from '../utils';

interface DisplayInfo {
  index: number;
  x: number;
  y: number;
  width: number;
  height: number;
  name: string;
  online: boolean;
  scale: number;
}

interface PeerSessionInfo {
  username: string;
  hostname: string;
  platform: string;
  version: string;
  current_display: number;
  displays: DisplayInfo[];
}

interface Props {
  peer: Peer;
  onClose: () => void;
  onToast: (msg: string) => void;
}

const CODEC_MAP: Record<number, { name: string; config: string }> = {
  1: { name: 'VP9', config: 'vp09.00.10.08' },
  2: { name: 'VP8', config: 'vp8' },
  3: { name: 'H.264', config: 'avc1.42E01F' },
  4: { name: 'AV1', config: 'av01.0.04M.08' },
  5: { name: 'H.265', config: 'hev1.1.6.L93.B0' },
};

const SPECIAL_KEYS = new Set([
  'Alt', 'Backspace', 'CapsLock', 'Control', 'Delete',
  'ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight',
  'End', 'Home', 'Escape', 'Enter', 'Tab', 'Shift', 'Meta',
  'PageUp', 'PageDown', 'Insert',
  'F1', 'F2', 'F3', 'F4', 'F5', 'F6', 'F7', 'F8', 'F9', 'F10', 'F11', 'F12',
]);

export const WebClientModal: React.FC<Props> = ({ peer, onClose, onToast }) => {
  const [phase, setPhase] = useState<'connecting' | 'auth' | 'ready' | 'error' | 'closed'>('connecting');
  const [statusMsg, setStatusMsg] = useState('Signaling via Rendezvous server...');
  const [authError, setAuthError] = useState('');
  const [password, setPassword] = useState('');
  const [submittingAuth, setSubmittingAuth] = useState(false);
  const [peerInfo, setPeerInfo] = useState<PeerSessionInfo | null>(null);
  const [activeDisplay, setActiveDisplay] = useState(0);
  const [codecName, setCodecName] = useState('');
  const [resolution, setResolution] = useState<{ w: number; h: number }>({ w: 0, h: 0 });
  const [fps, setFps] = useState(0);
  const [isFullscreen, setIsFullscreen] = useState(false);
  const [clipboardText, setClipboardText] = useState('');
  const [showClipBox, setShowClipBox] = useState(false);
  const [streamPreset, setStreamPreset] = useState('vp8-balanced');

  const containerRef = useRef<HTMLDivElement | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const httpSidRef = useRef<string>('');
  const decoderRef = useRef<VideoDecoder | null>(null);
  const configuredCodecRef = useRef<number>(0);
  const gotKeyFrameRef = useRef<boolean>(false);
  const frameCountRef = useRef<number>(0);
  const activeDisplayRef = useRef<number>(0);
  const peerInfoRef = useRef<PeerSessionInfo | null>(null);
  const lastMouseMoveRef = useRef<number>(0);
  const httpQueueRef = useRef<Record<string, unknown>[]>([]);
  const httpFlushingRef = useRef<boolean>(false);
  const httpBatchTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const httpRttMsRef = useRef<number>(0);
  const moveInFlightRef = useRef<boolean>(false);
  const pendingMoveRef = useRef<Record<string, unknown> | null>(null);

  useEffect(() => {
    activeDisplayRef.current = activeDisplay;
  }, [activeDisplay]);

  useEffect(() => {
    peerInfoRef.current = peerInfo;
  }, [peerInfo]);

  const flushHTTPQueue = useCallback(async () => {
    if (httpBatchTimerRef.current) {
      clearTimeout(httpBatchTimerRef.current);
      httpBatchTimerRef.current = null;
    }
    if (httpFlushingRef.current) return;
    httpFlushingRef.current = true;
    try {
      while (httpQueueRef.current.length > 0) {
        const sid = httpSidRef.current;
        if (!sid) {
          httpQueueRef.current = [];
          break;
        }
        const batch = httpQueueRef.current.splice(0, httpQueueRef.current.length);
        const token = getStoredToken();
        const t0 = performance.now();
        try {
          const res = await fetch(`/admin/api/webclient/send?sid=${encodeURIComponent(sid)}`, {
            method: 'POST',
            headers: {
              'Content-Type': 'application/json',
              Authorization: `Bearer ${token}`,
            },
            body: JSON.stringify(batch.length === 1 ? batch[0] : batch),
          });
          httpRttMsRef.current = performance.now() - t0;
          if (res.status === 404 || res.status === 410) {
            httpQueueRef.current = [];
            pendingMoveRef.current = null;
            break;
          }
        } catch {
          // ignore transient send errors
        }
      }
    } finally {
      httpFlushingRef.current = false;
      if (httpQueueRef.current.length > 0 && httpSidRef.current) {
        void flushHTTPQueue();
      }
    }
  }, []);

  const scheduleHTTPFlush = useCallback((immediate: boolean) => {
    if (immediate || httpRttMsRef.current < 250) {
      void flushHTTPQueue();
      return;
    }
    if (!httpBatchTimerRef.current && !httpFlushingRef.current) {
      // On high-RTT proxies (>250ms), wait 65ms so click down+up or bursts of keys batch into 1 HTTP call
      httpBatchTimerRef.current = setTimeout(() => {
        httpBatchTimerRef.current = null;
        void flushHTTPQueue();
      }, 65);
    }
  }, [flushHTTPQueue]);

  const flushHoverMove = useCallback(async () => {
    if (moveInFlightRef.current) return;
    moveInFlightRef.current = true;
    try {
      while (pendingMoveRef.current) {
        const sid = httpSidRef.current;
        if (!sid) {
          pendingMoveRef.current = null;
          break;
        }
        const mv = pendingMoveRef.current;
        pendingMoveRef.current = null;
        const token = getStoredToken();
        const t0 = performance.now();
        try {
          const res = await fetch(`/admin/api/webclient/send?sid=${encodeURIComponent(sid)}`, {
            method: 'POST',
            headers: {
              'Content-Type': 'application/json',
              Authorization: `Bearer ${token}`,
            },
            body: JSON.stringify(mv),
          });
          httpRttMsRef.current = performance.now() - t0;
          if (res.status === 404 || res.status === 410) {
            pendingMoveRef.current = null;
            break;
          }
        } catch {
          // ignore transient hover move errors
        }
      }
    } finally {
      moveInFlightRef.current = false;
      if (pendingMoveRef.current && httpSidRef.current) {
        void flushHoverMove();
      }
    }
  }, []);

  const sendJSON = useCallback((obj: Record<string, unknown>) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
      return;
    }
    if (httpSidRef.current) {
      const q = httpQueueRef.current;
      if (obj.type === 'mouse' && obj.evt_type === 0 && q.length > 0) {
        const prev = q[q.length - 1];
        if (prev.type === 'mouse' && prev.evt_type === 0) {
          q[q.length - 1] = obj;
          scheduleHTTPFlush(false);
          return;
        }
      }
      q.push(obj);
      const urgent = obj.type === 'login' || obj.type === 'refresh' || obj.type === 'option' || obj.type === 'ctrl_alt_del';
      scheduleHTTPFlush(urgent);
    }
  }, [scheduleHTTPFlush]);

  const sendHoverMove = useCallback((obj: Record<string, unknown>) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
      return;
    }
    if (httpSidRef.current) {
      // Over high-RTT HTTP proxies (>250ms), skip hover-only moves so clicks/keys never compete for proxy slots
      if (httpRttMsRef.current > 250) return;
      pendingMoveRef.current = obj;
      void flushHoverMove();
    }
  }, [flushHoverMove]);

  const sendBatch = useCallback((items: Record<string, unknown>[], flushNow = false) => {
    const ws = wsRef.current;
    if (ws && ws.readyState === WebSocket.OPEN) {
      for (const item of items) ws.send(JSON.stringify(item));
      return;
    }
    if (httpSidRef.current) {
      pendingMoveRef.current = null;
      httpQueueRef.current.push(...items);
      scheduleHTTPFlush(flushNow);
    }
  }, [scheduleHTTPFlush]);

  const ensureDecoder = useCallback((codecId: number) => {
    if (configuredCodecRef.current === codecId && decoderRef.current && decoderRef.current.state === 'configured') {
      return decoderRef.current;
    }
    if (typeof VideoDecoder === 'undefined') {
      setStatusMsg('Your browser does not support WebCodecs VideoDecoder (use Chrome, Edge, Safari, or Firefox 130+).');
      return null;
    }
    const spec = CODEC_MAP[codecId];
    if (!spec) return null;

    if (decoderRef.current) {
      try {
        decoderRef.current.close();
      } catch {
        // ignore
      }
      decoderRef.current = null;
    }

    const dec = new VideoDecoder({
      output: (frame: VideoFrame) => {
        const canvas = canvasRef.current;
        if (canvas) {
          if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
            canvas.width = frame.displayWidth;
            canvas.height = frame.displayHeight;
            setResolution({ w: frame.displayWidth, h: frame.displayHeight });
          }
          const ctx = canvas.getContext('2d');
          if (ctx) {
            ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
          }
        }
        frameCountRef.current += 1;
        frame.close();
      },
      error: (err) => {
        console.error('WebCodecs decode error:', err);
        gotKeyFrameRef.current = false;
      },
    });

    dec.configure({
      codec: spec.config,
      optimizeForLatency: true,
    });
    decoderRef.current = dec;
    configuredCodecRef.current = codecId;
    gotKeyFrameRef.current = false;
    setCodecName(spec.name);
    return dec;
  }, []);

  // FPS counter
  useEffect(() => {
    const t = setInterval(() => {
      setFps(frameCountRef.current);
      frameCountRef.current = 0;
    }, 1000);
    return () => clearInterval(t);
  }, []);

  // Open WebSocket session with automatic pure-HTTP fetch tunnel fallback for proxies/preview panes
  useEffect(() => {
    let disposed = false;
    let usingHTTP = false;
    let pollAbort: AbortController | null = null;

    const decodeBinaryPacket = (buf: Uint8Array) => {
      if (buf.byteLength <= 11) return;
      const codecId = buf[0];
      const isKey = (buf[1] & 1) === 1;
      const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
      const ptsBig = view.getBigInt64(3, false);
      const payload = buf.subarray(11);

      const dec = ensureDecoder(codecId);
      if (!dec || dec.state !== 'configured') return;
      if (!gotKeyFrameRef.current && !isKey) return;
      if (isKey) gotKeyFrameRef.current = true;

      try {
        const chunk = new EncodedVideoChunk({
          type: isKey ? 'key' : 'delta',
          timestamp: Number(ptsBig) * 1000,
          data: payload,
        });
        dec.decode(chunk);
      } catch (err) {
        console.warn('decode chunk error:', err);
        gotKeyFrameRef.current = false;
      }
    };

    const handleControlMsg = (msg: Record<string, unknown>) => {
      switch (msg.type) {
        case 'status':
          setStatusMsg(String(msg.message || 'Connecting...'));
          break;
        case 'auth_required':
          setPhase('auth');
          setSubmittingAuth(false);
          setStatusMsg('Remote host requires authentication');
          break;
        case 'auth_error':
          setPhase('auth');
          setSubmittingAuth(false);
          setAuthError(String(msg.message || 'Authentication failed'));
          break;
        case 'ready': {
          const info = msg.peer_info as PeerSessionInfo;
          setPeerInfo(info);
          setActiveDisplay(info.current_display || 0);
          setPhase('ready');
          setAuthError('');
          setSubmittingAuth(false);
          setStatusMsg('Connected');
          break;
        }
        case 'video':
          if (typeof msg.data === 'string') {
            const raw = atob(msg.data);
            const bytes = new Uint8Array(raw.length);
            for (let i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
            decodeBinaryPacket(bytes);
          }
          break;
        case 'clipboard':
          if (typeof msg.text === 'string') {
            setClipboardText(msg.text);
            navigator.clipboard?.writeText(msg.text).catch(() => {});
          }
          break;
        case 'error':
          setPhase('error');
          setStatusMsg(String(msg.message || 'Connection failed'));
          break;
        case 'closed':
          setPhase('closed');
          setStatusMsg(String(msg.message || 'Session closed'));
          break;
      }
    };

    const rawToken = getStoredToken();
    const tokenParam = encodeURIComponent(rawToken);

    const startHTTPTunnel = async () => {
      if (disposed || usingHTTP) return;
      usingHTTP = true;
      try {
        wsRef.current?.close();
      } catch {
        // ignore
      }
      wsRef.current = null;
      setStatusMsg('Opening HTTP encrypted tunnel...');

      try {
        const openRes = await fetch(`/admin/api/webclient/open?peer_id=${encodeURIComponent(peer.id)}`, {
          method: 'POST',
          headers: { Authorization: `Bearer ${rawToken}` },
        });
        if (!openRes.ok) {
          const txt = await openRes.text();
          if (!disposed) {
            setPhase('error');
            setStatusMsg(`Tunnel open failed (${openRes.status}): ${txt}`);
          }
          return;
        }
        const { sid } = (await openRes.json()) as { sid: string };
        if (disposed) {
          fetch(`/admin/api/webclient/close?sid=${encodeURIComponent(sid)}`, {
            method: 'POST',
            headers: { Authorization: `Bearer ${rawToken}` },
          }).catch(() => {});
          return;
        }
        httpSidRef.current = sid;

        // Binary poll loop (eliminates Base64 + JSON overhead for video frames)
        const textDec = new TextDecoder();
        let consecutiveErrors = 0;
        while (!disposed && httpSidRef.current === sid) {
          pollAbort = new AbortController();
          const pollRes = await fetch(`/admin/api/webclient/poll?sid=${encodeURIComponent(sid)}&bin=1`, {
            headers: { Authorization: `Bearer ${rawToken}` },
            signal: pollAbort.signal,
          });
          if (!pollRes.ok) {
            if (pollRes.status === 404 || consecutiveErrors >= 2) {
              if (!disposed) {
                setPhase((prev) => (prev === 'error' ? prev : 'closed'));
                setStatusMsg(
                  pollRes.status === 404
                    ? 'Session expired or server restarted'
                    : `Connection lost (HTTP ${pollRes.status})`,
                );
              }
              break;
            }
            consecutiveErrors++;
            await new Promise((r) => setTimeout(r, 350));
            continue;
          }
          consecutiveErrors = 0;
          const ab = await pollRes.arrayBuffer();
          const bytes = new Uint8Array(ab);
          const dv = new DataView(ab);
          let off = 0;
          while (off + 5 <= bytes.byteLength && !disposed) {
            const kind = bytes[off];
            const len = dv.getUint32(off + 1, false);
            off += 5;
            if (off + len > bytes.byteLength) break;
            const slice = bytes.subarray(off, off + len);
            off += len;
            if (kind === 1) {
              decodeBinaryPacket(slice);
            } else if (kind === 0) {
              try {
                handleControlMsg(JSON.parse(textDec.decode(slice)));
              } catch {
                // ignore malformed control frame
              }
            }
          }
        }
      } catch (err) {
        if (!disposed) {
          setPhase('error');
          setStatusMsg(`HTTP tunnel error: ${(err as Error).message}`);
        }
      }
    };

    let gotFirstMsg = false;
    let fallbackTimer: ReturnType<typeof setTimeout> | null = null;

    try {
      const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const wsUrl = `${proto}//${window.location.host}/admin/api/webclient/ws?peer_id=${encodeURIComponent(peer.id)}&token=${tokenParam}`;
      const ws = new WebSocket(wsUrl);
      ws.binaryType = 'arraybuffer';
      wsRef.current = ws;

      fallbackTimer = setTimeout(() => {
        if (!gotFirstMsg && !disposed) {
          startHTTPTunnel();
        }
      }, 1500);

      ws.onmessage = (ev) => {
        if (!gotFirstMsg) {
          gotFirstMsg = true;
          if (fallbackTimer) clearTimeout(fallbackTimer);
        }
        if (typeof ev.data === 'string') {
          try {
            handleControlMsg(JSON.parse(ev.data));
          } catch {
            // ignore
          }
          return;
        }
        decodeBinaryPacket(new Uint8Array(ev.data as ArrayBuffer));
      };

      ws.onerror = () => {
        if (!gotFirstMsg) {
          if (fallbackTimer) clearTimeout(fallbackTimer);
          startHTTPTunnel();
        } else {
          setPhase('error');
          setStatusMsg('WebSocket connection error');
        }
      };

      ws.onclose = () => {
        if (!gotFirstMsg && !usingHTTP && !disposed) {
          if (fallbackTimer) clearTimeout(fallbackTimer);
          startHTTPTunnel();
          return;
        }
        if (!usingHTTP) {
          setPhase((prev) => (prev === 'error' ? prev : 'closed'));
        }
      };
    } catch {
      startHTTPTunnel();
    }

    return () => {
      disposed = true;
      if (fallbackTimer) clearTimeout(fallbackTimer);
      pollAbort?.abort();
      const sid = httpSidRef.current;
      httpSidRef.current = '';
      if (sid) {
        fetch(`/admin/api/webclient/close?sid=${encodeURIComponent(sid)}`, {
          method: 'POST',
          headers: { Authorization: `Bearer ${rawToken}` },
        }).catch(() => {});
      }
      try {
        wsRef.current?.close();
      } catch {
        // ignore
      }
      if (decoderRef.current) {
        try {
          decoderRef.current.close();
        } catch {
          // ignore
        }
      }
    };
  }, [peer.id, ensureDecoder]);

  // Keyboard forwarding when session is active
  useEffect(() => {
    if (phase !== 'ready' || showClipBox) return;

    const getModifiers = (e: KeyboardEvent): string[] => {
      const mods: string[] = [];
      if (e.ctrlKey) mods.push('Control');
      if (e.altKey) mods.push('Alt');
      if (e.shiftKey) mods.push('Shift');
      if (e.metaKey) mods.push('Meta');
      return mods;
    };

    const handleKey = (down: boolean) => (e: KeyboardEvent) => {
      if ((e.target as HTMLElement)?.tagName === 'INPUT' || (e.target as HTMLElement)?.tagName === 'TEXTAREA') {
        return;
      }
      e.preventDefault();
      if (SPECIAL_KEYS.has(e.key)) {
        sendJSON({
          type: 'key',
          down,
          control_key: e.key,
          modifiers: getModifiers(e),
        });
      } else if (e.key === ' ') {
        sendJSON({
          type: 'key',
          down,
          control_key: 'Space',
          modifiers: getModifiers(e),
        });
      } else if (e.key.length === 1) {
        if (!down) return;
        const mods = e.ctrlKey || e.altKey || e.metaKey ? getModifiers(e) : [];
        sendJSON({
          type: 'key',
          down: true,
          press: true,
          unicode: e.key.codePointAt(0),
          modifiers: mods,
        });
      }
    };

    const onDown = handleKey(true);
    const onUp = handleKey(false);
    window.addEventListener('keydown', onDown);
    window.addEventListener('keyup', onUp);
    return () => {
      window.removeEventListener('keydown', onDown);
      window.removeEventListener('keyup', onUp);
    };
  }, [phase, showClipBox, sendJSON]);

  const mapPointerCoords = (e: React.MouseEvent<HTMLCanvasElement>) => {
    const canvas = canvasRef.current;
    if (!canvas || canvas.width === 0 || canvas.height === 0) return { x: 0, y: 0 };
    const rect = canvas.getBoundingClientRect();
    // Compute exact rendered video area inside canvas accounting for object-fit: contain letterboxing
    const scale = Math.min(rect.width / canvas.width, rect.height / canvas.height);
    const drawW = canvas.width * scale;
    const drawH = canvas.height * scale;
    const offsetX = (rect.width - drawW) / 2;
    const offsetY = (rect.height - drawH) / 2;
    const relX = Math.max(0, Math.min(1, (e.clientX - rect.left - offsetX) / drawW));
    const relY = Math.max(0, Math.min(1, (e.clientY - rect.top - offsetY) / drawH));
    const disp = peerInfoRef.current?.displays?.[activeDisplayRef.current];
    const originX = disp ? disp.x : 0;
    const originY = disp ? disp.y : 0;
    const targetW = disp && disp.width > 0 ? disp.width : canvas.width;
    const targetH = disp && disp.height > 0 ? disp.height : canvas.height;
    return {
      x: Math.round(originX + relX * targetW),
      y: Math.round(originY + relY * targetH),
    };
  };

  const mapMouseButton = (btn: number): number => {
    if (btn === 0) return 1; // LEFT
    if (btn === 2) return 2; // RIGHT
    if (btn === 1) return 4; // MIDDLE
    if (btn === 3) return 8; // BACK
    if (btn === 4) return 16; // FORWARD
    return 1;
  };

  const handleMouseMove = (e: React.MouseEvent<HTMLCanvasElement>) => {
    if (phase !== 'ready') return;
    if (httpSidRef.current) {
      const now = performance.now();
      if (now - lastMouseMoveRef.current < 45) return;
      lastMouseMoveRef.current = now;
    }
    const { x, y } = mapPointerCoords(e);
    const ev = { type: 'mouse', evt_type: 0, buttons: 0, x, y };
    if (e.buttons !== 0) {
      // Dragging: keep ordered with mousedown/mouseup
      sendJSON(ev);
    } else {
      sendHoverMove(ev);
    }
  };

  const handleMouseDown = (e: React.MouseEvent<HTMLCanvasElement>) => {
    if (phase !== 'ready') return;
    e.preventDefault();
    const { x, y } = mapPointerCoords(e);
    // RustDesk's MOUSE_TYPE_DOWN does not reposition the cursor; send MOVE+DOWN atomically
    sendBatch([
      { type: 'mouse', evt_type: 0, buttons: 0, x, y },
      { type: 'mouse', evt_type: 1, buttons: mapMouseButton(e.button), x, y },
    ]);
  };

  const handleMouseUp = (e: React.MouseEvent<HTMLCanvasElement>) => {
    if (phase !== 'ready') return;
    e.preventDefault();
    const { x, y } = mapPointerCoords(e);
    sendBatch(
      [
        { type: 'mouse', evt_type: 0, buttons: 0, x, y },
        { type: 'mouse', evt_type: 2, buttons: mapMouseButton(e.button), x, y },
      ],
      true,
    );
  };

  const handleWheel = (e: React.WheelEvent<HTMLCanvasElement>) => {
    if (phase !== 'ready') return;
    const dx = Math.sign(-e.deltaX);
    const dy = Math.sign(-e.deltaY);
    sendJSON({ type: 'mouse', evt_type: 3, buttons: 0, x: dx, y: dy });
  };

  const handleLoginSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setAuthError('');
    setSubmittingAuth(true);
    sendJSON({ type: 'login', password });
  };

  const toggleFullscreen = () => {
    if (!document.fullscreenElement) {
      containerRef.current?.requestFullscreen?.().then(() => setIsFullscreen(true)).catch(() => {});
    } else {
      document.exitFullscreen?.().then(() => setIsFullscreen(false)).catch(() => {});
    }
  };

  const handleSendClipboard = () => {
    sendJSON({ type: 'clipboard', text: clipboardText });
    onToast('Clipboard sent to remote host');
    setShowClipBox(false);
  };

  const displayName = peer.alias || peer.hostname || formatPeerID(peer.id);

  return (
    <div className="webclient-overlay" ref={containerRef}>
      <div className="webclient-bar">
        <div className="webclient-bar-left">
          <Monitor size={16} color="var(--accent)" />
          <strong>{displayName}</strong>
          <span className="mono" style={{ fontSize: 12, color: 'var(--ink-muted)' }}>
            {formatPeerID(peer.id)}
          </span>
          <span
            className={`badge ${
              phase === 'ready'
                ? 'badge-success'
                : phase === 'error'
                  ? 'badge-danger'
                  : 'badge-accent'
            }`}
          >
            <ShieldCheck size={12} />
            {phase === 'ready'
              ? 'E2E Encrypted'
              : phase === 'auth'
                ? 'Auth Required'
                : phase === 'error'
                  ? 'Error'
                  : phase === 'closed'
                    ? 'Closed'
                    : 'Connecting'}
          </span>
          {phase === 'ready' && codecName && (
            <span className="mono" style={{ fontSize: 11.5, color: 'var(--ink-muted)' }}>
              {codecName} · {resolution.w}×{resolution.h} · {fps} FPS
            </span>
          )}
        </div>

        <div className="webclient-bar-right">
          {phase === 'ready' && peerInfo && peerInfo.displays.length > 1 && (
            <select
              className="select"
              style={{ padding: '4px 8px', fontSize: 12 }}
              value={activeDisplay}
              onChange={(e) => {
                const idx = Number(e.target.value);
                setActiveDisplay(idx);
                sendJSON({ type: 'switch_display', display: idx });
              }}
            >
              {peerInfo.displays.map((d) => (
                <option key={d.index} value={d.index}>
                  Display {d.index + 1} ({d.width}×{d.height})
                </option>
              ))}
            </select>
          )}
          {phase === 'ready' && (
            <>
              <select
                className="select"
                style={{ padding: '4px 8px', fontSize: 12 }}
                value={streamPreset}
                title="Video codec & quality preset"
                onChange={(e) => {
                  const val = e.target.value;
                  setStreamPreset(val);
                  const [codec, quality] = val.split('-');
                  sendJSON({ type: 'option', codec, quality, fps: 30 });
                  sendJSON({ type: 'refresh' });
                }}
              >
                <option value="vp8-fast">VP8 · Fast</option>
                <option value="vp8-balanced">VP8 · Balanced</option>
                <option value="vp9-best">VP9 · Sharp</option>
                <option value="h264-balanced">H.264 · Balanced</option>
              </select>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => sendJSON({ type: 'refresh' })}
                title="Request keyframe / refresh screen"
              >
                <RefreshCw size={13} /> Refresh
              </button>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => setShowClipBox((v) => !v)}
                title="Sync clipboard text"
              >
                <ClipboardCopy size={13} /> Clipboard
              </button>
              <button
                type="button"
                className="btn btn-sm"
                onClick={() => sendJSON({ type: 'ctrl_alt_del' })}
                title="Send Ctrl+Alt+Del"
              >
                Ctrl+Alt+Del
              </button>
              <button
                type="button"
                className="btn btn-sm btn-icon"
                onClick={toggleFullscreen}
                title="Toggle Fullscreen"
              >
                {isFullscreen ? <Minimize2 size={14} /> : <Maximize2 size={14} />}
              </button>
            </>
          )}
          <button
            type="button"
            className="btn btn-sm btn-danger"
            onClick={onClose}
            title="Disconnect & close web client"
          >
            <Power size={13} /> Disconnect
          </button>
        </div>
      </div>

      <div className="webclient-stage">
        <canvas
          ref={canvasRef}
          className="webclient-canvas"
          style={{ display: phase === 'ready' ? 'block' : 'none' }}
          onMouseMove={handleMouseMove}
          onMouseDown={handleMouseDown}
          onMouseUp={handleMouseUp}
          onWheel={handleWheel}
          onContextMenu={(e) => e.preventDefault()}
        />

        {phase === 'connecting' && (
          <div className="webclient-card">
            <Monitor size={28} color="var(--accent)" />
            <div style={{ fontWeight: 600, fontSize: 15 }}>Opening Remote Session</div>
            <div style={{ fontSize: 13, color: 'var(--ink-secondary)' }}>{statusMsg}</div>
          </div>
        )}

        {phase === 'auth' && (
          <div className="webclient-card">
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <KeyRound size={20} color="var(--accent)" />
              <strong style={{ fontSize: 16 }}>Host Password Required</strong>
            </div>
            <div style={{ fontSize: 13, color: 'var(--ink-secondary)' }}>
              Enter the RustDesk password for <span className="mono">{formatPeerID(peer.id)}</span>, or leave blank to request click-to-accept approval on the host.
            </div>
            {authError && <div className="error-banner">{authError}</div>}
            <form onSubmit={handleLoginSubmit} style={{ display: 'flex', flexDirection: 'column', gap: 12, width: '100%' }}>
              <input
                className="input"
                type="password"
                autoFocus
                placeholder="RustDesk session or permanent password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
              <div style={{ display: 'flex', gap: 8 }}>
                <button type="submit" className="btn btn-primary" style={{ flex: 1 }} disabled={submittingAuth}>
                  {submittingAuth ? 'Authenticating...' : 'Connect'}
                </button>
                <button type="button" className="btn" onClick={onClose}>
                  Cancel
                </button>
              </div>
            </form>
          </div>
        )}

        {(phase === 'error' || phase === 'closed') && (
          <div className="webclient-card">
            <div style={{ fontWeight: 700, fontSize: 15, color: phase === 'error' ? 'var(--danger)' : 'var(--ink-primary)' }}>
              {phase === 'error' ? 'Connection Failed' : 'Session Disconnected'}
            </div>
            <div style={{ fontSize: 13, color: 'var(--ink-secondary)' }}>{statusMsg}</div>
            <button type="button" className="btn btn-primary" onClick={onClose}>
              Return to Address Book
            </button>
          </div>
        )}

        {showClipBox && phase === 'ready' && (
          <div className="webclient-clip-popover">
            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
              <strong style={{ fontSize: 13 }}>Remote Clipboard</strong>
              <button type="button" className="btn btn-ghost btn-icon" onClick={() => setShowClipBox(false)}>
                <X size={14} />
              </button>
            </div>
            <textarea
              className="textarea"
              rows={4}
              value={clipboardText}
              onChange={(e) => setClipboardText(e.target.value)}
              placeholder="Paste text to send to remote host..."
            />
            <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 6, marginTop: 8 }}>
              <button type="button" className="btn btn-sm" onClick={() => setShowClipBox(false)}>
                Close
              </button>
              <button type="button" className="btn btn-primary btn-sm" onClick={handleSendClipboard}>
                Send to Host
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
};
