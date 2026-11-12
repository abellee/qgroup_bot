package sub2api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type recorded struct {
	head       http.Header
	query      url.Values
	auditQuery url.Values
}

// newStub serves a canned admin response and records how it was asked.
func newStub(t *testing.T, body string) (string, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.head = r.Header.Clone()
		if strings.HasSuffix(r.URL.Path, "/audit-logs") {
			rec.auditQuery = r.URL.Query()
			body = `{"code":0,"data":{"items":[{"method":"POST","path":"/api/v1/announcements/17/read"}],"total":1,"page":1,"page_size":200}}`
		} else {
			rec.query = r.URL.Query()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, rec
}

const realEmail = "abellee@example.com"

func TestFindUserByEmailUsesAdminKeyAndExactMatch(t *testing.T) {
	base, rec := newStub(t, `{"code":0,"message":"success","data":{"total":1,"items":[{"id":4,"email":"`+realEmail+`","username":"abel","status":"active","created_at":"2026-11-12T00:00:00Z","last_used_at":"2026-11-13T00:00:00Z"}]}}`)
	c := New(base, "admin-key", "/api/v1/admin/users", &http.Client{})

	found, active, used, err := c.FindUser(context.Background(), realEmail, true)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if !found || !active || !used {
		t.Errorf("FindUser = (%v, %v, %v), want (true, true, true)", found, active, used)
	}
	if got := rec.head.Get("x-api-key"); got != "admin-key" {
		t.Errorf("x-api-key = %q, want admin-key (sub2api rejects Authorization: Bearer)", got)
	}
	if got := rec.query.Get("search"); got != realEmail {
		t.Errorf("search = %q, want %q", got, realEmail)
	}
	if got := rec.query.Get("email"); got != "" {
		t.Errorf("email param sent (%q); the admin list ignores it", got)
	}
}

// The admin search is a substring match over several fields, so rows that are
// not the requested address must not count as a hit.
func TestFindUserRejectsFuzzyEmailNeighbours(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"other domain same local", `{"code":0,"data":{"total":1,"items":[{"email":"abellee@other.com"}]}}`},
		{"email contains query", `{"code":0,"data":{"total":1,"items":[{"email":"abellee@example.com.impostor.net"}]}}`},
		{"matched on username", `{"code":0,"data":{"total":1,"items":[{"email":"someone@else.cn"}]}}`},
		{"empty page", `{"code":0,"data":{"total":0,"items":[]}}`},
	}
	for _, c := range cases {
		base, _ := newStub(t, c.body)
		cl := New(base, "k", "/api/v1/admin/users", &http.Client{})
		found, _, _, err := cl.FindUser(context.Background(), realEmail, true)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if found {
			t.Errorf("%s: matched a non-identical email, want no approval", c.name)
		}
	}
}

func TestFindUserMatchesCaseAndPadding(t *testing.T) {
	base, _ := newStub(t, `{"code":0,"data":{"total":1,"items":[{"id":4,"email":" AbelLee@Example.COM ","status":"active","created_at":"2026-11-12T00:00:00Z","last_used_at":"2026-11-13T00:00:00Z"}]}}`)
	c := New(base, "k", "/api/v1/admin/users", &http.Client{})
	found, active, used, err := c.FindUser(context.Background(), realEmail, true)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if !found || !active || !used {
		t.Errorf("FindUser = (%v, %v, %v), want an active used case-insensitive match", found, active, used)
	}
}

func TestFindUserMatchesUsernameAndReturnsStatus(t *testing.T) {
	base, rec := newStub(t, `{"code":0,"data":{"total":1,"items":[{"id":4,"email":"other@example.com","username":"Abel","status":"disabled","created_at":"2026-11-12T00:00:00Z","last_used_at":"2026-11-13T00:00:00Z"}]}}`)
	c := New(base, "k", "/api/v1/admin/users", &http.Client{})
	found, active, used, err := c.FindUser(context.Background(), "abel", false)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if !found || active || !used {
		t.Errorf("FindUser = (%v, %v, %v), want (true, false, true) for disabled used account", found, active, used)
	}
	if got := rec.query.Get("search"); got != "abel" {
		t.Errorf("search = %q, want username", got)
	}
}

func TestFindUserDoesNotMatchEmailWhenLookingUpUsername(t *testing.T) {
	base, _ := newStub(t, `{"code":0,"data":{"total":1,"items":[{"email":"abel","username":"another","status":"active"}]}}`)
	c := New(base, "k", "/api/v1/admin/users", &http.Client{})
	found, _, _, err := c.FindUser(context.Background(), "abel", false)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if found {
		t.Error("FindUser matched the email field when looking up a username")
	}
}

func TestFindUserReportsNoOtherActivityWhenOnlyAnnouncementReadsExist(t *testing.T) {
	base, rec := newStub(t, `{"code":0,"data":{"total":1,"items":[{"id":7,"email":"`+realEmail+`","username":"abel","status":"active","created_at":"2026-11-12T00:00:00Z","last_used_at":null}]}}`)
	c := New(base, "k", "/api/v1/admin/users", &http.Client{})
	found, active, activity, err := c.FindUser(context.Background(), realEmail, true)
	if err != nil {
		t.Fatalf("FindUser: %v", err)
	}
	if !found || !active || activity {
		t.Errorf("FindUser = (%v, %v, %v), want (true, true, false)", found, active, activity)
	}
	if rec.auditQuery.Get("actor_user_id") != "7" || rec.auditQuery.Get("start_time") != "2026-11-12T00:00:00Z" {
		t.Errorf("audit query = %v, want user 7 since registration", rec.auditQuery)
	}
}

func TestFindUserDetectsOtherActivitySinceRegistration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/audit-logs") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[{"method":"POST","path":"/api/v1/keys"}],"total":1,"page":1,"page_size":200}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"total":1,"items":[{"id":8,"email":"` + realEmail + `","username":"abel","status":"active","created_at":"2026-11-12T00:00:00Z","last_used_at":null}]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "/api/v1/admin/users", &http.Client{})
	found, active, activity, err := c.FindUser(context.Background(), realEmail, true)
	if err != nil || !found || !active || !activity {
		t.Fatalf("FindUser = (%v, %v, %v, %v), want active user with activity", found, active, activity, err)
	}
}

func TestAnnouncementReadAuditEntriesDoNotCountAsOtherActivity(t *testing.T) {
	if !isPassiveAuditEntry("POST", "/api/v1/announcements/123/read") {
		t.Fatal("announcement read should not count as other activity")
	}
	for _, tc := range []struct {
		method, path string
		passive      bool
	}{
		{"POST", "/api/v1/keys", false},
		{"PUT", "/api/v1/user", false},
		{"DELETE", "/api/v1/keys/1", false},
		{"POST", "/api/v1/auth/login", true},
		{"POST", "/api/v1/auth/refresh", true},
		{"POST", "/api/v1/auth/revoke-all-sessions", true},
		{"POST", "/api/v1/announcements/123/read/extra", false},
	} {
		if isPassiveAuditEntry(tc.method, tc.path) != tc.passive {
			t.Errorf("isPassiveAuditEntry(%q, %q) returned wrong value", tc.method, tc.path)
		}
	}
}

func TestFindUserIgnoresAPIUseBeforeRegistration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/audit-logs") {
			_, _ = w.Write([]byte(`{"code":0,"data":{"items":[],"total":0,"page":1,"page_size":200}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"total":1,"items":[{"id":9,"email":"` + realEmail + `","username":"abel","status":"active","created_at":"2026-11-12T00:00:00Z","last_used_at":"2026-11-11T00:00:00Z"}]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "/api/v1/admin/users", &http.Client{})
	_, _, activity, err := c.FindUser(context.Background(), realEmail, true)
	if err != nil || activity {
		t.Fatalf("activity = %v, err = %v; want no activity since registration", activity, err)
	}
}

func TestFindUserFailsClosedWhenActivityLookupFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/audit-logs") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":500,"message":"unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"total":1,"items":[{"id":10,"email":"` + realEmail + `","username":"abel","status":"active","created_at":"2026-11-12T00:00:00Z","last_used_at":null}]}}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "k", "/api/v1/admin/users", &http.Client{})
	found, _, _, err := c.FindUser(context.Background(), realEmail, true)
	if !found || err == nil {
		t.Fatalf("FindUser = (found=%v, err=%v), want found user and lookup error", found, err)
	}
}

// Anything the client cannot read must surface as an error, never as "not
// registered", or a transient failure would silently drop a legitimate applicant.
func TestFindUserFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"invalid token", 401, `{"code":"INVALID_TOKEN","message":"Invalid token"}`},
		{"business error", 200, `{"code":40001,"message":"bad request"}`},
		{"not json", 502, `<html>bad gateway</html>`},
		{"missing code", 200, `{"data":{"total":1,"items":[{"email":"x"}]}}`},
		{"truncated page", 200, `{"code":0,"data":{"total":900,"items":[{"email":"other@x.com"}]}}`},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.code)
			_, _ = w.Write([]byte(c.body))
		}))
		cl := New(srv.URL, "k", "/api/v1/admin/users", &http.Client{})
		found, _, _, err := cl.FindUser(context.Background(), realEmail, true)
		srv.Close()
		if err == nil {
			t.Errorf("%s: no error returned (found=%v)", c.name, found)
		}
		if found {
			t.Errorf("%s: reported existence despite an unusable response", c.name)
		}
	}
}
