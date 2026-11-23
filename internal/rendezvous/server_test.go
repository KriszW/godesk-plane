package rendezvous

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/KriszW/godesk-plane/internal/crypto"
	"github.com/KriszW/godesk-plane/internal/pb"
	"github.com/KriszW/godesk-plane/internal/protocol"
	"github.com/KriszW/godesk-plane/internal/relay"
	"github.com/KriszW/godesk-plane/internal/store"
	"google.golang.org/protobuf/proto"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func sendUDP(t *testing.T, conn *net.UDPConn, msg *pb.RendezvousMessage) {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(raw); err != nil {
		t.Fatal(err)
	}
}

func recvUDP(t *testing.T, conn *net.UDPConn) *pb.RendezvousMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16*1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("recvUDP: %v", err)
	}
	var msg pb.RendezvousMessage
	if err := proto.Unmarshal(buf[:n], &msg); err != nil {
		t.Fatalf("unmarshal UDP: %v", err)
	}
	return &msg
}

func sendTCP(t *testing.T, conn net.Conn, msg *pb.RendezvousMessage) {
	t.Helper()
	raw, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteFrame(conn, raw); err != nil {
		t.Fatal(err)
	}
}

func recvTCP(t *testing.T, conn net.Conn) *pb.RendezvousMessage {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	raw, err := protocol.ReadFrame(conn, protocol.DefaultMaxPacketLength)
	if err != nil {
		t.Fatalf("recvTCP: %v", err)
	}
	var msg pb.RendezvousMessage
	if err := proto.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("unmarshal TCP: %v", err)
	}
	return &msg
}

func TestRendezvousFullHandshakeAndOnline(t *testing.T) {
	dir := t.TempDir()
	kp, err := crypto.LoadOrGenerate(dir)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	relaySrv := relay.NewServer(relay.Config{BindHost: "127.0.0.1", TCPPort: 0, Key: kp.PublicB64}, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := relaySrv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer relaySrv.Close()

	mainPort := freeTCPPort(t)
	natPort := freeTCPPort(t)
	srv := NewServer(Config{
		BindHost:     "127.0.0.1",
		Port:         mainPort,
		NATPort:      natPort,
		RequireKey:   true,
		RelayServers: []string{relaySrv.TCPAddr().String()},
	}, kp, st, relaySrv)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	udpSrvAddr := srv.UDPAddr().(*net.UDPAddr)
	tcpSrvAddr := srv.TCPAddr().String()
	natSrvAddr := srv.NATAddr().String()

	// 1. Peer B registers via UDP: RegisterPeer -> RequestPk=true -> RegisterPk -> OK
	peerBUDP, err := net.DialUDP("udp", nil, udpSrvAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer peerBUDP.Close()

	sendUDP(t, peerBUDP, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RegisterPeer{
			RegisterPeer: &pb.RegisterPeer{Id: "222333444"},
		},
	})
	resp := recvUDP(t, peerBUDP)
	if !resp.GetRegisterPeerResponse().GetRequestPk() {
		t.Fatal("expected RequestPk=true for brand new peer")
	}

	peerBPK := bytes.Repeat([]byte{0xAB}, 32)
	sendUDP(t, peerBUDP, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RegisterPk{
			RegisterPk: &pb.RegisterPk{
				Id:   "222333444",
				Uuid: []byte("peer-b-uuid-1234"),
				Pk:   peerBPK,
			},
		},
	})
	pkResp := recvUDP(t, peerBUDP)
	if pkResp.GetRegisterPkResponse().GetResult() != pb.RegisterPkResponse_OK {
		t.Fatalf("expected OK, got %v", pkResp.GetRegisterPkResponse().GetResult())
	}

	// Second RegisterPeer should return RequestPk=false
	sendUDP(t, peerBUDP, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RegisterPeer{
			RegisterPeer: &pb.RegisterPeer{Id: "222333444"},
		},
	})
	resp2 := recvUDP(t, peerBUDP)
	if resp2.GetRegisterPeerResponse().GetRequestPk() {
		t.Fatal("expected RequestPk=false for already registered peer")
	}

	// 2. TestNatRequest on NAT port (21115)
	natConn, err := net.Dial("tcp", natSrvAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendTCP(t, natConn, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_TestNatRequest{TestNatRequest: &pb.TestNatRequest{}},
	})
	natResp := recvTCP(t, natConn)
	natConn.Close()
	if natResp.GetTestNatResponse().GetPort() <= 0 {
		t.Fatalf("expected positive NAT port, got %+v", natResp)
	}

	// 3. OnlineRequest on NAT port (21115)
	onConn, err := net.Dial("tcp", natSrvAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendTCP(t, onConn, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_OnlineRequest{
			OnlineRequest: &pb.OnlineRequest{Peers: []string{"222333444", "999999999"}},
		},
	})
	onResp := recvTCP(t, onConn)
	onConn.Close()
	states := onResp.GetOnlineResponse().GetStates()
	if len(states) != 1 || states[0] != 0x80 {
		t.Fatalf("expected online bitmask 0x80 (peer 0 online, peer 1 offline), got %x", states)
	}

	// 4. Peer A sends PunchHoleRequest (force relay) -> Server forwards PunchHole via UDP to Peer B -> Peer B sends PunchHoleSent -> Peer A receives PunchHoleResponse with valid Ed25519-signed IdPk
	connA, err := net.Dial("tcp", tcpSrvAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()

	sendTCP(t, connA, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_PunchHoleRequest{
			PunchHoleRequest: &pb.PunchHoleRequest{
				Id:         "222333444",
				LicenceKey: kp.PublicB64,
				ForceRelay: true,
				NatType:    pb.NatType_ASYMMETRIC,
				ConnType:   pb.ConnType_DEFAULT_CONN,
			},
		},
	})

	// Peer B receives PunchHole on UDP
	phMsg := recvUDP(t, peerBUDP)
	ph := phMsg.GetPunchHole()
	if ph == nil || !ph.ForceRelay {
		t.Fatalf("expected PunchHole with ForceRelay=true, got %+v", phMsg)
	}

	// Peer B responds over TCP with PunchHoleSent
	connB, err := net.Dial("tcp", tcpSrvAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendTCP(t, connB, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_PunchHoleSent{
			PunchHoleSent: &pb.PunchHoleSent{
				SocketAddr:  ph.SocketAddr,
				Id:          "222333444",
				RelayServer: ph.RelayServer,
				Version:     "1.5.3",
				NatType:     pb.NatType_ASYMMETRIC,
			},
		},
	})
	connB.Close()

	// Peer A receives PunchHoleResponse with Ed25519 signed IdPk
	phrMsg := recvTCP(t, connA)
	phr := phrMsg.GetPunchHoleResponse()
	if phr == nil || len(phr.Pk) == 0 {
		t.Fatalf("expected PunchHoleResponse with signed Pk, got %+v", phrMsg)
	}
	unwrapped, ok := crypto.VerifyMessage(kp.PublicKey, phr.Pk)
	if !ok {
		t.Fatal("failed to verify Ed25519 signature on IdPk")
	}
	var idpk pb.IdPk
	if err := proto.Unmarshal(unwrapped, &idpk); err != nil {
		t.Fatalf("unmarshal IdPk: %v", err)
	}
	if idpk.Id != "222333444" || !bytes.Equal(idpk.Pk, peerBPK) {
		t.Fatalf("IdPk content mismatch: id=%s pk=%x", idpk.Id, idpk.Pk)
	}

	// 5. Peer A sends RequestRelay on same TCP conn -> Peer B gets RequestRelay on UDP -> Peer B sends RelayResponse -> Peer A gets RelayResponse with signed IdPk
	sendTCP(t, connA, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RequestRelay{
			RequestRelay: &pb.RequestRelay{
				Id:          "222333444",
				Uuid:        "test-relay-uuid-99",
				RelayServer: relaySrv.TCPAddr().String(),
				LicenceKey:  kp.PublicB64,
				ConnType:    pb.ConnType_DEFAULT_CONN,
			},
		},
	})

	rrUDP := recvUDP(t, peerBUDP)
	rf := rrUDP.GetRequestRelay()
	if rf == nil || rf.Uuid != "test-relay-uuid-99" {
		t.Fatalf("expected RequestRelay on Peer B UDP, got %+v", rrUDP)
	}

	connB2, err := net.Dial("tcp", tcpSrvAddr)
	if err != nil {
		t.Fatal(err)
	}
	sendTCP(t, connB2, &pb.RendezvousMessage{
		Union: &pb.RendezvousMessage_RelayResponse{
			RelayResponse: &pb.RelayResponse{
				SocketAddr:  rf.SocketAddr,
				Uuid:        rf.Uuid,
				RelayServer: rf.RelayServer,
				Union:       &pb.RelayResponse_Id{Id: "222333444"},
				Version:     "1.5.3",
			},
		},
	})
	connB2.Close()

	rrTCP := recvTCP(t, connA)
	rr := rrTCP.GetRelayResponse()
	if rr == nil || rr.Uuid != "test-relay-uuid-99" || len(rr.GetPk()) == 0 {
		t.Fatalf("expected RelayResponse with signed Pk on Peer A, got %+v", rrTCP)
	}
	if _, ok := crypto.VerifyMessage(kp.PublicKey, rr.GetPk()); !ok {
		t.Fatal("failed to verify RelayResponse IdPk signature")
	}
}
