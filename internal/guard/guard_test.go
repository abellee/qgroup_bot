package guard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// probe answers 404 on /missing and 200 everywhere else, like the real mux
// looks to the guard.
func probe() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func request(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestMissesBoxTheScanningAddress(t *testing.T) {
	g := New(discardLogger())
	h := g.Wrap(probe())

	// Just under the budget, a miss is a miss.
	for i := 1; i < maxMisses; i++ {
		if rec := request(t, h, "/missing"); rec.Code != http.StatusNotFound {
			t.Fatalf("miss %d got %d, want 404", i, rec.Code)
		}
	}
	if rec := request(t, h, "/valid"); rec.Code != http.StatusOK {
		t.Fatalf("a healthy request got %d, want 200", rec.Code)
	}

	// The miss that tips the budget still answers, then the box shuts.
	if rec := request(t, h, "/missing"); rec.Code != http.StatusNotFound {
		t.Fatalf("miss %d got %d, want 404", maxMisses, rec.Code)
	}
	for _, target := range []string{"/missing", "/valid"} {
		if rec := request(t, h, target); rec.Code != http.StatusTooManyRequests {
			t.Errorf("boxed GET %s got %d, want 429", target, rec.Code)
		}
	}

	// Another address scans in peace.
	req := httptest.NewRequest(http.MethodGet, "/missing", nil)
	req.RemoteAddr = "203.0.113.10:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("another address got %d, want 404", rec.Code)
	}
}

func TestHealthyTrafficNeverBoxes(t *testing.T) {
	g := New(discardLogger())
	h := g.Wrap(probe())

	for i := 0; i < 10*maxMisses; i++ {
		if rec := request(t, h, "/valid"); rec.Code != http.StatusOK {
			t.Fatalf("request %d got %d, want 200", i, rec.Code)
		}
	}
	if rec := request(t, h, "/missing"); rec.Code != http.StatusNotFound {
		t.Errorf("a first miss after heavy healthy traffic got %d, want 404", rec.Code)
	}
}

func TestTheBoxOpensWhenTheCoolOffPasses(t *testing.T) {
	g := New(discardLogger())
	h := g.Wrap(probe())
	current := time.Now()
	g.now = func() time.Time { return current }

	for i := 0; i < maxMisses; i++ {
		request(t, h, "/missing")
	}
	if rec := request(t, h, "/valid"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("boxed request got %d, want 429", rec.Code)
	}

	current = current.Add(coolOff + time.Minute)
	if rec := request(t, h, "/missing"); rec.Code != http.StatusNotFound {
		t.Errorf("request after the cool-off got %d, want a served 404", rec.Code)
	}
	if rec := request(t, h, "/valid"); rec.Code != http.StatusOK {
		t.Errorf("request after the cool-off got %d, want 200", rec.Code)
	}
}

func TestSecurityHeadersAreStamped(t *testing.T) {
	g := New(discardLogger())
	rec := request(t, g.Wrap(probe()), "/valid")

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "deny",
		"Referrer-Policy":        "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("content-security-policy missing")
	}
}

func TestClientAddrPrefersTheForwardedHost(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:5555"
	if got := ClientAddr(req); got != "10.0.0.1" {
		t.Errorf("ClientAddr = %q, want the remote host", got)
	}

	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
	if got := ClientAddr(req); got != "203.0.113.5" {
		t.Errorf("ClientAddr = %q, want the forwarded client", got)
	}
}
