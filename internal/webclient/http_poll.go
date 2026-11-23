package webclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/KriszW/godesk-plane/internal/pb"
	"github.com/google/uuid"
)

type httpEvent struct {
	isVideo bool
	raw     []byte
	jsonEv  map[string]any
}

type httpSession struct {
	id        string
	sess      *sseSession
	cancel    context.CancelFunc
	mu        sync.Mutex
	queue     []httpEvent
	notify    chan struct{}
	lastTouch time.Time
	done      bool
}

var (
	httpSessionsMu sync.RWMutex
	httpSessions   = make(map[string]*httpSession)
)

func (h *httpSession) pushEvent(ev httpEvent) {
	h.mu.Lock()
	// Bound queued video frames so slow browsers stay live on latest frames
	if len(h.queue) > 64 {
		kept := make([]httpEvent, 0, 32)
		for _, item := range h.queue {
			if !item.isVideo {
				kept = append(kept, item)
			}
		}
		h.queue = kept
	}
	h.queue = append(h.queue, ev)
	h.mu.Unlock()
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

func (h *httpSession) push(ev map[string]any) {
	h.pushEvent(httpEvent{jsonEv: ev})
}

func (h *httpSession) pushVideo(pkt []byte) {
	h.pushEvent(httpEvent{isVideo: true, raw: pkt})
}

func (h *httpSession) drain() []httpEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastTouch = time.Now()
	if len(h.queue) == 0 {
		return nil
	}
	out := h.queue
	h.queue = nil
	return out
}

// HandleHTTPOpen starts a new RustDesk WebClient session accessible over standard HTTP fetch() polling.
func HandleHTTPOpen(w http.ResponseWriter, r *http.Request, cfg Config, peerID, actorName string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sid := uuid.NewString()
	ctx, cancel := context.WithCancel(context.Background())

	hs := &httpSession{
		id:        sid,
		cancel:    cancel,
		notify:    make(chan struct{}, 1),
		lastTouch: time.Now(),
	}

	httpSessionsMu.Lock()
	httpSessions[sid] = hs
	httpSessionsMu.Unlock()

	slog.InfoContext(r.Context(), "starting webclient HTTP tunnel session",
		"component", "webclient", "peer_id", peerID, "actor", actorName, "sid", sid)

	hs.push(map[string]any{"type": "status", "phase": "signaling", "message": "Connecting via rendezvous server..."})

	go runHTTPSession(ctx, hs, cfg, peerID, actorName)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"sid": sid})
}

func runHTTPSession(ctx context.Context, hs *httpSession, cfg Config, peerID, actorName string) {
	defer func() {
		hs.mu.Lock()
		hs.done = true
		hs.mu.Unlock()
		select {
		case hs.notify <- struct{}{}:
		default:
		}
		time.AfterFunc(30*time.Second, func() {
			httpSessionsMu.Lock()
			delete(httpSessions, hs.id)
			httpSessionsMu.Unlock()
		})
	}()

	stream, err := dialPeerViaRelay(ctx, cfg, peerID)
	if err != nil {
		slog.Warn("webclient HTTP tunnel connect failed", "component", "webclient", "peer_id", peerID, "sid", hs.id, "err", err)
		hs.push(map[string]any{"type": "error", "message": err.Error()})
		return
	}
	defer stream.conn.Close()

	sess := &sseSession{
		peerID:    peerID,
		actorName: actorName,
		sessionID: uint64(time.Now().UnixNano()),
		stream:    stream,
		inCh:      make(chan wsClientMsg, 256),
		closed:    make(chan struct{}),
	}
	hs.mu.Lock()
	hs.sess = sess
	hs.mu.Unlock()
	defer close(sess.closed)

	hs.push(map[string]any{"type": "status", "phase": "connected_relay", "message": "Encrypted tunnel established, waiting for host challenge..."})

	// Idle reaper (closes session if browser stops polling for 20s)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				hs.mu.Lock()
				idle := time.Since(hs.lastTouch)
				hs.mu.Unlock()
				if idle > 20*time.Second {
					slog.Info("webclient HTTP tunnel idle timeout", "component", "webclient", "peer_id", peerID, "sid", hs.id)
					hs.cancel()
					_ = stream.conn.Close()
					return
				}
			}
		}
	}()

	// Input pump
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case cm := <-sess.inCh:
				sess.handleClientMsg(cm)
			}
		}
	}()

	// Host -> queue pump
	for {
		msg, err := stream.readMsg(0)
		if err != nil {
			if ctx.Err() == nil {
				slog.Info("webclient HTTP tunnel host closed", "component", "webclient", "peer_id", peerID, "sid", hs.id, "err", err)
				hs.push(map[string]any{"type": "closed", "message": "Remote host closed the connection"})
			}
			return
		}
		switch u := msg.Union.(type) {
		case *pb.Message_Hash:
			if u.Hash != nil {
				sess.hashMu.Lock()
				sess.hash = u.Hash
				sess.hashMu.Unlock()
				hs.push(map[string]any{
					"type":      "auth_required",
					"salt":      u.Hash.Salt,
					"challenge": u.Hash.Challenge,
				})
			}

		case *pb.Message_LoginResponse:
			lr := u.LoginResponse
			if lr == nil {
				continue
			}
			if errStr := lr.GetError(); errStr != "" {
				hs.push(map[string]any{"type": "auth_error", "message": errStr})
			} else if pi := lr.GetPeerInfo(); pi != nil {
				displays := make([]map[string]any, 0, len(pi.Displays))
				for i, d := range pi.Displays {
					displays = append(displays, map[string]any{
						"index":  i,
						"x":      d.X,
						"y":      d.Y,
						"width":  d.Width,
						"height": d.Height,
						"name":   d.Name,
						"online": d.Online,
						"scale":  d.Scale,
					})
				}
				hs.push(map[string]any{
					"type": "ready",
					"peer_info": map[string]any{
						"username":        pi.Username,
						"hostname":        pi.Hostname,
						"platform":        pi.Platform,
						"version":         pi.Version,
						"current_display": pi.CurrentDisplay,
						"displays":        displays,
					},
				})
				slog.Info("webclient HTTP tunnel authenticated to peer",
					"component", "webclient", "peer_id", peerID, "actor", actorName, "hostname", pi.Hostname)
			}

		case *pb.Message_VideoFrame:
			vf := u.VideoFrame
			if vf == nil {
				continue
			}
			var codec byte
			var frames []*pb.EncodedVideoFrame
			switch vu := vf.Union.(type) {
			case *pb.VideoFrame_Vp9S:
				codec = codecVP9
				frames = vu.Vp9S.GetFrames()
			case *pb.VideoFrame_Vp8S:
				codec = codecVP8
				frames = vu.Vp8S.GetFrames()
			case *pb.VideoFrame_H264S:
				codec = codecH264
				frames = vu.H264S.GetFrames()
			case *pb.VideoFrame_Av1S:
				codec = codecAV1
				frames = vu.Av1S.GetFrames()
			case *pb.VideoFrame_H265S:
				codec = codecH265
				frames = vu.H265S.GetFrames()
			}
			for _, f := range frames {
				if len(f.Data) == 0 {
					continue
				}
				pkt := make([]byte, 11+len(f.Data))
				pkt[0] = codec
				if f.Key {
					pkt[1] = 1
				}
				pkt[2] = byte(vf.Display)
				binary.BigEndian.PutUint64(pkt[3:11], uint64(f.Pts))
				copy(pkt[11:], f.Data)
				hs.pushVideo(pkt)
			}

		case *pb.Message_TestDelay:
			if td := u.TestDelay; td != nil {
				_ = stream.writeMsg(&pb.Message{
					Union: &pb.Message_TestDelay{
						TestDelay: &pb.TestDelay{Time: td.Time, FromClient: false, LastDelay: td.LastDelay},
					},
				})
			}

		case *pb.Message_Clipboard:
			if cb := u.Clipboard; cb != nil && cb.Format == pb.ClipboardFormat_Text {
				hs.push(map[string]any{"type": "clipboard", "text": string(cb.Content)})
			}

		case *pb.Message_Misc:
			if m := u.Misc; m != nil {
				if reason := m.GetCloseReason(); reason != "" {
					hs.push(map[string]any{"type": "error", "message": reason})
				}
			}
		}
	}
}

// HandleHTTPPoll returns queued WebClient events immediately, or waits up to 250ms for new events.
func HandleHTTPPoll(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	httpSessionsMu.RLock()
	hs := httpSessions[sid]
	httpSessionsMu.RUnlock()
	if hs == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	events := hs.drain()
	if len(events) == 0 {
		timer := time.NewTimer(4 * time.Second)
		defer timer.Stop()
		select {
		case <-hs.notify:
			events = hs.drain()
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}

	if r.URL.Query().Get("bin") == "1" {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		var hdr [5]byte
		for _, ev := range events {
			if ev.isVideo {
				hdr[0] = 1
				binary.BigEndian.PutUint32(hdr[1:5], uint32(len(ev.raw)))
				_, _ = w.Write(hdr[:])
				_, _ = w.Write(ev.raw)
			} else {
				b, err := json.Marshal(ev.jsonEv)
				if err != nil {
					continue
				}
				hdr[0] = 0
				binary.BigEndian.PutUint32(hdr[1:5], uint32(len(b)))
				_, _ = w.Write(hdr[:])
				_, _ = w.Write(b)
			}
		}
		return
	}

	out := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		if ev.isVideo {
			out = append(out, map[string]any{
				"type": "video",
				"data": base64.StdEncoding.EncodeToString(ev.raw),
			})
		} else {
			out = append(out, ev.jsonEv)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": out})
}

// HandleHTTPSend forwards a browser input/login command to either an HTTP tunnel or SSE session.
func HandleHTTPSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if sid := r.URL.Query().Get("sid"); sid != "" {
		httpSessionsMu.RLock()
		hs := httpSessions[sid]
		httpSessionsMu.RUnlock()
		if hs == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		hs.mu.Lock()
		hs.lastTouch = time.Now()
		sess := hs.sess
		hs.mu.Unlock()
		if sess == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var msgs []wsClientMsg
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			if err := json.Unmarshal(trimmed, &msgs); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		} else {
			var cm wsClientMsg
			if err := json.Unmarshal(trimmed, &cm); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			msgs = []wsClientMsg{cm}
		}
		for _, cm := range msgs {
			select {
			case sess.inCh <- cm:
			case <-sess.closed:
				w.WriteHeader(http.StatusGone)
				return
			default:
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	HandleSSESend(w, r)
}

// HandleHTTPClose terminates an active HTTP tunnel WebClient session.
func HandleHTTPClose(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	httpSessionsMu.Lock()
	hs := httpSessions[sid]
	delete(httpSessions, sid)
	httpSessionsMu.Unlock()
	if hs != nil {
		hs.cancel()
		hs.mu.Lock()
		if hs.sess != nil && hs.sess.stream != nil {
			_ = hs.sess.stream.conn.Close()
		}
		hs.mu.Unlock()
	}
	w.WriteHeader(http.StatusNoContent)
}
