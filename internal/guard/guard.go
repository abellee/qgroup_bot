// Package guard fronts the whole service against crawlers and vulnerability
// scanners. It stamps the security headers every response should carry, counts
// the not-found misses each address rings up, and puts the addresses that probe
// too hard into a timeout box. Nothing here can leak the panel's secret path:
// a probe is answered like any other miss, the address just stops getting
// replies after a while.
package guard

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// maxMisses is how many 404s one address may ring up inside missWindow
	// before the box closes. A human mis-typing the panel path stays far
	// below it; a content-discovery scan blows through it in seconds.
	maxMisses = 30

	missWindow = 5 * time.Minute
	coolOff    = 15 * time.Minute

	// purgeThreshold is the map size that triggers a sweep of expired
	// records, so a long-running scan cannot grow the table without bound.
	purgeThreshold = 4096
)

// Guard is an http.Handler wrapper. Its state lives in process memory: a
// restart forgets who was boxed, which costs a scanner a fresh budget and an
// operator nothing.
type Guard struct {
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	records map[string]*record
}

type record struct {
	count       int
	windowStart time.Time
	boxedUntil  time.Time
}

func New(log *slog.Logger) *Guard {
	return &Guard{
		log:     log,
		now:     time.Now,
		records: map[string]*record{},
	}
}

// Wrap puts the mux behind the guard. The headers land before the handler
// runs, so one that sets its own wins and one that does not inherits.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.stamp(w.Header())
		addr := ClientAddr(r)
		if g.boxed(addr) {
			w.Header().Set("retry-after", strconv.Itoa(int(coolOff/time.Second)))
			http.Error(w, "请求过于频繁，请稍后再来", http.StatusTooManyRequests)
			return
		}

		tw := &trackWriter{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(tw, r)
		if tw.code == http.StatusNotFound {
			g.miss(addr)
		}
	})
}

func (g *Guard) stamp(h http.Header) {
	h.Set("x-content-type-options", "nosniff")
	h.Set("x-frame-options", "deny")
	h.Set("referrer-policy", "no-referrer")
	// The panel is a same-origin app: nothing may frame it, scripts load only
	// from the bundle's own host - plus Cloudflare's, because the login form
	// embeds their Turnstile widget.
	h.Set("content-security-policy",
		"default-src 'self'; script-src 'self' https://challenges.cloudflare.com; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data:; "+
			"frame-src https://challenges.cloudflare.com; frame-ancestors 'none'; "+
			"base-uri 'none'; form-action 'self'")
}

func (g *Guard) boxed(addr string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec, ok := g.records[addr]
	return ok && g.now().Before(rec.boxedUntil)
}

func (g *Guard) miss(addr string) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()

	rec, ok := g.records[addr]
	if !ok || now.Sub(rec.windowStart) >= missWindow {
		rec = &record{windowStart: now}
		g.records[addr] = rec
	}
	rec.count++
	if rec.count < maxMisses {
		return
	}
	rec.count = 0
	rec.windowStart = now
	rec.boxedUntil = now.Add(coolOff)
	g.log.Warn("address boxed for scanning", "addr", addr, "cool_off", coolOff.String())

	if len(g.records) > purgeThreshold {
		for a, r := range g.records {
			if now.After(r.boxedUntil) && now.Sub(r.windowStart) >= missWindow {
				delete(g.records, a)
			}
		}
	}
}

// ClientAddr prefers the forwarded address, because behind the front proxy
// every request's RemoteAddr is the proxy itself. Behind Cloudflare the edge
// names the real client in CF-Connecting-IP, which wins outright: a client can
// stuff the first hop of X-Forwarded-For, and the login throttle must not be
// fooled by that.
func ClientAddr(r *http.Request) string {
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		return cf
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if first, _, _ := strings.Cut(fwd, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// trackWriter remembers the status a handler answered with, which is the only
// signal the miss counter needs.
type trackWriter struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (w *trackWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.code, w.wrote = code, true
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.code, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer's deadlines, which
// the panel's slow model calls extend.
func (w *trackWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
