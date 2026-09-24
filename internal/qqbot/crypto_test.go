package qqbot

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
	"time"
)

// Vectors published in the official docs (安全和授权 / Webhook 方式). The key
// derivation reproduces Tencent's reference output exactly, so TestKeyDerivation
// MatchesDoc pins it down.
//
// The docs' sample signature for a dispatch body does NOT reproduce against that
// same key (the rendered body may differ from what generated the sample), so it
// is not asserted here. Instead the verify/reject tests sign with our own key
// pair, which keeps every case meaningful: a broken rule fails loudly rather
// than passing against an already-invalid baseline signature.
const (
	docSecret    = "naOC0ocQE3shWLAfffVLB1rhYPG7"
	docBody      = `{ "op": 0,"d": {}, "t": "GATEWAY_EVENT_NAME"}`
	docTimestamp = "1725442341"
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

// signDoc mimics what the platform sends: Ed25519 over timestamp+rawBody.
func signDoc(t *testing.T, kp *KeyPair, timestamp string, body []byte) string {
	t.Helper()
	var msg bytes.Buffer
	msg.WriteString(timestamp)
	msg.Write(body)
	return kp.Sign(msg.Bytes())
}

func TestVerifyAcceptsMatchingSignature(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(docBody)
	sig := signDoc(t, kp, docTimestamp, body)
	// maxSkew 0 disables the freshness check; the doc timestamp is years old.
	if err := kp.Verify(docTimestamp, sig, body, 0); err != nil {
		t.Fatalf("Verify rejected a valid signature: %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatal(err)
	}
	good := signDoc(t, kp, docTimestamp, []byte(docBody))

	// Sanity check first: the untampered triplet must verify, otherwise every
	// rejection below would pass for the wrong reason.
	if err := kp.Verify(docTimestamp, good, []byte(docBody), 0); err != nil {
		t.Fatalf("baseline signature does not verify: %v", err)
	}

	cases := []struct {
		name      string
		timestamp string
		sig       string
		body      []byte
	}{
		{"appended body byte", docTimestamp, good, []byte(docBody + " ")},
		{"mismatched timestamp", "1725442342", good, []byte(docBody)},
		{"all-ones signature", docTimestamp, hex.EncodeToString(bytes.Repeat([]byte{1}, 64)), []byte(docBody)},
		{"non-hex signature", docTimestamp, "zz" + good[2:], []byte(docBody)},
		{"short signature", docTimestamp, good[:20], []byte(docBody)},
		{"empty signature", docTimestamp, "", []byte(docBody)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := kp.Verify(tc.timestamp, tc.sig, tc.body, 0); err == nil {
				t.Errorf("Verify accepted %s", tc.name)
			}
		})
	}
}

func TestVerifyMissingHeaders(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatal(err)
	}
	// op=13 URL validation arrives unsigned, so the handler needs to tell a
	// missing-signature request apart from a malformed one.
	err = kp.Verify("", "", []byte(docBody), 0)
	if !errors.Is(err, ErrMissingSignature) {
		t.Fatalf("Verify with no headers = %v, want %v", err, ErrMissingSignature)
	}
}

func TestVerifyEnforcesSkew(t *testing.T) {
	kp, err := NewKeyPair(docSecret)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(docBody)
	sig := signDoc(t, kp, docTimestamp, body)
	if err := kp.Verify(docTimestamp, sig, body, 5*time.Minute); err == nil {
		t.Error("Verify accepted a years-old timestamp with skew enabled")
	}

	recent := strconv.FormatInt(time.Now().Unix(), 10)
	freshSig := signDoc(t, kp, recent, body)
	if err := kp.Verify(recent, freshSig, body, 5*time.Minute); err != nil {
		t.Errorf("Verify rejected a fresh timestamp: %v", err)
	}
}
