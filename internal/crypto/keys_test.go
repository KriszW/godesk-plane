package crypto

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/KriszW/godesk-plane/internal/pb"
	"google.golang.org/protobuf/proto"
)

func TestLoadOrGenerateAndSignIdPk(t *testing.T) {
	dir := t.TempDir()
	kp1, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("LoadOrGenerate first: %v", err)
	}
	if len(kp1.PublicKey) != 32 || len(kp1.PrivateKey) != 64 {
		t.Fatalf("unexpected key sizes: pk=%d sk=%d", len(kp1.PublicKey), len(kp1.PrivateKey))
	}

	// Verify files created with proper permissions
	info, err := os.Stat(filepath.Join(dir, "id_ed25519"))
	if err != nil {
		t.Fatalf("stat id_ed25519: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected 0600 permissions, got %o", info.Mode().Perm())
	}

	// Second load returns identical keypair
	kp2, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatalf("LoadOrGenerate second: %v", err)
	}
	if kp1.PublicB64 != kp2.PublicB64 {
		t.Fatalf("public key mismatch across reloads: %s vs %s", kp1.PublicB64, kp2.PublicB64)
	}

	// Sign and verify IdPk
	peerPK := bytes.Repeat([]byte{0x42}, 32)
	signed, err := kp1.SignIdPk("123456789", peerPK)
	if err != nil {
		t.Fatalf("SignIdPk: %v", err)
	}
	msg, ok := VerifyMessage(kp1.PublicKey, signed)
	if !ok {
		t.Fatal("VerifyMessage failed")
	}
	var decoded pb.IdPk
	if err := proto.Unmarshal(msg, &decoded); err != nil {
		t.Fatalf("unmarshal IdPk: %v", err)
	}
	if decoded.Id != "123456789" || !bytes.Equal(decoded.Pk, peerPK) {
		t.Fatalf("unexpected IdPk payload: id=%q pk=%x", decoded.Id, decoded.Pk)
	}
}
