package protocol

import (
	"encoding/binary"
	"net"
	"net/netip"
	"time"
)

// NormalizeAddr unmaps IPv4-mapped IPv6 addresses (e.g. ::ffff:1.2.3.4 -> 1.2.3.4)
// matching hbb_common::try_into_v4.
func NormalizeAddr(addr netip.AddrPort) netip.AddrPort {
	if !addr.IsValid() {
		return addr
	}
	ip := addr.Addr()
	if ip.Is4In6() {
		return netip.AddrPortFrom(ip.Unmap(), addr.Port())
	}
	return addr
}

// NetAddrToAddrPort converts a net.Addr (*net.TCPAddr or *net.UDPAddr) to a normalized netip.AddrPort.
func NetAddrToAddrPort(addr net.Addr) netip.AddrPort {
	switch a := addr.(type) {
	case *net.TCPAddr:
		return NormalizeAddr(a.AddrPort())
	case *net.UDPAddr:
		return NormalizeAddr(a.AddrPort())
	default:
		ap, err := netip.ParseAddrPort(addr.String())
		if err != nil {
			return netip.AddrPort{}
		}
		return NormalizeAddr(ap)
	}
}

// EncodeAddr implements hbb_common::AddrMangle::encode.
func EncodeAddr(addr netip.AddrPort) []byte {
	return encodeAddrWithTime(addr, uint32(time.Now().UnixMicro()))
}

func encodeAddrWithTime(addr netip.AddrPort, tm32 uint32) []byte {
	addr = NormalizeAddr(addr)
	if !addr.IsValid() {
		return nil
	}
	if addr.Addr().Is4() {
		ip4 := addr.Addr().As4()
		ip := uint64(binary.LittleEndian.Uint32(ip4[:]))
		tm := uint64(tm32)
		port := uint64(addr.Port())

		ipPlusTm := ip + tm
		lo := (ipPlusTm << 49) | (tm << 17) | (port + (tm & 0xFFFF))
		hi := ipPlusTm >> 15

		var buf [16]byte
		binary.LittleEndian.PutUint64(buf[0:8], lo)
		binary.LittleEndian.PutUint64(buf[8:16], hi)

		end := 16
		for end > 0 && buf[end-1] == 0 {
			end--
		}
		out := make([]byte, end)
		copy(out, buf[:end])
		return out
	}

	// IPv6: 16 bytes address + 2 bytes little-endian port.
	ip6 := addr.Addr().As16()
	out := make([]byte, 18)
	copy(out[:16], ip6[:])
	binary.LittleEndian.PutUint16(out[16:18], addr.Port())
	return out
}

// DecodeAddr implements hbb_common::AddrMangle::decode.
func DecodeAddr(b []byte) netip.AddrPort {
	if len(b) > 16 {
		if len(b) != 18 {
			return netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
		}
		var ip6 [16]byte
		copy(ip6[:], b[:16])
		port := binary.LittleEndian.Uint16(b[16:18])
		return netip.AddrPortFrom(netip.AddrFrom16(ip6), port)
	}

	var padded [16]byte
	copy(padded[:], b)
	lo := binary.LittleEndian.Uint64(padded[0:8])
	hi := binary.LittleEndian.Uint64(padded[8:16])

	tm := (lo >> 17) & 0xFFFFFFFF
	upper := (lo >> 49) | (hi << 15)
	ipU32 := uint32(upper - tm)
	var ip4 [4]byte
	binary.LittleEndian.PutUint32(ip4[:], ipU32)
	port := uint16((lo & 0xFFFFFF) - (tm & 0xFFFF))

	return netip.AddrPortFrom(netip.AddrFrom4(ip4), port)
}
