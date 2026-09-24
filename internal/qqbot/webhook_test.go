package qqbot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	docAppID        = "11111111"
	docAppSecret    = "DG5g3B4j9X2KOErG"
	docPlainToken   = "Arq0D5A61EgUu4OxUvOp"
	docValidationSig = "87befc99c42c651b3aac0278e71ada338433ae26fcb24307bdc5ad38c1adc2d01bcfcadc0842edac85e85205028a1132afe09280305f13aa6909ffc2d652c706"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestHandler(t *testing.T, fn JoinHandler) *Handler {
	t.Helper()
	h, err := NewHandler(docAppID, docAppSecret, 5*time.Minute, 8, fn, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The documented address-verification exchange: the platform asks us to sign
// event_ts+plain_token with the key derived from BotSecret.
func TestHandlerURLValidation(t *testing.T) {
	h := newTestHandler(t, func(context.Context, *JoinRequestEvent) {})
	body := fmt.Sprintf(`{"d":{"plain_token":%q,"event_ts":%q},"op":13}`, docPlainToken, docTimestamp)

	req := httptest.NewRequest("POST", "/qq/callback", strings.NewReader(body))
	req.Header.Set("User-Agent", "QQBot-Callback")
	req.Header.Set("X-Bot-Appid", docAppID)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body)
	}
	var out ValidationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.PlainToken != docPlainToken {
		t.Errorf("plain_token = %q, want %q", out.PlainToken, docPlainToken)
	}
	if out.Signature != docValidationSig {
		t.Errorf("signature mismatch:\n got %s\nwant %s", out.Signature, docValidationSig)
	}
}

func TestHandlerDispatchRequiresValidSignature(t *testing.T) {
	var mu sync.Mutex
	var seen []*JoinRequestEvent
	h := newTestHandler(t, func(_ context.Context, ev *JoinRequestEvent) {
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})
	h.Start(context.Background())

	const joinBody = `{"id":"evt-1","op":0,"s":7,"t":"GROUP_JOIN_REQUEST","d":{"group_openid":"g1","member_openid":"m1","join_request_id":"jr1","apply_source":"self_apply","verify_info":{"method":"verify_message","verify_message":"申请入群 abcd@Example.com "}}}`

	kp, err := NewKeyPair(docAppSecret)
	if err != nil {
		t.Fatal(err)
	}
	ts := fmt.Sprintf("%d", time.Now().Unix())

	t.Run("unsigned event is rejected", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/qq/callback", strings.NewReader(joinBody))
		req.Header.Set("X-Bot-Appid", docAppID)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 401 {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})

	t.Run("signed event is ACKed and queued", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/qq/callback", strings.NewReader(joinBody))
		req.Header.Set("X-Bot-Appid", docAppID)
		req.Header.Set("X-Signature-Timestamp", ts)
		req.Header.Set("X-Signature-Ed25519", kp.Sign([]byte(ts+joinBody)))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != 200 {
			t.Fatalf("status = %d, body=%s", rr.Code, rr.Body)
		}
		var ack struct {
			Op int `json:"op"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &ack); err != nil || ack.Op != OpCallbackACK {
			t.Fatalf("ack = %s (op=%d, err=%v), want op=%d", rr.Body, ack.Op, err, OpCallbackACK)
		}

		ev := waitForEvent(t, &mu, &seen)
		if ev.GroupOpenID != "g1" || ev.JoinRequestID != "jr1" {
			t.Errorf("decoded event = %+v", ev)
		}
		if ev.VerifyInfo.VerifyMessage != "申请入群 abcd@Example.com " {
			t.Errorf("verify_message = %q", ev.VerifyInfo.VerifyMessage)
		}
	})

	t.Run("stale signature timestamp is rejected", func(t *testing.T) {
		old := "1725442341"
		req := httptest.NewRequest("POST", "/qq/callback", strings.NewReader(joinBody))
		req.Header.Set("X-Signature-Timestamp", old)
		req.Header.Set("X-Signature-Ed25519", kp.Sign([]byte(old+joinBody)))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != 401 {
			t.Fatalf("status = %d, want 401", rr.Code)
		}
	})
}

func TestHandlerRejectsOtherMethods(t *testing.T) {
	h := newTestHandler(t, func(context.Context, *JoinRequestEvent) {})
	req := httptest.NewRequest("GET", "/qq/callback", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != 405 {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func waitForEvent(t *testing.T, mu *sync.Mutex, seen *[]*JoinRequestEvent) *JoinRequestEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(*seen)
		var ev *JoinRequestEvent
		if n > 0 {
			ev = (*seen)[n-1]
		}
		mu.Unlock()
		if ev != nil {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("join handler was never called")
	return nil
}
