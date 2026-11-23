package webclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/KriszW/godesk-plane/internal/crypto"
	"github.com/KriszW/godesk-plane/internal/pb"
	"github.com/KriszW/godesk-plane/internal/protocol"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/nacl/box"
	"golang.org/x/crypto/nacl/secretbox"
	"google.golang.org/protobuf/proto"
)

const (
	codecVP9  byte = 1
	codecVP8  byte = 2
	codecH264 byte = 3
	codecAV1  byte = 4
	codecH265 byte = 5
)

// Config configures how the built-in WebClient connects to local Rendezvous and Relay listeners.
type Config struct {
	RendezvousAddr string // e.g. 127.0.0.1:21116
	RelayAddr      string // e.g. 127.0.0.1:21117
	RelayAdvertise string // host[:port] sent in RequestRelay (e.g. 127.0.0.1)
	KeyPair        *crypto.KeyPair
}

type wsClientMsg struct {
	Type       string   `json:"type"`
	Password   string   `json:"password,omitempty"`
	OSUser     string   `json:"os_username,omitempty"`
	OSPass     string   `json:"os_password,omitempty"`
	EvtType    int32    `json:"evt_type,omitempty"`
	Buttons    int32    `json:"buttons,omitempty"`
	X          int32    `json:"x,omitempty"`
	Y          int32    `json:"y,omitempty"`
	Down       bool     `json:"down,omitempty"`
	Press      bool     `json:"press,omitempty"`
	ControlKey string   `json:"control_key,omitempty"`
	Unicode    uint32   `json:"unicode,omitempty"`
	Modifiers  []string `json:"modifiers,omitempty"`
	Display    int32    `json:"display,omitempty"`
	Text       string   `json:"text,omitempty"`
	Quality    string   `json:"quality,omitempty"`
	Codec      string   `json:"codec,omitempty"`
	FPS        int32    `json:"fps,omitempty"`
}

// secureStream wraps a RustDesk BytesCodec TCP connection with optional NaCl secretbox encryption.
type secureStream struct {
	conn    net.Conn
	wmu     sync.Mutex
	key     *[32]byte
	sendSeq uint64
	recvSeq uint64
}

func (s *secureStream) setKey(k [32]byte) {
	s.key = &k
	s.sendSeq = 0
	s.recvSeq = 0
}

func makeNonce(seq uint64) [24]byte {
	var nonce [24]byte
	binary.LittleEndian.PutUint64(nonce[:8], seq)
	return nonce
}

func (s *secureStream) writeMsg(msg *pb.Message) error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.key != nil {
		s.sendSeq++
		nonce := makeNonce(s.sendSeq)
		raw = secretbox.Seal(nil, raw, &nonce, s.key)
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return protocol.WriteFrame(s.conn, raw)
}

func (s *secureStream) readMsg(timeout time.Duration) (*pb.Message, error) {
	if timeout > 0 {
		_ = s.conn.SetReadDeadline(time.Now().Add(timeout))
	} else {
		_ = s.conn.SetReadDeadline(time.Time{})
	}
	raw, err := protocol.ReadFrame(s.conn, 16*1024*1024)
	if err != nil {
		return nil, err
	}
	if s.key != nil && len(raw) > 1 {
		s.recvSeq++
		nonce := makeNonce(s.recvSeq)
		opened, ok := secretbox.Open(nil, raw, &nonce, s.key)
		if !ok {
			return nil, errors.New("secretbox decryption failed")
		}
		raw = opened
	}
	var msg pb.Message
	if err := proto.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}
	return &msg, nil
}

// ServeWS upgrades an authenticated browser request to a live RustDesk Web Client session.
func ServeWS(w http.ResponseWriter, r *http.Request, cfg Config, peerID, actorName string) {
	upgrader := websocket.Upgrader{
		CheckOrigin:     func(r *http.Request) bool { return true },
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 256 * 1024,
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.WarnContext(r.Context(), "webclient ws upgrade failed", "component", "webclient", "peer_id", peerID, "err", err)
		return
	}
	defer ws.Close()

	var wsWriteMu sync.Mutex
	sendJSON := func(v any) error {
		wsWriteMu.Lock()
		defer wsWriteMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return ws.WriteJSON(v)
	}
	sendBinary := func(b []byte) error {
		wsWriteMu.Lock()
		defer wsWriteMu.Unlock()
		_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
		return ws.WriteMessage(websocket.BinaryMessage, b)
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	slog.InfoContext(ctx, "starting webclient session", "component", "webclient", "peer_id", peerID, "actor", actorName)
	_ = sendJSON(map[string]any{"type": "status", "phase": "signaling", "message": "Connecting via rendezvous server..."})

	stream, err := dialPeerViaRelay(ctx, cfg, peerID)
	if err != nil {
		slog.WarnContext(ctx, "webclient rendezvous/relay connect failed", "component", "webclient", "peer_id", peerID, "err", err)
		_ = sendJSON(map[string]any{"type": "error", "message": err.Error()})
		return
	}
	defer stream.conn.Close()

	_ = sendJSON(map[string]any{"type": "status", "phase": "connected_relay", "message": "Encrypted tunnel established, waiting for host challenge..."})

	// Shared auth challenge state
	var hashMu sync.Mutex
	var currentHash *pb.Hash
	sessionID := uint64(time.Now().UnixNano())

	// Host -> Browser pump
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
					hashMu.Lock()
					currentHash = u.Hash
					hashMu.Unlock()
					_ = sendJSON(map[string]any{
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
					_ = sendJSON(map[string]any{"type": "auth_error", "message": errStr})
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
					_ = sendJSON(map[string]any{
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
					slog.InfoContext(ctx, "webclient authenticated to peer",
						"component", "webclient", "peer_id", peerID, "actor", actorName, "hostname", pi.Hostname, "displays", len(displays))
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
					if err := sendBinary(pkt); err != nil {
						hostErrCh <- err
						return
					}
				}

			case *pb.Message_TestDelay:
				if td := u.TestDelay; td != nil {
					_ = stream.writeMsg(&pb.Message{
						Union: &pb.Message_TestDelay{
							TestDelay: &pb.TestDelay{
								Time:       td.Time,
								FromClient: false,
								LastDelay:  td.LastDelay,
							},
						},
					})
				}

			case *pb.Message_Clipboard:
				if cb := u.Clipboard; cb != nil && cb.Format == pb.ClipboardFormat_Text {
					_ = sendJSON(map[string]any{
						"type": "clipboard",
						"text": string(cb.Content),
					})
				}

			case *pb.Message_Misc:
				if m := u.Misc; m != nil {
					if reason := m.GetCloseReason(); reason != "" {
						_ = sendJSON(map[string]any{"type": "error", "message": reason})
					}
				}
			}
		}
	}()

	// Browser -> Host pump
	browserErrCh := make(chan error, 1)
	go func() {
		for {
			mt, data, err := ws.ReadMessage()
			if err != nil {
				browserErrCh <- err
				return
			}
			if mt != websocket.TextMessage {
				continue
			}
			var cm wsClientMsg
			if err := json.Unmarshal(data, &cm); err != nil {
				continue
			}

			switch cm.Type {
			case "login":
				hashMu.Lock()
				h := currentHash
				hashMu.Unlock()
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
					Username:   peerID,
					Password:   pwdHash,
					MyId:       "godesk-web",
					MyName:     actorName,
					SessionId:  sessionID,
					Version:    "1.5.3",
					MyPlatform: "Linux",
					Option: buildOptionMessage(cm.Quality, cm.Codec, cm.FPS),
				}
				if cm.OSUser != "" || cm.OSPass != "" {
					lr.OsLogin = &pb.OSLogin{Username: cm.OSUser, Password: cm.OSPass}
				}
				if err := stream.writeMsg(&pb.Message{
					Union: &pb.Message_LoginRequest{LoginRequest: lr},
				}); err != nil {
					browserErrCh <- err
					return
				}

			case "mouse":
				mask := (cm.Buttons << 3) | (cm.EvtType & 0x7)
				_ = stream.writeMsg(&pb.Message{
					Union: &pb.Message_MouseEvent{
						MouseEvent: &pb.MouseEvent{
							Mask: mask,
							X:    cm.X,
							Y:    cm.Y,
						},
					},
				})

			case "key":
				ke := &pb.KeyEvent{
					Down:  cm.Down,
					Press: cm.Press,
					Mode:  pb.KeyboardMode_Legacy,
				}
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
						continue
					}
					ke.Down = true
					ke.Press = true
					ke.Union = &pb.KeyEvent_Unicode{Unicode: cm.Unicode}
				}
				if ke.Union != nil {
					_ = stream.writeMsg(&pb.Message{Union: &pb.Message_KeyEvent{KeyEvent: ke}})
				}

			case "ctrl_alt_del":
				_ = stream.writeMsg(&pb.Message{
					Union: &pb.Message_KeyEvent{
						KeyEvent: &pb.KeyEvent{
							Down:  true,
							Union: &pb.KeyEvent_ControlKey{ControlKey: pb.ControlKey_CtrlAltDel},
						},
					},
				})

			case "switch_display":
				_ = stream.writeMsg(&pb.Message{
					Union: &pb.Message_Misc{
						Misc: &pb.Misc{
							Union: &pb.Misc_SwitchDisplay{
								SwitchDisplay: &pb.SwitchDisplay{Display: cm.Display},
							},
						},
					},
				})

			case "refresh":
				_ = stream.writeMsg(&pb.Message{
					Union: &pb.Message_Misc{
						Misc: &pb.Misc{
							Union: &pb.Misc_RefreshVideo{RefreshVideo: true},
						},
					},
				})

			case "clipboard":
				_ = stream.writeMsg(&pb.Message{
					Union: &pb.Message_Clipboard{
						Clipboard: &pb.Clipboard{
							Content: []byte(cm.Text),
							Format:  pb.ClipboardFormat_Text,
						},
					},
				})

			case "option":
				_ = stream.writeMsg(&pb.Message{
					Union: &pb.Message_Misc{
						Misc: &pb.Misc{
							Union: &pb.Misc_Option{
								Option: buildOptionMessage(cm.Quality, cm.Codec, cm.FPS),
							},
						},
					},
				})
			}
		}
	}()

	select {
	case err := <-hostErrCh:
		slog.InfoContext(ctx, "webclient host stream ended", "component", "webclient", "peer_id", peerID, "err", err)
		_ = sendJSON(map[string]any{"type": "closed", "message": "Remote host closed the connection"})
	case <-browserErrCh:
		slog.InfoContext(ctx, "webclient browser disconnected", "component", "webclient", "peer_id", peerID)
	case <-ctx.Done():
	}
}

// dialPeerViaRelay signals target peerID through our Rendezvous server, pairs via Relay,
// and completes the RustDesk Ed25519 + X25519 + NaCl secretbox handshake.
func dialPeerViaRelay(ctx context.Context, cfg Config, peerID string) (*secureStream, error) {
	rdvConn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", cfg.RendezvousAddr)
	if err != nil {
		return nil, fmt.Errorf("connect rendezvous: %w", err)
	}
	defer rdvConn.Close()

	key := ""
	if cfg.KeyPair != nil {
		key = cfg.KeyPair.PublicB64
	}

	// 1. Send PunchHoleRequest with ForceRelay=true
	phBytes, err := proto.Marshal(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_PunchHoleRequest{
			PunchHoleRequest: &pb.PunchHoleRequest{
				Id:         peerID,
				NatType:    pb.NatType_SYMMETRIC,
				LicenceKey: key,
				ConnType:   pb.ConnType_DEFAULT_CONN,
				Version:    "1.5.3",
				ForceRelay: true,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	_ = rdvConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteFrame(rdvConn, phBytes); err != nil {
		return nil, fmt.Errorf("send PunchHoleRequest: %w", err)
	}

	// 2. Read response (either PunchHoleResponse or directly RelayResponse)
	_ = rdvConn.SetReadDeadline(time.Now().Add(15 * time.Second))
	respRaw, err := protocol.ReadFrame(rdvConn, protocol.DefaultMaxPacketLength)
	if err != nil {
		return nil, fmt.Errorf("waiting for peer response timed out: %w", err)
	}
	var respMsg pb.RendezvousMessage
	if err := proto.Unmarshal(respRaw, &respMsg); err != nil {
		return nil, fmt.Errorf("decode rendezvous response: %w", err)
	}

	var relayUUID string
	var signedPeerIDPK []byte

	switch u := respMsg.Union.(type) {
	case *pb.RendezvousMessage_RelayResponse:
		rr := u.RelayResponse
		if rr.RefuseReason != "" {
			return nil, fmt.Errorf("peer refused connection: %s", rr.RefuseReason)
		}
		relayUUID = rr.Uuid
		signedPeerIDPK = rr.GetPk()

	case *pb.RendezvousMessage_PunchHoleResponse:
		phr := u.PunchHoleResponse
		if phr.OtherFailure != "" {
			return nil, errors.New(phr.OtherFailure)
		}
		if phr.SocketAddr == nil && len(phr.Pk) == 0 {
			switch phr.Failure {
			case pb.PunchHoleResponse_ID_NOT_EXIST:
				return nil, fmt.Errorf("peer %s does not exist", peerID)
			case pb.PunchHoleResponse_OFFLINE:
				return nil, fmt.Errorf("peer %s is offline", peerID)
			case pb.PunchHoleResponse_LICENSE_MISMATCH:
				return nil, errors.New("server key mismatch")
			default:
				return nil, fmt.Errorf("connection failed (%s)", phr.Failure.String())
			}
		}
		signedPeerIDPK = phr.Pk

		// Request relay explicitly
		relayUUID = uuid.NewString()
		relayHost := phr.RelayServer
		if relayHost == "" {
			relayHost = cfg.RelayAdvertise
		}
		rrReqBytes, _ := proto.Marshal(&pb.RendezvousMessage{
			Union: &pb.RendezvousMessage_RequestRelay{
				RequestRelay: &pb.RequestRelay{
					Id:          peerID,
					Uuid:        relayUUID,
					RelayServer: relayHost,
					Secure:      true,
					LicenceKey:  key,
					ConnType:    pb.ConnType_DEFAULT_CONN,
				},
			},
		})
		_ = rdvConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if err := protocol.WriteFrame(rdvConn, rrReqBytes); err != nil {
			return nil, fmt.Errorf("send RequestRelay: %w", err)
		}
		_ = rdvConn.SetReadDeadline(time.Now().Add(15 * time.Second))
		rrRaw, err := protocol.ReadFrame(rdvConn, protocol.DefaultMaxPacketLength)
		if err != nil {
			return nil, fmt.Errorf("waiting for RelayResponse: %w", err)
		}
		var rrMsg pb.RendezvousMessage
		if err := proto.Unmarshal(rrRaw, &rrMsg); err != nil {
			return nil, err
		}
		if rr := rrMsg.GetRelayResponse(); rr != nil {
			if rr.RefuseReason != "" {
				return nil, fmt.Errorf("peer refused relay: %s", rr.RefuseReason)
			}
			if rr.Uuid != "" {
				relayUUID = rr.Uuid
			}
			if len(rr.GetPk()) > 0 {
				signedPeerIDPK = rr.GetPk()
			}
		}
	default:
		return nil, errors.New("unexpected rendezvous response from server")
	}

	if relayUUID == "" {
		return nil, errors.New("empty relay UUID returned by peer")
	}

	// Verify server-signed IdPk to get the host's Ed25519 identity public key
	var hostSignPK []byte
	if cfg.KeyPair != nil && len(signedPeerIDPK) > 0 {
		if unwrapped, ok := crypto.VerifyMessage(cfg.KeyPair.PublicKey, signedPeerIDPK); ok {
			var idpk pb.IdPk
			if proto.Unmarshal(unwrapped, &idpk) == nil && idpk.Id == peerID {
				hostSignPK = idpk.Pk
			}
		}
	}

	// 3. Connect to local Relay server and pair with UUID
	relConn, err := (&net.Dialer{Timeout: 8 * time.Second}).DialContext(ctx, "tcp", cfg.RelayAddr)
	if err != nil {
		return nil, fmt.Errorf("connect relay: %w", err)
	}
	if tc, ok := relConn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	reqRelayBytes, _ := proto.Marshal(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RequestRelay{
			RequestRelay: &pb.RequestRelay{
				Uuid:       relayUUID,
				LicenceKey: key,
			},
		},
	})
	_ = relConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteFrame(relConn, reqRelayBytes); err != nil {
		relConn.Close()
		return nil, fmt.Errorf("relay handshake send: %w", err)
	}

	ss := &secureStream{conn: relConn}

	// 4. Receive SignedId from host, perform X25519 box seal of symmetric key
	firstMsg, err := ss.readMsg(15 * time.Second)
	if err != nil {
		relConn.Close()
		return nil, fmt.Errorf("waiting for host SignedId: %w", err)
	}
	si := firstMsg.GetSignedId()
	if si == nil || len(si.Id) == 0 {
		relConn.Close()
		return nil, errors.New("host did not send SignedId during handshake")
	}

	var hostIDPKBytes []byte
	if len(hostSignPK) == 32 {
		unwrapped, ok := crypto.VerifyMessage(hostSignPK, si.Id)
		if !ok {
			relConn.Close()
			return nil, errors.New("host SignedId Ed25519 signature verification failed")
		}
		hostIDPKBytes = unwrapped
	} else if len(si.Id) > 64 {
		// Fallback when server key signing wasn't present
		hostIDPKBytes = si.Id[64:]
	}

	var hostIdPk pb.IdPk
	if err := proto.Unmarshal(hostIDPKBytes, &hostIdPk); err != nil {
		relConn.Close()
		return nil, fmt.Errorf("unmarshal host IdPk: %w", err)
	}
	if hostIdPk.Id != peerID || len(hostIdPk.Pk) != 32 {
		relConn.Close()
		return nil, fmt.Errorf("host identity mismatch (got id=%q pk_len=%d)", hostIdPk.Id, len(hostIdPk.Pk))
	}

	var theirBoxPK [32]byte
	copy(theirBoxPK[:], hostIdPk.Pk)

	ourBoxPK, ourBoxSK, err := box.GenerateKey(rand.Reader)
	if err != nil {
		relConn.Close()
		return nil, err
	}
	var symKey [32]byte
	if _, err := rand.Read(symKey[:]); err != nil {
		relConn.Close()
		return nil, err
	}
	var zeroNonce [24]byte
	sealedSym := box.Seal(nil, symKey[:], &zeroNonce, &theirBoxPK, ourBoxSK)

	// Send PublicKey with KxVersion=0 (classic NaCl secretbox with LE counter nonces)
	if err := ss.writeMsg(&pb.Message{
		Union: &pb.Message_PublicKey{
			PublicKey: &pb.PublicKey{
				AsymmetricValue: ourBoxPK[:],
				SymmetricValue:  sealedSym,
				KxVersion:       0,
			},
		},
	}); err != nil {
		relConn.Close()
		return nil, fmt.Errorf("send PublicKey: %w", err)
	}

	ss.setKey(symKey)
	return ss, nil
}

func mapControlKey(name string) pb.ControlKey {
	switch strings.ToLower(name) {
	case "alt", "altleft":
		return pb.ControlKey_Alt
	case "altright":
		return pb.ControlKey_RAlt
	case "backspace":
		return pb.ControlKey_Backspace
	case "capslock":
		return pb.ControlKey_CapsLock
	case "control", "controlleft":
		return pb.ControlKey_Control
	case "controlright":
		return pb.ControlKey_RControl
	case "delete":
		return pb.ControlKey_Delete
	case "arrowdown":
		return pb.ControlKey_DownArrow
	case "arrowup":
		return pb.ControlKey_UpArrow
	case "arrowleft":
		return pb.ControlKey_LeftArrow
	case "arrowright":
		return pb.ControlKey_RightArrow
	case "end":
		return pb.ControlKey_End
	case "home":
		return pb.ControlKey_Home
	case "escape":
		return pb.ControlKey_Escape
	case "enter":
		return pb.ControlKey_Return
	case "tab":
		return pb.ControlKey_Tab
	case " ", "space":
		return pb.ControlKey_Space
	case "shift", "shiftleft":
		return pb.ControlKey_Shift
	case "shiftright":
		return pb.ControlKey_RShift
	case "meta", "metaleft":
		return pb.ControlKey_Meta
	case "metaright":
		return pb.ControlKey_RWin
	case "pageup":
		return pb.ControlKey_PageUp
	case "pagedown":
		return pb.ControlKey_PageDown
	case "insert":
		return pb.ControlKey_Insert
	case "f1":
		return pb.ControlKey_F1
	case "f2":
		return pb.ControlKey_F2
	case "f3":
		return pb.ControlKey_F3
	case "f4":
		return pb.ControlKey_F4
	case "f5":
		return pb.ControlKey_F5
	case "f6":
		return pb.ControlKey_F6
	case "f7":
		return pb.ControlKey_F7
	case "f8":
		return pb.ControlKey_F8
	case "f9":
		return pb.ControlKey_F9
	case "f10":
		return pb.ControlKey_F10
	case "f11":
		return pb.ControlKey_F11
	case "f12":
		return pb.ControlKey_F12
	default:
		return pb.ControlKey_Unknown
	}
}

func buildOptionMessage(quality, codec string, fps int32) *pb.OptionMessage {
	iq := pb.ImageQuality_Balanced
	switch strings.ToLower(quality) {
	case "low", "fast":
		iq = pb.ImageQuality_Low
	case "best", "high":
		iq = pb.ImageQuality_Best
	}

	prefer := pb.SupportedDecoding_VP8
	switch strings.ToLower(codec) {
	case "vp9":
		prefer = pb.SupportedDecoding_VP9
	case "h264":
		prefer = pb.SupportedDecoding_H264
	case "av1":
		prefer = pb.SupportedDecoding_AV1
	case "auto":
		prefer = pb.SupportedDecoding_Auto
	}

	if fps < 10 || fps > 60 {
		fps = 30
	}

	return &pb.OptionMessage{
		ImageQuality: iq,
		CustomFps:    fps,
		DisableAudio: pb.OptionMessage_Yes,
		SupportedDecoding: &pb.SupportedDecoding{
			AbilityVp8:  1,
			AbilityVp9:  1,
			AbilityH264: 1,
			AbilityAv1:  1,
			Prefer:      prefer,
		},
	}
}

