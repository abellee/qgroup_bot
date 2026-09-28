package admin

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"qgroup-bot/internal/store"
)

const (
	testUser = "admin"
	testPass = "a-correct-horse"
)

func newTestServer(t *testing.T) (*store.Store, http.Handler) {
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
	return st, srv.Handler()
}

// logIn returns the session cookie plus the csrf token the page renders, the way
// a browser would pick them up.
func logIn(t *testing.T, h http.Handler) (string, string) {
	t.Helper()

	rec := postForm(t, h, "/admin/login", "", "", url.Values{
		"username": {testUser},
		"password": {testPass},
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("login got %d body=%s", rec.Code, rec.Body)
	}
	cookie := sessionCookieOf(t, rec)
	return cookie, csrfOf(t, get(t, h, cookie, "/admin/models"))
}

func get(t *testing.T, h http.Handler, cookie, target string) string {
	t.Helper()
	rec := do(t, h, http.MethodGet, target, cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s got %d body=%s", target, rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func postForm(t *testing.T, h http.Handler, target, cookie, csrf string, v url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if v == nil {
		v = url.Values{}
	}
	if csrf != "" {
		v.Set("csrf", csrf)
	}
	return do(t, h, http.MethodPost, target, cookie, v)
}

func do(t *testing.T, h http.Handler, method, target, cookie string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	}
	req.RemoteAddr = "10.0.0.1:5555"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookieOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie in response", sessionCookie)
	return ""
}

var csrfPattern = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func csrfOf(t *testing.T, page string) string {
	t.Helper()
	m := csrfPattern.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no csrf token rendered in %s", page)
	}
	return m[1]
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

func TestEveryPanelRouteNeedsASession(t *testing.T) {
	_, h := newTestServer(t)

	for _, target := range []string{"/admin", "/admin/", "/admin/models", "/admin/models/form", "/admin/logout"} {
		rec := do(t, h, http.MethodGet, target, "", nil)
		if rec.Code != http.StatusFound {
			t.Errorf("GET %s got %d, want redirect", target, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/admin/models" && got != "/admin/login" {
			t.Errorf("GET %s redirected to %q", target, got)
		}
	}

	// Writes are refused before the handler ever sees the form.
	rec := do(t, h, http.MethodPost, "/admin/models/save", "", url.Values{"csrf": {"guess"}})
	if rec.Code != http.StatusFound {
		t.Fatalf("POST without session got %d, want redirect to login", rec.Code)
	}
}

func TestLoginRejectsBadCredentialsAndEndsTheSession(t *testing.T) {
	_, h := newTestServer(t)

	rec := postForm(t, h, "/admin/login", "", "", url.Values{"username": {testUser}, "password": {"wrong"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("wrong password got %d, want the form back", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "用户名或密码不正确") {
		t.Errorf("no login error rendered: %s", rec.Body)
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Errorf("session handed out for a bad password: %v", rec.Header().Values("Set-Cookie"))
	}

	cookie, csrf := logIn(t, h)
	if !strings.Contains(get(t, h, cookie, "/admin/models"), testUser) {
		t.Error("logged-in page does not name the administrator")
	}

	rec = postForm(t, h, "/admin/logout", cookie, csrf, nil)
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/login" {
		t.Fatalf("logout got %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = do(t, h, http.MethodGet, "/admin/models", cookie, nil)
	if rec.Code != http.StatusFound {
		t.Errorf("cookie still works after logout (got %d)", rec.Code)
	}
}

func TestRepeatedFailuresLockTheAddressOut(t *testing.T) {
	_, h := newTestServer(t)

	attempt := func(addr string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(url.Values{
			"username": {testUser},
			"password": {"wrong"},
		}.Encode()))
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
		req.RemoteAddr = addr + ":1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := 0; i < maxFailedLogins; i++ {
		if rec := attempt("203.0.113.9"); !strings.Contains(rec.Body.String(), "用户名或密码不正确") {
			t.Fatalf("failure %d did not report a bad login: %s", i, rec.Body)
		}
	}
	if rec := attempt("203.0.113.9"); !strings.Contains(rec.Body.String(), "失败次数过多") {
		t.Fatalf("lockout not enforced: %s", rec.Body)
	}
	// The lockout is checked before the credentials, so the right password does
	// not help either while the window is open.
	rec := attemptWithPassword(t, h, "203.0.113.9", testPass)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "失败次数过多") {
		t.Errorf("locked address got %d, want the throttled login form", rec.Code)
	}

	// Another address is unaffected.
	if rec := attemptWithPassword(t, h, "203.0.113.10", testPass); rec.Code != http.StatusFound {
		t.Errorf("unlocked address got %d, want a session", rec.Code)
	}
}

func attemptWithPassword(t *testing.T, h http.Handler, addr, password string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(url.Values{
		"username": {testUser},
		"password": {password},
	}.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.RemoteAddr = addr + ":1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWritesNeedTheSessionsCSRFToken(t *testing.T) {
	st, h := newTestServer(t)
	cookie, csrf := logIn(t, h)

	for _, tc := range []struct{ name, token string }{
		{"missing", ""},
		{"wrong", strings.Repeat("0", len(csrf))},
	} {
		v := url.Values{"id": {"0"}, "provider": {"openai"}, "base_url": {"https://x"}, "api_key": {"k"}, "model": {"m"}}
		if tc.token != "" {
			v.Set("csrf", tc.token)
		}
		if rec := do(t, h, http.MethodPost, "/admin/models/save", cookie, v); rec.Code != http.StatusBadRequest {
			t.Errorf("%s csrf got %d body=%s, want 400", tc.name, rec.Code, rec.Body)
		}
	}
	if rows, err := st.ListModels(); err != nil || len(rows) != 0 {
		t.Errorf("a request without a valid token wrote %d rows", len(rows))
	}
}

func TestStoredKeyOnlyEverAppearsMasked(t *testing.T) {
	st, h := newTestServer(t)
	cookie, _ := logIn(t, h)
	const key = "sk-live-abcdef1234567890"

	cfg := &store.ModelConfig{Provider: "openai", BaseURL: "https://api.example.com", APIKey: key, Model: "gpt-test"}
	if err := st.SaveModel(cfg); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	pages := map[string]string{
		"list": get(t, h, cookie, "/admin/models"),
		"edit": get(t, h, cookie, "/admin/models/form?id="+itoa(cfg.ID)),
	}
	for name, body := range pages {
		if strings.Contains(body, key) {
			t.Errorf("%s page leaked the stored key", name)
		}
		if !strings.Contains(body, maskKey(key)) {
			t.Errorf("%s page does not show the mask %q: %s", name, maskKey(key), body)
		}
	}
	if !strings.Contains(pages["edit"], `value=""`) {
		t.Error("edit form carries a key value instead of an empty field")
	}
}

func TestModelRoundTripThroughThePanel(t *testing.T) {
	st, h := newTestServer(t)
	cookie, csrf := logIn(t, h)

	save := func(v url.Values) string {
		t.Helper()
		rec := postForm(t, h, "/admin/models/save", cookie, csrf, v)
		if rec.Code != http.StatusFound {
			t.Fatalf("save got %d body=%s", rec.Code, rec.Body)
		}
		return rec.Header().Get("Location")
	}

	first := url.Values{
		"id": {"0"}, "name": {"备用"}, "provider": {"Anthropic"}, "base_url": {"https://api.anthropic.com"},
		"api_key": {"sk-ant-1234567890"}, "model": {"claude-test"}, "persona": {"回答要短"},
		"temperature": {"0.3"}, "max_tokens": {"512"}, "timeout_ms": {"30000"}, "enabled": {"1"},
	}
	if loc := save(first); loc != "/admin/models?flash=saved" {
		t.Fatalf("save redirected to %q", loc)
	}

	rows, err := st.ListModels()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("saved %d rows, want 1", len(rows))
	}
	id := rows[0].ID
	if rows[0].Provider != store.ProviderAnthropic {
		t.Errorf("provider = %q, want the lowercased choice", rows[0].Provider)
	}
	if !rows[0].Enabled {
		t.Error("checked row was not stored active")
	}

	// A second active row steals the flag from the first.
	second := url.Values{
		"id": {"0"}, "name": {"主用"}, "provider": {"gemini"}, "base_url": {"https://generativelanguage.googleapis.com"},
		"api_key": {"AIza-1234567890"}, "model": {"gemini-test"}, "temperature": {"1"}, "enabled": {"1"},
	}
	save(second)
	active, ok, err := st.ActiveModel()
	if err != nil || !ok {
		t.Fatalf("active model: ok=%v err=%v", ok, err)
	}
	if active.Name != "主用" {
		t.Errorf("active row is %q, want the newest one", active.Name)
	}

	// Editing with a blank key field keeps the stored secret.
	if loc := save(url.Values{
		"id": {itoa(id)}, "name": {"备用改"}, "provider": {"openai"}, "base_url": {"https://api.openai.com/v1"},
		"api_key": {"  "}, "model": {"gpt-test"}, "temperature": {"0.7"}, "max_tokens": {"bad"},
	}); loc == "" {
		t.Fatal("edit did not redirect")
	}
	edited, err := st.GetModel(id)
	if err != nil {
		t.Fatalf("get edited: %v", err)
	}
	if edited.APIKey != "sk-ant-1234567890" {
		t.Errorf("blank key field wiped the stored key: %q", edited.APIKey)
	}
	if edited.MaxTokens != 1024 {
		t.Errorf("unparsable max_tokens = %d, want the default", edited.MaxTokens)
	}
	if edited.Enabled {
		t.Error("an edit that left the checkbox out kept the row active")
	}

	rec := postForm(t, h, "/admin/models/enable", cookie, csrf, url.Values{"id": {itoa(id)}})
	if rec.Header().Get("Location") != "/admin/models?flash=enabled" {
		t.Fatalf("enable redirected to %q", rec.Header().Get("Location"))
	}
	if active, _, _ := st.ActiveModel(); active.ID != id {
		t.Error("enable did not make this row the active one")
	}

	rec = postForm(t, h, "/admin/models/delete", cookie, csrf, url.Values{"id": {itoa(id)}})
	if rec.Header().Get("Location") != "/admin/models?flash=deleted" {
		t.Fatalf("delete redirected to %q", rec.Header().Get("Location"))
	}
	if _, err := st.GetModel(id); err != store.ErrNotFound {
		t.Errorf("get deleted row = %v, want ErrNotFound", err)
	}
	if body := get(t, h, cookie, "/admin/models?flash=deleted"); !strings.Contains(body, "已删除") {
		t.Error("the delete flash was not rendered")
	}

	// Deleting whichever row is left empties the list, which is its own page.
	rec = postForm(t, h, "/admin/models/delete", cookie, csrf, url.Values{"id": {itoa(findID(t, st, "主用"))}})
	if rec.Code != http.StatusFound {
		t.Fatalf("second delete got %d", rec.Code)
	}
	if body := get(t, h, cookie, "/admin/models"); !strings.Contains(body, "还没有配置") {
		t.Errorf("empty list state missing: %s", body)
	}
}

// findID looks a stored row up by name, because the panel only ever reveals ids
// through a redirect.
func findID(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()

	rows, err := st.ListModels()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("no row named %q stored", name)
	return 0
}

func TestRejectedSaveEchoesTheFormBack(t *testing.T) {
	st, h := newTestServer(t)
	cookie, csrf := logIn(t, h)

	rec := postForm(t, h, "/admin/models/save", cookie, csrf, url.Values{
		"id": {"0"}, "provider": {"not-a-provider"}, "base_url": {""}, "api_key": {"typed-key"}, "model": {""},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("invalid save got %d, want the form back", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "保存失败") {
		t.Errorf("no save error rendered: %s", body)
	}
	if !strings.Contains(body, `value="typed-key"`) {
		t.Error("the key the operator just typed was thrown away")
	}
	if rows, _ := st.ListModels(); len(rows) != 0 {
		t.Errorf("an invalid row was stored: %+v", rows)
	}
}

func TestUnknownModelAndBadIDs(t *testing.T) {
	st, h := newTestServer(t)
	cookie, csrf := logIn(t, h)
	seed := &store.ModelConfig{Provider: "openai", BaseURL: "https://x", APIKey: "k", Model: "m", Enabled: true}
	if err := st.SaveModel(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/models/form?id=999", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing row got %d, want 404", rec.Code)
	}

	for _, target := range []string{"/admin/models/enable", "/admin/models/delete"} {
		rec := postForm(t, h, target, cookie, csrf, url.Values{"id": {"not-a-number"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s with a bad id got %d, want 400", target, rec.Code)
		}
	}

	// The active row survives an enable that names a row which does not exist.
	rec = postForm(t, h, "/admin/models/enable", cookie, csrf, url.Values{"id": {"999"}})
	if rec.Code != http.StatusFound {
		t.Fatalf("enable of a missing row got %d", rec.Code)
	}
	if active, ok, _ := st.ActiveModel(); !ok || active.ID != seed.ID {
		t.Error("a failed enable changed the active row")
	}
}

func TestReadOnlyRoutesRefuseWrites(t *testing.T) {
	_, h := newTestServer(t)
	cookie, csrf := logIn(t, h)

	for _, target := range []string{"/admin/models", "/admin/models/form"} {
		rec := postForm(t, h, target, cookie, csrf, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s got %d, want 405", target, rec.Code)
		}
	}
}

func TestLoginFormIsPublicAndSessionCookieIsHardened(t *testing.T) {
	_, h := newTestServer(t)

	body := get(t, h, "", "/admin/login")
	if !strings.Contains(body, "登录") {
		t.Fatalf("login form missing: %s", body)
	}

	// Over plain http (a tunnel to 127.0.0.1) Secure must stay off, or the
	// browser would drop the cookie; behind the proxy it must be on.
	rec := do(t, h, http.MethodPost, "/admin/login", "", url.Values{"username": {testUser}, "password": {testPass}})
	if c := sessionCookieOf(t, rec); strings.Contains(rec.Header().Get("Set-Cookie"), "Secure") {
		t.Errorf("plain-http session cookie marked Secure: %v", c)
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(url.Values{
		"username": {testUser}, "password": {testPass},
	}.Encode()))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.RemoteAddr = "10.0.0.2:1"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	cookie := sessionCookieOf(t, rec)
	sc := rec.Header().Get("Set-Cookie")
	for _, want := range []string{"Secure", "HttpOnly", "SameSite=Lax", "Path=/admin"} {
		if !strings.Contains(sc, want) {
			t.Errorf("proxied cookie missing %q: %s", want, sc)
		}
	}

	// The panel itself, not the login form, is what the proxied session reaches.
	if body := get(t, h, cookie, "/admin/models"); !strings.Contains(body, "退出") {
		t.Errorf("proxied session did not reach the panel: %s", body)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
