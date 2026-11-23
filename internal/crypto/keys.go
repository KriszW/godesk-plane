package crypto

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/KriszW/godesk-plane/internal/pb"
	"google.golang.org/protobuf/proto"
)

// KeyPair holds the RustDesk Ed25519 server signing keypair in a format
// 100% compatible with rustdesk-server's id_ed25519 / id_ed25519.pub files.
type KeyPair struct {
	PrivateKey ed25519.PrivateKey
	PublicKey  ed25519.PublicKey
	PublicB64  string
}

// LoadOrGenerate loads id_ed25519 from dataDir, or generates a new Ed25519 keypair
// avoiding '/' and ':' in the base64 public key (matching rustdesk-server).
func LoadOrGenerate(dataDir string) (*KeyPair, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	privPath := filepath.Join(dataDir, "id_ed25519")
	pubPath := filepath.Join(dataDir, "id_ed25519.pub")

	if raw, err := os.ReadFile(privPath); err == nil {
		skBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", privPath, err)
		}
		if len(skBytes) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("invalid private key length %d (expected %d)", len(skBytes), ed25519.PrivateKeySize)
		}
		sk := ed25519.PrivateKey(skBytes)
		pk := sk.Public().(ed25519.PublicKey)
		pkB64 := base64.StdEncoding.EncodeToString(pk)
		_ = os.WriteFile(pubPath, []byte(pkB64), 0o644)
		return &KeyPair{
			PrivateKey: sk,
			PublicKey:  pk,
			PublicB64:  pkB64,
		}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", privPath, err)
	}

	var pk ed25519.PublicKey
	var sk ed25519.PrivateKey
	var pkB64 string
	for i := 0; i < 300; i++ {
		var err error
		pk, sk, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate ed25519 key: %w", err)
		}
		pkB64 = base64.StdEncoding.EncodeToString(pk)
		if !strings.ContainsAny(pkB64, "/:") {
			break
		}
	}

	skB64 := base64.StdEncoding.EncodeToString(sk)
	if err := os.WriteFile(privPath, []byte(skB64), 0o600); err != nil {
		return nil, fmt.Errorf("write %s: %w", privPath, err)
	}
	if err := os.WriteFile(pubPath, []byte(pkB64), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", pubPath, err)
	}
	return &KeyPair{
		PrivateKey: sk,
		PublicKey:  pk,
		PublicB64:  pkB64,
	}, nil
}

// FromBase64PrivateKey parses a base64-encoded 64-byte Ed25519 private key.
func FromBase64PrivateKey(b64 string) (*KeyPair, error) {
	skBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, err
	}
	if len(skBytes) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key length: %d", len(skBytes))
	}
	sk := ed25519.PrivateKey(skBytes)
	pk := sk.Public().(ed25519.PublicKey)
	return &KeyPair{
		PrivateKey: sk,
		PublicKey:  pk,
		PublicB64:  base64.StdEncoding.EncodeToString(pk),
	}, nil
}

// SignMessage produces a libsodium-compatible signed message: signature (64 bytes) || msg.
func (kp *KeyPair) SignMessage(msg []byte) []byte {
	sig := ed25519.Sign(kp.PrivateKey, msg)
	out := make([]byte, len(sig)+len(msg))
	copy(out, sig)
	copy(out[len(sig):], msg)
	return out
}

// VerifyMessage verifies and unwraps a libsodium-compatible signed message (sig || msg).
func VerifyMessage(pk ed25519.PublicKey, signed []byte) ([]byte, bool) {
	if len(signed) < ed25519.SignatureSize {
		return nil, false
	}
	sig := signed[:ed25519.SignatureSize]
	msg := signed[ed25519.SignatureSize:]
	if !ed25519.Verify(pk, msg, sig) {
		return nil, false
	}
	return msg, true
}

// SignIdPk serializes IdPk{id, pk} and signs it in libsodium format,
// matching rustdesk-server's RendezvousServer::get_pk.
func (kp *KeyPair) SignIdPk(id string, peerPK []byte) ([]byte, error) {
	if kp == nil || len(kp.PrivateKey) != ed25519.PrivateKeySize {
		return nil, nil
	}
	raw, err := proto.Marshal(&pb.IdPk{
		Id: id,
		Pk: peerPK,
	})
	if err != nil {
		return nil, err
	}
	return kp.SignMessage(raw), nil
}
