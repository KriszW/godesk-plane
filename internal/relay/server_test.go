package relay

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/KriszW/godesk-plane/internal/pb"
	"github.com/KriszW/godesk-plane/internal/protocol"
	"github.com/KriszW/godesk-plane/internal/store"
	"github.com/gorilla/websocket"
	"google.golang.org/protobuf/proto"
)

func sendRequestRelay(t *testing.T, conn net.Conn, uuid, key string) {
	t.Helper()
	raw, err := proto.Marshal(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RequestRelay{
			RequestRelay: &pb.RequestRelay{
				Uuid:       uuid,
				LicenceKey: key,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal RequestRelay: %v", err)
	}
	if err := protocol.WriteFrame(conn, raw); err != nil {
		t.Fatalf("WriteFrame RequestRelay: %v", err)
	}
}

func TestRelayTCPPairAndStream(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	srv := NewServer(Config{
		BindHost: "127.0.0.1",
		TCPPort:  0,
		Key:      "test-public-key",
	}, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer srv.Close()

	addr := srv.TCPAddr().String()
	srv.RegisterMeta("relay-uuid-1", RelayMeta{
		FromIP:   "10.0.0.1",
		ToID:     "999888777",
		ToIP:     "10.0.0.2",
		ConnType: "DEFAULT_CONN",
	})

	// Bad key should be rejected
	badConn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial bad: %v", err)
	}
	sendRequestRelay(t, badConn, "relay-bad", "wrong-key")
	_ = badConn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1)
	if _, err := badConn.Read(buf); err == nil {
		t.Fatal("expected rejected connection with wrong key")
	}
	badConn.Close()

	// Pair two valid TCP legs
	connA, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	defer connA.Close()
	sendRequestRelay(t, connA, "relay-uuid-1", "test-public-key")

	connB, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	defer connB.Close()
	sendRequestRelay(t, connB, "relay-uuid-1", "test-public-key")

	// Bidirectional raw traffic
	msgAB := bytes.Repeat([]byte("hello-from-A-"), 400)
	msgBA := bytes.Repeat([]byte("reply-from-B-"), 400)

	go func() {
		_, _ = connA.Write(msgAB)
	}()
	gotB := make([]byte, len(msgAB))
	_ = connB.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(connB, gotB); err != nil {
		t.Fatalf("read on B: %v", err)
	}
	if !bytes.Equal(gotB, msgAB) {
		t.Fatal("A->B payload mismatch")
	}

	go func() {
		_, _ = connB.Write(msgBA)
	}()
	gotA := make([]byte, len(msgBA))
	_ = connA.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(connA, gotA); err != nil {
		t.Fatalf("read on A: %v", err)
	}
	if !bytes.Equal(gotA, msgBA) {
		t.Fatal("B->A payload mismatch")
	}

	connA.Close()
	connB.Close()

	// Wait briefly for session accounting to flush
	time.Sleep(150 * time.Millisecond)

	sessions, err := st.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session recorded, got %d", len(sessions))
	}
	if sessions[0].ToID != "999888777" || sessions[0].BytesSent+sessions[0].BytesRecv < int64(len(msgAB)+len(msgBA)) {
		t.Fatalf("unexpected session record: %+v", sessions[0])
	}
}

func TestRelayWSToTCPPair(t *testing.T) {
	srv := NewServer(Config{
		BindHost: "127.0.0.1",
		TCPPort:  0,
		WSPort:   0, // We'll bind WS on an ephemeral port using a fixed free port
	}, nil)

	// Pick a free port for WS
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	wsPort := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	srv.cfg.WSPort = wsPort

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatalf("start relay: %v", err)
	}
	defer srv.Close()

	wsURL := "ws://" + srv.WSAddr().String()
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer wsConn.Close()

	reqBytes, _ := proto.Marshal(&pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RequestRelay{
			RequestRelay: &pb.RequestRelay{Uuid: "ws-tcp-pair"},
		},
	})
	if err := wsConn.WriteMessage(websocket.BinaryMessage, reqBytes); err != nil {
		t.Fatalf("ws write RequestRelay: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	tcpConn, err := net.Dial("tcp", srv.TCPAddr().String())
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer tcpConn.Close()
	sendRequestRelay(t, tcpConn, "ws-tcp-pair", "")

	// Send framed payload from TCP -> WS
	payload := bytes.Repeat([]byte{0x7A}, 2048)
	if err := protocol.WriteFrame(tcpConn, payload); err != nil {
		t.Fatalf("tcp write frame: %v", err)
	}
	_ = wsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	mt, gotWS, err := wsConn.ReadMessage()
	if err != nil || mt != websocket.BinaryMessage || !bytes.Equal(gotWS, payload) {
		t.Fatalf("ws read mismatch: mt=%d len=%d err=%v", mt, len(gotWS), err)
	}

	// Send binary message from WS -> TCP
	reply := bytes.Repeat([]byte{0x5B}, 1024)
	if err := wsConn.WriteMessage(websocket.BinaryMessage, reply); err != nil {
		t.Fatalf("ws write reply: %v", err)
	}
	_ = tcpConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	gotTCP, err := protocol.ReadFrame(tcpConn, protocol.MaxFrameLength)
	if err != nil || !bytes.Equal(gotTCP, reply) {
		t.Fatalf("tcp read mismatch: len=%d err=%v", len(gotTCP), err)
	}
}
