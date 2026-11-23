package webclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/KriszW/godesk-plane/internal/pb"
	"github.com/google/uuid"
)

type sseSession struct {
	peerID    string
	actorName string
	sessionID uint64
	stream    *secureStream
	hashMu    sync.Mutex
	hash      *pb.Hash
	inCh      chan wsClientMsg
	closed    chan struct{}
}

var (
	sseSessionsMu sync.RWMutex
	sseSessions   = make(map[string]*sseSession)
)

// ServeSSE provides an HTTP Server-Sent Events fallback transport for environments/proxies
// that do not forward WebSocket upgrades.
func ServeSSE(w http.ResponseWriter, r *http.Request, cfg Config, peerID, actorName string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var writeMu sync.Mutex
	sendEvent := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	sid := uuid.NewString()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	slog.InfoContext(ctx, "starting webclient SSE session", "component", "webclient", "peer_id", peerID, "actor", actorName, "sse_id", sid)
	_ = sendEvent(map[string]any{"type": "session", "sse_id": sid})
	_ = sendEvent(map[string]any{"type": "status", "phase": "signaling", "message": "Connecting via rendezvous server..."})

	stream, err := dialPeerViaRelay(ctx, cfg, peerID)
	if err != nil {
		slog.WarnContext(ctx, "webclient SSE connect failed", "component", "webclient", "peer_id", peerID, "err", err)
		_ = sendEvent(map[string]any{"type": "error", "message": err.Error()})
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
	sseSessionsMu.Lock()
	sseSessions[sid] = sess
	sseSessionsMu.Unlock()
	defer func() {
		close(sess.closed)
		sseSessionsMu.Lock()
		delete(sseSessions, sid)
		sseSessionsMu.Unlock()
	}()

	_ = sendEvent(map[string]any{"type": "status", "phase": "connected_relay", "message": "Encrypted tunnel established, waiting for host challenge..."})

	// Browser POST commands -> Host pump
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

	// Host -> Browser SSE pump
	hostErrCh := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.readMsg(0)
			if err != nil {
				hostErrCh <- err
				return
			}
			switch u := msg.Union.(type) {
			case *pb.Message_Hash:
				if u.Hash != nil {
					sess.hashMu.Lock()
					sess.hash = u.Hash
					sess.hashMu.Unlock()
					_ = sendEvent(map[string]any{
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
					_ = sendEvent(map[string]any{"type": "auth_error", "message": errStr})
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
					_ = sendEvent(map[string]any{
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
					slog.InfoContext(ctx, "webclient SSE authenticated to peer",
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
					if err := sendEvent(map[string]any{
						"type": "video",
						"data": base64.StdEncoding.EncodeToString(pkt),
					}); err != nil {
						hostErrCh <- err
						return
					}
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
					_ = sendEvent(map[string]any{"type": "clipboard", "text": string(cb.Content)})
				}

			case *pb.Message_Misc:
				if m := u.Misc; m != nil {
					if reason := m.GetCloseReason(); reason != "" {
						_ = sendEvent(map[string]any{"type": "error", "message": reason})
					}
				}
			}
		}
	}()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-hostErrCh:
			slog.InfoContext(ctx, "webclient SSE host closed", "component", "webclient", "peer_id", peerID, "err", err)
			_ = sendEvent(map[string]any{"type": "closed", "message": "Remote host closed the connection"})
			return
		case <-keepalive.C:
			writeMu.Lock()
			_, _ = fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
			writeMu.Unlock()
		}
	}
}

func (sess *sseSession) handleClientMsg(cm wsClientMsg) {
	switch cm.Type {
	case "login":
		sess.hashMu.Lock()
		h := sess.hash
		sess.hashMu.Unlock()
		var pwdHash []byte
		if h != nil && cm.Password != "" {
			s1 := sha256.New()
			s1.Write([]byte(cm.Password))
			s1.Write([]byte(h.Salt))
			step1 := s1.Sum(nil)

			s2 := sha256.New()
			s2.Write(step1)
			s2.Write([]byte(h.Challenge))
			pwdHash = s2.Sum(nil)
		}
		lr := &pb.LoginRequest{
			Username:   sess.peerID,
			Password:   pwdHash,
			MyId:       "godesk-web",
			MyName:     sess.actorName,
			SessionId:  sess.sessionID,
			Version:    "1.5.3",
			MyPlatform: "Linux",
			Option:     buildOptionMessage(cm.Quality, cm.Codec, cm.FPS),
		}
		_ = sess.stream.writeMsg(&pb.Message{Union: &pb.Message_LoginRequest{LoginRequest: lr}})

	case "option":
		_ = sess.stream.writeMsg(&pb.Message{
			Union: &pb.Message_Misc{
				Misc: &pb.Misc{
					Union: &pb.Misc_Option{
						Option: buildOptionMessage(cm.Quality, cm.Codec, cm.FPS),
					},
				},
			},
		})

	case "mouse":
		mask := (cm.Buttons << 3) | (cm.EvtType & 0x7)
		_ = sess.stream.writeMsg(&pb.Message{
			Union: &pb.Message_MouseEvent{
				MouseEvent: &pb.MouseEvent{Mask: mask, X: cm.X, Y: cm.Y},
			},
		})

	case "key":
		ke := &pb.KeyEvent{Down: cm.Down, Press: cm.Press, Mode: pb.KeyboardMode_Legacy}
		for _, m := range cm.Modifiers {
			if ck := mapControlKey(m); ck != pb.ControlKey_Unknown {
				ke.Modifiers = append(ke.Modifiers, ck)
			}
		}
		if cm.ControlKey != "" {
			if ck := mapControlKey(cm.ControlKey); ck != pb.ControlKey_Unknown {
				ke.Union = &pb.KeyEvent_ControlKey{ControlKey: ck}
			}
		} else if cm.Unicode > 0 {
			if !cm.Down && !cm.Press {
				return
			}
			ke.Down = true
			ke.Press = true
			ke.Union = &pb.KeyEvent_Unicode{Unicode: cm.Unicode}
		}
		if ke.Union != nil {
			_ = sess.stream.writeMsg(&pb.Message{Union: &pb.Message_KeyEvent{KeyEvent: ke}})
		}

	case "ctrl_alt_del":
		_ = sess.stream.writeMsg(&pb.Message{
			Union: &pb.Message_KeyEvent{
				KeyEvent: &pb.KeyEvent{Down: true, Union: &pb.KeyEvent_ControlKey{ControlKey: pb.ControlKey_CtrlAltDel}},
			},
		})

	case "switch_display":
		_ = sess.stream.writeMsg(&pb.Message{
			Union: &pb.Message_Misc{Misc: &pb.Misc{Union: &pb.Misc_SwitchDisplay{SwitchDisplay: &pb.SwitchDisplay{Display: cm.Display}}}},
		})

	case "refresh":
		_ = sess.stream.writeMsg(&pb.Message{
			Union: &pb.Message_Misc{Misc: &pb.Misc{Union: &pb.Misc_RefreshVideo{RefreshVideo: true}}},
		})

	case "clipboard":
		_ = sess.stream.writeMsg(&pb.Message{
			Union: &pb.Message_Clipboard{Clipboard: &pb.Clipboard{Content: []byte(cm.Text), Format: pb.ClipboardFormat_Text}},
		})
	}
}

// HandleSSESend receives browser input/login commands for an active SSE WebClient session.
func HandleSSESend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	sid := r.URL.Query().Get("sse_id")
	sseSessionsMu.RLock()
	sess := sseSessions[sid]
	sseSessionsMu.RUnlock()
	if sess == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var cm wsClientMsg
	if err := json.Unmarshal(body, &cm); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	select {
	case sess.inCh <- cm:
		w.WriteHeader(http.StatusNoContent)
	case <-sess.closed:
		w.WriteHeader(http.StatusGone)
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}
