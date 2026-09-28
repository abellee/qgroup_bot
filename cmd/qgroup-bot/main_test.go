package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTurnstileCheckVerifiesWithCloudflare(t *testing.T) {
	var gotForm string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		w.Header().Set("content-type", "application/json")
		switch r.PostFormValue("response") {
		case "tok-ok":
			_, _ = w.Write([]byte(`{"success":true}`))
		case "tok-bad":
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["invalid-input-response"]}`))
		default:
			_, _ = w.Write([]byte(`{"success":false,"error-codes":["missing-input-response"]}`))
		}
	}))
	t.Cleanup(stub.Close)

	check := turnstileCheck{secret: "sec-1", endpoint: stub.URL, client: stub.Client()}

	if err := check.Verify(context.Background(), "tok-ok", "203.0.113.9"); err != nil {
		t.Fatalf("Verify(ok): %v", err)
	}
	// The secret never rides in a URL, and the caller's address goes along.
	for _, want := range []string{"secret=sec-1", "response=tok-ok", "remoteip=203.0.113.9"} {
		if !strings.Contains(gotForm, want) {
			t.Errorf("form = %q, want %s present", gotForm, want)
		}
	}

	if err := check.Verify(context.Background(), "tok-bad", ""); err == nil || !strings.Contains(err.Error(), "invalid-input-response") {
		t.Errorf("Verify(bad) = %v, want the provider's error code named", err)
	}
	if err := check.Verify(context.Background(), "", ""); err == nil || !strings.Contains(err.Error(), "missing token") {
		t.Errorf("Verify(empty) = %v, want the local refusal", err)
	}
}
