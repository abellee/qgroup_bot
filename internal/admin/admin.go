// Package admin is the back-end the operator uses to point the bot at a model.
// It is a small cookie-session panel over the same listener as the callback: one
// administrator account, one 模型 menu, and nothing else to click.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"qgroup-bot/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const (
	sessionCookie = "qgb_admin"
	sessionPath   = "/admin"
	sessionTTL    = 12 * time.Hour

	// maxFailedLogins is the point where an address has to wait. The panel is
	// reachable through the public host name, so guessing the administrator
	// password has to cost something.
	maxFailedLogins = 5
	loginLockout    = 10 * time.Minute
)

// Server holds the sessions in process memory: a restart asks the operator to
// log in again, which is the right trade against a session table that outlives a
// password change.
type Server struct {
	st       *store.Store
	log      *slog.Logger
	tmpl     *template.Template
	mu       sync.Mutex
	sess     map[string]*session
	throttle map[string]*failedLogins
}

type session struct {
	adminID  int64
	username string
	csrf     string
	expires  time.Time
}

type failedLogins struct {
	count int
	until time.Time
}

func New(st *store.Store, log *slog.Logger) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		st:       st,
		log:      log,
		tmpl:     tmpl,
		sess:     map[string]*session{},
		throttle: map[string]*failedLogins{},
	}, nil
}

// HashPassword turns a plain password into what belongs in the database, which
// is how the administrator from the env is stored at startup. bcrypt ignores
// everything past 72 bytes, so a longer secret is refused rather than silently
// weakened.
func HashPassword(plain string) (string, error) {
	if len(plain) == 0 {
		return "", errors.New("password is empty")
	}
	if len(plain) > 72 {
		return "", errors.New("password must be 72 bytes or fewer")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	return string(hash), err
}

// Handler is the panel mounted under /admin.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin", s.toModels)
	mux.HandleFunc("/admin/", s.toModels)
	mux.HandleFunc("/admin/login", s.handleLogin)
	mux.HandleFunc("/admin/logout", s.authed(s.handleLogout))
	mux.HandleFunc("/admin/models", s.authed(s.handleModels))
	mux.HandleFunc("/admin/models/form", s.authed(s.handleForm))
	mux.HandleFunc("/admin/models/save", s.authed(s.handleSave))
	mux.HandleFunc("/admin/models/enable", s.authed(s.handleEnable))
	mux.HandleFunc("/admin/models/delete", s.authed(s.handleDelete))
	return mux
}

func (s *Server) toModels(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/models", http.StatusFound)
}

// authed guards every page that is not the login form, and checks the csrf token
// a POST must carry.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := s.session(r)
		if sess == nil {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		if r.Method == http.MethodPost && !s.csrfOK(r, sess) {
			http.Error(w, "表单已过期，请重新提交", http.StatusBadRequest)
			return
		}
		next(w, r)
	}
}

func (s *Server) session(r *http.Request) *session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sess[c.Value]
	if !ok {
		return nil
	}
	if time.Now().After(sess.expires) {
		delete(s.sess, c.Value)
		return nil
	}
	return sess
}

// logout drops one session by cookie value.
func (s *Server) dropSession(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sess, c.Value)
		s.mu.Unlock()
	}
}

func (s *Server) csrfOK(r *http.Request, sess *session) bool {
	got := r.FormValue("csrf")
	return subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}

// startSession hands out the cookie. Secure follows the scheme the browser used
// - behind the front proxy that is the forwarded proto - so a plain http tunnel
// to 127.0.0.1 can still hold a session.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, adminID int64, username string) *session {
	token := randomToken()
	sess := &session{
		adminID:  adminID,
		username: username,
		csrf:     randomToken(),
		expires:  time.Now().Add(sessionTTL),
	}
	s.mu.Lock()
	s.sess[token] = sess
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     sessionPath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   requestIsHTTPS(r),
		Expires:  sess.expires,
		MaxAge:   int(sessionTTL / time.Second),
	})
	return sess
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.render(w, "login", &pageData{Title: "登录"})
		return
	}
	if err := r.ParseForm(); err != nil {
		s.render(w, "login", &pageData{Title: "登录", Err: "表单读取失败"})
		return
	}

	addr := clientAddr(r)
	data := &pageData{Title: "登录", Username: strings.TrimSpace(r.FormValue("username"))}

	if wait := s.lockedFor(addr); wait > 0 {
		data.Err = "失败次数过多，请 " + fmtDuration(wait) + " 后重试"
		s.render(w, "login", data)
		return
	}

	admin, err := s.st.AdminByUsername(data.Username)
	switch {
	case errors.Is(err, store.ErrNotFound):
		admin = nil
	case err != nil:
		s.log.Error("admin lookup failed", "error", err)
		data.Err = "登录暂时不可用"
		s.render(w, "login", data)
		return
	}

	if admin == nil || bcrypt.CompareHashAndPassword([]byte(admin.PassHash), []byte(r.FormValue("password"))) != nil {
		s.noteFailure(addr)
		data.Err = "用户名或密码不正确"
		s.render(w, "login", data)
		return
	}

	s.noteSuccess(addr)
	s.startSession(w, r, admin.ID, admin.Username)
	s.log.Info("admin login", "username", admin.Username, "addr", addr)
	http.Redirect(w, r, "/admin/models", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.dropSession(r)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: sessionPath, MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

// lockedFor reports how long this address still has to wait, or 0.
func (s *Server) lockedFor(addr string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, ok := s.throttle[addr]
	if !ok || f.count < maxFailedLogins {
		return 0
	}
	if wait := time.Until(f.until); wait > 0 {
		return wait
	}
	delete(s.throttle, addr)
	return 0
}

func (s *Server) noteFailure(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f := s.throttle[addr]
	if f == nil {
		f = &failedLogins{}
		s.throttle[addr] = f
	}
	f.count++
	if f.count >= maxFailedLogins {
		f.until = time.Now().Add(loginLockout)
	}
}

func (s *Server) noteSuccess(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.throttle, addr)
}

func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand has no failure mode worth branching on here; a panic would
		// be louder than a session that cannot be trusted.
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// maskKey keeps a stored key recognizable without showing it.
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "未设置"
	}
	runes := []rune(key)
	if len(runes) <= 8 {
		return "••••••••"
	}
	return string(runes[:3]) + "…" + string(runes[len(runes)-3:])
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))), "https")
}

// clientAddr prefers the forwarded address because the panel only ever sees the
// front proxy as RemoteAddr.
func clientAddr(r *http.Request) string {
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
