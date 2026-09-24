package qqbot

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// KeyPair derives the Ed25519 key pair the open platform uses to sign callbacks.
// The platform repeats BotSecret until it is long enough and takes the first
// ed25519.SeedSize bytes as the key seed, so both signing and verifying use the
// same secret (there is no public key distribution in this scheme).
type KeyPair struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func NewKeyPair(botSecret string) (*KeyPair, error) {
	if botSecret == "" {
		return nil, fmt.Errorf("empty bot secret")
	}
	seed := botSecret
	for len(seed) < ed25519.SeedSize {
		seed = strings.Repeat(seed, 2)
	}
	pub, priv, err := ed25519.GenerateKey(strings.NewReader(seed[:ed25519.SeedSize]))
	if err != nil {
		return nil, fmt.Errorf("derive ed25519 key: %w", err)
	}
	return &KeyPair{public: pub, private: priv}, nil
}

// ErrMissingSignature lets callers distinguish "no signature headers were sent"
// from "the signature is wrong", because the address-verification request is
// documented without any signature headers.
var ErrMissingSignature = errors.New("missing signature headers")

// Verify checks X-Signature-Ed25519 over timestamp+body.
func (k *KeyPair) Verify(timestamp, signatureHex string, body []byte, maxSkew time.Duration) error {
	if timestamp == "" || signatureHex == "" {
		return ErrMissingSignature
	}
	if maxSkew > 0 {
		var ts int64
		if _, err := fmt.Sscanf(timestamp, "%d", &ts); err != nil || ts <= 0 {
			return fmt.Errorf("bad X-Signature-Timestamp %q", timestamp)
		}
		d := time.Since(time.Unix(ts, 0))
		if d < 0 {
			d = -d
		}
		if d > maxSkew {
			return fmt.Errorf("signature timestamp %s is %s off, outside %s skew", timestamp, d, maxSkew)
		}
	}
	sig, err := hex.DecodeString(signatureHex)
	if err != nil {
		return fmt.Errorf("signature not hex: %w", err)
	}
	if len(sig) != ed25519.SignatureSize || sig[63]&224 != 0 {
		return fmt.Errorf("malformed signature length or encoding")
	}
	var msg bytes.Buffer
	msg.WriteString(timestamp)
	msg.Write(body)
	if !ed25519.Verify(k.public, msg.Bytes(), sig) {
		return fmt.Errorf("ed25519 verify failed")
	}
	return nil
}

// Sign returns the hex Ed25519 signature of the raw message.
func (k *KeyPair) Sign(msg []byte) string {
	return hex.EncodeToString(ed25519.Sign(k.private, msg))
}
