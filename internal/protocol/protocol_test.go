package protocol

import (
	"bytes"
	"net/netip"
	"testing"
)

func TestBytesCodecRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 0x3E, 0x3F, 0x40, 0x3FFF, 0x4000, 0x3FFFFF, 0x400000}
	for _, sz := range sizes {
		payload := bytes.Repeat([]byte{byte(sz%251 + 1)}, sz)
		frame, err := EncodeFrame(payload)
		if err != nil {
			t.Fatalf("EncodeFrame(len=%d): %v", sz, err)
		}
		var expectedHdr int
		switch {
		case sz <= 0x3F:
			expectedHdr = 1
		case sz <= 0x3FFF:
			expectedHdr = 2
		case sz <= 0x3FFFFF:
			expectedHdr = 3
		default:
			expectedHdr = 4
		}
		if len(frame) != sz+expectedHdr {
			t.Fatalf("len=%d: expected frame size %d, got %d", sz, sz+expectedHdr, len(frame))
		}
		got, err := ReadFrame(bytes.NewReader(frame), MaxFrameLength)
		if err != nil {
			t.Fatalf("ReadFrame(len=%d): %v", sz, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("len=%d: payload mismatch", sz)
		}
	}
}

func TestBytesCodecMaxLenRefused(t *testing.T) {
	payload := make([]byte, 1025)
	frame, err := EncodeFrame(payload)
	if err != nil {
		t.Fatalf("EncodeFrame: %v", err)
	}
	if _, err := ReadFrame(bytes.NewReader(frame), 1024); err == nil {
		t.Fatal("expected error for oversized frame")
	}
}

func TestAddrMangleIPv4(t *testing.T) {
	cases := []struct {
		addr string
		tm   uint32
	}{
		{"127.0.0.1:21116", 0},
		{"192.168.1.42:54321", 0xFFFFFFFF},
		{"255.255.255.255:65535", 0xDEADBEEF},
		{"10.0.0.1:1", 123456789},
		{"0.0.0.0:0", 0x8000FFFF},
	}
	for _, tc := range cases {
		ap := netip.MustParseAddrPort(tc.addr)
		encoded := encodeAddrWithTime(ap, tc.tm)
		decoded := DecodeAddr(encoded)
		if decoded != ap {
			t.Fatalf("IPv4 roundtrip failed for %s (tm=%d): got %s", ap, tc.tm, decoded)
		}
	}
}

func TestAddrMangleIPv6AndMapped(t *testing.T) {
	v6 := netip.MustParseAddrPort("[2001:db8::1]:21116")
	encoded := EncodeAddr(v6)
	if len(encoded) != 18 {
		t.Fatalf("expected 18 bytes for IPv6, got %d", len(encoded))
	}
	if got := DecodeAddr(encoded); got != v6 {
		t.Fatalf("IPv6 roundtrip mismatch: got %s want %s", got, v6)
	}

	mapped := netip.MustParseAddrPort("[::ffff:192.168.10.20]:44444")
	gotMapped := DecodeAddr(EncodeAddr(mapped))
	wantMapped := netip.MustParseAddrPort("192.168.10.20:44444")
	if gotMapped != wantMapped {
		t.Fatalf("IPv4-mapped IPv6 mismatch: got %s want %s", gotMapped, wantMapped)
	}
}
