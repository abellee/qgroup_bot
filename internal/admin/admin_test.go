package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/crypto/bcrypt"

	"qgroup-bot/internal/store"
)

const (
	testUser = "admin"
	testPass = "a-correct-horse"
)

func newTestServer(t *testing.T) (*store.Store, *Server) {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hash, err := HashPassword(testPass)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := st.CreateAdmin(testUser, hash); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	srv, err := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// The bundle inside the binary is whatever this checkout built; tests that
	// care about the app files install their own.
	srv.dist = fstest.MapFS{}
	// The mount path is random per store; the suite speaks /admin, so it is
	// pinned here in a way an operator cannot.
	srv.path = "/admin"
	return st, srv
}

// client is the app's side of the contract: one cookie, one csrf token, and a
// JSON body per mutation.
type client struct {
	t      *testing.T
	srv    *Server
	cookie string
	csrf   string
}

func newClient(t *testing.T, srv *Server) *client { return &client{t: t, srv: srv} }

func (c *client) do(method, target string, body any) *httptest.ResponseRecorder {
	c.t.Helper()

	var r io.Reader
	if body != nil {
		r = bytes.NewReader([]byte(mustJSON(c.t, body)))
	}
	req := httptest.NewRequest(method, target, r)
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.csrf != "" && method != http.MethodGet {
		req.Header.Set(csrfHeader, c.csrf)
	}
	if c.cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.cookie})
	}
	req.RemoteAddr = "10.0.0.1:5555"

	rec := httptest.NewRecorder()
	c.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func jsonOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not a json object (%v): %s", err, rec.Body)
	}
	return out
}

func errText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return strings.TrimSpace(jsonOf(t, rec)["error"].(string))
}

func logIn(t *testing.T, c *client) {
	t.Helper()

	rec := c.do(http.MethodPost, "/admin/api/login", map[string]string{"username": testUser, "password": testPass})
	if rec.Code != http.StatusOK {
		t.Fatalf("login got %d: %s", rec.Code, rec.Body)
	}
	body := jsonOf(t, rec)
	if body["authenticated"] != true || body["username"] != testUser {
		t.Fatalf("login body = %v, want an authenticated session", body)
	}
	csrf, _ := body["csrf"].(string)
	if csrf == "" {
		t.Fatal("no csrf token in the login response")
	}
	c.csrf = csrf
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookie && ck.Value != "" {
			c.cookie = ck.Value
		}
	}
	if c.cookie == "" {
		t.Fatal("login did not hand out a session cookie")
	}
}

func rowsOf(t *testing.T, c *client) []any {
	t.Helper()
	rec := c.do(http.MethodGet, "/admin/api/models", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list got %d: %s", rec.Code, rec.Body)
	}
	models, _ := jsonOf(t, rec)["models"].([]any)
	return models
}

func TestPanelPathIsDrawnOnceAndSticks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	hash, err := HashPassword(testPass)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := st.CreateAdmin(testUser, hash); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	mk := func() *Server {
		t.Helper()
		srv, err := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("new server: %v", err)
		}
		srv.dist = fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte(`<div id="app">`)}}
		return srv
	}

	first := mk()
	if !strings.HasPrefix(first.Path(), "/") || len(first.Path()) != 1+panelPathLen {
		t.Errorf("panel path = %q, want a %d-hex segment under /", first.Path(), panelPathLen)
	}
	if first.Path() == "/admin" {
		t.Error("the panel kept the predictable path it used to have")
	}

	// A restart reads the same path back out of the store.
	if again := mk(); again.Path() != first.Path() {
		t.Errorf("restarted path = %q, want the stored %q", again.Path(), first.Path())
	}

	// The drawn path serves the app under itself.
	h := first.Handler()
	rec := getStatic(t, h, first.Path()+"/")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="app">`) {
		t.Errorf("GET %s/ got %d: %s", first.Path(), rec.Code, rec.Body)
	}

	// The session cookie is scoped to the drawn path, not to the whole host.
	rec = loginRequest(t, h, nil, first.Path()+"/api/login")
	if set := rec.Header().Get("Set-Cookie"); !strings.Contains(set, "Path="+first.Path()) {
		t.Errorf("cookie = %s, want Path=%s", set, first.Path())
	}
}

func TestHashPasswordRefusesUnusableSecrets(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Error("empty password accepted")
	}
	if _, err := HashPassword(strings.Repeat("x", 73)); err == nil {
		t.Error("password past the bcrypt limit accepted")
	}

	hash, err := HashPassword(testPass)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == testPass {
		t.Fatal("password stored in the clear")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPass)); err != nil {
		t.Errorf("hash does not verify: %v", err)
	}
}

func TestMaskKeyStaysUnreadable(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "未设置"},
		{"   ", "未设置"},
		{"short", "••••••••"},
		{"sk-abcdef123456", "sk-…456"},
	} {
		if got := maskKey(tc.in); got != tc.want {
			t.Errorf("maskKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestEveryDataRouteRefusesAnAnonymousCaller(t *testing.T) {
	_, srv := newTestServer(t)
	c := newClient(t, srv)

	cases := []struct {
		method, target string
		body           any
	}{
		{http.MethodGet, "/admin/api/models", nil},
		{http.MethodGet, "/admin/api/providers", nil},
		{http.MethodPost, "/admin/api/models", map[string]any{"provider": "openai"}},
		{http.MethodPost, "/admin/api/models/enable", map[string]any{"id": 1}},
		{http.MethodPost, "/admin/api/models/delete", map[string]any{"id": 1}},
		{http.MethodPost, "/admin/api/logout", map[string]any{}},
	}
	for _, tc := range cases {
		rec := c.do(tc.method, tc.target, tc.body)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s got %d, want 401 (%s)", tc.method, tc.target, rec.Code, rec.Body)
		}
	}

	// The session probe is the exception: a logged-out answer is the norm.
	rec := c.do(http.MethodGet, "/admin/api/session", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session probe got %d: %s", rec.Code, rec.Body)
	}
	if jsonOf(t, rec)["authenticated"] != false {
		t.Errorf("session probe = %s, want authenticated:false", rec.Body)
	}
}

func TestLoginAndLogout(t *testing.T) {
	_, srv := newTestServer(t)
	c := newClient(t, srv)

	rec := c.do(http.MethodPost, "/admin/api/login", map[string]string{"username": testUser, "password": "wrong"})
	if rec.Code != http.StatusUnauthorized || !strings.Contains(errText(t, rec), "用户名或密码不正确") {
		t.Fatalf("wrong password got %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Error("a session was handed out for a bad password")
	}

	// A password longer than bcrypt reads is refused instead of truncated.
	rec = c.do(http.MethodPost, "/admin/api/login", map[string]string{"username": testUser, "password": strings.Repeat("x", 73)})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("over-long password got %d, want 401", rec.Code)
	}

	logIn(t, c)

	rec = c.do(http.MethodGet, "/admin/api/session", nil)
	if got := jsonOf(t, rec); got["username"] != testUser || got["csrf"] != c.csrf {
		t.Errorf("session probe = %v, want the live session", got)
	}

	rec = c.do(http.MethodPost, "/admin/api/logout", map[string]any{})
	if rec.Code != http.StatusOK || jsonOf(t, rec)["authenticated"] != false {
		t.Fatalf("logout got %d: %s", rec.Code, rec.Body)
	}
	if rec = c.do(http.MethodGet, "/admin/api/models", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("the dropped cookie still works (got %d)", rec.Code)
	}
}

func TestRepeatedFailuresLockTheAddressOut(t *testing.T) {
	_, srv := newTestServer(t)
	h := srv.Handler()

	attempt := func(addr, password string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/login",
			strings.NewReader(`{"username":"`+testUser+`","password":"`+password+`"}`))
		req.Header.Set("content-type", "application/json")
		req.RemoteAddr = addr + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < maxFailedLogins; i++ {
		if rec := attempt("203.0.113.9", "wrong"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d got %d, want 401", i, rec.Code)
		}
	}
	rec := attempt("203.0.113.9", "wrong")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(errText(t, rec), "失败次数过多") {
		t.Fatalf("lockout got %d: %s, want 429 with the wait", rec.Code, rec.Body)
	}
	// The lockout is checked before the credentials, so the right password does
	// not help while the window is open.
	if rec := attempt("203.0.113.9", testPass); rec.Code != http.StatusTooManyRequests {
		t.Errorf("locked address with the right password got %d, want 429", rec.Code)
	}
	if rec := attempt("203.0.113.10", testPass); rec.Code != http.StatusOK {
		t.Errorf("another address got %d, want a session", rec.Code)
	}

	// The wait the message promises is the one the server actually holds.
	if got := srv.lockedFor("203.0.113.9"); got <= 0 || got > loginLockout {
		t.Errorf("lockedFor = %v, want the rest of %v", got, loginLockout)
	}
}

func TestMutationsNeedTheSessionToken(t *testing.T) {
	st, srv := newTestServer(t)
	c := newClient(t, srv)
	logIn(t, c)

	good := map[string]any{"id": 0, "provider": "openai", "base_url": "https://x", "api_key": "k", "model": "m"}

	for name, token := range map[string]string{
		"no token": "",
		"wrong":    strings.Repeat("0", 64),
	} {
		c.csrf = token
		rec := c.do(http.MethodPost, "/admin/api/models", good)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s got %d, want 403 (%s)", name, rec.Code, rec.Body)
		}
		logIn(t, c)
	}

	if rows, err := st.ListModels(); err != nil || len(rows) != 0 {
		t.Errorf("a refused request still wrote %d rows (err %v)", len(rows), err)
	}
}

func TestStoredKeyNeverLeavesTheAPI(t *testing.T) {
	st, srv := newTestServer(t)
	c := newClient(t, srv)
	logIn(t, c)

	const key = "sk-live-abcdef1234567890"
	cfg := &store.ModelConfig{Provider: store.ProviderOpenAI, BaseURL: "https://api.example.com", APIKey: key, Model: "gpt-test"}
	if err := st.SaveModel(cfg); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Both the list and the row the save call returns must carry the mask only.
	bodies := []string{c.do(http.MethodGet, "/admin/api/models", nil).Body.String()}

	rec := c.do(http.MethodPost, "/admin/api/models", map[string]any{
		"id": cfg.ID, "name": "改名", "provider": store.ProviderOpenAI,
		"base_url": "https://api.example.com", "api_key": "", "model": "gpt-test",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit got %d: %s", rec.Code, rec.Body)
	}
	bodies = append(bodies, rec.Body.String())

	for _, body := range bodies {
		if strings.Contains(body, key) {
			t.Errorf("response leaked the stored key: %s", body)
		}
		if !strings.Contains(body, maskKey(key)) {
			t.Errorf("response has no mask for the key: %s", body)
		}
	}
	for _, field := range []string{"api_key", "pass_hash", "password"} {
		if strings.Contains(bodies[0], `"`+field+`"`) {
			t.Errorf("the list response carries a %q field", field)
		}
	}

	// The blank key field was an instruction, not data: the secret stayed.
	again, err := st.GetModel(cfg.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.APIKey != key {
		t.Errorf("stored key = %q, want the one that was already there", again.APIKey)
	}
}

func TestModelRoundTripThroughTheAPI(t *testing.T) {
	st, srv := newTestServer(t)
	c := newClient(t, srv)
	logIn(t, c)

	rec := c.do(http.MethodPost, "/admin/api/models", map[string]any{
		"id": 0, "name": "备用", "provider": "Anthropic", "base_url": "https://api.anthropic.com",
		"api_key": "sk-ant-1234567890", "model": "claude-test", "persona": "回答要短",
		"temperature": 0.3, "max_tokens": 512, "timeout_ms": 30000, "enabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create got %d: %s", rec.Code, rec.Body)
	}
	created := jsonOf(t, rec)
	if created["provider"] != store.ProviderAnthropic {
		t.Errorf("provider = %v, want the lowercased choice", created["provider"])
	}
	if created["enabled"] != true {
		t.Error("the checked row came back inactive")
	}
	if created["key_given"] != true {
		t.Error("key_given is false for a row that has a key")
	}
	id := int64(created["id"].(float64))
	if id == 0 {
		t.Error("create did not report the new id")
	}
	stamp, err := time.Parse(time.RFC3339, created["updated_at"].(string))
	if err != nil {
		t.Errorf("updated_at = %v, want RFC3339 the app can format: %v", created["updated_at"], err)
	}
	if stamp.Before(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("updated_at = %v, want the save time rather than a zero timestamp", created["updated_at"])
	}

	// Enabling a second row retires the first, in one write.
	rec = c.do(http.MethodPost, "/admin/api/models", map[string]any{
		"id": 0, "name": "主用", "provider": "gemini", "base_url": "https://generativelanguage.googleapis.com",
		"api_key": "AIza-1234567890", "model": "gemini-test", "temperature": 1,
		"max_tokens": 1024, "timeout_ms": 45000, "enabled": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("second create got %d: %s", rec.Code, rec.Body)
	}
	main := jsonOf(t, rec)
	mainID := int64(main["id"].(float64))

	active, ok, err := st.ActiveModel()
	if err != nil || !ok || active.Name != "主用" {
		t.Fatalf("active = %+v (ok %v, err %v), want 主用", active, ok, err)
	}

	rec = c.do(http.MethodPost, "/admin/api/models/enable", map[string]any{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable got %d: %s", rec.Code, rec.Body)
	}
	if got := jsonOf(t, rec)["active_id"]; got != float64(id) {
		t.Errorf("active_id = %v, want %d", got, id)
	}
	if enabled := countEnabled(t, c); enabled != 1 {
		t.Errorf("enabled rows = %d, want exactly one", enabled)
	}

	// Defaults belong to the server, not to a placeholder in the form.
	rec = c.do(http.MethodPost, "/admin/api/models", map[string]any{
		"id": mainID, "name": "主用", "provider": "openai", "base_url": "https://api.openai.com/v1",
		"api_key": "k2", "model": "gpt-test", "temperature": 0.7, "max_tokens": 0, "timeout_ms": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update got %d: %s", rec.Code, rec.Body)
	}
	updated := jsonOf(t, rec)
	if updated["max_tokens"] != float64(1024) || updated["timeout_ms"] != float64(45000) {
		t.Errorf("zero knobs = %v/%v, want the 1024 and 45000 defaults", updated["max_tokens"], updated["timeout_ms"])
	}
	if updated["enabled"] != false {
		t.Error("an update that left the checkbox out kept the row active")
	}

	// The two one-field calls refuse a row that is not there.
	for _, target := range []string{"/admin/api/models/enable", "/admin/api/models/delete"} {
		rec := c.do(http.MethodPost, target, map[string]any{"id": 9999})
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s for a missing id got %d, want 404", target, rec.Code)
		}
		if rec = c.do(http.MethodPost, target, map[string]any{"id": 0}); rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s without an id got %d, want 400", target, rec.Code)
		}
	}

	rec = c.do(http.MethodPost, "/admin/api/models/delete", map[string]any{"id": id})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete got %d: %s", rec.Code, rec.Body)
	}
	if _, err := st.GetModel(id); err != store.ErrNotFound {
		t.Errorf("GetModel after delete = %v, want ErrNotFound", err)
	}
	c.do(http.MethodPost, "/admin/api/models/delete", map[string]any{"id": mainID})
	if rows := rowsOf(t, c); len(rows) != 0 {
		t.Errorf("rows left = %v, want none", rows)
	}
}

func countEnabled(t *testing.T, c *client) int {
	t.Helper()
	var n int
	for _, raw := range rowsOf(t, c) {
		if row, _ := raw.(map[string]any); row["enabled"] == true {
			n++
		}
	}
	return n
}

func TestIncompleteRowIsRefusedByName(t *testing.T) {
	st, srv := newTestServer(t)
	c := newClient(t, srv)
	logIn(t, c)

	rec := c.do(http.MethodPost, "/admin/api/models", map[string]any{
		"id": 0, "provider": "mistral", "base_url": "", "api_key": "", "model": "", "persona": "说人话",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete save got %d: %s, want 400", rec.Code, rec.Body)
	}
	msg := errText(t, rec)
	for _, want := range []string{"name", "provider", "api base url", "api key", "model"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want %q listed", msg, want)
		}
	}
	if rows, _ := st.ListModels(); len(rows) != 0 {
		t.Errorf("a refused row was stored anyway: %+v", rows)
	}
}

func TestBadRequestsAreRejectedBeforeTheStore(t *testing.T) {
	st, srv := newTestServer(t)
	c := newClient(t, srv)
	logIn(t, c)

	cases := map[string]struct {
		body  string
		field string
	}{
		"empty body":            {body: "", field: "请求体为空"},
		"not json":              {body: `<html>`, field: "请求格式不正确"},
		"unknown field":         {body: `{"id":0,"api_key":"k","key_masked":"sneaky"}`, field: "key_masked"},
		"array instead":         {body: `[{"id":1}]`, field: "请求格式不正确"},
		"id in the wrong place": {body: `{"id":"abc"}`, field: "请求格式不正确"},
	}
	for name, tc := range cases {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/models", strings.NewReader(tc.body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set(csrfHeader, c.csrf)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.cookie})
		req.RemoteAddr = "10.0.0.1:1"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s got %d: %s, want 400", name, rec.Code, rec.Body)
			continue
		}
		if !strings.Contains(errText(t, rec), tc.field) {
			t.Errorf("%s error = %q, want it to name %q", name, errText(t, rec), tc.field)
		}
	}
	if rows, _ := st.ListModels(); len(rows) != 0 {
		t.Errorf("malformed requests wrote %d rows", len(rows))
	}
}

func TestReadOnlyRoutesRefuseOtherMethods(t *testing.T) {
	_, srv := newTestServer(t)
	c := newClient(t, srv)
	logIn(t, c)

	rec := c.do(http.MethodGet, "/admin/api/login", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET the login endpoint got %d, want 405", rec.Code)
	}
	for _, target := range []string{"/admin/api/models", "/admin/api/providers", "/admin/api/session"} {
		rec := c.do(http.MethodDelete, target, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s got %d, want 405", http.MethodDelete, target, rec.Code)
		}
	}
}

func TestProvidersComeFromTheStore(t *testing.T) {
	_, srv := newTestServer(t)
	c := newClient(t, srv)
	logIn(t, c)

	rec := c.do(http.MethodGet, "/admin/api/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("providers got %d: %s", rec.Code, rec.Body)
	}
	anyOf := jsonOf(t, rec)["providers"].([]any)
	if len(anyOf) != 3 {
		t.Fatalf("providers = %v, want the three the store validates", anyOf)
	}
	var values []string
	for _, p := range anyOf {
		opt := p.(map[string]any)
		values = append(values, opt["value"].(string))
		if opt["label"] == "" {
			t.Errorf("provider %v has no label for the dropdown", opt)
		}
		if !store.ValidProvider(opt["value"].(string)) {
			t.Errorf("provider %v would be rejected by the store", opt["value"])
		}
	}
	for _, want := range []string{"openai", "anthropic", "gemini"} {
		if !strings.Contains(strings.Join(values, ","), want) {
			t.Errorf("providers = %v, want %s in the list", values, want)
		}
	}
}

func TestSessionCookieFlagsFollowTheProxyScheme(t *testing.T) {
	_, srv := newTestServer(t)
	h := srv.Handler()

	// Over plain http (a tunnel to 127.0.0.1) Secure must stay off, or the
	// browser drops the cookie and the panel looks broken.
	rec := loginRequest(t, h, nil, "/admin/api/login")
	if rec.Code != http.StatusOK {
		t.Fatalf("login got %d: %s", rec.Code, rec.Body)
	}
	if set := rec.Header().Get("Set-Cookie"); strings.Contains(set, "Secure") {
		t.Errorf("plain-http session cookie marked Secure: %s", set)
	}
	for _, want := range []string{"HttpOnly", "SameSite=Lax", "Path=/admin"} {
		if !strings.Contains(rec.Header().Get("Set-Cookie"), want) {
			t.Errorf("cookie missing %q: %s", want, rec.Header().Get("Set-Cookie"))
		}
	}

	// Behind the front proxy the forwarded proto is what the browser used.
	rec = loginRequest(t, h, http.Header{"X-Forwarded-Proto": {"https"}}, "/admin/api/login")
	set := rec.Header().Get("Set-Cookie")
	if !strings.Contains(set, "Secure") {
		t.Errorf("proxied cookie missing Secure: %s", set)
	}
}

func loginRequest(t *testing.T, h http.Handler, header http.Header, target string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, target,
		strings.NewReader(`{"username":"`+testUser+`","password":"`+testPass+`"}`))
	req.Header.Set("content-type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	req.RemoteAddr = "10.0.0.2:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestStaticBundleIsServedFromTheBinary(t *testing.T) {
	_, srv := newTestServer(t)
	h := srv.Handler()

	// Nothing built into dist: the panel must say how to build it rather than
	// serve a blank page that looks like a crash.
	rec := getStatic(t, h, "/admin/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty bundle got %d: %s, want 503", rec.Code, rec.Body)
	}

	srv.dist = buildFS()

	if rec = getStatic(t, h, "/admin/"); rec.Code != http.StatusOK {
		t.Fatalf("index got %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("content-type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index content-type = %q", ct)
	}
	if rec.Header().Get("cache-control") != "no-store" {
		t.Errorf("index cache-control = %q, want no-store so a replaced bundle loads immediately", rec.Header().Get("cache-control"))
	}
	if !strings.Contains(rec.Body.String(), "<div id=\"app\">") {
		t.Errorf("index body = %s", rec.Body)
	}

	// Hashed assets are safe to cache forever; the entry document is not.
	rec = getStatic(t, h, "/admin/assets/app-abc123.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset got %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("content-type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset content-type = %q", ct)
	}
	if cc := rec.Header().Get("cache-control"); !strings.Contains(cc, "immutable") {
		t.Errorf("asset cache-control = %q", cc)
	}

	// A missing asset is a 404, not the app: a stale index.html referencing a
	// hashed file would otherwise mask the problem.
	if rec = getStatic(t, h, "/admin/assets/gone-abc123.css"); rec.Code != http.StatusNotFound {
		t.Errorf("missing asset got %d, want 404", rec.Code)
	}
	// An extension-less path is the app's own route.
	if rec = getStatic(t, h, "/admin/anything"); rec.Code != http.StatusOK {
		t.Errorf("spa fallback got %d, want index.html", rec.Code)
	}

	// The bare path redirects so relative asset URLs resolve.
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/" {
		t.Errorf("/admin got %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	if rec = getStaticMethod(t, h, http.MethodPost, "/admin/"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST a static path got %d, want 405", rec.Code)
	}
}

func buildFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":           &fstest.MapFile{Data: []byte(`<!doctype html><div id="app"></div>`)},
		"assets/app-abc123.js": &fstest.MapFile{Data: []byte(`console.log(1)`), Mode: 0},
	}
}

func getStatic(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	return getStaticMethod(t, h, http.MethodGet, target)
}

func getStaticMethod(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
