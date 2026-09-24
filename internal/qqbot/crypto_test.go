package qqbot

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"
	"time"
)

// Vectors published in the official docs (安全和授权 / Webhook 方式), so this
// checks our key derivation against Tencent's own reference output.
const (
	docSecret    = "naOC0ocQE3shWLAfffVLB1rhYPG7"
	docBody      = `{ "op": 0,"d": {}, "t": "GATEWAY_EVENT_NAME"}`
	docTimestamp = "1725442341"
	docSignature = "865ad13a61752ca65e26bde6676459cd36cf1be609375b37bd62af366e1dc25a8dc789ba7f14e017ada3d554c671a911bfdf075ba54835b23391d509579ed002"
)

var docPublicKey = []byte{
	215, 195, 98, 254, 120, 174, 248, 31, 242, 50, 135, 180, 147, 98, 139, 93,
	176, 42, 60, 79, 227, 11, 33, 94, 77, 25, 96, 155, 93, 118, 103, 58,
}

func TestKeyDerivationMatchesDoc(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatalf("NewKeyPair: %v", err)
	}
	if !bytes.Equal(kp.public, docPublicKey) {
		t.Errorf("public key = %v, want %v", []byte(kp.public), docPublicKey)
	}
	if want := ed25519.PrivateKeySize; len(kp.private) != want {
		t.Errorf("private key len = %d, want %d", len(kp.private), want)
	}
}

func TestVerifyAcceptsDocSignature(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatal(err)
	}
	// maxSkew 0 disables the freshness check; the doc timestamp is years old.
	if err := kp.Verify(docTimestamp, docSignature, []byte(docBody), 0); err != nil {
		t.Fatalf("Verify rejected the documented signature: %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatal(err)
	}

	if err := kp.Verify(docTimestamp, docSignature, []byte(docBody+" "), 0); err == nil {
		t.Error("Verify accepted a tampered body")
	}
	if err := kp.Verify("1725442342", docSignature, []byte(docBody), 0); err == nil {
		t.Error("Verify accepted a mismatched timestamp")
	}
	if err := kp.Verify(docTimestamp, hex.EncodeToString(bytes.Repeat([]byte{1}, 64)), []byte(docBody), 0); err == nil {
		t.Error("Verify accepted a bogus signature")
	}
	if err := kp.Verify("", "", []byte(docBody), 0); err == nil {
		t.Error("Verify accepted a request without signature headers")
	}
}

func TestVerifyEnforcesSkew(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := kp.Verify(docTimestamp, docSignature, []byte(docBody), 5*time.Minute); err == nil {
		t.Error("Verify accepted a stale timestamp with skew enabled")
	}
}
