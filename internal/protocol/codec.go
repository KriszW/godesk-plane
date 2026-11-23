package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxFrameLength is the largest payload representable by the 4-byte RustDesk frame header.
	MaxFrameLength = 0x3FFFFFFF
	// DefaultMaxPacketLength matches rustdesk-server's default 64 KiB rendezvous limit.
	DefaultMaxPacketLength = 64 * 1024
	// MaxRelayRequestLength matches rustdesk-server's bound for the initial RequestRelay frame.
	MaxRelayRequestLength = 16 * 1024
)

var (
	ErrFrameTooLarge = errors.New("frame exceeds maximum allowed length")
	ErrFrameOverflow = errors.New("frame length exceeds protocol maximum")
)

// EncodeFrame writes a length-prefixed RustDesk BytesCodec frame to w.
func EncodeFrame(data []byte) ([]byte, error) {
	n := len(data)
	var header [4]byte
	var headLen int

	switch {
	case n <= 0x3F:
		header[0] = byte(n << 2)
		headLen = 1
	case n <= 0x3FFF:
		binary.LittleEndian.PutUint16(header[:2], uint16(n<<2)|0x1)
		headLen = 2
	case n <= 0x3FFFFF:
		h := uint32(n<<2) | 0x2
		header[0] = byte(h)
		header[1] = byte(h >> 8)
		header[2] = byte(h >> 16)
		headLen = 3
	case n <= MaxFrameLength:
		binary.LittleEndian.PutUint32(header[:4], uint32(n<<2)|0x3)
		headLen = 4
	default:
		return nil, ErrFrameOverflow
	}

	out := make([]byte, headLen+n)
	copy(out[:headLen], header[:headLen])
	copy(out[headLen:], data)
	return out, nil
}

// WriteFrame encodes data using RustDesk's BytesCodec framing and writes it to w.
func WriteFrame(w io.Writer, data []byte) error {
	frame, err := EncodeFrame(data)
	if err != nil {
		return err
	}
	_, err = w.Write(frame)
	return err
}

// ReadFrame reads a single length-prefixed RustDesk BytesCodec frame from r.
// If maxLen <= 0, MaxFrameLength is used.
func ReadFrame(r io.Reader, maxLen int) ([]byte, error) {
	if maxLen <= 0 || maxLen > MaxFrameLength {
		maxLen = MaxFrameLength
	}

	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:1]); err != nil {
		return nil, err
	}

	headLen := int(hdr[0]&0x3) + 1
	if headLen > 1 {
		if _, err := io.ReadFull(r, hdr[1:headLen]); err != nil {
			return nil, err
		}
	}

	var raw uint32
	for i := 0; i < headLen; i++ {
		raw |= uint32(hdr[i]) << (8 * i)
	}
	payloadLen := int(raw >> 2)
	if payloadLen > maxLen {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, payloadLen, maxLen)
	}

	if payloadLen == 0 {
		return []byte{}, nil
	}

	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}
